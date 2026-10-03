package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/daemon"
)

// SupervisionInputs allows a scripted clock/worker to exercise hours of segment
// arithmetic without elapsed-time tests. Production passes nil.
type SupervisionInputs struct {
	Now   func() float64
	Sleep func(context.Context, float64) error
	Spawn func(*os.File, *os.File, string, float64, *float64, bool) (*exec.Cmd, error)
	Wait  func(*exec.Cmd) int
}

func wait(ctx context.Context, seconds float64) error {
	timer := time.NewTimer(time.Duration(math.Max(0, seconds) * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// awaitWorker reaps the worker, bounded by the supervisor's interrupt (decision 42). An
// interrupt that lands after the spawn, however late the scheduler lets this process see it,
// is passed on to the worker as the interrupt a Go worker stops on, and the supervisor still
// waits for that worker: its records and scope are released only after the process holding
// the inherited locks has gone. A worker that cannot be signalled is left to the operator's
// second interrupt, whose default disposition ends this supervisor and, through
// PR_SET_PDEATHSIG, the worker.
func awaitWorker(ctx context.Context, child *exec.Cmd, reap func(*exec.Cmd) int) int {
	exited := make(chan int, 1)
	go func() { exited <- reap(child) }()
	select {
	case code := <-exited:
		return code
	case <-ctx.Done():
	}
	// The process is this supervisor's unreaped child, signalled through its pidfd, so a
	// worker that has just exited answers os.ErrProcessDone and never a recycled pid.
	_ = child.Process.Signal(os.Interrupt)
	return <-exited
}

func (s *Service) Supervise(ctx context.Context, o Options, onStart func() error, inputs *SupervisionInputs) (out Object, err error) {
	if o.Deadline != nil && o.DeadlineMonotonic != nil {
		return nil, fmt.Errorf("deadline and deadline_monotonic are two different bounds; pass one. A duration starts at this process's clock; an instant was decided before it existed, and silently preferring either would end the run at a time the caller did not ask for")
	}
	for _, bound := range []struct {
		name  string
		value *float64
	}{{"deadline", o.Deadline}, {"deadline_monotonic", o.DeadlineMonotonic}} {
		if bound.value != nil {
			if math.IsNaN(*bound.value) || math.IsInf(*bound.value, 0) {
				return nil, fmt.Errorf("%s must be a finite number of seconds", bound.name)
			}
			if *bound.value < 0 {
				return nil, fmt.Errorf("%s cannot be negative", bound.name)
			}
		}
	}
	seconds := 3600.0
	if o.SegmentSeconds != nil {
		seconds = *o.SegmentSeconds
	}
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return nil, fmt.Errorf("segment_seconds must be a finite number greater than zero; it is the bound every worker is given, and a worker cannot be given a length it must refuse")
	}
	now, sleep, spawn := Monotonic, wait, s.SpawnWorker
	waitChild := func(cmd *exec.Cmd) int { _ = cmd.Wait(); return cmd.ProcessState.ExitCode() }
	if inputs != nil {
		if inputs.Now != nil {
			now = inputs.Now
		}
		if inputs.Sleep != nil {
			sleep = inputs.Sleep
		}
		if inputs.Spawn != nil {
			spawn = inputs.Spawn
		}
		if inputs.Wait != nil {
			waitChild = inputs.Wait
		}
	}
	if gate := s.AuthorityCheck(o.AllowIsolated); !truth(gate.Get("ok")) {
		return nil, &Refused{pyjson.Text(gate.Get("reason")), pyjson.Text(gate.Get("detail"))}
	}
	if !truth(s.Intent().Get("enabled")) {
		return nil, &Refused{"service_disabled", "this service is not enabled; supervising it would ignore the owner's intent"}
	}
	if err = os.Remove(s.path("stop.request")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	token, err := randomID()
	if err != nil {
		return nil, err
	}
	lock, err := daemon.Acquire(s.Selection.Path, true, nil)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if !truth(s.Intent().Get("enabled")) {
		return nil, &Refused{"service_disabled", "this service was disabled while this supervisor was starting"}
	}
	// service.py supervise: the scope is claimed with the identity read at start; the store
	// is opened by onStart (cli.py recover), which publishes its identity.
	if s.Socket != "" {
		claim, e := s.Scope.Claim(s.Socket, s.NewRecord(os.Getpid(), token))
		if e != nil {
			return nil, e
		}
		if !truth(claim.Get("ok")) {
			return nil, &Refused{pyjson.Text(claim.Get("reason")), pyjson.Dumps(claim.Get("held_by"), pyjson.Options{})}
		}
	}
	if err = s.WriteRecord(s.NewRecord(os.Getpid(), token)); err != nil {
		if s.Socket != "" {
			err = errors.Join(err, s.Scope.Release(s.Socket))
		}
		return nil, err
	}
	var outstanding *exec.Cmd
	defer func() {
		if s.Socket != "" {
			err = errors.Join(err, s.Scope.Release(s.Socket))
		}
		r := set(s.Record(), "pid", nil, "stoppedAt", stamp(), "nextRestartAt", nil)
		if outstanding == nil {
			r = set(r, "workerPid", nil, "workerStartTicks", nil)
		}
		err = errors.Join(err, s.WriteRecord(r))
	}()
	started := now()
	var end *float64
	if o.Deadline != nil {
		v := started + *o.Deadline
		end = &v
	}
	if o.DeadlineMonotonic != nil {
		end = o.DeadlineMonotonic
	}
	expired := func() bool { return end != nil && now() >= *end }
	if !expired() && onStart != nil {
		if err = onStart(); err != nil {
			return nil, err
		}
	}
	if !expired() {
		if err = s.note("readyAt", stamp()); err != nil {
			return nil, err
		}
	}
	segments := []any{}
	failures := 0
	var degraded any
	for {
		if o.MaxSegments != nil && len(segments) >= *o.MaxSegments {
			break
		}
		if expired() || s.StopRequested() || s.Draining() || !truth(s.Intent().Get("enabled")) {
			break
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		current := now()
		granted := seconds
		var instant *float64
		if end != nil {
			granted = math.Max(.1, math.Min(seconds, *end-current))
			v := math.Min(current+granted, *end)
			instant = &v
		}
		child, e := spawn(lock.File, s.Scope.file, token, granted, instant, o.AllowIsolated)
		if e != nil {
			return nil, e
		}
		outstanding = child
		if err = s.note("workerPid", child.Process.Pid, "workerStartTicks", StartTicks(child.Process.Pid)); err != nil {
			return nil, err
		}
		code := awaitWorker(ctx, child, waitChild)
		outstanding = nil
		if err = s.note("workerPid", nil, "workerStartTicks", nil, "lastExit", code, "restarts", len(segments)+1); err != nil {
			return nil, err
		}
		segments = append(segments, code)
		if err = ctx.Err(); err != nil {
			// The worker ended on this supervisor's interrupt: no failure, and no successor.
			return nil, err
		}
		if code == ExitBoundSpent {
			if !expired() {
				degraded = fmt.Sprintf("a worker costs more to start than the segment it was granted (%.3fs); it served nothing", granted)
				if err = s.JournalNote(degraded.(string)); err != nil {
					return nil, err
				}
				if err = s.note("consecutiveFailures", failures, "degraded", degraded); err != nil {
					return nil, err
				}
			}
			break
		}
		if code == 0 {
			failures = 0
		} else {
			failures++
		}
		if failures >= 3 {
			degraded = fmt.Sprintf("%d consecutive worker failures, last exit %d", failures, code)
			if err = s.JournalNote(degraded.(string)); err != nil {
				return nil, err
			}
		}
		if err = s.note("consecutiveFailures", failures, "degraded", degraded); err != nil {
			return nil, err
		}
		if s.StopRequested() || s.Draining() || !truth(s.Intent().Get("enabled")) || (o.MaxSegments != nil && len(segments) >= *o.MaxSegments) || expired() {
			break
		}
		delay := RestartDelay(failures)
		if end != nil {
			delay = math.Max(0, math.Min(delay, *end-now()))
		}
		if err = s.note("nextRestartAt", float64(time.Now().UnixNano())/1e9+delay); err != nil {
			return nil, err
		}
		if err = sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
	return obj("ok", true, "reason", nil, "segments", segments, "consecutiveFailures", failures, "degraded", degraded), nil
}
