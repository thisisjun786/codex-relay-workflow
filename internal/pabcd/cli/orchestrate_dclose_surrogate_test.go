package cli

// These are the CRW-850 cases: the D close's two idempotence guards read the ledger lines the way
// the oracle's JSON.parse does (CXC v0.2.40 pabcd-state/src/orchestrate-cli.ts:431-453). The oracle
// reads the file with readFileSync(path, "utf8") and parses each line with JSON.parse, so a lone
// surrogate escape stays a lone surrogate and never equals U+FFFD; encoding/json folds the escape
// into U+FFFD and a needed row is skipped. Every helper and test name carries the
// orchestrateDcloseSurrogate prefix the issue asks for.
//
// The rows are compared by their raw bytes, not through a decoding helper: the point of the issue is
// that one reader folds the escape and another keeps it, so a helper that decoded first could hide
// the difference. The temporary-world helpers come from orchestrate_dclose_test.go and
// orchestrate_transition_test.go, which point HOME, CODEX_HOME and CRW_HOME into temporary
// directories before anything here runs.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// orchestrateDcloseSurrogateFFFD is the real U+FFFD character: one code point, the three UTF-8 bytes
// EF BF BD. A Go string literal spells it as an escape; the bytes on disk are what matter.
const orchestrateDcloseSurrogateFFFD = "\uFFFD"

// orchestrateDcloseSurrogateEscape is the six characters the ledger stores for a lone surrogate:
// backslash, u, d, 8, 0, 0. Node's JSON.parse keeps it as a lone surrogate; encoding/json replaces
// it with U+FFFD.
const orchestrateDcloseSurrogateEscape = `\ud800`

// orchestrateDcloseSurrogateWTF8 is the three WTF-8 bytes a Go string holds a lone surrogate in,
// which is what the oracle's JSON.parse produces for the escape above.
const orchestrateDcloseSurrogateWTF8 = "\xed\xa0\x80"

// orchestrateDcloseSurrogateWriteRaw writes a ledger file's exact bytes. The surrogate cases need
// the escape verbatim, which a JSON encoder would never produce.
func orchestrateDcloseSurrogateWriteRaw(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := ""
	if len(lines) > 0 {
		body = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// orchestrateDcloseSurrogateCount is how many times needle occurs in the file's raw bytes.
func orchestrateDcloseSurrogateCount(t *testing.T, path, needle string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), needle)
}

// orchestrateDcloseSurrogateGoalplanLedgerPath is the plan's own ledger path.
func orchestrateDcloseSurrogateGoalplanLedgerPath(cwd, slug string) string {
	return filepath.Join(cwd, ".crw", "goalplans", slug, "ledger.jsonl")
}

// orchestrateDcloseSurrogatePabcdLedgerPath is the session ledger path.
func orchestrateDcloseSurrogatePabcdLedgerPath(cwd string) string {
	return filepath.Join(cwd, ".crw", "ledger.jsonl")
}

// orchestrateDcloseSurrogateGoalplanRow is one stored plan-ledger row, with the detail written as
// the caller spells it: the escape for the surrogate case, the real U+FFFD for the control.
func orchestrateDcloseSurrogateGoalplanRow(slug, detail string) string {
	return `{"ts":"2026-01-01T00:00:00.000Z","slug":"` + slug + `","event":"workphase_done","detail":"` + detail + `"}`
}

// orchestrateDcloseSurrogatePabcdRow is one stored session-ledger row for a C -> IDLE close.
func orchestrateDcloseSurrogatePabcdRow(id, epoch, closedID string) string {
	return `{"ts":"2026-01-01T00:00:00.000Z","sessionId":"` + id + `","from":"C","to":"IDLE","reason":"done","checkEpoch":"` + epoch + `","closedWorkPhaseId":"` + closedID + `"}`
}

// orchestrateDcloseSurrogateSeedPlan writes a one-phase plan whose only work-phase carries the id
// under test and one done task, so a bound close can close it. A real U+FFFD in the id is valid
// UTF-8, which is why the goalplan reader accepts the plan while the stored ledger row below is not
// the row of this close.
func orchestrateDcloseSurrogateSeedPlan(t *testing.T, cwd, slug, phaseID string) {
	t.Helper()
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "surrogate ledger row"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{
		ID: phaseID, Title: "the phase", Status: goalplan.WorkPhaseInProgress,
		Tasks:       []goalplan.GoalplanTask{{ID: "t-1", Title: "the task", Status: goalplan.TaskDone, Outcome: "focused tests passed"}},
		CriteriaIDs: []string{},
	}}
	active := phaseID
	plan.ActiveWorkPhaseID = &active
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
}

