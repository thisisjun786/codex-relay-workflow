package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// The A>B review binding and the B>C source gate, ported from CXC v0.2.40 orchestrate-cli.ts
// (validateReviewBinding :49-95 and the B>C block :701-717) with the security fix the issue body
// names. Every case drives the real transition through orchestrateTransitionRun, so the gate runs
// where the verb path reaches it (the verb row itself is CRW-758's).

// orchestrateReviewBindingPlanUnit seeds a numbered plan doc and returns its unit path.
func orchestrateReviewBindingPlanUnit(t *testing.T, cwd string) string {
	t.Helper()
	unit := "devlog/_plan/000000_review-binding"
	orchestrateTransitionPut(t, filepath.Join(cwd, filepath.FromSlash(unit), "000_plan.md"), "# 000 - review binding\n")
	return unit
}

// orchestrateReviewBindingSeed writes a bound goalplan holding the given rounds and a session at the
// phase asked for. The rounds go in by hand, because a round the writer would refuse - a plan-file
// entry that leaves the workspace, a recorded verdict without a binding - is the input under test.
func orchestrateReviewBindingSeed(t *testing.T, cwd, id, unit string, rounds []map[string]any, planEpoch *string, phase string) {
	t.Helper()
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: id})
	plan.Slug = id
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp1", Title: "one", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	wp := "wp1"
	plan.ActiveWorkPhaseID = &wp
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cwd, ".crw", "goalplans", id, "goalplan.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["reviewRounds"] = rounds
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	session := map[string]any{"phase": phase, "sessionId": id, "slug": id, "planUnit": unit}
	if planEpoch != nil {
		session["planEpoch"] = *planEpoch
	}
	body, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	orchestrateTransitionSession(t, cwd, id, string(body))
}

// orchestrateReviewBindingVerdictRound is a plan_audit round the oracle's reader keeps whole: a
// recorded verdict, the terminal status that verdict produces and the binding fields asked for.
// An empty field is left out, so a case can seed exactly the incomplete binding it means to.
func orchestrateReviewBindingVerdictRound(id, owner, workPhase, epoch, verdict string, files any) map[string]any {
	status := "approved"
	if verdict == "fail" {
		status = "changes_requested"
	}
	round := map[string]any{
		"roundId": id, "purpose": "plan_audit", "planPath": "devlog/_plan/000000_review-binding/000_plan.md",
		"planSha256": "sha", "status": status,
		"lane":     map[string]any{"launchId": id + "-20260101000000", "verdict": verdict},
		"openedAt": "2026-01-01T00:00:00.000Z",
	}
	if owner != "" {
		round["ownerSessionId"] = owner
	}
	if workPhase != "" {
		round["workPhaseId"] = workPhase
	}
	if epoch != "" {
		round["planEpoch"] = epoch
	}
	if files != nil {
		round["planFiles"] = files
	}
	return round
}

// orchestrateReviewBindingBoundFiles is the plan-file list of a complete binding and the aggregate
// hash the gate recomputes from it.
func orchestrateReviewBindingBoundFiles(t *testing.T, cwd, unit string) (any, string) {
	t.Helper()
	rel := filepath.ToSlash(filepath.Join(unit, "000_plan.md"))
	files := []map[string]any{{"path": rel, "sha256": "k"}}
	return files, PlanFilesHash(Recomputed(cwd, []goalplan.PlanFileHash{{Path: rel, Sha256: "k"}}))
}

func orchestrateReviewBindingAttest(verdict string) string {
	body := map[string]any{"from": "A", "to": "B", "did": "audited the plan", "auditOutput": "VERDICT: PASS",
		"auditVerdict": verdict, "workPhaseId": "wp1"}
	raw, _ := json.Marshal(body)
	return string(raw)
}

