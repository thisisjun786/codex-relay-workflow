package dagsched

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-411, criterion c1: a node that holds its regions (its child runs) may declare again only to narrow them; a declaration that widens them is refused
// disposition_conflict naming the region that widens.

// nr is a region of owner/repo with every field a narrowing is judged by.
func nr(path, kind, key, grade, rule string) Region {
	return Region{Repository: "owner/repo", Path: path, Kind: kind, Key: key, Change: "edit", Grade: grade, Rule: rule}
}

// narrowWorld is a plan with a running holder and a candidate that wants a place the holder holds. The candidate is declared local on pkg/x.go, so it is released beside a
// local holder and deferred beside an exclusive one.
func narrowWorld(t *testing.T, held ...Region) *fixture {
	t.Helper()
	f := newFixture(t)
	f.projectParent()
	f.putPlan("n", 0, "n-r1", addNode("held", dag.NodeImplementation), addNode("cand", dag.NodeImplementation))
	if _, err := f.sched.DeclareRegions(context.Background(), "n", "held", "parent", held); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sched.DeclareRegions(context.Background(), "n", "cand", "parent", []Region{nr("pkg/x.go", "file", "", "local", "")}); err != nil {
		t.Fatal(err)
	}
	f.startNode("n", "held")
	return f
}

func redeclareHeld(f *fixture, regions ...Region) (RegionDeclaration, error) {
	return f.sched.DeclareRegions(context.Background(), "n", "held", "parent", regions)
}

func TestARunningNodeMayNarrowItsDeclaration(t *testing.T) {
	exclusive := func(path, kind string) Region { return nr(path, kind, "", "exclusive", "") }
	wholeRepo := func(path string) Region {
		r := nr(path, "file", "", "exclusive", "")
		r.Exclusive = true
		return r
	}
	deleting := func(path string) Region {
		r := nr(path, "file", "", "", "")
		r.Change = "delete"
		return r
	}
	cases := []struct {
		name string
		held []Region
		next []Region
	}{
		{"one of two regions is dropped", []Region{exclusive("a.go", "file"), exclusive("pkg", "tree")}, []Region{exclusive("a.go", "file")}},
		{"a tree becomes a subtree", []Region{exclusive("pkg", "tree")}, []Region{exclusive("pkg/sub", "tree")}},
		{"a tree becomes a file inside it", []Region{exclusive("pkg", "tree")}, []Region{exclusive("pkg/y.go", "file")}},
		{"a file becomes a symbol of the file", []Region{nr("pkg/y.go", "file", "", "local", "")}, []Region{nr("pkg/y.go", "symbol", "Foo", "local", "")}},
		{"a tree becomes a symbol inside it", []Region{nr("pkg", "tree", "", "local", "")}, []Region{nr("pkg/y.go", "symbol", "Foo", "local", "")}},
		{"a local region is held more strictly", []Region{nr("pkg/y.go", "file", "", "local", "")}, []Region{nr("pkg/y.go", "file", "", "exclusive", "")}},
		{"a mechanical region is held as local", []Region{nr("pkg/y.go", "file", "", "mechanical", "union")}, []Region{nr("pkg/y.go", "file", "", "local", "")}},
		{"a mechanical region keeps its rule on a narrower place", []Region{nr("pkg", "tree", "", "mechanical", "union")}, []Region{nr("pkg/y.go", "file", "", "mechanical", "union")}},
		{"independent and exclusive hold alike, so either way is a narrowing", []Region{nr("pkg/y.go", "file", "", "independent", "")}, []Region{nr("pkg/y.go", "file", "", "exclusive", "")}},
		{"exclusive to independent", []Region{nr("pkg/y.go", "file", "", "exclusive", "")}, []Region{nr("pkg/y.go", "file", "", "independent", "")}},
		{"a hold on the whole repository narrows to one exclusive file", []Region{wholeRepo("old.go")}, []Region{exclusive("pkg/y.go", "file")}},
		{"a tree with a local and an exclusive region inside narrows to the stricter one", []Region{nr("pkg", "tree", "", "local", ""), exclusive("pkg/y.go", "file")}, []Region{exclusive("pkg/y.go", "file")}},
		{"a subtree keeps the strict file inside it", []Region{nr("pkg", "tree", "", "local", ""), exclusive("pkg/sub/x.go", "file")}, []Region{nr("pkg/sub", "tree", "", "local", ""), exclusive("pkg/sub/x.go", "file")}},
		{"a subtree that holds the strict file as strictly itself", []Region{nr("pkg", "tree", "", "local", ""), nr("pkg/sub/x.go", "file", "", "local", "")}, []Region{nr("pkg/sub", "tree", "", "exclusive", "")}},
		{"an edit becomes a delete of the same file, which holds that place exclusively", []Region{nr("pkg/y.go", "file", "", "local", "")}, []Region{deleting("pkg/y.go")}},
		{"a delete of a file inside a tree held locally", []Region{nr("pkg", "tree", "", "local", "")}, []Region{deleting("pkg/y.go")}},
		{"a symbol of a hotspot file becomes the file: it is one place", []Region{nr("go.mod", "symbol", "Foo", "local", "")}, []Region{nr("go.mod", "file", "", "local", "")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := narrowWorld(t, c.held...)
			got, err := redeclareHeld(f, c.next...)
			if err != nil {
				t.Fatalf("a narrowing of a running node was refused: %v", err)
			}
			if got.Replayed || got.Seq != 2 || !got.Narrowed {
				t.Fatalf("the narrowing = %+v, want declaration 2, narrowed and not a replay", got)
			}
			if n := f.count("SELECT COUNT(*) FROM dag_node_regions WHERE plan_id = 'n' AND node_id = 'held' AND declaration_seq = 2"); n != len(got.Regions) {
				t.Fatalf("the declaration in force holds %d rows, the answer %d regions", n, len(got.Regions))
			}
		})
	}

	t.Run("the place that was given up is free for the next pass", func(t *testing.T) {
		f := narrowWorld(t, exclusive("a.go", "file"), exclusive("pkg", "tree"))
		if got := f.read("n").node("cand"); got.Reason != DeferEditOverlap {
			t.Fatalf("the candidate beside a holder of pkg = %s, want it deferred", got.Reason)
		}
		if _, err := redeclareHeld(f, exclusive("a.go", "file")); err != nil {
			t.Fatal(err)
		}
		if got := f.read("n").node("cand"); got.Disposition != DispReady {
			t.Fatalf("the candidate after the holder gave up pkg = %+v, want it ready", got)
		}
	})
}

