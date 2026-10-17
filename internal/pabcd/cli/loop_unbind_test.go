package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-146: `crw pabcd loop unbind --session <id>` releases the goalplan slug a session is bound to. It is a CRW addition (the oracle
// has no such verb). It refuses while a PABCD cycle is in flight, so the supported recovery of a cycle stranded at B is
// `orchestrate reset` (which closes the cycle and keeps the slug) and then `loop unbind` (which releases the slug).

const loopUnbindSession = "unbind-s1"

type loopUnbindFiles struct{ state, plan, planLedger, ledger []byte }

func loopUnbindRead(t *testing.T, cwd, slug string) loopUnbindFiles {
	t.Helper()
	read := func(parts ...string) []byte {
		b, err := os.ReadFile(filepath.Join(append([]string{cwd, ".crw"}, parts...)...))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return b
	}
	return loopUnbindFiles{
		state:      read("sessions", loopUnbindSession+".json"),
		plan:       read("goalplans", slug, "goalplan.json"),
		planLedger: read("goalplans", slug, "ledger.jsonl"),
		ledger:     read("ledger.jsonl"),
	}
}

// loopUnbindBound is a workspace with a session bound to a fresh goalplan, then moved to the stranded X2 state: phase B, slug bound, no
// source delta since B began.
func loopUnbindBound(t *testing.T) (cwd, slug string) {
	t.Helper()
	cwd = loopReadWorkspace(t)
	gitInit(t, cwd)
	loopSession(t, cwd, loopUnbindSession)
	if got := loopRun(t, cwd, "init", "--objective", "Unbind probe objective", "--session", loopUnbindSession, "--criterion", "the probe closes"); got.Code != 0 {
		t.Fatalf("init: %d %q", got.Code, got.Output)
	}
	slug = state.ReadState(cwd, loopUnbindSession).Slug
	if slug != "unbind-probe-objective" {
		t.Fatalf("bound slug = %q", slug)
	}
	s := state.ReadState(cwd, loopUnbindSession)
	pointer, phase := "wp-1", state.PhaseB
	s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseB, true, &phase
	s.StopBlockPhase, s.StopBlockWorkPhaseID, s.StopBlockCount = &phase, &pointer, 1
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	return cwd, slug
}

func loopUnbindComplete(t *testing.T, cwd string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "session_id": loopUnbindSession, "cwd": cwd,
		"tool_name": "update_goal", "tool_input": map[string]any{"status": "complete"}})
	if err != nil {
		t.Fatal(err)
	}
	return hook.GoalGateHandlePreToolUseFailClosed(string(raw), os.LookupEnv, true)
}

