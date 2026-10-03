package dagsched

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// gr is a one-file edit region of owner/repo with a grade (and, for a mechanical grade, the rule that settles its overlaps).
func gr(path, grade, rule string) Region {
	return Region{Repository: "owner/repo", Path: path, Kind: "file", Change: "edit", Grade: grade, Rule: rule}
}

// gradedFixture is a plan of the implementation nodes named (they rank in the order given: one critical path, one introduction time, so node id breaks the tie) with each node's
// declaration made. A node without an entry is left undeclared.
func gradedFixture(t *testing.T, regions map[string][]Region, nodes ...string) (*fixture, Reading) {
	t.Helper()
	f := newFixture(t)
	f.projectParent()
	var changes []doc
	for _, n := range nodes {
		changes = append(changes, addNode(n, dag.NodeImplementation))
	}
	f.putPlan("g", 0, "g-r1", changes...)
	for node, list := range regions {
		if _, err := f.sched.DeclareRegions(context.Background(), "g", node, "parent", list); err != nil {
			t.Fatalf("declare %s: %v", node, err)
		}
	}
	return f, f.read("g")
}

// Criteria c1 and c2: what two nodes whose regions overlap are released as, by the grade of the overlap. Each row is one pair of declarations; p is released first and q is judged against it.
func TestReleaseByGrade(t *testing.T) {
	cases := []struct {
		name      string
		p, q      Region
		wantReady string
		wantRule  string
		wantCount OverlapCounts
	}{
		{"mechanical against mechanical with one rule (union)", gr("a.go", "mechanical", "union"), gr("a.go", "mechanical", "union"), "p,q", RuleMechanical, OverlapCounts{Mechanical: 1}},
		{"mechanical against mechanical with one rule (renumber)", gr("a.go", "mechanical", "renumber"), gr("a.go", "mechanical", "renumber"), "p,q", RuleMechanical, OverlapCounts{Mechanical: 1}},
		{"mechanical against mechanical with one regenerate command", gr("a.go", "mechanical", "regenerate:make generate"), gr("a.go", "mechanical", "regenerate:make generate"), "p,q", RuleMechanical, OverlapCounts{Mechanical: 1}},
		{"mechanical against mechanical with two rules is a local overlap", gr("a.go", "mechanical", "union"), gr("a.go", "mechanical", "renumber"), "p,q", RuleLocalOptimistic, OverlapCounts{Local: 1}},
		{"mechanical against mechanical with two regenerate commands is a local overlap", gr("a.go", "mechanical", "regenerate:make a"), gr("a.go", "mechanical", "regenerate:make b"), "p,q", RuleLocalOptimistic, OverlapCounts{Local: 1}},
		{"mechanical against local is a local overlap", gr("a.go", "mechanical", "union"), gr("a.go", "local", ""), "p,q", RuleLocalOptimistic, OverlapCounts{Local: 1}},
		{"local against mechanical is a local overlap", gr("a.go", "local", ""), gr("a.go", "mechanical", "union"), "p,q", RuleLocalOptimistic, OverlapCounts{Local: 1}},
		{"local against local", gr("a.go", "local", ""), gr("a.go", "local", ""), "p,q", RuleLocalOptimistic, OverlapCounts{Local: 1}},
		{"exclusive against exclusive", gr("a.go", "exclusive", ""), gr("a.go", "exclusive", ""), "p", RuleDefer, OverlapCounts{Exclusive: 1}},
		{"exclusive against local", gr("a.go", "exclusive", ""), gr("a.go", "local", ""), "p", RuleDefer, OverlapCounts{Exclusive: 1}},
		{"local against exclusive", gr("a.go", "local", ""), gr("a.go", "exclusive", ""), "p", RuleDefer, OverlapCounts{Exclusive: 1}},
		{"exclusive against mechanical", gr("a.go", "exclusive", ""), gr("a.go", "mechanical", "union"), "p", RuleDefer, OverlapCounts{Exclusive: 1}},
		{"no grade against no grade is as it was: deferred", gr("a.go", "", ""), gr("a.go", "", ""), "p", RuleDefer, OverlapCounts{Exclusive: 1}},
		{"an independent claim that the overlap contradicts", gr("a.go", "independent", ""), gr("a.go", "local", ""), "p", RuleDefer, OverlapCounts{Exclusive: 1}},
		{"a local declaration against an independent claim", gr("a.go", "local", ""), gr("a.go", "independent", ""), "p", RuleDefer, OverlapCounts{Exclusive: 1}},
		{"a tree against a file inside it, both local", Region{Repository: "owner/repo", Path: "internal", Kind: "tree", Change: "edit", Grade: "local"}, gr("internal/x.go", "local", ""), "p,q", RuleLocalOptimistic, OverlapCounts{Local: 1}},
		{"two symbols of one file never overlap, whatever their grade", Region{Repository: "owner/repo", Path: "a.go", Kind: "symbol", Key: "Foo", Change: "edit", Grade: "exclusive"}, Region{Repository: "owner/repo", Path: "a.go", Kind: "symbol", Key: "Bar", Change: "edit", Grade: "exclusive"}, "p,q", RuleIndependent, OverlapCounts{}},
		{"two files that do not overlap", gr("a.go", "local", ""), gr("b.go", "local", ""), "p,q", RuleIndependent, OverlapCounts{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, reading := gradedFixture(t, map[string][]Region{"p": {c.p}, "q": {c.q}}, "p", "q")
			if got := strings.Join(reading.readyIDs(), ","); got != c.wantReady {
				t.Fatalf("ready = %q, want %q: %s", got, c.wantReady, reading.brief())
			}
			p, q := reading.node("p"), reading.node("q")
			if p.Release == nil || p.Release.Rule != RuleIndependent {
				t.Fatalf("p holds nothing to overlap, so it is released as independent: %+v", p.Release)
			}
			if q.Release == nil || q.Release.Rule != c.wantRule || q.Release.Overlaps != c.wantCount {
				t.Fatalf("q release = %+v, want rule %s with overlaps %+v", q.Release, c.wantRule, c.wantCount)
			}
			if reading.Pass.Overlaps != c.wantCount {
				t.Fatalf("the pass counts %+v, want %+v", reading.Pass.Overlaps, c.wantCount)
			}
			if got, want := reading.Pass.Overlaps.Counted(), c.wantCount.Local+c.wantCount.Exclusive; got != want {
				t.Fatalf("overlap count = %d, want %d: mechanical overlaps are left out of it", got, want)
			}
			if c.wantRule == RuleDefer {
				if q.Disposition != DispDefer || q.Reason != DeferEditOverlap || reading.Pass.DecidingLimit != LimitEditOverlap {
					t.Fatalf("q = %+v pass = %+v, want defer:edit_overlap deciding the pass", q, reading.Pass)
				}
				return
			}
			// a released node stays a plain ready node: the rule lives in its release, never in a reason the release path would read as a hold
			if q.Disposition != DispReady || q.Reason != "" || q.Detail != "" || reading.Pass.DecidingLimit != LimitNone {
				t.Fatalf("q = %+v pass = %+v, want a ready node with no reason and no deciding limit", q, reading.Pass)
			}
		})
	}
}

