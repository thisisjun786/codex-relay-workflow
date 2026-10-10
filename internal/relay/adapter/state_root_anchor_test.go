package adapter

import (
	"os"
	"path/filepath"
	"testing"
)

// Verification round 2: a delivery whose thread's native root cannot be recorded as its anchor is
// refused before anything is sent, as not attempted and retry-safe.
func TestARelayDeliveryWhoseAnchorCannotBeRecordedIsRefused(t *testing.T) {
	home := filepath.Join(t.TempDir(), "crw-home")
	if err := os.WriteFile(home, []byte("a file, not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_HOME", home)
	a := t.TempDir()
	record := recordAt(a)
	rpc := &mcpRPC{}
	adapter := lostResumeAdapter(t, nativeRPC{rpc, a}, autoCompactChildPolicy(t, record))
	receipt := sendRecord(t, adapter, "send-unanchored", record)
	rpcError, _ := receipt["rpcError"].(map[string]any)
	if receipt["status"] != "not_attempted" || receipt["retrySafe"] != true || rpcError["code"] != "state_root_anchor_unrecorded" {
		t.Fatalf("receipt=%v", receipt)
	}
	if n := rpc.count("thread/resume") + rpc.count("turn/start"); n != 0 {
		t.Fatalf("the host was asked to resume or start a turn %d times", n)
	}
}
