// prompt_orchestrate_test.go is the Go form of the chat-command cases of CXC v0.2.40
// pabcd-state/test/hook.test.ts for the unit that owns handleOrchestrateCommand (hook.ts:860-937,
// 1287-1399), chatPlanBinding (105-126) and mintCheckEpoch (128-130). Every case drives the hook
// through PromptSubmitHandle or promptSubmitHandle, the way the leg runs it, so the whole path is
// exercised: the Stop-budget stamp, the turn guard, the command seam, the state write and the
// ledger. Expected texts are built from the directive helpers, so a comparison is byte for byte.
package hook

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// promptOrchestrateAnswer runs one chat command the way the leg does.
func promptOrchestrateAnswer(t *testing.T, cwd, sessionID, turn, prompt string) string {
	t.Helper()
	return PromptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: sessionID, Prompt: prompt,
		TurnID: turn, PabcdEnabled: true}, "", promptSubmitHost(cwd))
}

// promptOrchestrateLedger is the transition ledger the run wrote: one decoded row per line.
func promptOrchestrateLedger(t *testing.T, cwd string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cwd, crwdir.DirName, state.LedgerFile))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("ledger row %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

// promptOrchestrateSeed writes the session state a case starts from.
func promptOrchestrateSeed(t *testing.T, cwd, sessionID string, mutate func(*state.State)) {
	t.Helper()
	promptSubmitStateFile(t, cwd, sessionID, mutate)
}

// TestPromptOrchestratePAttestlessFreePass is hook.test.ts's chat free pass and the corpus fixture
// chat_orchestrate_p_enters_plan: a line-anchored command is the human source, so it advances and
// records a chat ledger row.
func TestPromptOrchestratePAttestlessFreePass(t *testing.T) {
	cwd := t.TempDir()
	want := WithFooter(PhaseDirective(state.PhaseP, nil), state.PhaseP)
	if got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate P"); got != want {
		t.Errorf("the free pass\n got %q\nwant %q", got, want)
	}
	if !strings.Contains(want, "[crw: PLAN]") {
		t.Errorf("the P directive is not the ported text: %q", want)
	}
	s := state.ReadState(cwd, "s1")
	if s.Phase != state.PhaseP || !s.OrchestrationActive || s.LastInjectedPhase == nil || *s.LastInjectedPhase != state.PhaseP {
		t.Errorf("the persisted state: %+v", s)
	}
	if len(s.InjectedTurns) != 1 || s.InjectedTurns[0] != "t1" {
		t.Errorf("the recorded turns: %+v", s.InjectedTurns)
	}
	rows := promptOrchestrateLedger(t, cwd)
	if len(rows) != 1 || rows[0]["from"] != "IDLE" || rows[0]["to"] != "P" || rows[0]["reason"] != "chat" || rows[0]["actor"] != "human" {
		t.Errorf("the ledger rows: %+v", rows)
	}
}

// TestPromptOrchestrateAFreePassFromP is the corpus fixture chat_orchestrate_a_free_pass_from_p: from
// P, a chat command advances without an attestation, the plan binding stays nil (no attestation names
// a unit) and no B entry snapshot or check epoch survives the move.
func TestPromptOrchestrateAFreePassFromP(t *testing.T) {
	cwd := t.TempDir()
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.OrchestrationActive = state.PhaseP, true
	})
	want := WithFooter(PhaseDirective(state.PhaseA, nil), state.PhaseA)
	if got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate A"); got != want {
		t.Errorf("the free pass\n got %q\nwant %q", got, want)
	}
	s := state.ReadState(cwd, "s1")
	if s.Phase != state.PhaseA || s.PlanUnit != nil || s.PlanEpoch != nil || s.PhaseEntrySource != nil || s.CheckEpoch != nil {
		t.Errorf("the persisted state: %+v", s)
	}
	rows := promptOrchestrateLedger(t, cwd)
	if len(rows) != 1 || rows[0]["from"] != "P" || rows[0]["to"] != "A" || rows[0]["reason"] != "chat" {
		t.Errorf("the ledger rows: %+v", rows)
	}
}

