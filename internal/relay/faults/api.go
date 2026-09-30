package faults

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"slices"
	"strconv"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Adoption is the existing issue a first observation adopts before suppression can file.
type Adoption struct {
	ExternalRef string
	Scope       map[string]any
}

var (
	classMu             sync.RWMutex
	classThresholds     = map[string]int64{}
	productDeclarations sync.Once
)

func classLookup(name string) (classPolicy, bool) {
	classMu.RLock()
	defer classMu.RUnlock()
	policy, ok := classes[name]
	return policy, ok
}

func classThreshold(name string) (int64, bool) {
	classMu.RLock()
	defer classMu.RUnlock()
	threshold, ok := classThresholds[name]
	return threshold, ok
}

func classNames(after string) []string {
	classMu.RLock()
	defer classMu.RUnlock()
	names := make([]string, 0, len(classes))
	for name := range classes {
		if name > after {
			names = append(names, name)
		}
	}
	return names
}

// RegisterClass installs a declared product fault class for the lifetime of this process.
// Python does this when the declaring module is imported; repeated imports are idempotent.
func RegisterClass(name string, threshold int64) error {
	policy, declared := declaredClasses[name]
	if !declared {
		return fmt.Errorf("fault_class_unregistered: %s", name)
	}
	classMu.Lock()
	defer classMu.Unlock()
	if existing, ok := classes[name]; ok && existing != policy {
		return fmt.Errorf("%s is already registered with different terms; pick another name", f1Repr(name))
	}
	classes[name] = policy
	if threshold > 0 {
		classThresholds[name] = threshold
	}
	return nil
}

var productDeclarationInstaller func()

// SetProductDeclarationInstaller binds the routing-owned project kind without creating an import cycle.
func SetProductDeclarationInstaller(install func()) { productDeclarationInstaller = install }

// InstallProductDeclarations models importing codex_session_relay.projects once per process.
func InstallProductDeclarations() {
	productDeclarations.Do(func() {
		for _, name := range []string{"product_defect", "product_expected", "unclassified_incident", "completion_mismatch", "completion_unverified", "project_needed"} {
			threshold := int64(0)
			if name == "completion_mismatch" {
				threshold = 1
			}
			if err := RegisterClass(name, threshold); err != nil {
				panic(err)
			}
		}
		if productDeclarationInstaller != nil {
			productDeclarationInstaller()
		}
	})
}

func RegisteredKind(name string) bool { _, ok := executableKind(name); return ok }
func RegisteredClass(name string) bool {
	_, ok := classLookup(name)
	return ok
}

// WithInputs injects clock and claim-token entropy into library calls on this context.
func WithInputs(ctx context.Context, clock Clock, entropy io.Reader) context.Context {
	return context.WithValue(ctx, f1InputsKey{}, f1Inputs{clock: clock, entropy: entropy})
}

// CanonicalID resolves workspace and legacy aliases without writing a record.
func (l *Ledger) CanonicalID(ctx context.Context, product, class string, signature map[string]any, workspace string) (string, error) {
	id := FaultIDInWorkspace(product, class, signature, workspace)
	alias, err := l.one(ctx, "SELECT fault_id FROM fault_aliases WHERE alias_id=?", id)
	if err != nil {
		return "", err
	}
	if alias != nil {
		return text(alias, "fault_id"), nil
	}
	if workspace != "" {
		legacy := FaultID(product, class, signature)
		old, err := l.one(ctx, "SELECT scope FROM fault_ledger WHERE fault_id=?", legacy)
		if err != nil {
			return "", err
		}
		if old != nil && loadsMap(text(old, "scope"))["workspace"] == workspace {
			return legacy, nil
		}
	}
	return id, nil
}

