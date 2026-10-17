package cli

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// CRW-1404: the release declares a node's admission regions by their normal form - trimmed, the path cleaned as DeclareRegions cleans
// it - so a DESIGN edit region that repeats a planned region in another spelling is that planned region, with the plan's change and
// exclusive flag, and not a second region of the same place that makes the relay refuse the node's whole declaration.

// TestReleaseDeclaresAPlannedRegionInAnotherSpelling: the plan states a rename that is exclusive, an exclusive tree and a deleted symbol;
// the DESIGN repeats them with blanks around them, as ./path, or in several spellings at once beside a region of its own that overlaps
// them. The node is declared (no warning), each planned region once with the plan's change and exclusive flag, and the DESIGN's own
// region beside them.
func TestReleaseDeclaresAPlannedRegionInAnotherSpelling(t *testing.T) {
	planned := []compile.Region{
		{Path: "pkg/a.go", Kind: "file", Change: "rename", Exclusive: true},
		{Path: "docs", Kind: "tree", Exclusive: true},
		{Path: "pkg/b.go", Kind: "symbol", Key: "Run", Change: "delete"},
	}
	plan := []string{"pkg/a.go", "docs/", "pkg/b.go#Run"}
	for _, c := range []struct {
		name string
		edit []string
		own  int // the DESIGN's own regions beside the planned ones
	}{
		{"whitespace", []string{" pkg/a.go ", "\tdocs/ ", " pkg/b.go#Run"}, 0},
		{"dot-slash", []string{"./pkg/a.go", "./docs/", "./pkg/b.go#Run"}, 0},
		{"overlapping", []string{"pkg//a.go", " ./pkg/a.go", "pkg/./a.go", "docs//", "./docs/", "pkg/../pkg/b.go#Run", "pkg/"}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1", Regions: planned})
			r := l.buildLane("run-forms00001", l.handoffBytes(nil), LaneOptions{})
			node := l.nodeOf["A-1"]
			regions := ops.AdmissionRegions(plan, c.edit)
			got, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-forms", Regions: regions})
			if err != nil || !got.Released || got.RegionsDeclared != "declared" || got.RegionsWarning != "" {
				t.Fatalf("the release with the regions %q: %+v %v", regions, got, err)
			}
			held, found, err := r.Sched.NodeRegions(ctx, l.plan, node)
			if err != nil || !found {
				t.Fatalf("the node's declaration: %v %v", found, err)
			}
			if len(held) != len(planned)+c.own {
				t.Fatalf("the node declares %+v, want the %d planned regions once and %d of its own", held, len(planned), c.own)
			}
			want := map[string]dagsched.Region{
				"file pkg/a.go":   {Path: "pkg/a.go", Kind: "file", Change: "rename", Exclusive: true},
				"tree docs":       {Path: "docs", Kind: "tree", Change: "edit", Exclusive: true},
				"symbol pkg/b.go": {Path: "pkg/b.go", Kind: "symbol", Key: "Run", Change: "delete"},
				"tree pkg":        {Path: "pkg", Kind: "tree", Change: "edit"},
			}
			for _, rg := range held {
				w, ok := want[rg.Kind+" "+rg.Path]
				if !ok || rg.Key != w.Key || rg.Change != w.Change || rg.Exclusive != w.Exclusive {
					t.Errorf("the node declares %+v, want %+v", rg, w)
				}
			}
		})
	}
}

// TestReleaseDeclaresAPlannedPathWithAHashAsThePlanStatesIt (CRW-1408): a planned region is declared field by field as the plan holds
// it (path, kind, symbol), not by reading its text again, so a planned file and a planned tree whose paths hold "#" (compile.cleanRegion
// allows them) stay a file and a tree, and a planned symbol keeps its path with "#" in it; a DESIGN edit region is still read from its
// text, a symbol after "#".
func TestReleaseDeclaresAPlannedPathWithAHashAsThePlanStatesIt(t *testing.T) {
	planned := []compile.Region{
		{Path: "pkg/a#b.go", Kind: "file", Exclusive: true},
		{Path: "gen#1", Kind: "tree"},
		{Path: "pkg/c#d.go", Kind: "symbol", Key: "Run", Change: "delete"},
	}
	// the plan's regions as the manifest carries them (compile.Region.String of the cleaned region)
	plan := []string{"pkg/a#b.go", "gen#1/", "pkg/c#d.go#Run"}
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1", Regions: planned})
	r := l.buildLane("run-hashes0001", l.handoffBytes(nil), LaneOptions{})
	node := l.nodeOf["A-1"]
	regions := ops.AdmissionRegions(plan, []string{" pkg/a#b.go ", "pkg/e.go#Main"})
	got, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-hashes", Regions: regions})
	if err != nil || !got.Released || got.RegionsDeclared != "declared" || got.RegionsWarning != "" {
		t.Fatalf("the release with the regions %q: %+v %v", regions, got, err)
	}
	held, found, err := r.Sched.NodeRegions(ctx, l.plan, node)
	if err != nil || !found {
		t.Fatalf("the node's declaration: %v %v", found, err)
	}
	want := map[string]dagsched.Region{
		"file pkg/a#b.go":   {Path: "pkg/a#b.go", Kind: "file", Change: "edit", Exclusive: true},
		"tree gen#1":        {Path: "gen#1", Kind: "tree", Change: "edit"},
		"symbol pkg/c#d.go": {Path: "pkg/c#d.go", Kind: "symbol", Key: "Run", Change: "delete"},
		"symbol pkg/e.go":   {Path: "pkg/e.go", Kind: "symbol", Key: "Main", Change: "edit"},
	}
	if len(held) != len(want) {
		t.Errorf("the node declares %+v, want the %d planned regions as the plan states them and the DESIGN's symbol", held, len(planned))
	}
	for _, rg := range held {
		w, ok := want[rg.Kind+" "+rg.Path]
		if !ok || rg.Key != w.Key || rg.Change != w.Change || rg.Exclusive != w.Exclusive {
			t.Errorf("the node declares %+v, want %+v", rg, w)
		}
	}
}

