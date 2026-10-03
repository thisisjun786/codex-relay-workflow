package faults

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// Remediate records an attempted fix or a check, retaining separate executions of the
// same check whenever another remediation has intervened.
func (l *Ledger) Remediate(ctx context.Context, identifier, kind, ref, method, outcome string) (string, bool, error) {
	if ref == "" || (kind != "fix" && kind != "reverification") || kind == "reverification" && (method != "suite" && method != "command" && method != "observation" || outcome != "passed" && outcome != "absent" && outcome != "failed") {
		return "", false, fmt.Errorf("fault_observation_malformed: invalid remediation")
	}
	id := ""
	recorded := false
	err := l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		fault, err := cFault(ctx, l, identifier)
		if err != nil {
			return err
		}
		identifier = fault.Text("fault_id")
		state := fault.Text("state")
		if kind == "fix" && state != Observed && state != Open && state != FixPending || kind == "reverification" && state != FixPending {
			return fmt.Errorf("fault_state_conflict: remediation on %s", state)
		}
		cycle := integer(fault, "cycle")
		var after any
		if kind == "reverification" {
			latest, err := l.one(ctx, "SELECT t.seq, r.kind, r.ref, r.method, r.outcome, t.ref_id FROM fault_timeline t JOIN fault_remediations r ON r.remediation_id = t.ref_id WHERE t.fault_id = ? AND t.cycle = ? AND t.kind IN ('fix','reverification') ORDER BY t.seq DESC LIMIT 1", identifier, cycle)
			if err != nil {
				return err
			}
			if latest != nil {
				if latest.Text("kind") == kind && latest.Text("ref") == ref && latest.Text("method") == method && latest.Text("outcome") == outcome {
					id = latest.Text("ref_id")
					return nil
				}
				after = integer(latest, "seq")
			}
		}
		pythonNone := func(s string) string {
			if s == "" {
				return "None"
			}
			return s
		}
		afterText := "None"
		if after != nil {
			afterText = fmt.Sprint(after)
		}
		id = pyvalue.SHA256Hex(fmt.Sprintf("%s|%d|%s|%s|%s|%s|%s", identifier, cycle, kind, ref, pythonNone(method), pythonNone(outcome), afterText))[:idWidth]
		n, err := l.exec(ctx, "INSERT OR IGNORE INTO fault_remediations (remediation_id, fault_id, cycle, kind, ref, method, outcome, detail, recorded_at) VALUES (?,?,?,?,?,?,?,?,?)", id, identifier, cycle, kind, ref, nilIfEmpty(method), nilIfEmpty(outcome), "", l.Clock.ISO())
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		recorded = true
		if _, err = l.exec(ctx, "INSERT INTO fault_timeline (fault_id, cycle, kind, ref_id, detail, recorded_at, recorded_ts) VALUES (?,?,?,?,?,?,?)", identifier, cycle, kind, id, ref, l.Clock.ISO(), l.Clock.Now()); err != nil {
			return err
		}
		_, err = l.exec(ctx, "UPDATE fault_ledger SET state = ?, updated_at = ? WHERE fault_id = ?", FixPending, l.Clock.ISO(), identifier)
		if err != nil || kind != "fix" {
			return err
		}
		holder, create, err := issueSlot(ctx, l, fault)
		if err != nil {
			return err
		}
		return l.enqueueWithRemediation(ctx, identifier, triggerFix+":"+id[:12], l.Clock.ISO(), holder, create, id)
	})
	return id, recorded, err
}
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
