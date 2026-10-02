package interview

import (
	"encoding/json"
	"strings"
	"testing"
)

func readyTracker() *Tracker {
	tr := DefaultInterview(0)
	for _, d := range DimensionOrder() {
		*tr.Dimensions.Score(d) = DimensionScore{Level: LevelMax, Known: []string{"x"}, Unknown: []string{}, Confidence: 1}
	}
	tr.ScanRounds, tr.LastScanRoundID = 1, 1 // readiness also requires scan-evidence
	return tr
}

// defaultInterview is not ready (all dimensions low); null/malformed -> false (fail-closed),
// where the oracle's {} cast is the zero Tracker.
func TestDefaultNilAndZeroTrackersAreNotReady(t *testing.T) {
	if IsInterviewReady(DefaultInterview(0)) || IsInterviewReady(nil) || IsInterviewReady(&Tracker{}) {
		t.Fatal("a fresh, nil or zero tracker must not be ready")
	}
}

// isInterviewReady: high or max, no contradictions, recorded assumptions.
func TestReadinessNeedsHighOrMaxEverywhere(t *testing.T) {
	tr := readyTracker()
	if !IsInterviewReady(tr) {
		t.Fatal("an all-max tracker with a scan must be ready")
	}
	for level, want := range map[DimensionLevel]bool{LevelHigh: true, LevelMid: false, LevelLow: false} {
		tr.Dimensions.Goal.Level = level
		if got := IsInterviewReady(tr); got != want {
			t.Errorf("goal %q: ready = %v, want %v", level, got, want)
		}
	}
}

// A single contradiction, whatever its severity, and an unrecorded assumption each block
// readiness.
func TestContradictionsAndUnrecordedAssumptionsBlockReadiness(t *testing.T) {
	tr := readyTracker()
	tr.Contradictions = []Contradiction{{ContradictionID: "c1", Severity: SeverityLow, Summary: "x"}}
	if IsInterviewReady(tr) {
		t.Error("a low-severity contradiction must still block")
	}
	tr = readyTracker()
	tr.Assumptions = []Assumption{{ID: "a1", Text: "assume X"}}
	if IsInterviewReady(tr) {
		t.Error("an unrecorded assumption must block")
	}
	tr.Assumptions[0].Recorded = true
	if !IsInterviewReady(tr) {
		t.Error("a recorded assumption must not block")
	}
}

// 131: scan-evidence is required for readiness (0 blocks, 1 allows, negative reads as 0), and
// reconstruct defaults the scan fields to 0, so a legacy tracker is not silently ready.
func TestScanEvidenceIsRequiredForReadiness(t *testing.T) {
	tr := readyTracker()
	for rounds, want := range map[int64]bool{0: false, -1: false, 1: true} {
		if tr.ScanRounds = rounds; IsInterviewReady(tr) != want {
			t.Errorf("scanRounds %d: ready = %v, want %v", rounds, !want, want)
		}
	}
	legacy := ReconstructInterview(fromJSON(t, `{"roundId":1,"dimensions":`+dimsJSON(`{"level":"max","known":["k"],"unknown":[],"confidence":1}`)+
		`,"contradictions":[],"assumptions":[{"id":"a","text":"x","recorded":true}]}`))
	if legacy.ScanRounds != 0 || legacy.LastScanRoundID != 0 || IsInterviewReady(legacy) {
		t.Errorf("a legacy tracker must reconstruct with scan fields 0 and not be ready: %+v", legacy)
	}
}

// CRITICAL-2: a partial {level:"max"} dimension is NOT ready (full shape required). A missing array
// is a nil slice, as a direct decode gives; a missing confidence decodes to a valid 0, which is why
// persisted JSON goes through ReconstructInterview.
func TestAPartialMaxDimensionIsNotReady(t *testing.T) {
	in := `{"roundId":1,"scanRounds":1,"dimensions":` + dimsJSON(`{"level":"max"}`) + `,"contradictions":[],"assumptions":[]}`
	var decoded Tracker
	if err := json.Unmarshal([]byte(in), &decoded); err != nil {
		t.Fatal(err)
	}
	if IsInterviewReady(&decoded) {
		t.Error("a decoded partial {level:max} dimension must not be ready")
	}
	for name, mutate := range map[string]func(*Tracker){
		"nil known":          func(tr *Tracker) { tr.Dimensions.Success.Known = nil },
		"nil unknown":        func(tr *Tracker) { tr.Dimensions.Ontology.Unknown = nil },
		"nil contradictions": func(tr *Tracker) { tr.Contradictions = nil },
		"nil assumptions":    func(tr *Tracker) { tr.Assumptions = nil },
		"confidence above 1": func(tr *Tracker) { tr.Dimensions.Goal.Confidence = 1.5 },
		"confidence below 0": func(tr *Tracker) { tr.Dimensions.Goal.Confidence = -0.5 },
		"unknown level":      func(tr *Tracker) { tr.Dimensions.Goal.Level = "maximum" },
	} {
		tr := readyTracker()
		if mutate(tr); IsInterviewReady(tr) {
			t.Errorf("%s: must not be ready", name)
		}
	}
	// Ported as-is: reconstruct fills the missing arrays and keeps the valid level, so the
	// repaired tracker passes the shape check that the raw partial failed.
	if !IsInterviewReady(ReconstructInterview(fromJSON(t, in))) {
		t.Error("the oracle's reconstruct repairs a partial max score into a ready-shaped one")
	}
}

