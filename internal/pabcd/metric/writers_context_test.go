package metric

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// CRW-632: the four writers that still waited for a lock without a deadline take the invocation's
// context the way the ingest does since CRW-627. The oracle has no lock and no signal handler, so its
// process dies at the first interrupt and writes nothing more. Each case here holds the writer's lock
// from its own descriptor, ends the wait with a cancelled context and requires the context's own error
// with the file exactly as the holder had it. The plain forms keep the blocking wait, so the second
// case releases the holder while the writer waits and requires the write to land.

// crw632HoldFile creates path with contents (nil leaves it absent) and locks it exclusively through a
// descriptor opened without O_APPEND, so an append descriptor on it can only be the writer's.
func crw632HoldFile(t *testing.T, path string, contents []byte) (release func()) {
	t.Helper()
	if contents != nil {
		metricsWrite(t, filepath.Dir(path), filepath.Base(path), contents)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
	metricsMust(t, err)
	metricsMust(t, unix.Flock(int(f.Fd()), unix.LOCK_EX))
	var once sync.Once
	release = func() { once.Do(func() { f.Close() }) }
	t.Cleanup(release)
	return release
}

// crw632HoldDir locks a directory exclusively, which is where the kind and mode writers wait.
func crw632HoldDir(t *testing.T, path string) (release func()) {
	t.Helper()
	metricsMust(t, os.MkdirAll(path, 0o777))
	f, err := os.Open(path)
	metricsMust(t, err)
	metricsMust(t, unix.Flock(int(f.Fd()), unix.LOCK_EX))
	var once sync.Once
	release = func() { once.Do(func() { f.Close() }) }
	t.Cleanup(release)
	return release
}

// crw632CancelAt is a context whose Done channel never closes and whose Err answers nil for the first
// errAfter asks and context.Canceled from then on. Done is a live channel, so metricLockWait takes its
// non-blocking path and a free lock is taken without an Err ask: errAfter 0 puts the cancellation at
// the writer's post-lock check.
type crw632CancelAt struct {
	context.Context
	errAsks  atomic.Int32
	errAfter int32
	done     chan struct{}
}

func (c *crw632CancelAt) Done() <-chan struct{} { return c.done }

func (c *crw632CancelAt) Err() error {
	if c.errAsks.Add(1) > c.errAfter {
		return context.Canceled
	}
	return nil
}

// crw632ReadFile is the state of a file the writer may not have created: "" when it is absent.
func crw632ReadFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	metricsMust(t, err)
	return string(raw)
}

// crw632Writer is one of the four writers: its lock, its plain call, its context-taking call and the
// state the case compares before and after.
type crw632Writer struct {
	name  string
	hold  func(t *testing.T, cwd string) func()
	plain func(cwd string) error
	ctxfn func(ctx context.Context, cwd string) error
	state func(t *testing.T, cwd string) string
}

// crw632Writers is the four writers of this issue. The ledger and the archive are files the case locks;
// the kind and mode writers lock the directory that holds the file they replace.
func crw632Writers() []crw632Writer {
	prior := Record{TS: "2026-01-01T00:00:00.000Z", SessionID: "p", WorkPhaseID: DefaultWorkPhaseID, MetricName: "prior", Value: 1, Baseline: 1, Best: 1, Source: EvaluateSh}
	return []crw632Writer{
		{
			name: "metric record",
			hold: func(t *testing.T, cwd string) func() {
				return crw632HoldFile(t, metricsPath(cwd), []byte(Encode(prior)+"\n"))
			},
			plain: func(cwd string) error {
				_, err := RecordObjectiveMetric(cwd, RecordInput{SessionID: "s", MetricName: "n", Value: 2, Source: EvaluateSh})
				return err
			},
			ctxfn: func(ctx context.Context, cwd string) error {
				_, err := RecordObjectiveMetricContext(ctx, cwd, RecordInput{SessionID: "s", MetricName: "n", Value: 2, Source: EvaluateSh})
				return err
			},
			state: func(t *testing.T, cwd string) string { return crw632ReadFile(t, metricsPath(cwd)) },
		},
		{
			name:  "metric kind",
			hold:  func(t *testing.T, cwd string) func() { return crw632HoldDir(t, objectiveKindDir(cwd)) },
			plain: func(cwd string) error { return WriteObjectiveKind(cwd, "s", Maximize) },
			ctxfn: func(ctx context.Context, cwd string) error { return WriteObjectiveKindContext(ctx, cwd, "s", Maximize) },
			state: func(t *testing.T, cwd string) string { return crw632ReadFile(t, objectiveKindPath(cwd, "s")) },
		},
		{
			name: "divergence mode",
			hold: func(t *testing.T, cwd string) func() { return crw632HoldDir(t, divergenceDir(cwd)) },
			plain: func(cwd string) error {
				_, err := WriteDivergenceMode(cwd, ModeInput{SessionID: "s", Active: true, CollapsePoint: CollapseD})
				return err
			},
			ctxfn: func(ctx context.Context, cwd string) error {
				_, err := WriteDivergenceModeContext(ctx, cwd, ModeInput{SessionID: "s", Active: true, CollapsePoint: CollapseD})
				return err
			},
			state: func(t *testing.T, cwd string) string { return crw632ReadFile(t, modePath(cwd, "s")) },
		},
		{
			name: "divergence candidate add",
			hold: func(t *testing.T, cwd string) func() {
				return crw632HoldFile(t, candidatesPath(cwd), []byte{})
			},
			plain: func(cwd string) error {
				_, err := RecordDivergenceCandidate(cwd, CandidateInput{SessionID: "s", Kind: KindStrong1, Title: "t", Rationale: "r", SourceURLs: []string{"u"}})
				return err
			},
			ctxfn: func(ctx context.Context, cwd string) error {
				_, err := RecordDivergenceCandidateContext(ctx, cwd, CandidateInput{SessionID: "s", Kind: KindStrong1, Title: "t", Rationale: "r", SourceURLs: []string{"u"}})
				return err
			},
			state: func(t *testing.T, cwd string) string { return crw632ReadFile(t, candidatesPath(cwd)) },
		},
	}
}

