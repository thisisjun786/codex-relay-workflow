package agy

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"syscall"
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
	cfg.TimeLimitFloor, cfg.TimeLimitCeiling = 2*time.Second, 2*time.Second
	res := run(t, cfg, Request{Prompt: []byte("hi")})
	if res.Class != ClassInvalid || res.Reason != ReasonTimeLimit || res.ExitCode != -1 || res.Limit != 2*time.Second {
		t.Fatalf("%s/%s exit %d limit %s: %s", res.Class, res.Reason, res.ExitCode, res.Limit, res.Detail)
	}
	if res.Elapsed > 10*time.Second {
		t.Errorf("the call ran %s past a 2s limit", res.Elapsed)
	}
	if _, err := os.Stat(recordPath(cfg)); err != nil {
		t.Skip("the fake had not started when the limit passed")
	}
	got := rec()
	if got.Pid == 0 || got.Child == 0 {
		t.Skipf("the fake had not started its child when the limit passed: %+v", got)
	}
	gone(t, got.Pid)
	gone(t, got.Child)
}

// TestAgyExitsFirst: agy answers and exits while a child it started in its group keeps running. Without the child holding stdout and stderr open the answer
// stands; with it, the output cannot be known to be whole and the call is a crash. Either way the child is gone when Run returns, so the lock never admits a
// call beside a leftover.
func TestAgyExitsFirst(t *testing.T) {
	defer func(old time.Duration) { killGrace = old }(killGrace)
	killGrace = 300 * time.Millisecond
	for _, c := range []struct {
		pipes bool
		want  Class
	}{{false, ClassNormal}, {true, ClassUnavailable}} {
		cfg, rec := fakeCfg(t, fakeSpec{Stdout: `{"status":"SUCCESS","response":"PONG"}`, Child: true, ChildPipes: c.pipes})
		res := run(t, cfg, Request{Prompt: []byte("hi")})
		if res.Class != c.want || res.Elapsed > 10*time.Second || (c.want == ClassNormal && res.Reason != "") || (c.want != ClassNormal && res.Reason != ReasonCrash) {
			t.Errorf("child holding pipes %v: %s/%s after %s: %s", c.pipes, res.Class, res.Reason, res.Elapsed, res.Detail)
		}
		if child := rec().Child; syscall.Kill(child, 0) == nil && !zombie(child) {
			t.Errorf("child %d (pipes %v) was still running when Run returned", child, c.pipes)
		}
	}
}

// TestProcessGroupClose: close reports a group that is empty, and one that still has a member, here a child that died and has not been reaped, which counts.
func TestProcessGroupClose(t *testing.T) {
	defer func(old time.Duration) { groupGrace = old }(groupGrace)
	groupGrace = 200 * time.Millisecond
	g, err := startProcessGroup()
	if err != nil {
		t.Fatal(err)
	}
	if !g.close() {
		t.Error("a group with only its sentinel is empty once it has been closed")
	}
	if g, err = startProcessGroup(); err != nil {
		t.Fatal(err)
	}
	member := exec.Command("/bin/sh", "-c", "exit 0")
	g.join(member)
	if err := member.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // it has exited and stays a zombie: nobody waits for it yet
	if g.close() {
		t.Error("a member nobody has reaped yet must count as present")
	}
	_ = member.Wait()
	if err := syscall.Kill(-g.pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("once reaped the group is empty: %v", err)
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

// TestCallerCancels: a caller that gives up ends the call, its process group and its directory, releases the lock, and gets the context's error. The caller
// gives up once the fake has started, so no deadline has to be guessed.
func TestCallerCancels(t *testing.T) {
	cfg, rec := fakeCfg(t, fakeSpec{Sleep: time.Minute, Child: true})
	before, cancelBefore := context.WithCancel(t.Context())
	cancelBefore()
	if _, err := Run(before, cfg, Request{Prompt: []byte("hi")}); !errors.Is(err, context.Canceled) {
		t.Errorf("a context cancelled before the call: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(recordPath(cfg)); err == nil {
				break
			}
		}
		cancel()
	}()
	if _, err := Run(ctx, cfg, Request{Prompt: []byte("hi")}); !errors.Is(err, context.Canceled) {
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
