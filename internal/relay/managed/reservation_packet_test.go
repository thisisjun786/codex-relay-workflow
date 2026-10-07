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

// A second packet of one issue may not be reserved while another start of the issue is pending: the
// shipped unique partial index managed_start_one_pending_issue allows one reserved or armed request per
// issue_key, so the two starts run one after the other. The refusal is the existing duplicate_assignment
// shape, not the index's constraint error.
func TestReservationRefusesASecondPendingPacket(t *testing.T) {
	t.Parallel()
	r, first := fixtureReservation(t)
	ctx := context.Background()
	if _, err := r.Reserve(ctx, first); err != nil {
		t.Fatal(err)
	}
	packetRelease(t, r, "PL", "n1", first.IssueKey, "p1", first.RequestID)

	other := first
	other.RequestID, other.Fingerprint, other.CreateRequestID, other.DispatchRequestID = "req-3", "fp-3", "create-3", "business-3"
	packetRelease(t, r, "PL", "n3", first.IssueKey, "p2", other.RequestID)
	if _, err := r.Reserve(ctx, other); err == nil {
		t.Fatal("a second pending packet of one issue was reserved")
	} else {
		reasonIs(t, err, "duplicate_assignment")
	}
	if n := pendingStarts(t, r, first.IssueKey); n != 1 {
		t.Fatalf("%d requests of the issue are pending, want 1", n)
	}

	// The first start leaves the pending state the way the managed start leaves it (attached): the
	// second packet is released once the first has attached.
	if _, err := r.Store.DB.ExecContext(ctx, "UPDATE managed_start_requests SET state = 'attached' WHERE request_id = ?", first.RequestID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reserve(ctx, other); err != nil {
		t.Fatalf("the second packet is refused after the first attached: %v", err)
	}
}

// A second packet of one issue is reserved beside an ACTIVE relationship of the same issue when both
// resolve to distinct registered packets of one plan, and refused when the rival is not such a packet.
// This is the admission the reservation makes; the pending-rival rule above is the other half.
func TestReservationAdmitsADistinctPacketBesideAnActiveRelationship(t *testing.T) {
	t.Parallel()
	r, first := fixtureReservation(t)
	ctx := context.Background()
	if _, err := r.Reserve(ctx, first); err != nil {
		t.Fatal(err)
	}
	packetRelease(t, r, "PL", "n1", first.IssueKey, "p1", first.RequestID)
	// The first packet's start attached: its relationship is live and bound to packet p1.
	if _, err := r.Store.DB.ExecContext(ctx, "UPDATE managed_start_requests SET state = 'attached' WHERE request_id = ?", first.RequestID); err != nil {
		t.Fatal(err)
	}
	packetRelationship(t, r, "rel-live", first.IssueKey, "PL", "n1", "p1")

	other := first
	other.RequestID, other.Fingerprint, other.CreateRequestID, other.DispatchRequestID = "req-3", "fp-3", "create-3", "business-3"
	packetRelease(t, r, "PL", "n3", first.IssueKey, "p2", other.RequestID)
	if _, err := r.Reserve(ctx, other); err != nil {
		t.Fatalf("a distinct packet beside an active relationship is refused: %v", err)
	}
	// The reservation above is pending; the next case needs the issue to hold only attached relationships,
	// so it is attached the way the managed start leaves it.
	if _, err := r.Store.DB.ExecContext(ctx, "UPDATE managed_start_requests SET state = 'attached' WHERE request_id = ?", other.RequestID); err != nil {
		t.Fatal(err)
	}

	// The same packet as the live relationship is refused.
	same := first
	same.RequestID, same.Fingerprint, same.CreateRequestID, same.DispatchRequestID = "req-4", "fp-4", "create-4", "business-4"
	packetRelease(t, r, "PL", "n4", first.IssueKey, "p1", same.RequestID)
	if _, err := r.Reserve(ctx, same); err == nil {
		t.Fatal("the same packet as the live relationship was reserved")
	} else {
		reasonIs(t, err, "duplicate_assignment")
	}

	// EVERY live relationship is a rival, not only the first: a second live relationship whose packet
	// cannot be resolved holds the issue, so a reservation for yet another packet is refused. Reading only
	// the first relationship would resolve the newcomer against rel-live (a distinct registered packet of
	// the same plan) and admit it, which is the defect this case pins.
	packetRelationshipOrphan(t, r, "rel-zz-orphan", first.IssueKey)
	third := first
	third.RequestID, third.Fingerprint, third.CreateRequestID, third.DispatchRequestID = "req-5", "fp-5", "create-5", "business-5"
	packetRelease(t, r, "PL", "n5", first.IssueKey, "p5", third.RequestID)
	if _, err := r.Reserve(ctx, third); err == nil {
		t.Fatal("a second live relationship the plan cannot account for was ignored")
	} else {
		reasonIs(t, err, "duplicate_assignment")
	}
}

// packetRelationshipOrphan records a live relationship of an issue with NO dag_execution_packets row: its
// packet cannot be resolved, so it holds the issue exactly as it did before there were packets.
func packetRelationshipOrphan(t *testing.T, r Reservation, relationship, issueKey string) {
	t.Helper()
	if _, err := r.Store.DB.ExecContext(context.Background(), "INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES (?,?,'active','p','h','c','h',1,'[]','[]','t','t')", relationship, issueKey); err != nil {
		t.Fatal(err)
	}
}

// packetRelationship records a live relationship of an issue and the packet its execution row names, the
// way dag-release writes both at bind.
func packetRelationship(t *testing.T, r Reservation, relationship, issueKey, plan, node, packet string) {
	t.Helper()
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := r.Store.DB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec("INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES (?,?,'active','p','h','c','h',1,'[]','[]','t','t')", relationship, issueKey)
	exec("INSERT INTO dag_execution_packets (relationship_id, plan_id, node_id, issue_key, packet_id, branch, recorded_at) VALUES (?,?,?,?,?,NULL,'t')", relationship, plan, node, issueKey, packet)
}

// pendingStarts counts the reserved or armed managed requests of an issue.
func pendingStarts(t *testing.T, r Reservation, issueKey string) int {
	t.Helper()
	var n int
	if err := r.Store.DB.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM managed_start_requests WHERE issue_key = ? AND state IN ('reserved','create_armed')", issueKey).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
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