// Several holders and several regions: the worst overlap with a holder is the one that counts, and it counts once per holder.
func TestOverlapCountsHoldersByTheirWorstGrade(t *testing.T) {
	_, reading := gradedFixture(t, map[string][]Region{
		"p": {gr("a.go", "mechanical", "union"), gr("b.go", "local", "")},
		"q": {gr("c.go", "local", "")},
		"r": {gr("a.go", "mechanical", "union"), gr("b.go", "mechanical", "union"), gr("c.go", "local", "")},
	}, "p", "q", "r")
	r := reading.node("r")
	if got := strings.Join(reading.readyIDs(), ","); got != "p,q,r" {
		t.Fatalf("ready = %q: %s", got, reading.brief())
	}
	// against p: a.go is mechanical and b.go is mechanical against local (local); against q: c.go is local
	if r.Release == nil || r.Release.Rule != RuleLocalOptimistic || r.Release.Overlaps != (OverlapCounts{Local: 2}) {
		t.Fatalf("r release = %+v, want local-optimistic with two local holders", r.Release)
	}
	if reading.Pass.Overlaps != (OverlapCounts{Local: 2}) {
		t.Fatalf("pass overlaps = %+v", reading.Pass.Overlaps)
	}

	_, reading = gradedFixture(t, map[string][]Region{
		"p": {gr("a.go", "local", ""), gr("b.go", "exclusive", "")},
		"q": {gr("a.go", "local", ""), gr("b.go", "local", "")},
	}, "p", "q")
	if q := reading.node("q"); q.Release == nil || q.Release.Rule != RuleDefer || q.Release.Overlaps != (OverlapCounts{Exclusive: 1}) || q.Reason != DeferEditOverlap {
		t.Fatalf("q = %+v %+v, want one exclusive holder (its local overlap with the same holder is not counted twice)", q, q.Release)
	}
}

