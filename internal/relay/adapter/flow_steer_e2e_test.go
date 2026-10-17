package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// Exercise the production adapter/bridge/ledger path, including an old active
// turn whose transcript changes without opening a new turn.
func TestFlowSteerE2E(t *testing.T) {
	for _, scene := range []string{"wake", "report", "review", "stale-turn", "lost-response", "mismatched-response"} {
		t.Run(scene, func(t *testing.T) {
			w := newMgmtWorld(t)
			policy := filepath.Join(t.TempDir(), "roles.json")
			if err := os.WriteFile(policy, []byte(`{"roles":{"supervisor":{"expectation":"record"},"parent":{"model":"anthropic/claude-opus-5","reasoningEffort":"xhigh"}}}`), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEX_THREAD_BRIDGE_EXECUTION_POLICY", policy)
			registry.ResetRolePolicySnapshot()
			w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtA, "--task", mgmtA, "--host", "host")
			w.bindOwner("P-A", ownerA, mgmtA)
			w.open(mgmtA, ownerA)
			f := w.flowRun("P-A")
			const turn = "already-running"
			w.host.Handle("thread/read", func(json.RawMessage) fakehost.Reply {
				return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "active"}, "canAcceptDirectInput": true}}}
			})
			w.host.Handle("thread/turns/list", func(json.RawMessage) fakehost.Reply {
				return fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": turn, "status": "inProgress", "startedAt": 1699999900}}, "nextCursor": nil}}
			})
			var text, landed string
			w.host.Handle("turn/steer", func(raw json.RawMessage) fakehost.Reply {
				var p struct {
					Expected string `json:"expectedTurnId"`
					Input    []struct {
						Text string `json:"text"`
					} `json:"input"`
				}
				if err := json.Unmarshal(raw, &p); err != nil || p.Expected != turn || len(p.Input) != 1 {
					t.Errorf("bad steer params: %s %v", raw, err)
				}
				if scene == "review" {
					return fakehost.Reply{ErrorObject: fakehost.ErrorWithInfo(-32600, "cannot steer review", map[string]any{"activeTurnNotSteerable": map[string]any{"turnKind": "review"}})}
				}
				if scene == "stale-turn" {
					return fakehost.Reply{ErrorObject: map[string]any{"code": "expected_turn_mismatch", "message": "turn moved"}}
				}
				w.mu.Lock()
				text, landed = p.Input[0].Text, turn
				if scene == "mismatched-response" {
					landed = "another-turn"
				}
				actual := landed
				w.mu.Unlock()
				if scene == "lost-response" {
					return fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "lost acknowledgement"}}
				}
				return fakehost.Reply{Result: map[string]any{"turnId": actual}}
			})
			w.host.Handle("thread/items/list", func(json.RawMessage) fakehost.Reply {
				w.mu.Lock()
				defer w.mu.Unlock()
				return fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"turnId": landed, "item": map[string]any{"id": "steered-input", "type": "userMessage", "content": []any{map[string]any{"type": "text", "text": text}}}}}, "nextCursor": nil}}
			})
			var id string
			if scene == "report" {
				if _, err := f.writer().Up(w.ctx, blockedReport()); err != nil {
					t.Fatal(err)
				}
				id, _ = f.flowMessage("flow_project_blocked")
			} else {
				r, err := w.c.StageDirect(w.ctx, supervisor.DirectMessage{Kind: supervisor.KindSessionStepWaiting, Relationship: "run:" + flowRun, Subject: supervisor.FlowSubject(flowRun, "wake-1"), Summary: "ready", Text: "continue", At: delivery.ISOOf(w.clock.T)}, nil)
				if err != nil {
					t.Fatal(err)
				}
				id = r["messageId"].(string)
			}
			var result map[string]any
			var err error
			if scene == "wake" {
				w.tick(1)
				attempt, readErr := w.s.LatestSupervisorAttempt(w.ctx, id)
				if readErr != nil {
					t.Fatal(readErr)
				}
				err = json.Unmarshal([]byte(attempt.Record), &result)
			} else {
				result, err = w.c.Attempt(w.ctx, id, w.a, w.clock.T)
			}
			if err != nil || w.host.Count("turn/steer") != 1 || w.host.Count("turn/start") != 0 || w.host.Count("thread/resume") != 0 {
				t.Fatalf("steer path: %v %v, calls=%v", result, err, w.host.Requests())
			}
			switch scene {
			case "review", "stale-turn":
				if result["deliveryState"] != delivery.DeferredBusy {
					t.Fatalf("rejection: %v", result)
				}
				row, _ := w.s.SupervisorMessage(w.ctx, id)
				if _, err := w.c.Attempt(w.ctx, id, w.a, row.NextEligibleAt.Float64); err != nil || w.host.Count("turn/steer") != 1 {
					t.Fatalf("repeated rejected steer: %v", err)
				}
				w.scriptHost() // owner turn ends; ordinary send becomes eligible
				row, _ = w.s.SupervisorMessage(w.ctx, id)
				result, err = w.c.Attempt(w.ctx, id, w.a, row.NextEligibleAt.Float64)
				if err != nil || result["deliveryState"] != delivery.Dispatched || w.host.Count("turn/start") != 1 {
					t.Fatalf("idle fallback: %v %v", result, err)
				}
			case "lost-response", "mismatched-response":
				if result["deliveryState"] != delivery.HeldUncertain || result["retrySafe"] != false {
					t.Fatalf("uncertain steer: %v", result)
				}
				if _, err := w.c.Attempt(w.ctx, id, w.a, w.clock.T+200); err != nil || w.host.Count("turn/steer") != 1 || w.host.Count("turn/start") != 0 {
					t.Fatalf("uncertain send was retried: %v", err)
				}
			default:
				if result["deliveryState"] != delivery.Dispatched || result["turnId"] != turn || result["deliveryMethod"] != "turn/steer" {
					t.Fatalf("accepted steer: %v", result)
				}
				attempts, err := w.s.SupervisorAttempts(w.ctx, id)
				if err != nil || len(attempts) != 1 {
					t.Fatalf("attempts: %v %v", attempts, err)
				}
				row, _ := w.s.SupervisorMessage(w.ctx, id)
				replay, err := w.a.SteerMessage(w.ctx, attempts[0].RequestID, row.RecipientTaskID, turn, attempts[0].Message)
				if err != nil || replay.Get("replayed") != true || w.host.Count("turn/steer") != 1 {
					t.Fatalf("ledger replay: %v %v", replay, err)
				}
			}
			if scene != "review" && scene != "stale-turn" {
				read, err := w.c.ReadBack(w.ctx, id, turn, supervisor.Proof(id, turn), "", w.a, w.clock.T+1)
				want := "host_read"
				if scene == "mismatched-response" {
					want = "transcript_turn_mismatch"
				}
				if err != nil || read["verified"] != want {
					t.Fatalf("readback: %v %v, want %s", read, err, want)
				}
			}
			starts := w.host.Count("turn/start")
			if _, err := w.c.Attempt(w.ctx, id, w.a, w.clock.T+200); err != nil || w.host.Count("turn/steer") != 1 || w.host.Count("turn/start") != starts {
				t.Fatalf("duplicate delivery: %v", err)
			}
		})
	}
}

