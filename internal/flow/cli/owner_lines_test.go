package cli

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/crwcmd"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// CRW-1572: every owner line (a step's then, next's hint, start's next and loop, dispatched's details) starts with the crw that
// runs, so the owner runs it as printed whether or not crw is on its PATH.

// testCrw is the word TestMain names as the running crw (crwcmd.Override): a fixed path with a space, so every then and hint a test
// reads also proves the quoting, and no test reads the test binary's own path.
const testCrw = "'/opt/crw test/bin/crw'"

// bare is s with the running crw's word of the tests spelled as the bare crw, for the tests that compare an owner line's text.
func bare(s string) string { return strings.ReplaceAll(s, testCrw+" flow", "crw flow") }

// ownerCommand is a command of the run's owner lines.
var ownerCommand = regexp.MustCompile(`flow (?:next|dispatched|resume|show|answer)\b`)

// ownerLineNames says where s names an owner command not preceded by word, or names none.
func ownerLineNames(t *testing.T, where, s, word string) {
	t.Helper()
	found := ownerCommand.FindAllStringIndex(s, -1)
	if len(found) == 0 {
		t.Errorf("%s names no crw flow command: %q", where, s)
	}
	for _, at := range found {
		if !strings.HasSuffix(s[:at[0]], word+" ") {
			t.Errorf("%s: %q at %d is not preceded by %s: %q", where, s[at[0]:at[1]], at[0], word, s)
		}
	}
	if regexp.MustCompile(`(^|[^/])crw flow (?:next|dispatched|resume|show|answer)\b`).MatchString(s) {
		t.Errorf("%s names a bare crw command: %q", where, s)
	}
}

