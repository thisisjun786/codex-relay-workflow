package ops

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// TestRegionsOverlapMatchesTheRelay pins RegionsOverlap (CRW-1441) to the relay's own judgement: for each pair of region texts, the
// answer is the pinned one, it is dagsched.Overlaps of the regions as cli/schedrelay.go declares a DESIGN edit region read from its
// text (written out field by field here, not through LaneRegion.readings), and it does not depend on the order of the two sides.
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
		{"nil is unknown, not an overlap", nil, []string{"a.go"}, false},
		{"two empty lists", []string{}, []string{}, false},
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

// TestRegionsOverlapReadsARegionAsScopeDoes pins the other half of the rule: a region text is also read as Scope reads it (its text as
// written: a prefix ending in "/", a pattern, the exact path), so a pair Scope calls covered is never called non-overlapping, where the
// DESIGN-text reading alone (RegionOf) makes a planned tree or pattern whose path holds "#" a symbol of another file.
func TestRegionsOverlapReadsARegionAsScopeDoes(t *testing.T) {
	for _, p := range []struct {
		x, y string
		want bool
	}{
		// RegionOf makes both symbols of the file gen (keys 1/ and 1/x.go); Scope reads gen#1/ as the tree gen#1
		{"gen#1/", "gen#1/x.go", true},
		{"gen#1/", "gen#2/x.go", false},
		{"a#*.go", "a#b.go", true},
		{"a#*.go", "a#b.txt", false},
		{"*.go", "a.go", true},
		{"src/*.go#F", "src/a.go", true},
		{"pkg/a#b.go", "pkg/a#b.go", true},
	} {
		if got := RegionsOverlap([]string{p.x}, []string{p.y}); got != p.want {
			t.Errorf("RegionsOverlap([%q], [%q]) = %v, want %v", p.x, p.y, got, p.want)
		}
		if got := RegionsOverlap([]string{p.y}, []string{p.x}); got != p.want {
			t.Errorf("RegionsOverlap([%q], [%q]) = %v, want %v (not symmetric)", p.y, p.x, got, p.want)
		}
	}
	regions := []string{"a.go", "a.go#F", "./a.go", " a.go ", "src/", " src/ ", "src", "src/./", "src//c.txt", "gen#1/", "a#*.go", "*.go",
		"src/*.go#F", "pkg/a#b.go", "src/c.txt#G"}
	paths := []string{"a.go", " a.go", "src", "src/a.go", "src/c.txt", "gen#1/x.go", "a#b.go", "pkg/a#b.go", "pkg/a", "b.go"}
	for _, r := range regions {
		for _, p := range paths {
			if inRegion(r, p) && !RegionsOverlap([]string{r}, []string{p}) {
				t.Errorf("Scope reads the region %q as covering %q, RegionsOverlap says they do not overlap", r, p)
			}
		}
	}
	// two regions Scope reads as covering one path (their glob languages intersect on it) overlap
	regions = append(regions, "a*.go", "*b.go", "?.go", "[ab].go", "src/*/x.go", "src/*/*.go", "src/b/", "*/x.go", "src/[ab]*.go", "*_test.go")
	paths = append(paths, "ab.go", "src/b/x.go", "src/b/y.go", "src/ab.go", "x/x.go", "a_test.go", "src/b/x_test.go")
	for _, x := range regions {
		for _, y := range regions {
			for _, p := range paths {
				if inRegion(x, p) && inRegion(y, p) && !RegionsOverlap([]string{x}, []string{y}) {
					t.Errorf("Scope reads %q and %q as both covering %q, RegionsOverlap says they do not overlap", x, y, p)
				}
			}
		}
	}
}

// TestRegionsOverlapKeepsIntersectingPatternsTogether pins two regions where at least one is a pattern (a path with `*`, `?` or `[`):
// they overlap unless they are proven disjoint (PR #896). Two patterns that both match one path (a*.go and *b.go both match ab.go)
// overlap though neither matches the other's text, and so do a pattern with a directory wildcard and a tree it can reach into.
func TestRegionsOverlapKeepsIntersectingPatternsTogether(t *testing.T) {
	for _, p := range []struct {
		x, y string
		want bool
	}{
		{"a*.go", "*b.go", true},
		{"*.go", "*_test.go", true},
		{"?.go", "[ab].go", true},
		{"src/*/x.go", "src/b/", true},
		{"src/*/x.go", "src/", true},
		{"*/x.go", "src/b/", false},
		{"src/*/", "src/b/x.go", true},
		{"src/*/x.go", "src/[ab]/*.go", true},
		// proven disjoint: a literal part that differs, or a different number of path segments (no wildcard matches "/")
		{"a*.go", "b*.go", false},
		{"*.go", "*.txt", false},
		{"src/*.go", "docs/*.go", false},
		{"a/*.go", "a/b/*.go", false},
		{"src/*/x.go", "docs/", false},
		{"src/*/x.go", "src/b/y/", false},
		{"src/*/x.go", "src/b/y.go", false},
	} {
		if got := RegionsOverlap([]string{p.x}, []string{p.y}); got != p.want {
			t.Errorf("RegionsOverlap([%q], [%q]) = %v, want %v", p.x, p.y, got, p.want)
		}
		if got := RegionsOverlap([]string{p.y}, []string{p.x}); got != p.want {
			t.Errorf("RegionsOverlap([%q], [%q]) = %v, want %v (not symmetric)", p.y, p.x, got, p.want)
		}
	}
}

