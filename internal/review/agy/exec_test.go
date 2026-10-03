package agy

import (
	"context"
	"errors"
	"math"
	"os"
	"testing"
	"time"
)

func TestTimeLimit(t *testing.T) {
	const kib = 1024
	for _, c := range []struct {
		floor, ceiling time.Duration
		size           int
		want           time.Duration
	}{
		{5 * time.Minute, 20 * time.Minute, 0, 5 * time.Minute},
		{5 * time.Minute, 20 * time.Minute, 100 * kib, 5 * time.Minute},
		{5 * time.Minute, 20 * time.Minute, 150 * kib, 7*time.Minute + 30*time.Second},
		{5 * time.Minute, 20 * time.Minute, 300 * kib, 15 * time.Minute},
		{5 * time.Minute, 20 * time.Minute, 1 << 30, 20 * time.Minute},
		{5 * time.Minute, 20 * time.Minute, math.MaxInt, 20 * time.Minute},
		{10 * time.Minute, 5 * time.Minute, 1 << 20, 10 * time.Minute}, // a ceiling below the floor cannot lower it
	} {
		if got := timeLimit(c.floor, c.ceiling, c.size); got != c.want {
			t.Errorf("timeLimit(%s, %s, %d) = %s, want %s", c.floor, c.ceiling, c.size, got, c.want)
		}
	}
}

// TestTimeLimitEndsTheProcessGroup: when the limit passes, the fake and the child it started in its group are both killed.
func TestTimeLimitEndsTheProcessGroup(t *testing.T) {
	cfg, rec := fakeCfg(t, fakeSpec{Sleep: time.Minute, Child: true})
	cfg.TimeLimitFloor, cfg.TimeLimitCeiling = 1500*time.Millisecond, 1500*time.Millisecond
	res := run(t, cfg, Request{Prompt: []byte("hi")})
	if res.Class != ClassInvalid || res.Reason != ReasonTimeLimit || res.ExitCode != -1 || res.Limit != 1500*time.Millisecond {
		t.Fatalf("%s/%s exit %d limit %s: %s", res.Class, res.Reason, res.ExitCode, res.Limit, res.Detail)
	}
	if res.Elapsed > 10*time.Second {
		t.Errorf("the call ran %s past a 1.5s limit", res.Elapsed)
	}
	got := rec()
	if got.Pid == 0 || got.Child == 0 {
		t.Fatalf("the fake and its child must both have started: %+v", got)
	}
	gone(t, got.Pid)
	gone(t, got.Child)
}

// TestLeaderExitsFirst: agy answers and exits while a child it started in its group keeps running, with and without holding stdout and stderr open. The
// answer stands, the call does not wait for the child, and the child is gone when Run returns, so the lock never admits a call beside a leftover.
func TestLeaderExitsFirst(t *testing.T) {
	defer func(old time.Duration) { killGrace = old }(killGrace)
	killGrace = 300 * time.Millisecond
	for _, pipes := range []bool{false, true} {
		cfg, rec := fakeCfg(t, fakeSpec{Stdout: `{"status":"SUCCESS","response":"PONG"}`, Child: true, ChildPipes: pipes})
		res := run(t, cfg, Request{Prompt: []byte("hi")})
		if res.Class != ClassNormal || res.Elapsed > 10*time.Second {
			t.Errorf("child holding pipes %v: %s/%s after %s: %s", pipes, res.Class, res.Reason, res.Elapsed, res.Detail)
		}
		gone(t, rec().Child)
	}
}

func TestOutputIsCapped(t *testing.T) {
	cfg, _ := fakeCfg(t, fakeSpec{Flood: 4096})
	cfg.MaxOutputBytes = 1024
	res := run(t, cfg, Request{Prompt: []byte("hi")})
	if res.Class != ClassUnavailable || res.Reason != ReasonCrash || res.StructuredOutput != nil {
		t.Errorf("%s/%s: %s", res.Class, res.Reason, res.Detail)
	}
}

// TestCallerCancels: a caller that gives up ends the call, its process group and its directory, releases the lock, and gets the context's error.
func TestCallerCancels(t *testing.T) {
	cfg, rec := fakeCfg(t, fakeSpec{Sleep: time.Minute, Child: true})
	before, cancelBefore := context.WithCancel(t.Context())
	cancelBefore()
	if _, err := Run(before, cfg, Request{Prompt: []byte("hi")}); !errors.Is(err, context.Canceled) {
		t.Errorf("a context cancelled before the call: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	if _, err := Run(ctx, cfg, Request{Prompt: []byte("hi")}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a context that expires during the call: %v", err)
	}
	got := rec()
	gone(t, got.Pid)
	gone(t, got.Child)
	if left, _ := os.ReadDir(cfg.WorkRoot); len(left) != 0 {
		t.Errorf("left under the working root: %v", left)
	}
	next, _ := fakeCfg(t, fakeSpec{Stdout: `{"status":"SUCCESS","response":"PONG"}`})
	next.LockPath, next.LockWait = cfg.LockPath, -1
	if res := run(t, next, Request{Prompt: []byte("hi")}); res.Class != ClassNormal {
		t.Errorf("the lock was not released: %s/%s", res.Class, res.Reason)
	}
}
