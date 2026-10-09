package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1113 correction round 3. The A>B transition drains the reviewer sign-offs the observer kept. Two things it must hold to:
// an inbox it cannot read is not an empty inbox (a kept FAIL is never lost or stepped over), and the drain is the command's first
// durable effect, so a cancelled invocation writes nothing before it answers Interrupted (130).

// orchestrateReviewInboxKept seeds a session at A with an in-flight plan_audit round and keeps one reviewer sign-off for it
// (the observer met a held goalplan lock). It returns the workspace, the session and the kept entry's path.
func orchestrateReviewInboxKept(t *testing.T, id, verdict string) (cwd, entry string) {
	t.Helper()
	cwd = orchestrateTransitionRoot(t)
	unit := orchestrateReviewBindingPlanUnit(t, cwd)
	epoch := "e-plan-1"
	files, hash := orchestrateReviewBindingBoundFiles(t, cwd, unit)
	round := orchestrateReviewBindingVerdictRound("r1", id, "wp1", epoch, "pass", files)
	round["planSha256"], round["status"], round["lane"] = hash, "in_flight", map[string]any{"launchId": "r1-20260101000000"}
	orchestrateReviewBindingSeed(t, cwd, id, unit, []map[string]any{round}, &epoch, "A")
	lock := filepath.Join(cwd, ".crw", "goalplans", id, goalplan.GoalplanLockDir)
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lock, "owner.json"), []byte(`{"pid":4242}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"hook_event_name": "SubagentStop", "cwd": cwd, "session_id": id, "agent_type": "explorer",
		"agent_id": "reviewer-1", "last_assistant_message": "done\nLAUNCH: r1-20260101000000\nVERDICT: " + strings.ToUpper(verdict)})
	if out := hook.HandleReviewObserver(string(payload)); out != "" {
		t.Fatalf("the observer answers nothing: %q", out)
	}
	if err := os.RemoveAll(lock); err != nil {
		t.Fatal(err)
	}
	kept, _ := filepath.Glob(filepath.Join(cwd, ".crw", "review-inbox", "*", "*.json"))
	if len(kept) != 1 {
		t.Fatalf("setup: one kept sign-off: %v", kept)
	}
	return cwd, kept[0]
}

func orchestrateReviewInboxRound(t *testing.T, cwd, id string) goalplan.ReviewRoundState {
	t.Helper()
	round := review.LatestRound(goalplan.ReadGoalplan(cwd, id), goalplan.PurposePlanAudit)
	if round == nil {
		t.Fatal("the plan lost its round")
	}
	return *round
}

// d1: a kept FAIL the drain cannot read (access refused, not a corrupt file) is neither deleted nor stepped over: the edge is
// refused, nothing moves, and the entry is judged once it can be read again.
func TestOrchestrateReviewBindingKeepsAnUnreadableSignoffAndRefusesTheEdge(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	for _, what := range []string{"entry file", "session inbox directory"} {
		t.Run(what, func(t *testing.T) {
			id := "review-binding-unreadable"
			cwd, entry := orchestrateReviewInboxKept(t, id, "fail")
			blocked := entry
			if what != "entry file" {
				blocked = filepath.Dir(entry)
			}
			if err := os.Chmod(blocked, 0); err != nil {
				t.Fatal(err)
			}
			restore := func() { _ = os.Chmod(blocked, map[bool]os.FileMode{true: 0o644, false: 0o755}[what == "entry file"]) }
			t.Cleanup(restore)
			got := orchestrateCommitRunOK(t, cwd, nil, "B", "--session", id, "--attest", orchestrateReviewBindingAttest("pass"))
			restore()
			if got.Code != 1 {
				t.Fatalf("a kept sign-off that cannot be read refuses the edge: %+v", got)
			}
			if after := state.ReadState(cwd, id); after.Phase != state.PhaseA {
				t.Fatalf("the refused edge moved the session: %+v", after)
			}
			if _, err := os.Lstat(entry); err != nil {
				t.Fatalf("the kept FAIL is still there to be judged: %v", err)
			}
			if r := orchestrateReviewInboxRound(t, cwd, id); r.Lane.Verdict != "" {
				t.Fatalf("nothing was recorded: %+v", r)
			}
			// Readable again: the same transition now honours the kept FAIL.
			again := orchestrateCommitRunOK(t, cwd, nil, "B", "--session", id, "--attest", orchestrateReviewBindingAttest("pass"))
			want := "orchestrate B: current=A session=" + id + "; you attested \"pass\" but the reviewer recorded \"fail\""
			if again.Code != 1 || !strings.HasPrefix(again.Output, want) {
				t.Fatalf("the kept FAIL is honoured once readable\n got: %+v\nwant prefix: %s", again, want)
			}
		})
	}
}

// d2: the drain is the command's first durable effect. A cancelled invocation writes nothing before it answers Interrupted.
func TestOrchestrateReviewBindingCancelledBeforeTheFirstWriteDrainsNothing(t *testing.T) {
	for _, when := range []string{"at the first pre-write check", "while waiting for the goalplan lock"} {
		t.Run(when, func(t *testing.T) {
			id := "review-binding-cancel"
			cwd, entry := orchestrateReviewInboxKept(t, id, "pass")
			before := statusTree(t, cwd)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			seams := &orchestrateCommitSeams{}
			if when == "at the first pre-write check" {
				seams.interrupt = cancel
			} else {
				seams.lockGoalplan = func(cwd, slug string, fn func(*goalplan.Goalplan) (orchestrateCommitOutcome, error)) (goalplan.GoalplanWriteLockResult[orchestrateCommitOutcome], error) {
					cancel()
					return goalplan.WithGoalplanWriteLock(cwd, slug, fn, nil)
				}
			}
			parsed := ParseOrchestrateCliArgs([]string{"B", "--session", id, "--attest", orchestrateReviewBindingAttest("pass")}, cwd)
			got, err := orchestrateCommitRunContext(ctx, *parsed.Args, id, seams)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled transition returned %v, want context.Canceled (%+v)", err, got)
			}
			if _, err := os.Lstat(entry); err != nil {
				t.Fatalf("the cancelled command removed the kept sign-off: %v", err)
			}
			if r := orchestrateReviewInboxRound(t, cwd, id); r.Status != goalplan.ReviewInFlight || r.Lane.Verdict != "" {
				t.Fatalf("the cancelled command recorded the verdict: %+v", r)
			}
			if !reflect.DeepEqual(before, statusTree(t, cwd)) {
				t.Fatalf("the cancelled command changed the workspace")
			}
			if state.ReadState(cwd, id).Phase != state.PhaseA {
				t.Fatal("the cancelled command moved the phase")
			}
		})
	}
}

// d2, the other side: a cancellation that lands once the drain has recorded the verdict does not undo it, and the command finishes
// as a started write does (CRW-871), instead of answering Interrupted for a command that already wrote.
func TestOrchestrateReviewBindingCancelledAfterTheDrainWroteFinishes(t *testing.T) {
	id := "review-binding-cancel-after"
	cwd, entry := orchestrateReviewInboxKept(t, id, "pass")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	seams := &orchestrateCommitSeams{interrupt: func() {
		// 1: before the publication, 2: first check inside the goalplan lock, 3: the check after the binding is judged.
		if calls++; calls == 3 {
			cancel()
		}
	}}
	parsed := ParseOrchestrateCliArgs([]string{"B", "--session", id, "--attest", orchestrateReviewBindingAttest("pass")}, cwd)
	got, err := orchestrateCommitRunContext(ctx, *parsed.Args, id, seams)
	if err != nil {
		t.Fatalf("the drain wrote before the cancel, so the command finishes: %v", err)
	}
	if got.Code != 0 || state.ReadState(cwd, id).Phase != state.PhaseB {
		t.Fatalf("the edge finished: %+v", got)
	}
	if _, err := os.Lstat(entry); !os.IsNotExist(err) {
		t.Fatalf("the judged entry is gone: %v", err)
	}
	if r := orchestrateReviewInboxRound(t, cwd, id); r.Status != goalplan.ReviewApproved {
		t.Fatalf("the verdict stands: %+v", r)
	}
}
