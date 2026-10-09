package bridge

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// unwritableCRWHome points CRW_HOME at a regular file, so no anchor can be recorded under it.
func unwritableCRWHome(t *testing.T) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "crw-home")
	if err := os.WriteFile(home, []byte("a file, not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_HOME", home)
}

// Verification round 2: a send whose thread's native root cannot be recorded as its anchor is
// refused before anything is resumed, since the thread's SessionStart could not be guarded.
func TestASendWhoseAnchorCannotBeRecordedIsRefused(t *testing.T) {
	a := t.TempDir()
	bridge, host, input := stateRootSend(t, a, "notLoaded", a)
	unwritableCRWHome(t)
	receipt, err := bridge.SendMessageToThread(context.Background(), input)
	if rpc := pyjson.Map(receipt["rpcError"]); err != nil || receipt["status"] != "failed" || rpc["code"] != "state_root_anchor_unrecorded" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	if n := host.Count("thread/resume") + host.Count("turn/start"); n != 0 {
		t.Fatalf("the host was asked to resume or start a turn %d times", n)
	}
}