// orchestrateDcloseSurrogateSeedAtC seeds a bound session at C with a valid receipt and the plan above.
func orchestrateDcloseSurrogateSeedAtC(t *testing.T, cwd, id, slug, phaseID string) {
	t.Helper()
	orchestrateDcloseSurrogateSeedPlan(t, cwd, slug, phaseID)
	epoch := "c-surrogate-epoch"
	s := state.DefaultState(id, slug)
	s.Phase, s.CheckEpoch, s.OrchestrationActive = state.PhaseC, &epoch, true
	s.Flags = state.Flags{AuditPassed: true}
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	orchestrateDcloseSeedReceipt(t, cwd, id, epoch)
}

// orchestrateDcloseSurrogateAttest is the C>D attestation naming the surrogate phase.
func orchestrateDcloseSurrogateAttest(id, phaseID string) *attest.Attestation {
	zero := float64(0)
	return &attest.Attestation{
		From: state.PhaseC, To: state.PhaseD, Did: "ran the suite", CheckOutput: "722 pass", ExitCode: &zero,
		WorkPhaseID: phaseID, TestReceiptPath: ".crw/evidence/" + id + "/test-receipt.json",
	}
}

// orchestrateDcloseSurrogateRun drives the close the verb's call site would, with the given seam.
func orchestrateDcloseSurrogateRun(t *testing.T, cwd, id, phaseID string, recovering bool, seam orchestrateDcloseSeam) (CliResult, error) {
	t.Helper()
	return orchestrateDclose(cwd, id, phaseID, state.ReadState(cwd, id), orchestrateDcloseSurrogateAttest(id, phaseID), recovering, seam)
}

// TestOrchestrateDcloseSurrogateReaderKeepsALoneSurrogateDistinct is the reader contract the issue
// states: the file bytes go through source.DecodeUTF8 first and each non-empty line through
// pyjson.Loads with Surrogates/Map/SpelledNumbers, so a lone surrogate escape stays three WTF-8
// bytes and never equals U+FFFD, while a paired surrogate is its one character. The other reading
// rules stay: a blank line is skipped, a non-object value is skipped, a repeated key keeps its last
// value, a null line is refused and a line that is not JSON is an error.
func TestOrchestrateDcloseSurrogateReaderKeepsALoneSurrogateDistinct(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.jsonl")
	orchestrateDcloseSurrogateWriteRaw(t, path,
		`{"event":"workphase_done","detail":"closed wp-\ud800"}`,
		`{"event":"workphase_done","detail":"closed wp-\ufffd"}`,
		`{"event":"workphase_done","detail":"closed wp-\ud83d\ude00"}`,
		`{"event":"workphase_done","detail":"closed wp-`+"\xff"+`"}`,
		``,
		`42`,
		`{"event":"first","event":"workphase_started"}`,
	)
	rows, err := orchestrateDcloseReadJSONLObjects(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want the four objects and the repeated-key row", len(rows))
	}
	lone, _ := rows[0]["detail"].(string)
	if lone != "closed wp-"+orchestrateDcloseSurrogateWTF8 {
		t.Fatalf("lone surrogate detail = %q (%x), want the three WTF-8 bytes", lone, []byte(lone))
	}
	if lone == "closed wp-"+orchestrateDcloseSurrogateFFFD {
		t.Fatal("the lone surrogate read as U+FFFD, which is the defect this issue fixes")
	}
	if got, _ := rows[1]["detail"].(string); got != "closed wp-"+orchestrateDcloseSurrogateFFFD {
		t.Fatalf("U+FFFD detail = %q, want a real U+FFFD", got)
	}
	if got, _ := rows[2]["detail"].(string); got != "closed wp-\U0001F600" {
		t.Fatalf("paired surrogate detail = %q, want the single character", got)
	}
	if got, _ := rows[3]["detail"].(string); got != "closed wp-"+orchestrateDcloseSurrogateFFFD {
		t.Fatalf("invalid UTF-8 byte detail = %q, want U+FFFD as Node reads it", got)
	}
	if got, _ := rows[4]["event"].(string); got != "workphase_started" {
		t.Fatalf("repeated key event = %q, want the last value", got)
	}
}

