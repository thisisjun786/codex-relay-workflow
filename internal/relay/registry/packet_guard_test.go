package registry

import (
	"context"
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The duplicate-assignment guard with packets (CRW-839): a second active relationship of one issue is
// refused without packets, admitted for a distinct packet registered in the same plan, and refused for
// the same packet. The refusal keeps its name (duplicate_assignment); no new reason is added.

// packetRegistration is a managed registration of one child for the fixture's issue, with its request
// reserved, armed and receipted the way the managed guard requires.
func packetRegistration(t *testing.T, s *store.Store, requestID, childID string) Registration {
	t.Helper()
	ctx := context.Background()
	in := fixture()
	in.Child.TaskID = childID
	in.ManagedRequestID = requestID
	in.DispatchRequestID = "dispatch-" + requestID
	if err := s.ReserveManagedStart(ctx, store.ManagedStartRequestsRow{
		RequestID: requestID, IssueKey: issue, RequestFingerprint: "fp-" + requestID,
		FingerprintVersion: "1", Workspace: root, MarkerRoot: "/markers",
		SocketIdentity: "/socket", CreateRequestID: "create-" + requestID, DispatchRequestID: in.DispatchRequestID,
		CreatedAt: fakeISO, UpdatedAt: fakeISO,
	}); err != nil {
		t.Fatal(err)
	}
	if armed, err := s.ArmManagedStart(ctx, requestID, 0, fakeISO); err != nil || !armed {
		t.Fatalf("arm %s: changed=%v err=%v", requestID, armed, err)
	}
	if err := s.RecordManagedStartReceipt(ctx, requestID, "accepted", ns(childID), in.DispatchTurnID, fakeISO); err != nil {
		t.Fatal(err)
	}
	return in
}

// packetPlanRow writes the plan rows the guard reads for one packet: the release intent of a managed
// request, the live node it names, that node's packet, and, when a relationship is given, the packet
// that relationship was bound under.
func packetPlanRow(t *testing.T, s *store.Store, plan, node, issueKey, packet, requestID, relationship string) {
	t.Helper()
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.DB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec("INSERT OR IGNORE INTO dag_plans (plan_id, project_key, created_by_task_id, created_at) VALUES (?,?,?,?)", plan, "P-TEST", parent, fakeISO)
	exec("INSERT OR IGNORE INTO dag_plan_revisions (plan_id, revision_no, parent_revision_no, request_id, request_digest, change_json, state_digest, coordinator_epoch, author_task_id, recorded_at) VALUES (?,1,0,?,?,?,?,0,?,?)",
		plan, "rev-"+plan, "digest-"+plan, "[]", "state-"+plan, parent, fakeISO)
	exec("INSERT INTO dag_nodes (plan_id, node_id, introduced_rev, retired_rev, slice_digest, issue_key, node_kind, title, criteria_set_digest, supersedes_node_id) VALUES (?,?,1,NULL,?,?,'implementation',NULL,?,NULL)",
		plan, node, "slice-"+node, issueKey, "criteria-"+node)
	exec("INSERT INTO dag_node_packets (plan_id, node_id, introduced_rev, packet_id, covers_json, owns_json) VALUES (?,?,1,?,'[]','[]')", plan, node, packet)
	exec("INSERT INTO dag_releases (plan_id, node_id, manifest_digest, managed_request_id, coordinator_epoch, decided_at) VALUES (?,?,?,?,0,?)", plan, node, "manifest-"+node, requestID, fakeISO)
	if relationship != "" {
		exec("INSERT INTO dag_execution_packets (relationship_id, plan_id, node_id, issue_key, packet_id, branch, recorded_at) VALUES (?,?,?,?,?,NULL,?)", relationship, plan, node, issueKey, packet, fakeISO)
	}
}

// liveRelationshipOf is the relationship a raw registration left for the issue.
func liveRelationshipOf(t *testing.T, s *store.Store, issueKey string) string {
	t.Helper()
	var id string
	if err := s.DB.QueryRowContext(context.Background(), "SELECT relationship_id FROM relationships WHERE issue_key = ? AND status IN ('active','paused') AND superseded_by IS NULL ORDER BY relationship_id", issueKey).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func refusalReasonOf(t *testing.T, err error) string {
	t.Helper()
	var refused *store.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("want a refusal, got %v", err)
	}
	return refused.Reason
}

// A second active relationship of one issue is refused when neither side is a registered packet.
func TestPacketGuardRefusesASecondRelationshipWithoutPackets(t *testing.T) {
	t.Parallel()
	r := newRegistry(t)
	ctx := context.Background()
	if _, err := r.Register(ctx, fixture()); err != nil {
		t.Fatalf("the first registration is refused: %v", err)
	}
	second := packetRegistration(t, r.Store, "req-2", "01second-child")
	if _, err := r.Register(ctx, second); err == nil {
		t.Fatal("a second relationship of one issue with no packets was accepted")
	} else if reason := refusalReasonOf(t, err); reason != "duplicate_assignment" {
		t.Fatalf("reason = %s, want duplicate_assignment", reason)
	}
}

// A second active relationship of one issue is admitted for a distinct packet registered in the same
// plan, and refused for the same packet.
func TestPacketGuardAdmitsADistinctRegisteredPacket(t *testing.T) {
	t.Parallel()
	r := newRegistry(t)
	s := r.Store
	ctx := context.Background()

	first := packetRegistration(t, s, "req-1", child)
	if _, err := r.Register(ctx, first); err != nil {
		t.Fatalf("the first packet is refused: %v", err)
	}
	firstRelationship := liveRelationshipOf(t, s, issue)
	packetPlanRow(t, s, "PL", "n1", issue, "p1", "req-1", firstRelationship)

	second := packetRegistration(t, s, "req-2", "01second-child")
	packetPlanRow(t, s, "PL", "n2", issue, "p2", "req-2", "")
	if _, err := r.Register(ctx, second); err != nil {
		t.Fatalf("a distinct packet of one issue is refused: %v", err)
	}

	third := packetRegistration(t, s, "req-3", "01third-child")
	packetPlanRow(t, s, "PL", "n3", issue, "p1", "req-3", "")
	if _, err := r.Register(ctx, third); err == nil {
		t.Fatal("a third relationship bound to an already-used packet was accepted")
	} else if reason := refusalReasonOf(t, err); reason != "duplicate_assignment" {
		t.Fatalf("reason = %s, want duplicate_assignment", reason)
	}
}

// A packet in another plan is not the same registration, so it is refused: the decision admits a second
// relationship only for packets registered in the same plan.
func TestPacketGuardRefusesAPacketOfAnotherPlan(t *testing.T) {
	t.Parallel()
	r := newRegistry(t)
	s := r.Store
	ctx := context.Background()
	first := packetRegistration(t, s, "req-1", child)
	if _, err := r.Register(ctx, first); err != nil {
		t.Fatal(err)
	}
	packetPlanRow(t, s, "PL-A", "n1", issue, "p1", "req-1", liveRelationshipOf(t, s, issue))
	second := packetRegistration(t, s, "req-2", "01second-child")
	packetPlanRow(t, s, "PL-B", "n2", issue, "p2", "req-2", "")
	if _, err := r.Register(ctx, second); err == nil {
		t.Fatal("a packet of another plan was accepted beside the first")
	} else if reason := refusalReasonOf(t, err); reason != "duplicate_assignment" {
		t.Fatalf("reason = %s, want duplicate_assignment", reason)
	}
}
