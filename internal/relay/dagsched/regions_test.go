package dagsched

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func region(path, kind, key string) Region {
	return Region{Repository: "owner/repo", Path: path, Kind: kind, Key: key, Change: "edit"}
}

// Criterion c7: the rule the edit-region declarations are judged by. Every row says whether two regions can be touched by one change.
func TestEditRegionOverlapFixture(t *testing.T) {
	exclusive := func(r Region) Region { r.Exclusive = true; return r }
	other := func(r Region) Region { r.Repository = "owner/other"; return r }
	cases := []struct {
		name string
		a, b Region
		want bool
	}{
		{"two different files", region("a.go", "file", ""), region("b.go", "file", ""), false},
		{"the same file", region("a.go", "file", ""), region("a.go", "file", ""), true},
		{"a tree and a file under it", region("internal/x", "tree", ""), region("internal/x/y.go", "file", ""), true},
		{"a file and the tree above it", region("internal/x/y.go", "file", ""), region("internal/x", "tree", ""), true},
		{"a tree and a sibling whose name starts the same", region("internal/x", "tree", ""), region("internal/xy/z.go", "file", ""), false},
		{"two trees, one inside the other", region("internal", "tree", ""), region("internal/x", "tree", ""), true},
		{"two symbols of one file", region("a.go", "symbol", "Foo"), region("a.go", "symbol", "Bar"), false},
		{"one symbol twice", region("a.go", "symbol", "Foo"), region("a.go", "symbol", "Foo"), true},
		{"a symbol and its whole file", region("a.go", "symbol", "Foo"), region("a.go", "file", ""), true},
		{"the same path in two repositories", region("a.go", "file", ""), other(region("a.go", "file", "")), false},
		{"an exclusive region and an unrelated file", exclusive(region("go.mod", "file", "")), region("a.go", "file", ""), true},
		{"an exclusive region in another repository", exclusive(region("go.mod", "file", "")), other(region("a.go", "file", "")), false},
	}
	for _, c := range cases {
		if got := Overlaps(c.a, c.b); got != c.want {
			t.Errorf("%s: Overlaps = %v, want %v", c.name, got, c.want)
		}
		if got := Overlaps(c.b, c.a); got != c.want {
			t.Errorf("%s: Overlaps is not symmetric: reversed = %v, want %v", c.name, got, c.want)
		}
	}

	for _, c := range []struct {
		path, change string
		want         bool
	}{
		{"go.mod", "edit", true}, {"sub/go.sum", "edit", true}, {"package-lock.json", "edit", true}, {"uv.lock", "edit", true},
		{".github/workflows/ci.yml", "edit", true}, {"Makefile", "edit", true}, {"build/Dockerfile", "edit", true},
		{"db/001_init.sql", "edit", true}, {"internal/schema/x.json", "edit", true}, {"db/migrations/0002.go", "edit", true},
		{"internal/x/y.go", "edit", false}, {"README.md", "edit", false}, {"internal/schemas/y.go", "edit", false},
		{"internal/x/y.go", "rename", true}, {"internal/x/y.go", "delete", true},
	} {
		if got, _ := Classify(c.path, c.change); got != c.want {
			t.Errorf("Classify(%q, %q) = %v, want %v", c.path, c.change, got, c.want)
		}
	}

	// The rule, applied to nodes that are about to be released: the second candidate of every overlapping pair waits for the first to land.
	scenario := func(name string, regions map[string][]Region, wantReady string) {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.projectParent()
			f.putPlan("r", 0, "r-r1", addNode("p", dag.NodeImplementation), addNode("q", dag.NodeImplementation))
			for node, list := range regions {
				if _, err := f.sched.DeclareRegions(context.Background(), "r", node, "parent", list); err != nil {
					t.Fatal(err)
				}
			}
			reading := f.read("r")
			if got := strings.Join(reading.readyIDs(), ","); got != wantReady {
				t.Fatalf("ready = %q, want %q: %s", got, wantReady, reading.brief())
			}
			if wantReady == "p" {
				if q := reading.node("q"); q.Reason != DeferEditOverlap || reading.Pass.DecidingLimit != LimitEditOverlap {
					t.Fatalf("q = %+v pass = %+v, want defer:edit_overlap deciding the pass", q, reading.Pass)
				}
			}
		})
	}
	scenario("disjoint files run together", map[string][]Region{"p": {region("a.go", "file", "")}, "q": {region("b.go", "file", "")}}, "p,q")
	scenario("one file twice", map[string][]Region{"p": {region("a.go", "file", "")}, "q": {region("a.go", "file", "")}}, "p")
	scenario("a tree against a file inside it", map[string][]Region{"p": {region("internal", "tree", "")}, "q": {region("internal/x.go", "file", "")}}, "p")
	scenario("symbols of one file do not conflict", map[string][]Region{"p": {region("a.go", "symbol", "Foo")}, "q": {region("a.go", "symbol", "Bar")}}, "p,q")
	// CRW-431: a hotspot, a rename and a delete hold their own place and not the repository (narrow_exclusivity_test.go has the rest of the rows)
	scenario("a hotspot leaves an unrelated file alone", map[string][]Region{"p": {region("go.mod", "file", "")}, "q": {region("b.go", "file", "")}}, "p,q")
	scenario("a hotspot conflicts with the same file", map[string][]Region{"p": {region("go.mod", "file", "")}, "q": {region("go.mod", "file", "")}}, "p")
	scenario("a rename leaves an unrelated file alone", map[string][]Region{"p": {{Repository: "owner/repo", Path: "a.go", Kind: "file", Change: "rename"}}, "q": {region("b.go", "file", "")}}, "p,q")
	scenario("a rename conflicts with an edit of the same file", map[string][]Region{"p": {{Repository: "owner/repo", Path: "a.go", Kind: "file", Change: "rename"}}, "q": {region("a.go", "file", "")}}, "p")
}

