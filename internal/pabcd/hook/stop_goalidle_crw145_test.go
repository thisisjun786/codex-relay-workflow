package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-145 (the block names the honest exits first and drops the arming option that cannot run): the cases pin the block text for
// a git and a non-git workspace. The rigs are stop_goalidle_crw386_test.go's.

const (
	stopIdleHead = "[crw — goal continuation] A host goal is ACTIVE but no PABCD cycle is in flight.\n" +
		"GOAL-IDLE-CONTINUE-01: IDLE is not the end while the goal is active (LOOP-CONTINUE-01). Do not end the turn here.\n" +
		"Close the goal honestly: `update_goal` status \"complete\" (only when the recorded criteria are proven — the E8 gate checks a bound goalplan) or status \"blocked\" for an external blocker.\n"
	stopIdleChainFmt = "LOOP-UNIT-CHAIN-01: work-phases chain HETEROGENEOUS units in one session — an independent feature/plan discovered mid-loop is simply the NEXT work-phase (append it to the goalplan%s). \"Needs its own PABCD\" is a plan statement, not a session boundary; do not close the goal while naming remaining features that fit the objective.\n"
	stopIdleWork     = "Ready work phases: wp1 (Exporter)\nRequired evidence: green\nRecord progress in: .crw/goalplans/export/ledger.jsonl"
)

func stopIdleChain(suffix string) string { return strings.Replace(stopIdleChainFmt, "%s", suffix, 1) }

// CRW-145: the honest exits come first, and the option to arm the next work-phase follows them (a git workspace, where the
// command can run).
func TestStopGoalIdleBlockNamesTheHonestExitsFirst(t *testing.T) {
	cwd, env := stopIdleRig(t, true)
	got := stopBlockReason(t, stopRun(cwd, env))
	want := stopIdleHead +
		"Or start the next work-phase now: `CRW pabcd orchestrate P --session rec-s1`\n" +
		stopIdleChain(", then orchestrate P") + stopIdleWork
	if got != want {
		t.Errorf("git workspace:\n got %q\nwant %q", got, want)
	}
}

// CRW-145: in a workspace with no resolvable git source identity `orchestrate P` is refused for a goalplan-bound session
// (CheckBound), so the block does not offer it and says why; the honest exits, the counter and the other lines are unchanged.
func TestStopGoalIdleBlockDropsTheArmingOptionWithoutAGitSource(t *testing.T) {
	cwd, env := stopIdleRig(t, false)
	got := stopBlockReason(t, stopRun(cwd, env))
	want := stopIdleHead +
		"The next work-phase cannot be armed from this workspace: it has no resolvable git source identity, and a goalplan-bound cycle cannot be closed without one (C -> D needs a test receipt bound to a commit), so entering P is refused. To make arming possible, bind a git source tree: `CRW relay session source <absolute-path> --json`.\n" +
		stopIdleChain("") + stopIdleWork
	if got != want {
		t.Errorf("non-git workspace:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "orchestrate P") {
		t.Errorf("the block still names orchestrate P: %q", got)
	}
	// The gate strength is unchanged: the block is counted at IDLE as before and the per-phase cap of three still releases.
	if s := state.ReadState(cwd, stopSID); s.StopBlockPhase == nil || *s.StopBlockPhase != state.PhaseIdle || s.StopBlockCount != 1 || s.StopBlockTotal != 1 {
		t.Errorf("the block is counted at IDLE: %+v", s)
	}
	for i := 2; i <= 3; i++ {
		stopBlockReason(t, stopRun(cwd, env))
	}
	if a := stopRun(cwd, env); a != (StopAnswer{}) {
		t.Errorf("the fourth consecutive block must release: %+v", a)
	}
}

// CRW-145 (the verifier's correction): the predicate is the source identity, not the absence of .git. A non-git working directory
// whose session is bound to a git source worktree can run `orchestrate P`, so the option stays.
func TestStopGoalIdleBlockKeepsTheArmingOptionForABoundGitSource(t *testing.T) {
	cwd, env := stopIdleRig(t, false)
	source := filepath.Join(filepath.Dir(cwd), "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	stopMakeRepo(t, source)
	if _, err := session.Bind(cwd, stopSID, source); err != nil {
		t.Fatal(err)
	}
	got := stopBlockReason(t, stopRun(cwd, env))
	want := stopIdleHead +
		"Or start the next work-phase now: `CRW pabcd orchestrate P --session rec-s1`\n" +
		stopIdleChain(", then orchestrate P") + stopIdleWork
	if got != want {
		t.Errorf("non-git cwd with a bound git source:\n got %q\nwant %q", got, want)
	}
}
