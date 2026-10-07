package registry

import (
	"context"
	"testing"
)

// The duplicate-assignment guard resolves BOTH sides of the packet comparison through the same
// registration: a live node version of the plan that carries the packet, for the issue at hand. A row the
// plan cannot account for - a packet no live node version carries, a row naming another issue, a retired
// node - must hold the issue, never release it. The newcomer side reads the initial release intent
// (dag_releases) or the successor a rerelease took (dag_release_recoveries).

// A rival whose execution row names a packet no live node version of the plan carries holds the issue: the
// pair cannot be shown to be two packets of one feature, so the second relationship is refused as before.
func TestPacketGuardRefusesARowThePlanDoesNotAccountFor(t *testing.T) {
	t.Parallel()
	r := newRegistry(t)
	s := r.Store
	ctx := context.Background()
	if _, err := r.Register(ctx, packetRegistration(t, s, "req-1", child)); err != nil {
		t.Fatal(err)
	}
	rival := liveRelationshipOf(t, s, issue)
	// The node carries p1, but the rival's execution row names p9, which no live node version registers.
	packetPlanRow(t, s, "PL", "n1", issue, "p1", "req-1", "")
	if _, err := s.DB.ExecContext(ctx, "INSERT INTO dag_execution_packets (relationship_id, plan_id, node_id, issue_key, packet_id, branch, recorded_at) VALUES (?,?,?,?,?,NULL,?)",
		rival, "PL", "n1", issue, "p9", fakeISO); err != nil {
		t.Fatal(err)
	}
	second := packetRegistration(t, s, "req-2", "01second-child")
	packetPlanRow(t, s, "PL", "n2", issue, "p2", "req-2", "")
	if _, err := r.Register(ctx, second); err == nil {
		t.Fatal("a rival row naming a packet no live node version carries was accepted")
	} else if reason := refusalReasonOf(t, err); reason != "duplicate_assignment" {
		t.Fatalf("reason = %s, want duplicate_assignment", reason)
	}
}

// A rival whose execution row names another issue holds the issue: the row is not a registration of this
// issue's packet, so the pair is not two packets of one feature.
func TestPacketGuardRefusesARowOfAnotherIssue(t *testing.T) {
	t.Parallel()
	r := newRegistry(t)
	s := r.Store
	ctx := context.Background()
	if _, err := r.Register(ctx, packetRegistration(t, s, "req-1", child)); err != nil {
		t.Fatal(err)
	}
	rival := liveRelationshipOf(t, s, issue)
	packetPlanRow(t, s, "PL", "n1", issue, "p1", "req-1", "")
	if _, err := s.DB.ExecContext(ctx, "INSERT INTO dag_execution_packets (relationship_id, plan_id, node_id, issue_key, packet_id, branch, recorded_at) VALUES (?,?,?,?,?,NULL,?)",
		rival, "PL", "n1", "CRW-other", "p1", fakeISO); err != nil {
		t.Fatal(err)
	}
	second := packetRegistration(t, s, "req-2", "01second-child")
	packetPlanRow(t, s, "PL", "n2", issue, "p2", "req-2", "")
	if _, err := r.Register(ctx, second); err == nil {
		t.Fatal("a rival row naming another issue was accepted")
	} else if reason := refusalReasonOf(t, err); reason != "duplicate_assignment" {
		t.Fatalf("reason = %s, want duplicate_assignment", reason)
	}
}

// A rerelease takes a successor request id, which dag_releases does not hold: the newcomer's packet is
// resolved from dag_release_recoveries, so a distinct packet of the issue is still admitted.
func TestPacketGuardResolvesASuccessorRelease(t *testing.T) {
	t.Parallel()
	r := newRegistry(t)
	s := r.Store
	ctx := context.Background()
	if _, err := r.Register(ctx, packetRegistration(t, s, "req-1", child)); err != nil {
		t.Fatal(err)
	}
	rival := liveRelationshipOf(t, s, issue)
	packetPlanRow(t, s, "PL", "n1", issue, "p1", "req-1", rival)
	// The newcomer is a rerelease: its intent is the successor row, and dag_releases holds no row for it.
	second := packetRegistration(t, s, "req-2", "01second-child")
	packetPlanRow(t, s, "PL", "n2", issue, "p2", "req-2", "")
	if _, err := s.DB.ExecContext(ctx, "DELETE FROM dag_releases WHERE managed_request_id = ?", "req-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, "INSERT INTO dag_release_recoveries (plan_id, node_id, manifest_digest, abandoned_request_id, action, successor_request_id, request_sha256, request_json, marker_root, socket, state_selector, slot_id, slot_released, copy_path, reason, recorded_by, coordinator_epoch, recorded_at) VALUES (?,?,?,?,'rereleased',?,?,?,?,?,?,NULL,0,NULL,?,?,0,?)",
		"PL", "n2", "manifest-n2", "req-n2-old", "req-2", "sha", "{}", "/m", "/s", "/st", "released again", parent, fakeISO); err != nil {
		t.Fatal(err)
	}
	// the successor intent froze the manifest of the node version it released
	if _, err := s.DB.ExecContext(ctx, "INSERT OR IGNORE INTO dag_input_manifests (manifest_digest, node_id, body_json, rule_version_json, coordinator_epoch, created_at) VALUES (?,?,?,'{}',0,?)",
		"manifest-n2", "n2", "{\"node_slice_digest\":\"slice-n2\"}", fakeISO); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Register(ctx, second); err != nil {
		t.Fatalf("a distinct packet whose release is a successor intent is refused: %v", err)
	}
}