// TestPromptOrchestrateIllegalEdgeRefused is the corpus fixture chat_orchestrate_illegal_edge_refused:
// a non-adjacent edge is refused in the injected text and the phase does not move.
func TestPromptOrchestrateIllegalEdgeRefused(t *testing.T) {
	cwd := t.TempDir()
	if got, want := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate C"), "[crw \u2014 refused: illegal transition IDLE->C]"; got != want {
		t.Errorf("the refusal\n got %q\nwant %q", got, want)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseIdle || s.OrchestrationActive {
		t.Errorf("the phase moved: %+v", s)
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 0 {
		t.Errorf("a refused command wrote a ledger row: %+v", rows)
	}
}

// TestPromptOrchestrateStatusIsARead: the status affordance renders the line and changes neither the
// state nor the ledger.
func TestPromptOrchestrateStatusIsARead(t *testing.T) {
	cwd := t.TempDir()
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.Flags.AuditPassed = state.PhaseP, true, true
	})
	before, err := os.ReadFile(state.StatePath(cwd, "s1"))
	if err != nil {
		t.Fatal(err)
	}
	want := "[crw status] IPABCD: P (PLAN) \u00b7 interview=false auditPassed=true checkPassed=false"
	if got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate status"); got != want {
		t.Errorf("the status line\n got %q\nwant %q", got, want)
	}
	// The leading section's own Stop-budget stamp is the only writer here, so status may not move
	// the phase, the flags or the injection cursor.
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseP || !s.Flags.AuditPassed || s.LastInjectedPhase != nil {
		t.Errorf("status wrote the session: %+v", s)
	}
	if after, err := os.ReadFile(state.StatePath(cwd, "s1")); err != nil {
		t.Fatal(err)
	} else if len(after) == 0 || len(before) == 0 {
		t.Error("the state file vanished")
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 0 {
		t.Errorf("status wrote a ledger row: %+v", rows)
	}
}

// TestPromptOrchestrateResetFromIdleIsANoop: a reset from rest is recognised and writes nothing.
func TestPromptOrchestrateResetFromIdleIsANoop(t *testing.T) {
	cwd := t.TempDir()
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {})
	if got, want := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate reset"), "[crw \u2014 already IDLE]"; got != want {
		t.Errorf("the no-op\n got %q\nwant %q", got, want)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseIdle || s.OrchestrationActive {
		t.Errorf("the state changed: %+v", s)
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 0 {
		t.Errorf("the no-op wrote a ledger row: %+v", rows)
	}
}

// TestPromptOrchestrateResetFromBClearsTheCycle: the explicit reset is the operator's stand-down, so
// it clears the phase, the gate flags and loopArmSeen, and records the row.
func TestPromptOrchestrateResetFromBClearsTheCycle(t *testing.T) {
	cwd := t.TempDir()
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LoopArmSeen, s.Flags.AuditPassed = state.PhaseB, true, true, true
	})
	if got, want := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate reset"), "[crw \u2014 reset \u2192 IDLE]"; got != want {
		t.Errorf("the reset line\n got %q\nwant %q", got, want)
	}
	s := state.ReadState(cwd, "s1")
	if s.Phase != state.PhaseIdle || s.OrchestrationActive || s.LoopArmSeen || s.Flags.AuditPassed {
		t.Errorf("the cleared state: %+v", s)
	}
	rows := promptOrchestrateLedger(t, cwd)
	if len(rows) != 1 || rows[0]["from"] != "B" || rows[0]["to"] != "IDLE" || rows[0]["reason"] != "reset" {
		t.Errorf("the ledger rows: %+v", rows)
	}
}

// TestPromptOrchestrateGoalModeSuppressesTheInterview is the HIGH fix of hook.ts:863-866: under an
// active goal the parser path answers nothing at all - no state write, no ledger row - and the loose
// path's own firewall keeps the phase where it is.
func TestPromptOrchestrateGoalModeSuppressesTheInterview(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "ws")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	home := sessionHookGoalsDB(t, filepath.Join(dir, "codex"),
		"CREATE TABLE thread_goals (thread_id TEXT PRIMARY KEY NOT NULL, goal_id TEXT NOT NULL, objective TEXT NOT NULL, status TEXT NOT NULL)",
		"INSERT INTO thread_goals (thread_id, goal_id, objective, status) VALUES ('g-int', 'g', 'obj', 'active')")
	promptOrchestrateSeed(t, cwd, "g-int", func(s *state.State) {})
	got := PromptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "g-int", Prompt: "orchestrate I",
		TurnID: "t1", PabcdEnabled: true}, "", promptTriggerGoalEnv(home))
	if got != "" {
		t.Errorf("an active goal did not suppress the command: %q", got)
	}
	if s := state.ReadState(cwd, "g-int"); s.Phase != state.PhaseIdle || s.OrchestrationActive {
		t.Errorf("the suppressed command moved the phase: %+v", s)
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 0 {
		t.Errorf("the suppressed command wrote a ledger row: %+v", rows)
	}
}

