//go:build linux

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"golang.org/x/sys/unix"
)

// watch subscribes before an action. Waiting is on directory events, never timing
// luck; predicates also run before blocking to handle events already delivered.
type watch struct{ fd int }

func watchDir(t *testing.T, path string) *watch {
	t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = unix.InotifyAddWatch(fd, path, unix.IN_CLOSE_WRITE|unix.IN_MOVED_TO|unix.IN_CREATE); err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}
	w := &watch{fd}
	t.Cleanup(func() {
		if err := unix.Close(fd); err != nil {
			t.Error(err)
		}
	})
	return w
}
func (w *watch) until(t *testing.T, predicate func() bool) {
	t.Helper()
	end := time.Now().Add(15 * time.Second)
	for {
		if predicate() {
			return
		}
		remaining := time.Until(end)
		if remaining <= 0 {
			t.Fatal("file state did not arrive")
		}
		fds := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(remaining/time.Millisecond))
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatal("file event timed out")
		}
		var b [16384]byte
		_, err = unix.Read(w.fd, b[:])
		if err != nil && err != unix.EAGAIN {
			t.Fatal(err)
		}
	}
}
func process(t *testing.T, pid int) *ProcessHandle {
	t.Helper()
	h := OpenProcess(pid)
	if h.FD < 0 {
		t.Fatalf("pid %d: %s", pid, h.Detail)
	}
	t.Cleanup(func() {
		if !h.Wait(0) {
			h.Send(unix.SIGCONT)
			h.Send(unix.SIGKILL)
			if !h.Wait(5 * time.Second) {
				t.Errorf("process %d outlived cleanup", pid)
			}
		}
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	return h
}
func stopObserved(t *testing.T, h *ProcessHandle) {
	t.Helper()
	if !h.Send(unix.SIGSTOP) {
		t.Fatal(h.Detail)
	}
	end := time.Now().Add(5 * time.Second)
	for ProcessState(h.PID) != "T" {
		if time.Now().After(end) {
			t.Fatal("SIGSTOP not observed in /proc")
		}
	}
}
func runtimeObject(t *testing.T, c capture) Object {
	t.Helper()
	r, err := parse([]byte(c.Out))
	if err != nil {
		t.Fatalf("%v: %+v", err, c)
	}
	return r
}
func startServing(t *testing.T, home string, python bool) (*ProcessHandle, *ProcessHandle) {
	t.Helper()
	enabled := invoke(t, home, python, "service", "enable")
	if enabled.Code != 0 {
		t.Fatal(enabled)
	}
	w := watchDir(t, filepath.Join(home, "state"))
	start := invoke(t, home, python, "--socket", home+"/socket", "service", "start", "--allow-isolated-scope", "--segment-seconds", "600")
	if start.Code != 0 {
		t.Fatal(start)
	}
	supervisor := process(t, num(get(runtimeObject(t, start), "pid")))
	var r Object
	w.until(t, func() bool {
		r = read(filepath.Join(home, "state", "daemon.json"))
		receipt := read(filepath.Join(home, "state", "worker-policy.json"))
		worker, _ := get(receipt, "worker").(Object)
		return truth(get(r, "workerPid")) && equal(get(worker, "pid"), get(r, "workerPid"))
	})
	worker := process(t, num(get(r, "workerPid")))
	s := &Service{Selection: storeSelection(home), Socket: home + "/socket", StoreID: text(get(r, "storeId")), InstallationID: text(get(r, "installationId")), Scope: &ScopeRegistry{Root: home + "/scopes", Authority: "isolated"}}
	if observed := s.ReadWorkerPolicy(context.Background()); get(observed, "observed") != true {
		t.Fatalf("live worker receipt not observable: %v", observed)
	}
	return supervisor, worker
}
func Test29StartStopAndSecondStart(t *testing.T) {
	home := t.TempDir()
	var pythonSecond, pythonStop capture
	var pythonFiles map[string]string
	for _, python := range []bool{true, false} {
		t.Run(fmt.Sprint(python), func(t *testing.T) {
			supervisor, worker := startServing(t, home, python)
			recordPath := filepath.Join(home, "state", "daemon.json")
			before, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatal(err)
			}
			children, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", supervisor.PID, supervisor.PID))
			if err != nil {
				t.Fatal(err)
			}
			second := invoke(t, home, python, "--socket", home+"/socket", "service", "start", "--allow-isolated-scope")
			after, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatal(err)
			}
			nextChildren, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", supervisor.PID, supervisor.PID))
			if err != nil {
				t.Fatal(err)
			}
			if second.Code != 2 || get(runtimeObject(t, second), "reason") != "already_running" || !bytes.Equal(before, after) || !bytes.Equal(children, nextChildren) || worker.Wait(0) || supervisor.Wait(0) {
				t.Fatalf("second start changed live launch: %+v", second)
			}
			// Duplicate-start equality does not depend on the later stop result.
			if python {
				pythonSecond = second
			} else {
				compare(t, pythonSecond, second)
			}
			// Freezing forces worker escalation, but SIGKILL can still expose
			// Python's zombie-leader/held-lock race (decision 27).
			stopObserved(t, worker)
			stop := invoke(t, home, python, "--socket", home+"/socket", "service", "stop")
			t.Logf("escalated stop: %+v supervisorExitObserved=%v workerExitObserved=%v", stop, supervisor.Wait(0), worker.Wait(0))
			state := files(t, home, writtenBy(python))
			if python {
				pythonStop = stop
				pythonFiles = state
			} else {
				pythonOK := pythonStop.Code == 0 && get(runtimeObject(t, pythonStop), "ok") == true
				if err := plainStopProblem(pythonOK, stop, map[capture]bool{plainStopShape(pythonStop): true}); err != nil {
					t.Error(err)
				}
				if !pythonOK {
					t.Log("Python non-ok stop: omit stop-state equality; duplicate-start equality was checked")
					return
				}
				for _, name := range []string{"daemon.json", "daemon.log", "service.json", "worker-policy.json"} {
					if pythonFiles[name] != state[name] {
						t.Errorf("%s\nPython %s\nGo %s", name, pythonFiles[name], state[name])
					}
				}
			}
		})
		resetRuntime(t, home)
	}
}