func loopUnbindRows(t *testing.T, ledger []byte) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(string(ledger), "\n"), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestLoopUnbindRecoversTheStrandedX2Session(t *testing.T) {
	cwd, slug := loopUnbindBound(t)

	// X2 as found: the gate refuses complete on the cycle in flight, and unbind refuses to run under it.
	if out := loopUnbindComplete(t, cwd); !strings.Contains(out, "a PABCD cycle is in flight at phase B") {
		t.Fatalf("the gate before: %q", out)
	}
	start := loopUnbindRead(t, cwd, slug)
	refused := loopRun(t, cwd, "unbind", "--session", loopUnbindSession)
	if refused.Code != 1 || !strings.Contains(refused.Output, "phase B") || !strings.Contains(refused.Output, "crw work state reset --session "+loopUnbindSession) {
		t.Fatalf("in flight: %d %q", refused.Code, refused.Output)
	}
	if after := loopUnbindRead(t, cwd, slug); !equalFiles(after, start) {
		t.Fatal("a refused unbind wrote")
	}

	// Step 1 of the recovery, unchanged: reset closes the cycle and keeps the slug.
	if got := orchestrateCommitRunOK(t, cwd, nil, "reset", "--session", loopUnbindSession); got.Code != 0 {
		t.Fatalf("reset: %d %q", got.Code, got.Output)
	}
	reset := state.ReadState(cwd, loopUnbindSession)
	if reset.Phase != state.PhaseIdle || reset.Slug != slug {
		t.Fatalf("reset must keep the binding: %+v", reset)
	}
	if out := loopUnbindComplete(t, cwd); !strings.Contains(out, "GOAL-COMPLETE-GATE-01") || !strings.Contains(out, slug) {
		t.Fatalf("after reset the slug branch still holds the session: %q", out)
	}

	// Step 2: unbind. Only the session's slug (and the pointer that depends on it) changes.
	before := loopUnbindRead(t, cwd, slug)
	got := loopRun(t, cwd, "unbind", "--session", loopUnbindSession)
	if got.Code != 0 || !strings.Contains(got.Output, slug) {
		t.Fatalf("unbind: %d %q", got.Code, got.Output)
	}
	after := loopUnbindRead(t, cwd, slug)
	if !bytes.Equal(before.plan, after.plan) || !bytes.Equal(before.planLedger, after.planLedger) || len(before.plan) == 0 || len(before.planLedger) == 0 {
		t.Fatal("the goalplan file or its ledger changed")
	}
	s := state.ReadState(cwd, loopUnbindSession)
	want := reset
	want.Slug, want.StopBlockWorkPhaseID = "", nil
	s.UpdatedAt, want.UpdatedAt = "", ""
	if gotJSON, wantJSON := loopUnbindEncode(t, s), loopUnbindEncode(t, want); gotJSON != wantJSON {
		t.Fatalf("state\n got %s\nwant %s", gotJSON, wantJSON)
	}
	if !bytes.HasPrefix(after.ledger, before.ledger) {
		t.Fatal("the transition ledger was not append-only")
	}
	rows := loopUnbindRows(t, after.ledger[len(before.ledger):])
	if len(rows) != 1 || rows[0]["reason"] != "unbound" || rows[0]["from"] != "IDLE" || rows[0]["to"] != "IDLE" || rows[0]["sessionId"] != loopUnbindSession ||
		!strings.Contains(rows[0]["evidence"].(string), slug) {
		t.Fatalf("the unbound row: %+v", rows)
	}

	// update_goal complete now takes the no-slug branch.
	if out := loopUnbindComplete(t, cwd); out != "" {
		t.Fatalf("the gate after: %q", out)
	}
	// The plan is still there to read by slug, and the session no longer resolves it.
	if shown := loopRun(t, cwd, "show", "--slug", slug); shown.Code != 0 {
		t.Fatalf("show --slug: %d %q", shown.Code, shown.Output)
	}
	if shown := loopRun(t, cwd, "show", "--session", loopUnbindSession); shown.Code != 1 {
		t.Fatalf("show --session after unbind: %d %q", shown.Code, shown.Output)
	}

	// A repeat is idempotent: it succeeds and writes nothing.
	again := loopRun(t, cwd, "unbind", "--session", loopUnbindSession)
	if again.Code != 0 || !strings.Contains(again.Output, "nothing to release") {
		t.Fatalf("repeat: %d %q", again.Code, again.Output)
	}
	if final := loopUnbindRead(t, cwd, slug); !equalFiles(final, after) {
		t.Fatal("a repeat wrote")
	}
}

func equalFiles(a, b loopUnbindFiles) bool {
	return bytes.Equal(a.state, b.state) && bytes.Equal(a.plan, b.plan) && bytes.Equal(a.planLedger, b.planLedger) && bytes.Equal(a.ledger, b.ledger)
}