// TestPromptOrchestrateEntryFromIdleToI: the forward entry to I injects the interview directive and
// records the row, the same free pass as every other forward edge.
func TestPromptOrchestrateEntryFromIdleToI(t *testing.T) {
	cwd := t.TempDir()
	env := promptSubmitHost(cwd)
	if got, want := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate I"), WithFooter(InterviewDirective(env), state.PhaseI); got != want {
		t.Errorf("the entry\n got %q\nwant %q", got, want)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseI || !s.OrchestrationActive {
		t.Errorf("the state: %+v", s)
	}
}

// TestPromptOrchestrateSourceRootRefusal: a session whose pinned worktree has no binding is refused
// with the resolver's own text before any transition.
func TestPromptOrchestrateSourceRootRefusal(t *testing.T) {
	cwd := t.TempDir()
	other := t.TempDir()
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) { s.BoundSourceRoot = &other })
	got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate P")
	want := "[crw \u2014 refused: SOURCE-ROOT: Source binding is missing for the pinned worktree; restore the same binding before continuing.]"
	if got != want {
		t.Errorf("the refusal\n got %q\nwant %q", got, want)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseIdle {
		t.Errorf("the refused command moved the phase: %+v", s)
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 0 {
		t.Errorf("the refused command wrote a ledger row: %+v", rows)
	}
}

// promptOrchestrateLinkedWorktree makes dir a repository with one commit and a linked worktree, and
// writes the session's source binding, whose resolved source is the repository rather than the
// session's own directory. That is the shape the B>C cases need.
func promptOrchestrateLinkedWorktree(t *testing.T, dir, sessionID string) (worktree, root, head string) {
	t.Helper()
	repo, worktree := filepath.Join(dir, "repo"), filepath.Join(dir, "wt")
	canon := func(path string) string {
		if real, err := filepath.EvalSymlinks(path); err == nil {
			return real
		}
		return path
	}
	run := func(gitDir string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", gitDir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
			"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Skipf("git is unavailable for this case: %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run(repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "f.txt")
	run(repo, "commit", "-q", "-m", "initial")
	run(repo, "worktree", "add", "-q", worktree, "-b", "wt")
	head = run(worktree, "rev-parse", "HEAD")
	root = canon(run(repo, "rev-parse", "--show-toplevel"))
	common := canon(run(repo, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	gitDir := canon(run(repo, "rev-parse", "--absolute-git-dir"))
	binding := map[string]any{"version": 1, "ownerSessionId": sessionID, "nativeCwd": canon(worktree),
		"sourceRoot": root, "commonDir": common, "gitDir": gitDir}
	body, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	sources := filepath.Join(worktree, crwdir.DirName, "sources")
	if err := os.MkdirAll(sources, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sources, sessionID+".json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	return worktree, root, head
}

// TestPromptOrchestrateSourceGatesOnBC covers the three B>C refusals and the pass: no B baseline on a
// bound session, a binding that changed since B began, a source that did not move (SOURCE-DELTA-01),
// and a source that did move.
func TestPromptOrchestrateSourceGatesOnBC(t *testing.T) {
	t.Run("no B baseline", func(t *testing.T) {
		dir := t.TempDir()
		cwd, _, _ := promptOrchestrateLinkedWorktree(t, dir, "s1")
		promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
			s.Phase, s.OrchestrationActive, s.Flags.AuditPassed = state.PhaseB, true, true
		})
		got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate C")
		want := "[crw \u2014 refused: SOURCE-ROOT: bound source has no valid B baseline. Re-plan before continuing.]"
		if got != want {
			t.Errorf("the refusal\n got %q\nwant %q", got, want)
		}
		if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseB {
			t.Errorf("the refused command moved the phase: %+v", s)
		}
	})
	t.Run("the binding changed", func(t *testing.T) {
		dir := t.TempDir()
		cwd, _, head := promptOrchestrateLinkedWorktree(t, dir, "s1")
		gone := "/nonexistent-source"
		promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
			s.Phase, s.OrchestrationActive, s.Flags.AuditPassed = state.PhaseB, true, true
			s.PhaseEntrySource = &state.SourceIdentity{Kind: "resolved", CommitSha: head, CapturedAt: "2026-01-01T00:00:00.000Z", SourceRoot: &gone}
		})
		got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate C")
		want := "[crw \u2014 refused: SOURCE-ROOT: source binding changed since B began. Re-plan and capture a new baseline; nothing was written.]"
		if got != want {
			t.Errorf("the refusal\n got %q\nwant %q", got, want)
		}
		if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseB {
			t.Errorf("the refused command moved the phase: %+v", s)
		}
	})
	t.Run("the source did not move", func(t *testing.T) {
		dir := t.TempDir()
		cwd, root, head := promptOrchestrateLinkedWorktree(t, dir, "s1")
		promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
			s.Phase, s.OrchestrationActive, s.Flags.AuditPassed = state.PhaseB, true, true
			s.PhaseEntrySource = &state.SourceIdentity{Kind: "resolved", CommitSha: head, CapturedAt: "2026-01-01T00:00:00.000Z", SourceRoot: &root}
		})
		got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate C")
		if !strings.HasPrefix(got, "[crw \u2014 refused: the source is unchanged since B began (") ||
			!strings.HasSuffix(got, "so nothing was implemented in this B (SOURCE-DELTA-01). Nothing was written.]") {
			t.Errorf("the refusal: %q", got)
		}
		if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseB {
			t.Errorf("the refused command moved the phase: %+v", s)
		}
	})
	t.Run("the source moved", func(t *testing.T) {
		dir := t.TempDir()
		cwd, root, _ := promptOrchestrateLinkedWorktree(t, dir, "s1")
		promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
			s.Phase, s.OrchestrationActive, s.Flags.AuditPassed = state.PhaseB, true, true
			s.PhaseEntrySource = &state.SourceIdentity{Kind: "resolved", CommitSha: "0000000000000000000000000000000000000000", CapturedAt: "2026-01-01T00:00:00.000Z", SourceRoot: &root}
		})
		if got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate C"); !strings.Contains(got, "[crw: CHECK]") {
			t.Errorf("the passing B>C did not inject the check directive: %q", got)
		}
		s := state.ReadState(cwd, "s1")
		if s.Phase != state.PhaseC || s.CheckEpoch == nil || !strings.HasPrefix(*s.CheckEpoch, "c-") {
			t.Errorf("the C entry: %+v", s)
		}
		rows := promptOrchestrateLedger(t, cwd)
		if len(rows) != 1 || rows[0]["from"] != "B" || rows[0]["to"] != "C" {
			t.Errorf("the ledger rows: %+v", rows)
		}
	})
}

