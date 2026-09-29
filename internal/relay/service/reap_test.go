//go:build linux

package service

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"golang.org/x/sys/unix"
)

// This real subprocess is its worker's parent, and controls waitpid. A zombie
// remains unreaped until the test releases stdin; no elapsed-time delay decides
// which process state the stop command observes.
const reapController = `import json, os, select, sys
read, write = os.pipe()
pid = os.fork()
if pid == 0:
    os.close(write)
    os.read(read, 1)
    os._exit(0)
os.close(read)
fd = os.pidfd_open(pid)
raw = open('/proc/%d/stat' % pid).read().rsplit(')', 1)[1].split()
ticks = int(raw[19])
os.write(write, b'x')
os.close(write)
assert select.select([fd], [], [], 10)[0], 'worker exit not observed'
if sys.argv[1] == 'gone':
    os.waitpid(pid, 0)
print(json.dumps({'pid': pid, 'ticks': ticks}), flush=True)
sys.stdin.read(1)
if sys.argv[1] == 'exited':
    os.waitpid(pid, 0)
os.close(fd)
`

func controlledWorker(t *testing.T, state string) (int, int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	cmd := exec.CommandContext(ctx, filepath.Join(testRoot, ".venv/bin/python"), "-c", reapController, state)
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, e := io.WriteString(input, "x")
		if e != nil {
			t.Error(e)
		}
		_ = input.Close()
		if e = cmd.Wait(); e != nil {
			t.Error(e)
		}
		cancel()
	})
	line, err := bufio.NewReader(output).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var worker struct {
		PID   int
		Ticks int64
	}
	if err = json.Unmarshal(line, &worker); err != nil {
		t.Fatal(err)
	}
	if state == "gone" {
		if got := ProcessState(worker.PID); got != "" {
			t.Fatalf("already-reaped worker has state %q", got)
		}
	} else if got := ProcessState(worker.PID); got != "Z" {
		t.Fatalf("unreaped worker has state %q", got)
	}
	return worker.PID, worker.Ticks
}

func Test29D1DeterministicReapStates(t *testing.T) {
	for _, state := range []string{"gone", "exited"} {
		t.Run(state, func(t *testing.T) {
			home := t.TempDir()
			var want capture
			var wantFiles map[string]string
			var wantTables string
			for _, python := range []bool{true, false} {
				t.Run(fmt.Sprint(python), func(t *testing.T) {
					worker, ticks := controlledWorker(t, state)
					// A live, independently reaped supervisor makes stop take its ordinary
					// termination/re-read path; the worker state is already fixed before that.
					ctx, cancel := context.WithCancel(context.Background())
					supervisor := exec.CommandContext(ctx, "sleep", "60")
					if err := supervisor.Start(); err != nil {
						cancel()
						t.Fatal(err)
					}
					t.Cleanup(func() {
						cancel()
						if err := supervisor.Wait(); err != nil {
							if _, ok := err.(*exec.ExitError); !ok {
								t.Error(err)
							}
						}
					})
					handle := process(t, supervisor.Process.Pid)
					enabled := invoke(t, home, python, "service", "enable")
					if enabled.Code != 0 {
						t.Fatal(enabled)
					}
					s, err := New(context.Background(), storeSelection(home), home+"/socket")
					if err != nil {
						t.Fatal(err)
					}
					s.Scope = &ScopeRegistry{Root: home + "/scopes", Authority: "isolated"}
					s.InstallationID = installationForBinary(t, home)
					record := set(s.NewRecord(supervisor.Process.Pid, "controlled-run"), "workerPid", worker, "workerStartTicks", ticks)
					if err = s.WriteRecord(record); err != nil {
						t.Fatal(err)
					}
					result := invoke(t, home, python, "--socket", home+"/socket", "service", "stop")
					answer := runtimeObject(t, result)
					if result.Code != 0 || get(answer, "worker") != state || !handle.Wait(0) {
						t.Fatalf("%s: %+v", state, result)
					}
					// daemon.json is the record s.NewRecord (Go) wrote above in both runs; either
					// runtime's stop only adds to it, so its build is Go's own (null) on both sides.
					actualFiles := files(t, home, testsupport.Go)
					raw, err := os.ReadFile(home + "/state/stop.request")
					if err != nil {
						t.Fatal(err)
					}
					actualFiles["stop.request"] = normalize(string(raw))
					raw, err = os.ReadFile(home + "/state/daemon.lock")
					if err != nil {
						t.Fatal(err)
					}
					actualFiles["daemon.lock"] = string(raw)
					if python {
						want = result
						wantFiles = actualFiles
						wantTables = tables(t, home, testsupport.Python)
					} else {
						compare(t, want, result)
						if !reflect.DeepEqual(wantFiles, actualFiles) || wantTables != tables(t, home, testsupport.Go) {
							t.Fatalf("persisted state for %s\nPython %v\nGo %v", state, wantFiles, actualFiles)
						}
					}
				})
				resetRuntime(t, home)
			}
		})
	}
}
func installationForBinary(t *testing.T, home string) string {
	t.Helper()
	result := invoke(t, home, false, "service", "status")
	return text(get(runtimeObject(t, result), "installationId"))
}

