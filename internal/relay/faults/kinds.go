package faults

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Static kinds replace Python's dynamic --kind-module registration. The project_create
// declaration is sourced from projects.py; todo 23 owns its publication implementation.
type KindPolicy struct {
	Creates, RequiresIssue bool
	Target, Evidence       string
	// PreIssue is a read-only hook, run under a rolled-back savepoint before issue.
	// Todo 23 supplies project_create's hook and publication implementation.
	PreIssue func(map[string]any) any
	Validate func(any) []string
	Confirm  func(expected, observed any) []string
}

// kindPolicy keeps the built-in declarations and existing callers source-compatible.
type kindPolicy = KindPolicy

// RegisterKind installs an implementation for a declared extension kind. The manifest
// describes project_create, but without its todo 23 implementation it is not executable.
func RegisterKind(name string, policy KindPolicy) error {
	if policy.Creates && policy.Evidence != "block" {
		return fmt.Errorf("a kind that creates is confirmed by its block, never by fields")
	}
	kinds[name] = policy
	return nil
}

func executableKind(name string) (kindPolicy, bool) {
	policy, ok := kinds[name]
	return policy, ok && (name != "project_create" || policy.PreIssue != nil)
}

// queueKind is the extension create path: one create per kind and fault, even
// when the notice has never crossed the issue-opening threshold.
func queueKind(ctx context.Context, l *Ledger, a map[string]string, spec kindPolicy) (any, error) {
	trigger := a["--trigger"]
	if !named(trigger) || strings.Contains(trigger, "|") {
		return nil, fmt.Errorf("fault_observation_malformed: a trigger is a non-blank string without '|'")
	}
	var payload any
	var err error
	if a["--payload"] != "" {
		payload, err = loads(a["--payload"])
		if err != nil {
			return nil, err
		}
	}
	if spec.Validate != nil {
		if problems := spec.Validate(payload); len(problems) > 0 {
			return nil, fmt.Errorf("fault_observation_malformed: %s", strings.Join(problems, "; "))
		}
	}
	kind := a["--kind"]
	var answer any
	err = l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		fault, e := cFault(ctx, l, a["--fault"])
		if e != nil {
			return e
		}
		id := text(fault, "fault_id")
		if spec.Creates {
			trigger = "create"
			live, e := l.one(ctx, "SELECT publication_id FROM fault_publications WHERE fault_id=? AND kind=? AND state!='cancelled'", id, kind)
			if e != nil {
				return e
			}
			if live != nil {
				answer = map[string]any{"publicationId": live.Get("publication_id"), "kind": kind, "trigger": trigger, "queued": false, "reason": fmt.Sprintf("one %s per fault; this one is already queued", kind)}
				return nil
			}
		}
		pub := publicationID(id, kind, trigger)
		prior, e := l.one(ctx, "SELECT state FROM fault_publications WHERE publication_id=?", pub)
		if e != nil {
			return e
		}
		queued := prior == nil || text(prior, "state") == cancelled
		reason := "this reason was already queued"
		if queued {
			reason = "queued"
			if prior != nil {
				reason = "revived"
			}
			if e = l.insertPublication(ctx, id, kind, trigger, l.Clock.ISO(), ""); e != nil {
				return e
			}
			if payload != nil {
				if _, e = l.exec(ctx, "UPDATE fault_publication_payloads SET payload=? WHERE publication_id=?", dumps(payload, false), pub); e != nil {
					return e
				}
			}
		}
		var target row
		why := ""
		if spec.Target != "" {
			target, why, e = f1OwnedTarget(ctx, l, fault)
			if e != nil {
				return e
			}
		}
		awaiting := spec.Target != "" && target == nil
		if awaiting {
			reason += fmt.Sprintf("; no target owned by %s for %s (%s), so it waits rather than being filed somewhere guessed", text(fault, "product"), text(fault, "scope_key"), why)
		}
		answer = map[string]any{"publicationId": pub, "kind": kind, "trigger": trigger, "queued": queued, "awaitingTarget": awaiting, "awaitingRecord": spec.RequiresIssue && !named(fault.Get("external_ref")), "reason": reason}
		return nil
	})
	return answer, err
}

