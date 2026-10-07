//go:build dev

package cxcfuzz

import (
	"errors"
	"math/rand"
	"os"
	"testing"
	"time"
)

// A worker's start-up is charged to the startup deadline, not to the per-request timeout: a worker
// that takes far longer than the per-request timeout to boot still answers its first request. This
// is the pool-level form of the defect the issue reports.
func TestPoolStartupWaitsForAReadyWorker(t *testing.T) {
	target := helperTarget(t, func(rng *rand.Rand, size int) any { return nil })
	pool, err := NewPool(target.Oracle, 1, 50*time.Millisecond, 5*time.Second, helperEnvForBoot(750*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pool.Close() }()
	answer, err := pool.Call(`{"text":"ok"}`, "")
	if err != nil {
		t.Fatalf("a worker that took longer than the per-request timeout to boot did not answer: %v", err)
	}
	if answer != `{"text":"ok"}` {
		t.Fatalf("answer %q", answer)
	}
}

// A worker that never becomes ready within the startup deadline is a start-up failure reported as a
// timeout -- the campaign counts it as a timeout case rather than hanging -- and its slot goes back,
// so Close still returns.
func TestPoolStartupTimeoutIsReportedAndTheSlotReturns(t *testing.T) {
	target := helperTarget(t, func(rng *rand.Rand, size int) any { return nil })
	pool, err := NewPool(target.Oracle, 1, time.Second, 300*time.Millisecond, helperEnvForBoot(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Call(`{"text": "ok"}`, "")
	if !errors.Is(err, Timeout{}) {
		t.Fatalf("err = %v, want a Timeout", err)
	}
	done := make(chan error, 1)
	go func() { done <- pool.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return: the failed worker's slot was not returned")
	}
}

// The handshake proves readiness, so a worker whose reply does not answer the request the pool sent
// is not accepted as ready: the pool refuses it and its slot goes back, exactly as for a start-up
// that times out. The fake worker is told to name another request in its handshake reply, so the
// envelope check is driven through the pool rather than only through answer.
func TestPoolRejectsAWorkerThatAnswersTheHandshakeWrongly(t *testing.T) {
	target := helperTarget(t, func(rng *rand.Rand, size int) any { return nil })
	pool, err := NewPool(target.Oracle, 1, time.Second, 5*time.Second, helperEnvForBadHandshake())
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Call(`{"text":"ok"}`, "")
	if err == nil {
		t.Fatal("a worker that mis-answered the handshake was accepted as ready")
	}
	if errors.Is(err, Timeout{}) {
		t.Fatalf("err = %v, want a reply-envelope failure, not a timeout", err)
	}
	done := make(chan error, 1)
	go func() { done <- pool.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return: the rejected worker's slot was not returned")
	}
}

// The startup default is independent of the per-request timeout: NewPool fills in DefaultStartupTimeout
// when the caller passes zero, exactly as it fills in a default timeout.
func TestPoolDefaultsTheStartupTimeout(t *testing.T) {
	target := helperTarget(t, func(rng *rand.Rand, size int) any { return nil })
	pool, err := NewPool(target.Oracle, 1, 0, 0, helperEnvFor())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pool.Close() }()
	if pool.startup != DefaultStartupTimeout {
		t.Fatalf("startup %v, want %v", pool.startup, DefaultStartupTimeout)
	}
	if pool.timeout != DefaultTimeout {
		t.Fatalf("timeout %v, want %v", pool.timeout, DefaultTimeout)
	}
}

// The boot delay reaches the worker through the environment the pool hands it, the same channel
// helperEnv uses, so the delay is in place before the worker reads anything.
func TestHelperBootDelayComesFromTheEnvironment(t *testing.T) {
	if got := os.Getenv(helperBootDelay); got != "" {
		t.Skipf("the test process already sets %s=%s", helperBootDelay, got)
	}
	env := helperEnvForBoot(250 * time.Millisecond)
	found := false
	for _, entry := range env {
		if entry == helperBootDelay+"=250ms" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the boot delay is not in the worker environment: %v", env)
	}
}