// TestOrchestrateReviewBindingSecurityFix is the red-first case of the issue: a round whose planFiles
// list was dropped because one entry named a path outside the workspace still carries a recorded FAIL
// verdict, and the oracle waves it through (:78-80 returns null), so the reviewer's FAIL is ignored.
// The port refuses the A>B edge with the text the issue body fixes.
func TestOrchestrateReviewBindingSecurityFix(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "review-binding-security"
	unit := orchestrateReviewBindingPlanUnit(t, cwd)
	epoch := "e-plan-1"
	round := orchestrateReviewBindingVerdictRound("r1", id, "wp1", epoch, "fail", nil)
	round["planFiles"] = []map[string]any{{"path": "keep", "sha256": "k"}, {"path": "../outside", "sha256": "o"}}
	orchestrateReviewBindingSeed(t, cwd, id, unit, []map[string]any{round}, &epoch, "A")

	got := orchestrateTransitionRun(t, cwd, "B", "--session", id, "--attest", orchestrateReviewBindingAttest("pass"))
	want := "orchestrate B: current=A session=" + id + "; the latest plan_audit round has a recorded verdict but no complete binding (owner session, work-phase, plan epoch and plan files), so its verdict cannot be checked. Re-run the audit round before entering B. Nothing was written."
	if got.Code != 1 || got.Output != want {
		t.Fatalf("A>B refusal\n got: %+v\nwant: %s", got, want)
	}
	if after := state.ReadState(cwd, id); after.Phase != state.PhaseA {
		t.Fatalf("the refusal wrote state: %+v", after)
	}
	if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 0 {
		t.Fatalf("the refusal wrote a ledger row: %+v", rows)
	}
}

// TestOrchestrateReviewBindingRecordedBadEntryNull is the recorded corpus case the issue names
// (files_one_bad_entry_null in internal/pabcd/goalplan/testdata/oracle-revive.json): the planFiles
// list is [good, null], the reader drops the whole list, and the recorded verdict must still hold.
func TestOrchestrateReviewBindingRecordedBadEntryNull(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "review-binding-recorded"
	unit := orchestrateReviewBindingPlanUnit(t, cwd)
	epoch := "e-plan-1"
	round := orchestrateReviewBindingVerdictRound("r1", id, "wp1", epoch, "fail", nil)
	round["planFiles"] = []any{map[string]any{"path": "keep", "sha256": "k"}, nil}
	orchestrateReviewBindingSeed(t, cwd, id, unit, []map[string]any{round}, &epoch, "A")

	got := orchestrateTransitionRun(t, cwd, "B", "--session", id, "--attest", orchestrateReviewBindingAttest("pass"))
	if got.Code != 1 || !strings.Contains(got.Output, "no complete binding") {
		t.Fatalf("recorded bad-entry case\n got: %+v", got)
	}
}

// TestOrchestrateReviewBindingOpenRoundPasses is the oracle's other half: a round with no RECORDED
// verdict is not a blocker (LEAN-REVIEW-01), so the attest alone advances the edge.
func TestOrchestrateReviewBindingOpenRoundPasses(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "review-binding-open"
	unit := orchestrateReviewBindingPlanUnit(t, cwd)
	epoch := "e-plan-1"
	round := map[string]any{
		"roundId": "r1", "purpose": "plan_audit", "planPath": "devlog/_plan/000000_review-binding/000_plan.md",
		"planSha256": "sha", "status": "in_flight",
		"lane":     map[string]any{"launchId": "r1-20260101000000"},
		"openedAt": "2026-01-01T00:00:00.000Z",
	}
	orchestrateReviewBindingSeed(t, cwd, id, unit, []map[string]any{round}, &epoch, "A")
	got := orchestrateTransitionRun(t, cwd, "B", "--session", id, "--attest", orchestrateReviewBindingAttest("pass"))
	if got.Code != 0 {
		t.Fatalf("an open round must pass: %+v", got)
	}
	if after := state.ReadState(cwd, id); after.Phase != state.PhaseB {
		t.Fatalf("phase: %+v", after)
	}
}