// TestPromptOrchestrateMintsThePlanBinding is the 060/032 rule: a chat P>A with an attestation naming
// a real plan unit mints the same binding the CLI does.
func TestPromptOrchestrateMintsThePlanBinding(t *testing.T) {
	cwd := t.TempDir()
	unit := filepath.Join(cwd, "devlog", "_plan", "unit")
	if err := os.MkdirAll(unit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unit, "000_plan.md"), []byte("# plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.OrchestrationActive = state.PhaseP, true
	})
	prompt := "orchestrate A --attest {\"from\":\"P\",\"to\":\"A\",\"did\":\"wrote the plan\",\"planUnit\":\"devlog/_plan/unit\"}"
	if got := promptOrchestrateAnswer(t, cwd, "s1", "t1", prompt); !strings.Contains(got, "[crw: AUDIT]") {
		t.Errorf("the P>A did not inject the audit directive: %q", got)
	}
	s := state.ReadState(cwd, "s1")
	if s.Phase != state.PhaseA || s.PlanUnit == nil || *s.PlanUnit != "devlog/_plan/unit" {
		t.Errorf("the plan unit: %+v", s)
	}
	if s.PlanEpoch == nil || !regexp.MustCompile("^e-[0-9]{14}-[0-9a-f]{6}$").MatchString(*s.PlanEpoch) {
		t.Errorf("the plan epoch: %v", s.PlanEpoch)
	}
}

