package dagsched

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-431: a delete, a rename and a hotspot file hold their own place, not the repository. A whole-repository hold exists only where the declarer states it.

// nx is a region of owner/repo.
func nx(path, kind, key, change string) Region {
	return Region{Repository: "owner/repo", Path: path, Kind: kind, Key: key, Change: change}
}

func nxFile(path, change string) Region { return nx(path, "file", "", change) }

func nxStated(r Region) Region { r.Exclusive = true; return r }

func nxOtherRepository(r Region) Region { r.Repository = "owner/other"; return r }

// Criterion c1: a node that deletes a file, a node that adds a .sql file, a node that changes a workflow file and an ordinary node are ready together in one pass.
func TestStructuralNodesAreReadyTogetherWithAnOrdinaryNode(t *testing.T) {
	_, reading := gradedFixture(t, map[string][]Region{
		"a-delete": {nxFile("internal/old/legacy.go", "delete")},
		"b-sql":    {nxFile("db/hist_index.sql", "edit")},
		"c-ci":     {nxFile(".github/workflows/ci.yml", "edit")},
		"d-plain":  {nxFile("internal/x/a.go", "edit")},
	}, "a-delete", "b-sql", "c-ci", "d-plain")
	if got := strings.Join(reading.readyIDs(), ","); got != "a-delete,b-sql,c-ci,d-plain" {
		t.Fatalf("ready = %q, want the three structural nodes and the ordinary one in one pass: %s", got, reading.brief())
	}
	if reading.Pass.DecidingLimit != LimitNone || reading.Pass.Overlaps != (OverlapCounts{}) || reading.Pass.ReadyCount != 4 {
		t.Fatalf("pass = %+v, want four released and no limit that decided", reading.Pass)
	}
	for _, n := range reading.Nodes {
		if n.Release == nil || n.Release.Rule != RuleIndependent {
			t.Errorf("%s release = %+v, want the independent rule", n.NodeID, n.Release)
		}
	}
}

// Criterion c1 through the release itself, which judges again under its lock: the four are released, each with a child and a held slot.
func TestStructuralNodesAreReleasedTogether(t *testing.T) {
	k := newReleaseKit(t)
	k.putPlan("gp", 0, "gp-r1", addRelNode("A", dag.NodeImplementation), addRelNode("B", dag.NodeImplementation), addRelNode("C", dag.NodeImplementation), addRelNode("D", dag.NodeImplementation))
	for node, region := range map[string]Region{"A": nxFile("internal/old/legacy.go", "delete"), "B": nxFile("db/hist_index.sql", "edit"), "C": nxFile(".github/workflows/ci.yml", "edit"), "D": nxFile("internal/x/a.go", "edit")} {
		if _, err := k.sched.DeclareRegions(contextBackground(), "gp", node, "parent", []Region{region}); err != nil {
			t.Fatal(err)
		}
	}
	for _, node := range []string{"A", "B", "C", "D"} {
		k.mustRelease("gp", node)
	}
	if rows := k.rows(); rows.releases != 4 || rows.executions != 4 || rows.slots != 4 {
		t.Fatalf("rows = %+v, want an intent, an execution and a held slot for each of the four nodes", rows)
	}
}

