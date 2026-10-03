package dagsched

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// The host's answer to a creation was lost after the host did the work (the client's ack bound passed first): the bridge keeps its own
// outcome_unknown receipt and the release answered creation_unknown on every repeat. These tests run the real Go adapter and bridge against the fake
// App Server socket, so the receipts are the ones the bridge writes, and repeat the same release.

// hostNow is the host's clock in these tests: the kit's ledger and engine run on delivery.NewFakeClock, so a thread the host creates is created at its time.
const hostNow = 1_700_000_001

// leftovers is the App Server's memory of the threads it was asked for.
type leftovers struct {
	mu         sync.Mutex
	k          *realKit
	known      []string
	turns      map[string]int
	started    int
	notLoaded  bool // the threads read as not loaded, with no rollout to read their turns from
	rejects    map[string]bool
	startDelay time.Duration // the first thread/start is answered this late
}

func rpcFailure(message string) fakehost.Reply {
	return fakehost.Reply{Error: &fakehost.RPCError{Code: -32000, Message: message}}
}

func paramsOf(raw json.RawMessage) map[string]any {
	params := map[string]any{}
	_ = json.Unmarshal(raw, &params)
	return params
}

func (k *realKit) startReply(thread string) map[string]any {
	settings := k.settings()
	reply := map[string]any{}
	for key, v := range settings {
		if key != "environments" {
			reply[key] = v
		}
	}
	k.mu.Lock()
	reply["model"] = k.model
	k.mu.Unlock()
	reply["thread"] = map[string]any{"id": thread, "environments": settings["environments"]}
	return reply
}

// keepThreads makes the fake host remember the threads it creates, show them in its loaded listing, read them as an idle thread with no turn
// until one is started, and refuse a thread/resume of the threads the test names.
func keepThreads(k *realKit) *leftovers {
	st := &leftovers{k: k, turns: map[string]int{}, rejects: map[string]bool{}}
	h := k.host
	h.Handle("thread/start", func(json.RawMessage) fakehost.Reply {
		st.mu.Lock()
		st.started++
		id := "real-child"
		if st.started > 1 {
			id = fmt.Sprintf("real-child-%d", st.started)
		}
		st.known = append(st.known, id)
		delay := time.Duration(0)
		if st.started == 1 {
			delay = st.startDelay
		}
		st.mu.Unlock()
		return fakehost.Reply{Result: k.startReply(id), Delay: delay}
	})
	h.Handle("thread/loaded/list", func(json.RawMessage) fakehost.Reply {
		st.mu.Lock()
		defer st.mu.Unlock()
		data := []any{}
		for _, id := range st.known {
			data = append(data, id)
		}
		return fakehost.Reply{Result: map[string]any{"data": data}}
	})
	h.Handle("thread/list", func(raw json.RawMessage) fakehost.Reply {
		st.mu.Lock()
		defer st.mu.Unlock()
		data := []any{}
		if paramsOf(raw)["archived"] != true {
			for _, id := range st.known {
				data = append(data, map[string]any{"id": id})
			}
		}
		return fakehost.Reply{Result: map[string]any{"data": data, "nextCursor": nil}}
	})
	h.Handle("thread/read", func(raw json.RawMessage) fakehost.Reply {
		id, _ := paramsOf(raw)["threadId"].(string)
		st.mu.Lock()
		defer st.mu.Unlock()
		status, preview := "idle", ""
		if st.notLoaded {
			status = "notLoaded"
		}
		if st.turns[id] > 0 {
			preview = "the standby prompt"
		}
		return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": id, "cwd": k.root, "createdAt": hostNow, "name": nil, "model": "gpt-5.4", "reasoningEffort": "medium",
			"preview": preview, "ephemeral": false, "parentThreadId": nil, "status": map[string]any{"type": status}, "canAcceptDirectInput": true}}}
	})
	h.Handle("thread/turns/list", func(raw json.RawMessage) fakehost.Reply {
		id, _ := paramsOf(raw)["threadId"].(string)
		st.mu.Lock()
		defer st.mu.Unlock()
		switch {
		case st.turns[id] == 0 && st.notLoaded:
			return rpcFailure("missing source rollout")
		case st.turns[id] == 0:
			return rpcFailure("thread is not materialized yet; unavailable before first user message")
		}
		rows := []any{map[string]any{"id": "standby", "status": "completed"}}
		if st.turns[id] > 1 {
			rows = append(rows, map[string]any{"id": "business", "status": "completed"})
		}
		return fakehost.Reply{Result: map[string]any{"data": rows, "nextCursor": nil}}
	})
	h.Handle("turn/start", func(raw json.RawMessage) fakehost.Reply {
		id, _ := paramsOf(raw)["threadId"].(string)
		st.mu.Lock()
		defer st.mu.Unlock()
		st.turns[id]++
		turn := "standby"
		if st.turns[id] > 1 {
			turn = "business"
		}
		return fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": turn}}}
	})
	h.Handle("thread/resume", func(raw json.RawMessage) fakehost.Reply {
		id, _ := paramsOf(raw)["threadId"].(string)
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.rejects[id] {
			return rpcFailure("no rollout found for thread id " + id)
		}
		return fakehost.Reply{Result: k.startReply(id)}
	})
	return st
}

