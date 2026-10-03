package faults

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type cliClock struct{}

func (cliClock) Now() float64 { return float64(time.Now().UnixMicro()) / 1e6 }
func (cliClock) ISO() string  { return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00") }

// Names lists the fault commands, in cli.py's add_parser order.
func Names() []string {
	return append(append(append(append([]string{"fault-target", "fault-observe", "fault-fix", "fault-reverify", "fault-resolve", "fault-prune"}, dNames...), cNames...), f2Names...), f1Names...)
}

// readOnly is cli.py's READ_ONLY_COMMANDS among the fault commands; fault-policy and fault-limit
// read when they name no class or kind to set (_read_only_command).
var readOnly = map[string]func(dispatch.Args) bool{
	"fault-show":          func(dispatch.Args) bool { return true },
	"fault-next":          func(dispatch.Args) bool { return true },
	"fault-attention":     func(dispatch.Args) bool { return true },
	"fault-notifications": func(dispatch.Args) bool { return true },
	"fault-policy":        func(args dispatch.Args) bool { return !args.Given("fault-class") },
	"fault-limit":         func(args dispatch.Args) bool { return !args.Given("kind") },
}

func init() {
	// Importing codex_session_relay.projects declares the product fault classes.
	dispatch.OnKindModule("codex_session_relay.projects", InstallProductDeclarations)
	var commands []dispatch.Command
	for _, name := range Names() {
		commands = append(commands, dispatch.Command{Name: name, ReadOnlyWhen: readOnly[name],
			Run: func(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
				return execute(ctx, name, services, args)
			}})
	}
	dispatch.Register(nil, commands...)
}

// hostAnswer is the host envelope for err, answered whole whatever err is.
func hostAnswer(err error) error {
	return &dispatch.PayloadExit{Payload: contract.OrderedObject{{Key: "error", Value: "host"}, {Key: "detail", Value: hostText(err)}}, Code: contract.ExitHost}
}

// execute is cli.main for a fault command once the relay CLI checked the selected store: the
// handler's own argument checks, its store, and its answer in the family's key order.
func execute(ctx context.Context, name string, services dispatch.Services, line dispatch.Args) (any, error) {
	args := map[string]string{}
	for option, values := range line.Parsed.Values {
		args["--"+option] = values[len(values)-1]
	}
	ctx = context.WithValue(ctx, numberArgsKey{}, line.Parsed.Numbers)
	// These Python handlers validate before their first lazy services.store access
	// (cmd_fault_next reads --limit before it asks whether its store is read-only). Other
	// fault commands deliberately retain their existing precedence.
	var err error
	switch name {
	case "fault-show":
		err = validateShow(ctx, args)
	case "fault-next":
		_, err = cLimit(ctx, args["--limit"], "--limit", 4)
	case "fault-sweep":
		var input sweepInput
		input, err = validateSweep(ctx, args)
		ctx = context.WithValue(ctx, sweepInputKey{}, input)
	}
	if err != nil {
		reason, detail, refused := strings.Cut(err.Error(), ": ")
		if refused && strings.HasPrefix(reason, "fault_") {
			return nil, &dispatch.PayloadExit{Payload: contract.OrderedObject{{Key: "error", Value: "refused"}, {Key: "reason", Value: reason}, {Key: "detail", Value: detail}}, Code: contract.ExitRefused}
		}
		return nil, hostAnswer(err)
	}
	s, err := store.Open(ctx, services.Selection.DBPath(), services.SocketPath)
	if err != nil {
		// A refusal keeps its reason and exit 2, as cli.main answers every RelayError.
		return nil, err
	}
	defer s.Close()
	l := &Ledger{Store: s, Clock: f1Clock(ctx)}
	// These ledger mutations and their receipts use the canonical fault identity.
	if name == "fault-fix" || name == "fault-reverify" || name == "fault-resolve" || name == "fault-prune" {
		alias, e := l.one(ctx, "SELECT fault_id FROM fault_aliases WHERE alias_id=?", args["--fault"])
		if e != nil {
			return nil, hostAnswer(e)
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
			err = fmt.Errorf("fault_observation_malformed: the observation is not readable JSON: %v", err)
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
			n, _ := integerArg(ctx, "--keep", raw)
			if n < 1 {
				err = fmt.Errorf("fault_observation_malformed: keep at least one")
				break
			}
			keep = int(n)
		}
		var removed int64
		removed, err = l.Prune(ctx, args["--fault"], keep)
		if err == nil {
			result = map[string]any{"faultId": args["--fault"], "kept": keep, "removed": removed, "limits": "the ledger's occurrence_count still counts what was observed; these rows are the evidence, not the count"}
		}
	}
	value, code := faultAnswer(result, err)
	if code != contract.ExitOk {
		return nil, &dispatch.PayloadExit{Payload: value.(contract.OrderedObject), Code: code}
	}
	return value, nil
}

// faultAnswer is the printed object for a fault handler's ending, every map with its keys in
// sorted order, and its exit code.
func faultAnswer(result any, err error) (any, int) {
	if err != nil {
		var missing *cMissingFault
		if errors.As(err, &missing) {
			return answerObject(map[string]any{"faultId": missing.id, "found": false}), 2
		}
		reason, _, _ := strings.Cut(err.Error(), ":")
		if strings.HasPrefix(reason, "fault_") || strings.HasPrefix(err.Error(), "transaction body: fault_") {
			if strings.HasPrefix(err.Error(), "transaction body: ") {
				reason, _, _ = strings.Cut(strings.TrimPrefix(err.Error(), "transaction body: "), ":")
			}
			detail := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(err.Error(), "transaction body: "), reason+":"))
			return answerObject(map[string]any{"error": "refused", "reason": reason, "detail": detail}), 2
		}
		return answerObject(map[string]any{"error": "host", "detail": hostText(err)}), 3
	}
	return answerObject(result), 0
}