// TestOrchestrateReviewBindingBoundApprovePasses is the oracle's ordinary passing case: a complete
// binding, an APPROVE verdict and a plan whose recomputed hash still matches.
func TestOrchestrateReviewBindingBoundApprovePasses(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "review-binding-approve"
	unit := orchestrateReviewBindingPlanUnit(t, cwd)
	epoch := "e-plan-1"
	files, hash := orchestrateReviewBindingBoundFiles(t, cwd, unit)
	round := orchestrateReviewBindingVerdictRound("r1", id, "wp1", epoch, "pass", files)
	round["planSha256"] = hash
	orchestrateReviewBindingSeed(t, cwd, id, unit, []map[string]any{round}, &epoch, "A")
	got := orchestrateTransitionRun(t, cwd, "B", "--session", id, "--attest", orchestrateReviewBindingAttest("pass"))
	if got.Code != 0 {
		t.Fatalf("a bound APPROVE must pass: %+v", got)
	}
}

// TestOrchestrateReviewBindingBoundFailRefuses is the oracle's own refusal (:90-91): a recorded FAIL
// is honoured even when the agent attests pass.
func TestOrchestrateReviewBindingBoundFailRefuses(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "review-binding-fail"
	unit := orchestrateReviewBindingPlanUnit(t, cwd)
	epoch := "e-plan-1"
	files, hash := orchestrateReviewBindingBoundFiles(t, cwd, unit)
	round := orchestrateReviewBindingVerdictRound("r1", id, "wp1", epoch, "fail", files)
	round["planSha256"] = hash
	orchestrateReviewBindingSeed(t, cwd, id, unit, []map[string]any{round}, &epoch, "A")
	got := orchestrateTransitionRun(t, cwd, "B", "--session", id, "--attest", orchestrateReviewBindingAttest("pass"))
	want := "orchestrate B: current=A session=" + id + "; you attested \"pass\" but the reviewer recorded \"fail\" (LEAN-REVIEW-01)."
	if got.Code != 1 || !strings.HasPrefix(got.Output, want) {
		t.Fatalf("bound FAIL\n got: %+v\nwant prefix: %s", got, want)
	}
}

// TestOrchestrateReviewBindingForeignSessionRefuses is the oracle's first binding refusal (:81): an
// approval recorded for another session cannot be spent here.
func TestOrchestrateReviewBindingForeignSessionRefuses(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "review-binding-foreign"
	unit := orchestrateReviewBindingPlanUnit(t, cwd)
	epoch := "e-plan-1"
	files, hash := orchestrateReviewBindingBoundFiles(t, cwd, unit)
	round := orchestrateReviewBindingVerdictRound("r1", "somebody-else", "wp1", epoch, "pass", files)
	round["planSha256"] = hash
	orchestrateReviewBindingSeed(t, cwd, id, unit, []map[string]any{round}, &epoch, "A")
	got := orchestrateTransitionRun(t, cwd, "B", "--session", id, "--attest", orchestrateReviewBindingAttest("pass"))
	if got.Code != 1 || !strings.Contains(got.Output, "was approved for a different session") {
		t.Fatalf("foreign session\n got: %+v", got)
	}
}

// TestOrchestrateReviewBindingStaleEpochRefuses is the oracle's second binding refusal (:82): an
// approval from an earlier plan epoch cannot survive a re-plan.
func TestOrchestrateReviewBindingStaleEpochRefuses(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "review-binding-epoch"
	unit := orchestrateReviewBindingPlanUnit(t, cwd)
	epoch := "e-plan-2"
	files, hash := orchestrateReviewBindingBoundFiles(t, cwd, unit)
	round := orchestrateReviewBindingVerdictRound("r1", id, "wp1", "e-plan-1", "pass", files)
	round["planSha256"] = hash
	orchestrateReviewBindingSeed(t, cwd, id, unit, []map[string]any{round}, &epoch, "A")
	got := orchestrateTransitionRun(t, cwd, "B", "--session", id, "--attest", orchestrateReviewBindingAttest("pass"))
	if got.Code != 1 || !strings.Contains(got.Output, "approved an earlier plan") {
		t.Fatalf("stale epoch\n got: %+v", got)
	}
}

