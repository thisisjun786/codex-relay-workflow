package hook

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1094 (isolated trial S4-F2): a session .lock left by a killed hook made every later chat
// `orchestrate A` answer nothing after about 0.26 s, with the phase still P and no ledger row, until
// the file was removed by hand. A lock whose owner is gone is now taken over and the command applies;
// a lock a live owner holds is refused in words that name the lock, never with an empty answer.

func TestChatOrchestrateTakesOverTheLockOfADeadHook(t *testing.T) {
	cwd := t.TempDir()
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.OrchestrationActive = state.PhaseP, true
	})
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	lock := state.StatePath(cwd, "s1") + ".lock"
	if err := os.WriteFile(lock, []byte(strconv.Itoa(dead.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	want := WithFooter(PhaseDirective(state.PhaseA, nil), state.PhaseA)
	if got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate A"); got != want {
		t.Fatalf("chat orchestrate A over a dead hook's lock\n got %q\nwant %q", got, want)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseA {
		t.Fatalf("phase %s; want A", s.Phase)
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 1 || rows[0]["from"] != "P" || rows[0]["to"] != "A" {
		t.Fatalf("ledger rows %+v", rows)
	}
	if _, err := os.Lstat(lock); !os.IsNotExist(err) {
		t.Fatalf("the lock was not released: %v", err)
	}
}

func TestChatOrchestrateUnderALiveLockSaysItWasNotApplied(t *testing.T) {
	cwd := t.TempDir()
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.OrchestrationActive = state.PhaseP, true
	})
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- state.WithSessionLock(cwd, "s1", func() error { close(held); <-release; return nil })
	}()
	<-held
	got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate A")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(cwd, ".crw", "sessions", "s1.json.lock")
	for _, want := range []string{"orchestrate A was not applied", "session lock", lock, "The phase and ledger were not changed."} {
		if !strings.Contains(got, want) {
			t.Fatalf("the answer %q lacks %q", got, want)
		}
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseP {
		t.Fatalf("phase %s; want P", s.Phase)
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 0 {
		t.Fatalf("ledger rows %+v", rows)
	}
}

// CRW-1094 (pre-merge evaluation d2): the explanation is the command write's too, not only the turn stamp's. A payload without a turn id
// skips the stamp, and a writer that takes the lock after a successful stamp fails the command's own write; both answer in words that
// name the lock or the write error, with the phase and ledger unchanged.
func TestChatOrchestrateWithoutATurnIdUnderALiveLockSaysItWasNotApplied(t *testing.T) {
	cwd := t.TempDir()
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.OrchestrationActive = state.PhaseP, true
	})
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- state.WithSessionLock(cwd, "s1", func() error { close(held); <-release; return nil })
	}()
	<-held
	got := promptOrchestrateAnswer(t, cwd, "s1", "", "orchestrate A")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(cwd, ".crw", "sessions", "s1.json.lock")
	for _, want := range []string{"orchestrate A was not applied", "session lock", lock, "The phase and ledger were not changed."} {
		if !strings.Contains(got, want) {
			t.Fatalf("the answer %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "refused: the session state changed") {
		t.Fatalf("the answer is the generic refusal: %q", got)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseP {
		t.Fatalf("phase %s; want P", s.Phase)
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 0 {
		t.Fatalf("ledger rows %+v", rows)
	}
}

func TestChatOrchestrateWhoseCommandWriteFailsAfterTheStampSaysWhy(t *testing.T) {
	busy := &fs.PathError{Op: "open", Path: "x", Err: syscall.EEXIST}
	for name, c := range map[string]struct {
		fail error
		want []string
	}{
		"the lock is busy":    {busy, []string{"orchestrate A was not applied", "session lock", "s1.json.lock", "another process holds it"}},
		"the write is failed": {errors.New("disk exploded"), []string{"orchestrate A was not applied", "the session state could not be written", "disk exploded"}},
	} {
		cwd := t.TempDir()
		promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
			s.Phase, s.OrchestrationActive = state.PhaseP, true
		})
		calls := 0
		lock := func(cwd, sessionID string, fn func() error) error {
			if calls++; calls == 2 { // the stamp is the first lock, the command write the second
				return c.fail
			}
			return state.WithSessionLock(cwd, sessionID, fn)
		}
		got := promptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: "orchestrate A", TurnID: "t1", PabcdEnabled: true}, "", promptSubmitHost(cwd), lock)
		if calls != 2 {
			t.Fatalf("%s: %d lock calls; want the stamp and the command write", name, calls)
		}
		for _, want := range append(c.want, "The phase and ledger were not changed.") {
			if !strings.Contains(got, want) {
				t.Fatalf("%s: the answer %q lacks %q", name, got, want)
			}
		}
		if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseP {
			t.Fatalf("%s: phase %s; want P", name, s.Phase)
		}
	}
}

// A command refused for a reason of its own, the state having changed under the handler, keeps the generic refusal.
func TestChatOrchestrateRefusedWithoutALockFailureKeepsTheGenericRefusal(t *testing.T) {
	cwd := t.TempDir()
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.OrchestrationActive = state.PhaseP, true
	})
	calls := 0
	lock := func(cwd, sessionID string, fn func() error) error {
		if calls++; calls == 2 { // a participating writer moves the phase between the handler's read and the command's lock
			if err := state.WithSessionLock(cwd, sessionID, func() error {
				s, _ := state.ReadStateStrict(cwd, sessionID)
				s.Phase = state.PhaseA
				return state.WriteState(cwd, s)
			}); err != nil {
				t.Fatal(err)
			}
		}
		return state.WithSessionLock(cwd, sessionID, fn)
	}
	got := promptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: "orchestrate A", TurnID: "t1", PabcdEnabled: true}, "", promptSubmitHost(cwd), lock)
	if !strings.Contains(got, "refused: the session state changed") {
		t.Fatalf("the answer %q is not the generic refusal", got)
	}
}
