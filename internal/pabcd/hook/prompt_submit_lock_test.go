package hook

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