// A node that already holds a delete, a rename or a hotspot file leaves an unrelated candidate alone, and still holds back one on its own place.
func TestARunningStructuralNodeHoldsItsPlaceOnly(t *testing.T) {
	for _, c := range []struct {
		name   string
		holder Region
		same   Region
	}{
		{"a deleted file", nxFile("internal/old/legacy.go", "delete"), nxFile("internal/old/legacy.go", "edit")},
		{"a deleted tree", nx("internal/old", "tree", "", "delete"), nxFile("internal/old/legacy.go", "edit")},
		{"a renamed file", nxFile("internal/old/legacy.go", "rename"), nxFile("internal/old/legacy.go", "edit")},
		{"a .sql file", nxFile("db/hist_index.sql", "edit"), nxFile("db/hist_index.sql", "edit")},
		{"a workflow file", nxFile(".github/workflows/ci.yml", "edit"), nxFile(".github/workflows/ci.yml", "edit")},
		{"a lockfile", nxFile("go.sum", "edit"), nxFile("go.sum", "edit")},
		{"a Makefile", nxFile("Makefile", "edit"), nxFile("Makefile", "edit")},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.projectParent()
			f.putPlan("h", 0, "h-r1", addNode("h", dag.NodeImplementation), addNode("o", dag.NodeImplementation), addNode("s", dag.NodeImplementation))
			for node, region := range map[string]Region{"h": c.holder, "o": nxFile("internal/x/a.go", "edit"), "s": c.same} {
				if _, err := f.sched.DeclareRegions(context.Background(), "h", node, "parent", []Region{region}); err != nil {
					t.Fatal(err)
				}
			}
			f.startNode("h", "h")
			reading := f.read("h")
			if got := strings.Join(reading.readyIDs(), ","); got != "o" {
				t.Fatalf("ready = %q, want the unrelated candidate released and the one on the same place held: %s", got, reading.brief())
			}
			if s := reading.node("s"); s.Reason != DeferEditOverlap || s.Release.Rule != RuleDefer || s.Release.Overlaps != (OverlapCounts{Exclusive: 1}) {
				t.Fatalf("s = %+v %+v, want defer:edit_overlap on the place", s, s.Release)
			}
		})
	}
}

