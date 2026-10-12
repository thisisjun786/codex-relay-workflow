package scenario

import (
	"io/fs"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows/lib"
)

// The two invariants of CRW-1202's last correction round (verifier rf4 on b4ec3fd8).
//
// The release of a lane compares the live plan with the plan the audited DESIGN stands on in the flow, before Relay.Release: the plan
// the current revision was made for, or the plan a later signal judged and kept that revision for. A release whose plan is another at
// that comparison is not made; the lane reads the plan again, which is a trigger, and its signal re-plans and audits again (or keeps
// the revision) before the node is released. The relay's release commit does not compare the plan slice, so a plan that changes
// between the comparison and the commit is not caught there; CRW-1288 makes the relay compare the expected slice in its release
// transaction, and the verifier probe TestSolReleaseMustCompareAtRelayCommit is its first failing test.
//
// The worktree of a pinned architect turn is the call's own, and its removal is decided by the call's terminal row, which names it:
// the engine removes it after the row with a context of its own (not the run's, which a cancellation ends), and a resume removes every
// directory a terminal row names that is still there, so neither a cancellation right after the row nor a resume that replays the
// completed call leaves it behind.

// checkReleasesUnderTheirPlan fails for a release whose slice is not the slice the ledger revision it names was made for.
func checkReleasesUnderTheirPlan(t *testing.T, lab *Lab) {
	t.Helper()
	ledgers := map[string]engine.Event{}
	for _, ledger := range lab.Rows("crw.designLedger") {
		ledgers[str(ledger.Data, "revision")] = ledger
	}
	released := 0
	for _, release := range lab.Rows("crw.release") {
		if !flagOf(release.Data, "released") {
			continue
		}
		released++
		node, revision := str(release.Data, "node"), str(release.Data, "revision")
		slices, _ := ledgers[revision].Data["slices"].(map[string]any)
		if got, want := str(release.Data, "slice_digest"), str(slices, node); got != want {
			t.Errorf("%s released under %s with slice %s, but %s was audited for slice %s; release seq=%d calls=%v", node, revision, got, revision, want, release.Seq, lab.Labels())
		}
	}
	if released == 0 {
		t.Fatal("no node was released")
	}
}

