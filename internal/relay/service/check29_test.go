//go:build linux

package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"golang.org/x/sys/unix"
)

// Plain stops are evidence, not a pairwise timing oracle (decision 27). Python
// may observe its leader as exited before its other threads release daemon.lock.
// The controlled reaped/zombie tests own deterministic equality assertions.
// Each Go round trip is judged against pythonPlainStops, the stop the retained Python
// service answered in every one of its runs (the Python halves that started a Python service
// here left with the Python runtime, todo 44).
func Test29D1PlainRoundTripTwenty(t *testing.T) {
	counts := map[string]int{}
	defer func() {
		goDist, _ := json.Marshal(counts)
		t.Logf("decision 27 plain stop distribution: Go %s", goDist)
	}()
	for trial := 0; trial < 20; trial++ {
		t.Run(fmt.Sprint(trial), func(t *testing.T) {
			// No state, scope inode or cleanup is shared across iterations. t.Run completes
			// process() cleanup before returning.
			home := t.TempDir()
			supervisor, worker := startServing(t, home, false)
			result := invoke(t, home, false, "--socket", home+"/socket", "service", "stop")
			shape, _ := json.Marshal(capture{Out: normalize(result.Out), Err: result.Err, Code: result.Code})
			counts[string(shape)]++
			t.Logf("stop=%s supervisorExitObserved=%v workerExitObserved=%v state=%s", shape, supervisor.Wait(0), worker.Wait(0), home)
			if err := plainStopProblem(true, result, pythonPlainStops); err != nil {
				t.Errorf("iteration %d: %v", trial, err)
			}
		})
	}
}

// pythonPlainStops is the plain stop shape the retained Python service answered: a live
// supervisor stopped, its worker gone or exited, every one of the 20 Python round trips
// Test29D1PlainRoundTripTwenty observed while it ran them (decision 27 distribution).
var pythonPlainStops = map[capture]bool{
	plainStopShape(capture{Out: "{\n  \"ok\": true,\n  \"reason\": null,\n  \"detail\": null,\n  \"supervisor\": \"exited\",\n  \"worker\": \"gone\"\n}\n"}): true,
}

func plainStopShape(result capture) capture {
	answer, err := parse([]byte(result.Out))
	if err == nil && (get(answer, "worker") == "gone" || get(answer, "worker") == "exited") {
		answer = set(answer, "worker", "gone-or-exited")
		if raw, err := encoded(answer); err == nil {
			result.Out = string(raw)
		}
	}
	result.Out = normalize(result.Out)
	return result
}

func plainStopProblem(pythonOK bool, result capture, oracle map[capture]bool) error {
	answer, err := parse([]byte(result.Out))
	if err != nil || (get(answer, "worker") != "gone" && get(answer, "worker") != "exited") {
		return fmt.Errorf("Go worker is outside {gone,exited}: %+v", result)
	}
	if !pythonOK {
		return nil
	}
	if result.Code != 0 || get(answer, "ok") != true {
		return fmt.Errorf("Go non-ok stop after Python ok: %+v", result)
	}
	if !oracle[plainStopShape(result)] {
		return fmt.Errorf("Go stop shape was never produced by Python in this run: %+v", result)
	}
	return nil
}

func Test29D1EvidenceComparisonRules(t *testing.T) {
	python := capture{Out: `{"ok":true,"reason":null,"detail":null,"supervisor":"exited","worker":"gone"}`}
	exited := capture{Out: `{"ok":true,"reason":null,"detail":null,"supervisor":"exited","worker":"exited"}`}
	replaced := capture{Code: 2, Out: `{"ok":false,"reason":"replaced_by_new_launch","detail":"lock held","supervisor":"exited","worker":"exited"}`}
	badWorker := capture{Out: `{"ok":true,"reason":null,"detail":null,"supervisor":"exited","worker":"untouched"}`}
	unseen := capture{Out: `{"ok":true,"reason":null,"detail":null,"supervisor":"gone","worker":"gone"}`}
	oracle := map[capture]bool{plainStopShape(python): true, plainStopShape(replaced): true}
	for _, one := range []struct {
		name     string
		pythonOK bool
		result   capture
		refused  bool
	}{
		{"gone", true, python, false}, {"exited", true, exited, false},
		{"python_non_ok", false, replaced, false}, {"python_non_ok_unseen", false, unseen, false},
		{"go_non_ok", true, replaced, true}, {"go_unseen", true, unseen, true},
		{"worker_invalid", true, badWorker, true}, {"worker_invalid_after_python_non_ok", false, badWorker, true},
		{"stderr", true, capture{Out: python.Out, Err: "unexpected"}, true},
	} {
		t.Run(one.name, func(t *testing.T) {
			if err := plainStopProblem(one.pythonOK, one.result, oracle); (err != nil) != one.refused {
				t.Fatalf("refused=%v error=%v", one.refused, err)
			}
		})
	}
}

