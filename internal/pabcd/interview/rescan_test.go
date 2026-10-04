package interview_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	I "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview/ledger"
)

func rescanDeps() I.RescanDeps {
	return I.RescanDeps{GoalStatus: func() host.GoalStatus { return host.GoalInactive }, ReadQaEvents: func(cwd, id string) []json.RawMessage {
		rows := []json.RawMessage{}
		for _, e := range ledger.ReadQaEvents(cwd, id) {
			rows = append(rows, e.Raw)
		}
		return rows
	}}
}
func rescanCapture(cwd, turn string, answered bool) {
	answers := map[string]any{}
	if answered {
		answers["goal"] = map[string]any{"answers": []any{"yes"}}
	}
	ledger.CaptureInterviewAnswers(ledger.CaptureInput{Cwd: cwd, SessionID: "s", TurnID: turn,
		ToolInput: map[string]any{"questions": []any{map[string]any{"id": "goal", "question": "?"}}}, ToolResponse: map[string]any{"answers": answers}})
}
func rescanAssert(t *testing.T, got I.PendingInterviewWork, ids []string, high int, pending bool) {
	t.Helper()
	want := I.PendingInterviewWork{PendingQuestionIDs: ids, HighContradictionCount: high, Pending: pending}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pending work = %#v, want %#v", got, want)
	}
}
func rescanTracker(severities ...I.ContradictionSeverity) *I.Tracker {
	tr := I.DefaultInterview(1)
	for _, s := range severities {
		tr.Contradictions = append(tr.Contradictions, I.Contradiction{Severity: s, Summary: "x"})
	}
	return tr
}
func TestRescanPendingPair(t *testing.T) {
	cwd := t.TempDir()
	rescanCapture(cwd, "t1", false)
	rescanAssert(t, I.HasPendingInterviewWork(cwd, "s", nil, rescanDeps()), []string{"t1\x00goal"}, 0, true)
}
func TestRescanAnsweredPair(t *testing.T) {
	cwd := t.TempDir()
	rescanCapture(cwd, "t1", true)
	rescanAssert(t, I.HasPendingInterviewWork(cwd, "s", nil, rescanDeps()), []string{}, 0, false)
}
func TestRescanReusedQuestionAcrossTurns(t *testing.T) {
	cwd := t.TempDir()
	rescanCapture(cwd, "t1", true)
	rescanCapture(cwd, "t2", false)
	rescanAssert(t, I.HasPendingInterviewWork(cwd, "s", nil, rescanDeps()), []string{"t2\x00goal"}, 0, true)
}
func TestRescanHighContradictionsOnly(t *testing.T) {
	rescanAssert(t, I.HasPendingInterviewWork(t.TempDir(), "s", rescanTracker(I.SeverityLow, I.SeverityHigh, I.SeverityMedium), rescanDeps()), []string{}, 1, true)
}
func TestRescanLowMediumNotPending(t *testing.T) {
	rescanAssert(t, I.HasPendingInterviewWork(t.TempDir(), "s", rescanTracker(I.SeverityLow, I.SeverityMedium), rescanDeps()), []string{}, 0, false)
}
func TestRescanGoalSuppression(t *testing.T) {
	cwd := t.TempDir()
	rescanCapture(cwd, "t1", false)
	for _, status := range []host.GoalStatus{host.GoalActive, host.GoalUnreadable} {
		deps := rescanDeps()
		deps.GoalStatus = func() host.GoalStatus { return status }
		deps.ReadQaEvents = func(string, string) []json.RawMessage { t.Fatal("suppressed rescan read QA"); return nil }
		rescanAssert(t, I.HasPendingInterviewWork(cwd, "s", rescanTracker(I.SeverityHigh), deps), []string{}, 0, false)
	}
}
func TestRescanNextRoundCounters(t *testing.T) {
	tr := I.DefaultInterview(0)
	if got := I.ComputeNextScanRound(tr); got != 1 {
		t.Fatalf("initial next=%v", got)
	}
	tr.ScanRounds, tr.RoundID, tr.LastScanRoundID = 3, 99, 50
	if got := I.ComputeNextScanRound(tr); got != 4 {
		t.Fatalf("next=%v, want 4", got)
	}
	for _, raw := range []any{nil, map[string]any{"scanRounds": "x"}} {
		if got := I.ComputeNextScanRound(raw); got != 1 {
			t.Fatalf("malformed next=%v", got)
		}
	}
}
func TestRescanMalformedMissing(t *testing.T) {
	for _, tr := range []any{nil, map[string]any{"contradictions": "nope"}} {
		rescanAssert(t, I.HasPendingInterviewWork(t.TempDir(), "s", tr, rescanDeps()), []string{}, 0, false)
	}
	deps := rescanDeps()
	deps.ReadQaEvents = nil
	rescanAssert(t, I.HasPendingInterviewWork(t.TempDir(), "s", rescanTracker(I.SeverityHigh), deps), []string{}, 1, true)
}
func TestRescanScanRowsExcluded(t *testing.T) {
	cwd := t.TempDir()
	p := filepath.Join(cwd, ".crw", "interviews")
	if err := os.MkdirAll(p, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "s.jsonl"), []byte(`{"event":"scan_completed","eventId":"e","turnId":"t","questionId":"goal","contradictions":3,"high":3}`), 0600); err != nil {
		t.Fatal(err)
	}
	rescanAssert(t, I.HasPendingInterviewWork(cwd, "s", I.DefaultInterview(1), rescanDeps()), []string{}, 0, false)
}
func TestRescanRecordedOracle(t *testing.T) {
	cases := mindsLoadCases(t)
	for _, c := range cases.Pending {
		t.Run(c.ID, func(t *testing.T) {
			rows := []json.RawMessage{}
			for _, row := range c.Rows {
				b, err := json.Marshal(row)
				if err != nil {
					t.Fatal(err)
				}
				rows = append(rows, b)
			}
			deps := rescanDeps()
			deps.ReadQaEvents = func(string, string) []json.RawMessage { return rows }
			got := I.HasPendingInterviewWork(t.TempDir(), "s", c.Tracker, deps)
			if !reflect.DeepEqual(got, c.Expected) {
				t.Fatalf("got %#v, oracle %#v", got, c.Expected)
			}
		})
	}
	for n, c := range cases.Rounds {
		if got := I.ComputeNextScanRound(c.Tracker); got != c.Expected {
			t.Fatalf("round %d=%v, oracle %v", n, got, c.Expected)
		}
	}
}
func TestRescanDefaultGoalReader(t *testing.T) {
	cwd := t.TempDir()
	rescanCapture(cwd, "t1", false)
	deps := rescanDeps()
	deps.GoalStatus = nil
	// The default path is computed from the isolated host environment.
	t.Setenv("CODEX_SQLITE_HOME", cwd)
	rescanAssert(t, I.HasPendingInterviewWork(cwd, "s", nil, deps), []string{"t1\x00goal"}, 0, true)
	if err := os.WriteFile(filepath.Join(cwd, host.GoalsDBFilename), []byte("not sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	rescanAssert(t, I.HasPendingInterviewWork(cwd, "s", rescanTracker(I.SeverityHigh), deps), []string{}, 0, false)
}

func TestRescanReaderFailure(t *testing.T) {
	deps := rescanDeps()
	deps.ReadQaEvents = func(string, string) []json.RawMessage { panic("QA read failure") }
	rescanAssert(t, I.HasPendingInterviewWork(t.TempDir(), "s", rescanTracker(I.SeverityHigh), deps), []string{}, 1, true)
	deps.ReadQaEvents = func(string, string) []json.RawMessage {
		return []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`[]`), json.RawMessage(`broken`)}
	}
	rescanAssert(t, I.HasPendingInterviewWork(t.TempDir(), "s", nil, deps), []string{}, 0, false)
}

func TestRescanQAOverflowMetadata(t *testing.T) {
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".crw", "interviews")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	c := mindsLoadCases(t).Overflow
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(c.Row), 0600); err != nil {
		t.Fatal(err)
	}
	got := I.HasPendingInterviewWork(cwd, "s", nil, rescanDeps())
	if !reflect.DeepEqual(got, c.Expected) {
		t.Fatalf("overflow QA=%#v, oracle %#v", got, c.Expected)
	}
}
