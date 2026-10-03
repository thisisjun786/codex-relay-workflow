package dagsched

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The regions a node holds are held until its head lands (contract 7.2). A declaration that could only free them for another node early is refused
// once a release or an execution holds them, whether the node had declared before (a narrowing) or had not (an undeclared holder that becomes a
// declared one), and a harmless repeat is still a replay.
func TestDeclareRegionsCannotFreeAHeldNode(t *testing.T) {
	setup := func(t *testing.T, declareHolder bool) *fixture {
		f := newFixture(t)
		f.projectParent()
		f.putPlan("h", 0, "h-r1", addNode("held", dag.NodeImplementation), addNode("cand", dag.NodeImplementation))
		if declareHolder {
			f.declare("h", "held", "a.go")
		}
		f.declare("h", "cand", "a.go")
		f.startNode("h", "held")
		if got := f.read("h").node("cand").Reason; got != DeferEditOverlap {
			t.Fatalf("the candidate against a running holder = %s", got)
		}
		return f
	}
	refused := func(t *testing.T, f *fixture, path string) {
		t.Helper()
		_, err := f.sched.DeclareRegions(context.Background(), "h", "held", "parent", []Region{region(path, "file", "")})
		var r *store.RefusedError
		if !errors.As(err, &r) || r.Reason != "disposition_conflict" {
			t.Fatalf("a declaration of %s for a running node: %v, want disposition_conflict", path, err)
		}
		if got := f.read("h").node("cand").Reason; got != DeferEditOverlap {
			t.Fatalf("the candidate after the refused declaration = %s, want it still deferred", got)
		}
	}
	t.Run("narrowing the declaration of a running node", func(t *testing.T) {
		f := setup(t, true)
		refused(t, f, "b.go")
		if d := f.declare("h", "held", "a.go"); !d.Replayed || d.Seq != 1 {
			t.Fatalf("the identical declaration = %+v, want a replay", d)
		}
	})
	t.Run("declaring for the first time after the node started", func(t *testing.T) {
		f := setup(t, false)
		refused(t, f, "b.go")
	})
	t.Run("a release that was decided holds too", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		f.putPlan("h", 0, "h-r1", addNode("held", dag.NodeImplementation), addNode("cand", dag.NodeImplementation))
		f.declare("h", "held", "a.go")
		f.declare("h", "cand", "a.go")
		f.releaseRow("h", "held")
		refused(t, f, "b.go")
	})
	t.Run("before the release a declaration may change freely", func(t *testing.T) {
		f := newFixture(t)
		f.putPlan("h", 0, "h-r1", addNode("held", dag.NodeImplementation))
		f.declare("h", "held", "a.go")
		if d := f.declare("h", "held", "b.go"); d.Seq != 2 {
			t.Fatalf("redeclaration before release = %+v", d)
		}
	})
	t.Run("once the head has landed it may", func(t *testing.T) {
		f := newFixture(t)
		f.putPlan("h", 0, "h-r1", addNode("held", dag.NodeImplementation))
		f.declare("h", "held", "a.go")
		a := f.acceptNode("h", "held", pinnedOpts)
		f.integrate(a, "owner/repo", "dev", true, true)
		if d := f.declare("h", "held", "b.go"); d.Seq != 2 {
			t.Fatalf("redeclaration after landing = %+v", d)
		}
	})
}

// An attached managed start belongs to its relationship: once that relationship is finished the issue is free again (a managed row is never
// released after it attached, so counting it would own the issue forever). A start still pending, and a relationship still open, own it.
func TestFinishedRelationshipFreesTheIssue(t *testing.T) {
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	f.projectParent()
	f.foreignRelationship("CRW-research")
	f.managedRow("req-old", "CRW-research", "attached", "accepted")
	if got := f.read("p1").node("research").Reason; got != SkipAlreadyOwned {
		t.Fatalf("open relationship: research = %s", got)
	}
	f.exec("UPDATE relationships SET status = 'archived'")
	if n := f.read("p1").node("research"); n.Disposition != DispReady {
		t.Fatalf("finished relationship with its attached start: research = %+v", n)
	}
	f.managedRow("req-new", "CRW-research", "reserved", "")
	if got := f.read("p1").node("research").Reason; got != SkipAlreadyOwned {
		t.Fatalf("a start pending again: research = %s", got)
	}
}