// answerObject is a fault command's answer as the relay CLI prints it: every map as an object
// with its keys in sorted order.
func answerObject(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(contract.OrderedObject, 0, len(v))
		for _, key := range slices.Sorted(maps.Keys(v)) {
			out = append(out, contract.Field{Key: key, Value: answerObject(v[key])})
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = answerObject(item)
		}
		return out
	}
	return value
}

func latestPublicationAnswer(ctx context.Context, l *Ledger, id string, before int64) any {
	rows, err := l.Store.All(ctx, "SELECT p.publication_id,p.kind,p.trigger_key,p.state,p.tracker_ref,x.project_ref FROM fault_publications p LEFT JOIN fault_publication_payloads x ON x.publication_id=p.publication_id WHERE p.fault_id=? ORDER BY p.rowid", id)
	if err != nil || int64(len(rows)) <= before {
		return nil
	}
	// record() may queue a secondary update after its primary publication (reopen does);
	// its reply still describes the first publication created by this observation.
	row := rows[before]
	trigger, kind := row.Text("trigger_key"), row.Text("kind")
	awaitingTarget := kind == openRecord && row.Get("tracker_ref") == nil
	fault, _ := l.one(ctx, "SELECT product,scope_key,external_ref FROM fault_ledger WHERE fault_id=?", id)
	reason := "queued"
	if awaitingTarget {
		reason = fmt.Sprintf("queued; no target owned by %s for %s (awaiting_target), so it waits rather than being filed somewhere guessed", fault.Text("product"), fault.Text("scope_key"))
	}
	return map[string]any{"publicationId": row.Text("publication_id"), "kind": kind, "trigger": trigger, "queued": true, "awaitingTarget": awaitingTarget, "awaitingRecord": kind != openRecord && fault.Text("external_ref") == "", "reason": reason}
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
	if row.Text("state") != pending {
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
		reason = fmt.Sprintf("%s; no target owned by %s for %s (%s), so it waits rather than being filed somewhere guessed", reason, fault.Text("product"), fault.Text("scope_key"), why)
	}
	return map[string]any{"publicationId": row.Text("publication_id"), "kind": openRecord, "trigger": triggerOpen, "queued": true, "awaitingTarget": awaiting, "awaitingRecord": false, "reason": reason}
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

// hostText is the host envelope's detail for err: a str sqlite3 or an identity hash could not
// encode is cli.main's "UnicodeEncodeError: ...", whatever wrapped it on the way here.
func hostText(err error) string {
	if encode := store.EncodeError(err); encode != nil {
		return encode.HostDetail()
	}
	return err.Error()
}
