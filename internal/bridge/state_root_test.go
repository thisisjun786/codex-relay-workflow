package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// stateRootThread is the PABCD session the thread's id addresses: the bridge's test thread.
const stateRootThread = "thread-1"

// inFlightAt writes the thread's PABCD state at root in phase P, with a plan epoch and a goalplan
// file beside it, and returns every byte under root/.crw so a test can prove nothing changed.
func inFlightAt(t *testing.T, root string) map[string]string {
	t.Helper()
	s := state.DefaultState(stateRootThread, "work")
	s.Phase, s.OrchestrationActive = state.PhaseP, true
	epoch := "epoch-1"
	s.PlanEpoch = &epoch
	if err := state.WriteState(root, s); err != nil {
		t.Fatal(err)
	}
	goalplans := filepath.Join(root, ".crw", "goalplans")
	if err := os.MkdirAll(goalplans, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goalplans, "work.md"), []byte("# goalplan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return treeBytes(t, root)
}

func treeBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(filepath.Join(root, ".crw"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		out[path] = string(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameBytes(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// stateRootSend is a send whose thread the host reports at native, with status, asking for cwd.
func stateRootSend(t *testing.T, native, status, cwd string) (*Bridge, *fakehost.Server, SendMessage) {
	t.Helper()
	t.Setenv("CRW_HOME", t.TempDir())
	b, host, input := settingsSend(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": stateRootThread, "cwd": native, "status": map[string]any{"type": status}}}})
	resume := startReply(cwd)
	host.Respond("thread/resume", resume)
	input.Expected["cwd"] = cwd
	return b, host, input
}

// CRW-1140 criterion 1: a thread whose PABCD work is in flight at its native cwd A is not resumed
// at another cwd B. The send is refused before thread/resume, A's bytes are kept, and B gets no
// IDLE state file.
func TestASendToAnotherCwdIsRefusedWhileTheNativeStateIsInFlight(t *testing.T) {
	for _, status := range []string{"notLoaded", "idle"} {
		t.Run(status, func(t *testing.T) {
			a, b := t.TempDir(), t.TempDir()
			before := inFlightAt(t, a)
			bridge, host, input := stateRootSend(t, a, status, b)
			receipt, err := bridge.SendMessageToThread(context.Background(), input)
			rpc := pyjson.Map(receipt["rpcError"])
			if err != nil || receipt["status"] != "failed" || rpc["code"] != "state_root_conflict" {
				t.Fatalf("receipt=%v err=%v", receipt, err)
			}
			message, _ := rpc["message"].(string)
			if !strings.Contains(message, state.StatePath(a, stateRootThread)) || !strings.Contains(message, "phase P") {
				t.Errorf("the refusal does not name the preserved state: %q", message)
			}
			if n := host.Count("thread/resume") + host.Count("turn/start"); n != 0 {
				t.Fatalf("the host was asked to resume or start a turn %d times", n)
			}
			if after := treeBytes(t, a); !sameBytes(before, after) {
				t.Fatalf("the native state changed:\nbefore %v\nafter  %v", before, after)
			}
			if _, err := os.Stat(filepath.Join(b, ".crw")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the target cwd got a state directory: %v", err)
			}
		})
	}
}

// The native cwd itself, and a symbolic-link alias of it, are the same root: the send goes ahead,
// and the root is recorded as the thread's anchor for the SessionStart bootstrap.
func TestASendAtTheNativeRootOrAnAliasOfItGoesAhead(t *testing.T) {
	a := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(a, alias); err != nil {
		t.Fatal(err)
	}
	for name, cwd := range map[string]string{"native": a, "alias": alias} {
		t.Run(name, func(t *testing.T) {
			inFlightAt(t, a)
			bridge, host, input := stateRootSend(t, a, "notLoaded", cwd)
			receipt, err := bridge.SendMessageToThread(context.Background(), input)
			if err != nil || receipt["status"] != "accepted" || host.Count("thread/resume") != 1 {
				t.Fatalf("receipt=%v err=%v", receipt, err)
			}
			raw, err := os.ReadFile(filepath.Join(os.Getenv("CRW_HOME"), "state-roots", stateRootThread+".json"))
			var anchor struct{ NativeCwd string }
			if err != nil || json.Unmarshal(raw, &anchor) != nil || anchor.NativeCwd != a {
				t.Fatalf("anchor %s (%v), want %s", raw, err, a)
			}
		})
	}
}

// A native root with nothing in flight (no state, or an IDLE one) does not hold a move back.
func TestASendToAnotherCwdGoesAheadWhenNothingIsInFlight(t *testing.T) {
	for _, idle := range []bool{false, true} {
		a, b := t.TempDir(), t.TempDir()
		if idle {
			if _, err := state.EnsureState(a, stateRootThread); err != nil {
				t.Fatal(err)
			}
		}
		bridge, host, input := stateRootSend(t, a, "notLoaded", b)
		receipt, err := bridge.SendMessageToThread(context.Background(), input)
		if err != nil || receipt["status"] != "accepted" || host.Count("thread/resume") != 1 {
			t.Fatalf("idle=%v receipt=%v err=%v", idle, receipt, err)
		}
	}
}