// TestOrchestrateReviewBindingOtherWorkPhaseRefuses is the oracle's third binding refusal (:83-84).
func TestOrchestrateReviewBindingOtherWorkPhaseRefuses(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "review-binding-workphase"
	unit := orchestrateReviewBindingPlanUnit(t, cwd)
	epoch := "e-plan-1"
	files, hash := orchestrateReviewBindingBoundFiles(t, cwd, unit)
	round := orchestrateReviewBindingVerdictRound("r1", id, "wp9", epoch, "pass", files)
	round["planSha256"] = hash
	orchestrateReviewBindingSeed(t, cwd, id, unit, []map[string]any{round}, &epoch, "A")
	got := orchestrateTransitionRun(t, cwd, "B", "--session", id, "--attest", orchestrateReviewBindingAttest("pass"))
	if got.Code != 1 || !strings.Contains(got.Output, "approved work-phase wp9, but wp1 is active") {
		t.Fatalf("other work-phase\n got: %+v", got)
	}
}

// TestOrchestrateReviewBindingChangedPlanRefuses is the oracle's fourth binding refusal (:85-86): the
// plan changed after the round approved it, so the recomputed hash no longer matches.
func TestOrchestrateReviewBindingChangedPlanRefuses(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "review-binding-changed"
	unit := orchestrateReviewBindingPlanUnit(t, cwd)
	epoch := "e-plan-1"
	files, _ := orchestrateReviewBindingBoundFiles(t, cwd, unit)
	round := orchestrateReviewBindingVerdictRound("r1", id, "wp1", epoch, "pass", files)
	round["planSha256"] = "a-hash-the-plan-no-longer-has"
	orchestrateReviewBindingSeed(t, cwd, id, unit, []map[string]any{round}, &epoch, "A")
	got := orchestrateTransitionRun(t, cwd, "B", "--session", id, "--attest", orchestrateReviewBindingAttest("pass"))
	if got.Code != 1 || !strings.Contains(got.Output, "the plan changed after round r1 approved it") {
		t.Fatalf("changed plan\n got: %+v", got)
	}
}

// orchestrateReviewBindingWorktree is a git repository, a linked worktree of it and the session
// bound to that worktree, at phase B with the goalplan the work-phase gate reads.
func orchestrateReviewBindingWorktree(t *testing.T, id string) (cwd, worktree string) {
	t.Helper()
	cwd = orchestrateTransitionRepo(t)
	worktree = filepath.Join(t.TempDir(), "bound")
	orchestrateTransitionGit(t, cwd, "worktree", "add", "-qb", "bound", worktree)
	orchestrateReviewBindingSeed(t, cwd, id, "", nil, nil, "A")
	if _, err := session.Bind(cwd, id, worktree); err != nil {
		t.Fatal(err)
	}
	return cwd, worktree
}

func orchestrateTransitionGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// orchestrateReviewBindingEntrySource writes the session at B with the B-entry snapshot recorded.
func orchestrateReviewBindingEntrySource(t *testing.T, cwd, id, sourceRoot, commit string) {
	t.Helper()
	entry := map[string]any{"kind": "resolved", "commitSha": commit, "dirty": false,
		"capturedAt": "2026-01-01T00:00:00.000Z", "sourceRoot": sourceRoot}
	body, err := json.Marshal(map[string]any{"phase": "B", "sessionId": id, "slug": id, "phaseEntrySource": entry})
	if err != nil {
		t.Fatal(err)
	}
	orchestrateTransitionSession(t, cwd, id, string(body))
}

func orchestrateReviewBindingToCAttest() string {
	raw, _ := json.Marshal(map[string]any{"from": "B", "to": "C", "did": "implemented the gate", "workPhaseId": "wp1"})
	return string(raw)
}

