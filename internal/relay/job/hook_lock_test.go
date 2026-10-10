package job

import (
	"strings"
	"testing"
	"time"
)

// CRW-1092, post-evaluation round (f24d11cd): the wake switch is read under the lock the delivery runs under.

// "off" and the delivery share the store lock; a hook that saw the wake on before it waited for the lock sees it again under it.
func TestAHookThatWaitedForTheLockHonoursAnOffThatFinishedFirst(t *testing.T) {
	ws := workspace(t)
	hookDone(t, ws, "late", "S1")
	unlock, err := lockStore(ws) // "off" holds the lock and has not written its switch yet
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { done <- HandleStop(HookPayload{SessionID: "S1", Cwd: ws}, ws, hookEnv(nil), noon) }()
	time.Sleep(300 * time.Millisecond)
	put(t, DisabledPath(ws), "2026-09-09T00:09:00.000Z\n")
	unlock()
	if out := <-done; out != "" {
		t.Errorf("a Stop hook handed a completion out after off: %q", out)
	}
	if onDisk, _ := ReadRecord(ws, "late"); onDisk.DeliveredAt != nil {
		t.Errorf("the completion was stamped delivered while the wake is off")
	}
	// Drain keeps its documented exception: it collects with the wake off.
	if out := DrainNow(ws, sp("S1"), noon); !strings.Contains(out, "late") {
		t.Errorf("drain with the wake off: %q", out)
	}
}
