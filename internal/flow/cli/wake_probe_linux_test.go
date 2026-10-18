//go:build linux

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/adapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/daemon"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

type probeHostState struct {
	mu             sync.Mutex
	host           *fakehost.Server
	dir, scene     string
	threads        map[string]*probeHostThread
	worker         *fakehost.Conn
	creates, wakes int
}
type probeHostThread struct {
	id, status string
	archived   bool
	turns      []any
}

func newProbeHost(t *testing.T, dir, scene string) *probeHostState {
	h := &probeHostState{host: fakehost.Start(t), dir: dir, scene: scene, threads: map[string]*probeHostThread{}}
	h.host.HandleConn("probe/worker", func(conn *fakehost.Conn, _ json.RawMessage) fakehost.Reply {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.worker = conn
		return fakehost.Reply{}
	})
	for _, method := range []string{"thread/start", "thread/read", "thread/resume", "thread/list", "thread/turns/list", "thread/items/list", "thread/goal/get", "thread/name/set", "thread/unsubscribe", "thread/archive", "thread/unarchive", "turn/start"} {
		method := method
		h.host.HandleConn(method, func(conn *fakehost.Conn, raw json.RawMessage) fakehost.Reply {
			h.mu.Lock()
			defer h.mu.Unlock()
			var p map[string]any
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Error(err)
			}
			id, _ := p["threadId"].(string)
			fail := func(message string) fakehost.Reply {
				return fakehost.Reply{Error: &fakehost.RPCError{Code: -32601, Message: message}}
			}
			object := func(th *probeHostThread) map[string]any {
				return map[string]any{"id": th.id, "cwd": dir, "model": "model-a", "reasoningEffort": "high",
					"approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false},
					"runtimeWorkspaceRoots": []any{dir}, "environments": []any{},
					"status": map[string]any{"type": th.status}, "canAcceptDirectInput": true}
			}
			envelope := func(th *probeHostThread) fakehost.Reply {
				return fakehost.Reply{Result: map[string]any{"thread": object(th), "model": "model-a", "reasoningEffort": "high",
					"cwd": dir, "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false},
					"runtimeWorkspaceRoots": []any{dir}, "environments": []any{}}}
			}
			switch method {
			case "thread/start":
				h.creates++
				if scene == "create-unsupported" {
					return fail("creation unsupported")
				}
				id = fmt.Sprintf("probe-%d", h.creates)
				th := &probeHostThread{id: id, status: "idle"}
				h.threads[id] = th
				return envelope(th)
			case "thread/list":
				archived, _ := p["archived"].(bool)
				rows := []any{}
				for _, th := range h.threads {
					if th.archived == archived {
						rows = append(rows, object(th))
					}
				}
				return fakehost.Reply{Result: map[string]any{"data": rows, "nextCursor": nil}}
			case "thread/goal/get":
				return fakehost.Reply{Result: map[string]any{"goal": nil}}
			case "thread/name/set", "thread/unsubscribe":
				return fakehost.Reply{}
			}
			if id == "owner" {
				t.Errorf("probe touched the owner with %s", method)
				return fail("owner must not be touched")
			}
			th := h.threads[id]
			if th == nil {
				return fail("unknown probe")
			}
			switch method {
			case "thread/read":
				return envelope(th)
			case "thread/resume":
				th.status = "idle"
				return envelope(th)
			case "thread/archive":
				th.archived, th.status = true, "notLoaded"
				return fakehost.Reply{}
			case "thread/unarchive":
				th.archived = false
				if scene == "stays-loaded" {
					th.status = "idle"
				}
				return envelope(th)
			case "thread/turns/list":
				rows := append([]any{}, th.turns...)
				slices.Reverse(rows)
				return fakehost.Reply{Result: map[string]any{"data": rows, "nextCursor": nil}}
			case "thread/items/list":
				return fakehost.Reply{Result: map[string]any{"data": []any{}, "nextCursor": nil}}
			case "turn/start":
				if len(th.turns) != 0 {
					h.wakes++
					if conn != h.worker {
						t.Error("the CLI sent the wake instead of the observed worker")
					}
					if scene == "send-failed" {
						return fail("wake refused")
					}
				}
				turnID := fmt.Sprintf("%s-turn-%d", id, len(th.turns)+1)
				turn := map[string]any{"id": turnID, "status": "completed", "startedAt": time.Now().Unix(), "items": []any{}, "itemsView": "full"}
				th.turns = append(th.turns, turn)
				th.status = "idle"
				return fakehost.Reply{Result: map[string]any{"turn": turn}, Before: []fakehost.Notification{
					{Method: "turn/started", Params: map[string]any{"threadId": id, "turn": map[string]any{"id": turnID, "status": "inProgress"}}},
					{Method: "turn/completed", Params: map[string]any{"threadId": id, "turn": turn}},
				}}
			}
			return fail("unscripted method")
		})
	}
	return h
}