// releaseUntilBound repeats the release, as the coordinator does, and returns the first call that bound the child and how many calls it took.
func releaseUntilBound(t *testing.T, k *realKit, most int) (ReleaseResult, int) {
	t.Helper()
	for call := 1; call <= most; call++ {
		res, err := k.sched.Release(context.Background(), "rp", "A", "parent", k.request())
		if err != nil {
			t.Fatalf("release %d: %v", call, err)
		}
		t.Logf("release %d: %s/%s/%s", call, res.State, res.Stage, res.Reason)
		if res.Bound {
			return res, call
		}
		if res.Reason != "creation_unknown" {
			t.Fatalf("release %d answered %s/%s/%s, want creation_unknown or bound", call, res.State, res.Stage, res.Reason)
		}
	}
	var methods []string
	for _, request := range k.host.Requests() {
		methods = append(methods, request.Method)
	}
	t.Fatalf("the release did not bind in %d calls; the host saw %v", most, methods)
	return ReleaseResult{}, 0
}

func shortBounds() appserver.PhaseBounds {
	return appserver.PhaseBounds{Establish: 2 * time.Second, Transmit: 2 * time.Second, Ack: 250 * time.Millisecond}
}

func (k *realKit) assertOneChild(t *testing.T, res ReleaseResult, thread string, starts int) {
	t.Helper()
	if res.ChildTaskID != thread || k.host.Count("thread/start") != starts || k.host.Count("turn/start") != 2 {
		t.Fatalf("child %s, host saw %d thread/start and %d turn/start; want %s, %d and 2", res.ChildTaskID, k.host.Count("thread/start"), k.host.Count("turn/start"), thread, starts)
	}
	if rows := countRows(k.fixture); rows.releases != 1 || rows.executions != 1 || rows.slots != 1 || rows.manifests != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	again, err := k.sched.Release(context.Background(), "rp", "A", "parent", k.request())
	if err != nil || !again.Bound || !again.Replayed || again.ChildTaskID != thread || k.host.Count("thread/start") != starts {
		t.Fatalf("a further release = %v %+v, thread/start %d", err, again, k.host.Count("thread/start"))
	}
}

// A stuck release whose creation never answered thread/start: it was answered after the ack bound, so the receipt is outcome_unknown without a thread id, and the
// first reconciliation cannot read the host. The same release, repeated, finds the thread the host did create and ends bound to it.
func TestReleaseRepeatedAfterAThreadStartTimeoutBindsTheThreadTheHostCreated(t *testing.T) {
	k := newRealKit(t, shortBounds())
	st := keepThreads(k)
	st.startDelay = time.Second
	k.host.Script("thread/loaded/list", rpcFailure("host busy"))
	releasePlan(k.fixture, "rp")
	res, calls := releaseUntilBound(t, k, 4)
	if calls != 2 {
		t.Fatalf("the first release is stuck on the unreadable host and the repeat binds: %d calls", calls)
	}
	k.assertOneChild(t, res, "real-child", 1)
}

// A stuck release whose creation never answered thread/name/set: it was answered after the ack bound, so the receipt names the thread and the creation response.
func TestReleaseRepeatedAfterAThreadNameTimeoutBindsTheThread(t *testing.T) {
	k := newRealKit(t, shortBounds())
	keepThreads(k)
	k.host.Script("thread/name/set", fakehost.Reply{Result: map[string]any{}, Delay: time.Second})
	k.host.Script("thread/read", rpcFailure("host busy"))
	releasePlan(k.fixture, "rp")
	res, calls := releaseUntilBound(t, k, 4)
	if calls != 2 {
		t.Fatalf("%d calls", calls)
	}
	k.assertOneChild(t, res, "real-child", 1)
	if k.host.Count("thread/name/set") != 2 {
		t.Fatalf("the title is set once more after the standby is accepted: %d", k.host.Count("thread/name/set"))
	}
}

// A thread that is no longer loaded and has no rollout, so the host will not resume it: after the standby send is refused the release
// gives it up and creates the next attempt, which binds. One child, and the abandoned thread is never sent to or named again.
func TestReleaseRepeatedReplacesAThreadTheHostWillNotResume(t *testing.T) {
	k := newRealKit(t, shortBounds())
	st := keepThreads(k)
	k.host.Script("thread/name/set", fakehost.Reply{Result: map[string]any{}, Delay: time.Second})
	k.host.Script("thread/read", rpcFailure("host busy"))
	releasePlan(k.fixture, "rp")
	first, err := k.sched.Release(context.Background(), "rp", "A", "parent", k.request())
	if err != nil || first.Bound || first.Reason != "creation_unknown" {
		t.Fatalf("the first release = %v %+v", err, first)
	}
	st.mu.Lock()
	st.notLoaded, st.rejects["real-child"] = true, true
	st.mu.Unlock()
	second, err := k.sched.Release(context.Background(), "rp", "A", "parent", k.request())
	if err != nil || second.Bound || second.Reason != "creation_unknown" || k.host.Count("turn/start") != 0 {
		t.Fatalf("the repeat that reaches the refused resume = %v %+v, turn/start %d", err, second, k.host.Count("turn/start"))
	}
	st.mu.Lock()
	st.notLoaded = false
	st.mu.Unlock()
	res, _ := releaseUntilBound(t, k, 2)
	k.assertOneChild(t, res, "real-child-2", 2)
	if k.host.Count("thread/name/set") != 2 {
		t.Fatalf("the abandoned thread was named again: %d thread/name/set", k.host.Count("thread/name/set"))
	}
}