// TestOrchestrateReviewBindingSourceGateNoBaseline ports the oracle's first B>C source refusal
// (:701-703): a bound source other than the session's own cwd with no B-entry snapshot has no
// baseline to compare against.
func TestOrchestrateReviewBindingSourceGateNoBaseline(t *testing.T) {
	cwd, _ := orchestrateReviewBindingWorktree(t, "source-gate-baseline")
	orchestrateReviewBindingSeed(t, cwd, "source-gate-baseline", "", nil, nil, "B")
	got := orchestrateTransitionRun(t, cwd, "C", "--session", "source-gate-baseline", "--attest", orchestrateReviewBindingToCAttest())
	want := "orchestrate C: SOURCE-ROOT: bound source has no valid B baseline. Re-plan before continuing."
	if got.Code != 1 || got.Output != want {
		t.Fatalf("no baseline\n got: %+v\nwant: %s", got, want)
	}
}

// TestOrchestrateReviewBindingSourceGateChangedRoot ports the oracle's second B>C refusal (:706-708):
// the source root moved since B began.
func TestOrchestrateReviewBindingSourceGateChangedRoot(t *testing.T) {
	cwd, _ := orchestrateReviewBindingWorktree(t, "source-gate-root")
	orchestrateReviewBindingEntrySource(t, cwd, "source-gate-root", filepath.Join(t.TempDir(), "elsewhere"), "deadbeef")
	got := orchestrateTransitionRun(t, cwd, "C", "--session", "source-gate-root", "--attest", orchestrateReviewBindingToCAttest())
	want := "orchestrate C: SOURCE-ROOT: source binding changed since B began. Re-plan and capture a new baseline; nothing was written."
	if got.Code != 1 || got.Output != want {
		t.Fatalf("changed root\n got: %+v\nwant: %s", got, want)
	}
}

// TestOrchestrateReviewBindingSourceGateUnchanged ports the oracle's SOURCE-DELTA-01 refusal
// (:710-716): B implemented nothing, so the source reads exactly as it did on entry.
func TestOrchestrateReviewBindingSourceGateUnchanged(t *testing.T) {
	cwd, worktree := orchestrateReviewBindingWorktree(t, "source-gate-delta")
	head := strings.TrimSpace(orchestrateReviewBindingGitOut(t, worktree, "rev-parse", "HEAD"))
	orchestrateReviewBindingEntrySource(t, cwd, "source-gate-delta", worktree, head)
	got := orchestrateTransitionRun(t, cwd, "C", "--session", "source-gate-delta", "--attest", orchestrateReviewBindingToCAttest())
	if got.Code != 1 || !strings.Contains(got.Output, "the source is unchanged since B began (") ||
		!strings.Contains(got.Output, "SOURCE-DELTA-01)") {
		t.Fatalf("unchanged source\n got: %+v", got)
	}
}

// TestOrchestrateReviewBindingSourceGateNormalPass ports the oracle's passing B>C (:701-717): the
// source moved during B, so the edge advances.
func TestOrchestrateReviewBindingSourceGateNormalPass(t *testing.T) {
	cwd, worktree := orchestrateReviewBindingWorktree(t, "source-gate-pass")
	head := strings.TrimSpace(orchestrateReviewBindingGitOut(t, worktree, "rev-parse", "HEAD"))
	orchestrateReviewBindingEntrySource(t, cwd, "source-gate-pass", worktree, head)
	// B implemented something: a new commit in the bound worktree.
	orchestrateTransitionPut(t, filepath.Join(worktree, "implemented.txt"), "done\n")
	orchestrateTransitionGit(t, worktree, "add", "implemented.txt")
	orchestrateTransitionGit(t, worktree, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "implemented")
	got := orchestrateTransitionRun(t, cwd, "C", "--session", "source-gate-pass", "--attest", orchestrateReviewBindingToCAttest())
	if got.Code != 0 {
		t.Fatalf("a moved source must pass: %+v", got)
	}
	if after := state.ReadState(cwd, "source-gate-pass"); after.Phase != state.PhaseC {
		t.Fatalf("phase: %+v", after)
	}
}

func orchestrateReviewBindingGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