// configureProbeWorker is the existing TestStartReadsRealSupervisorWake service/worker
// arrangement: claimed scope, published worker policy, and one real production adapter
// goroutine as the only wake sender.
func configureProbeWorker(t *testing.T, w *world, h *probeHostState) {
	t.Helper()
	configureProbeWorkerUnder(t, w, h, false)
}

// configureProbeWorkerUnder is configureProbeWorker with the policy read from this process's environment
// (declared false), or declared by the service's launch declaration with the variable unset (declared true),
// which is what a daemon launched on the declaration runs under. A declared worker starts no in-process
// sender: the probe is observed up to its staged message, and no daemon send happens. It returns the digest
// of the policy the worker published.
func configureProbeWorkerUnder(t *testing.T, w *world, h *probeHostState, declared bool) string {
	t.Helper()
	policy := filepath.Join(t.TempDir(), "roles.json")
	if err := os.WriteFile(policy, []byte(`{"roles":{"parent":{"model":"model-a","reasoningEffort":"high"},"supervisor":{"expectation":"record"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var published registry.RolePolicy
	if declared {
		declareLaunchPolicy(t, w.state, policy)
		t.Setenv(execution.EnvPolicy, "")
		published = registry.ResolveRolePolicy(map[string]string{execution.EnvPolicy: policy})
	} else {
		t.Setenv("CODEX_THREAD_BRIDGE_EXECUTION_POLICY", policy)
	}
	t.Setenv(service.ScopeEnv, t.TempDir())
	if err := os.Chmod(os.Getenv(service.ScopeEnv), 0o700); err != nil {
		t.Fatal(err)
	}
	registry.ResetRolePolicySnapshot()
	t.Cleanup(registry.ResetRolePolicySnapshot)
	if !declared {
		published = registry.EnvironmentRolePolicy()
	}
	settings, err := json.Marshal(map[string]any{"approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "cwd": h.dir, "runtimeWorkspaceRoots": []string{h.dir}, "model": "model-a", "reasoningEffort": "high", "environments": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.st.DB.ExecContext(ctx, `INSERT INTO authorized_settings(task_id,settings,source,recorded_at) VALUES ('owner',?,'test','now')`, string(settings)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.st.DB.ExecContext(ctx, `INSERT INTO scope_bindings(binding_id,role,scope_kind,scope_key,task_id,host_id,status,revision,created_at,updated_at) VALUES ('owner-binding','parent','project','project-1','owner','host','active',1,'now','now')`); err != nil {
		t.Fatal(err)
	}
	// Bind the store's socket the way a socket-bound daemon open does: while no
	// writable store holds the gate, a socket-bound open stamps schema_meta and the
	// mirror under write-gate EX and keeps SH. Reopen the world's store named for
	// this socket so later writers only need that shared hold.
	if err := w.st.Close(); err != nil {
		t.Fatal(err)
	}
	bound, err := store.Open(ctx, w.db, h.host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	w.st = bound
	selection, err := ResolveState(w.state, h.host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := service.New(ctx, selection, h.host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := daemon.Acquire(w.state, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := svc.NewRecord(os.Getpid(), "wake-probe-test")
	if err := svc.WriteRecord(record); err != nil {
		t.Fatal(err)
	}
	claimed, err := svc.Scope.Claim(h.host.SocketPath, record)
	if err != nil || claimed.Get("ok") != true {
		t.Fatalf("scope: %v %v", claimed, err)
	}
	if err := svc.PublishWorkerPolicy(published.Summary()); err != nil {
		t.Fatal(err)
	}
	if declared {
		t.Cleanup(func() {
			if err := svc.Scope.Release(h.host.SocketPath); err != nil {
				t.Error(err)
			}
			if err := lock.Close(); err != nil {
				t.Error(err)
			}
		})
		return published.Digest()
	}
	worker, err := adapter.Open(h.host.SocketPath, w.state, adapter.Options{Store: w.st})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.HostCall(ctx, "probe/worker", nil); err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	errs := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
				if _, err := (&supervisor.Channel{Store: w.st, Socket: h.host.SocketPath}).AutoSend(workerCtx, worker, float64(time.Now().UnixMicro())/1e6, 0, 1, "", "", ""); err != nil && workerCtx.Err() == nil {
					select {
					case errs <- err:
					default:
					}
					return
				}
			}
		}
	}()
	// Registered last so the worker stops before the policy env is restored and the store closes.
	t.Cleanup(func() {
		cancel()
		<-done
		select {
		case err := <-errs:
			t.Errorf("worker send: %v", err)
		default:
		}
		if err := worker.Close(); err != nil {
			t.Error(err)
		}
		if err := svc.Scope.Release(h.host.SocketPath); err != nil {
			t.Error(err)
		}
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	})
	return published.Digest()
}

func TestWakeProbeFinishRetiresBindingAfterHostTimeout(t *testing.T) {
	w := newWorld(t)
	host := fakehost.Start(t)
	host.Respond("thread/turns/list", fakehost.Reply{Delay: time.Hour})
	a, err := adapter.Open(host.SocketPath, w.state, adapter.Options{Store: w.st})
	if err != nil {
		t.Fatal(err)
	}
	const id = "probe-timeout"
	scope := "wake-probe:" + id
	if _, err := w.st.DB.ExecContext(ctx, "INSERT INTO scope_bindings(binding_id,role,scope_kind,scope_key,task_id,host_id,status,revision,created_at,updated_at) VALUES ('probe-binding','parent','project',?,'probe-1','host','active',1,'now','now')", scope); err != nil {
		t.Fatal(err)
	}
	p := &wakeProbeRun{
		c: &command{Deps: &Deps{Now: time.Now}, ctx: ctx}, st: w.st, state: w.state, socket: host.SocketPath,
		owner: "owner", want: &delivery.WakeExpectation{Identity: delivery.WakeIdentity{Role: "parent"}},
		id: id, thread: "probe-1", bootstrap: "probe-1-turn-1", host: a,
		registry: &registry.Registry{Store: w.st}, bound: true, archiveSafe: true,
	}
	const hostFailure = "probe cleanup left unproved history intact"
	finishErr := p.finish(errors.New("probe failed"))
	// Both outcomes are read before either is judged, so one failing run reports both (CRW-1672).
	var active int
	activeErr := w.st.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM scope_bindings WHERE scope_key=? AND status='active'", scope).Scan(&active)
	var failures int
	var reason string
	evidenceErr := w.st.DB.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(MAX(json_extract(detail,'$.reason')),'') FROM journal WHERE kind=? AND json_extract(detail,'$.probe')=1", delivery.WakeObserved).Scan(&failures, &reason)
	if finishErr == nil || !strings.Contains(finishErr.Error(), hostFailure) {
		t.Errorf("finish must return the host cleanup failure: %v", finishErr)
	}
	if activeErr != nil || active != 0 {
		t.Errorf("probe binding stayed active: active=%d, want 0 (query error: %v)", active, activeErr)
	}
	if evidenceErr != nil || failures != 1 {
		t.Errorf("failure evidence: WakeObserved=%d, want 1 (query error: %v)", failures, evidenceErr)
	}
	if !strings.Contains(reason, "probe failed") || !strings.Contains(reason, hostFailure) {
		t.Errorf("failure reason=%q", reason)
	}
}

func TestStartWakeProbeEmptyJournal(t *testing.T) {
	for _, scene := range []string{"success", "create-unsupported", "stays-loaded", "send-failed"} {
		t.Run(scene, func(t *testing.T) {
			w := newWorld(t)
			h := newProbeHost(t, t.TempDir(), scene)
			// Use the existing currentWake fixture setup below verbatim.
			configureProbeWorker(t, w, h)
			td := w.deps()
			td.Probe, td.Now = StoreProber{}, time.Now
			args := []string{"start", "project-lane", "--preview", "--json", "--backend", "app-server", "--thread", "owner", "--state", w.state, "--socket", h.host.SocketPath, "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", defaultHandoff(t)}
			code, out, errOut := td.doKeep(args...)
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
			h.mu.Lock()
			creates, wakes := h.creates, h.wakes
			h.mu.Unlock()
			if creates != 1 {
				t.Fatalf("want one probe recipient, created=%d fact=%+v", creates, fact)
			}
			if scene == "success" {
				if !fact.Pass || wakes != 1 {
					t.Fatalf("one worker wake must prove fact5: wakes=%d fact=%+v", wakes, fact)
				}
				var proof, flowWake int
				if err := w.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM journal WHERE kind=? AND json_extract(detail,'$.newTurn')=1`, delivery.WakeObserved).Scan(&proof); err != nil || proof != 1 {
					t.Fatalf("proof=%d %v", proof, err)
				}
				if err := w.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM flow_events WHERE kind='message_delivery' AND json_extract(payload_json,'$.wake_id')='wake-probe' AND json_extract(payload_json,'$.result')='accepted'`).Scan(&flowWake); err != nil || flowWake != 1 {
					t.Fatalf("run wake=%d %v", flowWake, err)
				}
				code, out, errOut = td.doKeep(args...)
				if code != 0 || json.Unmarshal([]byte(out), &answer) != nil {
					t.Fatalf("cached start: %d %s %s", code, out, errOut)
				}
				h.mu.Lock()
				creates, wakes = h.creates, h.wakes
				h.mu.Unlock()
				if creates != 1 || wakes != 1 {
					t.Fatalf("cached proof created another probe: creates=%d wakes=%d", creates, wakes)
				}
				for _, f := range answer.Plan.Facts {
					if f.Name == FactIdleWake && !f.Pass {
						t.Fatalf("cached proof failed: %+v", f)
					}
				}
			} else {
				if fact.Level != LevelWarn || fact.Reason == "" {
					t.Fatalf("failed probe must warn: %+v", fact)
				}
				var failures int
				if err := w.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM journal WHERE kind=? AND json_extract(detail,'$.probe')=1 AND length(json_extract(detail,'$.reason'))>0`, delivery.WakeObserved).Scan(&failures); err != nil || failures != 1 {
					t.Fatalf("failure evidence=%d %v", failures, err)
				}
				if wakes > 1 {
					t.Fatalf("probe retried the wake %d times", wakes)
				}
			}
			var owner, activeProbes int
			if err := w.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM scope_bindings WHERE task_id='owner' AND status='active'`).Scan(&owner); err != nil || owner != 1 {
				t.Fatalf("owner binding changed: %d %v", owner, err)
			}
			if err := w.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM scope_bindings WHERE scope_key LIKE 'wake-probe:%' AND status='active'`).Scan(&activeProbes); err != nil || activeProbes != 0 {
				t.Fatalf("live probe bindings=%d %v", activeProbes, err)
			}
		})
	}
}