// TestRegionsOverlapKeepsAPlannedChange pins the planned form of a region (CRW-1441): an object {text, change, exclusive} carries the
// plan's change and exclusive flag to the relay's rule, as DeclareRegions stores them, so a planned delete or rename of a symbol holds
// its whole file and an exclusive region overlaps every region; a bare text is an edit, as before.
func TestRegionsOverlapKeepsAPlannedChange(t *testing.T) {
	sym := func(p, key, change string, exclusive bool) dagsched.Region {
		return dagsched.Region{Path: p, Kind: "symbol", Key: key, Change: change, Exclusive: exclusive}
	}
	for _, p := range []struct {
		name   string
		x, y   LaneRegion
		rx, ry dagsched.Region
		want   bool
	}{
		{"a bare text is an edit", LaneRegion{Text: "a.go#F"}, LaneRegion{Text: "a.go#G"}, sym("a.go", "F", "", false), sym("a.go", "G", "", false), false},
		{"an edit", LaneRegion{Text: "a.go#F", Change: "edit"}, LaneRegion{Text: "a.go#G"}, sym("a.go", "F", "edit", false), sym("a.go", "G", "", false), false},
		{"a delete holds its file", LaneRegion{Text: "a.go#F", Change: "delete"}, LaneRegion{Text: "a.go#G"}, sym("a.go", "F", "delete", false), sym("a.go", "G", "", false), true},
		{"a rename holds its file", LaneRegion{Text: "a.go#F", Change: "rename"}, LaneRegion{Text: "a.go#G"}, sym("a.go", "F", "rename", false), sym("a.go", "G", "", false), true},
		{"a delete holds only its place", LaneRegion{Text: "a.go#F", Change: "delete"}, LaneRegion{Text: "b.go#G"}, sym("a.go", "F", "delete", false), sym("b.go", "G", "", false), false},
		{"an exclusive region holds everything", LaneRegion{Text: "a.go#F", Exclusive: true}, LaneRegion{Text: "b.go#G"}, sym("a.go", "F", "", true), sym("b.go", "G", "", false), true},
	} {
		t.Run(p.name, func(t *testing.T) {
			relay := dagsched.Overlaps(p.rx, p.ry)
			if relay != p.want {
				t.Fatalf("dagsched.Overlaps(%+v, %+v) = %v, the pinned answer is %v", p.rx, p.ry, relay, p.want)
			}
			if got := LaneRegionsOverlap([]LaneRegion{p.x}, []LaneRegion{p.y}); got != relay {
				t.Errorf("LaneRegionsOverlap(%+v, %+v) = %v, the relay answers %v", p.x, p.y, got, relay)
			}
			if got := LaneRegionsOverlap([]LaneRegion{p.y}, []LaneRegion{p.x}); got != relay {
				t.Errorf("LaneRegionsOverlap(%+v, %+v) = %v, the relay answers %v (not symmetric)", p.y, p.x, got, relay)
			}
		})
	}
	// the argument shape: a text, or an object; an object with a field the op does not know, or a change the relay does not know, is refused
	var got []LaneRegion
	if err := json.Unmarshal([]byte(`["a.go#F", {"text": "b.go#G", "change": "delete"}, {"text": "c/", "exclusive": true}]`), &got); err != nil {
		t.Fatal(err)
	}
	want := []LaneRegion{{Text: "a.go#F"}, {Text: "b.go#G", Change: "delete"}, {Text: "c/", Exclusive: true}}
	if !slices.Equal(got, want) {
		t.Errorf("decoded %+v, want %+v", got, want)
	}
	if b, _ := json.Marshal(got); string(b) != `["a.go#F",{"text":"b.go#G","change":"delete"},{"text":"c/","exclusive":true}]` {
		t.Errorf("encoded %s: a region with no change and no hold is its text, any other the object", b)
	}
	for _, bad := range []string{`[{"path": "a.go"}]`, `[{"text": "a.go", "change": "move"}]`, `[7]`} {
		if err := json.Unmarshal([]byte(bad), &got); err == nil {
			t.Errorf("%s decoded without an error", bad)
		}
	}
}

