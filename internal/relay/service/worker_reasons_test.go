package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/adapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"golang.org/x/sys/unix"
)

// Every reason service.read_worker_policy gives, against the reason Python gave for the same
// change (testdata/worker_reasons.json, captured once by worker_reasons_capture.py, so no
// Python runs here). One live fixture per case: this test process is the serving worker - it
// holds the daemon lock and the scope lock and has published its receipt - and each case
// changes one thing. Three Go readers answer each case and must agree with Python: the
// service's own, doctor's (the relay CLI entry point, with its pre-checks), and the one
// managed-start admits through.

type workerGolden struct {
	Observations map[string]*string `json:"observations"`
	Readiness    map[string]*string `json:"readiness"`
	Policy       json.RawMessage    `json:"policy"`
}

func loadWorkerGolden(t *testing.T) workerGolden {
	t.Helper()
	raw, err := os.ReadFile("testdata/worker_reasons.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden workerGolden
	if err = json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	return golden
}

type workerFixture struct {
	t                       *testing.T
	home, state, socket     string
	scopeJSON, scopeLock    string
	policyFile              string
	service                 *service.Service
	locks                   []*os.File
	observer                adapter.WorkerObservation
	receiptPath, recordPath string
}

func newWorkerFixture(t *testing.T, policy json.RawMessage) *workerFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv(service.ScopeEnv, filepath.Join(home, "scopes"))
	f := &workerFixture{t: t, home: home, state: filepath.Join(home, "state"), socket: filepath.Join(home, "app.sock"), policyFile: filepath.Join(home, "policy.json")}
	if err := os.WriteFile(f.policyFile, policy, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(execution.EnvPolicy, f.policyFile)
	ctx := context.Background()
	// A copy of one store created once: creating a fenced store costs a quarter second, and
	// every case needs one of its own. The copy gets its own physical identity (Rehome).
	if err := os.MkdirAll(f.state, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(f.state, "relay.sqlite3")
	if err := os.WriteFile(database, workerStoreBytes(t), 0o600); err != nil {
		t.Fatal(err)
	}
	testsupport.Rehome(t, database)
	selection, err := store.ResolveStateDir(f.state, "")
	if err != nil {
		t.Fatal(err)
	}
	if f.service, err = service.New(ctx, selection, f.socket); err != nil {
		t.Fatal(err)
	}
	if f.service.StoreID == "" {
		t.Fatal("the fixture store states no identity")
	}
	key := f.service.Scope.Key(f.socket)
	f.scopeJSON, f.scopeLock = filepath.Join(f.service.Scope.Root, key+".json"), filepath.Join(f.service.Scope.Root, key+".lock")
	f.receiptPath, f.recordPath = filepath.Join(f.state, "worker-policy.json"), filepath.Join(f.state, "daemon.json")
	record := f.service.NewRecord(os.Getpid(), "token-1")
	if err = f.service.WriteRecord(record); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(f.service.Scope.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	f.writeJSON(f.scopeJSON, f.readJSON(f.recordPath))
	for _, path := range []string{filepath.Join(f.state, "daemon.lock"), f.scopeLock} {
		lock, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		f.locks = append(f.locks, lock)
	}
	t.Cleanup(func() {
		for _, lock := range f.locks {
			_ = lock.Close()
		}
	})
	summary := registry.ResolveRolePolicy(map[string]string{execution.EnvPolicy: f.policyFile}).Summary()
	if err = f.service.PublishWorkerPolicy(summary); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	if err != nil {
		t.Fatal(err)
	}
	f.observer = adapter.WorkerObservation{State: f.state, Socket: f.socket, Scope: f.service.Scope.Root, Authority: f.service.Scope.Authority, Installation: filepath.Dir(executable)}
	return f
}

// workerStore is the bytes of a closed, Go-created fenced store, made once per test binary.
var workerStore = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "worker-store-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	db, err := store.Open(context.Background(), filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		return nil, err
	}
	if err = db.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(dir, "relay.sqlite3"))
})

func workerStoreBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := workerStore()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (f *workerFixture) readJSON(path string) map[string]any {
	f.t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if err = decoder.Decode(&value); err != nil {
		f.t.Fatal(err)
	}
	return value
}

func (f *workerFixture) writeJSON(path string, value any) {
	f.t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		f.t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *workerFixture) edit(path string, change func(map[string]any)) {
	f.t.Helper()
	value := f.readJSON(path)
	change(value)
	f.writeJSON(path, value)
}

func (f *workerFixture) write(path, text string) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *workerFixture) remove(path string) {
	f.t.Helper()
	if err := os.Remove(path); err != nil {
		f.t.Fatal(err)
	}
}

// serveAs makes pid the serving worker every record names, as a supervisor's note and the
// worker's own receipt would.
func (f *workerFixture) serveAs(pid int, ticks any) {
	f.t.Helper()
	for _, path := range []string{f.recordPath, f.scopeJSON} {
		f.edit(path, func(r map[string]any) { r["pid"], r["startTicks"] = pid, ticks })
	}
	f.edit(f.receiptPath, func(r map[string]any) {
		r["worker"].(map[string]any)["pid"], r["worker"].(map[string]any)["startTicks"] = pid, ticks
		r["service"].(map[string]any)["pid"], r["service"].(map[string]any)["startTicks"] = pid, ticks
	})
}

// child starts a process that is not this one and returns it with its start ticks.
func (f *workerFixture) child() (*exec.Cmd, any) {
	f.t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd, service.StartTicks(cmd.Process.Pid)
}

var workerChanges = map[string]func(f *workerFixture){
	"valid":             func(*workerFixture) {},
	"receipt-absent":    func(f *workerFixture) { f.remove(f.receiptPath) },
	"receipt-not-json":  func(f *workerFixture) { f.write(f.receiptPath, "{") },
	"receipt-list":      func(f *workerFixture) { f.write(f.receiptPath, "[]") },
	"receipt-oversized": func(f *workerFixture) { f.write(f.receiptPath, strings.Repeat(" ", 70000)) },
	"record-absent":     func(f *workerFixture) { f.remove(f.recordPath) },
	"version-2":         func(f *workerFixture) { f.edit(f.receiptPath, func(r map[string]any) { r["schemaVersion"] = 2 }) },
	"version-true":      func(f *workerFixture) { f.edit(f.receiptPath, func(r map[string]any) { r["schemaVersion"] = true }) },
	"worker-not-object": func(f *workerFixture) { f.edit(f.receiptPath, func(r map[string]any) { r["worker"] = []any{} }) },
	"policy-not-object": func(f *workerFixture) { f.edit(f.receiptPath, func(r map[string]any) { r["policy"] = nil }) },
	"record-pid-zero":   func(f *workerFixture) { f.edit(f.recordPath, func(r map[string]any) { r["pid"] = 0 }) },
	"record-workerpid-string": func(f *workerFixture) {
		f.edit(f.recordPath, func(r map[string]any) { r["workerPid"] = "123" })
	},
	"worker-pid-other": func(f *workerFixture) {
		f.edit(f.receiptPath, func(r map[string]any) { r["worker"].(map[string]any)["pid"] = 1 })
	},
	"worker-ticks": func(f *workerFixture) {
		f.edit(f.receiptPath, func(r map[string]any) { r["worker"].(map[string]any)["startTicks"] = 1 })
	},
	"worker-bootid": func(f *workerFixture) {
		f.edit(f.receiptPath, func(r map[string]any) { r["worker"].(map[string]any)["bootId"] = "older-boot" })
	},
	"record-bootid": func(f *workerFixture) {
		f.edit(f.recordPath, func(r map[string]any) { r["bootId"] = "older-boot" })
		f.edit(f.receiptPath, func(r map[string]any) { r["worker"].(map[string]any)["bootId"] = "older-boot" })
	},
	"service-token": func(f *workerFixture) {
		f.edit(f.receiptPath, func(r map[string]any) { r["service"].(map[string]any)["token"] = "wrong" })
	},
	"record-statedir": func(f *workerFixture) {
		f.edit(f.recordPath, func(r map[string]any) { r["stateDir"] = "/elsewhere" })
		f.edit(f.receiptPath, func(r map[string]any) { r["service"].(map[string]any)["stateDir"] = "/elsewhere" })
	},
	"db-replaced": func(f *workerFixture) {
		database := filepath.Join(f.state, "relay.sqlite3")
		raw, err := os.ReadFile(database)
		if err != nil {
			f.t.Fatal(err)
		}
		f.write(database+".copy", string(raw))
		if err = os.Rename(database+".copy", database); err != nil {
			f.t.Fatal(err)
		}
	},
	"process-stopped": func(f *workerFixture) {
		cmd, ticks := f.child()
		if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
			f.t.Fatal(err)
		}
		for deadline := time.Now().Add(5 * time.Second); service.ProcessState(cmd.Process.Pid) != "T"; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				f.t.Fatalf("child state %q", service.ProcessState(cmd.Process.Pid))
			}
		}
		f.serveAs(cmd.Process.Pid, ticks)
	},
	"process-dead": func(f *workerFixture) {
		cmd, ticks := f.child()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		f.serveAs(cmd.Process.Pid, ticks)
	},
	"scope-absent": func(f *workerFixture) { f.remove(f.scopeJSON) },
	"scope-token":  func(f *workerFixture) { f.edit(f.scopeJSON, func(r map[string]any) { r["token"] = "other" }) },
	// The lock is unlinked while this process still holds the old inode, as Python's case
	// unlinks it under its worker: a missing lock is a read failure, never "unheld".
	"scope-lock-missing":  func(f *workerFixture) { f.remove(f.scopeLock) },
	"daemon-lock-missing": func(f *workerFixture) { f.remove(filepath.Join(f.state, "daemon.lock")) },
}