func TestARunningNodeCannotWidenItsDeclaration(t *testing.T) {
	exclusive := func(path, kind string) Region { return nr(path, kind, "", "exclusive", "") }
	deleting := func(path string) Region {
		r := nr(path, "file", "", "", "")
		r.Change = "delete"
		return r
	}
	whole := func(path string) Region {
		r := nr(path, "file", "", "exclusive", "")
		r.Exclusive = true
		return r
	}
	other := func(r Region) Region { r.Repository = "owner/other"; return r }
	cases := []struct {
		name  string
		held  []Region
		next  []Region
		names string // what the refusal names
	}{
		{"a path nothing held covers", []Region{nr("pkg/y.go", "file", "", "exclusive", "")}, []Region{nr("pkg/z.go", "file", "", "exclusive", "")}, "pkg/z.go"},
		{"a region beside the held ones", []Region{nr("pkg/y.go", "file", "", "exclusive", "")}, []Region{nr("pkg/y.go", "file", "", "exclusive", ""), nr("pkg/z.go", "file", "", "exclusive", "")}, "pkg/z.go"},
		{"a file becomes the tree that holds it", []Region{nr("pkg/y.go", "file", "", "exclusive", "")}, []Region{nr("pkg", "tree", "", "exclusive", "")}, "pkg"},
		{"a subtree becomes its parent", []Region{nr("pkg/sub", "tree", "", "exclusive", "")}, []Region{nr("pkg", "tree", "", "exclusive", "")}, "pkg"},
		{"a symbol becomes its file", []Region{nr("pkg/y.go", "symbol", "Foo", "local", "")}, []Region{nr("pkg/y.go", "file", "", "local", "")}, "pkg/y.go"},
		{"a symbol becomes another symbol of the file", []Region{nr("pkg/y.go", "symbol", "Foo", "local", "")}, []Region{nr("pkg/y.go", "symbol", "Bar", "local", "")}, "Bar"},
		{"a file becomes the delete of the tree that holds it", []Region{nr("pkg/y.go", "file", "", "exclusive", "")}, []Region{{Repository: "owner/repo", Path: "pkg", Kind: "tree", Change: "delete"}}, "pkg"},
		{"a symbol becomes the delete of its file", []Region{nr("pkg/y.go", "symbol", "Foo", "local", "")}, []Region{deleting("pkg/y.go")}, "pkg/y.go"},
		{"a region that holds the whole repository where none was held", []Region{nr("pkg/y.go", "file", "", "exclusive", "")}, []Region{whole("pkg/y.go")}, "pkg/y.go"},
		{"an exclusive region declared local", []Region{nr("pkg/y.go", "file", "", "exclusive", "")}, []Region{nr("pkg/y.go", "file", "", "local", "")}, "pkg/y.go"},
		{"an exclusive region declared mechanical", []Region{nr("pkg/y.go", "file", "", "exclusive", "")}, []Region{nr("pkg/y.go", "file", "", "mechanical", "union")}, "pkg/y.go"},
		{"an independent claim declared local", []Region{nr("pkg/y.go", "file", "", "independent", "")}, []Region{nr("pkg/y.go", "file", "", "local", "")}, "pkg/y.go"},
		{"a local region declared mechanical", []Region{nr("pkg/y.go", "file", "", "local", "")}, []Region{nr("pkg/y.go", "file", "", "mechanical", "union")}, "pkg/y.go"},
		{"a mechanical region with another rule", []Region{nr("pkg/y.go", "file", "", "mechanical", "union")}, []Region{nr("pkg/y.go", "file", "", "mechanical", "renumber")}, "pkg/y.go"},
		{"the stricter of two regions over a place is the one that counts", []Region{nr("pkg", "tree", "", "local", ""), nr("pkg/y.go", "file", "", "exclusive", "")}, []Region{nr("pkg/y.go", "file", "", "local", "")}, "pkg/y.go"},
		{"the same path in another repository", []Region{nr("pkg/y.go", "file", "", "exclusive", "")}, []Region{other(nr("pkg/y.go", "file", "", "exclusive", ""))}, "owner/other"},
		{"a hold on the whole repository narrows only to a region at least as strict", []Region{whole("old.go")}, []Region{nr("pkg/y.go", "file", "", "local", "")}, "pkg/y.go"},
		{"a subtree would hold the strict file inside it less strictly", []Region{nr("pkg", "tree", "", "local", ""), exclusive("pkg/sub/x.go", "file")}, []Region{nr("pkg/sub", "tree", "", "local", "")}, "pkg/sub/x.go"},
		{"a tree would hold a strict symbol inside it less strictly", []Region{nr("pkg", "tree", "", "local", ""), nr("pkg/y.go", "symbol", "Foo", "exclusive", "")}, []Region{nr("pkg", "tree", "", "local", "")}, "pkg/y.go"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := narrowWorld(t, c.held...)
			_, err := redeclareHeld(f, c.next...)
			var refused *store.RefusedError
			if !errors.As(err, &refused) || refused.Reason != "disposition_conflict" {
				t.Fatalf("a widening declaration: %v, want disposition_conflict", err)
			}
			if !strings.Contains(refused.Detail, c.names) {
				t.Fatalf("the refusal %q does not name %q", refused.Detail, c.names)
			}
			if n := f.count("SELECT MAX(declaration_seq) FROM dag_node_regions WHERE plan_id = 'n' AND node_id = 'held'"); n != 1 {
				t.Fatalf("the refused declaration was stored (latest declaration %d)", n)
			}
		})
	}
}