// Criterion c2: what stays serial and what does not. Each row is two nodes; the second is judged against the first. place is the place the overlap is on.
func TestSamePlaceStaysSerialAndOtherPlacesDoNot(t *testing.T) {
	type row struct {
		name  string
		p, q  Region
		place string // "" = the pair does not overlap
	}
	rows := []row{
		{"the same .sql file", nxFile("db/hist_index.sql", "edit"), nxFile("db/hist_index.sql", "edit"), "db/hist_index.sql"},
		{"the same deleted path", nxFile("internal/old/legacy.go", "delete"), nxFile("internal/old/legacy.go", "delete"), "internal/old/legacy.go"},
		{"the same renamed path", nxFile("internal/old/legacy.go", "rename"), nxFile("internal/old/legacy.go", "rename"), "internal/old/legacy.go"},
		{"a deleted file and an edit of it", nxFile("internal/old/legacy.go", "delete"), nxFile("internal/old/legacy.go", "edit"), "internal/old/legacy.go"},
		{"a deleted file and a symbol in it", nxFile("internal/old/legacy.go", "delete"), nx("internal/old/legacy.go", "symbol", "Run", "edit"), "internal/old/legacy.go"},
		{"a deleted tree and a file under it", nx("internal/old", "tree", "", "delete"), nxFile("internal/old/legacy.go", "edit"), "internal/old/legacy.go"},
		{"a tree that covers a deleted file", nx("internal", "tree", "", "edit"), nxFile("internal/old/legacy.go", "delete"), "internal/old/legacy.go"},
		{"a renamed tree and a file under it", nx("internal/old", "tree", "", "rename"), nxFile("internal/old/legacy.go", "edit"), "internal/old/legacy.go"},
		{"the same workflow file", nxFile(".github/workflows/ci.yml", "edit"), nxFile(".github/workflows/ci.yml", "edit"), ".github/workflows/ci.yml"},
		{"the .github tree and a workflow file", nx(".github", "tree", "", "edit"), nxFile(".github/workflows/ci.yml", "edit"), ".github/workflows/ci.yml"},
		{"the same lockfile whatever grade is declared", Region{Repository: "owner/repo", Path: "go.mod", Kind: "file", Change: "edit", Grade: GradeMechanical, Rule: RuleUnion},
			Region{Repository: "owner/repo", Path: "go.mod", Kind: "file", Change: "edit", Grade: GradeMechanical, Rule: RuleUnion}, "go.mod"},
		{"the same schema file", nxFile("internal/schema/x.json", "edit"), nxFile("internal/schema/x.json", "edit"), "internal/schema/x.json"},
		{"two symbols of one .sql file", nx("db/hist_index.sql", "symbol", "A", "edit"), nx("db/hist_index.sql", "symbol", "B", "edit"), "db/hist_index.sql"},
		{"two symbols of one workflow file", nx(".github/workflows/ci.yml", "symbol", "build", "edit"), nx(".github/workflows/ci.yml", "symbol", "test", "edit"), ".github/workflows/ci.yml"},
		{"a deleted symbol and another symbol of its file", nx("internal/old/legacy.go", "symbol", "Run", "delete"), nx("internal/old/legacy.go", "symbol", "Stop", "edit"), "internal/old/legacy.go"},
		{"a renamed symbol and another symbol of its file", nx("internal/old/legacy.go", "symbol", "Run", "edit"), nx("internal/old/legacy.go", "symbol", "Stop", "rename"), "internal/old/legacy.go"},

		{"two deleted files", nxFile("internal/old/a.go", "delete"), nxFile("internal/old/b.go", "delete"), ""},
		{"two .sql files", nxFile("db/a.sql", "edit"), nxFile("db/b.sql", "edit"), ""},
		{"two workflow files", nxFile(".github/workflows/ci.yml", "edit"), nxFile(".github/workflows/release.yml", "edit"), ""},
		{"go.mod and go.sum", nxFile("go.mod", "edit"), nxFile("go.sum", "edit"), ""},
		{"a deleted tree and a sibling whose name starts the same", nx("internal/x", "tree", "", "delete"), nxFile("internal/xy/z.go", "edit"), ""},
		{"a deleted file and an ordinary file", nxFile("internal/old/a.go", "delete"), nxFile("internal/x/b.go", "edit"), ""},
		{"a renamed file and an ordinary file", nxFile("internal/old/a.go", "rename"), nxFile("internal/x/b.go", "edit"), ""},
		{"a Makefile and a document", nxFile("Makefile", "edit"), nxFile("README.md", "edit"), ""},
		{"a .sql file and a Go file", nxFile("db/hist_index.sql", "edit"), nxFile("internal/x/a.go", "edit"), ""},
		{"the same deleted path in two repositories", nxFile("internal/old/a.go", "delete"), nxOtherRepository(nxFile("internal/old/a.go", "delete")), ""},
		{"two ordinary symbols of one file", nx("internal/x/a.go", "symbol", "Foo", "edit"), nx("internal/x/a.go", "symbol", "Bar", "edit"), ""},
	}
	for _, c := range rows {
		t.Run(c.name, func(t *testing.T) {
			// the rule itself, before it is applied to nodes: Overlaps and PairGrade agree with each other, in both directions, and with the place
			overlap := c.place != ""
			if Overlaps(c.p, c.q) != overlap || Overlaps(c.q, c.p) != overlap || (PairGrade(c.p, c.q) != "") != overlap || (PairGrade(c.q, c.p) != "") != overlap {
				t.Fatalf("Overlaps = %v/%v, PairGrade = %q/%q, want an overlap: %v", Overlaps(c.p, c.q), Overlaps(c.q, c.p), PairGrade(c.p, c.q), PairGrade(c.q, c.p), overlap)
			}
			_, reading := gradedFixture(t, map[string][]Region{"p": {c.p}, "q": {c.q}}, "p", "q")
			got := strings.Join(reading.readyIDs(), ",")
			if c.place == "" {
				if got != "p,q" || reading.node("q").Release.Rule != RuleIndependent {
					t.Fatalf("ready = %q q = %+v, want both released with nothing overlapped: %s", got, reading.node("q").Release, reading.brief())
				}
				return
			}
			q := reading.node("q")
			if got != "p" || q.Reason != DeferEditOverlap || q.Release.Rule != RuleDefer || q.Release.Overlaps != (OverlapCounts{Exclusive: 1}) || reading.Pass.DecidingLimit != LimitEditOverlap {
				t.Fatalf("ready = %q q = %+v %+v, want q deferred as an exclusive overlap: %s", got, q, q.Release, reading.brief())
			}
			if basis := q.Release.Basis; len(basis) != 1 || basis[0].Path != c.place || basis[0].Grade != GradeExclusive {
				t.Fatalf("basis = %+v, want one exclusive row on %s", basis, c.place)
			}
		})
	}
}

