package hook

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/stateroot"
)

const rootSession = "0190cafe-0279-7000-8000-000000000279"

func rootEnv(crwHome string) host.LookupEnv {
	return func(key string) (string, bool) {
		if key == "CRW_HOME" {
			return crwHome, true
		}
		if key == "HOME" {
			return filepath.Dir(crwHome), true
		}
		return "", false
	}
}

// rootInFlight writes the session's state at root in phase P with a plan epoch.
func rootInFlight(t *testing.T, root string) []byte {
	t.Helper()
	s := state.DefaultState(rootSession, "work")
	s.Phase, s.OrchestrationActive = state.PhaseP, true
	epoch := "epoch-1"
	s.PlanEpoch = &epoch
	if err := state.WriteState(root, s); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(state.StatePath(root, rootSession))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// CRW-1140: a relay-managed thread (one a CRW resume path anchored at its native root A) whose work
// is in flight at A is not bootstrapped at another cwd B: SessionStart creates no IDLE file at B,
// leaves A's bytes, and tells the agent where its state is.
func TestSessionStartDoesNotBootstrapAManagedThreadAwayFromItsInFlightRoot(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	env := rootEnv(filepath.Join(t.TempDir(), ".crw"))
	before := rootInFlight(t, a)
	if conflict := stateroot.Guard(env, a, a, rootSession); conflict != nil {
		t.Fatal(conflict)
	}
	answer := sessionHookSessionStart(SessionHookSessionStartPayload{Cwd: b, SessionID: rootSession}, env)
	if _, err := os.Stat(state.StatePath(b, rootSession)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("SessionStart created a state at the other cwd: %v", err)
	}
	if after, err := os.ReadFile(state.StatePath(a, rootSession)); err != nil || string(after) != string(before) {
		t.Fatalf("the native state changed (%v)", err)
	}
	var out struct {
		HookSpecificOutput struct{ HookEventName, AdditionalContext string }
	}
	if err := json.Unmarshal([]byte(answer), &out); err != nil || out.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Fatalf("answer %q (%v)", answer, err)
	}
	for _, want := range []string{state.StatePath(a, rootSession), "phase P", "--cwd " + a} {
		if !strings.Contains(out.HookSpecificOutput.AdditionalContext, want) {
			t.Errorf("the context lacks %q:\n%s", want, out.HookSpecificOutput.AdditionalContext)
		}
	}
}

// Criterion 3, the control: a standalone terminal session has no anchor, so SessionStart at B
// bootstraps an IDLE state there exactly as before and answers nothing; A is untouched.
func TestSessionStartOfAStandaloneSessionBootstrapsAsBefore(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	env := rootEnv(filepath.Join(t.TempDir(), ".crw"))
	before := rootInFlight(t, a)
	if answer := sessionHookSessionStart(SessionHookSessionStartPayload{Cwd: b, SessionID: rootSession}, env); answer != "" {
		t.Fatalf("answer %q", answer)
	}
	if s, bad := state.ReadStateStrict(b, rootSession); bad || s.Phase != state.PhaseIdle {
		t.Fatalf("no IDLE state at the standalone cwd: %+v %v", s, bad)
	}
	if after, _ := os.ReadFile(state.StatePath(a, rootSession)); string(after) != string(before) {
		t.Fatal("the other tree's state changed")
	}
}

// An anchored thread bootstraps at its root, at another cwd when nothing is in flight at the root,
// and never touches a state that already exists where it starts.
func TestSessionStartOfAnAnchoredThreadOtherwiseBootstrapsAsBefore(t *testing.T) {
	t.Run("at its root", func(t *testing.T) {
		a := t.TempDir()
		env := rootEnv(filepath.Join(t.TempDir(), ".crw"))
		if _, err := state.EnsureState(a, rootSession); err != nil {
			t.Fatal(err)
		}
		stateroot.Guard(env, a, a, rootSession)
		if err := os.Remove(state.StatePath(a, rootSession)); err != nil {
			t.Fatal(err)
		}
		if answer := sessionHookSessionStart(SessionHookSessionStartPayload{Cwd: a, SessionID: rootSession}, env); answer != "" {
			t.Fatalf("answer %q", answer)
		}
		if _, err := os.Stat(state.StatePath(a, rootSession)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("nothing in flight", func(t *testing.T) {
		a, b := t.TempDir(), t.TempDir()
		env := rootEnv(filepath.Join(t.TempDir(), ".crw"))
		if _, err := state.EnsureState(a, rootSession); err != nil {
			t.Fatal(err)
		}
		stateroot.Guard(env, a, a, rootSession)
		if answer := sessionHookSessionStart(SessionHookSessionStartPayload{Cwd: b, SessionID: rootSession}, env); answer != "" {
			t.Fatalf("answer %q", answer)
		}
		if _, err := os.Stat(state.StatePath(b, rootSession)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a state already at the cwd", func(t *testing.T) {
		a, b := t.TempDir(), t.TempDir()
		env := rootEnv(filepath.Join(t.TempDir(), ".crw"))
		rootInFlight(t, a)
		stateroot.Guard(env, a, a, rootSession)
		existing := rootInFlight(t, b)
		if answer := sessionHookSessionStart(SessionHookSessionStartPayload{Cwd: b, SessionID: rootSession}, env); answer != "" {
			t.Fatalf("answer %q", answer)
		}
		if after, _ := os.ReadFile(state.StatePath(b, rootSession)); string(after) != string(existing) {
			t.Fatal("the existing state changed")
		}
	})
}