// TestPromptOrchestratePlanBindingNilCases: no attestation, and an attestation whose unit holds no
// numbered plan docs, both move the phase and leave the binding nil - the honest outcome the oracle
// documents, because the audit then refuses to open.
func TestPromptOrchestratePlanBindingNilCases(t *testing.T) {
	for name, prompt := range map[string]string{
		"no attestation": "orchestrate A",
		"no numbered docs": "orchestrate A --attest {\"from\":\"P\",\"to\":\"A\",\"did\":\"wrote the plan\"," +
			"\"planUnit\":\"devlog/_plan/unit\"}",
	} {
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			if err := os.MkdirAll(filepath.Join(cwd, "devlog", "_plan", "unit"), 0o755); err != nil {
				t.Fatal(err)
			}
			promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
				s.Phase, s.OrchestrationActive = state.PhaseP, true
			})
			if got := promptOrchestrateAnswer(t, cwd, "s1", "t1", prompt); !strings.Contains(got, "[crw: AUDIT]") {
				t.Errorf("the phase did not move: %q", got)
			}
			if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseA || s.PlanUnit != nil || s.PlanEpoch != nil {
				t.Errorf("the binding: %+v", s)
			}
		})
	}
}

// TestPromptOrchestrateCheckEpochIsMintedKeptAndDropped: C mints, staying in C keeps, anywhere else
// drops. Only the middle rule is unreachable from a chat command (the table has no C>C edge), so it is
// checked on the field builder the write uses.
func TestPromptOrchestrateCheckEpochIsMintedKeptAndDropped(t *testing.T) {
	cwd := t.TempDir()
	kept := "c-20260101000000-abcdef"
	fresh := state.State{Phase: state.PhaseC, CheckEpoch: &kept}
	next, ok := promptOrchestrateFields(PromptSubmitPayload{Cwd: cwd, SessionID: "s1"},
		state.State{Phase: state.PhaseC}, fresh, state.State{Phase: state.PhaseC}, nil, "")
	if !ok || next.CheckEpoch == nil || *next.CheckEpoch != kept {
		t.Errorf("staying in C dropped the epoch: %+v ok=%v", next.CheckEpoch, ok)
	}
	// A backward edge out of C drops it.
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.Flags.AuditPassed, s.CheckEpoch = state.PhaseC, true, true, &kept
	})
	if got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate B"); !strings.Contains(got, "[crw: BUILD]") {
		t.Errorf("the C>B did not inject the build directive: %q", got)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseB || s.CheckEpoch != nil {
		t.Errorf("leaving C kept the epoch: %+v", s)
	}
}

// TestPromptOrchestrateBoundDCloseHandsToTheSeam: a bound D belongs to the seam prompt_dclose.go
// fills, so this unit hands it there instead of letting the loose detector run. The case carries no
// receipt, which is the gate's first refusal, and the seam's own unit owns the rest of its cases
// (prompt_dclose_test.go). The stub this asserted before CRW-797 is recorded as a pending defect of
// CRW-385, superseded by that issue's known-defects file.
func TestPromptOrchestrateBoundDCloseHandsToTheSeam(t *testing.T) {
	cwd := t.TempDir()
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.Slug = state.PhaseC, true, "some-unit"
	})
	got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate D")
	if !strings.Contains(got, "C -> D on a goalplan-bound session requires") {
		t.Errorf("the bound D did not reach the seam's receipt gate: %q", got)
	}
	if strings.Contains(got, "[crw: CHECK]") {
		t.Errorf("a bound D fell through to the loose path: %q", got)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseC || s.Slug != "some-unit" {
		t.Errorf("a refused bound D moved the phase: %+v", s)
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 0 {
		t.Errorf("a refused bound D wrote a ledger row: %+v", rows)
	}
}

// TestPromptOrchestrateUnboundDCloseClosesTheCycle: the human D means "I am done" - C>D and D>IDLE
// together - so the resting state is IDLE, the injected text is the DONE directive with an IDLE
// footer, and one row records the close.
func TestPromptOrchestrateUnboundDCloseClosesTheCycle(t *testing.T) {
	cwd := t.TempDir()
	epoch := "c-20260101000000-abcdef"
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.Flags.CheckPassed, s.CheckEpoch = state.PhaseC, true, true, &epoch
	})
	want := WithFooter(PhaseDirective(state.PhaseD, nil), state.PhaseIdle)
	if got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate D"); got != want {
		t.Errorf("the close\n got %q\nwant %q", got, want)
	}
	s := state.ReadState(cwd, "s1")
	if s.Phase != state.PhaseIdle || s.OrchestrationActive || s.CheckEpoch != nil || s.LastInjectedPhase != nil {
		t.Errorf("the resting state: %+v", s)
	}
	rows := promptOrchestrateLedger(t, cwd)
	if len(rows) != 1 || rows[0]["from"] != "C" || rows[0]["to"] != "IDLE" || rows[0]["reason"] != "done" {
		t.Errorf("the ledger rows: %+v", rows)
	}
}