// A node released earlier in the pass holds its regions for the nodes after it, so three nodes on one place are released together or cut in order.
func TestHoldersAddedDuringThePassAreJudgedToo(t *testing.T) {
	_, reading := gradedFixture(t, map[string][]Region{
		"p": {gr("a.go", "mechanical", "union")}, "q": {gr("a.go", "mechanical", "union")}, "r": {gr("a.go", "exclusive", "")},
	}, "p", "q", "r")
	if got := strings.Join(reading.readyIDs(), ","); got != "p,q" {
		t.Fatalf("ready = %q, want r cut by the two it overlaps: %s", got, reading.brief())
	}
	if r := reading.node("r"); r.Release.Rule != RuleDefer || r.Release.Overlaps != (OverlapCounts{Exclusive: 2}) {
		t.Fatalf("r = %+v, want two exclusive holders", r.Release)
	}
	if reading.Pass.Overlaps != (OverlapCounts{Mechanical: 1, Exclusive: 2}) {
		t.Fatalf("pass overlaps = %+v, want q's mechanical overlap with p and r's two exclusive ones", reading.Pass.Overlaps)
	}
}

// An undeclared node is unknown, and the unknown overlaps everything: that stays an exclusive overlap, and the row says which side is undeclared.
func TestUndeclaredNodesStayExclusive(t *testing.T) {
	_, reading := gradedFixture(t, map[string][]Region{"q": {gr("a.go", "local", "")}}, "p", "q")
	q := reading.node("q")
	if got := strings.Join(reading.readyIDs(), ","); got != "p" || q.Release.Rule != RuleDefer || q.Release.Overlaps != (OverlapCounts{Exclusive: 1}) {
		t.Fatalf("ready = %q q = %+v %+v", got, q, q.Release)
	}
	if len(q.Release.Basis) != 1 || q.Release.Basis[0].Undeclared != "holder" || q.Release.Basis[0].Holder != "p" || q.Release.Basis[0].Grade != GradeExclusive {
		t.Fatalf("basis = %+v, want one exclusive row naming the undeclared holder", q.Release.Basis)
	}
	_, reading = gradedFixture(t, map[string][]Region{"p": {gr("a.go", "local", "")}}, "p", "q")
	q = reading.node("q")
	if got := strings.Join(reading.readyIDs(), ","); got != "p" || q.Release.Rule != RuleDefer || len(q.Release.Basis) != 1 || q.Release.Basis[0].Undeclared != "candidate" {
		t.Fatalf("ready = %q q = %+v %+v, want the undeclared candidate cut", got, q, q.Release)
	}
}

