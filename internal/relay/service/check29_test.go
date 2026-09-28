//go:build linux

package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Plain stops are evidence, not a pairwise timing oracle (decision 27). Python
// may observe its leader as exited before its other threads release daemon.lock.
// The controlled reaped/zombie tests own deterministic equality assertions.
func Test29D1PlainRoundTripTwenty(t *testing.T) {
	counts := map[bool]map[string]int{true: {}, false: {}}
	oracle := map[capture]bool{}
	type comparison struct {
		trial    int
		pythonOK bool
		goResult capture
	}
	var comparisons []comparison
	defer func() {
		python, _ := json.Marshal(counts[true])
		goDist, _ := json.Marshal(counts[false])
		t.Logf("decision 27 plain stop distributions: Python %s Go %s", python, goDist)
	}()
	for trial := 0; trial < 20; trial++ {
		t.Run(fmt.Sprint(trial), func(t *testing.T) {
			pythonOK := false
			for _, python := range []bool{true, false} {
				t.Run(fmt.Sprint(python), func(t *testing.T) {
					// No state, scope inode or cleanup is shared across runtimes or
					// iterations. t.Run completes process() cleanup before returning.
					home := t.TempDir()
					supervisor, worker := startServing(t, home, python)
					result := invoke(t, home, python, "--socket", home+"/socket", "service", "stop")
					shape, _ := json.Marshal(capture{Out: normalize(result.Out), Err: result.Err, Code: result.Code})
					counts[python][string(shape)]++
					t.Logf("stop=%s supervisorExitObserved=%v workerExitObserved=%v state=%s", shape, supervisor.Wait(0), worker.Wait(0), home)
					answer, _ := parse([]byte(result.Out))
					if python {
						pythonOK = result.Code == 0 && get(answer, "ok") == true
						oracle[plainStopShape(result)] = true
						if !pythonOK {
							t.Log("Python non-ok outcome: omit this iteration's Go comparison")
						}
					} else {
						comparisons = append(comparisons, comparison{trial, pythonOK, result})
					}
				})
			}
		})
	}
	// Check against all Python observations from this run, not just the ones
	// scheduled before a particular Go launch. The only allowed shape variation
	// is worker gone/exited; every other field, stderr and exit stays significant.
	for _, one := range comparisons {
		if err := plainStopProblem(one.pythonOK, one.goResult, oracle); err != nil {
			t.Errorf("iteration %d: %v", one.trial, err)
		}
	}
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
func Test29D2DeclaredRoleOrder(t *testing.T) {
	home := t.TempDir()
	var wantPolicy, wantDoctor string
	for _, python := range []bool{true, false} {
		t.Run(fmt.Sprint(python), func(t *testing.T) {
			declarePolicy(t, home, python)
			startServing(t, home, python)
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
			doctor := runtimeObject(t, invoke(t, home, python, "--socket", home+"/socket", "doctor"))
			observed, _ := get(doctor, "workerPolicy").(Object)
			doctorPolicy, _ := get(observed, "policy").(Object)
			encodedDoctor, err := encoded(doctorPolicy)
			if err != nil {
				t.Fatal(err)
			}
			if python {
				wantPolicy = string(encodedPolicy)
				wantDoctor = string(encodedDoctor)
			} else if string(encodedPolicy) != wantPolicy || string(encodedDoctor) != wantDoctor {
				t.Fatalf("worker policy order\nPython %s\nGo %s\ndoctor Python %s\nGo %s", wantPolicy, encodedPolicy, wantDoctor, encodedDoctor)
			}
			// startServing registered pidfd cleanup. Ordinary stop timing is not
			// part of the declared-role-order contract (decision 27).
		})
		resetRuntime(t, home)
	}
}
func Test29D3LaunchSnapshotTwenty(t *testing.T) {
	counts := map[bool]map[string]map[string]int{
		true: {"start": {}, "restart": {}}, false: {"start": {}, "restart": {}},
	}
	oracle := map[string]map[capture]bool{"start": {}, "restart": {}}
	type comparison struct {
		trial    int
		action   string
		pythonOK map[string]bool
		result   capture
	}
	var comparisons []comparison
	defer func() {
		python, _ := json.Marshal(counts[true])
		goDist, _ := json.Marshal(counts[false])
		t.Logf("decision 27 launch snapshot distributions: Python %s Go %s", python, goDist)
	}()
	for trial := 0; trial < 20; trial++ {
		t.Run(fmt.Sprint(trial), func(t *testing.T) {
			// Action keys, not an outcome-sized slice: a refused Python start
			// leaves restart absent, and a refused restart has no start object.
			pythonOK := map[string]bool{}
			for _, python := range []bool{true, false} {
				t.Run(fmt.Sprint(python), func(t *testing.T) {
					home := t.TempDir()
					declarePolicy(t, home, python)
					if r := invoke(t, home, python, "service", "enable"); r.Code != 0 {
						t.Fatal(r)
					}
					watch := watchDir(t, filepath.Join(home, "state"))
					for _, action := range []string{"start", "restart"} {
						result := invoke(t, home, python, "--socket", home+"/socket", "service", action, "--allow-isolated-scope", "--segment-seconds", "600")
						shape, ok := launchSnapshotShape(action, result)
						encodedShape, _ := json.Marshal(shape)
						counts[python][action][string(encodedShape)]++
						raw, _ := json.Marshal(result)
						t.Logf("%s answer=%s snapshot=%s state=%s", action, raw, encodedShape, home)
						if python {
							pythonOK[action] = ok
							oracle[action][shape] = true
						} else {
							comparisons = append(comparisons, comparison{trial, action, pythonOK, result})
						}
						if !ok {
							// Log Python refusals as evidence; retain Go refusals for
							// comparison after every Python observation is collected.
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
					// pidfd cleanup completes before the next runtime starts.
				})
			}
			if !pythonOK["start"] || !pythonOK["restart"] {
				t.Logf("Python non-ok or missing launch: %v; omit this iteration's Go comparisons", pythonOK)
			}
		})
	}
	for _, one := range comparisons {
		if err := launchSnapshotProblem(one.pythonOK, one.action, one.result, oracle[one.action]); err != nil {
			t.Errorf("iteration %d %s: %v", one.trial, one.action, err)
		}
	}
}

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
func Test29D4FileModes(t *testing.T) {
	for _, mask := range []int{0002, 0022} {
		t.Run(fmt.Sprintf("%03o", mask), func(t *testing.T) {
			old := unix.Umask(mask)
			defer unix.Umask(old)
			home := t.TempDir()
			var want map[string]os.FileMode
			var pythonStop capture
			for _, python := range []bool{true, false} {
				t.Run(fmt.Sprint(python), func(t *testing.T) {
					declarePolicy(t, home, python)
					startServing(t, home, python)
					// Stop creates stop.request and may replace daemon.json; inspect
					// their modes even if Python reports its leader-exit race.
					result := invoke(t, home, python, "--socket", home+"/socket", "service", "stop")
					t.Logf("mode-check stop: %+v", result)
					if python {
						pythonStop = result
					} else if err := plainStopProblem(pythonStop.Code == 0, result, map[capture]bool{plainStopShape(pythonStop): true}); err != nil {
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
					if python {
						want = modes
					} else if !reflect.DeepEqual(want, modes) {
						t.Fatalf("modes Python %v Go %v", want, modes)
					}
				})
				resetRuntime(t, home)
			}
		})
	}
}
func Test29D5RefusalInitializesStore(t *testing.T) {
	for _, flags := range [][]string{{}, {"--allow-isolated-scope", "--supervised-token", "test-run"}, {"--allow-isolated-scope", "--supervised-token", "test-run", "--supervised-lock-fd", "99", "--supervised-scope-fd", "98"}} {
		t.Run(strings.Join(flags, "_"), func(t *testing.T) {
			home := t.TempDir()
			args := append([]string{"--socket", home + "/socket", "daemon", "--max-ticks", "0"}, flags...)
			var want capture
			var wantTables string
			var wantFiles []string
			for _, python := range []bool{true, false} {
				result := invoke(t, home, python, args...)
				entries := []string{}
				err := filepath.WalkDir(home+"/state", func(path string, entry os.DirEntry, err error) error {
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
				if python {
					want = result
					wantTables = tables(t, home)
					wantFiles = entries
					resetRuntime(t, home)
					if err = os.Remove(home + "/state/relay.sqlite3"); err != nil {
						t.Fatal(err)
					}
				} else {
					compare(t, want, result)
					if !reflect.DeepEqual(wantFiles, entries) || wantTables != tables(t, home) {
						a, _ := json.Marshal(wantFiles)
						b, _ := json.Marshal(entries)
						t.Fatalf("refusal state: Python %s Go %s; full tables equal=%v", a, b, wantTables == tables(t, home))
					}
				}
			}
		})
	}
}
