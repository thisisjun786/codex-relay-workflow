package faults

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// SelectionCheck runs the relay CLI's selection refusal before a fault handler.
type SelectionCheck func(store.StateSelection, string) error

// Names lists implemented fault commands. It grows with the port, rather than
// claiming an unsupported command succeeded.
type cliClock struct{}

func (cliClock) Now() float64 { return float64(time.Now().UnixMicro()) / 1e6 }
func (cliClock) ISO() string  { return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00") }

func Names() []string {
	return append(append(append(append([]string{"fault-target", "fault-observe", "fault-fix", "fault-reverify", "fault-resolve", "fault-prune"}, dNames...), cNames...), f2Names...), f1Names...)
}

func ExecuteAs(ctx context.Context, prog string, argv []string, stdout, stderr io.Writer, check SelectionCheck) (int, bool) {
	var state, socket string
	var modules []string
	i := 0
	for ; i < len(argv); i++ {
		name, value, has := strings.Cut(argv[i], "=")
		switch name {
		case "--json":
			continue
		case "--state", "--socket", "--kind-module":
			if !has {
				if i+1 >= len(argv) {
					return 0, false
				}
				i++
				value = argv[i]
			}
			switch name {
			case "--state":
				state = value
			case "--socket":
				socket = value
			default:
				modules = append(modules, value)
			}
			continue
		}
		break
	}
	if i >= len(argv) || !slices.Contains(Names(), argv[i]) {
		return 0, false
	}
	name := argv[i]
	parsed, code, handled := faultParse(prog, name, argv[i+1:], stdout, stderr)
	if handled {
		return code, true
	}
	args := parsed.text
	ctx = context.WithValue(ctx, numberArgsKey{}, parsed.numbers)
	selection, err := store.ResolveStateDir(state, socket)
	if err != nil {
		return response(stdout, map[string]any{"error": "host", "detail": err.Error()}, 3), true
	}
	if check != nil {
		if err = check(selection, socket); err != nil {
			var payload interface {
				ExitPayload() (contract.OrderedObject, int)
			}
			if errors.As(err, &payload) {
				body, code := payload.ExitPayload()
				return response(stdout, body, code), true
			}
			return response(stdout, map[string]any{"error": "host", "detail": err.Error()}, 3), true
		}
	}
	for _, module := range modules {
		if module == "codex_session_relay.projects" {
			InstallProductDeclarations()
		}
		if module == "" {
			return response(stdout, map[string]any{"error": "host", "detail": "ValueError: Empty module name"}, 3), true
		}
		if strings.HasPrefix(module, ".") {
			return response(stdout, map[string]any{"error": "host", "detail": "TypeError: the 'package' argument is required to perform a relative import for '" + module + "'"}, 3), true
		}
		if !RegisteredModule(module) {
			missing := module
			parts := strings.Split(module, ".")
			for j := 1; j < len(parts); j++ {
				prefix := strings.Join(parts[:j], ".")
				if prefix == "codex_session_relay" || RegisteredModule(prefix) {
					continue
				}
				missing = prefix
				break
			}
			return response(stdout, map[string]any{"error": "usage", "detail": fmt.Sprintf("--kind-module '%s' could not be imported: No module named '%s'", module, missing)}, 4), true
		}
	}
	// These two Python handlers validate before their first lazy services.store
	// access. Other fault commands deliberately retain their existing precedence.
	switch name {
	case "fault-show":
		err = validateShow(ctx, args)
	case "fault-sweep":
		var input sweepInput
		input, err = validateSweep(ctx, args)
		ctx = context.WithValue(ctx, sweepInputKey{}, input)
	}
	if err != nil {
		reason, detail, refused := strings.Cut(err.Error(), ": ")
		if refused && strings.HasPrefix(reason, "fault_") {
			return response(stdout, map[string]any{"error": "refused", "reason": reason, "detail": detail}, 2), true
		}
		return response(stdout, map[string]any{"error": "host", "detail": err.Error()}, 3), true
	}
	s, err := store.Open(ctx, selection.DBPath(), socket)
	if err != nil {
		return response(stdout, map[string]any{"error": "host", "detail": err.Error()}, 3), true
	}
	defer s.Close()
	l := &Ledger{Store: s, Clock: f1Clock(ctx)}
	// These ledger mutations and their receipts use the canonical fault identity.
	if name == "fault-fix" || name == "fault-reverify" || name == "fault-resolve" || name == "fault-prune" {
		alias, e := l.one(ctx, "SELECT fault_id FROM fault_aliases WHERE alias_id=?", args["--fault"])
		if e != nil {
			return response(stdout, map[string]any{"error": "host", "detail": e.Error()}, 3), true
		}
		if alias != nil {
			args["--fault"] = textRow(alias, "fault_id")
		}
	}
	var result any
	switch name {
	case "fault-claim", "fault-operation", "fault-reconcile", "fault-complete", "fault-sweep":
		result, err = executeF1(ctx, l, name, args)
	case "fault-fail", "fault-adopt", "fault-move", "fault-update":
		result, err = executeF2(ctx, l, name, args)
	case "fault-show", "fault-next", "fault-retry", "fault-queue", "fault-cancel", "fault-stage":
		result, err = executeC(ctx, l, name, args)
	case "fault-policy", "fault-limit", "fault-attention", "fault-relink", "fault-notifications", "fault-notification-raise", "fault-notification-reserve", "fault-notification-ack", "fault-notification-fail", "fault-notification-reconcile":
		result, err = executeD(ctx, l, name, args)
	case "fault-target":
		// Optional CLI values distinguish omitted (nil in Python) from an
		// explicitly empty string; the string-based ledger API cannot.
		if productName.MatchString(args["--product"]) {
			for _, field := range []string{"workspace", "project", "project-ref"} {
				if value, present := args["--"+field]; present && strings.TrimSpace(value) == "" {
					err = fmt.Errorf("fault_observation_malformed: %s is a non-blank string", strings.ReplaceAll(field, "-", "_"))
					break
				}
			}
		}
		if err != nil {
			break
		}
		result, err = l.SetWorkspaceTarget(ctx, args["--product"], args["--workspace"], args["--project"], args["--team"], args["--project-ref"])
	case "fault-observe":
		text := args["--observation"]
		if path, ok := strings.CutPrefix(text, "@"); ok {
			raw, e := os.ReadFile(filepath.Clean(path))
			if e != nil {
				err = e
				break
			}
			text = string(raw)
		}
		var value any
		value, err = loads(text)
		if err != nil {
			err = fmt.Errorf("fault_observation_malformed: the observation is not readable JSON: %s", store.PythonJSONError(text))
			break
		}
		var o Observation
		o, err = parseObservation(value)
		if err != nil {
			break
		}
		workspace, _ := o.Scope["workspace"].(string)
		candidate := FaultIDInWorkspace(o.Product, o.FaultClass, o.Signature, workspace)
		if alias, e := s.One(ctx, "SELECT fault_id FROM fault_aliases WHERE alias_id = ?", candidate); e != nil {
			err = e
			break
		} else if alias != nil {
			candidate = textRow(alias, "fault_id")
		}
		prior, lookupErr := s.One(ctx, "SELECT COUNT(*) AS n FROM fault_publications WHERE fault_id=?", candidate)
		if lookupErr != nil {
			err = lookupErr
			break
		}
		var recorded bool
		recorded, err = l.Record(ctx, o)
		if err == nil {
			id := candidate
			if alias, e := s.One(ctx, "SELECT fault_id FROM fault_aliases WHERE alias_id = ?", id); e != nil {
				err = e
				break
			} else if alias != nil {
				id = textRow(alias, "fault_id")
			}
			row, e := s.One(ctx, "SELECT state,cycle,severity,occurrence_count,suppression FROM fault_ledger WHERE fault_id = ?", id)
			err = e
			if err == nil {
				if !recorded {
					result = map[string]any{"faultId": id, "recorded": false, "state": textRow(row, "state"), "occurrenceCount": numberRow(row, "occurrence_count"), "reason": "this occurrence was already recorded in this episode", "publication": nil}
				} else {
					result = map[string]any{"faultId": id, "recorded": recorded, "state": textRow(row, "state"), "cycle": numberRow(row, "cycle"), "severity": textRow(row, "severity"), "occurrenceCount": numberRow(row, "occurrence_count"), "suppression": loadsMap(textRow(row, "suppression")), "publication": latestPublicationAnswer(ctx, l, id, integer(prior, "n"))}
				}
			}
		}
	case "fault-fix", "fault-reverify":
		kind := "fix"
		if name == "fault-reverify" {
			kind = "reverification"
		}
		var id string
		var recorded bool
		id, recorded, err = l.Remediate(ctx, args["--fault"], kind, args["--ref"], args["--method"], args["--outcome"])
		if err == nil {
			result = map[string]any{"faultId": args["--fault"], "recorded": recorded, "remediationId": id, "state": FixPending}
			if kind == "fix" && recorded {
				result.(map[string]any)["publication"], err = transitionPublicationAnswer(ctx, l, args["--fault"], triggerFix+":"+id[:12])
			} else {
				result.(map[string]any)["publication"] = nil
			}
		}
	case "fault-resolve":
		var resolved bool
		resolved, err = l.Resolve(ctx, args["--fault"])
		if err == nil {
			result = map[string]any{"faultId": args["--fault"], "state": Resolved, "resolved": resolved}
			if resolved {
				fault, lookupErr := l.one(ctx, "SELECT cycle FROM fault_ledger WHERE fault_id=?", args["--fault"])
				if lookupErr != nil {
					err = lookupErr
				} else {
					cycle := integer(fault, "cycle")
					result.(map[string]any)["cycle"] = cycle
					result.(map[string]any)["publication"], err = transitionPublicationAnswer(ctx, l, args["--fault"], fmt.Sprintf("%s:%d", triggerResolve, cycle))
				}
			} else {
				result.(map[string]any)["reason"] = "already resolved"
			}
		}
	case "fault-prune":
		keep := 20
		if raw := args["--keep"]; raw != "" {
			n := integerArg(ctx, "--keep", raw)
			if n.Sign() < 1 {
				err = fmt.Errorf("fault_observation_malformed: keep at least one")
				break
			}
			if !n.IsInt64() {
				if _, err = cFault(ctx, l, args["--fault"]); err != nil {
					break
				}
				_, err = argparse.SQLiteInteger(n)
				break
			}
			keep = int(n.Int64())
		}
		var removed int64
		removed, err = l.Prune(ctx, args["--fault"], keep)
		if err == nil {
			result = map[string]any{"faultId": args["--fault"], "kept": keep, "removed": removed, "limits": "the ledger's occurrence_count still counts what was observed; these rows are the evidence, not the count"}
		}
	}
	if err != nil {
		var missing *cMissingFault
		if errors.As(err, &missing) {
			return cResponse(stdout, map[string]any{"faultId": missing.id, "found": false}, 2), true
		}
		reason, _, _ := strings.Cut(err.Error(), ":")
		if strings.HasPrefix(reason, "fault_") || strings.HasPrefix(err.Error(), "transaction body: fault_") {
			if strings.HasPrefix(err.Error(), "transaction body: ") {
				reason, _, _ = strings.Cut(strings.TrimPrefix(err.Error(), "transaction body: "), ":")
			}
			detail := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(err.Error(), "transaction body: "), reason+":"))
			if slices.Contains(f1Names, name) {
				return f1Response(stdout, map[string]any{"error": "refused", "reason": reason, "detail": detail}, 2), true
			}
			if slices.Contains(f2Names, name) {
				return f2Response(stdout, map[string]any{"error": "refused", "reason": reason, "detail": detail}, 2), true
			}
			if slices.Contains(dNames, name) {
				return dResponse(stdout, map[string]any{"error": "refused", "reason": reason, "detail": detail}, 2), true
			}
			if slices.Contains(cNames, name) {
				return cResponse(stdout, map[string]any{"error": "refused", "reason": reason, "detail": detail}, 2), true
			}
			return response(stdout, map[string]any{"error": "refused", "reason": reason, "detail": detail}, 2), true
		}
		return response(stdout, map[string]any{"error": "host", "detail": err.Error()}, 3), true
	}
	if slices.Contains(f1Names, name) {
		return f1Response(stdout, result, 0), true
	}
	if slices.Contains(f2Names, name) {
		return f2Response(stdout, result, 0), true
	}
	if slices.Contains(dNames, name) {
		return dResponse(stdout, result, 0), true
	}
	if slices.Contains(cNames, name) {
		return cResponse(stdout, result, 0), true
	}
	return response(stdout, result, 0), true
}
func latestPublicationAnswer(ctx context.Context, l *Ledger, id string, before int64) any {
	rows, err := l.Store.All(ctx, "SELECT p.publication_id,p.kind,p.trigger_key,p.state,p.tracker_ref,x.project_ref FROM fault_publications p LEFT JOIN fault_publication_payloads x ON x.publication_id=p.publication_id WHERE p.fault_id=? ORDER BY p.rowid", id)
	if err != nil || int64(len(rows)) <= before {
		return nil
	}
	// record() may queue a secondary update after its primary publication (reopen does);
	// its reply still describes the first publication created by this observation.
	row := rows[before]
	trigger, kind := text(row, "trigger_key"), text(row, "kind")
	awaitingTarget := kind == openRecord && row.Get("tracker_ref") == nil
	fault, _ := l.one(ctx, "SELECT product,scope_key,external_ref FROM fault_ledger WHERE fault_id=?", id)
	reason := "queued"
	if awaitingTarget {
		reason = fmt.Sprintf("queued; no target owned by %s for %s (awaiting_target), so it waits rather than being filed somewhere guessed", text(fault, "product"), text(fault, "scope_key"))
	}
	return map[string]any{"publicationId": text(row, "publication_id"), "kind": kind, "trigger": trigger, "queued": true, "awaitingTarget": awaitingTarget, "awaitingRecord": kind != openRecord && text(fault, "external_ref") == "", "reason": reason}
}