// TestOrchestrateReviewBindingDispatchAdviceNamesTheReviewerRole is the reviewer-producer-contract
// case the issue names. The contract's point is that the CONSUMER is executed rather than that a
// particular sentence survived an edit, so this runs the consumer: the role the A>B advice names
// must resolve, through the store the spawn path reads, to the reviewer configuration.
//
// The producer half is the A>B advice TEXT, which attest.Validate emits (attest.ts :188, outside this
// issue's edit regions and therefore read, not changed). It carries the oracle's clause: the native
// agent_type "reviewer", otherwise agent_type "explorer" with a CRW-ROLE: reviewer marker, and an
// explicit omission instruction for a host with no agent_type field. The inferRole half of the
// consumer lives in internal/role/spawn, which reaches this package through internal/harness, so it
// is exercised in that package's own tests rather than from here.
func TestOrchestrateReviewBindingDispatchAdviceNamesTheReviewerRole(t *testing.T) {
	_ = orchestrateTransitionRoot(t)
	env := host.LookupEnv(func(key string) (string, bool) { return os.LookupEnv(key) })
	if _, err := role.SetRole(env, role.Reviewer, role.RolePatch{
		Mode: role.Some(role.ModeModel), Model: role.Some("reviewer-only"),
		Effort: role.Some(role.EffortHigh), PromptOverride: role.Some("Audit the change."),
	}); err != nil {
		t.Fatal(err)
	}
	refusal := attest.Validate(state.PhaseA, state.PhaseB, &attest.Attestation{From: state.PhaseA, To: state.PhaseB, Did: "Inspected the proposed patch."})
	if refusal.OK || refusal.Reason == "" {
		t.Fatalf("A>B without auditOutput must refuse with the dispatch advice: %+v", refusal)
	}
	if !strings.Contains(refusal.Reason, "omit agent_type") {
		t.Fatalf("a field-less host needs an executable omission instruction: %s", refusal.Reason)
	}
	for _, want := range []string{"agent_type \"reviewer\"", "agent_type \"explorer\"", "CRW-ROLE: reviewer before TASK:"} {
		if !strings.Contains(refusal.Reason, want) {
			t.Fatalf("the A>B advice does not carry %q: %s", want, refusal.Reason)
		}
	}
	resolved, err := role.ResolveSpawnConfig(env, role.Reviewer)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Model == nil || *resolved.Model != "reviewer-only" || resolved.Effort == nil || *resolved.Effort != role.EffortHigh ||
		resolved.PromptOverride == nil || *resolved.PromptOverride != "Audit the change." {
		t.Fatalf("the reviewer role resolved to %+v, want the reviewer configuration", resolved)
	}
}

// CRW-1113 (A3-06, port: fixed). A reviewer's FAIL that met the goalplan lock was dropped, so the A>B check saw a round with no
// verdict and let the attest through. The observer now keeps the sign-off, and the A>B transition drains it under the session
// lock it already holds before it judges the binding: the kept FAIL is honoured, and a kept PASS counts as the reviewer's.
func TestOrchestrateReviewBindingDrainsAKeptSignoffBeforeTheCheck(t *testing.T) {
	for _, verdict := range []string{"fail", "pass"} {
		t.Run(verdict, func(t *testing.T) {
			cwd := orchestrateTransitionRoot(t)
			id := "review-binding-inbox-" + verdict
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

			got := orchestrateTransitionRun(t, cwd, "B", "--session", id, "--attest", orchestrateReviewBindingAttest("pass"))
			if verdict == "pass" {
				if got.Code != 0 {
					t.Fatalf("a kept PASS is the reviewer's verdict: %+v", got)
				}
				return
			}
			want := "orchestrate B: current=A session=" + id + "; you attested \"pass\" but the reviewer recorded \"fail\" (LEAN-REVIEW-01)."
			if got.Code != 1 || !strings.HasPrefix(got.Output, want) {
				t.Fatalf("a kept FAIL is honoured\n got: %+v\nwant prefix: %s", got, want)
			}
		})
	}
}
