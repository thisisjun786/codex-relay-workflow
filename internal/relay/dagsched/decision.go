package dagsched

import (
	"context"
	"database/sql"
	"strings"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// DecisionInput is a decision the parent records for a decision edge: what was decided (the subject and the digest the plan fixed), whether it was approved, and by whose authority. The
// authority text is opaque (decision D-09): it is stored and compared with what the edge requires, never read.
type DecisionInput struct {
	Subject, Digest, Disposition, AuthorityKind, AuthorityRef string
}

// DecisionResult is the answer of RecordDecision.
type DecisionResult struct {
	PlanID, DecisionID, Subject string
	Revision                    int64
	Replayed                    bool
	SupersededID                string
}

// RecordDecision writes a decision row. Recording is not authority: a decision edge reads the row and compares its authority kind with the kinds the edge names, so a decision recorded by
// the wrong authority records and opens nothing. Only the project's registered parent records; an identical decision is a replay; any other decision of the subject supersedes the active one
// (the history is kept and the revision counts up), so a rejection after an approval is a fact and not an overwrite.
func (s *Scheduler) RecordDecision(ctx context.Context, plan, actor string, in DecisionInput) (DecisionResult, error) {
	out := DecisionResult{PlanID: plan, Subject: in.Subject}
	switch {
	case in.Subject == "" || in.Digest == "" || in.AuthorityKind == "" || in.AuthorityRef == "":
		return out, refuse(contract.RefusalMalformedReceipt, "a decision names its subject, its digest and its authority (kind and reference)")
	case in.Disposition != "approved" && in.Disposition != "rejected":
		return out, refuse(contract.RefusalMalformedReceipt, "a decision is approved or rejected, not %q", in.Disposition)
	case utf8.RuneCountInString(in.AuthorityKind) > 512 || utf8.RuneCountInString(in.AuthorityRef) > 512 || utf8.RuneCountInString(in.Subject) > 512 || strings.TrimSpace(in.AuthorityKind) != in.AuthorityKind:
		return out, refuse(contract.RefusalMalformedReceipt, "decision text is at most 512 characters and the authority kind has no padding")
	}
	err := s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		tx := s.Store.Q(txCtx)
		snap, _, err := dag.SnapshotAt(txCtx, tx, plan, 0)
		if err != nil {
			return err
		}
		parents, err := projectParents(txCtx, tx, snap.ProjectKey)
		if err != nil {
			return err
		}
		if len(parents) != 1 || parents[0] != actor {
			return refuse(contract.RefusalScopeRoleMismatch, "task %s is not the registered parent of project %s", actor, snap.ProjectKey)
		}
		var activeID, digest, disposition, kind, ref string
		var revision int64
		active, err := queryOne(txCtx, tx, "SELECT decision_id, digest, disposition, authority_kind, authority_ref, revision FROM dag_decisions WHERE plan_id = ? AND subject = ? AND state = 'active'",
			[]any{plan, in.Subject}, &activeID, &digest, &disposition, &kind, &ref, &revision)
		if err != nil {
			return err
		}
		if active && digest == in.Digest && disposition == in.Disposition && kind == in.AuthorityKind && ref == in.AuthorityRef {
			out.DecisionID, out.Revision, out.Replayed = activeID, revision, true
			return nil
		}
		var last sql.NullInt64
		if err := tx.QueryRowContext(txCtx, "SELECT MAX(revision) FROM dag_decisions WHERE plan_id = ? AND subject = ?", plan, in.Subject).Scan(&last); err != nil {
			return err
		}
		out.Revision = last.Int64 + 1
		out.DecisionID = "dec-" + shaOf([]byte(dag.Canonical(map[string]any{"plan_id": plan, "subject": in.Subject, "digest": in.Digest, "disposition": in.Disposition,
			"authority_kind": in.AuthorityKind, "authority_ref": in.AuthorityRef, "revision": out.Revision})))[:32]
		if active {
			if _, err := tx.ExecContext(txCtx, "UPDATE dag_decisions SET state = 'superseded' WHERE decision_id = ?", activeID); err != nil {
				return err
			}
			out.SupersededID = activeID
		}
		_, err = tx.ExecContext(txCtx, "INSERT INTO dag_decisions (decision_id, plan_id, subject, digest, disposition, authority_kind, authority_ref, revision, state, recorded_by_task_id, coordinator_epoch, recorded_at) VALUES (?,?,?,?,?,?,?,?,'active',?,0,?)",
			out.DecisionID, plan, in.Subject, in.Digest, in.Disposition, in.AuthorityKind, in.AuthorityRef, out.Revision, actor, s.now())
		return err
	})
	return out, err
}