// Contract 7.1: a node with no declaration counts as overlapping every other node. Each row is one way the rule can be wrong.
func TestUnknownRegionsOverlap(t *testing.T) {
	type tc struct {
		name  string
		setup func(f *fixture)
		want  map[string]string // node -> reason ("" = ready)
	}
	impl := func(f *fixture, ids ...string) {
		var changes []doc
		for _, id := range ids {
			changes = append(changes, addNode(id, dag.NodeImplementation))
		}
		f.putPlan("u", 0, "u-r1", changes...)
	}
	cases := []tc{
		{name: "an undeclared candidate with no holder is released", setup: func(f *fixture) { impl(f, "c") }, want: map[string]string{"c": ""}},
		{name: "an undeclared candidate against a declared holder waits", setup: func(f *fixture) {
			impl(f, "h", "c")
			f.declare("u", "h", "a.go")
			f.startNode("u", "h")
		}, want: map[string]string{"c": DeferEditOverlap}},
		{name: "a declared candidate against an undeclared holder waits", setup: func(f *fixture) {
			impl(f, "h", "c")
			f.declare("u", "c", "a.go")
			f.startNode("u", "h")
		}, want: map[string]string{"c": DeferEditOverlap}},
		{name: "two undeclared candidates: the first is released, the second waits", setup: func(f *fixture) { impl(f, "c1", "c2") },
			want: map[string]string{"c1": "", "c2": DeferEditOverlap}},
		{name: "a node that edits no repository never conflicts", setup: func(f *fixture) {
			f.putPlan("u", 0, "u-r1", addNode("h", dag.NodeImplementation), addNode("c", dag.NodeNonPR))
			f.startNode("u", "h")
		}, want: map[string]string{"c": ""}},
		{name: "an accepted head that has not landed still holds its regions", setup: func(f *fixture) {
			impl(f, "h", "c")
			f.declare("u", "h", "a.go")
			f.declare("u", "c", "a.go")
			f.acceptNode("u", "h", pinnedOpts)
		}, want: map[string]string{"c": DeferEditOverlap}},
		{name: "a landed head holds nothing", setup: func(f *fixture) {
			impl(f, "h", "c")
			f.declare("u", "h", "a.go")
			f.declare("u", "c", "a.go")
			a := f.acceptNode("u", "h", pinnedOpts)
			f.integrate(a, "owner/repo", "dev", true, true)
		}, want: map[string]string{"c": ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.projectParent()
			c.setup(f)
			reading := f.read("u")
			for node, reason := range c.want {
				n := reading.node(node)
				if n.Reason != reason || (reason == "" && n.Disposition != DispReady) {
					t.Errorf("%s: %+v, want reason %q (%s)", node, n, reason, reading.brief())
				}
			}
		})
	}
}