// recordChanges is Python's patch of service.record: the second read of daemon.json is another
// run's. Only service.ReadWorkerPolicy has the seam; the other readers are not asked.
const recordChanges = "record-changes"

func TestWorkerPolicy_every_reason_is_pythons_in_every_reader(t *testing.T) {
	golden := loadWorkerGolden(t)
	if len(golden.Observations) != len(workerChanges)+1 {
		t.Fatalf("golden has %d observation cases, the table %d", len(golden.Observations), len(workerChanges)+1)
	}
	requirements := `[{"role":"parent","model":"anthropic/claude-opus-5-5","reasoningEffort":"xhigh"}]`
	ctx := context.Background()
	for name := range golden.Observations {
		t.Run(name, func(t *testing.T) {
			want := golden.Observations[name]
			f := newWorkerFixture(t, golden.Policy)
			if name == recordChanges {
				restore := service.SetBeforeRecheck(func() {
					f.edit(f.recordPath, func(r map[string]any) { r["token"] = "new-run" })
				})
				defer restore()
			} else if change, ok := workerChanges[name]; ok {
				change(f)
			} else {
				t.Fatalf("no Go change for Python's case %q", name)
			}
			observation := f.service.ReadWorkerPolicy(ctx)
			if got := observed(observation); !sameReason(got, want) {
				t.Fatalf("service reader answered %v, Python %v: %s", show(got), show(want), encode(t, observation))
			}
			if name == recordChanges {
				return
			}
			if _, reason := f.observer.Read(ctx); !sameReason(nullable(reason), want) {
				t.Errorf("managed-start's reader answered %q, Python %v", reason, show(want))
			}
			var out, errOut bytes.Buffer
			code := cli.Execute(ctx, []string{"--state", f.state, "--socket", f.socket, "--json", "doctor", "--require-worker-policy", requirements}, &out, &errOut)
			if wantCode := map[bool]int{true: 0, false: 2}[want == nil]; code != wantCode {
				t.Fatalf("doctor exit %d, want %d: %s%s", code, wantCode, out.String(), errOut.String())
			}
			var report map[string]json.RawMessage
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			// The CLI entry point answers the object the service reader does, pre-checks and all.
			if doctor := string(report["workerPolicy"]); doctor != reindent(t, encode(t, observation)) {
				t.Errorf("doctor's workerPolicy\n%s\nservice reader\n%s", doctor, encode(t, observation))
			}
			var readiness struct{ Reason *string }
			if err := json.Unmarshal(report["workerReadiness"], &readiness); err != nil {
				t.Fatal(err)
			}
			if !sameReason(readiness.Reason, want) {
				t.Errorf("doctor's readiness answered %v, Python's observation %v", show(readiness.Reason), show(want))
			}
		})
	}
}

