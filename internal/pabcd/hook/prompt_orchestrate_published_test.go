// prompt_orchestrate_published_test.go is the CRW-845 suite: what the chat orchestrate write answers
// when state.WriteState published the state at its final path and only a step after the rename
// failed, and which transition's ledger row the handler appends. The session lock is swapped through
// a function argument, as TestPromptOrchestrateKeepsAParticipatingWritersUpdate does; no package-level
// variable carries the seam. The cases are red on dev: the answer is the refusal text and no row
// exists, and the row that is written comes from the transition applied before the lock.
package hook

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// promptOrchestratePublishedWarning is the durability line the orchestrate answer carries when the
// state reached its final path and the directory open or fsync after the rename failed. It is the
// CRW-797 sentence (promptDcloseWriteLanded), so the two chat surfaces report one text.
func promptOrchestratePublishedWarning(err error) string {
	return "the session state was published but its directory could not be synced: " + err.Error()
}

// promptOrchestratePublishedLock runs the real locked write and then reports the post-rename failure
// a directory open or fsync failure would: the state is at its final path, only a later step failed.
func promptOrchestratePublishedLock(cwd, sessionID string, fn func() error) error {
	if err := state.WithSessionLock(cwd, sessionID, fn); err != nil {
		return err
	}
	return &state.PublishedError{Err: syscall.EIO}
}

// (a) A post-publication failure in the orchestrate write is not "nothing was written": the state is
// at P with the turn recorded, the answer is the P directive plus the durability warning, and the
// IDLE->P chat human row is appended.
func TestPromptOrchestratePublishedWriteAnswersApplied(t *testing.T) {
	cwd := t.TempDir()
	got := promptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: "orchestrate P",
		TurnID: "t1", PabcdEnabled: true}, "", promptSubmitHost(cwd), promptOrchestratePublishedLock)
	want := WithFooter(PhaseDirective(state.PhaseP, nil), state.PhaseP) + "\n" + promptOrchestratePublishedWarning(syscall.EIO)
	if got != want {
		t.Errorf("the published write's answer\n got %q\nwant %q", got, want)
	}
	s := state.ReadState(cwd, "s1")
	if s.Phase != state.PhaseP || !s.OrchestrationActive || len(s.InjectedTurns) != 1 || s.InjectedTurns[0] != "t1" {
		t.Errorf("the persisted state: %+v", s)
	}
	rows := promptOrchestrateLedger(t, cwd)
	if len(rows) != 1 || rows[0]["from"] != "IDLE" || rows[0]["to"] != "P" || rows[0]["reason"] != "chat" || rows[0]["actor"] != "human" {
		t.Errorf("the ledger rows: %+v", rows)
	}
}

// (b) The same failure in the loop-arm write is not silence either: a published write no longer maps
// to promptSubmitFailed, so the caller that judges only that outcome proceeds and answers the arming
// mandate, as the oracle's successful unlocked write does.
func TestPromptOrchestratePublishedLoopArmWriteAnswersTheMandate(t *testing.T) {
	cwd := t.TempDir()
	const prompt = "이 유닛 crw-loop로 알아서 끝까지 해줘"
	env := promptSubmitHost(cwd)
	got := promptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: prompt,
		TurnID: "t1", PabcdEnabled: true}, "", env, promptOrchestratePublishedLock)
	want := ResolveCRWInDirective(LoopArmDirective(""), env)
	if got != want {
		t.Errorf("the published loop-arm write\n got %q\nwant %q", got, want)
	}
	if s := state.ReadState(cwd, "s1"); !s.LoopArmSeen || s.Phase != state.PhaseIdle {
		t.Errorf("the loop-arm state: %+v", s)
	}
}

// A published write on the passive trigger path is not silence either: the trigger branch's own write
// judges only == promptSubmitFailed, so it answers its directive, as the oracle's successful write
// does. The passive trigger records only dedup bookkeeping and never moves the phase.
func TestPromptOrchestratePublishedTriggerWriteAnswersTheDirective(t *testing.T) {
	cwd := t.TempDir()
	got := promptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "s1",
		Prompt: "Use crw-pabcd to start Check phase", TurnID: "t1", PabcdEnabled: true}, "", promptSubmitHost(cwd), promptOrchestratePublishedLock)
	if !strings.Contains(got, "[crw: CHECK]") {
		t.Errorf("a published trigger write answered %q, want the check directive", got)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseIdle {
		t.Errorf("a passive trigger moved the phase: %+v", s)
	}
}