// TestOwnerLinesNameTheRunningCrw (CRW-1572): with the running crw TestMain names, every owner line of next (the thens of a spawn and a
// send_input step, every hint), of start (next and loop) and of dispatched (its details) starts each crw flow command with that word.
func TestOwnerLinesNameTheRunningCrw(t *testing.T) {
	isolateGit(t)
	w := newWorld(t)
	td := w.deps()
	run := "run-0123456789c2"
	sessionSpec(t, w, run)
	lock, err := TakeLock(w.state, run, "_run", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	bus := engine.NewFileStepBus(RunDir(w.state, run))
	for _, st := range []engine.Step{
		{Run: run, Call: "c_000000000001", K: 1, Attempt: 1, Op: "c_000000000001/t1-a1", Role: "explorer", Action: engine.StepSpawn, AgentType: "explorer", Message: "crw-flow-op: c_000000000001/t1-a1\n\nlook"},
		{Run: run, Call: "c_000000000002", K: 2, Attempt: 1, Op: "c_000000000002/t2-a1", Role: "executor", Action: engine.StepSendInput, AgentType: "executor", AgentID: "agent-7", Message: "crw-flow-op: c_000000000002/t2-a1\n\nrepair"},
	} {
		if err := bus.Publish(st); err != nil {
			t.Fatal(err)
		}
	}
	code, out, errOut := td.do("next", run, "--json", "--state", w.state)
	if code != 0 {
		t.Fatalf("next: %d %s %s", code, out, errOut)
	}
	ans := decodeNext(t, out)
	if len(ans.Steps) != 2 {
		t.Fatalf("steps %+v", ans.Steps)
	}
	for _, st := range ans.Steps {
		ownerLineNames(t, "the then of the "+st.Action+" step", st.Then, testCrw)
		if !strings.HasPrefix(st.Then, testCrw+" flow dispatched "+run+" "+st.Call+" ") {
			t.Errorf("the then of the %s step: %q", st.Action, st.Then)
		}
	}
	ownerLineNames(t, "next's hint", ans.Hint, testCrw)
	// --wait 0 with the process holding the run: the run has not answered
	code, out, _ = td.do("dispatched", run, "c_000000000001", "--agent", "agent-1", "--k", "1", "--attempt", "1", "--wait", "0", "--json", "--state", w.state)
	var reply map[string]any
	if err := json.Unmarshal([]byte(out), &reply); err != nil || code != 0 || reply["status"] != "queued" {
		t.Fatalf("dispatched with a process: %d %s", code, out)
	}
	ownerLineNames(t, "dispatched's detail with a process", reply["detail"].(string), testCrw)
	lock.Release()
	code, out, _ = td.do("dispatched", run, "c_000000000002", "--agent", "agent-7", "--k", "2", "--attempt", "1", "--wait", "0", "--json", "--state", w.state)
	reply = nil
	if err := json.Unmarshal([]byte(out), &reply); err != nil || code != 0 || reply["status"] != "queued" || !strings.Contains(reply["detail"].(string), "no process holds") {
		t.Fatalf("dispatched without a process: %d %s", code, out)
	}
	ownerLineNames(t, "dispatched's detail without a process", reply["detail"].(string), testCrw)

	// every branch of the hint
	steps := []NextStep{{Step: engine.Step{Action: engine.StepSpawn}}}
	for name, c := range map[string]struct {
		ans  NextAnswer
		info RunInfo
	}{
		"resumed":            {NextAnswer{Run: "run-h", State: NextWaitingSession, Resumed: 42, Steps: steps}, RunInfo{State: StateRunning}},
		"ended":              {NextAnswer{Run: "run-h", State: NextEnded}, RunInfo{State: "completed", Status: "completed"}},
		"paused for answer":  {NextAnswer{Run: "run-h", State: NextPaused}, RunInfo{State: StatePaused, Paused: "answer: crw flow answer: a decision waits to be applied"}},
		"paused":             {NextAnswer{Run: "run-h", State: NextPaused}, RunInfo{State: StatePaused, Paused: "operator: crw flow pause"}},
		"waiting decision":   {NextAnswer{Run: "run-h", State: NextWaitingDecision}, RunInfo{State: StateRunning}},
		"starting":           {NextAnswer{Run: "run-h", State: NextStarting}, RunInfo{State: StateRunning}},
		"steps":              {NextAnswer{Run: "run-h", State: NextWaitingSession, Steps: steps}, RunInfo{State: StateRunning}},
		"no step":            {NextAnswer{Run: "run-h", State: NextWaitingSession}, RunInfo{State: StateRunning}},
		"no step and agents": {NextAnswer{Run: "run-h", State: NextWaitingSession, Slots: NextSlots{Limit: 6, Open: 1, Agents: []string{"agent-9"}}}, RunInfo{State: StateRunning}},
	} {
		hint := nextHint(c.ans, c.info, nil)
		// a pause label names the operator's command as it was given, which is not an owner line
		hint = strings.ReplaceAll(hint, "(operator: crw flow pause)", "(operator)")
		ownerLineNames(t, "the hint ("+name+")", hint, testCrw)
	}
	ownerLineNames(t, "the session loop", sessionLoop(run), testCrw)

	// start: its next and the loop of its plan
	script := helloScript(t)
	code, out, errOut = td.do("start", script, "--thread", sessOwner, "--state", w.state, "--json")
	if code != 0 || len(td.spawned) != 1 {
		t.Fatalf("start: %d %s %s", code, out, errOut)
	}
	var started StartAnswer
	if err := json.Unmarshal([]byte(answerOf(out)), &started); err != nil {
		t.Fatal(err)
	}
	if started.Next != testCrw+" flow next "+started.Run+" --wait 600 --json" {
		t.Errorf("start's next: %q", started.Next)
	}
	ownerLineNames(t, "start's loop", started.Plan.Loop, testCrw)
	code, out, errOut = td.do("start", script, "--thread", "01a30000-0000-7000-8000-00000000000b", "--state", w.state)
	if code != 0 {
		t.Fatalf("start (text): %d %s %s", code, out, errOut)
	}
	for _, line := range strings.Split(answerOf(out), "\n") {
		if strings.Contains(line, "its steps:") || strings.Contains(line, "take the steps of") {
			ownerLineNames(t, "start's text", line, testCrw)
		}
	}
}

// TestNextWarnsWhenNoCrwPathIsKnown (CRW-1572): with no CRW_BIN, no path of the running binary and no runtime pointer, the owner lines
// fall back to the bare crw and next says why among its warnings.
func TestNextWarnsWhenNoCrwPathIsKnown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(host.BinEnv, "")
	defer crwcmd.Override("")()
	defer crwcmd.UseExecutable("")()
	defer crwcmd.UseRuntimePointer("", errors.New("no absolute home directory holds the crw runtime pointer: set CRW_BIN"))()
	w := newWorld(t)
	td := w.deps()
	run := "run-0123456789c3"
	sessionSpec(t, w, run)
	lock, err := TakeLock(w.state, run, "_run", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	bus := engine.NewFileStepBus(RunDir(w.state, run))
	if err := bus.Publish(engine.Step{Run: run, Call: "c_000000000001", K: 1, Attempt: 1, Op: "c_000000000001/t1-a1", Role: "explorer", Action: engine.StepSpawn, AgentType: "explorer", Message: "crw-flow-op: c_000000000001/t1-a1\n\nlook"}); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := td.do("next", run, "--json", "--state", w.state)
	if code != 0 {
		t.Fatalf("next: %d %s %s", code, out, errOut)
	}
	ans := decodeNext(t, out)
	if len(ans.Steps) != 1 || !strings.HasPrefix(ans.Steps[0].Then, "crw flow dispatched "+run+" ") {
		t.Fatalf("the then without a known crw: %+v", ans.Steps)
	}
	warning := crwcmd.Warning()
	if warning == "" {
		t.Fatal("crwcmd gives no warning without a known crw")
	}
	found := false
	for _, w := range ans.Warnings {
		found = found || w == warning
	}
	if !found || !strings.Contains(errOut, warning) {
		t.Fatalf("next does not warn that no crw path is known: %v / %s", ans.Warnings, errOut)
	}
	// with the word TestMain names, next warns of nothing of the kind
	restore := crwcmd.Override(testCrw)
	code, out, _ = td.do("next", run, "--json", "--state", w.state)
	restore()
	if ans := decodeNext(t, out); code != 0 || strings.Contains(strings.Join(ans.Warnings, "\n"), warning) {
		t.Fatalf("a warning with a known crw: %d %v", code, ans.Warnings)
	}
}
