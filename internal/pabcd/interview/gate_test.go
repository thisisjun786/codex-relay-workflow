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
	tr.ScanRounds = 1 // readiness also requires scan-evidence
	tr.LastScanRoundID = 1
	return tr
}

// defaultInterview is not ready (all dimensions low).
func TestDefaultInterviewIsNotReady(t *testing.T) {
	if IsInterviewReady(DefaultInterview(0)) {
		t.Fatal("a fresh tracker must not be ready")
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

// A single contradiction blocks readiness, whatever its severity.
func TestASingleContradictionBlocksReadiness(t *testing.T) {
	tr := readyTracker()
	tr.Contradictions = []Contradiction{{ContradictionID: "c1", Severity: SeverityLow, Summary: "x"}}
	if IsInterviewReady(tr) {
		t.Fatal("a low-severity contradiction must still block")
	}
}

// An unrecorded assumption blocks readiness until it is recorded.
func TestAnUnrecordedAssumptionBlocksReadiness(t *testing.T) {
	tr := readyTracker()
	tr.Assumptions = []Assumption{{ID: "a1", Text: "assume X", Recorded: false}}
	if IsInterviewReady(tr) {
		t.Fatal("an unrecorded assumption must block")
	}
	tr.Assumptions[0].Recorded = true
	if !IsInterviewReady(tr) {
		t.Fatal("a recorded assumption must not block")
	}
}

// null/malformed -> false (fail-closed). The oracle's {} cast is the zero Tracker here.
func TestNilAndZeroTrackerAreNotReady(t *testing.T) {
	if IsInterviewReady(nil) || IsInterviewReady(&Tracker{}) {
		t.Fatal("nil and the zero tracker must not be ready")
	}
}

// 131: scan-evidence is required for readiness (scanRounds 0 blocks, 1 allows); a negative
// count reads as 0.
func TestScanEvidenceIsRequiredForReadiness(t *testing.T) {
	tr := readyTracker()
	for rounds, want := range map[int64]bool{0: false, -1: false, 1: true} {
		tr.ScanRounds = rounds
		if got := IsInterviewReady(tr); got != want {
			t.Errorf("scanRounds %d: ready = %v, want %v", rounds, got, want)
		}
	}
}

// 131: reconstruct defaults the scan fields to 0, so a legacy tracker is not silently ready.
func TestReconstructDefaultsScanFieldsToZero(t *testing.T) {
	legacy := `{"roundId":1,"dimensions":` + dimsJSON(`{"level":"max","known":["k"],"unknown":[],"confidence":1}`) +
		`,"contradictions":[],"assumptions":[{"id":"a","text":"x","recorded":true}]}`
	r := ReconstructInterview(fromJSON(t, legacy))
	if r.ScanRounds != 0 || r.LastScanRoundID != 0 {
		t.Fatalf("scan fields = %d and %d, want 0 and 0", r.ScanRounds, r.LastScanRoundID)
	}
	if IsInterviewReady(r) {
		t.Error("a legacy ready-shaped tracker must not pass without scan-evidence")
	}
}

// CRITICAL-2: a partial {level:"max"} dimension is NOT ready (full shape required). A
// missing array is a nil slice, which is also what a direct decode of the partial JSON gives.
func TestAPartialMaxDimensionIsNotReady(t *testing.T) {
	partial := readyTracker()
	for _, d := range DimensionOrder() {
		*partial.Dimensions.Score(d) = DimensionScore{Level: LevelMax}
	}
	if IsInterviewReady(partial) {
		t.Error("a typed partial score (nil arrays) must not be ready")
	}
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
		mutate(tr)
		if IsInterviewReady(tr) {
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
	want := "no contradiction scan has been recorded for this interview|1 high-severity contradiction(s) still open"
	if bad.Ready || bad.ScanRan || bad.HighContradictionCount != 1 || strings.Join(bad.Warnings, "|") != want {
		t.Fatalf("blocked gate = %+v", bad)
	}
	if got := EvaluateInterviewGate(nil, nil); got.Ready || got.ScanRan || len(got.Warnings) != 1 {
		t.Errorf("nil tracker gate = %+v", got)
	}
	tr = readyTracker()
	tr.Contradictions = []Contradiction{{ContradictionID: "c", Severity: SeverityMedium, Summary: "s"}}
	med := EvaluateInterviewGate(tr, nil)
	if med.Ready || med.HighContradictionCount != 0 || strings.Join(med.Warnings, "|") != "interview is not ready (dimensions/assumptions incomplete)" {
		t.Errorf("medium-contradiction gate = %+v", med)
	}
}

// A "high" level is only as good as the answer it was derived from: with ledger evidence
// supplied every high dimension must be backed, "max" needs no backing, and evidence that is
// supplied but empty backs nothing. Without evidence the gate is the shape check.
func TestGateRequiresLedgerBackingForHighDimensions(t *testing.T) {
	tr := readyTracker()
	for _, d := range DimensionOrder() {
		tr.Dimensions.Score(d).Level = LevelHigh
	}
	if g := EvaluateInterviewGate(tr, nil); !g.Ready {
		t.Errorf("without evidence the gate is the shape check: %+v", g)
	}
	const tail = " reached \"high\" without an answered question in the interview ledger \u2014 " +
		"ask and record one per dimension (`crw pabcd scan record --derive --map <questionId>=<dimension>`), or assert the level deliberately"
	if g := EvaluateInterviewGate(tr, &GateEvidence{}); g.Ready || strings.Join(g.Warnings, "|") != "goal, constraint, success, ontology"+tail {
		t.Errorf("empty evidence gate = %+v", g)
	}
	partly := &GateEvidence{BackedDimensions: map[Dimension]bool{DimensionGoal: true, DimensionSuccess: true}}
	if g := EvaluateInterviewGate(tr, partly); g.Ready || strings.Join(g.Warnings, "|") != "constraint, ontology"+tail {
		t.Errorf("partly backed gate = %+v", g)
	}
	all := &GateEvidence{BackedDimensions: map[Dimension]bool{"goal": true, "constraint": true, "success": true, "ontology": true}}
	if g := EvaluateInterviewGate(tr, all); !g.Ready || len(g.Warnings) != 0 {
		t.Errorf("fully backed gate = %+v", g)
	}
	if g := EvaluateInterviewGate(readyTracker(), &GateEvidence{}); !g.Ready {
		t.Errorf("max dimensions need no backing: %+v", g)
	}
	notShape := DefaultInterview(0)
	notShape.Dimensions.Goal.Level = LevelHigh
	if g := EvaluateInterviewGate(notShape, &GateEvidence{}); g.Ready || strings.Join(g.Warnings, "|") != "no contradiction scan has been recorded for this interview" {
		t.Errorf("provenance is only computed for a tracker with the right shape: %+v", g)
	}
}
