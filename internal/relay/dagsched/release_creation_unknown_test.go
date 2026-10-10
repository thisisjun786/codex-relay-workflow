package dagsched

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/adapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"golang.org/x/sys/unix"
)

// The host's answer to a creation was lost after the host did the work (the client's ack bound passed first): the bridge keeps its own
// outcome_unknown receipt and the release answered creation_unknown on every repeat. These tests run the real Go adapter and bridge against the fake
// App Server socket, so the receipts are the ones the bridge writes, and repeat the same release.

// hostNow is the host's clock in these tests: the kit's ledger and engine run on delivery.NewFakeClock, so a thread the host creates is created at its time.
const hostNow = 1_700_000_001

// leftovers is the App Server's memory of the threads it was asked for.
type leftovers struct {
	mu        sync.Mutex
	k         *realKit
	known     []string
	turns     map[string]int
	started   int
	notLoaded bool // the threads read as not loaded, with no rollout to read their turns from
	rejects   map[string]bool
	dropFirst bool // the first creation leaves no thread
	// lostFirstTitle drops the connection when the first creation's thread/name/set arrives, so
	// the creation receipt says outcome_unknown although the thread exists. The create path's ack
	// bound is 60 s and a fake answer cannot outlast it, so the lost answer is produced by the
	// transport ending rather than by a delayed reply.
	lostFirstTitle bool
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
		drop := st.started == 1 && st.dropFirst
		if !drop {
			st.known = append(st.known, id)
		}
		st.mu.Unlock()
		if drop {
			// The connection ends with this answer lost and the host leaves no thread: the bridge
			// keeps an outcome_unknown receipt listing thread/start. The create path's ack bound is
			// 60 s and a fake answer cannot outlast it, so the lost answer is produced by the
			// transport ending rather than by a delayed reply.
			return fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1011, Reason: "host lost the answer"}}
		}
		return fakehost.Reply{Result: k.startReply(id)}
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
	h.Handle("thread/name/set", func(raw json.RawMessage) fakehost.Reply {
		st.mu.Lock()
		lost := st.lostFirstTitle && st.started == 1
		st.lostFirstTitle = false
		st.mu.Unlock()
		if lost {
			// The connection ends with this answer lost: the host holds the thread it started, and
			// the bridge keeps an outcome_unknown receipt whose attempted effects are
			// [thread/start, thread/name/set] (turn/start is never reached).
			return fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1011, Reason: "host lost the answer"}}
		}
		return fakehost.Reply{}
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