// TestOrchestrateDcloseSurrogateReaderRefusesWhatTheOracleRefuses pins the rest of the reading rules.
func TestOrchestrateDcloseSurrogateReaderRefusesWhatTheOracleRefuses(t *testing.T) {
	dir := t.TempDir()
	nullPath := filepath.Join(dir, "null.jsonl")
	orchestrateDcloseSurrogateWriteRaw(t, nullPath, `null`)
	if _, err := orchestrateDcloseReadJSONLObjects(nullPath); err == nil {
		t.Fatal("a null line was accepted")
	}
	brokenPath := filepath.Join(dir, "broken.jsonl")
	orchestrateDcloseSurrogateWriteRaw(t, brokenPath, `{"event":`)
	if _, err := orchestrateDcloseReadJSONLObjects(brokenPath); err == nil {
		t.Fatal("a line that is not JSON was accepted")
	}
	missing, err := orchestrateDcloseReadJSONLObjects(filepath.Join(dir, "absent.jsonl"))
	if err != nil || len(missing) != 0 {
		t.Fatalf("a missing file = %v, %v, want no rows and no error", missing, err)
	}
}

// TestOrchestrateDcloseSurrogateGoalplanGuard is the plan-ledger guard: a row stored with the lone
// surrogate escape is not the row of a U+FFFD close, while a row stored with a real U+FFFD is.
func TestOrchestrateDcloseSurrogateGoalplanGuard(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "surrogate-goalplan-guard", "surrogate-goalplan-guard-plan"
	phaseID := "wp-" + orchestrateDcloseSurrogateFFFD
	orchestrateDcloseSurrogateSeedAtC(t, cwd, id, slug, phaseID)
	path := orchestrateDcloseSurrogateGoalplanLedgerPath(cwd, slug)
	orchestrateDcloseSurrogateWriteRaw(t, path,
		orchestrateDcloseSurrogateGoalplanRow(slug, "closed wp-"+orchestrateDcloseSurrogateEscape),
	)

	have, err := orchestrateDcloseHasGoalplanRow(cwd, slug, goalplan.EventWorkphaseDone, "closed "+phaseID)
	if err != nil {
		t.Fatal(err)
	}
	if have {
		t.Fatal("the lone-surrogate row was taken as the close's own row")
	}

	orchestrateDcloseSurrogateWriteRaw(t, path,
		orchestrateDcloseSurrogateGoalplanRow(slug, "closed "+phaseID),
	)
	have, err = orchestrateDcloseHasGoalplanRow(cwd, slug, goalplan.EventWorkphaseDone, "closed "+phaseID)
	if err != nil {
		t.Fatal(err)
	}
	if !have {
		t.Fatal("a row stored with a real U+FFFD no longer matched")
	}
}

// TestOrchestrateDcloseSurrogatePabcdGuard is the session-ledger guard: a C -> IDLE row whose
// closedWorkPhaseId holds the lone surrogate escape is not the row of a U+FFFD close, while a row
// stored with a real U+FFFD is.
func TestOrchestrateDcloseSurrogatePabcdGuard(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "surrogate-pabcd-guard", "surrogate-pabcd-guard-plan"
	phaseID := "wp-" + orchestrateDcloseSurrogateFFFD
	orchestrateDcloseSurrogateSeedAtC(t, cwd, id, slug, phaseID)
	path := orchestrateDcloseSurrogatePabcdLedgerPath(cwd)
	epoch := "c-surrogate-epoch"
	orchestrateDcloseSurrogateWriteRaw(t, path,
		orchestrateDcloseSurrogatePabcdRow(id, epoch, "wp-"+orchestrateDcloseSurrogateEscape),
	)

	have, err := orchestrateDcloseHasPabcdCloseRow(cwd, id, &epoch, &phaseID)
	if err != nil {
		t.Fatal(err)
	}
	if have {
		t.Fatal("the lone-surrogate C -> IDLE row was taken as this close's row")
	}

	orchestrateDcloseSurrogateWriteRaw(t, path, orchestrateDcloseSurrogatePabcdRow(id, epoch, phaseID))
	have, err = orchestrateDcloseHasPabcdCloseRow(cwd, id, &epoch, &phaseID)
	if err != nil {
		t.Fatal(err)
	}
	if !have {
		t.Fatal("a C -> IDLE row stored with a real U+FFFD no longer matched")
	}
}

