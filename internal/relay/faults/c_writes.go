package faults

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
)

func cRetryCancel(ctx context.Context, l *Ledger, name string, a map[string]string) (any, error) {
	id := a["--publication"]
	r, e := l.one(ctx, "SELECT * FROM fault_publications WHERE publication_id=?", id)
	if e != nil {
		return nil, e
	}
	if r == nil {
		return nil, fmt.Errorf("fault_unknown: no publication %q", id)
	}
	state := r.Text("state")
	if name == "fault-retry" {
		if state != "failed" {
			suffix := ""
			if state == "uncertain" {
				suffix = ". An uncertain write is reconciled, not retried"
			}
			return nil, fmt.Errorf("fault_not_claimable: retry offers a failed publication again; this one is %s%s", state, suffix)
		}
		f, e := cFault(ctx, l, r.Text("fault_id"))
		if e != nil {
			return nil, e
		}
		if r.Text("kind") == openRecord && f.Get("external_ref") != nil {
			return nil, fmt.Errorf("fault_state_conflict: this fault already owns an issue; its create is not retried")
		}
		_, e = l.exec(ctx, "UPDATE fault_publications SET state='pending',attempts=0,next_attempt_at=NULL,updated_at=? WHERE publication_id=?", l.Clock.ISO(), id)
		return map[string]any{"publicationId": id, "state": "pending"}, e
	}
	reason := a["--reason"]
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("fault_observation_malformed: a cancel says why")
	}
	if state != "pending" && state != "failed" && state != "claimed" {
		return nil, fmt.Errorf("fault_not_claimable: a %s write may already have reached the connector; it is reconciled, never cancelled", state)
	}
	e = l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		if state == "claimed" {
			attempt, e := l.one(ctx, "SELECT attempt_id FROM fault_publication_attempts WHERE publication_id=? ORDER BY attempt_id DESC LIMIT 1", id)
			if e != nil {
				return e
			}
			if attempt != nil {
				f, e := cFault(ctx, l, r.Text("fault_id"))
				if e != nil {
					return e
				}
				_, e = l.exec(ctx, "DELETE FROM fault_budget_uses WHERE product=? AND kind=? AND ref=?", f.Text("product"), r.Text("kind"), fmt.Sprintf("%s:%d", id, integer(attempt, "attempt_id")))
				if e != nil {
					return e
				}
				_, e = l.exec(ctx, "UPDATE fault_publication_attempts SET outcome='cancelled',ended=1,ended_at=? WHERE attempt_id=?", l.Clock.ISO(), integer(attempt, "attempt_id"))
				if e != nil {
					return e
				}
			}
		}
		attempts := integer(r, "attempts")
		if state == "claimed" {
			attempts--
		}
		_, e := l.exec(ctx, "UPDATE fault_publications SET state='cancelled',claim_token=NULL,lease_owner=NULL,lease_until=NULL,last_error=?,updated_at=?,attempts=? WHERE publication_id=?", reason, l.Clock.ISO(), attempts, id)
		return e
	})
	return map[string]any{"publicationId": id, "state": "cancelled", "reason": reason}, e
}
func cStage(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	stage, ref := a["--stage"], a["--ref"]
	allowed := map[string]bool{"accepted": true, "assigned": true, "merged": true, "installed": true}
	if !allowed[stage] {
		return nil, fmt.Errorf("fault_observation_malformed: stage %s is not one of accepted, assigned, merged, installed", quote.Value(stage))
	}
	if strings.TrimSpace(ref) == "" {
		return nil, fmt.Errorf("fault_observation_malformed: a stage names what it refers to")
	}
	f, e := cFault(ctx, l, a["--fault"])
	if e != nil {
		return nil, e
	}
	state := f.Text("state")
	if state == Resolved || state == Withdrawn {
		return nil, fmt.Errorf("fault_state_conflict: this fault is %s", state)
	}
	if (stage == "accepted" || stage == "assigned") && f.Get("external_ref") == nil {
		return nil, fmt.Errorf("fault_state_conflict: %s is recorded against an issue the fault owns, and it owns none yet", stage)
	}
	if stage == "merged" || stage == "installed" {
		r, e := l.one(ctx, "SELECT 1 FROM fault_timeline WHERE fault_id=? AND cycle=? AND kind='fix'", f.Text("fault_id"), integer(f, "cycle"))
		if e != nil {
			return nil, e
		}
		if r == nil {
			return nil, fmt.Errorf("fault_state_conflict: %s follows a fix, and none is recorded this cycle", stage)
		}
	}
	id := pyvalue.SHA256Hex(fmt.Sprintf("%s|%d|%s|%s", f.Text("fault_id"), integer(f, "cycle"), stage, ref))[:idWidth]
	recorded := false
	e = l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		n, e := l.exec(ctx, "INSERT OR IGNORE INTO fault_remediations(remediation_id,fault_id,cycle,kind,ref,method,outcome,detail,recorded_at) VALUES(?,?,?,?,?,NULL,NULL,?,?)", id, f.Text("fault_id"), integer(f, "cycle"), stage, ref, a["--detail"], l.Clock.ISO())
		if e != nil {
			return e
		}
		recorded = n == 1
		if recorded {
			_, e = l.exec(ctx, "INSERT INTO fault_timeline(fault_id,cycle,kind,ref_id,detail,recorded_at,recorded_ts) VALUES(?,?,?,?,?,?,?)", f.Text("fault_id"), integer(f, "cycle"), stage, id, ref, l.Clock.ISO(), l.Clock.Now())
		}
		return e
	})
	return map[string]any{"faultId": f.Text("fault_id"), "stage": stage, "ref": ref, "recorded": recorded, "remediationId": id}, e
}
func cQueue(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	kind := a["--kind"]
	if _, ok := executableKind(kind); !ok {
		return nil, fmt.Errorf("fault_kind_unregistered: kind %s is not registered in this process; load the module that declares it (--kind-module) before acting on its writes", quote.Value(kind))
	}
	if kind == openRecord {
		return nil, fmt.Errorf("fault_state_conflict: only suppression opens a fault's issue; queue() never creates one")
	}
	if kind == "update_record" {
		return nil, fmt.Errorf("fault_state_conflict: update_record is queued through request_update(), never queue()")
	}
	if spec := kinds[kind]; kind != appendComment {
		return queueKind(ctx, l, a, spec)
	}
	trigger := a["--trigger"]
	if strings.TrimSpace(trigger) == "" || strings.Contains(trigger, "|") {
		return nil, fmt.Errorf("fault_observation_malformed: a trigger is a non-blank string without '|'")
	}
	f, e := cFault(ctx, l, a["--fault"])
	if e != nil {
		return nil, e
	}
	id := f.Text("fault_id")
	publication := publicationID(id, kind, trigger)
	prior, e := l.one(ctx, "SELECT state FROM fault_publications WHERE publication_id=?", publication)
	if e != nil {
		return nil, e
	}
	if prior != nil && prior.Text("state") != "cancelled" {
		return map[string]any{"publicationId": publication, "kind": kind, "trigger": trigger, "queued": false, "awaitingTarget": false, "awaitingRecord": f.Get("external_ref") == nil, "reason": "this reason was already queued"}, nil
	}
	e = l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		return l.insertPublication(ctx, id, kind, trigger, l.Clock.ISO(), "")
	})
	if e != nil {
		return nil, e
	}
	reason := "queued"
	if prior != nil {
		reason = "revived"
	}
	return map[string]any{"publicationId": publication, "kind": kind, "trigger": trigger, "queued": true, "awaitingTarget": false, "awaitingRecord": f.Get("external_ref") == nil, "reason": reason}, nil
}
