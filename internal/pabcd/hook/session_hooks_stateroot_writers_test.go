package hook

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/stateroot"
)

// CRW-1140 round 3 (d1): the SessionStart refusal protects only the bootstrap. A prompt of the same
// thread at the other cwd must not write a state there either (the memory marker, the Stop-budget
// stamp and the trigger bookkeeping all create the file), and tells the agent where its state is.
func TestUserPromptSubmitAwayFromTheAnchoredRootCreatesNoState(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "pabcd on"
		if !enabled {
			name = "pabcd off"
		}
		t.Run(name, func(t *testing.T) {
			a, b := t.TempDir(), t.TempDir()
			env := rootEnv(filepath.Join(t.TempDir(), ".crw"))
			before := rootInFlight(t, a)
			if err := stateroot.Guard(env, a, a, rootSession); err != nil {
				t.Fatal(err)
			}
			if answer := sessionHookSessionStart(SessionHookSessionStartPayload{Cwd: b, SessionID: rootSession}, env); answer == "" {
				t.Fatal("SessionStart did not refuse")
			}
			answer := promptSubmitHandle(PromptSubmitPayload{Cwd: b, SessionID: rootSession, Prompt: "remember this: the build needs go1.27", TurnID: "turn-1", PabcdEnabled: enabled}, "linux", env, state.WithSessionLock)
			if _, err := os.Stat(state.StatePath(b, rootSession)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the prompt created a state at the other cwd: %v", err)
			}
			if _, err := os.Stat(filepath.Join(b, ".crw")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the prompt created %s: %v", filepath.Join(b, ".crw"), err)
			}
			if after, err := os.ReadFile(state.StatePath(a, rootSession)); err != nil || string(after) != string(before) {
				t.Fatalf("the native state changed (%v)", err)
			}
			for _, want := range []string{state.StatePath(a, rootSession), "--cwd " + a} {
				if !strings.Contains(answer, want) {
					t.Errorf("the prompt answer lacks %q:\n%s", want, answer)
				}
			}
		})
	}
}

// The control: a prompt at the root itself, and a prompt of a thread with no anchor, go on as before.
func TestUserPromptSubmitAtTheRootOrWithoutAnAnchorWritesAsBefore(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	env := rootEnv(filepath.Join(t.TempDir(), ".crw"))
	rootInFlight(t, a)
	if err := stateroot.Guard(env, a, a, rootSession); err != nil {
		t.Fatal(err)
	}
	promptSubmitHandle(PromptSubmitPayload{Cwd: a, SessionID: rootSession, Prompt: "remember this: x", TurnID: "turn-1", PabcdEnabled: true}, "linux", env, state.WithSessionLock)
	if s, bad := state.ReadStateStrict(a, rootSession); bad || !s.MemoryWriteRequested {
		t.Fatalf("no marker at the root: %+v %v", s, bad)
	}
	other := rootEnv(filepath.Join(t.TempDir(), ".crw"))
	promptSubmitHandle(PromptSubmitPayload{Cwd: b, SessionID: rootSession, Prompt: "remember this: x", TurnID: "turn-1", PabcdEnabled: true}, "linux", other, state.WithSessionLock)
	if s, bad := state.ReadStateStrict(b, rootSession); bad || !s.MemoryWriteRequested {
		t.Fatalf("no marker for a standalone thread: %+v %v", s, bad)
	}
}

// d3: an anchor that cannot be read is not an absent anchor: SessionStart creates no state and says
// why.
func TestSessionStartWithAnUntrustworthyAnchorBootstrapsNothing(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	env := rootEnv(filepath.Join(t.TempDir(), ".crw"))
	rootInFlight(t, a)
	if err := stateroot.Guard(env, a, a, rootSession); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateroot.AnchorPath(env, rootSession), []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	answer := sessionHookSessionStart(SessionHookSessionStartPayload{Cwd: b, SessionID: rootSession}, env)
	if _, err := os.Stat(state.StatePath(b, rootSession)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("SessionStart created a state at the other cwd: %v", err)
	}
	var out struct {
		HookSpecificOutput struct{ HookEventName, AdditionalContext string }
	}
	if err := json.Unmarshal([]byte(answer), &out); err != nil || out.HookSpecificOutput.HookEventName != "SessionStart" ||
		!strings.Contains(out.HookSpecificOutput.AdditionalContext, stateroot.AnchorPath(env, rootSession)) {
		t.Fatalf("answer %q (%v)", answer, err)
	}
}