func TestLaneReleaseComparesAuditedPlanRevision(t *testing.T) {
	// planChangedDuringRevisionAudit: P0 -> P1 -> P2 -> P1 (planCycleWorld) and, while the DESIGN delta the return to P1 made is audited,
	// B and C change once more: half of the nodes. The lane that waited for that signal must not be released under the delta made for P1.
	planChangedDuringRevisionAudit := func(t *testing.T) (*Lab, *atomic.Bool) {
		lab := planCycleWorld(t)
		var changed atomic.Bool
		lab.On("^design-audit:replan-s3:r1$", func(*Call) *fakehostPlan {
			if changed.CompareAndSwap(false, true) {
				for _, node := range []string{"B", "C"} {
					lab.Relay.ChangeNode(node, func(spec *NodeSpec) { spec.Regions = []string{node + "-three/"} })
				}
			}
			return nil
		})
		return lab, &changed
	}

	t.Run("plan changed during a revision audit", func(t *testing.T) {
		lab, changed := planChangedDuringRevisionAudit(t)
		got := mustRun(t, lab)
		if !changed.Load() {
			t.Fatalf("the plan was not changed during design-audit:replan-s3:r1; calls=%v", lab.Labels())
		}
		if got.State != "closed" {
			t.Fatalf("state %q; calls %v", got.State, lab.Labels())
		}
		checkReleasesUnderTheirPlan(t, lab)
		// the change is signalled and re-planned before B is released
		labels := lab.Labels()
		audit, build := index(labels, "design-audit:replan-s3:r1"), index(labels, "build:B:executor")
		replanned := false
		for i, label := range labels {
			if strings.HasPrefix(label, "replan:") && i > audit && i < build {
				replanned = true
			}
		}
		if !replanned {
			t.Errorf("no re-plan between the audit that saw the change and B's build: %v", labels)
		}
	})

	t.Run("plan changed during a revision audit, unchanged resume", func(t *testing.T) {
		lab, _ := planChangedDuringRevisionAudit(t)
		mustRun(t, lab)
		before := lab.LastSeq()
		lab.NewProcess("proc-2")
		if got := Value[laneResult](t, lab.Resume()); got.State != "closed" {
			t.Fatalf("resume: %+v; calls=%v", got, lab.Labels())
		}
		if live := lab.LiveCalls(before); len(live) != 0 {
			t.Errorf("unchanged resume made live calls: %+v", live)
		}
		for _, event := range rowsAfter(lab, before) {
			if event.Event == engine.EvSuperseded {
				t.Errorf("unchanged resume superseded %s at %s", event.Op, event.Path)
			}
			if event.Kind == engine.KindCall && event.Event == engine.EvCompleted && strings.HasPrefix(event.Op, "crw.") && event.Op != "crw.classify" {
				t.Errorf("unchanged resume ran %s live", event.Op)
			}
		}
		checkReleasesUnderTheirPlan(t, lab)
	})

	// The plan a signal judged is the one its revision stands on: a signal bound while the plan was P1 (half of the nodes changed) that
	// judged P0 again (no reason, continue) keeps the first revision for P0, and a lane that then reads P1 is a trigger, re-planned and
	// audited before its release; it is never released under the first revision with P1.
	t.Run("plan the signal judged is the plan the revision stands on", func(t *testing.T) {
		lab := NewLab(t, fourNodes()...)
		set := func(version string) {
			for _, node := range []string{"B", "C"} {
				regions := []string{strings.ToLower(node) + "/"}
				if version != "" {
					regions = []string{node + "-" + version + "/"}
				}
				lab.Relay.ChangeNode(node, func(spec *NodeSpec) { spec.Regions = regions })
			}
		}
		lab.On("^build:A:executor$", func(*Call) *fakehostPlan {
			set("one")
			return nil
		})
		var bound, done atomic.Bool
		lab.OnRow(func(event engine.Event) {
			if event.Kind != engine.KindCall || event.Event != engine.EvCompleted {
				return
			}
			if event.Op == "crw.signalBind" && bound.CompareAndSwap(false, true) {
				set("")
			}
			if event.Op == "crw.signalDone" && done.CompareAndSwap(false, true) {
				set("one")
			}
		})
		got := mustRun(t, lab)
		if !bound.Load() || !done.Load() {
			t.Fatalf("the signal of A's RECEIPT was not decided; calls=%v", lab.Labels())
		}
		if got.State != "closed" {
			t.Fatalf("state %q; calls %v", got.State, lab.Labels())
		}
		checkReleasesUnderTheirPlan(t, lab)
		if replans := labelsWithPrefix(lab, "replan:"); len(replans) == 0 {
			t.Errorf("half of the nodes changed against the revision and nothing re-planned: %v", lab.Labels())
		}
	})
}

