package agy

import (
	"math"
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

func TestTimeLimitEndsTheProcessGroup(t *testing.T) {
	cfg, rec := fakeCfg(t, fakeSpec{Sleep: time.Minute, Child: true})
	cfg.TimeLimitFloor, cfg.TimeLimitCeiling = 400*time.Millisecond, 400*time.Millisecond
	res := run(t, cfg, Request{Prompt: []byte("hi")})
	if res.Class != ClassInvalid || res.Reason != ReasonTimeLimit || res.ExitCode != -1 || res.Limit != 400*time.Millisecond {
		t.Fatalf("%s/%s exit %d limit %s: %s", res.Class, res.Reason, res.ExitCode, res.Limit, res.Detail)
	}
	if res.Elapsed > 10*time.Second {
		t.Errorf("the call ran %s past a 400ms limit", res.Elapsed)
	}
	got := rec()
	if got.Pid == 0 || got.Child == 0 {
		t.Fatalf("the fake and its child must both have started: %+v", got)
	}
	gone(t, got.Pid)
	gone(t, got.Child)
}

func TestOutputIsCapped(t *testing.T) {
	cfg, _ := fakeCfg(t, fakeSpec{Flood: 4096})
	cfg.MaxOutputBytes = 1024
	res := run(t, cfg, Request{Prompt: []byte("hi")})
	if res.Class != ClassUnavailable || res.Reason != ReasonCrash || res.StructuredOutput != nil {
		t.Errorf("%s/%s: %s", res.Class, res.Reason, res.Detail)
	}
}