// Both workers arm PR_SET_PDEATHSIG=SIGTERM (service.py:299-322). Freezing
// before supervisor death proves the flock survives WHILE the worker lives;
// an unfrozen worker is expected to exit, not to remain a permanent orphan.
func Test29OrphanKeepsInheritedLocks(t *testing.T) {
	home := t.TempDir()
	var pythonStatus capture
	for _, python := range []bool{true, false} {
		t.Run(fmt.Sprint(python), func(t *testing.T) {
			supervisor, worker := startServing(t, home, python)
			stopObserved(t, worker)
			if !supervisor.Send(unix.SIGKILL) || !supervisor.Wait(5*time.Second) {
				t.Fatal("supervisor did not exit")
			}
			if worker.Wait(0) {
				t.Fatal("stopped worker exited")
			}
			if !lockHeld(filepath.Join(home, "state", "daemon.lock")) {
				t.Fatal("supervisor released worker's daemon lock")
			}
			scope := &ScopeRegistry{Root: home + "/scopes", Authority: "isolated"}
			if !lockHeld(scope.path(home+"/socket", ".lock")) {
				t.Fatal("supervisor released worker's scope lock")
			}
			status := invoke(t, home, python, "--socket", home+"/socket", "service", "status")
			r := runtimeObject(t, status)
			if get(r, "running") != true || get(r, "lock") != "held" {
				t.Fatal(status)
			}
			if python {
				pythonStatus = status
			} else {
				compare(t, pythonStatus, status)
			}
			second := invoke(t, home, python, "--socket", home+"/socket", "service", "start", "--allow-isolated-scope")
			if get(runtimeObject(t, second), "reason") != "already_running" {
				t.Fatal(second)
			}
			worker.Send(unix.SIGKILL)
			if !worker.Wait(5 * time.Second) {
				t.Fatal("worker did not exit")
			}
		})
		resetRuntime(t, home)
	}
}
func storeSelection(home string) store.StateSelection {
	return store.StateSelection{Path: filepath.Join(home, "state")}
}
func Test29StopEscalatesAndRefusesForeign(t *testing.T) {
	home := t.TempDir()
	supervisor, worker := startServing(t, home, false)
	stopObserved(t, supervisor)
	stopObserved(t, worker)
	s, err := New(context.Background(), storeSelection(home), home+"/socket")
	if err != nil {
		t.Fatal(err)
	}
	r := s.Record()
	s.InstallationID = text(get(r, "installationId"))
	if err = s.WriteRecord(set(r, "installationId", "foreign-installation")); err != nil {
		t.Fatal(err)
	}
	out, err := s.Stop("test", time.Millisecond)
	if err != nil || get(out, "reason") != "not_ours" || s.StopRequested() {
		t.Fatalf("foreign stop %+v %v", out, err)
	}
	if err = s.WriteRecord(r); err != nil {
		t.Fatal(err)
	}
	out, err = s.Stop("test", time.Second)
	if err != nil || get(out, "ok") != true || !supervisor.Wait(0) || !worker.Wait(0) {
		t.Fatalf("escalation %+v %v", out, err)
	}
}

// Decision 42: a service supervisor interrupted by itself stops, and its worker with it, in
// both runtimes. Python's KeyboardInterrupt ends the supervisor and PR_SET_PDEATHSIG then its
// worker; Go passes the interrupt on and exits after its worker, clearing both identities.
// Interrupted together, as a Go-owner drain or a terminal's process group does it, the Go
// worker absorbs the repeat and stops through its own cleanup (control.sock unbound), not by
// SIGINT's default disposition.
func Test42InterruptedSupervisorStopsItsWorker(t *testing.T) {
	for _, tc := range []struct {
		name         string
		python, both bool
	}{{"python", true, false}, {"go", false, false}, {"go-interrupted-together", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			supervisor, worker := startServing(t, home, tc.python)
			if !supervisor.Send(unix.SIGINT) {
				t.Fatal(supervisor.Detail)
			}
			if tc.both && !worker.Send(unix.SIGINT) {
				t.Fatal(worker.Detail)
			}
			if !supervisor.Wait(10 * time.Second) {
				t.Fatal("the interrupted supervisor outlived its interrupt while its worker ran")
			}
			if !worker.Wait(10 * time.Second) {
				t.Fatal("the worker outlived its interrupted supervisor")
			}
			scope := &ScopeRegistry{Root: home + "/scopes", Authority: "isolated"}
			for _, lock := range []string{filepath.Join(home, "state", "daemon.lock"), scope.path(home+"/socket", ".lock")} {
				if lockHeld(lock) {
					t.Fatalf("%s is still held", lock)
				}
			}
			if tc.python {
				return
			}
			r := read(filepath.Join(home, "state", "daemon.json"))
			if get(r, "pid") != nil || get(r, "workerPid") != nil || get(r, "lastExit") == nil || num(get(r, "lastExit")) < 0 || get(r, "nextRestartAt") != nil {
				t.Fatalf("the supervisor did not record its worker's own exit: %v", r)
			}
			if _, err := os.Lstat(ControlPath(filepath.Join(home, "state"))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the worker ended without closing control.sock: %v", err)
			}
		})
	}
}