// TestObjectiveWritersEndTheirLockWaitWithTheContext holds each writer's lock and cancels the context
// while it waits on it: the writer must return the context's own error and leave the file as the holder
// had it. Before this change each of the four waited in the kernel and wrote once the holder let go.
func TestObjectiveWritersEndTheirLockWaitWithTheContext(t *testing.T) {
	for _, w := range crw632Writers() {
		t.Run(w.name, func(t *testing.T) {
			cwd := t.TempDir()
			release := w.hold(t, cwd)
			before := w.state(t, cwd)
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			// metricsWaiting closes waiting when the lock wait asks Done for the second time, so the
			// cancellation lands while the writer is parked on the holder's lock.
			ctx := &metricsWaiting{Context: base, waiting: make(chan struct{})}
			done := make(chan error, 1)
			go func() { done <- w.ctxfn(ctx, cwd) }()
			select {
			case <-ctx.waiting:
			case err := <-done:
				t.Fatalf("the writer ended before it reached the lock: %v", err)
			case <-time.After(5 * time.Second):
				release()
				<-done
				t.Fatal("the writer did not reach the lock within 5 s")
			}
			cancel()
			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				release()
				<-done
				t.Fatal("the writer was still waiting 5 s after its context ended")
			}
			if err != context.Canceled || w.state(t, cwd) != before {
				t.Fatalf("cancelled in the lock wait: error %v, state now %q, was %q; want the context's own error and no write", err, w.state(t, cwd), before)
			}
		})
	}
}

// TestObjectiveWritersKeepTheBlockingWaitWithoutAContext is the other half of the rule: the plain forms
// (and any caller whose context can never end) still block in the kernel and then write, so every
// uninterrupted run is unchanged.
func TestObjectiveWritersKeepTheBlockingWaitWithoutAContext(t *testing.T) {
	for _, w := range crw632Writers() {
		t.Run(w.name, func(t *testing.T) {
			cwd := t.TempDir()
			release := w.hold(t, cwd)
			before := w.state(t, cwd)
			done := make(chan error, 1)
			go func() { done <- w.plain(cwd) }()
			time.Sleep(50 * time.Millisecond) // lets it reach the lock
			release()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the writer that waited for the lock failed: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the writer did not end within 5 s after the holder released the lock")
			}
			if after := w.state(t, cwd); after == before {
				t.Fatalf("the writer that waited for the lock wrote nothing: %q", before)
			}
		})
	}
}

// TestObjectiveWritersCheckTheContextAfterAFreeLock is the CRW-667 rule applied to the four writers: the
// context is read once more after the lock is taken, whether or not the wait had to wait, so a writer
// whose context has ended writes nothing even when the lock was free.
func TestObjectiveWritersCheckTheContextAfterAFreeLock(t *testing.T) {
	metricsAcquiredTempHomes(t)
	for _, w := range crw632Writers() {
		t.Run(w.name, func(t *testing.T) {
			cwd := t.TempDir()
			ctx := &crw632CancelAt{Context: context.Background(), done: make(chan struct{})}
			if err := w.ctxfn(ctx, cwd); err != context.Canceled {
				t.Fatalf("ended context with a free lock: error %v, want the context's own error", err)
			}
			if state := w.state(t, cwd); state != "" {
				t.Fatalf("ended context with a free lock wrote %q", state)
			}
		})
	}
}