func TestDeclareRegionsReplay(t *testing.T) {
	f := newFixture(t)
	f.putPlan("d", 0, "d-r1", addNode("impl", dag.NodeImplementation), addNode("note", dag.NodeNonPR))
	first := f.declare("d", "impl", "b.go", "a.go")
	if first.Seq != 1 || first.Replayed {
		t.Fatalf("first declaration = %+v", first)
	}
	if first.Regions[0].Path != "a.go" {
		t.Fatalf("regions are stored sorted: %+v", first.Regions)
	}
	if again := f.declare("d", "impl", "a.go", "b.go"); again.Seq != 1 || !again.Replayed {
		t.Fatalf("the same regions in another order = %+v, want a replay of 1", again)
	}
	if f.count("SELECT COUNT(*) FROM dag_node_regions") != 2 {
		t.Fatal("a replay wrote rows")
	}
	if changed := f.declare("d", "impl", "a.go"); changed.Seq != 2 || changed.Replayed {
		t.Fatalf("a different declaration = %+v, want sequence 2", changed)
	}
	// the latest declaration is the one in force, the earlier is history.
	if got, _ := loadDeclarations(context.Background(), f.s.Q(context.Background()), "d"); len(got["impl"]) != 1 || got["impl"][0].Path != "a.go" {
		t.Fatalf("declarations in force = %+v", got)
	}
	if f.count("SELECT COUNT(*) FROM dag_node_regions") != 3 {
		t.Fatal("the earlier declaration was not kept")
	}
	// the classifier cannot be talked down: a caller's exclusive=false on go.mod is still exclusive at its place (and holds nothing outside it, since only the declarer's word holds the repository).
	hot, err := f.sched.DeclareRegions(context.Background(), "d", "impl", "parent", []Region{{Repository: "owner/repo", Path: "go.mod", Kind: "file", Change: "edit"}})
	if err != nil || hot.Regions[0].Exclusive || hot.Regions[0].Grade != GradeExclusive {
		t.Fatalf("go.mod = %+v, %v, want the exclusive grade and no whole-repository hold", hot, err)
	}
}

func TestDeclareRegionsRefusals(t *testing.T) {
	f := newFixture(t)
	f.putPlan("d", 0, "d-r1", addNode("impl", dag.NodeImplementation), addNode("note", dag.NodeNonPR))
	ctx := context.Background()
	reasonOf := func(plan, node string, regions []Region) string {
		_, err := f.sched.DeclareRegions(ctx, plan, node, "parent", regions)
		var refused *store.RefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("%s/%s %+v: got %v, want a refusal", plan, node, regions, err)
		}
		return refused.Reason
	}
	good := []Region{region("a.go", "file", "")}
	many := make([]Region, MaxRegions+1)
	for i := range many {
		many[i] = region("f"+string(rune('a'+i%26))+string(rune('a'+i/26))+".go", "file", "")
	}
	for _, c := range []struct {
		name       string
		plan, node string
		regions    []Region
		wantReason string
	}{
		{"an unknown plan", "nope", "impl", good, "unregistered_scope"},
		{"an unknown node", "d", "ghost", good, "unregistered_scope"},
		{"a node that edits no repository", "d", "note", good, "disposition_conflict"},
		{"no regions", "d", "impl", nil, "malformed_receipt"},
		{"too many regions", "d", "impl", many, "malformed_receipt"},
		{"an absolute path", "d", "impl", []Region{region("/etc/passwd", "file", "")}, "malformed_receipt"},
		{"a path that leaves the repository", "d", "impl", []Region{region("../x.go", "file", "")}, "malformed_receipt"},
		{"the repository root as a path", "d", "impl", []Region{region(".", "tree", "")}, "malformed_receipt"},
		{"an unknown kind", "d", "impl", []Region{region("a.go", "line", "")}, "malformed_receipt"},
		{"a symbol without its name", "d", "impl", []Region{region("a.go", "symbol", "")}, "malformed_receipt"},
		{"a key on a file", "d", "impl", []Region{region("a.go", "file", "Foo")}, "malformed_receipt"},
		{"an unknown change", "d", "impl", []Region{{Repository: "owner/repo", Path: "a.go", Kind: "file", Change: "move"}}, "malformed_receipt"},
		{"no repository", "d", "impl", []Region{{Path: "a.go", Kind: "file", Change: "edit"}}, "malformed_receipt"},
		{"one path declared as an edit and a delete", "d", "impl", []Region{region("a.go", "file", ""), {Repository: "owner/repo", Path: "a.go", Kind: "file", Change: "delete"}}, "malformed_receipt"},
	} {
		if got := reasonOf(c.plan, c.node, c.regions); got != c.wantReason {
			t.Errorf("%s: reason %q, want %q", c.name, got, c.wantReason)
		}
	}
	if f.count("SELECT COUNT(*) FROM dag_node_regions") != 0 {
		t.Fatal("a refused declaration wrote rows")
	}
}
