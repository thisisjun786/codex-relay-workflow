package managed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Reservation implements registry.py's durable managed request exclusion on one store.
// This subset is ported for todo 27; registry's managed admission extends it.
type Reservation struct {
	Store *store.Store
	Now   func() string
}
type Identity struct{ RequestID, IssueKey, Fingerprint, Version, Workspace, MarkerRoot, SocketIdentity, CreateRequestID, DispatchRequestID string }

func (r Reservation) now() string {
	if r.Now != nil {
		return r.Now()
	}
	return ""
}
func refusal(reason, detail string) error { return &store.RefusedError{Reason: reason, Detail: detail} }

// The packet identity of a managed reservation (CRW-839). A feature issue may be delivered by several
// packets, so a second reservation of one issue is admitted when it is a different packet of the same
// plan. A packet is resolved from the release intent the request recorded (dag_releases to the live
// node's dag_node_packets row) or, for a relationship that already exists, from dag_execution_packets.
// Every one of these reads answers false when anything cannot be resolved: a managed start that is not a
// DAG release, a node without a packet, a store that predates the packet tables. A false is the
// caller's refusal, so an unresolvable pair never slips through.

// releasedPacket is the plan and packet a managed request was released under, resolved through the SAME
// registration the registry guard uses (registry.releasedPacket): a live node version of the plan that
// carries the packet, for this issue, whether the release intent is the initial one (dag_releases) or the
// successor a rerelease took (dag_release_recoveries). found is false when anything cannot be resolved, and
// the caller then refuses as it always did.
func (r Reservation) releasedPacket(ctx context.Context, requestID, issueKey string) (plan, packet string, found bool) {
	q := r.Store.Querier(ctx)
	err := q.QueryRowContext(ctx, "SELECT r.plan_id, p.packet_id FROM dag_releases r"+
		" JOIN dag_node_packets p ON p.plan_id = r.plan_id AND p.node_id = r.node_id"+
		" JOIN dag_nodes n ON n.plan_id = p.plan_id AND n.node_id = p.node_id AND n.introduced_rev = p.introduced_rev"+
		" WHERE r.managed_request_id = ? AND n.retired_rev IS NULL AND n.issue_key = ?", requestID, issueKey).Scan(&plan, &packet)
	if err == nil {
		return plan, packet, packet != ""
	}
	err = q.QueryRowContext(ctx, "SELECT c.plan_id, p.packet_id FROM dag_release_recoveries c"+
		" JOIN dag_node_packets p ON p.plan_id = c.plan_id AND p.node_id = c.node_id"+
		" JOIN dag_nodes n ON n.plan_id = p.plan_id AND n.node_id = p.node_id AND n.introduced_rev = p.introduced_rev"+
		" WHERE c.successor_request_id = ? AND c.action = 'rereleased' AND n.retired_rev IS NULL AND n.issue_key = ?", requestID, issueKey).Scan(&plan, &packet)
	if err != nil {
		return "", "", false
	}
	return plan, packet, packet != ""
}

// liveRelationships are the live relationships of an issue, in relationship id order: every one of them
// is a rival a reservation must resolve. found is false when the store predates the packet tables, and the
// caller then treats every relationship as unresolved.
type packetLiveRelationship struct{ id, status string }

