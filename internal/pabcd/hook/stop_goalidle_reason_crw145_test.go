package hook

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sourcesession "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-145 (verification round 2): the goal-idle block names the reason CheckBound actually refused arming for, and offers
// binding a git source tree only where binding one clears the refusal; the source investigation runs only for a Stop whose
// counter leaves a block.

const stopIdleRefusedFmt = "The next work-phase cannot be armed from this workspace: the session's source identity cannot be resolved (%s), and a goalplan-bound cycle cannot be closed without one (C -> D needs a test receipt bound to a commit), so entering P is refused. Binding a different source tree does not clear this (a session's source binding is immutable); resolve what the reason names first.\n"

func stopIdleRefused(reason string) string {
	return strings.Replace(stopIdleRefusedFmt, "%s", reason, 1)
}

// A corrupt source binding is not a missing source: `crw relay session source` refuses it with the same error, so the block
// names that error and does not offer the bind command.
func TestStopGoalIdleBlockNamesACorruptBindingRefusal(t *testing.T) {
	cwd, env := stopIdleRig(t, false)
	dir := filepath.Join(cwd, ".crw", sourcesession.SourcesSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stopSID+".json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := stopBlockReason(t, stopRun(cwd, env))
	want := stopIdleHead +
		stopIdleRefused("Source binding is unreadable or corrupt; existing bytes were preserved.") +
		stopIdleChain("") + stopIdleWork
	if got != want {
		t.Errorf("corrupt binding:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "relay session source") || strings.Contains(got, "orchestrate P") {
		t.Errorf("the block offers a command that cannot clear the refusal: %q", got)
	}
}

// A bound source worktree that yields no identity is not cleared by binding either (the binding is immutable): the block says
// the bound worktree is the problem.
func TestStopGoalIdleBlockNamesABoundSourceWithoutIdentity(t *testing.T) {
	cwd, env := stopIdleRig(t, false)
	src := defaultStopSource()
	src.check = func(context.Context, string, string) sourcesession.GateResult {
		return sourcesession.GateResult{Reason: "oracle text", Cause: sourcesession.CauseBoundUnavailable}
	}
	got := stopBlockReason(t, stopRunWith(cwd, env, src))
	want := stopIdleHead +
		stopIdleRefused("its bound source worktree has no resolvable git source identity") +
		stopIdleChain("") + stopIdleWork
	if got != want {
		t.Errorf("bound source without identity:\n got %q\nwant %q", got, want)
	}
}

// A git that could not answer is named with its failure, and the bind command and the arming command are both withheld.
func TestStopGoalIdleBlockNamesAFailedGitProbe(t *testing.T) {
	cwd, env := stopIdleRig(t, false)
	src := defaultStopSource()
	src.check = func(context.Context, string, string) sourcesession.GateResult {
		return sourcesession.GateResult{Reason: "oracle text", Cause: sourcesession.CauseProbeFailed, Probe: errors.New("git exited with\nstatus 128")}
	}
	got := stopBlockReason(t, stopRunWith(cwd, env, src))
	want := stopIdleHead +
		"The next work-phase cannot be armed from this workspace: git could not read the source tree (git exited with status 128), and a goalplan-bound cycle cannot be closed without a resolved source identity (C -> D needs a test receipt bound to a commit), so entering P is refused. Binding a git source tree does not clear this (binding runs the same git); make git work for this workspace first.\n" +
		stopIdleChain("") + stopIdleWork
	if got != want {
		t.Errorf("failed git probe:\n got %q\nwant %q", got, want)
	}
}

// stopCountChecks returns the source check of a Stop that counts its calls and answers "cannot arm, unbound", with the counter.
func stopCountChecks() (*int, stopSource) {
	calls := 0
	src := defaultStopSource()
	src.check = func(context.Context, string, string) sourcesession.GateResult {
		calls++
		return sourcesession.GateResult{Reason: "none", Cause: sourcesession.CauseUnavailable}
	}
	return &calls, src
}

// The total-cap release answers only the systemMessage; it investigates no source.
func TestStopGoalIdleTotalCapReleaseInvestigatesNoSource(t *testing.T) {
	cwd, env := stopIdleRig(t, false)
	sessionHookStateFile(t, cwd, stopSID, func(s *state.State) { s.StopBlockTotal = StopMaxBlocksTotal })
	calls, src := stopCountChecks()
	if a := stopRunWith(cwd, env, src); a.Stdout != stopSystemMessage(stopTotalCapMessage) {
		t.Fatalf("not the total-cap release: %+v", a)
	}
	if *calls != 0 {
		t.Errorf("the total-cap release investigated the source %d times", *calls)
	}
}

// A spent per-phase budget releases without investigating the source, on the first release and on the latched ones after it.
func TestStopGoalIdlePhaseCapReleaseInvestigatesNoSource(t *testing.T) {
	cwd, env := stopIdleRig(t, false)
	calls, src := stopCountChecks()
	for i := 1; i <= StopMaxBlocks; i++ {
		stopBlockReason(t, stopRunWith(cwd, env, src))
	}
	for i := 0; i < 2; i++ {
		if a := stopRunWith(cwd, env, src); a != (StopAnswer{}) {
			t.Fatalf("release %d after the per-phase cap: %+v", i+1, a)
		}
	}
	if *calls != StopMaxBlocks {
		t.Errorf("the source was investigated %d times for %d blocks", *calls, StopMaxBlocks)
	}
}