// CRW-1411: a planned region is matched by its normal form, and a request text names every planned region one of its readings
// (ops.RegionReadings) is. A path spelled unclean with "#" in it (./pkg/a#b.go, gen#1//, pkg/a#b//c.go) is the planned file or tree it
// names, and a text that is both a planned file and a planned symbol declares both.

// TestReleaseDeclaresAPlannedHashPathInAnUncleanSpelling (CRW-1411): the plan holds a file and a tree whose paths hold "#" and a file
// "pkg/a#b/c.go"; the request names them unclean, sent as they are and through AdmissionRegions beside the plan's clean texts. The node
// declares exactly the three planned regions with the plan's flags.
func TestReleaseDeclaresAPlannedHashPathInAnUncleanSpelling(t *testing.T) {
	planned := []compile.Region{
		{Path: "pkg/a#b.go", Kind: "file", Change: "rename", Exclusive: true},
		{Path: "gen#1", Kind: "tree", Exclusive: true},
		{Path: "pkg/a#b/c.go", Kind: "file", Change: "delete"},
	}
	plan := []string{"pkg/a#b.go", "gen#1/", "pkg/a#b/c.go"}
	edit := []string{"./pkg/a#b.go", "gen#1//", "pkg/a#b//c.go"}
	for _, c := range []struct {
		name    string
		regions []string
	}{
		{"as sent", edit},
		{"admission", ops.AdmissionRegions(plan, edit)},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1", Regions: planned})
			r := l.buildLane("run-unclean00001", l.handoffBytes(nil), LaneOptions{})
			node := l.nodeOf["A-1"]
			got, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-unclean", Regions: c.regions})
			if err != nil || !got.Released || got.RegionsDeclared != "declared" || got.RegionsWarning != "" {
				t.Fatalf("the release with the regions %q: %+v %v", c.regions, got, err)
			}
			held, found, err := r.Sched.NodeRegions(ctx, l.plan, node)
			if err != nil || !found {
				t.Fatalf("the node's declaration: %v %v", found, err)
			}
			want := map[string]dagsched.Region{
				"file pkg/a#b.go":   {Path: "pkg/a#b.go", Kind: "file", Change: "rename", Exclusive: true},
				"tree gen#1":        {Path: "gen#1", Kind: "tree", Change: "edit", Exclusive: true},
				"file pkg/a#b/c.go": {Path: "pkg/a#b/c.go", Kind: "file", Change: "delete"},
			}
			if len(held) != len(want) {
				t.Errorf("the node declares %+v, want the %d planned regions", held, len(want))
			}
			for _, rg := range held {
				w, ok := want[rg.Kind+" "+rg.Path]
				if !ok || rg.Key != w.Key || rg.Change != w.Change || rg.Exclusive != w.Exclusive {
					t.Errorf("the node declares %+v, want %+v", rg, w)
				}
			}
		})
	}
}

