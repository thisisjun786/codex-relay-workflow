package cli

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// CRW-1391: the run's release declares the node's admission regions (ops.ReleaseRequest.Regions: the plan's regions and the DESIGN's
// edit_regions) through dag_node_regions before the relay's agent release, when the node has no declaration in force; a declaration the
// relay refuses is a warning of the release, which goes on (fail open). A release that asks for none over a node with no declaration
// in force answers regions unknown with the same warning: the edit overlap is unknown and does not block (CRW-1576).

// labRegions are the declared regions of a node in force, as the relay reads them.
func (l *laneLab) labRegions(node string) []dagsched.Region {
	l.t.Helper()
	rows, err := l.w.st.DB.Query("SELECT r.path, r.region_kind FROM dag_node_regions r WHERE r.plan_id = ? AND r.node_id = ? AND r.declaration_seq = "+
		"(SELECT MAX(declaration_seq) FROM dag_node_regions m WHERE m.plan_id = r.plan_id AND m.node_id = r.node_id) ORDER BY r.path", l.plan, node)
	if err != nil {
		l.t.Fatal(err)
	}
	defer rows.Close()
	var out []dagsched.Region
	for rows.Next() {
		var r dagsched.Region
		if err := rows.Scan(&r.Path, &r.Kind); err != nil {
			l.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// TestReleaseDeclaresTheAdmissionRegions: two nodes whose admission regions are disjoint are both released, each with its regions
// declared; two whose regions really overlap still have the second held back (defer:edit_overlap), as before.
func TestReleaseDeclaresTheAdmissionRegions(t *testing.T) {
	for _, c := range []struct {
		name     string
		a1, a2   []string
		released bool
	}{
		{"disjoint", []string{"node-A-1.txt"}, []string{"node-A-2.txt", "docs/a2/"}, true},
		{"overlapping", []string{"node-A-1.txt", "shared/"}, []string{"node-A-2.txt", "shared/x.go"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1"})
			r := l.buildLane("run-regions001", l.handoffBytes(nil), LaneOptions{})
			first, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: l.nodeOf["A-1"], RunID: r.RunID, RequestID: "req-a1", Regions: c.a1})
			if err != nil || !first.Released || first.RegionsDeclared != "declared" || first.RegionsWarning != "" {
				t.Fatalf("the release of A-1: %+v %v", first, err)
			}
			second, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: l.nodeOf["A-2"], RunID: r.RunID, RequestID: "req-a2", Regions: c.a2})
			if err != nil || second.RegionsDeclared != "declared" {
				t.Fatalf("the release of A-2: %+v %v", second, err)
			}
			if c.released != second.Released || (!c.released && second.Reason != "defer:edit_overlap") {
				t.Fatalf("A-2 beside A-1 (%s): %+v, want released %v", c.name, second, c.released)
			}
			if got := l.labRegions(l.nodeOf["A-2"]); len(got) != len(c.a2) {
				t.Fatalf("the regions of A-2 = %+v, want %q", got, c.a2)
			}
			for _, rg := range l.labRegions(l.nodeOf["A-2"]) {
				if strings.HasPrefix(rg.Path, "docs/a2") && rg.Kind != "tree" {
					t.Errorf("the region docs/a2/ was declared %+v, want a tree", rg)
				}
			}
		})
	}
}

// TestReleaseKeepsADeclarationInForce: a node whose regions someone declared before the run (the parent's graded declaration of the
// crw-run procedure) keeps it: the release declares nothing over it.
func TestReleaseKeepsADeclarationInForce(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	r := l.buildLane("run-regions002", l.handoffBytes(nil), LaneOptions{})
	node := l.nodeOf["A-1"]
	if _, err := r.Sched.DeclareRegions(ctx, l.plan, node, labOwner, []dagsched.Region{{Repository: l.repo, Path: "node-A-1.txt", Kind: "file", Grade: "local"}}); err != nil {
		t.Fatal(err)
	}
	got, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-kept", Regions: []string{"node-A-1.txt", "other/"}})
	if err != nil || !got.Released || got.RegionsDeclared != "kept" {
		t.Fatalf("the release: %+v %v", got, err)
	}
	if n := l.count("SELECT COUNT(DISTINCT declaration_seq) FROM dag_node_regions WHERE node_id = ?", node); n != 1 {
		t.Fatalf("%d declarations of the node, want the one made before the run", n)
	}
}