// Criterion c3 (declaration): a mechanical grade is declared with the rule that settles it, and a declaration that cannot say so is refused with the reason the relay already has for a region that is not valid.
func TestGradeDeclarationRefusals(t *testing.T) {
	f := newFixture(t)
	f.putPlan("d", 0, "d-r1", addNode("impl", dag.NodeImplementation))
	ctx := context.Background()
	reasonOf := func(r Region) string {
		_, err := f.sched.DeclareRegions(ctx, "d", "impl", "parent", []Region{r})
		var refused *store.RefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("%+v: got %v, want a refusal", r, err)
		}
		return refused.Reason
	}
	for _, c := range []struct {
		name string
		r    Region
	}{
		{"a mechanical grade without a rule", gr("a.go", "mechanical", "")},
		{"a mechanical grade with a rule that is not one", gr("a.go", "mechanical", "merge")},
		{"a regenerate rule without its command", gr("a.go", "mechanical", "regenerate:")},
		{"a regenerate rule whose command is blank", gr("a.go", "mechanical", "regenerate:   ")},
		{"a regenerate command with a control character", gr("a.go", "mechanical", "regenerate:make\ngenerate")},
		{"a regenerate command that is too long", gr("a.go", "mechanical", "regenerate:"+strings.Repeat("x", MaxRuleBytes))},
		{"a rule on a local grade", gr("a.go", "local", "union")},
		{"a rule on an exclusive grade", gr("a.go", "exclusive", "union")},
		{"a rule on an independent region", gr("a.go", "", "renumber")},
		{"a grade that is not one", gr("a.go", "shared", "")},
		{"a grade spelled with capitals", gr("a.go", "Local", "")},
	} {
		if got := reasonOf(c.r); got != "malformed_receipt" {
			t.Errorf("%s: reason %q, want malformed_receipt", c.name, got)
		}
	}
	if f.count("SELECT COUNT(*) FROM dag_node_regions") != 0 || f.count("SELECT COUNT(*) FROM dag_node_region_grades") != 0 {
		t.Fatal("a refused declaration wrote rows")
	}
	for _, r := range []Region{gr("a.go", "mechanical", "union"), gr("b.go", "mechanical", "renumber"), gr("c.go", "mechanical", "regenerate:make generate"), gr("d.go", "local", ""), gr("e.go", "exclusive", ""), gr("f.go", "independent", ""), gr("g.go", "", "")} {
		if _, err := f.sched.DeclareRegions(ctx, "d", "impl", "parent", []Region{r}); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
}

// The grade and the rule are part of the declaration: they come back as declared, and the same regions declared again are a replay.
func TestGradeIsPartOfTheDeclaration(t *testing.T) {
	f := newFixture(t)
	f.putPlan("d", 0, "d-r1", addNode("impl", dag.NodeImplementation))
	ctx := context.Background()
	regions := []Region{gr("b.go", "mechanical", "union"), gr("a.go", "local", ""), gr("c.go", "", "")}
	first, err := f.sched.DeclareRegions(ctx, "d", "impl", "parent", regions)
	if err != nil || first.Seq != 1 || first.Replayed {
		t.Fatalf("first = %+v, %v", first, err)
	}
	want := []Region{gr("a.go", GradeLocal, ""), gr("b.go", GradeMechanical, "union"), gr("c.go", GradeIndependent, "")}
	for i := range want {
		if first.Regions[i] != want[i] {
			t.Fatalf("region %d = %+v, want %+v", i, first.Regions[i], want[i])
		}
	}
	if again, err := f.sched.DeclareRegions(ctx, "d", "impl", "parent", []Region{regions[2], regions[1], regions[0]}); err != nil || !again.Replayed || again.Seq != 1 {
		t.Fatalf("the same declaration in another order = %+v, %v, want a replay", again, err)
	}
	// the same places with another grade are another declaration
	if other, err := f.sched.DeclareRegions(ctx, "d", "impl", "parent", []Region{gr("b.go", "mechanical", "renumber"), regions[1], regions[2]}); err != nil || other.Replayed || other.Seq != 2 {
		t.Fatalf("another rule = %+v, %v, want sequence 2", other, err)
	}
	loaded, err := loadDeclarations(ctx, f.s.Q(ctx), "d")
	if err != nil || len(loaded["impl"]) != 3 || loaded["impl"][1] != gr("b.go", GradeMechanical, "renumber") {
		t.Fatalf("declarations in force = %+v, %v", loaded, err)
	}
	// one place declared twice with two grades is not one declaration
	_, err = f.sched.DeclareRegions(ctx, "d", "impl", "parent", []Region{gr("a.go", "local", ""), gr("a.go", "exclusive", "")})
	var refused *store.RefusedError
	if !errors.As(err, &refused) || refused.Reason != "malformed_receipt" {
		t.Fatalf("one place with two grades: %v", err)
	}
}