// Get returns the ledger row with its current link reading, using the caller's connection.
func (l *Ledger) Get(ctx context.Context, id string) (map[string]any, error) {
	r, err := l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id=COALESCE((SELECT fault_id FROM fault_aliases WHERE alias_id=?),?)", id, id)
	if err != nil || r == nil {
		return nil, err
	}
	out := cView(r)
	out["linkState"], out["linkedProject"] = "none", nil
	if named(r.Get("external_ref")) {
		out["linkState"] = "unlinked"
		link, err := l.one(ctx, "SELECT * FROM fault_links WHERE fault_id=?", text(r, "fault_id"))
		if err != nil {
			return nil, err
		}
		if link != nil {
			out["linkState"], out["linkedProject"] = link.Get("state"), link.Get("observed_project_ref")
			if out["linkState"] == "linked" {
				target, _, err := f1OwnedTarget(ctx, l, r)
				if err != nil {
					return nil, err
				}
				if target == nil || target.Get("project_ref") == nil || target.Get("project_ref") != out["linkedProject"] {
					out["linkState"] = "unlinked"
				}
			}
		}
	}
	return out, nil
}

// RecordObservation returns FaultLedger.record's complete receipt. Compose joins a caller's
// transaction, including adoption, occurrence, publications and notifications in one commit.
func (l *Ledger) RecordObservation(ctx context.Context, o Observation, adoption *Adoption) (map[string]any, error) {
	var answer map[string]any
	err := l.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		workspace, _ := o.Scope["workspace"].(string)
		id, err := l.CanonicalID(ctx, o.Product, o.FaultClass, o.Signature, workspace)
		if err != nil {
			return err
		}
		before, err := l.Store.All(ctx, "SELECT publication_id,state FROM fault_publications WHERE fault_id=? ORDER BY rowid", id)
		if err != nil {
			return err
		}
		states := map[string]string{}
		for _, r := range before {
			states[text(r, "publication_id")] = text(r, "state")
		}
		recorded, err := l.record(ctx, o, adoption)
		if err != nil {
			return err
		}
		row, err := l.Get(ctx, id)
		if err != nil {
			return err
		}
		if row == nil {
			answer = map[string]any{"faultId": id, "recorded": false, "state": nil, "occurrenceCount": 0, "publication": nil, "reason": "a clearing observation for a fault that was never recorded"}
			return nil
		}
		if !recorded {
			reason := "this occurrence was already recorded in this episode"
			if o.Cleared && (row["state"] == Resolved || row["state"] == Withdrawn) {
				reason = "this fault is already closed"
			}
			answer = map[string]any{"faultId": id, "recorded": false, "state": row["state"], "occurrenceCount": row["occurrence_count"], "reason": reason, "publication": nil}
			return nil
		}
		var published any
		rows, err := l.Store.All(ctx, "SELECT * FROM fault_publications WHERE fault_id=? ORDER BY rowid", id)
		if err != nil {
			return err
		}
		for _, r := range rows {
			pid := text(r, "publication_id")
			prior, existed := states[pid]
			if existed && prior != "cancelled" {
				continue
			}
			if text(r, "kind") == "update_record" {
				continue
			}
			published, err = l.publicationReceipt(ctx, r, existed)
			if err != nil {
				return err
			}
			break
		}
		answer = map[string]any{"faultId": id, "recorded": true, "state": row["state"], "cycle": row["cycle"], "severity": row["severity"], "occurrenceCount": row["occurrence_count"], "suppression": loadsMap(row["suppression"].(string)), "publication": published}
		return nil
	})
	return answer, err
}
func (l *Ledger) publicationReceipt(ctx context.Context, r row, revived bool) (map[string]any, error) {
	kind := text(r, "kind")
	spec := kinds[kind]
	fault, err := cFault(ctx, l, text(r, "fault_id"))
	if err != nil {
		return nil, err
	}
	var target row
	why := ""
	if spec.Target != "" {
		target, why, err = f1OwnedTarget(ctx, l, fault)
		if err != nil {
			return nil, err
		}
	}
	awaiting := spec.Target != "" && target == nil
	reason := "queued"
	if revived {
		reason = "revived"
	}
	if awaiting {
		reason += fmt.Sprintf("; no target owned by %s for %s (%s), so it waits rather than being filed somewhere guessed", text(fault, "product"), text(fault, "scope_key"), why)
	}
	return map[string]any{"publicationId": text(r, "publication_id"), "kind": kind, "trigger": text(r, "trigger_key"), "queued": true, "awaitingTarget": awaiting, "awaitingRecord": spec.RequiresIssue && !named(fault.Get("external_ref")), "reason": reason}, nil
}