// The same rule through the release, which judges again under its lock: an overlap on the place refuses the second release with the reason the release already had, and another place does not.
func TestReleaseHoldsASharedStructuralPlaceOnly(t *testing.T) {
	for _, c := range []struct {
		name         string
		p, q         Region
		wantReleased bool
	}{
		{"the same .sql file", nxFile("db/hist_index.sql", "edit"), nxFile("db/hist_index.sql", "edit"), false},
		{"two symbols of one .sql file", nx("db/hist_index.sql", "symbol", "A", "edit"), nx("db/hist_index.sql", "symbol", "B", "edit"), false},
		{"the same deleted path", nxFile("internal/old/legacy.go", "delete"), nxFile("internal/old/legacy.go", "delete"), false},
		{"a deleted file and an ordinary file", nxFile("internal/old/a.go", "delete"), nxFile("internal/x/b.go", "edit"), true},
		{"two workflow files", nxFile(".github/workflows/ci.yml", "edit"), nxFile(".github/workflows/release.yml", "edit"), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newReleaseKit(t)
			k.putPlan("gp", 0, "gp-r1", addRelNode("P", dag.NodeImplementation), addRelNode("Q", dag.NodeImplementation))
			for node, region := range map[string]Region{"P": c.p, "Q": c.q} {
				if _, err := k.sched.DeclareRegions(contextBackground(), "gp", node, "parent", []Region{region}); err != nil {
					t.Fatal(err)
				}
			}
			k.mustRelease("gp", "P")
			_, err := k.release("gp", "Q")
			if c.wantReleased {
				if err != nil {
					t.Fatalf("release Q: %v, want it released beside P", err)
				}
				return
			}
			if got := refusalReason(err); got != "region_overlap" {
				t.Fatalf("release Q: %v (%q), want region_overlap", err, got)
			}
		})
	}
}

// A whole-repository hold is the declarer's word. It stays a hold when it is stated, on any path or change, survives being stored and read back, and is a replay when stated again.
func TestAStatedWholeRepositoryHoldStaysAHold(t *testing.T) {
	for _, c := range []struct {
		name string
		hold Region
	}{
		{"a repository-wide tree rename", nxStated(nx("internal/old", "tree", "", "rename"))},
		{"a stated hold on an ordinary edit", nxStated(nxFile("internal/x/a.go", "edit"))},
		{"a stated hold on a hotspot", nxStated(nxFile("go.mod", "edit"))},
		{"a stated hold on a delete", nxStated(nxFile("internal/old/legacy.go", "delete"))},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, reading := gradedFixture(t, map[string][]Region{"p": {c.hold}, "q": {nxFile("docs/other.md", "edit")}}, "p", "q")
			q := reading.node("q")
			if got := strings.Join(reading.readyIDs(), ","); got != "p" || q.Reason != DeferEditOverlap || q.Release.Rule != RuleDefer {
				t.Fatalf("ready = %q q = %+v, want an unrelated candidate held by the stated hold: %s", got, q.Release, reading.brief())
			}
			if basis := q.Release.Basis; len(basis) != 1 || basis[0].Path != c.hold.Path || basis[0].Grade != GradeExclusive {
				t.Fatalf("basis = %+v, want the row on the hold's own path", basis)
			}
			loaded, err := loadDeclarations(context.Background(), f.s.Q(context.Background()), "g")
			if err != nil || len(loaded["p"]) != 1 || !loaded["p"][0].Exclusive || loaded["p"][0].Grade != GradeExclusive {
				t.Fatalf("declaration read back = %+v, %v, want the hold kept", loaded["p"], err)
			}
			again, err := f.sched.DeclareRegions(context.Background(), "g", "p", "parent", []Region{c.hold})
			if err != nil || !again.Replayed || again.Seq != 1 {
				t.Fatalf("the same declaration again = %+v, %v, want a replay", again, err)
			}
			// a hold of one repository is not a hold of another
			_, other := gradedFixture(t, map[string][]Region{"p": {c.hold}, "q": {nxOtherRepository(nxFile("docs/other.md", "edit"))}}, "p", "q")
			if got := strings.Join(other.readyIDs(), ","); got != "p,q" {
				t.Fatalf("ready = %q, want a hold to stay inside its repository: %s", got, other.brief())
			}
		})
	}
}

