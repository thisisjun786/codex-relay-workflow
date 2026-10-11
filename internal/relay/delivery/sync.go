package delivery

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The verdict's sync obligation (sync.py enqueue_verdict_in) and the claim fence the outbox's
// completion rests on (claim, _fenced). The rest of the outbox - next, complete's readback
// validation, fail, retry - is todo 23's.
const coordinationDocument = "coordination_document"

func syncCanonical(target, targetRef, kind, rid string, event, generation, revision, verdict, digest, ruling any) string {
	render := func(v any) string {
		if v == nil {
			return "null"
		}
		return pyStr(v)
	}
	parts := []string{target, targetRef, kind, rid, render(event), render(generation), render(revision), render(verdict)}
	payload := strings.Join(parts, "|")
	if digest != nil {
		payload += "|criteria=" + render(digest)
	}
	if ruling != nil {
		payload += "|ruling=" + render(ruling)
	}
	return payload
}

func renderVerdictSummary(r Relationship, event Row, verdict string, findings []any, record Obj, digest any, ruling int64) string {
	lines := []string{
		fmt.Sprintf("%s · %s · %s", r.IssueKey, r.Child.TaskID, verdict),
		fmt.Sprintf("generation %d, revision %s", event.I("execution_generation"), event.S("revision_hash")[:12]),
	}
	if d, ok := digest.(string); ok && d != "" {
		line := "criteria set " + d[:12]
		if ruling > 1 {
			line += fmt.Sprintf(", ruling %d", ruling)
		} else if ruling == 1 {
			line += ", ruling 1"
		}
		lines = append(lines, line)
	} else if ruling > 1 {
		// a ruling that replaced another on a relationship with no criteria set (CRW-404): without the
		// ordinal the two blocks could not be told apart by which is current when they post out of order
		lines = append(lines, fmt.Sprintf("ruling %d", ruling))
	}
	if next, _ := record.Lookup("nextExecutionGeneration"); truthy(next) {
		lines = append(lines, fmt.Sprintf("a revision request was queued to the same child under generation %s", pyStr(next)))
	}
	if len(findings) > 0 {
		lines = append(lines, "findings:")
		for _, f := range findings {
			o := f.(Obj)
			line := "  " + pyjson.Text(o.Get("id")) + ": " + pyjson.Text(o.Get("verdict"))
			if note := pyjson.Text(o.Get("note")); note != "" {
				line += " — " + note
			}
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// planTables reports whether the store holds the tables a plan relationship is read from (dag_node_executions, dag_nodes and dag_summary_outbox); a store without them has no plan relationship.
func planTables(ctx context.Context, q store.Querier) (bool, error) {
	var tables int
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('dag_node_executions', 'dag_nodes', 'dag_summary_outbox')").Scan(&tables); err != nil {
		return false, err
	}
	return tables == 3, nil
}

// planOfRelationship is the plan whose summary outbox projects the verdicts of this relationship to Linear (dagsched/outbox.go, docs/relay/dag-outbox.md): the store holds the summary outbox and a plan runs
// the relationship on a node the plan still holds, so a row of the plan's summary states the verdict. The binding is the relationship's own row of dag_node_executions (every release, correction and
// replacement of a plan node leaves one); a node the plan retired or replaced has no row in any summary, so the relationship of such a node is not a plan relationship and keeps the sync_outbox row it always
// had. A relationship that no plan runs, and a store without either table, answer "". Only VerdictSync asks it: the live-node rule decides whether a new verdict enqueues a row, and does not
// decide whether an entry that was made while the node was live has carried a verdict (PlanSummaryCarriesVerdict).
func planOfRelationship(ctx context.Context, q store.Querier, relationshipID string) (string, error) {
	if ok, err := planTables(ctx, q); err != nil || !ok {
		return "", err
	}
	var plan string
	err := q.QueryRowContext(ctx, "SELECT x.plan_id FROM dag_node_executions x WHERE x.relationship_id = ?"+
		" AND EXISTS (SELECT 1 FROM dag_nodes n WHERE n.plan_id = x.plan_id AND n.node_id = x.node_id AND n.retired_rev IS NULL)"+
		" ORDER BY x.execution_generation DESC LIMIT 1", relationshipID).Scan(&plan)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return plan, err
}

// PlanSummaryCarriesVerdict is the id of the confirmed summary entry that carries the verdict of this event, or "" when none does. The verdict of a relationship a plan runs enqueues no sync_outbox row (VerdictSync),
// so the record the supervisor reads for it is the plan's summary: an entry of the relationship's coordination document, confirmed, enqueued at or after the instant the verdict was decided (the entry reads
// the newest verdict of the node in the transaction that enqueues it), and made at a plan revision where the node the relationship is bound to (dag_node_executions, any version of the node) was live, because
// only such an entry states a row of that node. A later retirement or replacement of the node does not undo the discharge: the confirmed entry stays in the record, as a confirmed sync_outbox row did. The
// supervisor's discharge readers (supervisor/obligation.go Reportable, proposal.go ReportableCurrent, supervisor/selection.go Selection) ask it next to their sync_outbox read, so the obligation of a plan
// relationship is discharged by the record that holds its verdict (CRW-1216).
func PlanSummaryCarriesVerdict(ctx context.Context, q store.Querier, relationshipID, eventID, document string) (string, error) {
	if ok, err := planTables(ctx, q); err != nil || !ok {
		return "", err
	}
	var id string
	err := q.QueryRowContext(ctx, "SELECT o.summary_id FROM dag_node_executions x JOIN dag_summary_outbox o ON o.plan_id = x.plan_id"+
		" WHERE x.relationship_id = ? AND o.document = ? AND o.state = 'confirmed'"+
		" AND o.created_at >= (SELECT v.decided_at FROM verdicts v WHERE v.event_id = ?)"+
		" AND EXISTS (SELECT 1 FROM dag_nodes n WHERE n.plan_id = x.plan_id AND n.node_id = x.node_id AND n.introduced_rev <= o.plan_revision AND (n.retired_rev IS NULL OR n.retired_rev > o.plan_revision))"+
		" ORDER BY o.seq, o.plan_id LIMIT 1", relationshipID, document, eventID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// VerdictSync is the SyncHook record_verdict calls inside its transaction. A relationship a DAG plan runs enqueues nothing: its verdict is a row of the plan's summary outbox (CRW-1216).
func VerdictSync(s *store.Store, clock Clock) SyncHook {
	return func(ctx context.Context, r Relationship, event Row, verdict string, findings []any, record Obj, digest any, ruling int64) error {
		target, err := one(ctx, s, "SELECT * FROM sync_targets WHERE relationship_id = ? AND target = ?", r.ID, coordinationDocument)
		if err != nil || target == nil {
			return err
		}
		if plan, err := planOfRelationship(ctx, s.Q(ctx), r.ID); err != nil || plan != "" {
			return err
		}
		var identityRuling any
		if ruling > 1 {
			identityRuling = ruling
		}
		summaryRuling := ruling
		if digest == nil && ruling <= 1 {
			summaryRuling = 0
		}
		canonical := syncCanonical(coordinationDocument, target.S("target_ref"), "verdict", r.ID, event.S("event_id"), event.I("execution_generation"), event.S("revision_hash"), verdict, digest, identityRuling)
		sum := sha256.Sum256([]byte(canonical))
		full := hex.EncodeToString(sum[:])
		id := full[:32]
		now := clock.ISO()
		inserted, err := execSQL(ctx, s, "INSERT OR IGNORE INTO sync_outbox (sync_id, relationship_id, issue_key, target, target_ref, subject_kind, event_id, execution_generation, revision_hash, verdict, identity_digest, summary, state, attempts, next_attempt_at, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,0,NULL,?,?)",
			id, r.ID, r.IssueKey, coordinationDocument, target.S("target_ref"), "verdict", event.S("event_id"), event.I("execution_generation"), event.S("revision_hash"), verdict, full, renderVerdictSummary(r, event, verdict, findings, record, digest, summaryRuling), "pending", now, now)
		if err != nil {
			return err
		}
		return journal(ctx, s, "sync_enqueued", id, Obj{{Key: "subjectKind", Value: "verdict"}, {Key: "eventId", Value: event.S("event_id")}, {Key: "criteriaDigest", Value: digest}, {Key: "ruling", Value: identityRuling}, {Key: "inserted", Value: inserted == 1}}, now)
	}
}
