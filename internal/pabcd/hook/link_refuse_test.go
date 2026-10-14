package hook

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-475 (evaluation d2): with .crw/sessions a symbolic link to another tree's sessions, every state write of the hooks is
// refused, and the hooks fail open. SessionStart creates nothing and tells the agent which link refused the state; a chat
// orchestrate command is answered as not applied, naming the link; a remembered prompt and a Stop of the in-flight state the
// link leads to release without blocking. The state behind the link is left byte for byte and the link stays.
func TestHooksFailOpenWithAWarningOnALinkedStateTree(t *testing.T) {
	cwd, env := stopRig(t, "active")
	outside := filepath.Join(filepath.Dir(cwd), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	stopInFlight(t, outside, state.PhaseB)
	sessions := filepath.Join(cwd, crwdir.DirName, state.SessionsSubdir)
	if err := os.MkdirAll(filepath.Dir(sessions), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, crwdir.DirName, state.SessionsSubdir), sessions); err != nil {
		t.Fatal(err)
	}
	target := state.StatePath(outside, stopSID)
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	unchanged := func(step string) {
		t.Helper()
		if after, err := os.ReadFile(target); err != nil || string(after) != string(before) {
			t.Errorf("%s wrote through the link: %q, %v", step, after, err)
		}
		if info, err := os.Lstat(sessions); err != nil || info.Mode()&fs.ModeSymlink == 0 {
			t.Errorf("%s: the link is not kept: %v", step, err)
		}
	}

	answer := SessionHookSessionStart(SessionHookSessionStartPayload{Cwd: cwd, SessionID: "fresh"})
	if !strings.Contains(answer, `"hookEventName":"SessionStart"`) || !strings.Contains(answer, sessions+" is a symbolic link") ||
		!strings.Contains(answer, "No PABCD state was created") {
		t.Errorf("SessionStart answer %q", answer)
	}
	if _, err := os.Lstat(state.StatePath(outside, "fresh")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("SessionStart created a state through the link: %v", err)
	}
	unchanged("SessionStart")

	a := PromptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: stopSID, Prompt: "orchestrate A", TurnID: "t1", PabcdEnabled: true}, "linux", env)
	if !strings.Contains(a, "was not applied") || !strings.Contains(a, sessions+" is a symbolic link") {
		t.Errorf("orchestrate command answer %q", a)
	}
	unchanged("an orchestrate command")
	if a := PromptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: stopSID, Prompt: "remember this: x", TurnID: "t2"}, "linux", env); a != "" {
		t.Errorf("a remembered prompt answered %q", a)
	}
	unchanged("a remembered prompt")
	if a := stopRun(cwd, env); a != (StopAnswer{}) {
		t.Errorf("Stop did not release: %+v", a)
	}
	unchanged("Stop")
}
