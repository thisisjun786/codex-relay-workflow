package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// TestLegacyIssueChildRunIsNotResumed (CRW-1415): a run whose journal holds a crw.releaseChild row started an issue child under the
// old project lane, which the current lane does not replay. `crw flow resume RUN --detach` refuses it before a process is started, the
// run process refuses it before it writes a row, the refusal names the legacy issue-child path, and `crw flow show` still reads the run.
func TestLegacyIssueChildRunIsNotResumed(t *testing.T) {
	pw := newProcessWorld(t, twoCalls)
	run := "run-legacy-child"
	pw.handle = func(engine.FakeRequest) engine.FakeReply {
		return engine.FakeReply{Tokens: &engine.Tokens{Input: 10, Output: 5, Total: 15}}
	}
	spec := pw.spec(run, nil)
	if code, errOut := pw.execute(spec, false, make(chan os.Signal, 1)); code != 0 {
		t.Fatalf("the first process: exit %d %s", code, errOut)
	}
	// the child start of an old project lane, as an old run's journal holds it
	j, err := openJournal(pw.world, run)
	if err != nil {
		t.Fatal(err)
	}
	j.Append(engine.Event{RunID: run, Kind: engine.KindCall, CallID: "c_child", Path: "root", Position: 9, Op: engine.OpLegacyReleaseChild, Label: engine.OpLegacyReleaseChild,
		Event: engine.EvCompleted, Data: map[string]any{"node": "A", "child": "child-A", "started": true}})
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	rowsOf := func() int {
		events, _, err := loadRun(pw.world, run)
		if err != nil {
			t.Fatal(err)
		}
		return len(events)
	}
	rows := rowsOf()

	before := len(pw.td.spawned)
	code, out, errOut := pw.td.do("resume", run, "--detach", "--state", pw.state)
	if code == 0 || !strings.Contains(errOut, "legacy issue-child run") || !strings.Contains(errOut, engine.OpLegacyReleaseChild) || strings.Contains(out, "resumed") {
		t.Errorf("a detached resume of a legacy issue-child run: exit %d %q %q", code, out, errOut)
	}
	if n := len(pw.td.spawned) - before; n != 0 {
		t.Errorf("a detached resume of a legacy issue-child run started %d processes", n)
	}

	code, errOut = pw.execute(spec, true, make(chan os.Signal, 1))
	if code == 0 || !strings.Contains(errOut, "legacy issue-child run") {
		t.Errorf("the run process resumed a legacy issue-child run: exit %d %q", code, errOut)
	}
	if got := rowsOf(); got != rows {
		t.Errorf("the refused resumes wrote rows: %d -> %d", rows, got)
	}

	if code, out, errOut := pw.td.do("show", run, "--state", pw.state); code != 0 || !strings.Contains(out, run) {
		t.Errorf("crw flow show of a legacy issue-child run: exit %d %q %q", code, out, errOut)
	}
}