// A refusal that is not about width keeps its old meaning: a node whose head is accepted keeps what it holds until the head lands (the declaration describes the pull request and
// conflicts are classified from it), a holder that never declared has nothing to narrow, and the same declaration again is a replay.
func TestNarrowingKeepsTheOtherRefusals(t *testing.T) {
	t.Run("an accepted head does not narrow", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		f.putPlan("n", 0, "n-r1", addNode("held", dag.NodeImplementation))
		f.declare("n", "held", "a.go", "b.go")
		f.acceptNode("n", "held", pinnedOpts)
		_, err := f.sched.DeclareRegions(context.Background(), "n", "held", "parent", []Region{region("a.go", "file", "")})
		wantRefusalText(t, err, "disposition_conflict", "accepted")
	})
	t.Run("a holder that never declared has nothing to narrow", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		f.putPlan("n", 0, "n-r1", addNode("held", dag.NodeImplementation))
		f.startNode("n", "held")
		_, err := f.sched.DeclareRegions(context.Background(), "n", "held", "parent", []Region{region("a.go", "file", "")})
		wantRefusalText(t, err, "disposition_conflict", "never declared")
	})
	t.Run("the same declaration again is a replay", func(t *testing.T) {
		f := narrowWorld(t, nr("pkg/y.go", "file", "", "local", ""))
		got, err := redeclareHeld(f, nr("pkg/y.go", "file", "", "local", ""))
		if err != nil || !got.Replayed || got.Seq != 1 {
			t.Fatalf("the identical declaration = %+v, %v, want a replay of declaration 1", got, err)
		}
	})
}

func wantRefusalText(t *testing.T, err error, reason, mention string) {
	t.Helper()
	var refused *store.RefusedError
	if !errors.As(err, &refused) || refused.Reason != reason {
		t.Fatalf("got %v, want the refusal %s", err, reason)
	}
	if !strings.Contains(refused.Detail, mention) {
		t.Fatalf("the refusal %q does not mention %q", refused.Detail, mention)
	}
}
