package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/daemon"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

type wakeHost struct {
	delivery.Adapter
	status     string
	now        float64
	sends      int
	folded     bool
	connection delivery.WakeConnection
}

func (h *wakeHost) WakeConnection() delivery.WakeConnection { return h.connection }

func (h *wakeHost) ReadThread(context.Context, string) (delivery.ThreadFacts, error) {
	return delivery.ThreadFacts{RuntimeStatus: h.status, CanAcceptInput: true}, nil
}
func (*wakeHost) IsArchived(context.Context, string, any) (*bool, error) { b := false; return &b, nil }
func (*wakeHost) ReadGoalStatus(context.Context, string) (any, error)    { return nil, nil }
func (*wakeHost) ListTurnIDs(context.Context, string, int) ([]any, error) {
	return []any{"old-turn"}, nil
}
func (h *wakeHost) ReadTurn(_ context.Context, _, id string) (*delivery.TurnInfo, error) {
	return &delivery.TurnInfo{TurnID: id, StartedAt: h.now}, nil
}
func (h *wakeHost) SendMessage(_ context.Context, request, _, _ string, _ *delivery.TaskSettings) (delivery.Obj, error) {
	h.sends++
	turn := "new-turn"
	if h.folded {
		turn = "old-turn"
	}
	return delivery.Obj{{Key: "requestId", Value: request}, {Key: "status", Value: "accepted"}, {Key: "turnId", Value: turn}, {Key: "statusBeforeResume", Value: h.status}}, nil
}