func (l *Ledger) Adopt(ctx context.Context, id, reference string, scope map[string]any) (any, error) {
	return f2Write(ctx, l, "fault-adopt", map[string]string{"--fault": id, "--external-ref": reference, "--scope": dumps(scope, false)})
}
func (l *Ledger) Move(ctx context.Context, id string, scope map[string]any) (any, error) {
	return f2Write(ctx, l, "fault-move", map[string]string{"--fault": id, "--scope": dumps(scope, false)})
}
func (l *Ledger) RequestUpdate(ctx context.Context, id, op string, value any) (any, error) {
	return f2Write(ctx, l, "fault-update", map[string]string{"--fault": id, "--op": op, "--value": dumps(value, false)})
}
func (l *Ledger) Queue(ctx context.Context, id, kind, trigger string, payload any) (any, error) {
	a := map[string]string{"--fault": id, "--kind": kind, "--trigger": trigger}
	if payload != nil {
		a["--payload"] = dumps(payload, false)
	}
	return cQueue(ctx, l, a)
}
func (l *Ledger) Cancel(ctx context.Context, id, reason string) (any, error) {
	return cRetryCancel(ctx, l, "fault-cancel", map[string]string{"--publication": id, "--reason": reason})
}
func (l *Ledger) Publication(ctx context.Context, id string) (map[string]any, error) {
	return cPublication(ctx, l, id)
}
func (l *Ledger) Publications(ctx context.Context, id string, kind, state any, limit int, after any) ([]any, error) {
	bound, err := cLimit(ctx, strconv.Itoa(limit), "limit", 20)
	if err != nil {
		return nil, err
	}
	if after == nil {
		after = 0
	}
	alias, err := l.one(ctx, "SELECT fault_id FROM fault_aliases WHERE alias_id=?", id)
	if err != nil {
		return nil, err
	}
	if alias != nil {
		id = text(alias, "fault_id")
	}
	rows, err := l.Store.All(ctx, "SELECT publication_id FROM fault_publications WHERE fault_id=? AND (? IS NULL OR kind=?) AND (? IS NULL OR state=?) AND rowid>? ORDER BY rowid LIMIT ?", id, kind, kind, state, state, after, bound)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, r := range rows {
		v, err := l.Publication(ctx, text(r, "publication_id"))
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
func (l *Ledger) Next(ctx context.Context, limit int) ([]any, error) {
	answer, err := cNext(ctx, l, map[string]string{"--limit": strconv.Itoa(limit)})
	if err != nil {
		return nil, err
	}
	return answer.(map[string]any)["publications"].([]any), nil
}
func (l *Ledger) Claim(ctx context.Context, id, owner string, takeover bool) (any, error) {
	a := map[string]string{"--publication": id, "--owner": owner}
	if takeover {
		a["--takeover"] = "true"
	}
	return f1Claim(ctx, l, a)
}
func (l *Ledger) Operation(ctx context.Context, id, token string) (any, error) {
	return f1Operation(ctx, l, map[string]string{"--publication": id, "--claim-token": token})
}
func (l *Ledger) Complete(ctx context.Context, id, token, readback, reference, project string, observed any) (any, error) {
	a := map[string]string{"--publication": id, "--claim-token": token, "--readback": readback, "--external-ref": reference, "--project-ref": project}
	if observed != nil {
		a["--observed-fields"] = dumps(observed, false)
	}
	return f1Complete(ctx, l, a)
}
func (l *Ledger) Fail(ctx context.Context, id, token, message string, ended bool) (any, error) {
	a := map[string]string{"--publication": id, "--claim-token": token, "--error": message}
	if ended {
		a["--ended"] = "true"
	}
	return f2Fail(ctx, l, a)
}
func (l *Ledger) Reconcile(ctx context.Context, id, observedText string, searched bool, observed any, priorEnded bool, reason string) (any, error) {
	a := map[string]string{"--publication": id, "--observed": observedText, "--reason": reason}
	if searched {
		a["--searched"] = "true"
	}
	if priorEnded {
		a["--prior-ended"] = "true"
	}
	if observed != nil {
		a["--observed-fields"] = dumps(observed, false)
	}
	return f1Reconcile(ctx, l, a)
}
func (l *Ledger) ExpireLeases(ctx context.Context) (map[string]any, error) {
	answer := map[string]any{"released": 0, "uncertain": 0, "more": false}
	err := l.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		rows, err := l.Store.All(ctx, "SELECT state FROM fault_publications WHERE state IN ('claimed','issued') AND lease_until IS NOT NULL AND lease_until<=? ORDER BY lease_until,rowid LIMIT 101", l.Clock.Now())
		if err != nil {
			return err
		}
		answer["more"] = len(rows) > 100
		for _, row := range rows[:min(len(rows), 100)] {
			key := "released"
			if text(row, "state") == "issued" {
				key = "uncertain"
			}
			answer[key] = answer[key].(int) + 1
		}
		return l.expireLeases(ctx)
	})
	return answer, err
}
func (l *Ledger) Notify(ctx context.Context, id, reason, ref string) (any, error) {
	return dRaise(ctx, l, map[string]string{"--fault": id, "--reason": reason, "--ref": ref})
}
func (l *Ledger) Notifications(ctx context.Context, state string, limit int, after any) (any, error) {
	a := map[string]string{"--limit": strconv.Itoa(limit)}
	if state != "" {
		a["--notification-state"] = state
	}
	if after != nil {
		a["--after"] = fmt.Sprint(after)
	}
	return dNotifications(ctx, l, a)
}
func (l *Ledger) Snapshot(ctx context.Context, product, class, state string, limit int, after any) (any, error) {
	a := map[string]string{"--limit": strconv.Itoa(limit)}
	for k, v := range map[string]string{"--product": product, "--fault-class": class, "--fault-state": state} {
		if v != "" {
			a[k] = v
		}
	}
	if after != nil {
		a["--after"] = fmt.Sprint(after)
	}
	answer, err := cShow(ctx, l, a)
	if err != nil {
		return nil, err
	}
	for _, v := range answer.(map[string]any)["faults"].([]any) {
		m := v.(map[string]any)
		fresh, err := l.Get(ctx, m["fault_id"].(string))
		if err != nil {
			return nil, err
		}
		m["linkState"], m["linkedProject"] = fresh["linkState"], fresh["linkedProject"]
	}
	return answer, nil
}
func (l *Ledger) Remediations(ctx context.Context, id string, limit int) ([]any, error) {
	rows, err := l.Store.All(ctx, "SELECT * FROM fault_remediations WHERE fault_id=COALESCE((SELECT fault_id FROM fault_aliases WHERE alias_id=?),?) ORDER BY rowid DESC LIMIT ?", id, id, min(limit, 1000))
	if err != nil {
		return nil, err
	}
	slices.Reverse(rows)
	return cList(rows), nil
}