func Test29D1TerminationCadence(t *testing.T) {
	now := time.Unix(1000, 0)
	reads := 0
	pauses := []time.Duration{}
	ended := terminationCadence(time.Second, func() time.Time { return now }, func() bool { reads++; return reads == 2 }, func(delay time.Duration) { pauses = append(pauses, delay); now = now.Add(delay) })
	if !ended || reads != 2 || len(pauses) != 1 || pauses[0] != 100*time.Millisecond {
		t.Fatalf("cadence: ended=%v reads=%d pauses=%v", ended, reads, pauses)
	}
	reads = 0
	if terminationCadence(0, func() time.Time { return now }, func() bool { reads++; return true }, func(time.Duration) { t.Fatal("spent bound waited") }) || reads != 0 {
		t.Fatal("spent bound observed after its deadline")
	}
}

// leaderGone is a worker whose leader thread has exited while another thread keeps the
// descriptor table the threads share, and the daemon lock in it, until stdin is written: a
// multithreaded service process on its way out, held there. SIGTERM is blocked in every thread,
// so only SIGKILL ends it. SYS_exit (argv[2]) ends the calling thread alone.
const leaderGone = `import ctypes, fcntl, os, signal, sys, threading
lock = open(sys.argv[1], "a+")
fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGTERM})
threading.Thread(target=lambda: (sys.stdin.buffer.read(1), os._exit(0))).start()
print(os.getpid(), flush=True)
ctypes.CDLL(None, use_errno=True).syscall(int(sys.argv[2]), 0)
`

// A zombie leader is not an exited process (decisions.md 40). Stop observes a worker's exit on
// its pidfd, which becomes readable only after every thread has closed its descriptors, so the
// daemon lock the remaining thread holds is free when stop probes it. The Python fence read the
// leader's /proc state instead and answered replaced_by_new_launch here, with the thread still
// running; tests/test_service.py holds the same case for it.
func Test29D1StopWaitsForEveryThreadOfTheWorker(t *testing.T) {
	home := t.TempDir()
	if enabled := invoke(t, home, false, "service", "enable"); enabled.Code != 0 {
		t.Fatal(enabled)
	}
	s, err := New(context.Background(), storeSelection(home), home+"/socket")
	if err != nil {
		t.Fatal(err)
	}
	s.Scope = &ScopeRegistry{Root: home + "/scopes", Authority: "isolated"}
	s.InstallationID = installationForBinary(t, home)
	lock := filepath.Join(home, "state", "daemon.lock")
	cmd := exec.Command(filepath.Join(testRoot, ".venv/bin/python"), "-c", leaderGone, lock, strconv.Itoa(unix.SYS_EXIT))
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	worker := process(t, cmd.Process.Pid)
	t.Cleanup(func() { _ = input.Close(); _ = cmd.Wait() })
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != fmt.Sprintf("%d\n", worker.PID) {
		t.Fatalf("worker readiness %q: %v", line, err)
	}
	for end := time.Now().Add(10 * time.Second); ProcessState(worker.PID) != "Z"; {
		if time.Now().After(end) {
			t.Fatal("the worker's leader never exited")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !existingLockHeld(lock) {
		t.Fatal("the fixture needs the remaining thread to hold the lock")
	}
	record := set(s.NewRecord(os.Getpid(), "controlled-run"), "pid", nil, "workerPid", worker.PID, "workerStartTicks", StartTicks(worker.PID))
	if err = s.WriteRecord(record); err != nil {
		t.Fatal(err)
	}
	stopped, err := s.Stop("owner", 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	want := obj("ok", true, "reason", nil, "detail", nil, "supervisor", "gone", "worker", "exited")
	if !sameObject(stopped, want) {
		t.Fatalf("stop of a worker whose leader has exited: %v", stopped)
	}
	if !worker.Wait(0) || get(s.Record(), "workerPid") != nil || existingLockHeld(lock) {
		t.Fatalf("stopped worker: exited=%v record=%v lockHeld=%v", worker.Wait(0), s.Record(), existingLockHeld(lock))
	}
}

// Keep a direct check that the cadence implementation recognizes pidfd exit.
func Test29D1GraceObservesZombie(t *testing.T) {
	pid, _ := controlledWorker(t, "exited")
	h := OpenProcess(pid)
	defer h.Close()
	if !h.Send(unix.SIGTERM) || !waitTermination(h, time.Second) {
		t.Fatal("zombie was not an observed exit")
	}
}