// (c) Control: a failure before the publication is unchanged. The sessions directory is made
// unwritable inside the lock only, so state.WriteState cannot create its temp file and nothing is
// published; the command is refused with the file as it was and no row.
func TestPromptOrchestratePrePublicationFailureStillRefuses(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this case needs")
	}
	cwd := t.TempDir()
	turn := "t1"
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
		// The turn is already stamped, so the leading section's Stop-budget write is skipped and the
		// orchestrate write is the only locked write this case fails.
		s.StopBlockTurnID = &turn
	})
	before, err := os.ReadFile(state.StatePath(cwd, "s1"))
	if err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Dir(state.StatePath(cwd, "s1"))
	lock := func(cwd, sessionID string, fn func() error) error {
		return state.WithSessionLock(cwd, sessionID, func() error {
			if err := os.Chmod(sessions, 0o500); err != nil {
				return err
			}
			defer func() { _ = os.Chmod(sessions, 0o755) }()
			return fn()
		})
	}
	got := promptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: "orchestrate P",
		TurnID: turn, PabcdEnabled: true}, "", promptSubmitHost(cwd), lock)
	want := "[crw — refused: the session state changed or cannot be rewritten without losing a stored record, so this command was not applied. Nothing was written.]"
	if got != want {
		t.Errorf("the pre-publication failure\n got %q\nwant %q", got, want)
	}
	after, err := os.ReadFile(state.StatePath(cwd, "s1"))
	if err != nil || string(after) != string(before) {
		t.Errorf("the refused command rewrote the file (%v)", err)
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 0 {
		t.Errorf("the refused command wrote a ledger row: %+v", rows)
	}
}

// (d) The row must describe the transition that was applied under the lock. The handler reads the
// state, a participating writer adds one high contradiction and bumps scanRounds, and the locked
// re-application then passes I->P only by the attestation's override; the row must carry that
// override and its scan evidence, not the pre-lock transition's (which saw a ready tracker).
func TestPromptOrchestrateLedgerRowComesFromTheAppliedTransition(t *testing.T) {
	cwd := t.TempDir()
	promptOrchestrateSeed(t, cwd, "s1", func(s *state.State) {
		tr := interview.DefaultInterview(1)
		for _, d := range interview.DimensionOrder() {
			*tr.Dimensions.Score(d) = interview.DimensionScore{Level: interview.LevelMax, Known: []string{}, Unknown: []string{}, Confidence: 1}
		}
		tr.ScanRounds = 1
		s.Phase, s.Interview = state.PhaseI, tr
	})
	calls := 0
	lock := func(cwd, sessionID string, fn func() error) error {
		calls++
		n := calls
		return state.WithSessionLock(cwd, sessionID, func() error {
			if n == 2 { // 1 = the Stop-budget stamp, 2 = the orchestrate write
				fresh := state.ReadState(cwd, sessionID)
				fresh.Interview.Contradictions = append(fresh.Interview.Contradictions,
					interview.Contradiction{ContradictionID: "c1", Severity: interview.SeverityHigh, Summary: "late"})
				fresh.Interview.ScanRounds++
				if err := state.WriteState(cwd, fresh); err != nil {
					return err
				}
			}
			return fn()
		})
	}
	prompt := "orchestrate P --attest {\"from\":\"I\",\"to\":\"P\",\"did\":\"proceed\",\"override\":true}"
	got := promptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: prompt,
		TurnID: "t1", PabcdEnabled: true}, "", promptSubmitHost(cwd), lock)
	if !strings.Contains(got, "[crw: PLAN]") {
		t.Errorf("the I>P did not inject the plan directive: %q", got)
	}
	s := state.ReadState(cwd, "s1")
	high := 0
	if s.Interview != nil {
		for _, c := range s.Interview.Contradictions {
			if c.Severity == interview.SeverityHigh {
				high++
			}
		}
	}
	if s.Phase != state.PhaseP || high != 1 {
		t.Errorf("the persisted state: phase=%s high=%d", s.Phase, high)
	}
	rows := promptOrchestrateLedger(t, cwd)
	if len(rows) != 1 || rows[0]["override"] != true {
		t.Errorf("the ledger rows: %+v", rows)
	}
	scan, ok := rows[0]["scanEvidence"].(map[string]any)
	if !ok || scan["highContradictionCount"] != 1.0 || scan["scanRounds"] != 2.0 {
		t.Errorf("the scan evidence: %+v", rows[0]["scanEvidence"])
	}
}
