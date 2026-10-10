package job

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// CRW-1092: a completion is stamped delivered only after its text was written, and two hooks never hand out the same completion or
// put back a stamp the other wrote.

func delivered(t *testing.T, ws, id string) bool {
	t.Helper()
	r, ok := ReadRecord(ws, id)
	if !ok {
		t.Fatalf("no record %s", id)
	}
	return r.DeliveredAt != nil
}

func TestBgHookStdoutThatFailsLeavesTheCompletionPending(t *testing.T) {
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skip("no /dev/full")
	}
	defer full.Close()
	ws := workspace(t)
	hookDone(t, ws, "lost", "S1")
	lookup := func(k string) (string, bool) { return "S1", k == "CODEX_THREAD_ID" }
	if code := RunHook(context.Background(), "stop", strings.NewReader("{}"), full, lookup, ws, noon); code != 0 || delivered(t, ws, "lost") {
		t.Fatalf("a write that failed (ENOSPC) stamped the completion: code %d", code)
	}
	var out strings.Builder
	if RunHook(context.Background(), "stop", strings.NewReader("{}"), &out, lookup, ws, noon); !strings.Contains(out.String(), "lost") || !delivered(t, ws, "lost") {
		t.Fatalf("the pending completion did not wake later: %q", out.String())
	}
}

// TestBgHookHelperStdout is the hook process of the next cases: its stdout is what the parent gave it.
func TestBgHookHelperStdout(t *testing.T) {
	ws, mode := os.Getenv("CRW_BG_HOOK_WS"), os.Getenv("CRW_BG_HOOK_MODE")
	if ws == "" {
		t.Skip("the hook half of TestBgHookCrashOrFullStdoutLeavesTheCompletionPending")
	}
	lookup := func(k string) (string, bool) { return "S1", k == "CODEX_THREAD_ID" }
	var out = any(os.Stdout)
	switch mode {
	case "crash-before":
		out = crashWriter{before: true}
	case "crash-after":
		out = crashWriter{}
	}
	os.Exit(RunHook(context.Background(), "stop", strings.NewReader("{}"), out.(interface{ Write([]byte) (int, error) }), lookup, ws, noon))
}

// crashWriter kills its own process before or after it has written the text.
type crashWriter struct{ before bool }

func (w crashWriter) Write(p []byte) (int, error) {
	if w.before {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	}
	_, _ = os.Stdout.Write(p)
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {}
}

func TestBgHookCrashOrFullStdoutLeavesTheCompletionPending(t *testing.T) {
	for _, mode := range []string{"full", "crash-before", "crash-after"} {
		t.Run(mode, func(t *testing.T) {
			ws := workspace(t)
			hookDone(t, ws, "pending", "S1")
			c := exec.Command(os.Args[0], "-test.run=^TestBgHookHelperStdout$")
			c.Env = append(os.Environ(), "CRW_BG_HOOK_WS="+ws, "CRW_BG_HOOK_MODE="+mode)
			if mode == "full" {
				full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
				if err != nil {
					t.Skip("no /dev/full")
				}
				defer full.Close()
				c.Stdout = full // the hook's own fd 1
			}
			_ = c.Run()
			if delivered(t, ws, "pending") {
				t.Errorf("%s: the completion was stamped delivered although its text never reached the host", mode)
			}
		})
	}
}

// race runs first with a clock that, at its n-th reading (by then first has read the store), starts second in another goroutine and
// gives it a moment to finish; second runs on a clock of its own. If first never reads the clock n times, second runs after it.
func race(first func(clock func() time.Time) string, n int, second func() string) (string, string) {
	var b string
	finished := make(chan struct{})
	started, reads := false, 0
	clock := func() time.Time {
		if reads++; reads == n && !started {
			started = true
			go func() { b = second(); close(finished) }()
			select {
			case <-finished:
			case <-time.After(300 * time.Millisecond):
			}
		}
		return noon()
	}
	a := first(clock)
	if !started {
		b = second()
		close(finished)
	}
	<-finished
	return a, b
}

func TestBgHookConcurrentHooksHandOutACompletionOnce(t *testing.T) {
	none := hookEnv(nil)
	for _, firstName := range []string{"stop", "prompt"} {
		for _, secondName := range []string{"stop", "prompt", "drain", "session-start"} {
			t.Run(firstName+"/"+secondName, func(t *testing.T) {
				ws := workspace(t)
				hookDone(t, ws, "once", "S1")
				s1, s2 := HookPayload{SessionID: "S1", Cwd: ws}, HookPayload{SessionID: "S2", Cwd: ws}
				second := map[string]func() string{
					"stop":          func() string { return HandleStop(s1, ws, none, noon) },
					"prompt":        func() string { return HandleUserPromptSubmit(s1, ws, none, noon) },
					"drain":         func() string { return DrainNow(ws, sp("S1"), noon) },
					"session-start": func() string { HandleSessionStart(s2, ws, none, noon); return "" },
				}[secondName]
				first := func(clock func() time.Time) string {
					if firstName == "stop" {
						return HandleStop(s1, ws, none, clock)
					}
					return HandleUserPromptSubmit(s1, ws, none, clock)
				}
				a, b := race(first, 1, second)
				later := HandleStop(s1, ws, none, noon) + HandleStop(s2, ws, none, noon)
				if n := strings.Count(a+b+later, "- once ("); n != 1 || !delivered(t, ws, "once") {
					t.Errorf("the completion was handed out %d times (first %q, second %q, later %q)", n, a, b, later)
				}
			})
		}
	}
	// A SessionStart that adopts from its snapshot must not put back a stamp a Stop wrote meanwhile.
	ws := workspace(t)
	hookDone(t, ws, "kept", "S1")
	s1, s2 := HookPayload{SessionID: "S1", Cwd: ws}, HookPayload{SessionID: "S2", Cwd: ws}
	_, stop := race(func(clock func() time.Time) string { return HandleSessionStart(s2, ws, none, clock) }, 2,
		func() string { return HandleStop(s1, ws, none, noon) })
	again := HandleStop(s2, ws, none, noon) + HandleStop(s1, ws, none, noon)
	if n := strings.Count(stop+again, "- kept ("); n != 1 || !delivered(t, ws, "kept") {
		t.Errorf("an adoption put back an undelivered snapshot: handed out %d times (%q, then %q)", n, stop, again)
	}
}