// pinnedDirOf is the directory the prepared row of a call allocated, and the terminal row the call ended with.
func pinnedDirOf(t *testing.T, lab *Lab, callID string) (string, engine.Event) {
	t.Helper()
	path := ""
	var terminal engine.Event
	for _, event := range lab.Events() {
		if event.CallID != callID || event.Kind != engine.KindCall || event.Op != "agent" {
			continue
		}
		if event.Event == engine.EvPrepared {
			allocation, _ := event.Data["allocation"].(map[string]any)
			path = str(allocation, "path")
		}
		switch event.Event {
		case engine.EvCompleted, engine.EvHeld, engine.EvNullModel, engine.EvRefused, engine.EvSuperseded, engine.EvOutcomeUnknown:
			terminal = event
		}
	}
	if path == "" {
		t.Fatalf("call %s has no allocated directory", callID)
	}
	return path, terminal
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestPinnedWorktreeCleanupInCompletionUnit(t *testing.T) {
	// killAt kills the run right after the completed row of the first pinned architect call whose label starts with prefix
	killAt := func(lab *Lab, prefix string) *atomic.Value {
		var terminal atomic.Value
		var killed atomic.Bool
		lab.OnRow(func(event engine.Event) {
			if event.Op == "agent" && event.Event == engine.EvCompleted && strings.HasPrefix(event.Label, prefix) && killed.CompareAndSwap(false, true) {
				terminal.Store(event)
				lab.Kill()
			}
		})
		return &terminal
	}

	for _, testCase := range []struct {
		name, prefix string
		world        func(t *testing.T) *Lab
	}{
		{"cancelled after the terminal row of a reflection", "reflection:", func(t *testing.T) *Lab { return NewLab(t, twoNodes()...) }},
		{"cancelled after the terminal row of a re-plan", "replan:", planCycleWorld},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			lab := testCase.world(t)
			terminal := killAt(lab, testCase.prefix)
			lab.Run()
			row, ok := terminal.Load().(engine.Event)
			if !ok {
				t.Fatalf("the run was not stopped after a completed %s call; calls=%v", testCase.prefix, lab.Labels())
			}
			path, _ := pinnedDirOf(t, lab, row.CallID)
			if got := str(row.Data, "own_directory"); got != path {
				t.Errorf("the completed row of %s names the directory %q, want its own %s", row.Label, got, path)
			}
			lab.NewProcess("proc-2")
			if got := Value[laneResult](t, lab.Resume()); got.State != "closed" {
				t.Fatalf("resume: %+v; calls=%v", got, lab.Labels())
			}
			if exists(path) {
				t.Errorf("the pinned worktree %s of %s is still there after the resume", path, row.Label)
			}
		})
	}

	// A terminal row the resume writes itself for a pinned call decides the directory the same way: a reflection whose turn was in flight
	// when the process died (its worktree left behind: a hard kill removes nothing) is superseded by the resume that runs it again with
	// another packet, and that superseded row names the directory of the attempt it ends, which goes; the attempt that runs again in a
	// directory of its own removes that one with its completed row. A resume that goes on with the same call from what reconcile decided
	// keeps the directory until the call's own terminal row.
	for _, edited := range []bool{true, false} {
		name := "a terminal row the resume writes for an open pinned call names its directory"
		if !edited {
			name = "an open pinned call reconcile hands to the resume ends in its own directory"
		}
		t.Run(name, func(t *testing.T) {
			lab := NewLab(t, twoNodes()...)
			var started atomic.Value
			lab.OnRow(func(event engine.Event) {
				if event.Op == "agent" && event.Event == engine.EvTurnStarted && strings.HasPrefix(event.Label, "reflection:") && started.Load() == nil {
					started.Store(event)
					path := ""
					for _, prior := range lab.Events() {
						if prior.CallID == event.CallID && prior.Event == engine.EvPrepared {
							allocation, _ := prior.Data["allocation"].(map[string]any)
							path = str(allocation, "path")
						}
					}
					// the process dies with the turn in flight: nothing of it removes the directory
					lab.git(lab.Repo, "worktree", "lock", path)
					lab.Kill()
				}
			})
			lab.Run()
			row, ok := started.Load().(engine.Event)
			if !ok {
				t.Fatalf("the run was not stopped in the reflection's turn; calls=%v", lab.Labels())
			}
			path, _ := pinnedDirOf(t, lab, row.CallID)
			lab.git(lab.Repo, "worktree", "unlock", path)
			if !exists(path) {
				t.Fatalf("the directory of the reflection in flight %s is not there after the process died", path)
			}
			before := lab.LastSeq()
			lab.NewProcess("proc-2")
			var result *engine.Result
			if edited {
				result = lab.execEdited(func(text string) string {
					return strings.Replace(text, "reflectionPacket(ms, current, closed)", "reflectionPacket(ms, current, closed) + '\\n\\nEDITED REFLECTION'", 1)
				}, true)
			} else {
				result = lab.Resume()
			}
			if got := Value[laneResult](t, result); got.State != "closed" {
				t.Fatalf("resume: %+v; calls=%v", got, lab.Labels())
			}
			named := false
			for _, event := range rowsAfter(lab, before) {
				if event.CallID == row.CallID && event.Kind == engine.KindCall && event.Event == engine.EvSuperseded {
					named = str(event.Data, "own_directory") == path
				}
			}
			if edited && !named {
				t.Errorf("the superseded row of the reflection in flight does not name its directory %s", path)
			}
			if exists(path) {
				t.Errorf("the directory %s of the reflection that was in flight is still there after the run closed", path)
			}
			for _, event := range rowsAfter(lab, before) {
				if event.CallID == row.CallID && event.Event == engine.EvPrepared {
					allocation, _ := event.Data["allocation"].(map[string]any)
					if again := str(allocation, "path"); exists(again) {
						t.Errorf("the directory %s of the reflection that ran again is still there", again)
					}
				}
			}
		})
	}

	t.Run("a removal that fails is recorded and retried by every resume", func(t *testing.T) {
		lab := NewLab(t, twoNodes()...)
		var locked atomic.Value
		lab.OnRow(func(event engine.Event) {
			if event.Op == "agent" && event.Event == engine.EvCompleted && strings.HasPrefix(event.Label, "reflection:") && locked.Load() == nil {
				locked.Store(event)
				path := ""
				for _, prior := range lab.Events() {
					if prior.CallID == event.CallID && prior.Event == engine.EvPrepared {
						allocation, _ := prior.Data["allocation"].(map[string]any)
						path = str(allocation, "path")
					}
				}
				// a locked worktree is not removed by `git worktree remove --force`
				lab.git(lab.Repo, "worktree", "lock", path)
			}
		})
		mustRun(t, lab)
		row, ok := locked.Load().(engine.Event)
		if !ok {
			t.Fatalf("no reflection; calls=%v", lab.Labels())
		}
		path, _ := pinnedDirOf(t, lab, row.CallID)
		kept := func() int {
			n := 0
			for _, event := range lab.FlowRows(engine.EvOwnDirectoryKept) {
				if str(event.Data, "path") == path && str(event.Data, "error") != "" {
					n++
				}
			}
			return n
		}
		if !exists(path) || kept() != 1 {
			t.Fatalf("the failed removal of %s: exists=%v, own_directory_kept rows=%d", path, exists(path), kept())
		}
		// a resume while the directory still cannot be removed records it again and goes on
		before := lab.LastSeq()
		lab.NewProcess("proc-2")
		if got := Value[laneResult](t, lab.Resume()); got.State != "closed" {
			t.Fatalf("second process: %+v", got)
		}
		if !exists(path) || kept() != 2 {
			t.Fatalf("the second failed removal of %s: exists=%v, own_directory_kept rows=%d", path, exists(path), kept())
		}
		lab.git(lab.Repo, "worktree", "unlock", path)
		lab.NewProcess("proc-3")
		if got := Value[laneResult](t, lab.Resume()); got.State != "closed" {
			t.Fatalf("third process: %+v", got)
		}
		if exists(path) {
			t.Errorf("the resume did not remove the pinned worktree %s of the completed %s", path, row.Label)
		}
		if live := lab.LiveCalls(before); len(live) != 0 {
			t.Errorf("unchanged resumes made live calls: %+v", live)
		}
	})
}