func transitionPublicationAnswer(ctx context.Context, l *Ledger, id, trigger string) (any, error) {
	publication := publicationID(id, appendComment, trigger)
	row, err := l.one(ctx, "SELECT 1 FROM fault_publications WHERE publication_id=?", publication)
	if err != nil || row == nil {
		return nil, err
	}
	return map[string]any{"publicationId": publication, "kind": appendComment, "trigger": trigger, "queued": true, "awaitingTarget": false, "awaitingRecord": true, "reason": "queued"}, nil
}

func publicationAnswer(ctx context.Context, l *Ledger, id string, recorded bool, revived ...bool) any {
	if !recorded {
		return nil
	}
	row, err := l.one(ctx, "SELECT publication_id,kind,trigger_key,state FROM fault_publications WHERE fault_id = ? AND kind = 'open_record'", id)
	if err != nil || row == nil {
		return nil
	}
	if text(row, "state") != pending {
		return nil
	}
	fault, err := l.one(ctx, "SELECT scope_key,product FROM fault_ledger WHERE fault_id = ?", id)
	if err != nil || fault == nil {
		return nil
	}
	target, why, err := f1OwnedTarget(ctx, l, fault)
	if err != nil {
		return nil
	}
	awaiting := target == nil
	reason := "queued"
	if len(revived) > 0 && revived[0] {
		reason = "revived"
	}
	if awaiting {
		reason = fmt.Sprintf("%s; no target owned by %s for %s (%s), so it waits rather than being filed somewhere guessed", reason, text(fault, "product"), text(fault, "scope_key"), why)
	}
	return map[string]any{"publicationId": text(row, "publication_id"), "kind": openRecord, "trigger": triggerOpen, "queued": true, "awaitingTarget": awaiting, "awaitingRecord": false, "reason": reason}
}
func ordered(value any) any {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		order := []string{"error", "faultId", "recorded", "remediationId", "state", "resolved", "cycle", "severity", "occurrenceCount", "suppression", "reason", "publication", "kept", "removed", "limits", "scopeKey", "product", "team", "projectRef", "changed", "backfilled", "backfillPending", "relinked", "relinkPending", "detail"}
		if _, ok := v["publicationId"]; ok {
			order = []string{"publicationId", "kind", "trigger", "queued", "awaitingTarget", "awaitingRecord", "reason"}
		} else if _, ok := v["publish"]; ok {
			order = []string{"publish", "threshold", "window", "counted", "reason"}
		}
		slices.SortFunc(keys, func(a, b string) int {
			rank := func(k string) int {
				for i, name := range order {
					if name == k {
						return i
					}
				}
				return len(order)
			}
			if rank(a) != rank(b) {
				return rank(a) - rank(b)
			}
			return strings.Compare(a, b)
		})
		out := make(contract.OrderedObject, 0, len(keys))
		for _, key := range keys {
			out = append(out, contract.Field{Key: key, Value: ordered(v[key])})
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = ordered(item)
		}
		return out
	default:
		return value
	}
}
func response(w io.Writer, value any, code int) int {
	if err := contract.Emit(w, ordered(value)); err != nil {
		return 3
	}
	return code
}
func textRow(row store.Row, key string) string {
	if row == nil {
		return ""
	}
	v, _ := row.Get(key).(string)
	return v
}
func numberRow(row store.Row, key string) int64 {
	if row == nil {
		return 0
	}
	v, _ := row.Get(key).(int64)
	return v
}
func parseObservation(value any) (Observation, error) {
	m, ok := value.(map[string]any)
	if !ok {
		return Observation{}, fmt.Errorf("fault_observation_malformed: an observation is an object")
	}
	if m["schema"] != SchemaObservation {
		return Observation{}, fmt.Errorf("fault_observation_malformed: invalid schema")
	}
	if v, ok := m["cleared"]; ok && v != nil {
		if _, yes := v.(bool); !yes {
			pythonType := fmt.Sprintf("%T", v)
			if _, isString := v.(string); isString {
				pythonType = "str"
			}
			return Observation{}, fmt.Errorf("fault_observation_malformed: cleared is a boolean, not %s", pythonType)
		}
	}
	s, _ := m["signature"].(map[string]any)
	scope, _ := m["scope"].(map[string]any)
	if m["scope"] != nil && scope == nil {
		return Observation{}, fmt.Errorf("fault_observation_malformed: scope is an object")
	}
	for key, value := range scope {
		switch value.(type) {
		case nil, string, float64, bool:
		default:
			return Observation{}, fmt.Errorf("fault_observation_malformed: scope.%s is not a JSON scalar", key)
		}
		if (key == "workspace" || key == "projectKey") && value != nil && !named(value) {
			return Observation{}, fmt.Errorf("fault_observation_malformed: scope.%s is a non-blank string", key)
		}
	}
	e, _ := m["evidence"].([]any)
	if m["evidence"] != nil && e == nil {
		return Observation{}, fmt.Errorf("fault_observation_malformed: evidence is a list of objects")
	}
	str := func(key string) string { v, _ := m[key].(string); return v }
	cleared, _ := m["cleared"].(bool)
	return Observation{Product: str("product"), FaultClass: str("faultClass"), Severity: str("severity"), Signature: s, OccurrenceKey: str("occurrenceKey"), Scope: scope, Detail: str("detail"), Evidence: e, Cleared: cleared}, nil
}