// Under a launch declaration the probe creates its recipient, binds it and records its settings with the
// declared policy, the same policy the wake identity and the probe's own answer use.
func TestStartWakeProbeUnderLaunchDeclaration(t *testing.T) {
	w := newWorld(t)
	h := newProbeHost(t, t.TempDir(), "create-unsupported")
	configureProbeWorkerUnder(t, w, h, true)
	td := w.deps()
	td.Probe, td.Now = StoreProber{}, time.Now
	code, out, errOut := td.doKeep("start", "project-lane", "--preview", "--json", "--backend", "app-server", "--thread", "owner", "--state", w.state, "--socket", h.host.SocketPath, "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", defaultHandoff(t))
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
	h.mu.Lock()
	creates := h.creates
	h.mu.Unlock()
	if creates != 1 {
		t.Fatalf("want one probe recipient under the declaration, created=%d fact=%+v", creates, fact)
	}
	// The host refuses creation, so the probe reports its own creation failure; the policy refusals must not appear.
	if fact.Level != LevelWarn || !strings.Contains(fact.Reason, "creation did not prove a separate probe recipient") || strings.Contains(fact.Reason, "role_policy_unconfigured") || strings.Contains(fact.Reason, "execution_role_unknown") {
		t.Fatalf("the probe must reach creation and fail there, not on the policy: %+v", fact)
	}
}

