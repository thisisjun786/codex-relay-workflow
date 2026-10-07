package managed

import (
	"context"
	"testing"
)

// Managed reservations are deduplicated per (issue, packet) (CRW-839): a second reservation of one issue
// is admitted when it is a different packet of the same plan, and refused for the same packet or when
// either side is not a resolvable packet.

// packetRelease writes the release intent and the live node packet the reservation reads for one request.
func packetRelease(t *testing.T, r Reservation, plan, node, issueKey, packet, requestID string) {
	t.Helper()
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := r.Store.DB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	at := r.now()
	exec("INSERT OR IGNORE INTO dag_plans (plan_id, project_key, created_by_task_id, created_at) VALUES (?,?,'task-parent',?)", plan, "P-TEST", at)
	exec("INSERT OR IGNORE INTO dag_plan_revisions (plan_id, revision_no, parent_revision_no, request_id, request_digest, change_json, state_digest, coordinator_epoch, author_task_id, recorded_at) VALUES (?,1,0,?,?,?,?,0,'task-parent',?)",
		plan, "rev-"+plan, "digest-"+plan, "[]", "state-"+plan, at)
	exec("INSERT INTO dag_nodes (plan_id, node_id, introduced_rev, retired_rev, slice_digest, issue_key, node_kind, title, criteria_set_digest, supersedes_node_id) VALUES (?,?,1,NULL,?,?,'implementation',NULL,?,NULL)",
		plan, node, "slice-"+node, issueKey, "criteria-"+node)
	exec("INSERT INTO dag_node_packets (plan_id, node_id, introduced_rev, packet_id, covers_json, owns_json) VALUES (?,?,1,?,'[]','[]')", plan, node, packet)
	exec("INSERT INTO dag_releases (plan_id, node_id, manifest_digest, managed_request_id, coordinator_epoch, decided_at) VALUES (?,?,?,?,0,?)", plan, node, "manifest-"+node, requestID, at)
}

// TestReservationDeduplicatesPerIssueAndPacket: the same packet in flight is refused, and a different
// packet of the same plan is admitted beside a request that is no longer pending. The v1 unique index
// managed_start_one_pending_issue allows one pending managed request per issue, so two packets are
// reserved in sequence (the first is attached by the time the second is released) and never at once;
// the dedup itself is per (issue, packet).
func TestReservationDeduplicatesPerIssueAndPacket(t *testing.T) {
	t.Parallel()
	r, first := fixtureReservation(t)
	ctx := context.Background()
	if _, err := r.Reserve(ctx, first); err != nil {
		t.Fatal(err)
	}
	packetRelease(t, r, "PL", "n1", first.IssueKey, "p1", first.RequestID)

	same := first
	same.RequestID, same.Fingerprint, same.CreateRequestID, same.DispatchRequestID = "req-2", "fp-2", "create-2", "business-2"
	packetRelease(t, r, "PL", "n2", first.IssueKey, "p1", same.RequestID)
	if _, err := r.Reserve(ctx, same); err == nil {
		t.Fatal("a second reservation of the same packet was accepted")
	} else {
		reasonIs(t, err, "duplicate_assignment")
	}

	// The first packet's request leaves the pending state the way the managed start leaves it (attached),
	// which is what frees the issue for the next packet.
	if _, err := r.Store.DB.ExecContext(ctx, "UPDATE managed_start_requests SET state = 'attached' WHERE request_id = ?", first.RequestID); err != nil {
		t.Fatal(err)
	}
	other := first
	other.RequestID, other.Fingerprint, other.CreateRequestID, other.DispatchRequestID = "req-3", "fp-3", "create-3", "business-3"
	packetRelease(t, r, "PL", "n3", first.IssueKey, "p2", other.RequestID)
	if _, err := r.Reserve(ctx, other); err != nil {
		t.Fatalf("a distinct packet of one issue is refused: %v", err)
	}
}

// TestReservationRefusesWhenAPacketCannotBeResolved: a reservation of a request that is not a release of
// a packet is refused beside another one, as it always was.
func TestReservationRefusesWhenAPacketCannotBeResolved(t *testing.T) {
	t.Parallel()
	r, first := fixtureReservation(t)
	ctx := context.Background()
	if _, err := r.Reserve(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.RequestID, second.Fingerprint, second.CreateRequestID, second.DispatchRequestID = "req-2", "fp-2", "create-2", "business-2"
	if _, err := r.Reserve(ctx, second); err == nil {
		t.Fatal("a second reservation with no packet on either side was accepted")
	} else {
		reasonIs(t, err, "duplicate_assignment")
	}
}