func observed(observation contract.OrderedObject) *string {
	for _, field := range observation {
		if field.Key == "reason" {
			if reason, ok := field.Value.(string); ok {
				return &reason
			}
		}
	}
	return nil
}

func nullable(reason string) *string {
	if reason == "" {
		return nil
	}
	return &reason
}

func sameReason(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

func show(reason *string) string {
	if reason == nil {
		return "observed"
	}
	return *reason
}

func encode(t *testing.T, value any) string {
	t.Helper()
	var out bytes.Buffer
	if err := contract.Emit(&out, value); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(out.String(), "\n")
}

// reindent is value as it sits one level into doctor's report.
func reindent(t *testing.T, value string) string {
	t.Helper()
	return strings.ReplaceAll(value, "\n", "\n  ")
}

// managed-start's own readiness over the live observation answers the readiness reasons
// Python's worker_readiness gives for the same change (the "readiness" golden cases).
func TestWorkerPolicy_managed_start_readiness_is_pythons(t *testing.T) {
	golden := loadWorkerGolden(t)
	settings := func(model string) map[string]any {
		return map[string]any{"settings": map[string]any{"model": model, "reasoningEffort": "xhigh"}}
	}
	request := func(parentModel string) map[string]any {
		return map[string]any{"parent": settings(parentModel), "child": settings("anthropic/claude-opus-5-5")}
	}
	receiptPolicy := func(change func(policy map[string]any)) func(f *workerFixture) {
		return func(f *workerFixture) {
			f.edit(f.receiptPath, func(r map[string]any) { change(r["policy"].(map[string]any)) })
		}
	}
	for name, c := range map[string]struct {
		change       func(f *workerFixture)
		parentModel  string
		callerPolicy bool
	}{
		"ready":             {func(*workerFixture) {}, "anthropic/claude-opus-5-5", true},
		"worker-unresolved": {receiptPolicy(func(p map[string]any) { p["state"] = "unresolved" }), "anthropic/claude-opus-5-5", true},
		"caller-unresolved": {func(*workerFixture) {}, "anthropic/claude-opus-5-5", false},
		"digest-mismatch":   {receiptPolicy(func(p map[string]any) { p["digest"] = strings.Repeat("0", 64) }), "anthropic/claude-opus-5-5", true},
		"summary-mismatch": {receiptPolicy(func(p map[string]any) {
			p["roles"].(map[string]any)["parent"].(map[string]any)["model"] = "not-the-declared-model"
		}), "anthropic/claude-opus-5-5", true},
		"pair-mismatch-model": {func(*workerFixture) {}, "other-model", true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newWorkerFixture(t, golden.Policy)
			c.change(f)
			caller := registry.ResolveRolePolicy(map[string]string{})
			if c.callerPolicy {
				caller = registry.ResolveRolePolicy(map[string]string{execution.EnvPolicy: f.policyFile})
			}
			reason, err := f.observer.Ready(context.Background(), request(c.parentModel), caller)
			if err != nil {
				t.Fatal(err)
			}
			if want := golden.Readiness[name]; !sameReason(nullable(reason), want) {
				t.Fatalf("managed-start readiness %q, Python %v", reason, show(want))
			}
		})
	}
}