// An accepted node still has a relationship: a pause keeps its regions held, a cancellation before landing frees them, and a landing that is
// proven stays proven.
func TestAcceptedNodeFollowsItsRelationship(t *testing.T) {
	setup := func(t *testing.T) (*fixture, accepted) {
		f := newFixture(t)
		f.projectParent()
		f.putPlan("r", 0, "r-r1", addNode("done", dag.NodeImplementation), addNode("cand", dag.NodeImplementation))
		f.declare("r", "done", "a.go")
		f.declare("r", "cand", "a.go")
		return f, f.acceptNode("r", "done", pinnedOpts)
	}
	t.Run("paused after acceptance", func(t *testing.T) {
		f, _ := setup(t)
		f.exec("UPDATE relationships SET status = 'paused'")
		r := f.read("r")
		if n := r.node("done"); n.State != StatePausedNode || n.Reason != SkipAlreadyOwned {
			t.Fatalf("done = %+v", n)
		}
		if n := r.node("cand"); n.Reason != DeferEditOverlap {
			t.Fatalf("cand = %+v, want it still waiting for the paused holder", n)
		}
	})
	t.Run("cancelled after acceptance, not landed", func(t *testing.T) {
		f, _ := setup(t)
		f.exec("UPDATE relationships SET status = 'cancelled'")
		r := f.read("r")
		if n := r.node("done"); n.State != StateCancelled || n.Reason != SkipAlreadyOwned {
			t.Fatalf("done = %+v", n)
		}
		if n := r.node("cand"); n.Disposition != DispReady {
			t.Fatalf("cand = %+v, want it released from the holder that was cancelled", n)
		}
	})
	t.Run("cancelled after the head landed", func(t *testing.T) {
		f, a := setup(t)
		f.integrate(a, "owner/repo", "dev", true, true)
		f.exec("UPDATE relationships SET status = 'cancelled'")
		if n := f.read("r").node("done"); n.State != StateIntegrated || n.Reason != DoneIntegrated {
			t.Fatalf("done = %+v, want the proven landing kept", n)
		}
	})
}