func TestFlowSteerTransportExclusionAndShutdown(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "recipient-exclusion", true: "shutdown"}[shutdown], func(t *testing.T) {
			w := newMgmtWorld(t)
			w.open(mgmtA)
			w.host.Handle("thread/read", func(json.RawMessage) fakehost.Reply {
				return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "active"}}}}
			})
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			releaseReply := sync.OnceFunc(func() { close(release) })
			defer releaseReply()
			w.host.Respond("turn/steer", fakehost.Reply{Paused: entered, Release: release, Result: map[string]any{"turnId": "running"}})
			done := make(chan error, 1)
			go func() {
				_, err := w.a.SteerMessage(w.ctx, "held-steer", mgmtA, "running", "input")
				done <- err
			}()
			waitEdge(t, entered)
			if shutdown {
				w.a.drain = -1
				closed := make(chan error, 1)
				go func() { closed <- w.a.Close() }()
				if err := waitEdge(t, done); !errors.Is(err, context.Canceled) {
					t.Fatalf("shutdown did not cancel steer: %v", err)
				}
				// Let the fake's handler service the WebSocket close handshake once
				// the transport has demonstrably cancelled the in-flight call.
				releaseReply()
				if err := waitEdge(t, closed); err != nil {
					t.Fatal(err)
				}
				if _, err := w.a.SteerMessage(w.ctx, "late", mgmtA, "running", "late input"); !errors.Is(err, ErrTransportClosing) {
					t.Fatalf("late admission: %v", err)
				}
				return
			}
			for _, steer := range []bool{false, true} {
				var r delivery.Obj
				var err error
				if steer {
					r, err = w.a.SteerMessage(w.ctx, "competing-steer", mgmtA, "running", "input")
				} else {
					r, err = w.a.SendMessage(w.ctx, "competing-start", mgmtA, "input", &delivery.TaskSettings{})
				}
				if err != nil || delivery.Classify(r).DeliveryState != delivery.DeferredBusy {
					t.Fatalf("competing send: %v %v", r, err)
				}
			}
			releaseReply()
			if err := waitEdge(t, done); err != nil {
				t.Fatal(err)
			}
			if w.host.Count("turn/steer") != 1 || w.host.Count("turn/start") != 0 {
				t.Fatal("concurrent transport escaped exclusion")
			}
		})
	}
}