// TestOrchestrateDcloseSurrogateWritesTheMissingDoneRow is the issue's red case: the plan ledger
// already holds a workphase_done row for the lone surrogate "wp-\ud800" while the close's target is
// the real U+FFFD phase. The close must append its own closed row exactly once, reach IDLE and clear
// the recovery marker, and a retry must add no duplicate. On dev the escape folds to U+FFFD, the
// guard answers "row present", and no closed row is ever written.
func TestOrchestrateDcloseSurrogateWritesTheMissingDoneRow(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "surrogate-done-row", "surrogate-done-row-plan"
	phaseID := "wp-" + orchestrateDcloseSurrogateFFFD
	orchestrateDcloseSurrogateSeedAtC(t, cwd, id, slug, phaseID)
	ledger := orchestrateDcloseSurrogateGoalplanLedgerPath(cwd, slug)
	orchestrateDcloseSurrogateWriteRaw(t, ledger,
		orchestrateDcloseSurrogateGoalplanRow(slug, "closed wp-"+orchestrateDcloseSurrogateEscape),
	)

	// Step 1: crash right after the plan commit, so the marker survives and the done row is still owed.
	stop := errors.New("fail right after the goalplan commit")
	if _, err := orchestrateDcloseSurrogateRun(t, cwd, id, phaseID, false, orchestrateDcloseSeam{afterGoalplanCommit: func() error { return stop }}); !errors.Is(err, stop) {
		t.Fatalf("err = %v, want the seam's error", err)
	}
	if state.ReadState(cwd, id).DcloseRecovery == nil {
		t.Fatal("the recovery marker did not survive the crash")
	}

	// Step 2: the retry closes the fixed phase and owes the row. Crash after the PABCD append.
	stop2 := errors.New("fail right after the PABCD append")
	if _, err := orchestrateDcloseSurrogateRun(t, cwd, id, phaseID, true, orchestrateDcloseSeam{afterPabcdLedgerAppend: func() error { return stop2 }}); !errors.Is(err, stop2) {
		t.Fatalf("retry err = %v, want the seam's error", err)
	}

	// Step 3: a second retry finds both rows and appends neither.
	got, err := orchestrateDcloseSurrogateRun(t, cwd, id, phaseID, true, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 0 || !strings.Contains(got.Output, "close target "+phaseID+" is complete") {
		t.Fatalf("close: %+v", got)
	}
	after := state.ReadState(cwd, id)
	if after.Phase != state.PhaseIdle || after.DcloseRecovery != nil {
		t.Fatalf("state: %+v", after)
	}

	// The rows are counted in the file's own bytes: the escape row stays one row, and the row this
	// close owes is the one whose detail holds the real U+FFFD phase id.
	wantRow := `"detail":"closed ` + phaseID + `"`
	if n := orchestrateDcloseSurrogateCount(t, ledger, wantRow); n != 1 {
		t.Fatalf("rows closed on %q = %d, want exactly 1; ledger:\n%s", phaseID, n, orchestrateDcloseSurrogateRaw(t, ledger))
	}
	if n := orchestrateDcloseSurrogateCount(t, ledger, orchestrateDcloseSurrogateEscape); n != 1 {
		t.Fatalf("the stored escape row changed: %d occurrences; ledger:\n%s", n, orchestrateDcloseSurrogateRaw(t, ledger))
	}
	pabcd := orchestrateDcloseSurrogatePabcdLedgerPath(cwd)
	wantPabcd := `"closedWorkPhaseId":"` + phaseID + `"`
	if n := orchestrateDcloseSurrogateCount(t, pabcd, wantPabcd); n != 1 {
		t.Fatalf("C -> IDLE rows closed on %q = %d, want exactly 1; ledger:\n%s", phaseID, n, orchestrateDcloseSurrogateRaw(t, pabcd))
	}
}

// TestOrchestrateDcloseSurrogateWritesItsOwnCloseRow is the issue's second red case: a C -> IDLE row
// stored with the lone surrogate closedWorkPhaseId must not suppress this close's own row.
func TestOrchestrateDcloseSurrogateWritesItsOwnCloseRow(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "surrogate-close-row", "surrogate-close-row-plan"
	phaseID := "wp-" + orchestrateDcloseSurrogateFFFD
	orchestrateDcloseSurrogateSeedAtC(t, cwd, id, slug, phaseID)
	epoch := "c-surrogate-epoch"
	path := orchestrateDcloseSurrogatePabcdLedgerPath(cwd)
	orchestrateDcloseSurrogateWriteRaw(t, path,
		orchestrateDcloseSurrogatePabcdRow(id, epoch, "wp-"+orchestrateDcloseSurrogateEscape),
	)

	got, err := orchestrateDcloseSurrogateRun(t, cwd, id, phaseID, false, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 0 {
		t.Fatalf("close: %+v", got)
	}
	want := `"closedWorkPhaseId":"` + phaseID + `"`
	if n := orchestrateDcloseSurrogateCount(t, path, want); n != 1 {
		t.Fatalf("rows closed on %q = %d, want exactly 1; ledger:\n%s", phaseID, n, orchestrateDcloseSurrogateRaw(t, path))
	}
	if n := orchestrateDcloseSurrogateCount(t, path, orchestrateDcloseSurrogateEscape); n != 1 {
		t.Fatalf("the stored escape row changed: %d occurrences", n)
	}
	if state.ReadState(cwd, id).Phase != state.PhaseIdle {
		t.Fatal("the close did not reach IDLE")
	}
}

// TestOrchestrateDcloseSurrogateInvalidByteMatchesTheReplacement is the issue's other control: an
// invalid UTF-8 byte inside a stored ledger row is read as U+FFFD, as Node's readFileSync utf8 reads
// it, so the row does match a close whose id carries the real U+FFFD. The byte is written raw (0xFF),
// never as an escape, which is the shape a hand edit or a byte-level copy leaves behind.
func TestOrchestrateDcloseSurrogateInvalidByteMatchesTheReplacement(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "surrogate-invalid-byte", "surrogate-invalid-byte-plan"
	phaseID := "wp-" + orchestrateDcloseSurrogateFFFD
	orchestrateDcloseSurrogateSeedAtC(t, cwd, id, slug, phaseID)
	ledger := orchestrateDcloseSurrogateGoalplanLedgerPath(cwd, slug)
	// "closed wp-" followed by the single byte 0xFF: Node reads the byte as U+FFFD, so the detail
	// becomes exactly the close's own target.
	orchestrateDcloseSurrogateWriteRaw(t, ledger,
		orchestrateDcloseSurrogateGoalplanRow(slug, "closed wp-"+"\xff"),
	)

	have, err := orchestrateDcloseHasGoalplanRow(cwd, slug, goalplan.EventWorkphaseDone, "closed "+phaseID)
	if err != nil {
		t.Fatal(err)
	}
	if !have {
		t.Fatal("a row whose invalid byte reads as U+FFFD did not match the U+FFFD close, as it does in Node")
	}
}

// TestOrchestrateDcloseSurrogateRetryAddsNoDuplicate is the control the issue asks for: a row stored
// with a real U+FFFD still matches, so the close writes no duplicate row.
func TestOrchestrateDcloseSurrogateRetryAddsNoDuplicate(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "surrogate-fffd-retry", "surrogate-fffd-retry-plan"
	phaseID := "wp-" + orchestrateDcloseSurrogateFFFD
	orchestrateDcloseSurrogateSeedAtC(t, cwd, id, slug, phaseID)
	ledger := orchestrateDcloseSurrogateGoalplanLedgerPath(cwd, slug)
	orchestrateDcloseSurrogateWriteRaw(t, ledger, orchestrateDcloseSurrogateGoalplanRow(slug, "closed "+phaseID))

	got, err := orchestrateDcloseSurrogateRun(t, cwd, id, phaseID, false, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 0 {
		t.Fatalf("close: %+v", got)
	}
	wantRow := `"detail":"closed ` + phaseID + `"`
	if n := orchestrateDcloseSurrogateCount(t, ledger, wantRow); n != 1 {
		t.Fatalf("closed rows = %d, want the single pre-existing row; ledger:\n%s", n, orchestrateDcloseSurrogateRaw(t, ledger))
	}
}

// orchestrateDcloseSurrogateRaw is a ledger file's bytes, for a failure message.
func orchestrateDcloseSurrogateRaw(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(raw)
}