// A declared worker has no in-process sender, so the run stops at its staged message and the test checks what
// the probe recorded up to that point, all under the declared policy.
func TestWakeProbeBindsAndRecordsUnderLaunchDeclaration(t *testing.T) {
	w := newWorld(t)
	h := newProbeHost(t, t.TempDir(), "success")
	published := configureProbeWorkerUnder(t, w, h, true)
	want, err := currentWake(ctx, &Reader{St: w.st, State: w.state}, h.host.SocketPath, "owner")
	if err != nil {
		t.Fatal(err)
	}
	const id = "probe-declared"
	p := &wakeProbeRun{
		c: &command{Deps: &Deps{Now: time.Now}, ctx: ctx}, st: w.st, state: w.state, socket: h.host.SocketPath,
		owner: "owner", want: want, id: id,
	}
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				var staged int
				if err := w.st.DB.QueryRowContext(runCtx, "SELECT COUNT(*) FROM supervisor_messages WHERE relationship_id = ?", "run:"+id).Scan(&staged); err == nil && staged == 1 {
					cancel()
					return
				}
			}
		}
	}()
	runErr := p.run(runCtx)
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("the run must stop at its post-staging wait: %v", runErr)
	}
	h.mu.Lock()
	creates, wakes := h.creates, h.wakes
	h.mu.Unlock()
	if creates != 1 || wakes != 0 {
		t.Fatalf("probe creations=%d wakes=%d, want 1 and 0", creates, wakes)
	}
	if !p.bound || p.journal == nil || p.message == "" {
		t.Fatalf("bound=%v journal=%v message=%q: the binding, journal and staged message must all exist", p.bound, p.journal != nil, p.message)
	}
	var recorded int
	if err := w.st.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM authorized_settings WHERE task_id=? AND source='wake_probe'", p.thread).Scan(&recorded); err != nil || recorded != 1 {
		t.Fatalf("probe settings records=%d: %v", recorded, err)
	}
	if digest := p.registry.Policy.Digest(); digest == "" || digest != want.Identity.RolePolicy || digest != published {
		t.Fatalf("probe registry policy %q, wake identity %q, published %q: one declared policy must answer all three", digest, want.Identity.RolePolicy, published)
	}
	if err := p.finish(runErr); err != nil {
		t.Logf("finish: %v", err)
	}
	var active int
	if err := w.st.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM scope_bindings WHERE scope_key=? AND status='active'", "wake-probe:"+id).Scan(&active); err != nil || active != 0 {
		t.Fatalf("probe binding stayed active=%d: %v", active, err)
	}
}