// TestPromptOrchestrateSameTurnInjectsOnce is the same-turn dedup the leading section owns: a re-fired
// hook for one turn answers nothing.
func TestPromptOrchestrateSameTurnInjectsOnce(t *testing.T) {
	cwd := t.TempDir()
	if got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate P"); !strings.Contains(got, "[crw: PLAN]") {
		t.Errorf("the first turn: %q", got)
	}
	if got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate P"); got != "" {
		t.Errorf("a re-fired turn answered %q", got)
	}
	if s := state.ReadState(cwd, "s1"); len(s.InjectedTurns) != 1 {
		t.Errorf("the recorded turns: %+v", s.InjectedTurns)
	}
}

// TestPromptOrchestrateKeepsAParticipatingWritersUpdate is the data-loss fix on this unit's write: the
// oracle reads the state and writes the whole state back with no lock, so an update a participating
// writer lands in between is lost. The lock stands in for that writer, which the handler cannot be
// timed against.
func TestPromptOrchestrateKeepsAParticipatingWritersUpdate(t *testing.T) {
	cwd := t.TempDir()
	landed := func(cwd, sessionID string, fn func() error) error {
		return state.WithSessionLock(cwd, sessionID, func() error {
			fresh := state.ReadState(cwd, sessionID)
			fresh.MemoryWriteGrant = true
			if err := state.WriteState(cwd, fresh); err != nil {
				return err
			}
			return fn()
		})
	}
	got := promptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: "orchestrate P",
		TurnID: "t1", PabcdEnabled: true}, "", promptSubmitHost(cwd), landed)
	if !strings.Contains(got, "[crw: PLAN]") {
		t.Errorf("the free pass: %q", got)
	}
	s := state.ReadState(cwd, "s1")
	if !s.MemoryWriteGrant {
		t.Error("the writer's update was overwritten by the stale snapshot")
	}
	if s.Phase != state.PhaseP {
		t.Errorf("the phase: %+v", s)
	}
}

// TestPromptOrchestrateRefusesARewriteTheReaderWouldLose: a file holding a record the reader cannot
// keep whole is not rewritten, and the command is refused with nothing written.
func TestPromptOrchestrateRefusesARewriteTheReaderWouldLose(t *testing.T) {
	cwd := t.TempDir()
	receipt := strings.Repeat("r", state.MaxReceiptClaimLen+1)
	body, err := json.Marshal(map[string]any{
		"phase": "IDLE", "sessionId": "s1", "injectedTurns": []any{},
		"unverifiedSubagents": []any{map[string]any{"agentId": "a1", "turnId": "t1", "agentType": "executor",
			"attempts": 3, "receiptClaimed": receipt, "recordedAt": "2026-01-01T00:00:00.000Z", "resolvable": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(state.StatePath(cwd, "s1")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.StatePath(cwd, "s1"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	got := promptOrchestrateAnswer(t, cwd, "s1", "t1", "orchestrate P")
	want := "[crw \u2014 refused: the session state changed or cannot be rewritten without losing a stored record, so this command was not applied. Nothing was written.]"
	if got != want {
		t.Errorf("the refusal\n got %q\nwant %q", got, want)
	}
	after, err := os.ReadFile(state.StatePath(cwd, "s1"))
	if err != nil || string(after) != string(body) {
		t.Errorf("the refused command rewrote the file (%v)", err)
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 0 {
		t.Errorf("the refused command wrote a ledger row: %+v", rows)
	}
}