// RecordRemediation includes the detail the library caller supplies, with the same core
// remediation transition the CLI uses. The enclosing Compose keeps the summary atomic.
func (l *Ledger) RecordRemediation(ctx context.Context, id, kind, ref, method, outcome, detail string) (map[string]any, error) {
	var answer map[string]any
	err := l.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		canonical, err := cFault(ctx, l, id)
		if err != nil {
			return err
		}
		id = text(canonical, "fault_id")
		rid, recorded, err := l.Remediate(ctx, id, kind, ref, method, outcome)
		if err != nil {
			return err
		}
		if recorded {
			if _, err = l.exec(ctx, "UPDATE fault_remediations SET detail=? WHERE remediation_id=?", detail, rid); err != nil {
				return err
			}
		}
		answer = map[string]any{"faultId": id, "recorded": recorded, "remediationId": rid, "state": FixPending}
		if !recorded {
			reason := "this remediation was already recorded"
			if kind == "reverification" {
				reason = "this check was already the latest remediation"
			}
			answer["reason"] = reason
			return nil
		}
		var publication any
		if kind == "fix" {
			fault, err := cFault(ctx, l, id)
			if err != nil {
				return err
			}
			holder, create, err := issueSlot(ctx, l, fault)
			if err != nil {
				return err
			}
			trigger := triggerFix + ":" + rid[:12]
			if err = l.enqueueWithRemediation(ctx, id, trigger, l.Clock.ISO(), holder, create, rid); err != nil {
				return err
			}
			row, err := l.one(ctx, "SELECT * FROM fault_publications WHERE publication_id=?", publicationID(id, appendComment, trigger))
			if err != nil {
				return err
			}
			if row != nil {
				publication, err = l.publicationReceipt(ctx, row, false)
				if err != nil {
					return err
				}
			}
		}
		answer["publication"] = publication
		return nil
	})
	return answer, err
}
func (l *Ledger) ResolveRecord(ctx context.Context, id string) (map[string]any, error) {
	var answer map[string]any
	err := l.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		r, err := cFault(ctx, l, id)
		if err != nil {
			return err
		}
		id = text(r, "fault_id")
		resolved, err := l.Resolve(ctx, id)
		if err != nil {
			return err
		}
		answer = map[string]any{"faultId": id, "state": Resolved, "resolved": resolved}
		if !resolved {
			answer["reason"] = "already resolved"
			return nil
		}
		cycle := integer(r, "cycle")
		answer["cycle"] = cycle
		var publication any
		row, err := l.one(ctx, "SELECT * FROM fault_publications WHERE publication_id=?", publicationID(id, appendComment, fmt.Sprintf("resolve:%d", cycle)))
		if err != nil {
			return err
		}
		if row != nil {
			publication, err = l.publicationReceipt(ctx, row, false)
			if err != nil {
				return err
			}
		}
		answer["publication"] = publication
		return nil
	})
	return answer, err
}

