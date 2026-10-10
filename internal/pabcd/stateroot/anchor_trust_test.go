package stateroot

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1140 round 3 (d3): an anchor that exists but cannot be trusted is not an absent anchor. The
// thread is relay-managed and its work may be in flight at the root the anchor named, so a resume
// and a SessionStart are refused until the operator repairs it, and nothing overwrites it.
func TestAnAnchorThatCannotBeTrustedIsNotAnAbsentOne(t *testing.T) {
	cases := map[string]string{
		"truncated":           `{"version":1,"sessionId":"` + session,
		"unsupported version": `{"version":2,"sessionId":"` + session + `","nativeCwd":"/x"}`,
		"another session":     `{"version":1,"sessionId":"0190cafe-0279-7000-8000-00000000ffff","nativeCwd":"/x"}`,
		"relative root":       `{"version":1,"sessionId":"` + session + `","nativeCwd":"x"}`,
		"empty":               ``,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			a, b := t.TempDir(), t.TempDir()
			env := envAt(t.TempDir())
			writePhase(t, a, state.PhaseP)
			if err := Guard(env, a, a, session); err != nil {
				t.Fatal(err)
			}
			path := AnchorPath(env, session)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := Bootstrap(env, b, session); err == nil {
				t.Error("a SessionStart beside an untrustworthy anchor was allowed")
			} else if CodeOf(err) != AnchorUnreadableCode {
				t.Errorf("Bootstrap code %s: %v", CodeOf(err), err)
			}
			if err := Guard(env, b, b, session); err == nil {
				t.Error("a resume beside an untrustworthy anchor was allowed")
			} else if CodeOf(err) != AnchorUnreadableCode {
				t.Errorf("Guard code %s: %v", CodeOf(err), err)
			}
			if err := Resolve(env, b, b, session); err == nil {
				t.Error("the dry run beside an untrustworthy anchor was allowed")
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != content {
				t.Errorf("the anchor was overwritten: %q (%v)", got, err)
			}
		})
	}
}

// d5: an anchor path that is not a regular file is refused without being read: a FIFO with no writer
// must not hold a resume or a SessionStart.
func TestANonRegularAnchorIsRefusedWithoutBlocking(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	env := envAt(t.TempDir())
	path := AnchorPath(env, session)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("no fifo: %v", err)
	}
	t.Cleanup(func() {
		// Release a reader that did block, so the leak does not outlive the test.
		if f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
	calls := map[string]func() error{
		"Guard":     func() error { return Guard(env, a, b, session) },
		"Resolve":   func() error { return Resolve(env, a, b, session) },
		"Bootstrap": func() error { return Bootstrap(env, b, session) },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- call() }()
			select {
			case err := <-done:
				if err == nil || CodeOf(err) != AnchorUnreadableCode {
					t.Fatalf("a FIFO anchor: %v (code %s)", err, CodeOf(err))
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the call blocked on the FIFO anchor")
			}
		})
	}
}

// An oversized anchor is not read whole.
func TestAnOversizedAnchorIsRefused(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	env := envAt(t.TempDir())
	writePhase(t, a, state.PhaseP)
	if err := Guard(env, a, a, session); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 1<<20)
	for i := range big {
		big[i] = ' '
	}
	if err := os.WriteFile(AnchorPath(env, session), big, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Bootstrap(env, b, session); err == nil || CodeOf(err) != AnchorUnreadableCode {
		t.Fatalf("an oversized anchor: %v", err)
	}
}

// d4: a SessionStart whose anchor cannot follow the thread to its new cwd is refused, so no state
// exists at a cwd the anchor does not track.
func TestBootstrapRefusesWhenTheAnchorCannotFollowTheThread(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	a, b := t.TempDir(), t.TempDir()
	env := envAt(t.TempDir())
	writePhase(t, a, state.PhaseIdle)
	if err := Guard(env, a, a, session); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(AnchorPath(env, session))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err := Bootstrap(env, b, session)
	var anchorErr *AnchorError
	if !errors.As(err, &anchorErr) || CodeOf(err) != AnchorCode {
		t.Fatalf("Bootstrap with an unwritable anchor directory: %v", err)
	}
	if anchorOf(t, env) != a {
		t.Fatalf("anchor %q", anchorOf(t, env))
	}
	// At the root nothing needs moving, so the bootstrap goes ahead.
	if err := Bootstrap(env, a, session); err != nil {
		t.Fatalf("Bootstrap at the anchored root: %v", err)
	}
}

// d2: the anchor follows the cwd the host reported for the resumed thread, not the one asked for.
func TestMovedFollowsTheCwdTheHostReported(t *testing.T) {
	a, b, c := t.TempDir(), t.TempDir(), t.TempDir()
	env := envAt(t.TempDir())
	writePhase(t, a, state.PhaseIdle)
	if err := Guard(env, a, b, session); err != nil {
		t.Fatal(err)
	}
	// The host kept the thread at a although b was asked for.
	if err := Moved(env, a, a, session); err != nil {
		t.Fatal(err)
	}
	if anchorOf(t, env) != a {
		t.Fatalf("anchor %q after a resume that stayed at the root", anchorOf(t, env))
	}
	// No cwd reported: nothing is known to have moved.
	if err := Moved(env, a, "", session); err != nil || anchorOf(t, env) != a {
		t.Fatalf("anchor %q after a result without a cwd (%v)", anchorOf(t, env), err)
	}
	if err := Moved(env, a, c, session); err != nil || anchorOf(t, env) != c {
		t.Fatalf("anchor %q after a resume the host ran at %s (%v)", anchorOf(t, env), c, err)
	}
}

// d4: a move whose anchor cannot be recorded is reported to the caller.
func TestMovedReportsAnAnchorThatCouldNotFollow(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	a, b := t.TempDir(), t.TempDir()
	env := envAt(t.TempDir())
	writePhase(t, a, state.PhaseIdle)
	if err := Guard(env, a, b, session); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(AnchorPath(env, session))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err := Moved(env, a, b, session)
	var anchorErr *AnchorError
	if !errors.As(err, &anchorErr) || CodeOf(err) != AnchorCode {
		t.Fatalf("Moved with an unwritable anchor directory: %v", err)
	}
}