// TestRegionOpsRefuseARegionWithNoText pins the argument shape against a member that names no place (PR #896): null, an empty or
// blank text and an object with no text are refused through the op's JSON arguments, so a malformed list is an error and never a
// declared list that overlaps nothing. A null list stays undeclared. A planned edit with no hold is written as its text.
func TestRegionOpsRefuseARegionWithNoText(t *testing.T) {
	w := newWorld(t)
	for _, bad := range []any{
		[]any{nil},
		[]any{"a.go", nil},
		[]string{""},
		[]string{"  "},
		[]any{map[string]any{"text": ""}},
		[]any{map[string]any{"change": "delete"}},
		[]any{map[string]any{"text": " ", "exclusive": true}},
	} {
		if out, err := w.op(NameRegionsOverlap, bad, []string{"a.go"}); err == nil {
			t.Errorf("regionsOverlap(%v, [a.go]) = %s, want an error", bad, out)
		}
		if out, err := w.op(NameRegionsOverlap, []string{"a.go"}, bad); err == nil {
			t.Errorf("regionsOverlap([a.go], %v) = %s, want an error", bad, out)
		}
		if out, err := w.op(NameAdmissionRegions, bad, []string{"a.go"}); err == nil {
			t.Errorf("admissionRegions(%v, [a.go]) = %s, want an error", bad, out)
		}
	}
	if got := string(w.must(NameRegionsOverlap, nil, []string{"a.go"})); got != "false" {
		t.Errorf("regionsOverlap(null, [a.go]) = %s, want false: undeclared is unknown, not an overlap", got)
	}
	if b, _ := json.Marshal([]LaneRegion{{Text: "a.go", Change: "edit"}, {Text: "b.go", Change: "edit", Exclusive: true}}); string(b) != `["a.go",{"text":"b.go","change":"edit","exclusive":true}]` {
		t.Errorf("encoded %s: an edit with no hold is its text", b)
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
	if got := string(w.must(NameRegionsOverlap, nil, []string{"b.go"})); got != "false" {
		t.Errorf("regionsOverlap(null, [b.go]) = %s, want false: undeclared is unknown, not an overlap", got)
	}
	var adm []string
	if err := json.Unmarshal(w.must(NameAdmissionRegions, []string{"a.go"}, []string{"./a.go", "b.go"}), &adm); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(adm, []string{"a.go", "b.go"}) {
		t.Errorf("admissionRegions([a.go], [./a.go, b.go]) = %q, want [a.go b.go]", adm)
	}
	if got := string(w.must(NameAdmissionRegions, []string{"a.go"}, nil)); got != `["a.go"]` {
		t.Errorf("admissionRegions([a.go], null) = %s, want [a.go]: the named side is kept", got)
	}
	if got := string(w.must(NameAdmissionRegions, nil, []string{"a.go"})); got != `["a.go"]` {
		t.Errorf("admissionRegions(null, [a.go]) = %s, want [a.go]: the named side is kept", got)
	}
	if got := string(w.must(NameAdmissionRegions, nil, nil)); got != "[]" {
		t.Errorf("admissionRegions(null, null) = %s, want []", got)
	}
	deleteF := map[string]any{"text": "a.go#F", "change": "delete"}
	if got := string(w.must(NameRegionsOverlap, []any{"b.go", deleteF}, []string{"a.go#G"})); got != "true" {
		t.Errorf("regionsOverlap([b.go, {a.go#F delete}], [a.go#G]) = %s, want true", got)
	}
	if got := string(w.must(NameRegionsOverlap, []any{map[string]any{"text": "a.go#F", "exclusive": true}}, []string{"b.go"})); got != "true" {
		t.Errorf("regionsOverlap([{a.go#F exclusive}], [b.go]) = %s, want true", got)
	}
	if got := string(w.must(NameRegionsOverlap, []string{"a.go#F"}, []string{"a.go#G"})); got != "false" {
		t.Errorf("regionsOverlap([a.go#F], [a.go#G]) = %s, want false", got)
	}
	// a planned region keeps its change through admission, and an edit region that names it in another spelling is that region
	if got := string(w.must(NameAdmissionRegions, []any{deleteF}, []string{"./a.go#F", "b.go"})); got != `[{"text":"a.go#F","change":"delete"},"b.go"]` {
		t.Errorf("admissionRegions([{a.go#F delete}], [./a.go#F, b.go]) = %s", got)
	}
	if got := string(w.must(NameAdmissionRegions, []any{deleteF}, nil)); got != `[{"text":"a.go#F","change":"delete"}]` {
		t.Errorf("admissionRegions([{a.go#F delete}], null) = %s, want the planned region kept", got)
	}
	if _, err := w.op(NameRegionsOverlap, []any{map[string]any{"text": "a.go", "change": "move"}}, []string{"b.go"}); err == nil {
		t.Error("regionsOverlap with an unknown change did not fail")
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