func (l *Ledger) SetPolicy(ctx context.Context, product, class, severity string, threshold, window any, reason string) (any, error) {
	a := map[string]string{"--product": product, "--fault-class": class, "--severity": severity, "--reason": reason}
	if threshold != nil {
		a["--threshold"] = fmt.Sprint(threshold)
	}
	if window != nil {
		a["--window"] = fmt.Sprint(window)
	}
	return dSetPolicy(ctx, l, a)
}
func (l *Ledger) SetLimit(ctx context.Context, product, kind string, maximum, window any) (any, error) {
	var answer any
	err := l.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		var err error
		answer, err = dSetLimit(ctx, l, map[string]string{"--product": product, "--kind": kind, "--max-count": fmt.Sprint(maximum), "--window": fmt.Sprint(window)})
		if err != nil {
			return err
		}
		// The journal keeps the library caller's number spelling; the receipt reads the
		// persisted REAL. Argparse supplies a float, direct callers may supply an integer.
		_, err = l.exec(ctx, "UPDATE journal SET detail=? WHERE rowid=(SELECT MAX(rowid) FROM journal WHERE kind='fault_limit_set' AND subject=?)", dumps(map[string]any{"maxCount": maximum, "window": window}, false), product+":"+kind)
		return err
	})
	return answer, err
}

// Querier exposes only the same transaction-aware store reader the ledger uses.
func (l *Ledger) Querier(ctx context.Context) store.Querier { return l.Store.Querier(ctx) }
