package pipeline

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/agy"
)

func TestRunnerErrorAxesAndCancellation(t *testing.T) {
	cfg := Config{Head: fakeHead{}}
	calls := 0
	run := func(context.Context, agy.Config, agy.Request) (agy.Result, error) {
		calls++
		switch calls {
		case 1:
			return findings(sample(2, "valid output", "P2")), errors.New("cleanup failed")
		case 4:
			return normal(map[string]any{"verdict": "confirmed", "needsContext": false}), errors.New("cleanup failed")
		default:
			return findings(), nil
		}
	}
	a, err := Run(context.Background(), testBundle(), run, cfg)
	if err != nil || a.Status != review.StatusPartial || a.Reviewers.Failed != 0 || len(a.Findings) != 1 || a.Calls[0].Class != "normal" || a.Calls[0].Error != "cleanup failed" || a.Findings[0].Verdict != review.VerdictConfirmed {
		t.Fatalf("valid result plus error: %+v %v", a, err)
	}
	a, err = Run(context.Background(), testBundle(), func(context.Context, agy.Config, agy.Request) (agy.Result, error) {
		return agy.Result{}, errors.New("host failure")
	}, cfg)
	if err != nil || a.Status != review.StatusUnavailable || a.Reviewers.Failed != 3 || a.Calls[0].Class != "unavailable" || a.Calls[0].Reason != "runner_error" || a.Calls[0].Error != "host failure" {
		t.Fatalf("zero result plus error: %+v %v", a, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls = 0
	a, err = Run(ctx, testBundle(), func(context.Context, agy.Config, agy.Request) (agy.Result, error) {
		calls++
		cancel()
		return findings(), nil
	}, cfg)
	if !errors.Is(err, context.Canceled) || a != nil || calls != 1 {
		t.Fatalf("cancellation dispatched further: %+v %v calls%d", a, err, calls)
	}
	a, err = Run(context.Background(), testBundle(), func(context.Context, agy.Config, agy.Request) (agy.Result, error) { return findings(), nil }, cfg)
	if err != nil || a.Status != review.StatusComplete {
		t.Fatalf("guard was not released: %v", err)
	}
}

func TestCanceledWaiterDoesNotInvokeRunner(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var calls atomic.Int32
	go func() {
		_, err := Run(context.Background(), testBundle(), func(context.Context, agy.Config, agy.Request) (agy.Result, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-release
			}
			return findings(), nil
		}, Config{Head: fakeHead{}})
		done <- err
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waiterCalls := 0
	a, err := Run(ctx, testBundle(), func(context.Context, agy.Config, agy.Request) (agy.Result, error) {
		waiterCalls++
		return findings(), nil
	}, Config{Head: fakeHead{}})
	close(release)
	firstErr := <-done
	if !errors.Is(err, context.Canceled) || a != nil || waiterCalls != 0 || firstErr != nil || calls.Load() != 3 {
		t.Fatalf("waiter %+v %v calls%d, first%v calls%d", a, err, waiterCalls, firstErr, calls.Load())
	}
}