func declarePolicy(t *testing.T, home string, python bool) {
	t.Helper()
	path := filepath.Join(home, "policy.json")
	// Deliberately neither alphabetic nor the fixed supervisor/parent/child order.
	raw := `{"roles":{"parent":{"model":"test-model","reasoningEffort":"high"},"supervisor":{"expectation":"record"},"child":{"model":"test-model","reasoningEffort":"high"}}}`
	if err := os.WriteFile(path, []byte(raw), 0666); err != nil {
		t.Fatal(err)
	}
	if result := invoke(t, home, python, "service", "declare", "--execution-policy", path); result.Code != 0 {
		t.Fatal(result)
	}
}

// Test29D2DeclaredRoleOrder: the worker receipt and doctor keep the declared role order, which is
// neither alphabetic nor the fixed supervisor/parent/child order. declaredRoleOrder is what the
// retained Python service published for the same declaration (its half left with the Python
// runtime, todo 44).
func Test29D2DeclaredRoleOrder(t *testing.T) {
	home := t.TempDir()
	declarePolicy(t, home, false)
	startServing(t, home, false)
	raw, err := os.ReadFile(filepath.Join(home, "state", "worker-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := get(receipt, "policy").(Object)
	encodedPolicy, err := encoded(policy)
	if err != nil {
		t.Fatal(err)
	}
	doctor := runtimeObject(t, invoke(t, home, false, "--socket", home+"/socket", "doctor"))
	observed, _ := get(doctor, "workerPolicy").(Object)
	doctorPolicy, _ := get(observed, "policy").(Object)
	encodedDoctor, err := encoded(doctorPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if string(encodedPolicy) != declaredRoleOrder || string(encodedDoctor) != declaredRoleOrder {
		t.Fatalf("worker policy order\nPython %s\nGo %s\ndoctor Go %s", declaredRoleOrder, encodedPolicy, encodedDoctor)
	}
	// startServing registered pidfd cleanup. Ordinary stop timing is not
	// part of the declared-role-order contract (decision 27).
}

// declaredRoleOrder is the worker receipt's policy, and doctor's, for declarePolicy's file: the
// roles in the order the file declares them.
const declaredRoleOrder = `{
  "state": "declared",
  "digest": "acb317d3704ea1d3ce896df584d310b221f0763b4243f0ce8b8d4712e91c3c08",
  "roles": {
    "parent": {
      "role": "parent",
      "expectation": "pair",
      "model": "test-model",
      "reasoningEffort": "high"
    },
    "supervisor": {
      "role": "supervisor",
      "expectation": "record",
      "model": null,
      "reasoningEffort": null
    },
    "child": {
      "role": "child",
      "expectation": "pair",
      "model": "test-model",
      "reasoningEffort": "high"
    }
  },
  "detail": null
}`

// Test29D3LaunchSnapshotTwenty judges twenty Go start/restart snapshots against
// pythonLaunchSnapshots, the snapshot the retained Python service answered in every one of its
// runs (its halves left with the Python runtime, todo 44).
func Test29D3LaunchSnapshotTwenty(t *testing.T) {
	counts := map[string]map[string]int{"start": {}, "restart": {}}
	defer func() {
		goDist, _ := json.Marshal(counts)
		t.Logf("decision 27 launch snapshot distribution: Go %s", goDist)
	}()
	ok := map[string]bool{"start": true, "restart": true}
	for trial := 0; trial < 20; trial++ {
		t.Run(fmt.Sprint(trial), func(t *testing.T) {
			home := t.TempDir()
			declarePolicy(t, home, false)
			if r := invoke(t, home, false, "service", "enable"); r.Code != 0 {
				t.Fatal(r)
			}
			watch := watchDir(t, filepath.Join(home, "state"))
			for _, action := range []string{"start", "restart"} {
				result := invoke(t, home, false, "--socket", home+"/socket", "service", action, "--allow-isolated-scope", "--segment-seconds", "600")
				shape, launched := launchSnapshotShape(action, result)
				encodedShape, _ := json.Marshal(shape)
				counts[action][string(encodedShape)]++
				raw, _ := json.Marshal(result)
				t.Logf("%s answer=%s snapshot=%s state=%s", action, raw, encodedShape, home)
				if err := launchSnapshotProblem(ok, action, result, pythonLaunchSnapshots); err != nil {
					t.Errorf("iteration %d %s: %v", trial, action, err)
				}
				if !launched {
					t.Logf("%s non-ok launch; no dependent actions", action)
					break
				}
				answer := runtimeObject(t, result)
				if action == "restart" {
					answer, _ = get(answer, "start").(Object)
				}
				supervisor := process(t, num(get(answer, "pid")))
				// Subscribe before starting; wait only AFTER recording the
				// response snapshot, never to manufacture its readiness.
				var record Object
				watch.until(t, func() bool {
					record = read(filepath.Join(home, "state", "daemon.json"))
					receipt := read(filepath.Join(home, "state", "worker-policy.json"))
					run, _ := get(receipt, "service").(Object)
					return equal(get(run, "pid"), supervisor.PID)
				})
				process(t, num(get(record, "workerPid")))
			}
			// pidfd cleanup completes before the next iteration starts.
		})
	}
}

// pythonLaunchSnapshots is the readiness snapshot the retained Python service answered to
// declarePolicy's start and restart in each of the 20 runs Test29D3LaunchSnapshotTwenty
// observed while it ran them (decision 27 distribution): launched, the worker's own digest not
// yet published.
var pythonLaunchSnapshots = func() map[capture]bool {
	raw, err := encoded(obj("ok", true, "reason", nil, "launchOK", true, "launchReason", nil, "runningDigest", nil, "matchesRunning", "unknown"))
	if err != nil {
		panic(err)
	}
	return map[capture]bool{{Out: string(raw)}: true}
}()

// D3 compares the readiness snapshot, not paths, process identities or the
// independently scheduled restart stop outcome. Complete answers are logged.
func launchSnapshotShape(action string, result capture) (capture, bool) {
	answer, err := parse([]byte(result.Out))
	launch := answer
	if action == "restart" {
		launch, _ = get(answer, "start").(Object)
	}
	ok := err == nil && result.Code == 0 && get(answer, "ok") == true && get(launch, "ok") == true
	if !ok {
		result.Out = normalize(result.Out)
		return result, false
	}
	status, _ := get(launch, "status").(Object)
	policy, _ := get(status, "launchPolicy").(Object)
	shape := obj("ok", get(answer, "ok"), "reason", get(answer, "reason"),
		"launchOK", get(launch, "ok"), "launchReason", get(launch, "reason"),
		"runningDigest", get(policy, "runningDigest"), "matchesRunning", get(policy, "matchesRunning"))
	raw, err := encoded(shape)
	if err != nil {
		return result, false
	}
	result.Out = string(raw)
	return result, true
}

func launchSnapshotProblem(pythonOK map[string]bool, action string, result capture, oracle map[capture]bool) error {
	// Missing map entries are non-ok oracle observations, never slice indexes.
	if !pythonOK["start"] || !pythonOK["restart"] {
		return nil
	}
	shape, ok := launchSnapshotShape(action, result)
	if !ok {
		return fmt.Errorf("Go non-ok launch after Python ok: %+v", result)
	}
	if !oracle[shape] {
		return fmt.Errorf("Go launch snapshot was never produced by Python in this run: %+v", result)
	}
	return nil
}

func Test29D3EvidenceComparisonRules(t *testing.T) {
	start := capture{Out: `{"ok":true,"reason":null,"status":{"launchPolicy":{"runningDigest":null,"matchesRunning":false}}}`}
	restart := capture{Out: `{"ok":true,"reason":null,"start":` + start.Out + `}`}
	refused := capture{Code: 2, Out: `{"ok":false,"reason":"replaced_by_new_launch"}`}
	published := capture{Out: `{"ok":true,"reason":null,"status":{"launchPolicy":{"runningDigest":"digest","matchesRunning":true}}}`}
	startShape, _ := launchSnapshotShape("start", start)
	restartShape, _ := launchSnapshotShape("restart", restart)
	oracle := map[capture]bool{startShape: true, restartShape: true}
	ok := map[string]bool{"start": true, "restart": true}
	for _, one := range []struct {
		name     string
		pythonOK map[string]bool
		action   string
		result   capture
		refused  bool
	}{
		{"start", ok, "start", start, false},
		{"restart", ok, "restart", restart, false},
		{"missing_restart", map[string]bool{"start": true}, "restart", restart, false},
		{"missing_both", nil, "start", refused, false},
		{"python_non_ok", map[string]bool{"start": true, "restart": false}, "restart", refused, false},
		{"python_non_ok_unseen", map[string]bool{"start": true, "restart": false}, "start", published, false},
		{"go_non_ok", ok, "restart", refused, true},
		{"missing_restart_start_object", ok, "restart", start, true},
		{"go_unseen", ok, "start", published, true},
		{"stderr", ok, "start", capture{Out: start.Out, Err: "unexpected"}, true},
	} {
		t.Run(one.name, func(t *testing.T) {
			if err := launchSnapshotProblem(one.pythonOK, one.action, one.result, oracle); (err != nil) != one.refused {
				t.Fatalf("refused=%v error=%v", one.refused, err)
			}
		})
	}
}

// Test29D4FileModes: under a umask that grants group or other access the service's files have the
// modes the retained Python service created them with, 0666 under the umask (its halves left with
// the Python runtime, todo 44).
func Test29D4FileModes(t *testing.T) {
	for _, mask := range []int{0002, 0022} {
		t.Run(fmt.Sprintf("%03o", mask), func(t *testing.T) {
			old := unix.Umask(mask)
			defer unix.Umask(old)
			home := t.TempDir()
			declarePolicy(t, home, false)
			startServing(t, home, false)
			// Stop creates stop.request and may replace daemon.json; inspect their modes.
			result := invoke(t, home, false, "--socket", home+"/socket", "service", "stop")
			t.Logf("mode-check stop: %+v", result)
			if err := plainStopProblem(true, result, pythonPlainStops); err != nil {
				t.Error(err)
			}
			modes := map[string]os.FileMode{}
			for _, name := range []string{"daemon.json", "daemon.log", "daemon.lock", "service.json", "worker-policy.json", "stop.request", "launch-policy.json"} {
				info, err := os.Stat(filepath.Join(home, "state", name))
				if err != nil {
					t.Fatal(err)
				}
				modes[name] = info.Mode().Perm()
			}
			scopes, err := os.ReadDir(home + "/scopes")
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range scopes {
				info, err := entry.Info()
				if err != nil {
					t.Fatal(err)
				}
				modes["scope"+filepath.Ext(entry.Name())] = info.Mode().Perm()
			}
			want := map[string]os.FileMode{}
			for _, name := range pythonFileModes {
				want[name] = os.FileMode(0o666 &^ mask)
			}
			if !reflect.DeepEqual(want, modes) {
				t.Fatalf("modes Python %v Go %v", want, modes)
			}
		})
	}
}

// pythonFileModes are the service files the retained Python service left, each created 0666
// under the umask, as Go creates them.
var pythonFileModes = []string{"daemon.json", "daemon.lock", "daemon.log", "launch-policy.json", "scope.json", "scope.lock", "service.json", "stop.request", "worker-policy.json"}

// d5Answer is a refused daemon's answer, the files it left in the state directory (nil when it
// left no state directory) and its tables.
type d5Answer struct {
	Capture capture  `json:"capture"`
	Entries []string `json:"entries"`
	Tables  string   `json:"tables"`
}

// stateEntries is every file under home/state by base name, nil when there is no state directory.
func stateEntries(t *testing.T, home string) []string {
	t.Helper()
	// A refusal that leaves no state directory at all is a state of its own (nil), distinct
	// from an empty one, and still compared across runtimes.
	var entries []string
	if _, err := os.Stat(home + "/state"); !errors.Is(err, os.ErrNotExist) {
		entries = []string{}
		err = filepath.WalkDir(home+"/state", func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				entries = append(entries, filepath.Base(path))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return entries
}

// Test29D5RefusalInitializesStore: a refused daemon leaves the state its golden, which began as
// the retained Python's, holds.
func Test29D5RefusalInitializesStore(t *testing.T) {
	for _, flags := range [][]string{{}, {"--allow-isolated-scope", "--supervised-token", "test-run"}, {"--allow-isolated-scope", "--supervised-token", "test-run", "--supervised-lock-fd", "99", "--supervised-scope-fd", "98"}} {
		t.Run(strings.Join(flags, "_"), func(t *testing.T) {
			home := t.TempDir()
			args := append([]string{"--socket", home + "/socket", "daemon", "--max-ticks", "0"}, flags...)
			result := invoke(t, home, false, args...)
			checkAnswer(t, home, "answer", d5Answer{normalizedCapture(result), stateEntries(t, home), tables(t, home, testsupport.Go)})
		})
	}
}