// A delete, a rename and a hotspot declared without the word are not a hold, are stored with the column an older runtime reads, and are a replay when declared again.
func TestAnUnstatedStructuralRegionIsNotAHold(t *testing.T) {
	f := newFixture(t)
	f.putPlan("d", 0, "d-r1", addNode("impl", dag.NodeImplementation))
	list := []Region{nxFile("internal/old/legacy.go", "delete"), nxFile("internal/old/moved.go", "rename"), nxFile("db/hist_index.sql", "edit"), nxFile("internal/x/a.go", "edit")}
	declared, err := f.sched.DeclareRegions(context.Background(), "d", "impl", "parent", list)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range declared.Regions {
		// a delete, a rename and a hotspot are the exclusive grade at their place; an ordinary edit stays independent; none holds the repository
		wantGrade := GradeExclusive
		if r.Path == "internal/x/a.go" {
			wantGrade = GradeIndependent
		}
		if r.Exclusive || r.Grade != wantGrade {
			t.Errorf("declared %+v, want no whole-repository hold and the %s grade", r, wantGrade)
		}
	}
	if n := f.count("SELECT COUNT(*) FROM dag_node_regions WHERE exclusive = 1"); n != 3 {
		t.Errorf("%d rows stored with the exclusive column set, want the 3 an older runtime reads as a hold (delete, rename, .sql)", n)
	}
	if n := f.count("SELECT COUNT(*) FROM dag_node_region_holds WHERE stated = 0"); n != 4 {
		t.Errorf("%d rows say the hold was not stated, want one per region of the declaration", n)
	}
	loaded, err := loadDeclarations(context.Background(), f.s.Q(context.Background()), "d")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range loaded["impl"] {
		if r.Exclusive {
			t.Errorf("read back %+v, want no whole-repository hold", r)
		}
	}
	if again, err := f.sched.DeclareRegions(context.Background(), "d", "impl", "parent", list); err != nil || !again.Replayed {
		t.Fatalf("the same declaration again = %+v, %v, want a replay", again, err)
	}
}

// legacyRow writes a region the way the build before CRW-431 did: the exclusive column set by the classifier or the caller, no row in dag_node_region_holds, and a grade row only for a declaration made
// after the grades (which stored the folded grade, exclusive for a hold).
func legacyRow(f *fixture, plan, node string, seq int, r Region, exclusive int, graded bool) {
	f.t.Helper()
	f.exec("INSERT INTO dag_node_regions (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key, change, exclusive, declared_by, declared_at) VALUES (?, ?, ?, 'owner/repo', ?, ?, ?, ?, ?, 'parent', 't')",
		plan, node, seq, r.Path, r.Kind, r.Key, r.Change, exclusive)
	if graded {
		grade := GradeIndependent
		if exclusive == 1 {
			grade = GradeExclusive
		}
		f.exec("INSERT INTO dag_node_region_grades (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key, grade, rule) VALUES (?, ?, ?, 'owner/repo', ?, ?, ?, ?, '')", plan, node, seq, r.Path, r.Kind, r.Key, grade)
	}
}

// Existing declarations: every row an earlier build stored with the classifier's exclusive column. A delete and a hotspot protected nothing outside their own place, so they read at it; a rename also protected
// the destination nobody could name, and a row on a path the classifier does not flag can only be the declarer's word, so those two stay a whole-repository hold.
func TestExistingDeclarationsAreReadAtTheNarrowedJudgement(t *testing.T) {
	for _, c := range []struct {
		name          string
		region        Region
		exclusive     int
		wantHold      bool
		wantExclusive bool // the exclusive grade at its place
	}{
		{"a delete", nxFile("internal/old/legacy.go", "delete"), 1, false, true},
		{"a deleted tree", nx("internal/old", "tree", "", "delete"), 1, false, true},
		{"a .sql file", nxFile("db/hist_index.sql", "edit"), 1, false, true},
		{"a workflow file", nxFile(".github/workflows/ci.yml", "edit"), 1, false, true},
		{"a lockfile", nxFile("go.mod", "edit"), 1, false, true},
		{"a rename", nxFile("internal/old/legacy.go", "rename"), 1, true, true},
		{"a renamed tree", nx("internal/old", "tree", "", "rename"), 1, true, true},
		{"a stated hold on an ordinary path", nxFile("internal/x/a.go", "edit"), 1, true, true},
		{"an ordinary path with no hold", nxFile("internal/x/a.go", "edit"), 0, false, false},
	} {
		for _, graded := range []bool{false, true} {
			name := c.name + " without a grade row"
			if graded {
				name = c.name + " with a grade row"
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				f.putPlan("g", 0, "g-r1", addNode("p", dag.NodeImplementation))
				legacyRow(f, "g", "p", 1, c.region, c.exclusive, graded)
				loaded, err := loadDeclarations(context.Background(), f.s.Q(context.Background()), "g")
				if err != nil || len(loaded["p"]) != 1 {
					t.Fatalf("loaded %+v, %v", loaded, err)
				}
				got := loaded["p"][0]
				if got.Exclusive != c.wantHold || (got.Grade == GradeExclusive) != c.wantExclusive {
					t.Fatalf("read as %+v, want hold=%v exclusive grade=%v", got, c.wantHold, c.wantExclusive)
				}
			})
		}
	}
}

