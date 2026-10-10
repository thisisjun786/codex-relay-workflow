//go:build dev

package cxcfuzz

import (
	"errors"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A worker's start-up is charged to the startup deadline, not to the per-request timeout: a worker
// that takes far longer than the per-request timeout to boot still answers its first request. This
// is the pool-level form of the defect the issue reports. The per-request timeout is long enough for a
// ready worker to answer on a loaded host and the boot delay is three times it, so the boot outlasts the
// timeout by construction and the answer depends on nothing but where the pool charges the boot
// (CRW-1172).
func TestPoolStartupWaitsForAReadyWorker(t *testing.T) {
	const perRequest = 400 * time.Millisecond
	target := helperTarget(t, func(rng *rand.Rand, size int) any { return nil })
	pool, err := NewPool(target.Oracle, 1, perRequest, 20*time.Second, helperEnvForBoot(3*perRequest))
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

// A PATH entry that is present but empty is an empty search path, so nothing is found there; only an
// environment with no PATH entry at all falls back to this process's own PATH, as a child would
// inherit (CRW-708 generation 5, d5). The worker gets exactly this environment, so the dependency
// check and the worker must agree about what is reachable.
func TestLookPathInTreatsAnEmptyPathEntryAsEmpty(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "python3")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The helper is reachable only through this process's own PATH, never through the worker's.
	t.Setenv("PATH", dir)
	if _, err := lookPathIn([]string{"PATH="}, "python3"); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("an explicit empty PATH resolved python3: %v", err)
	}
	if _, err := lookPathIn(nil, "python3"); err != nil {
		t.Fatalf("an environment with no PATH entry did not fall back to this process's PATH: %v", err)
	}
	if _, err := lookPathIn([]string{"HOME=/tmp"}, "python3"); err != nil {
		t.Fatalf("an environment with no PATH entry did not fall back to this process's PATH: %v", err)
	}
}

// A campaign whose worker environment holds an explicit empty PATH must refuse the target before any
// case runs when a required command is reachable only through this process's own PATH: the campaign
// would otherwise record a worker failure as a fuzzing difference (CRW-708 generation 5, d5).
func TestCampaignRefusesAMissingRequirementUnderAnEmptyPath(t *testing.T) {
	// node and python3 are both on this process's PATH, so the pool resolves the interpreter and the
	// only thing that can refuse the target is the requirement check against the worker's own
	// environment. That environment holds an explicit empty PATH, so nothing is reachable in it.
	// NewPool resolves the interpreter before its requirements, so a host without node refuses on node
	// and this test has nothing to say about the requirement check.
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node is not on PATH: %v", err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 is not on PATH: %v", err)
	}
	target := pyjsonTarget()
	_, err := NewPool(target.Oracle, 1, DefaultTimeout, DefaultStartupTimeout, []string{"PATH="})
	var missing NoCommand
	if !errors.As(err, &missing) || missing.Command != "python3" {
		t.Fatalf("err = %v, want NoCommand naming python3", err)
	}
}