// TestReleaseDeclaresTheMilestoneTextsOfAPlannedHashPath (CRW-1411): the handoff's regions as they are written, a file "./pkg/a#b.go"
// (rename, exclusive), a tree "gen#1/" (exclusive) and a region with no kind "pkg/a#b//c.go" (delete). The release takes the texts the
// milestone gives them, through AdmissionRegions, and declares the file "pkg/a#b.go", the tree "gen#1" and the file "pkg/a#b/c.go" with
// their flags. The region with no kind is the kind-default case: ops.RegionFormOf reads it as a file.
func TestReleaseDeclaresTheMilestoneTextsOfAPlannedHashPath(t *testing.T) {
	planned := []compile.Region{
		{Path: "./pkg/a#b.go", Kind: "file", Change: "rename", Exclusive: true},
		{Path: "gen#1/", Kind: "tree", Exclusive: true},
		{Path: "pkg/a#b//c.go", Change: "delete"},
	}
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1", Regions: planned})
	r := l.buildLane("run-milestxt0001", l.handoffBytes(nil), LaneOptions{})
	node := l.nodeOf["A-1"]
	m, err := r.Milestone(ctx, l.plan, "M1")
	if err != nil || len(m.Nodes) != 1 {
		t.Fatalf("the nodes of M1: %+v %v", m.Nodes, err)
	}
	regions := ops.AdmissionRegions(m.Nodes[0].Regions, nil)
	got, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-milestxt", Regions: regions})
	if err != nil || !got.Released || got.RegionsDeclared != "declared" || got.RegionsWarning != "" {
		t.Fatalf("the release with the regions %q: %+v %v", regions, got, err)
	}
	held, found, err := r.Sched.NodeRegions(ctx, l.plan, node)
	if err != nil || !found {
		t.Fatalf("the node's declaration: %v %v", found, err)
	}
	want := map[string]dagsched.Region{
		"file pkg/a#b.go":   {Path: "pkg/a#b.go", Kind: "file", Change: "rename", Exclusive: true},
		"tree gen#1":        {Path: "gen#1", Kind: "tree", Change: "edit", Exclusive: true},
		"file pkg/a#b/c.go": {Path: "pkg/a#b/c.go", Kind: "file", Change: "delete"},
	}
	if len(held) != len(want) {
		t.Errorf("the node declares %+v, want the %d planned regions", held, len(want))
	}
	for _, rg := range held {
		w, ok := want[rg.Kind+" "+rg.Path]
		if !ok || rg.Key != w.Key || rg.Change != w.Change || rg.Exclusive != w.Exclusive {
			t.Errorf("the node declares %+v, want %+v", rg, w)
		}
	}
}

// TestReleaseDeclaresAPlannedFileAndSymbolOfOneText (CRW-1411): the plan holds a file "pkg/a#b.go" (exclusive) and the symbol "b.go" of
// "pkg/a" (delete), which have one text. Both are declared with their own flags, whichever the plan lists first, and through the
// milestone's texts.
func TestReleaseDeclaresAPlannedFileAndSymbolOfOneText(t *testing.T) {
	file := compile.Region{Path: "pkg/a#b.go", Kind: "file", Exclusive: true}
	symbol := compile.Region{Path: "pkg/a", Kind: "symbol", Key: "b.go", Change: "delete"}
	for _, c := range []struct {
		name    string
		planned []compile.Region
	}{
		{"file first", []compile.Region{file, symbol}},
		{"symbol first", []compile.Region{symbol, file}},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1", Regions: c.planned})
			r := l.buildLane("run-sametext0001", l.handoffBytes(nil), LaneOptions{})
			node := l.nodeOf["A-1"]
			m, err := r.Milestone(ctx, l.plan, "M1")
			if err != nil || len(m.Nodes) != 1 {
				t.Fatalf("the nodes of M1: %+v %v", m.Nodes, err)
			}
			regions := ops.AdmissionRegions(m.Nodes[0].Regions, nil)
			got, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-sametext", Regions: regions})
			if err != nil || !got.Released || got.RegionsDeclared != "declared" || got.RegionsWarning != "" {
				t.Fatalf("the release with the regions %q: %+v %v", regions, got, err)
			}
			held, found, err := r.Sched.NodeRegions(ctx, l.plan, node)
			if err != nil || !found {
				t.Fatalf("the node's declaration: %v %v", found, err)
			}
			want := map[string]dagsched.Region{
				"file pkg/a#b.go": {Path: "pkg/a#b.go", Kind: "file", Change: "edit", Exclusive: true},
				"symbol pkg/a":    {Path: "pkg/a", Kind: "symbol", Key: "b.go", Change: "delete"},
			}
			if len(held) != len(want) {
				t.Errorf("the node declares %+v, want the file and the symbol of the one text", held)
			}
			for _, rg := range held {
				w, ok := want[rg.Kind+" "+rg.Path]
				if !ok || rg.Key != w.Key || rg.Change != w.Change || rg.Exclusive != w.Exclusive {
					t.Errorf("the node declares %+v, want %+v", rg, w)
				}
			}
		})
	}
}

