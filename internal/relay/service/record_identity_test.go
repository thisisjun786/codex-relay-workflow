//go:build linux

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// runProgram runs one runtime's console entry point with nothing prepared: the state directory
// stays exactly as the previous command left it, whoever owns it.
func runProgram(t *testing.T, home string, python bool, args ...string) capture {
	t.Helper()
	program := filepath.Join(filepath.Dir(testBinary), "codex-session-relay")
	if python {
		program = testPython
	}
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
// no fence build (decisions.md 31), and the retained Python fence reads what Go wrote: service
// status and doctor answer a running Go service, service stop is refused by the fence's own
// admission of a Go-owned store (the Go runtime stops its daemon), and after a completed takeover
// to Python the stopped Go records are read by status, stop, doctor and a Python start.
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

	status := runProgram(t, home, true, "--socket", socket, "service", "status")
	answer := runtimeObject(t, status)
	if status.Code != 0 || get(answer, "running") != true || num(get(answer, "pid")) != supervisor.PID || num(get(answer, "workerPid")) != worker.PID || get(answer, "storeId") != get(record, "storeId") {
		t.Fatalf("Python status of the running Go service: %+v", status)
	}
	doctor := runProgram(t, home, true, "--socket", socket, "doctor")
	var report struct {
		Ownership struct {
			Owner     string         `json:"owner"`
			Processes map[string]any `json:"processes"`
		} `json:"ownership"`
	}
	if err := json.Unmarshal([]byte(doctor.Out), &report); err != nil || doctor.Code != 0 {
		t.Fatalf("Python doctor of the running Go service: %v %+v", err, doctor)
	}
	if report.Ownership.Owner != "go" || len(report.Ownership.Processes) != 2 || report.Ownership.Processes["supervisor"] != nil || report.Ownership.Processes["worker"] != nil {
		t.Fatalf("Python doctor ownership: %+v", report.Ownership)
	}
	refused := runProgram(t, home, true, "--socket", socket, "service", "stop")
	if refused.Code != 2 || get(runtimeObject(t, refused), "reason") != "store_owned_by_other" || worker.Wait(0) || supervisor.Wait(0) {
		t.Fatalf("Python stop of a Go-owned service: %+v", refused)
	}

	if stop := runProgram(t, home, false, "--socket", socket, "service", "stop"); stop.Code != 0 || get(runtimeObject(t, stop), "ok") != true {
		t.Fatalf("Go stop: %+v", stop)
	}
	if !supervisor.Wait(10*time.Second) || !worker.Wait(10*time.Second) {
		t.Fatal("the Go service outlived its stop")
	}
	func() {
		// The scope key the mirror records is checked against the registry both runtimes use.
		defer inRuntimeScope(t, home)()
		testsupport.HandOver(t, filepath.Join(home, "state", "relay.sqlite3"), "python")
	}()
	stopped := read(filepath.Join(home, "state", "daemon.json"))
	if key, value := firstField(t, stopped, "stopped daemon.json"); key != "python_compatibility_build" || value != nil || get(stopped, "pid") != nil {
		t.Fatalf("stopped Go record: %v", stopped)
	}
	status = runProgram(t, home, true, "--socket", socket, "service", "status")
	if answer = runtimeObject(t, status); status.Code != 0 || get(answer, "running") != false || get(answer, "pid") != nil || get(answer, "storeId") != get(stopped, "storeId") {
		t.Fatalf("Python status of the stopped Go records: %+v", status)
	}
	if doctor = runProgram(t, home, true, "--socket", socket, "doctor"); doctor.Code != 0 || json.Unmarshal([]byte(doctor.Out), &report) != nil || report.Ownership.Owner != "python" || report.Ownership.Processes["supervisor"] != nil {
		t.Fatalf("Python doctor of the stopped Go records: %+v", doctor)
	}
	overGo := runProgram(t, home, true, "--socket", socket, "service", "stop")
	if overGo.Code != 2 || get(runtimeObject(t, overGo), "reason") != "not_running" {
		t.Fatalf("Python stop over the stopped Go records: %+v", overGo)
	}
	supervisor, worker = startServing(t, home, true)
	if restarted := read(filepath.Join(home, "state", "daemon.json")); get(restarted, "python_compatibility_build") != ownership.PythonBuild || num(get(restarted, "pid")) != supervisor.PID || get(restarted, "storeId") != get(stopped, "storeId") {
		t.Fatalf("Python start over the Go records: %v", restarted)
	}
	if stop := runProgram(t, home, true, "--socket", socket, "service", "stop"); stop.Code != 0 || get(runtimeObject(t, stop), "ok") != true {
		t.Fatalf("Python stop: %+v", stop)
	}
	if !supervisor.Wait(10*time.Second) || !worker.Wait(10*time.Second) {
		t.Fatal("the Python service outlived its stop")
	}
	// Python reads the stopped Go records exactly as it reads the ones it leaves itself.
	if overPython := runProgram(t, home, true, "--socket", socket, "service", "stop"); overPython != overGo {
		t.Fatalf("Python stop over stopped records\nGo-written     %+v\nPython-written %+v", overGo, overPython)
	}
}

// A supervisor opens its store in recovery, which a run whose bound is already spent never
// reaches (cli.py _supervise recover, service.py supervise): on an absent store neither runtime
// creates one, and both records keep the null identity read at start. A run with a bound left
// creates the store and publishes its identity into daemon.json and the scope registration.
func Test29SpentServiceRunCreatesNoStore(t *testing.T) {
	for _, python := range []bool{true, false} {
		for _, bound := range [][]string{{"--deadline", "0"}, {"--deadline-monotonic", "0"}, {"--max-segments", "0"}} {
			spent := bound[0] != "--max-segments"
			t.Run(map[bool]string{true: "python", false: "go"}[python]+bound[0], func(t *testing.T) {
				home := t.TempDir()
				socket := home + "/socket"
				if r := runProgram(t, home, python, "service", "enable"); r.Code != 0 {
					t.Fatal(r)
				}
				run := runProgram(t, home, python, append([]string{"--socket", socket, "service", "run", "--allow-isolated-scope"}, bound...)...)
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
}