// TestReleaseGoesOnWhenTheDeclarationFails: regions the relay refuses to declare (a path outside the repository) are a warning of the
// release, which releases the node as an undeclared one (fail open), and nothing is declared.
func TestReleaseGoesOnWhenTheDeclarationFails(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	r := l.buildLane("run-regions003", l.handoffBytes(nil), LaneOptions{})
	node := l.nodeOf["A-1"]
	got, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-fail", Regions: []string{"node-A-1.txt", "../outside.txt"}})
	if err != nil || !got.Released {
		t.Fatalf("a release whose regions cannot be declared is still made: %+v %v", got, err)
	}
	if got.RegionsDeclared != "failed" || !strings.Contains(got.RegionsWarning, "../outside.txt") {
		t.Fatalf("the release = %+v, want regions failed with the relay's reason", got)
	}
	if n := l.count("SELECT COUNT(*) FROM dag_node_regions WHERE node_id = ?", node); n != 0 {
		t.Fatalf("%d regions declared, want none", n)
	}
}

// TestReleaseDeclaresTheMilestonesOtherNodes: the release judgment reads every ready node ranked before the one released as a holder of
// its declaration — an undeclared one blocks nothing (CRW-1576), and a clash of declared regions is judged on the declarations of both
// sides (the race of the cap-2 session milestone, whose lanes release in either order). The release declares the milestone's other
// admitted nodes that have no declaration in force too, so A-2 released before A-1 is released, and A-1's own release keeps it.
func TestReleaseDeclaresTheMilestonesOtherNodes(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1"})
	r := l.buildLane("run-regions004", l.handoffBytes(nil), LaneOptions{})
	a1, a2 := l.nodeOf["A-1"], l.nodeOf["A-2"]
	got, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: a2, RunID: r.RunID, RequestID: "req-a2-first", Regions: []string{"node-A-2.txt"},
		Admitted: map[string][]string{a1: {"node-A-1.txt"}}})
	if err != nil || !got.Released || got.RegionsDeclared != "declared" {
		t.Fatalf("A-2 released before A-1: %+v %v", got, err)
	}
	if regions := l.labRegions(a1); len(regions) != 1 || regions[0].Path != "node-A-1.txt" {
		t.Fatalf("the regions of A-1 = %+v, want node-A-1.txt", regions)
	}
	next, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: a1, RunID: r.RunID, RequestID: "req-a1-second", Regions: []string{"node-A-1.txt"},
		Admitted: map[string][]string{a2: {"node-A-2.txt"}}})
	if err != nil || !next.Released || next.RegionsDeclared != "kept" {
		t.Fatalf("A-1 after A-2: %+v %v", next, err)
	}
}