// preIssue runs the extension on the operation's connection. SQLite total_changes
// includes writes subsequently rolled back; a hook cannot hide a write by undoing it.
func preIssue(ctx context.Context, l *Ledger, spec kindPolicy, r, fault row, moment float64, stamp string) (refusal, err error) {
	if spec.PreIssue == nil {
		return nil, nil
	}
	publication, err := cPublication(ctx, l, text(r, "publication_id"))
	if err != nil {
		return nil, err
	}
	view := cView(fault)
	view["linkState"], view["linkedProject"] = "none", nil
	if named(fault.Get("external_ref")) {
		view["linkState"] = "unlinked"
		link, e := l.one(ctx, "SELECT * FROM fault_links WHERE fault_id=?", text(fault, "fault_id"))
		if e != nil {
			return nil, e
		}
		if link != nil {
			view["linkedProject"] = link.Get("observed_project_ref")
			view["linkState"] = link.Get("state")
			target, _, e := f1OwnedTarget(ctx, l, fault)
			if e != nil {
				return nil, e
			}
			if view["linkState"] == "linked" && (target == nil || target.Get("project_ref") == nil || target.Get("project_ref") != view["linkedProject"]) {
				view["linkState"] = "unlinked"
			}
		}
	}
	db := l.Store.Q(ctx)
	if _, err = db.ExecContext(ctx, "SAVEPOINT fault_pre_issue"); err != nil {
		return nil, err
	}
	var before, after int64
	var answer any
	// Always release the savepoint, including when a hook panics. The enclosing
	// transaction owns panic propagation and rollback.
	func() {
		defer func() {
			scanErr := db.QueryRowContext(ctx, "SELECT total_changes()").Scan(&after)
			_, rollbackErr := db.ExecContext(ctx, "ROLLBACK TO fault_pre_issue")
			_, releaseErr := db.ExecContext(ctx, "RELEASE fault_pre_issue")
			err = errors.Join(err, scanErr, rollbackErr, releaseErr)
		}()
		if err = db.QueryRowContext(ctx, "SELECT total_changes()").Scan(&before); err != nil {
			return
		}
		answer = spec.PreIssue(map[string]any{"publication": publication, "fault": view, "db": db, "store": l.Store, "context": ctx, "now": moment})
	}()
	if err != nil {
		return nil, err
	}
	kind := text(r, "kind")
	if before != after {
		return nil, fmt.Errorf("fault_not_claimable: the %s pre-issue check wrote to the store; its writes were discarded and nothing was issued", kind)
	}
	if e, ok := answer.(error); ok {
		return nil, e
	}
	if result, ok := answer.(map[string]any); ok {
		if reason, ok := result["cancel"]; ok {
			if err = f1CancelClaim(ctx, l, r, stamp, pyStr(reason)); err != nil {
				return nil, err
			}
			if kind == "update_record" {
				target, _, e := f1OwnedTarget(ctx, l, fault)
				if e != nil {
					return nil, e
				}
				if target != nil && named(target.Get("project_ref")) {
					err = dRelinkOne(ctx, l, text(fault, "fault_id"), text(target, "project_ref"), stamp)
				} else {
					err = dUnlinkOne(ctx, l, text(fault, "fault_id"), stamp)
				}
				if err != nil {
					return nil, err
				}
			}
			return fmt.Errorf("fault_not_claimable: cancelled before issue: %s", pyStr(reason)), nil
		}
		if reason, ok := result["hold"]; ok {
			seconds := f1Number(result["seconds"])
			if value, ok := result["seconds"].(int); ok {
				seconds = float64(value)
			}
			if seconds <= 0 {
				seconds = 30
			}
			if err = f1Release(ctx, l, r, stamp, "held", moment+seconds, pyStr(reason)); err != nil {
				return nil, err
			}
			return fmt.Errorf("fault_not_claimable: held before issue: %s", pyStr(reason)), nil
		}
	}
	if answer != nil {
		return nil, fmt.Errorf("fault_not_claimable: the %s pre-issue check answered %s", kind, f1Repr(answer))
	}
	return nil, nil
}

// PreIssueDB is the transaction-bound connection exposed to registered hooks.
// It deliberately permits SQL writes so the savepoint can detect and discard them.
type PreIssueDB interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

var kinds = map[string]kindPolicy{
	"append_comment": {RequiresIssue: true, Evidence: "block"},
	"open_record":    {Creates: true, Target: "team+project", Evidence: "block"},
	"project_create": {Creates: true, Target: "team", Evidence: "block"},
	"update_record":  {RequiresIssue: true, Evidence: "fields"},
}