func (r Reservation) packetLiveRelationships(ctx context.Context, issueKey string) ([]packetLiveRelationship, error) {
	rows, err := r.Store.Querier(ctx).QueryContext(ctx, "SELECT relationship_id, status FROM relationships"+
		" WHERE issue_key = ? AND status IN ('active','paused') AND superseded_by IS NULL ORDER BY relationship_id", issueKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []packetLiveRelationship
	for rows.Next() {
		var rel packetLiveRelationship
		if err := rows.Scan(&rel.id, &rel.status); err != nil {
			return nil, err
		}
		out = append(out, rel)
	}
	return out, rows.Err()
}

// relationshipPackets is the plan and packet of every live relationship of the issue whose execution row
// is a packet the plan still registers for a live node of that same issue, keyed by relationship id. A
// row the plan cannot account for - another issue, another plan, a packet no live node carries, a retired
// node - is absent from the map, and its relationship then holds the issue exactly as it did before there
// were packets.
func (r Reservation) packetRelationshipRows(ctx context.Context, issueKey string) (map[string]struct{ plan, packet string }, error) {
	rows, err := r.Store.Querier(ctx).QueryContext(ctx, "SELECT e.relationship_id, e.plan_id, e.packet_id FROM dag_execution_packets e"+
		" JOIN dag_node_packets p ON p.plan_id = e.plan_id AND p.node_id = e.node_id AND p.packet_id = e.packet_id"+
		" JOIN dag_nodes n ON n.plan_id = p.plan_id AND n.node_id = p.node_id AND n.introduced_rev = p.introduced_rev"+
		" JOIN relationships r ON r.relationship_id = e.relationship_id AND r.issue_key = ? AND r.status IN ('active','paused') AND r.superseded_by IS NULL"+
		" WHERE e.issue_key = ? AND e.packet_id <> '' AND n.retired_rev IS NULL AND n.issue_key = ?", issueKey, issueKey, issueKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{ plan, packet string }{}
	for rows.Next() {
		var id, plan, packet string
		if err := rows.Scan(&id, &plan, &packet); err != nil {
			return nil, err
		}
		out[id] = struct{ plan, packet string }{plan, packet}
	}
	return out, rows.Err()
}
func (r Reservation) get(ctx context.Context, id string) (store.ManagedStartRequestsRow, error) {
	return r.Store.ManagedStartRequest(ctx, id)
}
func (r Reservation) Reserve(ctx context.Context, in Identity) (out store.ManagedStartRequestsRow, err error) {
	err = r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		current, e := r.get(ctx, in.RequestID)
		if e == nil {
			if current.RequestFingerprint != in.Fingerprint || current.IssueKey != in.IssueKey || current.Workspace != in.Workspace || current.MarkerRoot != in.MarkerRoot || current.SocketIdentity != in.SocketIdentity || current.CreateRequestID != in.CreateRequestID || current.DispatchRequestID != in.DispatchRequestID || current.FingerprintVersion != in.Version {
				return refusal("relationship_conflict", fmt.Sprintf("request %q already retained request_fingerprint %q, not %q", in.RequestID, current.RequestFingerprint, in.Fingerprint))
			}
			if current.State == "released" {
				return refusal("relationship_conflict", fmt.Sprintf("request %q was released and cannot be reused", in.RequestID))
			}
			out = current
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		// CRW-839: a second packet of one feature issue may be reserved beside the first. Every live
		// relationship of the issue is a rival and every one of them must be a distinct registered packet of
		// this request's own plan; a rival that cannot be resolved, or that resolves to this same packet, is
		// refused as it always was. Reading all of them and not just the first is what keeps a third packet
		// from being admitted beside the first while a second already holds it.
		minePlan, minePacket, mineOK := r.releasedPacket(ctx, in.RequestID, in.IssueKey)
		live, e := r.packetLiveRelationships(ctx, in.IssueKey)
		if e != nil {
			return e
		}
		if len(live) > 0 {
			rivals, e := r.packetRelationshipRows(ctx, in.IssueKey)
			if e != nil {
				return e
			}
			for _, rel := range live {
				if !mineOK {
					return refusal("duplicate_assignment", fmt.Sprintf("issue %q is already assigned under %q (%s); a reservation cannot take it", in.IssueKey, rel.id, rel.status))
				}
				rival, ok := rivals[rel.id]
				if !ok || rival.packet == minePacket || rival.plan != minePlan {
					return refusal("duplicate_assignment", fmt.Sprintf("issue %q is already assigned under %q (%s); a reservation cannot take it", in.IssueKey, rel.id, rel.status))
				}
			}
		}
		// Every other request in flight for the issue is a rival, and a packet does not get past one
		// (CRW-839): the shipped unique partial index managed_start_one_pending_issue allows a single
		// reserved or create_armed request per issue_key, so two packet starts of one issue run one after the
		// other rather than at once. Admitting the second here would only move the refusal to that index's
		// constraint error, so it is refused with the duplicate_assignment shape the caller already knows;
		// the packet node waits in resourceHold until the start in flight has attached.
		others, e := r.Store.PendingManagedStarts(ctx, in.IssueKey)
		if e != nil {
			return e
		}
		for _, other := range others {
			if other.RequestID == in.RequestID {
				continue
			}
			return refusal("duplicate_assignment", fmt.Sprintf("issue %q is already held by request %q (%s)", in.IssueKey, other.RequestID, other.State))
		}
		at := r.now()
		e = r.Store.ReserveManagedStart(ctx, store.ManagedStartRequestsRow{RequestID: in.RequestID, IssueKey: in.IssueKey, RequestFingerprint: in.Fingerprint, FingerprintVersion: in.Version, Workspace: in.Workspace, MarkerRoot: in.MarkerRoot, SocketIdentity: in.SocketIdentity, CreateRequestID: in.CreateRequestID, DispatchRequestID: in.DispatchRequestID, CreatedAt: at, UpdatedAt: at})
		if e != nil {
			return e
		}
		out, e = r.get(ctx, in.RequestID)
		return e
	})
	return
}
func (r Reservation) Arm(ctx context.Context, id, fp string, revision int64) (out store.ManagedStartRequestsRow, err error) {
	err = r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		row, e := r.get(ctx, id)
		if e != nil {
			return e
		}
		if row.RequestFingerprint != fp {
			return refusal("relationship_conflict", "managed request fingerprint changed")
		}
		if row.State == "create_armed" && row.Revision == revision+1 {
			out = row
			return nil
		}
		if row.State != "reserved" || row.Revision != revision {
			return refusal("relationship_conflict", fmt.Sprintf("request %q is %q at revision %d, not reserved at %d", id, row.State, row.Revision, revision))
		}
		changed, e := r.Store.ArmManagedStart(ctx, id, revision, r.now())
		if e != nil {
			return e
		}
		if !changed {
			return refusal("relationship_conflict", "managed request changed before it could be armed")
		}
		out, e = r.get(ctx, id)
		return e
	})
	return
}
func (r Reservation) Release(ctx context.Context, id, fp string, revision int64, reason string) (out store.ManagedStartRequestsRow, err error) {
	for _, field := range []struct{ name, value string }{{"request_id", id}, {"request_fingerprint", fp}} {
		if strings.TrimSpace(field.value) == "" {
			return out, refusal("malformed_receipt", fmt.Sprintf("%s must be a non-empty string, not %q", field.name, field.value))
		}
	}
	if revision < 0 {
		return out, refusal("malformed_receipt", fmt.Sprintf("revision must be a non-negative integer, not %d", revision))
	}
	if strings.TrimSpace(reason) == "" {
		return out, refusal("malformed_receipt", fmt.Sprintf("reason must be a non-empty string, not %q", reason))
	}
	err = r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		row, e := r.get(ctx, id)
		if errors.Is(e, sql.ErrNoRows) {
			return refusal("unregistered_relationship", fmt.Sprintf("no managed request %q", id))
		}
		if e != nil {
			return e
		}
		if row.RequestFingerprint != fp {
			return refusal("relationship_conflict", "managed request fingerprint changed")
		}
		if row.State != "reserved" || row.Revision != revision {
			return refusal("relationship_conflict", fmt.Sprintf("request %q is %q at revision %d; only a reserved row at revision %d can be released", id, row.State, row.Revision, revision))
		}
		changed, e := r.Store.ReleaseManagedStart(ctx, id, revision, reason, r.now())
		if e != nil {
			return e
		}
		if !changed {
			return refusal("relationship_conflict", "managed request changed before it could be released")
		}
		out, e = r.get(ctx, id)
		return e
	})
	return
}
func (r Reservation) Receipt(ctx context.Context, id, fp string, receipt map[string]any) (out store.ManagedStartRequestsRow, err error) {
	status, ok := receipt["status"].(string)
	if !ok || strings.TrimSpace(status) == "" {
		return out, refusal("malformed_receipt", "a start receipt needs a status")
	}
	child, _ := receipt["threadId"].(string)
	turn, _ := receipt["turnId"].(string)
	if status != "accepted" || strings.TrimSpace(child) == "" || strings.TrimSpace(turn) == "" {
		child = ""
		turn = ""
		if status == "accepted" {
			status = "partial"
		}
	}
	err = r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		row, e := r.get(ctx, id)
		if e != nil {
			return e
		}
		if row.RequestFingerprint != fp {
			return refusal("relationship_conflict", "managed request fingerprint changed")
		}
		if row.State == "attached" || row.ReceiptStatus.Valid && row.ReceiptStatus.String == "accepted" {
			if status == "accepted" && row.ChildTaskID.String == child && row.StandbyTurnID.String == turn {
				out = row
				return nil
			}
			return refusal("relationship_conflict", fmt.Sprintf("request %q already published child %q and standby %q", id, row.ChildTaskID.String, row.StandbyTurnID.String))
		}
		if row.State != "create_armed" {
			return refusal("relationship_conflict", fmt.Sprintf("request %q is %q; a receipt is recorded only while the request is create_armed", id, row.State))
		}
		e = r.Store.RecordManagedStartReceipt(ctx, id, status, sql.NullString{String: child, Valid: child != ""}, sql.NullString{String: turn, Valid: turn != ""}, r.now())
		if e != nil {
			return e
		}
		out, e = r.get(ctx, id)
		return e
	})
	return
}
