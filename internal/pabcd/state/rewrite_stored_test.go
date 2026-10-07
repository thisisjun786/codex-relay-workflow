package state

import (
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
)

// CRW-815: RewriteKeepsStored is the one data-loss judgement every writer that rewrites the whole session
// state asks. It is false when RewriteKeepsUnverified refuses the stored unverified-subagent list, otherwise
// RewriteKeepsInterview's answer for the stored interview tracker; a document that is not one JSON object is
// false. A legacy D-close recovery marker is NOT part of it (decision 2026-10-07): since CRW-648 the reader
// restores the stored legacy flag, so a write no longer loses the marker's distinction, and the CLI reset
// must stay able to clear such a marker. The per-writer legacy refusal is DcloseRecoveryLegacy.

// rewriteStoredVerdict reads raw exactly as every caller does (the strict reader rebuilds next from it) and
// asks whether writing that next back over raw would keep every stored record.
func rewriteStoredVerdict(t *testing.T, raw string) ([]byte, State) {
	t.Helper()
	s, unreadable := restore("s1", []byte(raw), time.Now())
	if unreadable {
		t.Fatalf("the file does not read as a state: %s", raw)
	}
	return []byte(raw), s
}

// rewriteStoredTracker is a stored interview value whose contradictions array is one past the cap.
func rewriteStoredTracker() string {
	return `{"contradictions":` + rewriteInterviewContradictions(interview.MaxTrackerArray+1) + `,"assumptions":[]}`
}

func TestRewriteKeepsStoredIsTheSharedDataLossJudgement(t *testing.T) {
	legacy := `{"phase":"IDLE","sessionId":"s1","dcloseRecovery":{"sessionId":"s1","checkEpoch":"e","closedWorkPhaseId":"wp1","nextWorkPhaseId":null,"legacy":true}}`
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"an unverified list past the cap", rewriteFile(rewriteMany(MaxUnverifiedSubagents + 1)), false},
		{"a receipt past the cap", rewriteFile(`[` + rewriteRecord(`,"receiptClaimed":"`+strings.Repeat("r", MaxReceiptClaimLen+1)+`"`) + `]`), false},
		{"an interview tracker past the cap", rewriteInterviewFile(rewriteStoredTracker()), false},
		{"an ontology relationship list past the cap", rewriteInterviewFile(`{"contradictions":[],"assumptions":[],"ontologySchema":[{"name":"n","relationships":` + rewriteInterviewRelationships(interview.MaxTrackerArray+1) + `}]}`), false},
		{"no tracker at all", `{"phase":"B"}`, true},
		{"a null tracker", `{"phase":"B","interview":null}`, true},
		{"a document that is not one JSON object", `[1,2]`, false},
		{"a slug holding an unpaired surrogate escape", `{"phase":"B","slug":"\ud800"}`, false},
		{"a legacy D-close marker with nothing else to lose", legacy, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, next := []byte(c.raw), State{}
			if c.raw == `[1,2]` {
				// the reader refuses a document that is not an object, so the caller holds no rebuilt state
			} else {
				raw, next = rewriteStoredVerdict(t, c.raw)
			}
			if got := RewriteKeepsStored(raw, next); got != c.want {
				t.Fatalf("RewriteKeepsStored = %v, want %v", got, c.want)
			}
		})
	}
}

// TestDcloseRecoveryLegacyNamesOnlyTheRefusedMarker pins the separate per-writer predicate: only a marker the
// reader restored as Legacy is named, so every writer that refuses such a state today keeps refusing it and no
// writer starts refusing a marker that carries a successor.
func TestDcloseRecoveryLegacyNamesOnlyTheRefusedMarker(t *testing.T) {
	marker := func(edit func(*DcloseRecoveryMarker)) State {
		m := &DcloseRecoveryMarker{SessionID: "s1", CheckEpoch: "e", ClosedWorkPhaseID: "wp1"}
		edit(m)
		return State{SessionID: "s1", DcloseRecovery: m}
	}
	if !DcloseRecoveryLegacy(marker(func(m *DcloseRecoveryMarker) { m.Legacy = true })) {
		t.Error("a legacy marker was not named")
	}
	for name, s := range map[string]State{
		"no marker":                            {},
		"an authoritative no-successor marker": marker(func(*DcloseRecoveryMarker) {}),
		"a marker with a successor":            marker(func(m *DcloseRecoveryMarker) { m.NextWorkPhaseID = str("wp2") }),
	} {
		if DcloseRecoveryLegacy(s) {
			t.Errorf("%s was named legacy", name)
		}
	}
}
