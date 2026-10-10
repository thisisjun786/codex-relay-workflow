package harness

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	_ "modernc.org/sqlite"
)

// The Stop leg through the harness: payload -> decision -> state effect, as the host runs it
// (crw hook stop --leg stop-checking-pabcd-continuation). The handler reads the process environment, so every
// case points CODEX_SQLITE_HOME and CRW_BIN at temporary values; nothing reads the real ~/.codex or ~/.crw.

const stopLeg = "stop-checking-pabcd-continuation"

// stopSetup builds a workspace whose session s1 is in flight at B, with a goals database holding status for it
// ("" builds none).
func stopSetup(t *testing.T, status string) (cwd string, env map[string]string) {
	t.Helper()
	root := t.TempDir()
	cwd = filepath.Join(root, "ws")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(root, "codex")
	if err := os.MkdirAll(codex, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SQLITE_HOME", codex)
	t.Setenv("CRW_BIN", "CRW")
	t.Setenv("CRW_PABCD", "")
	if status != "" {
		db, err := sql.Open("sqlite", filepath.Join(codex, host.GoalsDBFilename))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, q := range []string{"CREATE TABLE thread_goals (thread_id TEXT PRIMARY KEY NOT NULL, status TEXT NOT NULL, objective TEXT)",
			"INSERT INTO thread_goals (thread_id, status, objective) VALUES ('s1', '" + status + "', 'x')"} {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	}
	s := state.DefaultState("s1", "")
	s.Phase, s.OrchestrationActive = state.PhaseB, true
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	return cwd, map[string]string{"CODEX_HOME": filepath.Join(root, "codex-home")}
}

func stopHook(legs []Leg, stdin string, env map[string]string) (int, string, string) {
	return hook(legs, []string{"stop", "--leg", stopLeg}, stdin, env)
}

func TestStopLegBlocksAnActiveGoalAndSpendsTheBudgetOnce(t *testing.T) {
	cwd, env := stopSetup(t, "active")
	in := payload("Stop", cwd, `,"turn_id":"t1","stop_hook_active":false,"last_assistant_message":"Done.","transcript_path":null`)
	code, out, errOut := stopHook(Legs(), in, env)
	if code != 0 || errOut != "" || !strings.HasPrefix(out, `{"decision":"block","reason":"[crw — continue PABCD] You are mid-cycle at B (BUILD) with an active goal.`) || !strings.HasSuffix(out, "\"}\n") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if !strings.Contains(out, "`CRW pabcd orchestrate C --session s1 --attest ") {
		t.Errorf("the command is resolved at emission with the session: %q", out)
	}
	if s := state.ReadState(cwd, "s1"); s.StopBlockCount != 1 || s.StopBlockTotal != 1 || s.Phase != state.PhaseB {
		t.Errorf("state after one Stop: %+v", s)
	}
}

// A goal the user paused or the host limited, a session without a goal, a subagent's turn, a PABCD-off project and
// a payload that is not a Stop all leave the turn alone and the state as it was.
func TestStopLegReleasesAndWritesNothing(t *testing.T) {
	sub := func(cwd string) string { return payload("Stop", cwd, `,"agent_id":"a1","agent_type":"worker"`) }
	cases := []struct {
		name, status string
		in           func(cwd string) string
		off          bool
	}{
		{"paused", "paused", func(cwd string) string { return payload("Stop", cwd, "") }, false},
		{"budget limited", "budget_limited", func(cwd string) string { return payload("Stop", cwd, "") }, false},
		{"no goal", "", func(cwd string) string { return payload("Stop", cwd, "") }, false},
		{"subagent turn", "active", sub, false},
		{"another event", "active", func(cwd string) string { return payload("UserPromptSubmit", cwd, "") }, false},
		{"not an object", "active", func(string) string { return "[]" }, false},
		{"empty input", "active", func(string) string { return "" }, false},
		{"pabcd off", "active", func(cwd string) string { return payload("Stop", cwd, "") }, true},
	}
	for _, c := range cases {
		cwd, env := stopSetup(t, c.status)
		if c.off {
			if err := os.WriteFile(filepath.Join(cwd, "crw.json"), []byte(`{"pabcd":{"enabled":false}}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		before, err := os.ReadFile(state.StatePath(cwd, "s1"))
		if err != nil {
			t.Fatal(err)
		}
		if code, out, errOut := stopHook(Legs(), c.in(cwd), env); code != 0 || out != "" || errOut != "" {
			t.Errorf("%s: %d %q %q", c.name, code, out, errOut)
		}
		if after, _ := os.ReadFile(state.StatePath(cwd, "s1")); string(after) != string(before) {
			t.Errorf("%s: the state changed", c.name)
		}
	}
}

// failingWriter is a stdout that cannot be written, as a closed pipe is.
type failingWriter struct{ calls int }

func (w *failingWriter) Write([]byte) (int, error) { w.calls++; return 0, errors.New("broken pipe") }

// A stdout that fails, and a handler that panics, are not a block: exit status 2 with stderr is the host's block
// signal for a Stop hook, and neither of these may produce it (exit 0, or 1 with the oracle's message).
func TestStopLegFailuresAreNotABlock(t *testing.T) {
	cwd, env := stopSetup(t, "active")
	in := payload("Stop", cwd, "")
	w := &failingWriter{}
	var errOut strings.Builder
	code := Hook(context.Background(), []string{"stop", "--leg", stopLeg}, strings.NewReader(in), w, &errOut, lookup(env), Legs())
	if code == 2 || code != 0 || errOut.String() != "" || w.calls == 0 {
		t.Errorf("a failing stdout: code %d, stderr %q, writes %d", code, errOut.String(), w.calls)
	}

	legs := only(stopLeg, func(Call) string { panic("boom") })
	if code, out, errOut := hook(legs, []string{"stop", "--leg", stopLeg}, in, env); code != 0 || out != "" || errOut != "" {
		t.Errorf("a panic in the handler: %d %q %q", code, out, errOut)
	}

	// an oversized input is answered before the handler, as cli.ts:101-117 does for Stop (a fail-closed block
	// that names the limit, exit 0), and the handler never runs, so the state is not touched
	big := strings.Repeat("x", MaxStdinBytes+1)
	before, _ := os.ReadFile(state.StatePath(cwd, "s1"))
	if code, out, _ := stopHook(Legs(), big, env); code != 0 || out != block {
		t.Errorf("an oversized input: %d %q", code, out)
	}
	if after, _ := os.ReadFile(state.StatePath(cwd, "s1")); string(after) != string(before) {
		t.Errorf("an oversized input reached the handler")
	}
}

// The same event run again and again ends: three blocks per phase, and the turn's 24 at most, whatever the host does.
func TestStopLegRepeatedEventIsBounded(t *testing.T) {
	cwd, env := stopSetup(t, "active")
	in := payload("Stop", cwd, "")
	blocks := 0
	for i := 0; i < 200; i++ {
		if _, out, _ := stopHook(Legs(), in, env); strings.Contains(out, `"decision":"block"`) {
			blocks++
		}
	}
	if blocks > 24 || blocks < 3 {
		t.Errorf("200 runs of one event blocked %d times", blocks)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseB || !s.OrchestrationActive || !s.StopBlockCapNotified {
		t.Errorf("state after the loop: phase %s notified %v", s.Phase, s.StopBlockCapNotified)
	}
}

// A Stop of an earlier user turn (its turn_id differs from the stamp the next prompt wrote) releases and neither
// blocks nor spends the new turn's budget; the stamped turn's own Stop keeps the loop.
func TestStopLegReleasesAnEventOfAnEarlierTurn(t *testing.T) {
	cwd, env := stopSetup(t, "active")
	s := state.ReadState(cwd, "s1")
	t1 := "t1"
	s.StopBlockTurnID = &t1
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(state.StatePath(cwd, "s1"))
	if code, out, errOut := stopHook(Legs(), payload("Stop", cwd, `,"turn_id":"t0"`), env); code != 0 || out != "" || errOut != "" {
		t.Fatalf("a stale turn: %d %q %q", code, out, errOut)
	}
	if after, _ := os.ReadFile(state.StatePath(cwd, "s1")); string(after) != string(before) {
		t.Errorf("a stale turn changed the state")
	}
	if _, out, _ := stopHook(Legs(), payload("Stop", cwd, `,"turn_id":"t1"`), env); !strings.Contains(out, `"decision":"block"`) {
		t.Errorf("the stamped turn: %q", out)
	}
}
