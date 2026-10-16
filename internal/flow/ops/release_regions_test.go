package ops

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// CRW-1391: crw.release asks the relay to declare the node's admission regions (the plan's regions and the DESIGN's edit_regions) before
// it releases the node, and a declaration the relay could not make is a warning of the release row, never a refusal.

// regionsRelay answers a release with what became of its regions.
type regionsRelay struct {
	*stubRelay
	declared, warning string
}

func (relay *regionsRelay) Release(ctx context.Context, request ReleaseRequest) (ReleaseResult, error) {
	res, err := relay.stubRelay.Release(ctx, request)
	res.RegionsDeclared, res.RegionsWarning = relay.declared, relay.warning
	return res, err
}

// TestReleaseAsksForTheAdmissionRegions: the release request carries the plan's regions of the node and then the DESIGN's edit_regions
// that the plan does not name, as crw:lib/lane admissionRegions makes them; a node with no regions on either side asks for none (it is
// undeclared: its overlap is unknown, not blocking). The row records the regions and what the relay did with them.
func TestReleaseAsksForTheAdmissionRegions(t *testing.T) {
	for _, c := range []struct {
		name string
		plan []string
		want []string
	}{
		{"plan and design", []string{"A/x.go", "A/"}, []string{"A/x.go", "A/"}},
		{"design adds", []string{"B/y.go"}, []string{"B/y.go", "A/"}},
		{"no plan regions", nil, []string{"A/"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			world := newWorld(t)
			plan := planOfNodes(map[string]string{"A": "a1"}, "A")
			plan.Nodes[0].Regions = c.plan
			world.relay.plan = plan
			world.ledgered(true, designOf("A"))
			relay := &regionsRelay{stubRelay: world.relay, declared: "declared"}
			world.lane.d.Relay = relay
			raw, err := world.op("release", "plan-1", "A", "M", map[string]any{"pinned": true})
			if err != nil {
				t.Fatal(err)
			}
			var out ReleaseOutcome
			if err := json.Unmarshal(raw, &out); err != nil || !out.Released || len(out.Warnings) != 0 {
				t.Fatalf("the release: %+v %v", out, err)
			}
			got := world.relay.releases[0].Regions
			if got == nil {
				got = []string{}
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("the release asked for the regions %q, want %q", got, c.want)
			}
			rows := world.releaseRows()
			if len(rows) != 1 || dataStr(rows[0], "regions_declared") != "declared" {
				t.Fatalf("the release row = %+v, want regions_declared declared", rows)
			}
			recorded, _ := rows[0].Data["regions"].([]any)
			if len(recorded) != len(c.want) {
				t.Fatalf("the release row records the regions %v, want %q", rows[0].Data["regions"], c.want)
			}
		})
	}
}

// TestReleaseWarnsOfRegionsTheRelayDidNotDeclare: a declaration the relay could not make leaves the release as it is (released, fail open)
// with a warning in its answer and its row.
func TestReleaseWarnsOfRegionsTheRelayDidNotDeclare(t *testing.T) {
	world := newWorld(t)
	plan := planOfNodes(map[string]string{"A": "a1"}, "A")
	plan.Nodes[0].Regions = []string{"A/"}
	world.relay.plan = plan
	world.ledgered(true, designOf("A"))
	world.lane.d.Relay = &regionsRelay{stubRelay: world.relay, declared: "failed", warning: "region path \"../x\" is not a clean path inside the repository"}
	raw, err := world.op("release", "plan-1", "A", "M", map[string]any{"pinned": true})
	if err != nil {
		t.Fatal(err)
	}
	var out ReleaseOutcome
	if err := json.Unmarshal(raw, &out); err != nil || !out.Released {
		t.Fatalf("a release whose regions were not declared is still released: %+v %v", out, err)
	}
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], WarnRegionsNotDeclared) || !strings.Contains(out.Warnings[0], "../x") {
		t.Fatalf("the warnings = %q, want one %s naming the relay's reason", out.Warnings, WarnRegionsNotDeclared)
	}
	rows := world.releaseRows()
	warnings, _ := rows[0].Data["warnings"].([]any)
	if len(rows) != 1 || dataStr(rows[0], "regions_declared") != "failed" || len(warnings) != 1 {
		t.Fatalf("the release row = %+v, want regions_declared failed and the warning", rows)
	}
}

// TestReleaseWarnsOfAnUndeclaredNode: a node with no admission regions and no declaration in force is released anyway, with the
// regions' status unknown and the warning in the answer and the row (CRW-1576).
func TestReleaseWarnsOfAnUndeclaredNode(t *testing.T) {
	world := newWorld(t)
	plan := planOfNodes(map[string]string{"A": "a1"}, "A")
	plan.Nodes[0].Regions = nil
	world.relay.plan = plan
	d := designOf("A")
	d.Issues[0].EditRegions = []string{}
	world.ledgered(true, d)
	world.lane.d.Relay = &regionsRelay{stubRelay: world.relay, declared: "unknown",
		warning: "no admission regions were declared; edit overlap is unknown and does not block release"}
	raw, err := world.op("release", "plan-1", "A", "M", map[string]any{"pinned": true})
	if err != nil {
		t.Fatal(err)
	}
	var out ReleaseOutcome
	if err := json.Unmarshal(raw, &out); err != nil || !out.Released {
		t.Fatalf("the release of an undeclared node is still released: %+v %v", out, err)
	}
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], WarnRegionsNotDeclared) || !strings.Contains(out.Warnings[0], "edit overlap is unknown") {
		t.Fatalf("the warnings = %q, want one %s saying the edit overlap is unknown", out.Warnings, WarnRegionsNotDeclared)
	}
	rows := world.releaseRows()
	warnings, _ := rows[0].Data["warnings"].([]any)
	if len(rows) != 1 || dataStr(rows[0], "regions_declared") != "unknown" || len(warnings) != 1 {
		t.Fatalf("the release row = %+v, want regions_declared unknown and the warning", rows)
	}
}

// TestReleaseAsksForTheOtherNodesAdmissionRegions: the release request carries the admission regions of the milestone's other designed
// nodes too (ReleaseRequest.Admitted), since the relay's judgment reads an earlier-ranked ready node as the holder of its declaration,
// and a declared clash is judged on the declarations of both sides; a node with no regions on either side is left out (it has none to
// declare).
func TestReleaseAsksForTheOtherNodesAdmissionRegions(t *testing.T) {
	world := newWorld(t)
	plan := planOfNodes(map[string]string{"A": "a1", "B": "b1", "C": "c1"}, "A", "B", "C")
	plan.Nodes[0].Regions, plan.Nodes[1].Regions = []string{"A/x.go"}, []string{"B/y.go"}
	world.relay.plan = plan
	world.ledgered(true, designOf("A", "B", "C"))
	if _, err := world.op("release", "plan-1", "B", "M", map[string]any{"pinned": true}); err != nil {
		t.Fatal(err)
	}
	got := world.relay.releases[0]
	if !slices.Equal(got.Regions, []string{"B/y.go", "B/"}) || len(got.Admitted) != 2 || !slices.Equal(got.Admitted["A"], []string{"A/x.go", "A/"}) ||
		!slices.Equal(got.Admitted["C"], []string{"C/"}) {
		t.Fatalf("the release of B asked for %q and the others %q, want B/y.go B/ and A: A/x.go A/, C: C/", got.Regions, got.Admitted)
	}
}