func TestStartReadsRealSupervisorWake(t *testing.T) {
	for _, status := range []string{"notLoaded", "idle", "folded", "manual"} {
		t.Run(status, func(t *testing.T) {
			w := newWorld(t)
			host := fakehost.Start(t)
			// A live worker's store is socket-bound. Leave probe creation explicitly
			// unsupported here so negative wake evidence remains a warning without
			// depending on a lock timeout in an unbound test store.
			w.close()
			var err error
			w.st, err = store.Open(ctx, w.db, host.SocketPath)
			if err != nil {
				t.Fatal(err)
			}
			host.Respond("thread/start", fakehost.Reply{Error: &fakehost.RPCError{Code: -32601, Message: "probe creation unsupported"}})
			policy := filepath.Join(t.TempDir(), "roles.json")
			if err := os.WriteFile(policy, []byte(`{"roles":{"parent":{"model":"model-a","reasoningEffort":"high"},"supervisor":{"expectation":"record"}}}`), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEX_THREAD_BRIDGE_EXECUTION_POLICY", policy)
			t.Setenv(service.ScopeEnv, t.TempDir())
			if err := os.Chmod(os.Getenv(service.ScopeEnv), 0700); err != nil {
				t.Fatal(err)
			}
			registry.ResetRolePolicySnapshot()
			t.Cleanup(registry.ResetRolePolicySnapshot)
			a, _ := wakeFixture()
			if _, err := w.st.DB.ExecContext(ctx, `INSERT INTO authorized_settings(task_id,settings,source,recorded_at) VALUES ('owner',?,'test','now')`, string(a.RequiredSettings)); err != nil {
				t.Fatal(err)
			}
			if _, err := w.st.DB.ExecContext(ctx, `INSERT INTO scope_bindings(binding_id,role,scope_kind,scope_key,task_id,host_id,status,revision,created_at,updated_at) VALUES ('owner-binding','parent','project','project-1','owner','host','active',1,'now','now')`); err != nil {
				t.Fatal(err)
			}
			selection, err := ResolveState(w.state, host.SocketPath)
			if err != nil {
				t.Fatal(err)
			}
			svc, err := service.New(ctx, selection, host.SocketPath)
			if err != nil {
				t.Fatal(err)
			}
			lock, err := daemon.Acquire(w.state, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			record := svc.NewRecord(os.Getpid(), "test-service-run")
			if err := svc.WriteRecord(record); err != nil {
				t.Fatal(err)
			}
			claimed, err := svc.Scope.Claim(host.SocketPath, record)
			if err != nil || claimed.Get("ok") != true {
				t.Fatalf("scope: %v %v", claimed, err)
			}
			defer svc.Scope.Release(host.SocketPath)
			if err := svc.PublishWorkerPolicy(registry.EnvironmentRolePolicy().Summary()); err != nil {
				t.Fatal(err)
			}
			r := &Reader{St: w.st, State: w.state}
			want, err := currentWake(ctx, r, host.SocketPath, "owner")
			if err != nil {
				t.Fatal(err)
			}
			before := StoreProber{}.Facts(ctx, PreflightRequest{Reader: r, Thread: "owner", Now: time.Now(), Wake: want})
			if before[4].Pass {
				t.Fatal("no wake was sent")
			}
			w.newRun("wake-run", "owner")
			c := &supervisor.Channel{Store: w.st, Socket: host.SocketPath}
			now := float64(time.Now().Unix())
			staged, err := c.StageDirect(ctx, supervisor.DirectMessage{Kind: supervisor.KindSessionStepWaiting, Relationship: "run:wake-run", Subject: supervisor.FlowSubject("wake-run", "wake-1"), Summary: "ready", Text: "continue", At: delivery.ISOOf(now)}, nil)
			if err != nil {
				t.Fatal(err)
			}
			client, err := appserver.Dial(ctx, host.SocketPath)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			peer, connID := client.ConnectionSnapshot()
			h := &wakeHost{status: status, now: now, connection: delivery.WakeConnection{Conn: peer, ID: connID, Socket: host.SocketPath}}
			if status == "folded" || status == "manual" {
				h.status = "notLoaded"
				h.folded = status == "folded"
			}
			if status == "manual" {
				_, err = c.Attempt(ctx, staged["messageId"].(string), h, now)
			} else {
				_, err = c.AutoSend(ctx, h, now, 0, 1, "", "", "")
			}
			if err != nil || h.sends != 1 {
				t.Fatalf("send count=%d err=%v", h.sends, err)
			}
			// The journal survives consumption of the scheduler's transient wake rows.
			if _, err := w.st.DB.ExecContext(ctx, `DELETE FROM delivery_wakes`); err != nil {
				t.Fatal(err)
			}
			td := w.deps()
			td.Probe = StoreProber{}
			td.Now = time.Now
			code, out, errOut := td.doKeep("start", "project-lane", "--preview", "--json", "--backend", "app-server", "--thread", "owner", "--state", w.state, "--socket", host.SocketPath, "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", defaultHandoff(t))
			var answer StartAnswer
			if code != 0 || json.Unmarshal([]byte(out), &answer) != nil {
				t.Fatalf("start: %d %s %s", code, out, errOut)
			}
			var fact Fact
			for _, f := range answer.Plan.Facts {
				if f.Name == FactIdleWake {
					fact = f
				}
			}
			if fact.Pass != (status == "notLoaded") {
				t.Fatalf("start fact 5 for %s: %+v", status, fact)
			}
			probeFailures := 1
			if status == "notLoaded" {
				probeFailures = 0
			}
			var failures int
			if err := w.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM journal WHERE kind=? AND json_extract(detail,'$.probe')=1 AND length(json_extract(detail,'$.reason'))>0`, delivery.WakeObserved).Scan(&failures); err != nil || failures != probeFailures {
				t.Fatalf("probe failures=%d, want %d: %v", failures, probeFailures, err)
			}
			// A bounded no-eviction wait may explicitly require idle; ordinary start may not.
			if status == "idle" {
				want.RequiredStatus = "idle"
				facts := StoreProber{}.Facts(ctx, PreflightRequest{Reader: r, Thread: "owner", Now: time.Now(), Wake: want})
				if !facts[4].Pass {
					t.Fatalf("bounded idle wake: %+v", facts[4])
				}
			}
			if status == "notLoaded" {
				// A stale service record must invalidate the independent current reading.
				if err := svc.WriteRecord(record.Set("startTicks", int64(-1))); err != nil {
					t.Fatal(err)
				}
				if _, err := currentWake(ctx, r, host.SocketPath, "owner"); err == nil {
					t.Fatal("stale process identity passed")
				}
			}
		})
	}
}
