package faults

import (
	"context"
	"database/sql"
	"fmt"
)

// Resolve closes a fix only after its latest verification has passed and no newer
// occurrence has contradicted it. Sequence order, not caller timestamps, decides.
func (l *Ledger) Resolve(ctx context.Context, identifier string) (bool, error) {
	resolved := false
	err := l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		fault, err := cFault(ctx, l, identifier)
		if err != nil {
			return err
		}
		identifier = fault.Text("fault_id")
		if fault.Text("state") == Resolved {
			return nil
		}
		if fault.Text("state") != FixPending {
			return fmt.Errorf("fault_state_conflict: a fault is resolved from fix_pending and this one is %s", fault.Text("state"))
		}
		cycle := integer(fault, "cycle")
		fix, err := l.one(ctx, "SELECT MAX(seq) AS seq FROM fault_timeline WHERE fault_id = ? AND cycle = ? AND kind = 'fix'", identifier, cycle)
		if err != nil {
			return err
		}
		if fix.Get("seq") == nil {
			return fmt.Errorf("fault_unverified: no fix is recorded for this cycle, so there is nothing to have verified")
		}
		latest, err := l.one(ctx, "SELECT t.seq, r.outcome FROM fault_timeline t JOIN fault_remediations r ON r.remediation_id = t.ref_id WHERE t.fault_id = ? AND t.cycle = ? AND t.kind = 'reverification' ORDER BY t.seq DESC LIMIT 1", identifier, cycle)
		if err != nil {
			return err
		}
		if latest == nil {
			return fmt.Errorf("fault_unverified: a fix alone does not resolve a fault. Record a reverification that looked for it after the fix")
		}
		if integer(latest, "seq") <= integer(fix, "seq") {
			return fmt.Errorf("fault_verification_stale: the newest reverification was recorded BEFORE the newest fix")
		}
		installed, err := l.one(ctx, "SELECT MAX(seq) AS seq FROM fault_timeline WHERE fault_id = ? AND cycle = ? AND kind = 'installed' AND seq > ?", identifier, cycle, integer(fix, "seq"))
		if err != nil {
			return err
		}
		if installed.Get("seq") != nil && integer(installed, "seq") > integer(latest, "seq") {
			return fmt.Errorf("fault_verification_stale: the fix was installed after the newest reverification")
		}
		if outcome := latest.Text("outcome"); outcome != "passed" && outcome != "absent" {
			return fmt.Errorf("fault_unverified: the newest reverification reported %q", outcome)
		}
		recurrence, err := l.one(ctx, "SELECT MIN(seq) AS seq FROM fault_timeline WHERE fault_id = ? AND kind = 'occurrence' AND seq > ?", identifier, integer(latest, "seq"))
		if err != nil {
			return err
		}
		if recurrence.Get("seq") != nil {
			return fmt.Errorf("fault_recurred_after_verification: the fault was observed again after that reverification")
		}
		now := l.Clock.ISO()
		_, err = l.exec(ctx, "UPDATE fault_ledger SET state = ?, resolved_at = ?, updated_at = ?, episode = episode + 1 WHERE fault_id = ?", Resolved, now, now, identifier)
		if err != nil {
			return err
		}
		holder, create, err := issueSlot(ctx, l, fault)
		if err != nil {
			return err
		}
		if err = l.enqueue(ctx, identifier, fmt.Sprintf("%s:%d", triggerResolve, cycle), now, holder, create, ""); err != nil {
			return err
		}
		if err = l.notifyKind(ctx, identifier, "resolved", cycle, now); err != nil {
			return err
		}
		resolved = true
		return nil
	})
	return resolved, err
}