// What the classifier forces: a rename, a delete and the hotspots are stored as the exclusive grade whatever was declared, at their own place (CRW-431: no whole-repository hold, which only the declarer states).
func TestClassifierFoldsIntoTheGrade(t *testing.T) {
	f := newFixture(t)
	f.putPlan("d", 0, "d-r1", addNode("impl", dag.NodeImplementation))
	declared, err := f.sched.DeclareRegions(context.Background(), "d", "impl", "parent", []Region{
		gr("go.mod", "mechanical", "union"), {Repository: "owner/repo", Path: "old.go", Kind: "file", Change: "delete", Grade: "local"}, gr("a.go", "local", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Region{}
	for _, r := range declared.Regions {
		byPath[r.Path] = r
	}
	if r := byPath["go.mod"]; r.Exclusive || r.Grade != GradeExclusive || r.Rule != "" {
		t.Errorf("go.mod = %+v, want it stored exclusive with no rule and no hold of the repository", r)
	}
	if r := byPath["old.go"]; r.Exclusive || r.Grade != GradeExclusive {
		t.Errorf("old.go = %+v, want the delete stored exclusive at its place", r)
	}
	if r := byPath["a.go"]; r.Exclusive || r.Grade != GradeLocal {
		t.Errorf("a.go = %+v, want it as declared", r)
	}
}

// Criterion c3 (exclusive list): the shared contract surfaces are exclusive whatever grade is declared. The hold is on the place, so the same node's unrelated work is not held back, unlike a hotspot.
func TestSharedContractSurfacesAreExclusive(t *testing.T) {
	surfaces := []string{
		"internal/relay/argparse/specs.json",
		"contract/golden/ack-proof.json",
		"contract/fixtures/cli-shape/a__b.json",
		"internal/relay/store/testdata/golden/x.json",
		"contract/schema/relay-exit-codes.json",
	}
	for _, path := range surfaces {
		for _, grade := range []Region{gr(path, "mechanical", "union"), gr(path, "local", ""), gr(path, "independent", ""), gr(path, "", "")} {
			if g := EffectiveGrade(grade); g != GradeExclusive {
				t.Errorf("EffectiveGrade(%+v) = %s, want exclusive", grade, g)
			}
		}
		_, reading := gradedFixture(t, map[string][]Region{"p": {gr(path, "mechanical", "union")}, "q": {gr(path, "mechanical", "union")}}, "p", "q")
		q := reading.node("q")
		if got := strings.Join(reading.readyIDs(), ","); got != "p" || q.Reason != DeferEditOverlap || q.Release.Rule != RuleDefer {
			t.Errorf("%s: ready = %q q = %+v %+v, want the second mechanical declaration cut", path, got, q, q.Release)
		}
	}
	// the stored declaration says exclusive, with no rule, and no whole-repository hold (only the declarer's word makes one)
	f := newFixture(t)
	f.putPlan("d", 0, "d-r1", addNode("impl", dag.NodeImplementation))
	declared, err := f.sched.DeclareRegions(context.Background(), "d", "impl", "parent", []Region{gr("internal/relay/argparse/specs.json", "mechanical", "union"), gr("contract/golden/ack-proof.json", "local", "")})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range declared.Regions {
		if r.Grade != GradeExclusive || r.Rule != "" || r.Exclusive {
			t.Errorf("%+v, want exclusive by grade, no rule and no whole-repository hold", r)
		}
	}
	// a place on the list holds that place and nothing else
	_, reading := gradedFixture(t, map[string][]Region{"p": {gr("internal/relay/argparse/specs.json", "local", "")}, "q": {gr("internal/x.go", "local", "")}}, "p", "q")
	if got := strings.Join(reading.readyIDs(), ","); got != "p,q" {
		t.Fatalf("ready = %q, want a node on the list to leave unrelated work alone: %s", got, reading.brief())
	}
	// and files that merely sit beside the list are not on it
	for _, path := range []string{"internal/relay/argparse/parse_test.go", "contract/README.md", "contract/notes/x.md", "internal/relay/golden.go"} {
		if g := EffectiveGrade(gr(path, "local", "")); g != GradeLocal {
			t.Errorf("EffectiveGrade(%s) = %s, want local", path, g)
		}
	}
}

// The list is applied again when a declaration is read, so a row stored below it (an older declaration, or a row written by other means) cannot sit under it.
func TestSharedContractSurfaceHoldsAnOlderRow(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("g", 0, "g-r1", addNode("p", dag.NodeImplementation), addNode("q", dag.NodeImplementation))
	for _, node := range []string{"p", "q"} {
		f.exec("INSERT INTO dag_node_regions (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key, change, exclusive, declared_by, declared_at) VALUES ('g', ?, 1, 'owner/repo', 'contract/golden/ack-proof.json', 'file', '', 'edit', 0, 'parent', 't')", node)
		f.exec("INSERT INTO dag_node_region_grades (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key, grade, rule) VALUES ('g', ?, 1, 'owner/repo', 'contract/golden/ack-proof.json', 'file', '', 'local', '')", node)
	}
	reading := f.read("g")
	if got := strings.Join(reading.readyIDs(), ","); got != "p" || reading.node("q").Reason != DeferEditOverlap {
		t.Fatalf("ready = %q: %s", got, reading.brief())
	}
}

// Declarations made before grades existed have no grade row. They read as independent (which is exactly how they were judged, except that a hotspot or a delete holds its own place and no longer the repository: CRW-431), and a running node's identical redeclaration is still a replay.
func TestDeclarationsWithoutAGradeRowAreIndependent(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("g", 0, "g-r1", addNode("p", dag.NodeImplementation), addNode("q", dag.NodeImplementation))
	for _, row := range [][]any{{"p", "a.go", 0}, {"p", "go.mod", 1}, {"q", "a.go", 0}} {
		f.exec("INSERT INTO dag_node_regions (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key, change, exclusive, declared_by, declared_at) VALUES ('g', ?, 1, 'owner/repo', ?, 'file', '', 'edit', ?, 'parent', 't')", row...)
	}
	loaded, err := loadDeclarations(context.Background(), f.s.Q(context.Background()), "g")
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded["p"]; len(got) != 2 || got[0].Grade != GradeIndependent || got[1].Grade != GradeExclusive || got[1].Exclusive {
		t.Fatalf("p = %+v, want a.go independent and go.mod exclusive at its place, no longer a hold of the repository", got)
	}
	reading := f.read("g")
	if got := strings.Join(reading.readyIDs(), ","); got != "p" || reading.node("q").Release.Rule != RuleDefer {
		t.Fatalf("ready = %q: %s", got, reading.brief())
	}
	// p is running: an identical redeclaration (what a coordinator re-sending its declaration would do) is a replay and not a refusal of a changed declaration
	f.startNode("g", "p")
	again, err := f.sched.DeclareRegions(context.Background(), "g", "p", "parent", []Region{{Repository: "owner/repo", Path: "a.go", Kind: "file", Change: "edit"}, {Repository: "owner/repo", Path: "go.mod", Kind: "file", Change: "edit"}})
	if err != nil || !again.Replayed || again.Seq != 1 {
		t.Fatalf("redeclaration of a running node = %+v, %v, want a replay", again, err)
	}
	// while a different grade for a running node is a changed declaration and is refused as before
	_, err = f.sched.DeclareRegions(context.Background(), "g", "p", "parent", []Region{gr("a.go", "local", ""), {Repository: "owner/repo", Path: "go.mod", Kind: "file", Change: "edit"}})
	var refused *store.RefusedError
	if !errors.As(err, &refused) || refused.Reason != "disposition_conflict" {
		t.Fatalf("a regrade of a running node: %v", err)
	}
}