// shortBounds are the client's phase bounds for the legacy-profile test. The host loses an answer by
// ending the connection, never by holding it back, so no bound is reached here and none is short: a
// 250 ms ack bound only failed the handshake and the reads on a loaded host (CRW-1161).
func shortBounds() appserver.PhaseBounds {
	return appserver.PhaseBounds{Establish: 30 * time.Second, Transmit: 30 * time.Second, Ack: 30 * time.Second}
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

const recoveryProfiles = `{"roles":{"parent":{"model":"gpt-5.4","reasoningEffort":"medium"},"child":{"model":"gpt-5.4","reasoningEffort":"medium","mcp":{"default":"minimal","profiles":{"minimal":{},"ui-qa":{"servers":["extra"]}}}}}}`

// This is a synthetic worker, with real worker-policy observation and locks. No daemon is launched.
func creationWorker(t *testing.T, k *realKit) (*service.Service, adapter.WorkerObservation) {
	t.Helper()
	srv, err := service.New(context.Background(), store.StateSelection{Path: k.state}, k.host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	record := srv.NewRecord(os.Getpid(), "test-worker")
	if err := srv.WriteRecord(record); err != nil {
		t.Fatal(err)
	}
	claimed, err := srv.Scope.Claim(k.host.SocketPath, record)
	if err != nil || claimed.Get("ok") != true {
		t.Fatalf("synthetic scope: %v %v", claimed, err)
	}
	t.Cleanup(func() {
		if err := srv.Scope.Release(k.host.SocketPath); err != nil {
			t.Error(err)
		}
	})
	lock, err := os.OpenFile(filepath.Join(k.state, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return srv, adapter.WorkerObservation{State: k.state, Socket: k.host.SocketPath, Scope: srv.Scope.Root, Authority: srv.Scope.Authority, Installation: filepath.Dir(executable)}
}

// sequential: t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR") is process-wide.
func TestReleaseCreationUnknownLegacyProfiles(t *testing.T) {
	for _, tc := range []struct {
		named          bool
		guard, changed string
	}{
		{true, "", ""}, {false, "", ""},
		{true, "standby", "fingerprint"}, {true, "standby", "state"},
		{false, "business", "fingerprint"}, {false, "business", "state"},
	} {
		t.Run(fmt.Sprintf("named=%v/%s/%s", tc.named, tc.guard, tc.changed), func(t *testing.T) {
			named := tc.named
			k := newRealKit(t)
			st := keepThreads(k)
			srv, observer := creationWorker(t, k)
			clock := delivery.NewFakeClock()
			var active *adapter.Adapter
			var requests []string
			bounds := shortBounds()
			reload := func() {
				t.Helper()
				if active != nil {
					if err := active.Close(); err != nil {
						t.Fatal(err)
					}
				}
				registry.ResetRolePolicySnapshot()
				policy := registry.EnvironmentRolePolicy()
				if err := srv.PublishWorkerPolicy(policy.Summary()); err != nil {
					t.Fatal(err)
				}
				var err error
				active, err = adapter.Open(k.host.SocketPath, k.state, adapter.Options{Policy: policy.BridgePolicy(), Clock: clock, RPC: appserver.New(k.host.SocketPath, bounds)})
				if err != nil {
					t.Fatal(err)
				}
				k.sched.Start = func(ctx context.Context, raw []byte) (StartAnswer, error) {
					requests = append(requests, string(raw))
					engine := managed.Start{Store: k.s, Adapter: adapter.Managed{Adapter: active}, Now: clock.ISO, Socket: k.host.SocketPath, MarkerRoot: k.marker, StateSelector: k.state, Readiness: func(ctx context.Context, req map[string]any) (string, error) { return observer.Ready(ctx, req, policy) }}
					answer, err := engine.Run(ctx, raw)
					return answerOf(answer), err
				}
			}
			t.Cleanup(func() {
				if active != nil {
					_ = active.Close()
				}
			})
			reload()
			if named {
				st.mu.Lock()
				st.lostFirstTitle = true
				st.mu.Unlock()
				k.host.Script("thread/read", rpcFailure("host busy"))
			} else {
				st.dropFirst = true
			}
			releasePlan(k.fixture, "rp")
			first, err := k.sched.Release(context.Background(), "rp", "A", "parent", k.request())
			if err != nil || first.Bound || first.Reason != "creation_unknown" {
				t.Fatalf("initial: %v %+v", err, first)
			}
			if k.count("SELECT COUNT(*) FROM managed_start_requests WHERE state='create_armed'") != 1 {
				t.Fatal("start was not armed")
			}
			frozen := requests[0]
			if strings.Contains(frozen, "mcpProfile") {
				t.Fatal("legacy request states a profile")
			}
			beforeProfiles := len(k.host.Requests())
			if err := os.WriteFile(os.Getenv(execution.EnvPolicy), []byte(recoveryProfiles), 0600); err != nil {
				t.Fatal(err)
			}
			k.host.Respond("config/read", fakehost.Reply{Result: map[string]any{"config": map[string]any{"mcp_servers": map[string]any{"extra": map[string]any{}}}}})
			k.host.Handle("mcpServerStatus/list", func(json.RawMessage) fakehost.Reply {
				var config map[string]any
				for _, r := range k.host.Requests() {
					if r.Method == "thread/start" || r.Method == "thread/resume" {
						config, _ = paramsOf(r.Params)["config"].(map[string]any)
					}
				}
				status := "connected"
				if _, off := pyjson.Map(config["mcp_servers"])["extra"]; off {
					status = "disabled"
				}
				return fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"name": "extra", "pluginId": nil, "runtimeStatus": status}}, "nextCursor": nil}}
			})
			if tc.guard != "" {
				bounds.Ack = 5 * time.Second
			}
			reload()
			if tc.guard != "" {
				if named {
					st.mu.Lock()
					st.notLoaded = true
					st.mu.Unlock()
				} else {
					clock.Advance(180)
				}
				paused, resume := make(chan struct{}, 1), make(chan struct{})
				var once sync.Once
				defer once.Do(func() { close(resume) })
				k.host.Handle("thread/resume", func(raw json.RawMessage) fakehost.Reply {
					id, _ := paramsOf(raw)["threadId"].(string)
					return fakehost.Reply{Result: k.startReply(id), Paused: paused, Release: resume}
				})
				type result struct {
					res ReleaseResult
					err error
				}
				finished := make(chan result, 1)
				go func() {
					res, err := k.sched.Release(context.Background(), "rp", "A", "parent", k.request())
					finished <- result{res, err}
				}()
				select {
				case <-paused:
				case <-time.After(10 * time.Second):
					t.Fatal("resume did not reach guard fault point")
				}
				turns := k.host.Count("turn/start")
				if tc.changed == "fingerprint" {
					k.exec("UPDATE managed_start_requests SET request_fingerprint='changed' WHERE request_id=?", first.RequestID)
				} else {
					k.exec("UPDATE managed_start_requests SET state='reserved' WHERE request_id=?", first.RequestID)
				}
				once.Do(func() { close(resume) })
				got := <-finished
				if got.err != nil || got.res.Bound || k.host.Count("turn/start") != turns {
					t.Fatalf("guard accepted invalid %s: %v %+v turns %d -> %d", tc.changed, got.err, got.res, turns, k.host.Count("turn/start"))
				}
				want := "creation_unknown"
				if tc.guard == "business" {
					want = "business_not_attempted"
				}
				if got.res.Reason != want {
					t.Fatalf("guard did not withhold intended send: %+v", got.res)
				}
				operation := first.Managed.Get("businessRequestId").(string)
				if tc.guard == "standby" {
					sum := sha256.Sum256([]byte(first.RequestID))
					operation = fmt.Sprintf("managed-standby-%x", sum)
				}
				receipt, err := (adapter.Managed{Adapter: active}).GetOperation(context.Background(), operation)
				if err != nil || pyjson.Map(receipt["rpcError"])["code"] != "mcp_profile_required" {
					t.Fatalf("reproof did not clear legacy admission: %v %v", receipt, err)
				}
				return
			}
			if named {
				st.mu.Lock()
				st.notLoaded, st.rejects["real-child"] = true, true
				st.mu.Unlock()
				refused, err := k.sched.Release(context.Background(), "rp", "A", "parent", k.request())
				if err != nil || refused.Bound || refused.Reason != "creation_unknown" || k.host.Count("turn/start") != 0 {
					t.Fatalf("refused resume: %v %+v", err, refused)
				}
				st.mu.Lock()
				st.notLoaded = false
				st.mu.Unlock()
			} else {
				clock.Advance(180)
			}
			res, _ := releaseUntilBound(t, k, 2)
			k.assertOneChild(t, res, "real-child-2", 2)
			if res.State != "admitted" || res.RequestID != first.RequestID || res.ManifestDigest != first.ManifestDigest || res.SlotID != first.SlotID {
				t.Fatalf("changed release identity: %+v", res)
			}
			for _, raw := range requests {
				if raw != frozen {
					t.Fatal("frozen managed request changed")
				}
			}
			if k.count("SELECT COUNT(*) FROM relationships") != 1 || k.count("SELECT COUNT(*) FROM generation_turns WHERE turn_id='business'") != 1 {
				t.Fatal("duplicate child admission")
			}
			verifiedProfiles := 0
			for _, r := range k.host.Requests()[beforeProfiles:] {
				params := paramsOf(r.Params)
				if r.Method == "turn/start" && params["threadId"] != "real-child-2" {
					t.Fatal("orphan was sent to")
				}
				if (r.Method == "thread/start" || r.Method == "thread/resume") && params["threadId"] != "real-child" && params["config"] != nil {
					verifiedProfiles++
					off := pyjson.Map(pyjson.Map(params["config"])["mcp_servers"])
					if !reflect.DeepEqual(off, map[string]any{"extra": map[string]any{"enabled": false}}) {
						t.Fatalf("default minimal not applied: %v", params)
					}
				}
			}
			if verifiedProfiles < 2 {
				t.Fatal("creation and resume profiles were not both observed")
			}
		})
	}
}