// 131: evaluateInterviewGate reports scanRan/high-contradiction/warnings. Only a high
// contradiction is counted; a medium one blocks readiness with the fallback warning.
func TestGateReportsScanRanHighContradictionsAndWarnings(t *testing.T) {
	tr := readyTracker()
	if ok := EvaluateInterviewGate(tr, nil); !ok.Ready || !ok.ScanRan || ok.HighContradictionCount != 0 || ok.Warnings == nil || len(ok.Warnings) != 0 {
		t.Fatalf("ready gate = %+v", ok)
	}
	tr.ScanRounds = 0
	tr.Contradictions = []Contradiction{{ContradictionID: "c1", Severity: SeverityHigh, Summary: "x"}}
	bad := EvaluateInterviewGate(tr, nil)
	if bad.Ready || bad.ScanRan || bad.HighContradictionCount != 1 ||
		strings.Join(bad.Warnings, "|") != "no contradiction scan has been recorded for this interview|1 high-severity contradiction(s) still open" {
		t.Fatalf("blocked gate = %+v", bad)
	}
	if got := EvaluateInterviewGate(nil, nil); got.Ready || got.ScanRan || len(got.Warnings) != 1 {
		t.Errorf("nil tracker gate = %+v", got)
	}
	tr = readyTracker()
	tr.Contradictions = []Contradiction{{ContradictionID: "c", Severity: SeverityMedium, Summary: "s"}}
	if med := EvaluateInterviewGate(tr, nil); med.Ready || med.HighContradictionCount != 0 || strings.Join(med.Warnings, "|") != "interview is not ready (dimensions/assumptions incomplete)" {
		t.Errorf("medium-contradiction gate = %+v", med)
	}
}

// A "high" level is only as good as the answer it came from: with ledger evidence every high
// dimension must be backed, "max" needs no backing, and evidence supplied but empty backs
// nothing. Without evidence the gate is the shape check.
func TestGateRequiresLedgerBackingForHighDimensions(t *testing.T) {
	tr := readyTracker()
	for _, d := range DimensionOrder() {
		tr.Dimensions.Score(d).Level = LevelHigh
	}
	const tail = " reached \"high\" without an answered question in the interview ledger \u2014 " +
		"ask and record one per dimension (`crw pabcd scan record --derive --map <questionId>=<dimension>`), or assert the level deliberately"
	partly := &GateEvidence{BackedDimensions: map[Dimension]bool{DimensionGoal: true, DimensionSuccess: true}}
	all := &GateEvidence{BackedDimensions: map[Dimension]bool{"goal": true, "constraint": true, "success": true, "ontology": true}}
	for _, c := range []struct {
		name     string
		tracker  *Tracker
		evidence *GateEvidence
		ready    bool
		warning  string
	}{
		{"no evidence is the shape check", tr, nil, true, ""},
		{"empty evidence backs nothing", tr, &GateEvidence{}, false, "goal, constraint, success, ontology" + tail},
		{"partly backed", tr, partly, false, "constraint, ontology" + tail},
		{"fully backed", tr, all, true, ""},
		{"max needs no backing", readyTracker(), &GateEvidence{}, true, ""},
		{"provenance waits for the shape", DefaultInterview(0), &GateEvidence{}, false, "no contradiction scan has been recorded for this interview"},
	} {
		if g := EvaluateInterviewGate(c.tracker, c.evidence); g.Ready != c.ready || strings.Join(g.Warnings, "|") != c.warning {
			t.Errorf("%s: gate = %+v", c.name, g)
		}
	}
}