// B-17: the frozen copy of a manifest is read and compared, not only found. A directory in its place, a document that is not JSON and a copy that
// disagrees with the receipt all block; a good copy does not; and the store half of the checks (SkipArtifactBytes) touches no file at all.
func TestReadyFrozenManifestMustBeReadable(t *testing.T) {
	prepare := func(t *testing.T) (*fixture, accepted, string) {
		f := newFixture(t)
		forkJoinPlan(f, "p1")
		f.projectParent()
		a := f.acceptNode("p1", "research", acceptOpts{})
		frozen := filepath.Join(a.Root, "frozen")
		f.exec("UPDATE events SET manifest_ref = ? WHERE event_id = ?", frozen, a.Event)
		return f, a, frozen
	}
	cases := []struct {
		name    string
		prepare func(t *testing.T, a accepted, frozen string)
		blocked bool
	}{
		{"a good copy", func(t *testing.T, a accepted, frozen string) {
			entries, err := store.BuildManifest(context.Background(), a.Files, []string{a.Root})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.FreezeManifest(entries, frozen); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"no copy", func(t *testing.T, a accepted, frozen string) {}, true},
		{"a directory where the document should be", func(t *testing.T, a accepted, frozen string) {
			if err := os.MkdirAll(filepath.Join(frozen, "MANIFEST.json"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"a document that is not JSON", func(t *testing.T, a accepted, frozen string) {
			if err := os.MkdirAll(frozen, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(frozen, "MANIFEST.json"), []byte("not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"a copy of other bytes", func(t *testing.T, a accepted, frozen string) {
			other := filepath.Join(a.Root, "other.md")
			if err := os.WriteFile(other, []byte("something else\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			entries, err := store.BuildManifest(context.Background(), []string{other}, []string{a.Root})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.FreezeManifest(entries, frozen); err != nil {
				t.Fatal(err)
			}
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, a, frozen := prepare(t)
			c.prepare(t, a, frozen)
			n := f.read("p1").node("design")
			if c.blocked != (n.Reason == BlockedInputMissing) || (!c.blocked && n.Disposition != DispReady) {
				t.Fatalf("design = %+v", n)
			}
			skipped, err := f.sched.Read(context.Background(), "p1", ReadyOptions{SkipArtifactBytes: true})
			if err != nil {
				t.Fatal(err)
			}
			if got := skipped.node("design"); got.Disposition != DispReady {
				t.Fatalf("store half: design = %+v, want it independent of every file", got)
			}
		})
	}
}

// The child's newest word decides, even while an earlier report is under verification.
func TestReadyBlockedReportWhileVerifying(t *testing.T) {
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	f.projectParent()
	rid := f.startNode("p1", "research")
	insert := func(id, outcome, at string) {
		f.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
			" VALUES (?, ?, 1, ?, ?, 'child', 'child-research', 'turn-x', 'completed', '{}', 'final', ?, ?)", id, rid, dig(id), outcome, at, at)
	}
	insert("evt-report", "ready_for_review", "a")
	f.exec("INSERT INTO revision_lineage (relationship_id, execution_generation, event_id, revision_hash, declared_by, recorded_at) VALUES (?, 1, 'evt-report', ?, 'child', 'a')", rid, dig("evt-report"))
	f.exec("INSERT INTO verification_claims (event_id, claim_turn_id, claimed_at) VALUES ('evt-report', 't', 'a')")
	if n := f.read("p1").node("research"); n.State != StateVerifying || n.Reason != SkipAlreadyOwned {
		t.Fatalf("under verification: research = %+v", n)
	}
	insert("evt-blocked", "blocked_needs_input", "b")
	if n := f.read("p1").node("research"); n.Reason != BlockedInputUnverifiedAtUse {
		t.Fatalf("a blocked report after it: research = %+v, want the newest word", n)
	}
}

// capacity.Reserve refuses when held >= ceiling, so the reader's slots are the ceiling rounded up: a fractional limit must not read as no capacity
// while Reserve would still accept.
func TestCapacityFractionalCeiling(t *testing.T) {
	f := newFixture(t)
	f.declareLimit("project", "P-TEST", "runs", 0.5)
	if c := f.capacityNow(); c.Free != 1 || c.Ceiling != 1 {
		t.Fatalf("ceiling 0.5, nothing held: %+v", c)
	}
	f.holdSlots(1)
	if c := f.capacityNow(); c.Free != 0 {
		t.Fatalf("ceiling 0.5, one held: %+v", c)
	}
}

// The stored pass is read back as the table holds it and compared with what the reading said, not with the function that wrote it.
func TestPassDispositionsAreStoredAsRead(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("pd", 0, "pd-r1", addNode("n1", dag.NodeNonPR), addNode("n2", dag.NodeNonPR), addEdge("e", "n1", "n2", dag.EdgeArtifactVerified, nil))
	f.recordPass("pd")
	rows := f.passRows("pd")
	var stored []struct {
		NodeID      string `json:"node_id"`
		State       string `json:"state"`
		Disposition string `json:"disposition"`
		Reason      string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(rows[0].dispositions), &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || stored[0].NodeID != "n1" || stored[0].Disposition != DispReady || stored[0].Reason != "" ||
		stored[1].NodeID != "n2" || stored[1].Disposition != DispWait || stored[1].Reason != WaitEdge("e") || stored[1].State != StateWaiting {
		t.Fatalf("stored dispositions = %+v", stored)
	}
	var order []string
	if err := json.Unmarshal([]byte(rows[0].order), &order); err != nil || len(order) != 1 || order[0] != "n1" {
		t.Fatalf("stored order = %s (%v)", rows[0].order, err)
	}
}

// Two spellings of one place must not be two locks: a repository path is canonical, and a path keeps the spaces that are part of a name.
func TestRegionSpellingsShareOneLock(t *testing.T) {
	holderThenCandidate := func(t *testing.T, holder, candidate Region) NodeReading {
		f := newFixture(t)
		f.projectParent()
		f.putPlan("s", 0, "s-r1", addNode("held", dag.NodeImplementation), addNode("cand", dag.NodeImplementation))
		if _, err := f.sched.DeclareRegions(context.Background(), "s", "held", "parent", []Region{holder}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.sched.DeclareRegions(context.Background(), "s", "cand", "parent", []Region{candidate}); err != nil {
			t.Fatal(err)
		}
		f.startNode("s", "held")
		return f.read("s").node("cand")
	}
	local := func(repository, p string) Region {
		return Region{Repository: repository, Path: p, Kind: "file", Change: "edit"}
	}
	checkout := t.TempDir()
	if err := os.Mkdir(filepath.Join(checkout, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{checkout + "/", checkout + "/.", checkout + "/sub/..", "/" + checkout} {
		if got := holderThenCandidate(t, local(checkout, "a.go"), local(alias, "a.go")); got.Reason != DeferEditOverlap {
			t.Errorf("repository %q against %s: %+v, want the same lock", alias, checkout, got)
		}
	}
	if got := holderThenCandidate(t, local(checkout, "a.go"), local(t.TempDir(), "a.go")); got.Disposition != DispReady {
		t.Errorf("another repository: %+v", got)
	}
	if got := holderThenCandidate(t, local("owner/repo", "dir/./a.go"), local("owner/repo", "dir/a.go")); got.Reason != DeferEditOverlap {
		t.Errorf("a dotted path against the clean one: %+v", got)
	}
	// a space inside a name is part of the name: "dir /a.go" is not under "dir"
	if got := holderThenCandidate(t, Region{Repository: "owner/repo", Path: "dir", Kind: "tree", Change: "edit"}, local("owner/repo", "dir /a.go")); got.Disposition != DispReady {
		t.Errorf("a file under a directory whose name has a space: %+v, want it disjoint from dir", got)
	}
	if got := holderThenCandidate(t, Region{Repository: "owner/repo", Path: "dir ", Kind: "tree", Change: "edit"}.withName("dir x"), local("owner/repo", "dir x/a.go")); got.Reason != DeferEditOverlap {
		t.Errorf("a tree with a space inside its name: %+v", got)
	}
	// a link to a checkout is the checkout
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if got := holderThenCandidate(t, local(real, "a.go"), local(link, "a.go")); got.Reason != DeferEditOverlap {
		t.Errorf("a symbolic link to the checkout: %+v, want the same lock", got)
	}
	if got := holderThenCandidate(t, local(link+"/", "a.go"), local(real+"/.", "a.go")); got.Reason != DeferEditOverlap {
		t.Errorf("the link and the real path in other spellings: %+v", got)
	}
	// ".." after a link means the target's parent, not the link's: /base/link/.. is the parent of the real directory
	base := t.TempDir()
	deep := filepath.Join(base, "real", "sub")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.Symlink(deep, filepath.Join(elsewhere, "link")); err != nil {
		t.Fatal(err)
	}
	if got := holderThenCandidate(t, local(filepath.Join(base, "real"), "a.go"), local(elsewhere+"/link/..", "a.go")); got.Reason != DeferEditOverlap {
		t.Errorf("a path that goes through a link and then up: %+v, want the directory it reaches", got)
	}
	f := newFixture(t)
	f.putPlan("s", 0, "s-r1", addNode("n", dag.NodeImplementation))
	for _, bad := range []Region{local("owner/repo", " a.go"), local("owner/repo", "a.go "), local("owner/repo", "dir \n/a.go"), Region{Repository: "owner/repo", Path: "dir ", Kind: "tree", Change: "edit"},
		local(" owner/repo", "a.go"), local("owner/repo/", "a.go"), local("./owner/repo", "a.go"), local("../x", "a.go"), local("owner/repo\t", "a.go"),
		local("repo", "a.go"), local("a/b/c", "a.go"), local(".", "a.go"), local("./x", "a.go"), local("owner/..", "a.go"), local("owner/", "a.go"), local("owner/re po", "a.go"),
		local("/nonexistent/checkout", "a.go"), local(filepath.Join(t.TempDir(), "missing"), "a.go"), local("/etc/hostname", "a.go")} {
		if _, err := f.sched.DeclareRegions(context.Background(), "s", "n", "parent", []Region{bad}); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
}

func (r Region) withName(path string) Region { r.Path = path; return r }
