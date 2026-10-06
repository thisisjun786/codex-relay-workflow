//go:build linux

package service

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// stopWorkerReapedFixture owns a pipe-controlled child and its reap. No service
// process is launched; the record and enabled intent belong to a temporary store.
func stopWorkerReapedFixture(t *testing.T) (*Service, *ProcessHandle, func(), func()) {
	t.Helper()
	s := testService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := helper(ctx, "idle", "")
	input, err := cmd.StdinPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce, reapOnce sync.Once
	release := func() { releaseOnce.Do(func() { _ = input.Close() }) }
	reap := func() {
		reapOnce.Do(func() {
			release()
			if err := cmd.Wait(); err != nil {
				t.Errorf("owned worker wait: %v", err)
			}
		})
	}
	t.Cleanup(reap)
	h := OpenProcess(cmd.Process.Pid)
	if h.FD < 0 {
		t.Fatal(h.Detail)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	ticks := StartTicks(h.PID)
	if ticks == nil {
		t.Fatal("live worker has no initial start ticks")
	}
	r := obj("pid", nil, "bootId", BootID(), "installationId", s.InstallationID,
		"workerPid", h.PID, "workerStartTicks", ticks, "launchId", "test-launch")
	if err := s.WriteRecord(r); err != nil {
		t.Fatal(err)
	}
	return s, h, release, reap
}

func TestStopWorkerReapedBetweenPidfdAndTicks(t *testing.T) {
	for _, action := range []string{"worker", "stop", "restart"} {
		t.Run(action, func(t *testing.T) {
			s, h, _, reap := stopWorkerReapedFixture(t)
			t.Setenv("CODEX_THREAD_BRIDGE_EXECUTION_POLICY", "")
			s.stopWorkerReadTicks = func(pid int) any {
				// stopWorker has already opened its real pidfd. Hold this
				// window until the worker exits AND is reaped, then read /proc.
				reap()
				ticks := StartTicks(pid)
				if ticks != nil || !h.Wait(0) {
					t.Fatalf("window not reached: ticks=%v pidfdExited=%v", ticks, h.Wait(0))
				}
				return ticks
			}
			if action == "worker" {
				outcome := s.stopWorker(s.Record(), time.Second)
				t.Logf("post-open reap: worker=%s", outcome)
				if outcome != "exited" {
					t.Fatalf("worker=%s, want exited", outcome)
				}
				return
			}
			var stopped Object
			if action == "restart" {
				path := filepath.Join(s.Selection.Path, "policy.json")
				if err := os.WriteFile(path, []byte(`{"roles":{"child":{"model":"test-model","reasoningEffort":"high"}}}`), 0600); err != nil {
					t.Fatal(err)
				}
				if declared, err := s.Declare(path, "test", false); err != nil || declared.Get("ok") != true {
					t.Fatalf("declare: %v %v", declared, err)
				}
				// Start refuses this isolated scope after Restart's stop guard.
				// Reaching that refusal proves the guard passed, without a launch.
				answer, err := s.Restart(context.Background(), Options{Actor: "test", Timeout: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("restart reason=%v stop=%v", answer.Get("reason"), answer.Get("stop"))
				if answer.Get("reason") != "isolated_scope_not_allowed" {
					t.Errorf("restart reason=%v, want Start's isolated_scope_not_allowed", answer.Get("reason"))
				}
				stopped, _ = answer.Get("stop").(Object)
			} else {
				var err error
				stopped, err = s.Stop("test", time.Second)
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("stop=%v", stopped)
			if stopped.Get("ok") != true || stopped.Get("reason") != nil || stopped.Get("supervisor") != "gone" || stopped.Get("worker") != "exited" {
				t.Errorf("unexpected stop: %v", stopped)
			}
			r := s.Record()
			if r.Get("workerPid") != nil || r.Get("workerStartTicks") != nil || r.Get("stoppedAt") == nil {
				t.Errorf("exited worker identity not cleared: %v", r)
			}
		})
	}
}

func TestStopWorkerReapedLiveUnidentifiable(t *testing.T) {
	for _, missing := range []string{"current", "recorded"} {
		t.Run(missing, func(t *testing.T) {
			s, h, _, _ := stopWorkerReapedFixture(t)
			if missing == "current" {
				// A read/parse failure alone says nothing about process exit.
				s.stopWorkerReadTicks = func(int) any { return nil }
			} else if err := s.WriteRecord(set(s.Record(), "workerStartTicks", nil)); err != nil {
				t.Fatal(err)
			}
			outcome := s.stopWorker(s.Record(), time.Second)
			if outcome != "unverifiable" || h.Wait(0) {
				t.Fatalf("unidentified live worker: outcome=%s exited=%v", outcome, h.Wait(0))
			}
		})
	}
}

func TestStopWorkerReapedExitedWithoutRecordedTicks(t *testing.T) {
	s, h, release, _ := stopWorkerReapedFixture(t)
	release()
	if !h.Wait(5*time.Second) || StartTicks(h.PID) == nil {
		t.Fatal("worker must be exited but not reaped, with readable current ticks")
	}
	outcome := s.stopWorker(set(s.Record(), "workerStartTicks", nil), time.Second)
	if outcome != "exited" {
		t.Fatalf("exited worker without recorded ticks=%s, want exited", outcome)
	}
}