// TestUndeclaredNodeReleasesAfterEarlierExecutionCloses (CRW-1576): a node with no declaration does not overlap every other. Two
// issues that declare no regions: the first is released and its execution closed (an escalation or the run's cancel, each of which
// returns the slot), and the second is released next. The closed node becomes a planned candidate of the pass again and, undeclared,
// once counted as an exclusive overlap with everything — the release it gated now goes on, with the regions' status unknown and a
// warning that the overlap is unknown.
func TestUndeclaredNodeReleasesAfterEarlierExecutionCloses(t *testing.T) {
	for _, reason := range []string{"tiers_exhausted", "flow_cancel"} {
		t.Run(reason, func(t *testing.T) {
			l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1", Regions: []compile.Region{}}, labIssue{Key: "A-2", Milestone: "M1", Regions: []compile.Region{}})
			r := l.buildLane("run-unknown-close", l.handoffBytes(nil), LaneOptions{})
			first, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: l.nodeOf["A-1"], RunID: r.RunID, RequestID: "req-a1"})
			if err != nil || !first.Released {
				t.Fatalf("first release: %+v %v", first, err)
			}
			closed, err := r.Close(ctx, ops.CloseRequest{Plan: l.plan, Node: l.nodeOf["A-1"], Reason: reason, RequestID: "close-a1"})
			if err != nil || !closed.Closed || !closed.SlotReleased {
				t.Fatalf("close: %+v %v", closed, err)
			}
			second, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: l.nodeOf["A-2"], RunID: r.RunID, RequestID: "req-a2"})
			if err != nil || !second.Released {
				t.Fatalf("second release after %s: %+v %v", reason, second, err)
			}
			if second.RegionsDeclared != "unknown" || second.RegionsWarning == "" {
				t.Fatalf("missing undeclared warning: %+v", second)
			}
		})
	}
}

// TestUndeclaredNodesReleaseConcurrentlyWithWarnings (CRW-1576): two nodes with no regions on either side hold no regions, so the
// second does not wait for the first to land or close. Both are released with the regions' status unknown and its warning.
func TestUndeclaredNodesReleaseConcurrentlyWithWarnings(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1", Regions: []compile.Region{}}, labIssue{Key: "A-2", Milestone: "M1", Regions: []compile.Region{}})
	r := l.buildLane("run-unknown-parallel", l.handoffBytes(nil), LaneOptions{})
	second, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: l.nodeOf["A-2"], RunID: r.RunID, RequestID: "req-a2-first"})
	if err != nil || !second.Released {
		t.Fatalf("the release of A-2: %+v %v", second, err)
	}
	if second.RegionsDeclared != "unknown" || second.RegionsWarning == "" {
		t.Fatalf("the release of A-2 = %+v, want regions unknown with the warning", second)
	}
	first, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: l.nodeOf["A-1"], RunID: r.RunID, RequestID: "req-a1-second"})
	if err != nil || !first.Released {
		t.Fatalf("the release of A-1 beside A-2 still running: %+v %v", first, err)
	}
	if first.RegionsDeclared != "unknown" || first.RegionsWarning == "" {
		t.Fatalf("the release of A-1 = %+v, want regions unknown with the warning", first)
	}
	for _, key := range []string{"A-1", "A-2"} {
		if regions := l.labRegions(l.nodeOf[key]); len(regions) != 0 {
			t.Fatalf("the regions of %s = %+v, want none declared", key, regions)
		}
	}
}

// TestReleaseKeepsDeclaredRegionsWhenAdmissionListIsEmpty (CRW-1576): a node whose regions somebody declared keeps the declaration
// even when the release asks for no admission regions — an empty list marks an undeclared node, it does not turn a declared one
// unknown.
func TestReleaseKeepsDeclaredRegionsWhenAdmissionListIsEmpty(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1", Regions: []compile.Region{}})
	r := l.buildLane("run-unknown-kept", l.handoffBytes(nil), LaneOptions{})
	node := l.nodeOf["A-1"]
	if _, err := r.Sched.DeclareRegions(ctx, l.plan, node, labOwner, []dagsched.Region{{Repository: l.repo, Path: "node-A-1.txt", Kind: "file", Grade: "local"}}); err != nil {
		t.Fatal(err)
	}
	got, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-kept-empty"})
	if err != nil || !got.Released || got.RegionsDeclared != "kept" || got.RegionsWarning != "" {
		t.Fatalf("the release = %+v %v, want released with the declaration kept and no warning", got, err)
	}
	if regions := l.labRegions(node); len(regions) != 1 || regions[0].Path != "node-A-1.txt" {
		t.Fatalf("the regions of A-1 = %+v, want the declaration made before the run", regions)
	}
}
