package managed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestReconcileAbandonsOnlyUnresumableThreads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		receipt map[string]any
		replace bool
	}{
		{"resume refused", hostRefusal("thread/resume: no rollout found", "notLoaded", nil), true},
		{"unknown thread", hostRefusal("thread/read: thread not found", "", nil), true},
		{"settings mismatch", hostRefusal("thread/resume: settings differ", "notLoaded", map[string]any{"resumed": nil}), false},
		{"connection unavailable", hostRefusal("thread/resume: establish timeout", "notLoaded", map[string]any{"rpcError": map[string]any{"code": "connection_unavailable"}}), false},
		{"initialization refused", hostRefusal("initialize: refused", "notLoaded", nil), false},
		{"busy", hostRefusal("thread/read: Thread is active", "active", nil), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			k := newReconcileKit(t, "name-timeout", "accept")
			k.host.sendReceipts = []map[string]any{tc.receipt}
			k.expect(k.run(), "incomplete", "creation_unknown", "adopted")
			got := k.run()
			if !tc.replace {
				k.expect(got, "incomplete", "creation_unknown", "adopted")
				k.effects(1, 1)
				return
			}
			k.expect(got, "admitted", "", "recreated")
			if got["childTaskId"] != "t-2" || len(k.host.threads["t-1"].turns) != 0 || len(k.host.named) != 0 {
				t.Fatalf("abandoned orphan was used: %+v", got)
			}
			k.run()
			k.effects(2, 2) // refused recovery, then the replacement's business send
		})
	}
}

func TestReconcileUnknownTurnNeverLicensesAbandonment(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"turn-timeout", "saved-title"} {
		t.Run(outcome, func(t *testing.T) {
			k := newReconcileKit(t, outcome, "accept")
			k.host.failures["thread/read|t-1"] = errors.New("thread/read: thread not found")
			k.expect(k.run(), "incomplete", "creation_unknown", "standby_turn_unknown")
			k.effects(1, 0)
		})
	}
}

type readTrackingApp struct {
	*scriptedApp
	reads []string
}

func (h *readTrackingApp) HostCall(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	if method == "thread/read" {
		h.reads = append(h.reads, params["threadId"].(string))
	}
	return h.scriptedApp.HostCall(ctx, method, params)
}

func TestReconcileUUIDv7Window(t *testing.T) {
	t.Parallel()
	k := newReconcileKit(t, "lost")
	h := &readTrackingApp{scriptedApp: k.host}
	k.start.Adapter = h
	uuid := func(at time.Time, tail int) string {
		digits := fmt.Sprintf("%012x", at.UnixMilli())
		return fmt.Sprintf("%s-%s-7000-8000-%012x", digits[:8], digits[8:], tail)
	}
	// The read window includes one minute of ID clock slack on either side.
	for i, at := range []time.Time{k.clock.now.Add(-time.Minute - time.Millisecond), k.clock.now.Add(3*time.Minute + 20*time.Second + time.Millisecond)} {
		id := k.host.addThreadAs(uuid(at, i+1))
		k.host.failures["thread/read|"+id] = errors.New("out-of-window read")
	}
	fresh := k.host.addThreadAs(uuid(k.clock.now, 3))
	k.expect(k.run(), "admitted", "", "adopted")
	for _, id := range h.reads {
		if id != fresh {
			t.Fatalf("read outside window: %v", h.reads)
		}
	}
	if len(h.reads) == 0 {
		t.Fatal("in-window thread was not read")
	}
}

func TestReconcileExcludesBothEarlierReceiptSources(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"creation", "recovery"} {
		t.Run(source, func(t *testing.T) {
			k := newReconcileKit(t, "lost-applied", "lost-applied")
			k.host.sendReceipts = []map[string]any{hostRefusal("thread/resume: no rollout found", "notLoaded", nil)}
			k.expect(k.run(), "incomplete", "creation_unknown", "adopted")
			if source == "creation" {
				for id, receipt := range k.host.operations {
					if !strings.HasPrefix(id, "managed-standby-") {
						receipt["threadId"] = "t-1"
					}
				}
			}
			k.expect(k.run(), "incomplete", "creation_unknown", "recreated")
			h := &readTrackingApp{scriptedApp: k.host}
			k.start.Adapter = h
			got := k.run()
			k.expect(got, "admitted", "", "adopted")
			if got["childTaskId"] != "t-2" {
				t.Fatalf("adopted earlier attempt: %v", got)
			}
			for _, id := range h.reads {
				if id == "t-1" {
					t.Fatal("read abandoned thread")
				}
			}
			k.effects(2, 3)
		})
	}
}