// Every release of a node in the shipped workflows goes through the flow-side plan comparison of invariant 1: each call of crw.release passes the
// pinned option (the constant PINNED of crw:lib/lane, or the literal), so a new release path cannot skip it silently. A release named
// any other way (crw['release'], a release taken off crw) is refused as well.
func TestEveryWorkflowReleasePassesPinned(t *testing.T) {
	sites := 0
	for root, fsys := range map[string]fs.FS{"workflows/": workflows.FS, "workflows/lib/": lib.FS} {
		if err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(name, ".js") {
				return err
			}
			raw, err := fs.ReadFile(fsys, name)
			if err != nil {
				return err
			}
			releaseSites(t, root+name, string(raw), &sites)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if sites == 0 {
		t.Fatal("no crw.release call site found in the workflows")
	}
}

// releaseSites checks the release call sites of one workflow source.
func releaseSites(t *testing.T, path, text string, sites *int) {
	t.Helper()
	call := regexp.MustCompile(`crw\.release\(`)
	other := regexp.MustCompile(`\[\s*['"]release['"]\s*\]|\brelease\s*=\s*crw\.release\b|\{[^}]*\brelease\b[^}]*\}\s*=\s*crw\b`)
	pinnedConst := regexp.MustCompile(`const PINNED = \{ pinned: true \}`)
	pinnedLiteral := regexp.MustCompile(`^\{\s*pinned:\s*true\s*\}$`)
	if loc := other.FindStringIndex(text); loc != nil {
		t.Errorf("%s releases a node by another name than crw.release(...): %q", path, text[loc[0]:loc[1]])
	}
	for _, loc := range call.FindAllStringIndex(text, -1) {
		*sites++
		// the argument list, up to the parenthesis that closes the call
		depth, end := 0, -1
		for i := loc[1] - 1; i < len(text) && end < 0; i++ {
			switch text[i] {
			case '(', '{', '[':
				depth++
			case ')', '}', ']':
				depth--
				if depth == 0 {
					end = i
				}
			}
		}
		if end < 0 {
			t.Fatalf("%s: unbalanced crw.release call", path)
		}
		argList := text[loc[1]:end]
		parts := strings.Split(argList, ",")
		switch {
		case len(parts) == 4 && strings.TrimSpace(parts[3]) == "PINNED" && pinnedConst.MatchString(text):
		case len(parts) == 4 && pinnedLiteral.MatchString(strings.TrimSpace(parts[3])):
		default:
			t.Errorf("%s: crw.release(%s) does not pass {pinned: true}", path, argList)
		}
	}
}