// d4: a SessionStart whose anchor cannot follow the thread to the new cwd creates no state there
// (a state the anchor does not track would be a detached FSM), and says why.
func TestSessionStartThatCannotMoveTheAnchorBootstrapsNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	a, b := t.TempDir(), t.TempDir()
	env := rootEnv(filepath.Join(t.TempDir(), ".crw"))
	if err := stateroot.Guard(env, a, a, rootSession); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(stateroot.AnchorPath(env, rootSession))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	answer := sessionHookSessionStart(SessionHookSessionStartPayload{Cwd: b, SessionID: rootSession}, env)
	if _, err := os.Stat(state.StatePath(b, rootSession)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("SessionStart created a state the anchor does not track: %v", err)
	}
	if !strings.Contains(answer, stateroot.AnchorPath(env, rootSession)) {
		t.Fatalf("answer %q", answer)
	}
}

// Round 4 (regression of the d4 fix): the SessionStart refusal of an anchor that cannot follow the
// thread is not undone by the next prompt. The prompt's writers (the memory marker with PABCD off,
// every writer with it on) would create the state the bootstrap refused, at a cwd the anchor does
// not track, so the prompt writes nothing and says why.
func TestAPromptAfterASessionStartThatCannotMoveTheAnchorCreatesNoState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "pabcd off", true: "pabcd on"}[enabled], func(t *testing.T) {
			a, b := t.TempDir(), t.TempDir()
			env := rootEnv(filepath.Join(t.TempDir(), ".crw"))
			if _, err := state.EnsureState(a, rootSession); err != nil {
				t.Fatal(err)
			}
			if err := stateroot.Guard(env, a, a, rootSession); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Dir(stateroot.AnchorPath(env, rootSession))
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			if answer := sessionHookSessionStart(SessionHookSessionStartPayload{Cwd: b, SessionID: rootSession}, env); answer == "" {
				t.Fatal("SessionStart did not refuse")
			}
			answer := promptSubmitHandle(PromptSubmitPayload{Cwd: b, SessionID: rootSession, Prompt: "remember this: the build needs go1.27", TurnID: "turn-1", PabcdEnabled: enabled}, "linux", env, state.WithSessionLock)
			if _, err := os.Stat(filepath.Join(b, ".crw")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the prompt created %s, which the anchor does not track: %v", filepath.Join(b, ".crw"), err)
			}
			if !strings.Contains(answer, stateroot.AnchorCode) || !strings.Contains(answer, stateroot.AnchorPath(env, rootSession)) {
				t.Fatalf("the prompt answer does not say why:\n%s", answer)
			}
		})
	}
}

// The control: a prompt that would write at a cwd away from an anchored root holding nothing in
// flight moves the anchor there first, then writes as before; a prompt that writes nothing moves
// nothing.
func TestAPromptAwayFromAnIdleAnchorMovesTheAnchorBeforeItWrites(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	env := rootEnv(filepath.Join(t.TempDir(), ".crw"))
	if err := stateroot.Guard(env, a, a, rootSession); err != nil {
		t.Fatal(err)
	}
	anchored := func() string {
		raw, err := os.ReadFile(stateroot.AnchorPath(env, rootSession))
		if err != nil {
			t.Fatal(err)
		}
		var rec struct{ NativeCwd string }
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		return rec.NativeCwd
	}
	promptSubmitHandle(PromptSubmitPayload{Cwd: b, SessionID: rootSession, Prompt: "hello", TurnID: "turn-1"}, "linux", env, state.WithSessionLock)
	if got := anchored(); got != a {
		t.Fatalf("a prompt that writes nothing moved the anchor to %q", got)
	}
	promptSubmitHandle(PromptSubmitPayload{Cwd: b, SessionID: rootSession, Prompt: "remember this: x", TurnID: "turn-2"}, "linux", env, state.WithSessionLock)
	if got := anchored(); got != b {
		t.Fatalf("anchor %q, want %q", got, b)
	}
	if s, bad := state.ReadStateStrict(b, rootSession); bad || !s.MemoryWriteRequested {
		t.Fatalf("no marker at the new cwd: %+v %v", s, bad)
	}
}