// The live case: three nodes declared by the earlier build (a delete, a .sql file and a workflow file, each stored as a hold) and an ordinary node are ready together.
func TestLegacyStructuralNodesAreReleasedByTheNarrowedReading(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("g", 0, "g-r1", addNode("a-delete", dag.NodeImplementation), addNode("b-sql", dag.NodeImplementation), addNode("c-ci", dag.NodeImplementation), addNode("d-plain", dag.NodeImplementation))
	legacyRow(f, "g", "a-delete", 2, nxFile("internal/old/legacy.go", "delete"), 1, true)
	legacyRow(f, "g", "b-sql", 2, nxFile("db/hist_index.sql", "edit"), 1, true)
	legacyRow(f, "g", "c-ci", 2, nxFile(".github/workflows/ci.yml", "edit"), 1, true)
	f.declare("g", "d-plain", "internal/x/a.go")
	reading := f.read("g")
	if got := strings.Join(reading.readyIDs(), ","); got != "a-delete,b-sql,c-ci,d-plain" {
		t.Fatalf("ready = %q, want all four: %s", got, reading.brief())
	}
}

// A legacy rename still holds the repository until it is declared again, and a declaration made by this build narrows it.
func TestALegacyRenameHoldsUntilItIsDeclaredAgain(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("g", 0, "g-r1", addNode("p", dag.NodeImplementation), addNode("q", dag.NodeImplementation))
	legacyRow(f, "g", "p", 1, nxFile("internal/old/legacy.go", "rename"), 1, true)
	f.declare("g", "q", "docs/other.md")
	reading := f.read("g")
	if got := strings.Join(reading.readyIDs(), ","); got != "p" || reading.node("q").Release.Rule != RuleDefer {
		t.Fatalf("ready = %q, want the legacy rename to keep holding the repository: %s", got, reading.brief())
	}
	again, err := f.sched.DeclareRegions(context.Background(), "g", "p", "parent", []Region{nxFile("internal/old/legacy.go", "rename")})
	if err != nil || again.Replayed || again.Seq != 2 {
		t.Fatalf("declared again = %+v, %v, want a new declaration that names no hold", again, err)
	}
	if got := strings.Join(f.read("g").readyIDs(), ","); got != "p,q" {
		t.Fatalf("ready = %q, want the narrowed declaration to leave the other node alone", got)
	}
}

