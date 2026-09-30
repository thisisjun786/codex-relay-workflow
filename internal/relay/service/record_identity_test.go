//go:build linux

package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// runProgram runs the Go console entry point with nothing prepared: the state directory stays
// exactly as the previous command left it, whoever owns it.
func runProgram(t *testing.T, home string, args ...string) capture {
	t.Helper()
	program := filepath.Join(filepath.Dir(testBinary), "codex-session-relay")
	cmd := exec.Command(program, append([]string{"--state", home + "/state"}, args...)...)
	cmd.Env = environment(home)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
	}
	return capture{out.String(), stderr.String(), cmd.ProcessState.ExitCode()}
}

func firstField(t *testing.T, o Object, what string) (string, any) {
	t.Helper()
	if len(o) == 0 {
		t.Fatalf("%s: empty record", what)
	}
	return o[0].Key, o[0].Value
}

// The fence's process records name the Python fence build a process runs: first in daemon.json
// and the scope registration, last in worker-policy.json's worker (service.py new_record,
// publish_worker_policy). Go writes the same key in the same place with null, a Go process being
// no fence build (decisions.md 31), running and once stopped. That the retained Python fence read
// what Go wrote (status, doctor, stop and a Python start over the Go records) was checked here
// until todo 44: rollback to Python closed at todo 43 (rollback_allowed=0), and the Python runtime
// leaves in todo 44.
func Test29GoRecordsCarryANullFenceBuildThePythonFenceReads(t *testing.T) {
	home := t.TempDir()
	socket := home + "/socket"
	supervisor, worker := startServing(t, home, false)
	record := read(filepath.Join(home, "state", "daemon.json"))
	scope := (&ScopeRegistry{Root: home + "/scopes", Authority: "isolated"}).Read(socket)
	receipt := read(filepath.Join(home, "state", "worker-policy.json"))
	workerIdentity, _ := get(receipt, "worker").(Object)
	for what, o := range map[string]Object{"daemon.json": record, "scope registration": scope} {
		if key, value := firstField(t, o, what); key != "python_compatibility_build" || value != nil {
			t.Fatalf("%s starts with %s=%v, want python_compatibility_build=null: %v", what, key, value, o)
		}
	}
	if n := len(workerIdentity); n == 0 || workerIdentity[n-1].Key != "python_compatibility_build" || workerIdentity[n-1].Value != nil {
		t.Fatalf("worker-policy.json worker %v: want python_compatibility_build=null last", workerIdentity)
	}
	if stop := runProgram(t, home, "--socket", socket, "service", "stop"); stop.Code != 0 || get(runtimeObject(t, stop), "ok") != true {
		t.Fatalf("Go stop: %+v", stop)
	}
	if !supervisor.Wait(10*time.Second) || !worker.Wait(10*time.Second) {
		t.Fatal("the Go service outlived its stop")
	}
	stopped := read(filepath.Join(home, "state", "daemon.json"))
	if key, value := firstField(t, stopped, "stopped daemon.json"); key != "python_compatibility_build" || value != nil || get(stopped, "pid") != nil || get(stopped, "storeId") != get(record, "storeId") {
		t.Fatalf("stopped Go record: %v", stopped)
	}
}

// A supervisor opens its store in recovery, which a run whose bound is already spent never
// reaches (cli.py _supervise recover, service.py supervise): on an absent store neither runtime
// creates one, and both records keep the null identity read at start. A run with a bound left
// creates the store and publishes its identity into daemon.json and the scope registration.
// (The Python runs of the same bounds left with the Python runtime, todo 44.)
func Test29SpentServiceRunCreatesNoStore(t *testing.T) {
	for _, bound := range [][]string{{"--deadline", "0"}, {"--deadline-monotonic", "0"}, {"--max-segments", "0"}} {
		spent := bound[0] != "--max-segments"
		t.Run("go"+bound[0], func(t *testing.T) {
			home := t.TempDir()
			socket := home + "/socket"
			if r := runProgram(t, home, "service", "enable"); r.Code != 0 {
				t.Fatal(r)
			}
			run := runProgram(t, home, append([]string{"--socket", socket, "service", "run", "--allow-isolated-scope"}, bound...)...)
			if run.Code != 0 || get(runtimeObject(t, run), "ok") != true {
				t.Fatalf("service run: %+v", run)
			}
			record := read(filepath.Join(home, "state", "daemon.json"))
			scope := (&ScopeRegistry{Root: home + "/scopes", Authority: "isolated"}).Read(socket)
			if record == nil || scope == nil {
				t.Fatalf("records: %v %v", record, scope)
			}
			database := filepath.Join(home, "state", "relay.sqlite3")
			if spent {
				for _, name := range []string{"relay.sqlite3", "takeover.json", "write-gate.lock"} {
					if _, err := os.Lstat(filepath.Join(home, "state", name)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("a spent run left %s: %v", name, err)
					}
				}
				if get(record, "storeId") != nil || get(scope, "storeId") != nil {
					t.Fatalf("a spent run recorded a store: %v %v", get(record, "storeId"), get(scope, "storeId"))
				}
				return
			}
			stamp, err := ownership.SnapshotMeta(context.Background(), database)
			if err != nil {
				t.Fatal(err)
			}
			if stamp.StoreID == "" || get(record, "storeId") != stamp.StoreID || get(scope, "storeId") != stamp.StoreID {
				t.Fatalf("published identity %v %v, store %s", get(record, "storeId"), get(scope, "storeId"), stamp.StoreID)
			}
		})
	}
}
