package dagsched

import (
	"context"
	"testing"
)

// d1 of CRW-839 generation 4, PINNED AS IT IS: the normal project-scoped path cannot attach a second
// packet of one feature.
//
// This test drives the REAL two-release lifecycle - both releases through dag-release, with the scope
// bindings the product writes - instead of seeding the first relationship without its issue-scope binding
// (the seeding in packet_release_ready_test.go, which exercises resourceHold's packet rule in isolation
// and never reaches project linkage). The first packet attaches; the second is refused by project linkage
// with duplicate_scope_owner, because the issue's child binding is unique per live owner (the shipped v1
// partial index scope_bindings_one_live_owner, contract/schema/relay-sqlite.sql:1554) and the execution
// link is keyed by the issue (registry/linkage.go:407). The narrowed packet guard this issue added is
// never reached on this path.
//
// The assertion below pins that behaviour so the defect cannot be forgotten. It is NOT the promised
// behaviour: c1's multi-packet delivery is unavailable here, and the packet owner reports it as
// blocked_needs_input naming the v1 change (relaxing scope_bindings_one_live_owner and
// scope_links_one_live_edge for packet-bound rows, or re-keying the issue scope and updating every reader
// of it). Whoever lands that change must turn this test around to require the second packet to be bound.
func TestPacketSecondReleaseOnTheProjectScopedPathIsRefusedByProjectLinkage(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	twoPacketPlan(t, k.fixture, "rp")

	first, err := k.sched.Release(context.Background(), "rp", "n1", "parent", packetRequest(k))
	if err != nil || !first.Bound {
		t.Fatalf("the first packet was not released: %v %+v", err, first)
	}
	if got := k.count("SELECT COUNT(*) FROM scope_bindings WHERE scope_kind = 'issue' AND scope_key = 'CRW-F' AND role = 'child' AND status = 'active'"); got != 1 {
		t.Fatalf("the first packet's release wrote %d live issue-scope child bindings, want 1", got)
	}
	if got := k.count("SELECT COUNT(*) FROM dag_execution_packets WHERE packet_id = 'p1'"); got != 1 {
		t.Fatalf("the first packet recorded %d execution packet rows, want 1", got)
	}

	reading := k.read("rp")
	if got := reading.node("n2").Disposition; got != DispReady {
		t.Fatalf("the second packet reads %q (%s) after the first attached, want %q", got, reading.node("n2").Reason, DispReady)
	}
	_, err = k.sched.Release(context.Background(), "rp", "n2", "parent", packetRequest(k))
	if err == nil {
		t.Fatal("the second packet was bound on the project-scoped path; d1 is fixed and this test must be turned around to require it")
	}
	if reason := refusalReason(err); reason != "duplicate_scope_owner" {
		t.Fatalf("the second packet was refused with %q (%v), want duplicate_scope_owner: project linkage is the refusal d1 names", reason, err)
	}
}