// A node that already runs keeps the regions it holds unless a declaration narrows them (CRW-411): what a re-sent declaration is depends on what the stored row says. A legacy rename reads as a hold of the
// whole repository, so the same rename re-sent without the hold gives that hold up, which is a narrowing and is accepted; a legacy delete re-sent with the hold stated claims more than the node holds, and is refused.
func TestAHoldingNodeAnswersAResentLegacyDeclarationByWhatTheRowSays(t *testing.T) {
	for _, c := range []struct {
		name        string
		region      Region
		resend      Region
		wantReplay  bool
		wantRefused bool
	}{
		{"a legacy delete re-sent as it was declared", nxFile("internal/old/legacy.go", "delete"), nxFile("internal/old/legacy.go", "delete"), true, false},
		{"a legacy delete re-sent with the hold stated", nxFile("internal/old/legacy.go", "delete"), nxStated(nxFile("internal/old/legacy.go", "delete")), false, true},
		{"a legacy rename re-sent without the hold", nxFile("internal/old/legacy.go", "rename"), nxFile("internal/old/legacy.go", "rename"), false, false},
		{"a legacy rename re-sent with the hold stated", nxFile("internal/old/legacy.go", "rename"), nxStated(nxFile("internal/old/legacy.go", "rename")), true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.projectParent()
			f.putPlan("g", 0, "g-r1", addNode("p", dag.NodeImplementation))
			legacyRow(f, "g", "p", 1, c.region, 1, true)
			f.startNode("g", "p")
			again, err := f.sched.DeclareRegions(context.Background(), "g", "p", "parent", []Region{c.resend})
			var refused *store.RefusedError
			switch {
			case c.wantRefused:
				if !errors.As(err, &refused) || refused.Reason != "disposition_conflict" {
					t.Fatalf("re-sent = %+v, %v, want disposition_conflict: the running node holds the regions it was released with", again, err)
				}
			case err != nil || again.Replayed != c.wantReplay:
				t.Fatalf("re-sent = %+v, %v, want replayed=%v", again, err, c.wantReplay)
			}
		})
	}
}

// A zone that predates dag_node_region_holds reads every row by the rule for an existing declaration, since no row can say what was stated.
func TestAZoneWithoutTheHoldsTableReadsEveryRowAsAnExistingDeclaration(t *testing.T) {
	f := newFixture(t)
	f.putPlan("d", 0, "d-r1", addNode("impl", dag.NodeImplementation))
	if _, err := f.sched.DeclareRegions(context.Background(), "d", "impl", "parent", []Region{nxFile("internal/old/legacy.go", "delete"), nxStated(nxFile("internal/x/a.go", "edit")), nxFile("internal/old/moved.go", "rename")}); err != nil {
		t.Fatal(err)
	}
	f.exec("DROP TABLE dag_node_region_holds")
	loaded, err := loadDeclarations(context.Background(), f.s.Q(context.Background()), "d")
	if err != nil {
		t.Fatal(err)
	}
	holds := map[string]bool{}
	for _, r := range loaded["impl"] {
		holds[r.Path] = r.Exclusive
	}
	if len(holds) != 3 || holds["internal/old/legacy.go"] || !holds["internal/x/a.go"] || !holds["internal/old/moved.go"] {
		t.Fatalf("holds = %v, want the delete narrowed and the stated hold and the rename kept", holds)
	}
}

// The command line: a stated hold, a delete and a hotspot as an operator declares them, and what the answer says of each.
func TestCLIRegionDeclareStatesTheHoldAndNotTheClassifier(t *testing.T) {
	state, _ := cliState(t)
	regions := `[{"repository":"owner/repo","path":"internal/old","kind":"tree","change":"rename","exclusive":true},{"repository":"owner/repo","path":"internal/old2/legacy.go","kind":"file","change":"delete"},{"repository":"owner/repo","path":"db/hist_index.sql","kind":"file"}]`
	out, code := crw(t, state, "dag-region-declare", "--plan", "p1", "--node", "impl-a", "--actor", "parent", "--regions", regions)
	m := parseOut(t, out)
	if code != 0 || m["declaration_seq"] != float64(1) || m["replayed"] != false {
		t.Fatalf("declare: exit %d\n%s", code, out)
	}
	got := m["regions"].([]any)
	want := map[string]bool{"internal/old": true, "internal/old2/legacy.go": false, "db/hist_index.sql": false}
	if len(got) != 3 {
		t.Fatalf("regions = %v", got)
	}
	for _, r := range got {
		row := r.(map[string]any)
		if row["exclusive"] != want[row["path"].(string)] || row["grade"] != "exclusive" || row["rule"] != nil {
			t.Errorf("region %v, want exclusive=%v and the exclusive grade", row, want[row["path"].(string)])
		}
	}
	if out, code := crw(t, state, "dag-region-declare", "--plan", "p1", "--node", "impl-a", "--actor", "parent", "--regions", regions); code != 0 || parseOut(t, out)["replayed"] != true {
		t.Fatalf("replay: exit %d\n%s", code, out)
	}
}
