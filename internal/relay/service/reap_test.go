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
	"os/signal"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// helperEnv makes this test binary one of its helper processes (runHelper) in place of its tests.
const helperEnv = "CRW_SERVICE_TEST_HELPER"

// The helpers run from init, before TestMain, on the main thread: a leader thread that exits
// must be the thread the kernel reports for the process.
func init() {
	if helper := os.Getenv(helperEnv); helper != "" {
		os.Exit(runHelper(helper, os.Getenv(helperEnv+"_ARG")))
	}
}

// runHelper is one helper process, the fixtures that were Python scripts until todo 44:
//
//   - "controller": the parent of a worker it controls waitpid for. A zombie remains unreaped
//     until the test releases stdin; no elapsed-time delay decides which process state the stop
//     command observes. arg is "gone" (reaped before it answers) or "exited" (reaped after).
//   - "idle": a process that stays alive until its stdin is closed and then exits, as cat did.
//     It is the controller's worker and the supervisor reapStop stops, so these tests start no
//     program found on PATH.
//   - "leader-gone": a worker whose leader thread has exited while another thread keeps the
//     descriptor table the threads share, and the daemon lock (arg) in it, until stdin is
//     written: a multithreaded service process on its way out, held there. SIGTERM is ignored,
//     so only SIGKILL ends it. SYS_exit ends the calling thread alone.
func runHelper(role, arg string) int {
	switch role {
	case "controller":
		worker := helper(context.Background(), "idle", "")
		release, err := worker.StdinPipe()
		if err == nil {
			err = worker.Start()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		// Whatever ends the controller before it has waited for the worker, the worker is
		// stopped and reaped rather than left running.
		defer func() {
			if worker.ProcessState == nil {
				_ = worker.Process.Kill()
				_ = worker.Wait()
			}
		}()
		fd, err := unix.PidfdOpen(worker.Process.Pid, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		ticks := StartTicks(worker.Process.Pid)
		_ = release.Close()
		if n, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, 10000); err != nil || n != 1 {
			fmt.Fprintln(os.Stderr, "worker exit not observed", err)
			return 1
		}
		if arg == "gone" {
			_ = worker.Wait()
		}
		raw, _ := json.Marshal(map[string]any{"pid": worker.Process.Pid, "ticks": ticks})
		fmt.Println(string(raw))
		_, _ = os.Stdin.Read(make([]byte, 1))
		if arg == "exited" {
			_ = worker.Wait()
		}
		_ = unix.Close(fd)
		return 0
	case "idle":
		_, _ = io.Copy(io.Discard, os.Stdin)
		return 0
	case "leader-gone":
		lock, err := os.OpenFile(arg, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
		if err == nil {
			err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		signal.Ignore(unix.SIGTERM)
		go func() {
			_, _ = os.Stdin.Read(make([]byte, 1))
			os.Exit(0)
		}()
		fmt.Println(os.Getpid())
		_, _, _ = unix.Syscall(unix.SYS_EXIT, 0, 0, 0)
		return 1
	}
	fmt.Fprintf(os.Stderr, "unknown helper %q\n", role)
	return 2
}

// helper starts one of this binary's helper processes.
func helper(ctx context.Context, name, arg string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"="+name, helperEnv+"_ARG="+arg)
	cmd.Stderr = os.Stderr
	return cmd
}

func controlledWorker(t *testing.T, state string) (int, int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	cmd := helper(ctx, "controller", state)
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

// reapAnswer is one runtime's stop of the controlled worker: its answer, the state files and
// the tables.
type reapAnswer struct {
	Capture capture           `json:"capture"`
	Files   map[string]string `json:"files"`
	Tables  string            `json:"tables"`
}

// reapStop enables the service, publishes a Go record naming a live, independently reaped
// supervisor and a controlled worker in state, and stops it.
func reapStop(t *testing.T, home, state string) reapAnswer {
	t.Helper()
	worker, ticks := controlledWorker(t, state)
	// A live, independently reaped supervisor makes stop take its ordinary
	// termination/re-read path; the worker state is already fixed before that.
	ctx, cancel := context.WithCancel(context.Background())
	// The supervisor is the idle helper: it lives while the test holds its stdin open, and
	// Wait closes that pipe.
	supervisor := helper(ctx, "idle", "")
	_, err := supervisor.StdinPipe()
	if err == nil {
		err = supervisor.Start()
	}
	if err != nil {
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
	enabled := invoke(t, home, "service", "enable")
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
	result := invoke(t, home, "--socket", home+"/socket", "service", "stop")
	answer := runtimeObject(t, result)
	if result.Code != 0 || get(answer, "worker") != state || !handle.Wait(0) {
		t.Fatalf("%s: %+v", state, result)
	}
	// daemon.json is the record s.NewRecord (Go) wrote above; stop only adds to it, so its
	// build is Go's own (null).
	actualFiles := files(t, home)
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
	return reapAnswer{normalizedCapture(result), actualFiles, tables(t, home)}
}

// Test29D1DeterministicReapStates stops a worker that is already gone, or exited and unreaped,
// and checks the answer and persisted state against the golden, which began as the retained
// Python's stop.
func Test29D1DeterministicReapStates(t *testing.T) {
	for _, state := range []string{"gone", "exited"} {
		t.Run(state, func(t *testing.T) {
			home := t.TempDir()
			t.Run("false", func(t *testing.T) {
				checkAnswer(t, home, "answer", reapStop(t, home, state))
			})
		})
	}
}
func installationForBinary(t *testing.T, home string) string {
	t.Helper()
	result := invoke(t, home, "service", "status")
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

// A zombie leader is not an exited process (decisions.md 40). Stop observes a worker's exit on
// its pidfd, which becomes readable only after every thread has closed its descriptors, so the
// daemon lock the remaining thread holds is free when stop probes it. The Python fence read the
// leader's /proc state instead and answered replaced_by_new_launch here, with the thread still
// running; tests/test_service.py holds the same case for it.
func Test29D1StopWaitsForEveryThreadOfTheWorker(t *testing.T) {
	home := t.TempDir()
	if enabled := invoke(t, home, "service", "enable"); enabled.Code != 0 {
		t.Fatal(enabled)
	}
	s, err := New(context.Background(), storeSelection(home), home+"/socket")
	if err != nil {
		t.Fatal(err)
	}
	s.Scope = &ScopeRegistry{Root: home + "/scopes", Authority: "isolated"}
	s.InstallationID = installationForBinary(t, home)
	lock := filepath.Join(home, "state", "daemon.lock")
	cmd := helper(context.Background(), "leader-gone", lock)
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
	if held, err := existingLockHeld(lock); err != nil || !held {
		t.Fatalf("the fixture needs the remaining thread to hold the lock: held=%v err=%v", held, err)
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
	held, err := existingLockHeld(lock)
	if !worker.Wait(0) || get(s.Record(), "workerPid") != nil || err != nil || held {
		t.Fatalf("stopped worker: exited=%v record=%v lockHeld=%v err=%v", worker.Wait(0), s.Record(), held, err)
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