func loopUnbindEncode(t *testing.T, s state.State) string {
	t.Helper()
	b, err := state.Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLoopUnbindRefusesEveryPhaseButIdle(t *testing.T) {
	cwd, slug := loopUnbindBound(t)
	for _, phase := range []state.Phase{state.PhaseI, state.PhaseP, state.PhaseA, state.PhaseB, state.PhaseC, state.PhaseD} {
		s := state.ReadState(cwd, loopUnbindSession)
		s.Phase = phase
		if err := state.WriteState(cwd, s); err != nil {
			t.Fatal(err)
		}
		before := loopUnbindRead(t, cwd, slug)
		got := loopRun(t, cwd, "unbind", "--session", loopUnbindSession)
		if got.Code != 1 || !strings.Contains(got.Output, "phase "+string(phase)) || !strings.Contains(got.Output, "Nothing was written") {
			t.Fatalf("%s: %d %q", phase, got.Code, got.Output)
		}
		if !equalFiles(before, loopUnbindRead(t, cwd, slug)) {
			t.Fatalf("%s: a refusal wrote", phase)
		}
	}
}

func TestLoopUnbindRefusesAPendingClose(t *testing.T) {
	cwd, slug := loopUnbindBound(t)
	s := state.ReadState(cwd, loopUnbindSession)
	epoch := "c-1"
	s.Phase, s.OrchestrationActive, s.LastInjectedPhase, s.CheckEpoch = state.PhaseIdle, false, nil, &epoch
	next := "wp-2"
	s.DcloseRecovery = &state.DcloseRecoveryMarker{SessionID: loopUnbindSession, CheckEpoch: epoch, ClosedWorkPhaseID: "wp-1", NextWorkPhaseID: &next}
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	before := loopUnbindRead(t, cwd, slug)
	got := loopRun(t, cwd, "unbind", "--session", loopUnbindSession)
	if got.Code != 1 || !strings.Contains(got.Output, "reset") || !equalFiles(before, loopUnbindRead(t, cwd, slug)) {
		t.Fatalf("%d %q", got.Code, got.Output)
	}
}

func TestLoopUnbindNeedsACanonicalSession(t *testing.T) {
	cwd := loopReadWorkspace(t)
	args, err := ParseLoopCliArgs([]string{"unbind"}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := RunLoopCli(args); err != nil || got.Code != 1 || got.Output != "loop unbind: --session <id> is required" {
		t.Fatalf("missing: %+v %v", got, err)
	}
	if got := loopRun(t, cwd, "unbind", "--session", "a/b"); got.Code != 1 || got.Output != "loop unbind: session id is not canonical" {
		t.Fatalf("alias: %+v", got)
	}
	if got := loopRun(t, cwd, "unbind", "--session", "   "); got.Code != 1 {
		t.Fatalf("blank: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".crw")); !os.IsNotExist(err) {
		t.Fatalf("a refused unbind wrote state: %v", err)
	}
}

func TestLoopUnbindOfASessionWithoutStateWritesNothing(t *testing.T) {
	cwd := loopReadWorkspace(t)
	got := loopRun(t, cwd, "unbind", "--session", "never-seen")
	if got.Code != 0 || !strings.Contains(got.Output, "nothing to release") {
		t.Fatalf("%d %q", got.Code, got.Output)
	}
	if _, err := os.Stat(state.StatePath(cwd, "never-seen")); !os.IsNotExist(err) {
		t.Fatalf("a state file appeared: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".crw", "ledger.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("a ledger appeared: %v", err)
	}
}

func TestLoopUnbindRefusesAnUnreadableSessionState(t *testing.T) {
	cwd := loopReadWorkspace(t)
	path := state.StatePath(cwd, loopUnbindSession)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := loopRun(t, cwd, "unbind", "--session", loopUnbindSession)
	if got.Code != 1 || !strings.Contains(got.Output, "unreadable") {
		t.Fatalf("%d %q", got.Code, got.Output)
	}
	if b, _ := os.ReadFile(path); string(b) != "{not json" {
		t.Fatalf("the damaged state was replaced: %q", b)
	}
}

func TestLoopUnbindFlags(t *testing.T) {
	if _, err := ParseLoopCliArgs([]string{"unbind", "--session", "s", "--cwd", "/x"}, "/y"); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--slug", "--objective", "--id"} {
		if _, err := ParseLoopCliArgs([]string{"unbind", "--session", "s", flag, "v"}, "/y"); err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("%s: %v", flag, err)
		}
	}
	parsed, _ := ParseLoopCliArgs([]string{"unbind", "--session", " s "}, "/y")
	if LoopMutationSession(parsed) != "s" {
		t.Errorf("a native session guard needs the unbind session, got %q", LoopMutationSession(parsed))
	}
}
