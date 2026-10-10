package spawn

import (
	"os"
	"testing"
	"time"
)

// CRW-1118 (pre-merge evaluation of 2be6b6f2): a refused spawn's grant survives a concurrent clean-up of the invalid-grant path.

// --- CRW-1118 D1: a concurrent clean-up never removes a valid grant that comes back.

func TestSpawnGrantCleanupKeepsAGrantReleasedMeanwhile(t *testing.T) {
	rig, marker, file := spawnGrantClaimRig(t)
	scope := map[string]any{"cwd": rig.ws, "session_id": "rec-s1"}
	now := time.Now()
	obj := map[string]any{"cwd": rig.ws, "session_id": "rec-s1", "agent_id": "child-1"}
	a, ok := spawnGrantCheck(obj, marker, rig.tmp, os.Getuid(), now, "call-A", "in-A")
	if !ok || !a.reserve(now) {
		t.Fatal("call A could not reserve the grant")
	}
	// Call B judges the grant while A holds it, and A gives it back before B's clean-up runs.
	spawnGrantBeforeCleanup = func() { a.release() }
	t.Cleanup(func() { spawnGrantBeforeCleanup = func() {} })
	b, okB := spawnGrantCheck(obj, marker, rig.tmp, os.Getuid(), now, "call-B", "in-B")
	spawnGrantBeforeCleanup = func() {}
	if _, err := os.Lstat(file); err != nil {
		t.Fatalf("the clean-up removed the released grant: %v (B %v %v)", err, b, okB)
	}
	if again, ok := spawnGrantCheck(obj, marker, rig.tmp, os.Getuid(), now, "call-A", "in-A"); !ok || again == nil {
		t.Fatal("the corrected retry lost its capability")
	}
	_ = scope
}
