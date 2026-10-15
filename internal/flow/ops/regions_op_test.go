package ops

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// TestRegionsOverlapMatchesTheRelay pins RegionsOverlap (CRW-1441) to the relay's own judgement: for each pair of region texts, the
// answer is the pinned one, it is dagsched.Overlaps of the regions as cli/schedrelay.go declares a DESIGN edit region read from its
// text (written out field by field here, not through relayRegion), and it does not depend on the order of the two sides.
func TestRegionsOverlapMatchesTheRelay(t *testing.T) {
	file := func(p string) dagsched.Region { return dagsched.Region{Path: p, Kind: "file"} }
	tree := func(p string) dagsched.Region { return dagsched.Region{Path: p, Kind: "tree"} }
	symbol := func(p, key string) dagsched.Region { return dagsched.Region{Path: p, Kind: "symbol", Key: key} }
	pairs := []struct {
		name   string
		x, y   string
		rx, ry dagsched.Region
		want   bool
	}{
		{"symbol in its file", "a.go#F", "a.go", symbol("a.go", "F"), file("a.go"), true},
		{"two symbols of a file", "a.go#F", "a.go#G", symbol("a.go", "F"), symbol("a.go", "G"), false},
		{"the same symbol", "a.go#F", "a.go#F", symbol("a.go", "F"), symbol("a.go", "F"), true},
		// the JS regionsOverlap answers false here: the divergence the bundle names
		{"./ notation", "./a.go", "a.go", file("a.go"), file("a.go"), true},
		{"blanks around a path", " a.go ", "a.go", file("a.go"), file("a.go"), true},
		{"a directory with no slash is a file", "src", "src/a.go", file("src"), file("src/a.go"), false},
		{"a tree covers its file", "src/", "src/a.go", tree("src"), file("src/a.go"), true},
		{"a tree covers a symbol under it", "src/", "./src/a.go#F", tree("src"), symbol("src/a.go", "F"), true},
		{"a tree does not cover a sibling prefix", "src/", "srcx/a.go", tree("src"), file("srcx/a.go"), false},
		{"hotspot go.mod is one place", "go.mod#A", "go.mod#B", symbol("go.mod", "A"), symbol("go.mod", "B"), true},
		{"a schema segment is one place", "pkg/schema/x.go#A", "pkg/schema/x.go#B", symbol("pkg/schema/x.go", "A"), symbol("pkg/schema/x.go", "B"), true},
		{"a shared surface is one place", "internal/relay/argparse/specs.json#A", "internal/relay/argparse/specs.json#B",
			symbol("internal/relay/argparse/specs.json", "A"), symbol("internal/relay/argparse/specs.json", "B"), true},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			relay := dagsched.Overlaps(p.rx, p.ry)
			if relay != p.want {
				t.Fatalf("dagsched.Overlaps(%+v, %+v) = %v, the pinned answer is %v", p.rx, p.ry, relay, p.want)
			}
			if got := RegionsOverlap([]string{p.x}, []string{p.y}); got != relay {
				t.Errorf("RegionsOverlap([%q], [%q]) = %v, the relay answers %v", p.x, p.y, got, relay)
			}
			if got := RegionsOverlap([]string{p.y}, []string{p.x}); got != relay {
				t.Errorf("RegionsOverlap([%q], [%q]) = %v, the relay answers %v (not symmetric)", p.y, p.x, got, relay)
			}
		})
	}
	lists := []struct {
		name string
		a, b []string
		want bool
	}{
		{"nil is undeclared", nil, []string{"a.go"}, true},
		{"two empty lists", []string{}, []string{}, true},
		{"one pair of two lists", []string{"a.go", "b.go"}, []string{"c.go", "./b.go"}, true},
		{"disjoint lists", []string{"a.go"}, []string{"b.go"}, false},
	}
	for _, l := range lists {
		t.Run(l.name, func(t *testing.T) {
			if got := RegionsOverlap(l.a, l.b); got != l.want {
				t.Errorf("RegionsOverlap(%q, %q) = %v, want %v", l.a, l.b, got, l.want)
			}
			if got := RegionsOverlap(l.b, l.a); got != l.want {
				t.Errorf("RegionsOverlap(%q, %q) = %v, want %v (not symmetric)", l.b, l.a, got, l.want)
			}
		})
	}
}

// TestRegionOpsAnswerThroughTheLane runs crw.regionsOverlap and crw.admissionRegions as the engine installs them from Lane.Ops: the
// script gets the JSON answer, an absent edit list is undeclared (admissionRegions answers [], never null), and the row of each op
// stays proven on a resume.
func TestRegionOpsAnswerThroughTheLane(t *testing.T) {
	w := newWorld(t)
	if got := string(w.must(NameRegionsOverlap, []string{"./a.go"}, []string{"a.go#F"})); got != "true" {
		t.Errorf("regionsOverlap([./a.go], [a.go#F]) = %s, want true", got)
	}
	if got := string(w.must(NameRegionsOverlap, []string{"a.go"}, []string{"b.go"})); got != "false" {
		t.Errorf("regionsOverlap([a.go], [b.go]) = %s, want false", got)
	}
	if got := string(w.must(NameRegionsOverlap, nil, []string{"b.go"})); got != "true" {
		t.Errorf("regionsOverlap(null, [b.go]) = %s, want true", got)
	}
	var adm []string
	if err := json.Unmarshal(w.must(NameAdmissionRegions, []string{"a.go"}, []string{"./a.go", "b.go"}), &adm); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(adm, []string{"a.go", "b.go"}) {
		t.Errorf("admissionRegions([a.go], [./a.go, b.go]) = %q, want [a.go b.go]", adm)
	}
	if got := string(w.must(NameAdmissionRegions, []string{"a.go"}, nil)); got != "[]" {
		t.Errorf("admissionRegions([a.go], null) = %s, want []", got)
	}
	if _, err := w.op(NameRegionsOverlap, []string{"a.go"}, []string{"b.go"}, []string{"c.go"}); err == nil {
		t.Error("regionsOverlap with three arguments did not fail")
	}
	ops := w.lane.Ops()
	for _, ev := range w.j.Events() {
		var name string
		switch ev.Op {
		case OpRegionsOverlap:
			name = NameRegionsOverlap
		case OpAdmissionRegions:
			name = NameAdmissionRegions
		default:
			continue
		}
		ok, why, err := ops[name].Proven(t.Context(), w.j.Events(), ev)
		if err != nil || !ok {
			t.Errorf("%s row %s: Proven = %v, %q, %v; want true", ev.Op, ev.CallID, ok, why, err)
		}
	}
}