// TestRegionFormOfReadsNoKindAsAFile (CRW-1411): a planned region with no kind is a file. ops.RegionFormOf takes the path field by
// field, so a "#" in it never makes a symbol.
func TestRegionFormOfReadsNoKindAsAFile(t *testing.T) {
	for _, c := range []struct {
		path string
		want ops.RegionForm
	}{
		{"pkg/k.go", ops.RegionForm{Path: "pkg/k.go", Kind: "file"}},
		{"gen#1/", ops.RegionForm{Path: "gen#1", Kind: "file"}},
	} {
		if got := ops.RegionFormOf(c.path, "", ""); got != c.want {
			t.Errorf("RegionFormOf(%q, no kind) = %+v, want %+v", c.path, got, c.want)
		}
	}
}

// TestReleaseOfAnotherRunsReplayNamesItsState: a release the relay answers with the replay of an execution another run holds (the
// fallback of a land, CRW-1397) carries that run's state as the first answer of land_node_held does, so a land told of a run that has
// ended names its cancel.
func TestReleaseOfAnotherRunsReplayNamesItsState(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	node := l.nodeOf["A-1"]
	first := l.relayFor("run-holder0001", l.handoffBytes(nil), 1)
	res, err := first.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: first.RunID, RequestID: "req-1"})
	if err != nil || !res.Released {
		t.Fatalf("the holder's release: %+v %v", res, err)
	}
	// the holder's process started, died and the run ended with the execution it holds
	j, err := journal.Open(ctx, l.w.st, first.RunID, journal.Identity{PlanID: l.plan, PlanRevision: 1, ActorTaskID: labOwner, EpochNonce: first.RunID}, journal.Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	j.Append(engine.Event{RunID: first.RunID, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "x", "script_digest": "sha256:x", "args_digest": "sha256:x"}})
	j.Append(engine.Event{RunID: first.RunID, Kind: engine.KindRun, Event: "finished", Data: map[string]any{"status": engine.RunScriptError, "error_kind": "script_error"}})
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	// another run of the same session answers the holder's release again: the relay replays the holder's execution
	other := l.relayFor("run-holder0001-b", l.handoffBytes(nil), 1)
	other.RunID = "run-other00001"
	other.Nonce, other.Handoff = first.Nonce, first.Handoff
	again, err := other.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: other.RunID, RequestID: "req-1"})
	if err != nil || !again.Released || !again.Replayed || again.Run != first.RunID {
		t.Fatalf("the replay of the holder's release: %+v %v", again, err)
	}
	if again.HeldRunState != StateFailed {
		t.Fatalf("the replay names the holder's state %q, want %q", again.HeldRunState, StateFailed)
	}
}

// TestLandHeadOfANodeAnEndedRunHoldsOnTheReplay: a land whose release the relay answers with the replay of the execution an ended run
// holds (released, but under that run: the fallback after the relay's first answer) is held as the first answer is - it names crw flow
// cancel of the ended run, not its resume, and the row records held_run_state and the warning land_node_held_by_ended_run.
func TestLandHeadOfANodeAnEndedRunHoldsOnTheReplay(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	l.outsideHead("A-1", "outside-a1")
	author := l.authorThread("claude-fake-sonnet", "high")
	l.killAt = l.writes + 1
	run1 := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
	if _, killed := l.process(run1.RunID, false); !killed {
		t.Fatal("the first land was not killed")
	}
	l.killAt = 0
	l.endRun(run1.RunID)

	// the relay answers the second land as the replay of run1's execution, with run1's state, as SchedRelay.Release answers a replay
	// another run holds
	l.releaseAnswer = func(req ops.ReleaseRequest, out ops.ReleaseResult) ops.ReleaseResult {
		if !req.Land || out.Reason != ops.ReasonLandNodeHeld {
			return out
		}
		return ops.ReleaseResult{Released: true, Replayed: true, ExecutionID: out.HeldExecution, Run: out.HeldRun, HeldRunState: out.HeldRunState}
	}
	run2 := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
	v := landValueOf(t, l.runToEnd(run2.RunID))
	if v.State != "unreleased" || v.Reason != "land_node_held" {
		t.Fatalf("a land answered with the replay of an ended run's execution: %+v", v)
	}
	res, _ := ReadResult(l.w.state, run2.RunID)
	if !strings.Contains(string(res.Value), "crw flow cancel "+run1.RunID) || strings.Contains(string(res.Value), "crw flow resume "+run1.RunID) {
		t.Errorf("the land does not name the command that gives the ended run's execution up: %s", res.Value)
	}
	rows := l.releaseRowsOf(l.nodeOf["A-1"])
	if len(rows) != 1 || str(rows[0].Data, "held_run") != run1.RunID || str(rows[0].Data, "held_run_state") != StateFailed ||
		!strings.Contains(strings.Join(rowWarningsOf(rows[0].Data), " | "), ops.WarnLandHeldByEndedRun) {
		t.Fatalf("the release row of the held land: %+v", rows)
	}
}
