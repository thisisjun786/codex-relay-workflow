package interview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fromJSON decodes text the way JSON.parse hands a value to the oracle: objects become
// map[string]any, arrays []any and numbers float64.
func fromJSON(t *testing.T, text string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("bad test JSON %q: %v", text, err)
	}
	return v
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// dimsJSON is a dimensions object with the same score on every axis.
func dimsJSON(score string) string {
	return fmt.Sprintf(`{"goal":%[1]s,"constraint":%[1]s,"success":%[1]s,"ontology":%[1]s}`, score)
}

// DIMENSIONS are the four IPABCD interview axes in order.
func TestDimensionsAreTheFourInterviewAxesInOrder(t *testing.T) {
	want := [4]Dimension{"goal", "constraint", "success", "ontology"}
	if got := DimensionOrder(); got != want {
		t.Fatalf("DimensionOrder() = %v, want %v", got, want)
	}
	var d Dimensions
	for _, dim := range want {
		if d.Score(dim) == nil {
			t.Fatalf("Score(%q) is nil", dim)
		}
	}
	if d.Score("other") != nil {
		t.Fatal("Score of a name that is not an axis must be nil")
	}
}

// A fresh tracker marshals in the oracle's key order, every array as [] and no
// ontologySchema key.
func TestDefaultInterviewMarshalsInTheOraclesShape(t *testing.T) {
	const score = `{"level":"low","known":[],"unknown":[],"confidence":0}`
	want := `{"roundId":0,"dimensions":` + dimsJSON(score) + `,"contradictions":[],"assumptions":[],` +
		`"autoResolveCount":0,"consecutiveAutoResolves":0,"scanRounds":0,"lastScanRoundId":0}`
	if got := marshal(t, DefaultInterview(0)); got != want {
		t.Fatalf("DefaultInterview(0) = %s\nwant %s", got, want)
	}
}

// reconstructInterview: null/non-object -> null (the oracle's undefined has no Go value).
func TestReconstructOfANonObjectIsNil(t *testing.T) {
	for _, v := range []any{nil, "nope", float64(42), 42, true, []any{}, []any{map[string]any{}}} {
		if got := ReconstructInterview(v); got != nil {
			t.Errorf("ReconstructInterview(%#v) = %+v, want nil", v, got)
		}
	}
}

// reconstructInterview: invalid level->low, invalid confidence->0 (T3 fail-closed).
func TestReconstructInvalidLevelAndConfidenceFailClosed(t *testing.T) {
	r := ReconstructInterview(fromJSON(t, `{"roundId":"r","dimensions":{"goal":{"level":"ZZZ","confidence":9,"known":"bad","unknown":["ok"]}}}`))
	if r == nil {
		t.Fatal("ReconstructInterview returned nil for an object")
	}
	goal := r.Dimensions.Goal
	if goal.Level != LevelLow || goal.Confidence != 0 {
		t.Errorf("level, confidence = %q, %v, want low, 0", goal.Level, goal.Confidence)
	}
	if goal.Known == nil || len(goal.Known) != 0 {
		t.Errorf("a non-array known must become an empty array, got %#v", goal.Known)
	}
	if !reflect.DeepEqual(goal.Unknown, []string{"ok"}) {
		t.Errorf("unknown = %#v, want [ok]", goal.Unknown)
	}
	if IsInterviewReady(r) {
		t.Error("a reconstructed corrupt tracker must not be ready")
	}
}

// reconstructInterview: legacy assumption w/o recorded -> recorded:false (T3). Only the
// boolean true records one, and a severity outside the three known ones is dropped.
func TestReconstructLegacyAssumptionIsUnrecorded(t *testing.T) {
	r := ReconstructInterview(fromJSON(t, `{"assumptions":[{"id":"a","text":"t"}]}`))
	if len(r.Assumptions) != 1 || r.Assumptions[0].Recorded {
		t.Fatalf("legacy assumption must reconstruct as recorded:false, got %+v", r.Assumptions)
	}
	r = ReconstructInterview(fromJSON(t, `{"assumptions":[{"id":"a","text":"t","recorded":true,"severity":"low","requiresUserReview":true},`+
		`{"id":"b","recorded":"true","severity":"bogus","requiresUserReview":"yes"}]}`))
	want := []Assumption{{ID: "a", Text: "t", Recorded: true, Severity: SeverityLow, RequiresUserReview: true}, {ID: "b"}}
	if !reflect.DeepEqual(r.Assumptions, want) {
		t.Fatalf("assumptions = %+v, want %+v", r.Assumptions, want)
	}
}

// reconstructInterview: unknown contradiction severity -> high (blocks readiness). A
// contradiction without a string contradictionId falls back to a legacy id key.
func TestReconstructUnknownSeverityIsHigh(t *testing.T) {
	r := ReconstructInterview(fromJSON(t, `{"contradictions":[{"id":"c","severity":"weird","summary":"s"}]}`))
	if got := r.Contradictions[0]; got != (Contradiction{ContradictionID: "c", Severity: SeverityHigh, Summary: "s"}) {
		t.Fatalf("contradiction = %+v, want id c, high severity, summary s", got)
	}
	r = ReconstructInterview(fromJSON(t, `{"contradictions":[{"contradictionId":5,"id":"x","severity":"medium"},{"contradictionId":"","id":"z"}]}`))
	if got := r.Contradictions[0]; got.ContradictionID != "x" || got.Severity != SeverityMedium {
		t.Errorf("a non-string contradictionId falls back to id, got %+v", got)
	}
	if got := r.Contradictions[1].ContradictionID; got != "" {
		t.Errorf("an empty contradictionId is a string and is kept, got %q", got)
	}
}

// reconstructInterview: arrays capped at MAX_TRACKER_ARRAY (T2), dropping the oldest.
func TestReconstructCapsArraysDroppingTheOldest(t *testing.T) {
	big := make([]any, 0, MaxTrackerArray+20)
	for i := 0; i < MaxTrackerArray+20; i++ {
		big = append(big, map[string]any{"contradictionId": fmt.Sprintf("c%d", i), "severity": "low", "summary": "s"})
	}
	r := ReconstructInterview(map[string]any{"contradictions": big})
	if len(r.Contradictions) != MaxTrackerArray {
		t.Fatalf("len = %d, want %d", len(r.Contradictions), MaxTrackerArray)
	}
	if first, last := r.Contradictions[0].ContradictionID, r.Contradictions[MaxTrackerArray-1].ContradictionID; first != "c20" || last != "c69" {
		t.Errorf("first, last = %s, %s, want c20, c69 (the newest survive)", first, last)
	}
}

// CRITICAL-1: malformed persisted contradiction/assumption entries BLOCK readiness. The
// oracle test leaves scanRounds unset, which blocks readiness by itself; here scanRounds is
// 1 and a clean control is ready, so only the malformed entry can be the cause.
func TestMalformedPersistedEntriesBlockReadiness(t *testing.T) {
	build := func(contradictions, assumptions string) *Tracker {
		return ReconstructInterview(fromJSON(t, fmt.Sprintf(`{"roundId":1,"scanRounds":1,"dimensions":%s,"contradictions":%s,"assumptions":%s}`,
			dimsJSON(`{"level":"max","known":[],"unknown":[],"confidence":1}`), contradictions, assumptions)))
	}
	if !IsInterviewReady(build("[]", "[]")) {
		t.Fatal("control: a clean all-max tracker with one scan must be ready")
	}
	rc := build(`["bad"]`, "[]")
	if want := []Contradiction{{Severity: SeverityHigh, Summary: "[malformed contradiction entry]"}}; !reflect.DeepEqual(rc.Contradictions, want) {
		t.Fatalf("a malformed contradiction must be kept as a high blocker, got %+v", rc.Contradictions)
	}
	if IsInterviewReady(rc) {
		t.Error("a malformed contradiction entry must block readiness")
	}
	ra := build("[]", `["legacy",null]`)
	if want := []Assumption{{Text: "[malformed assumption entry]"}, {Text: "[malformed assumption entry]"}}; !reflect.DeepEqual(ra.Assumptions, want) {
		t.Fatalf("malformed assumptions must be kept unrecorded, got %+v", ra.Assumptions)
	}
	if IsInterviewReady(ra) {
		t.Error("malformed assumption entries must block readiness")
	}
}

// normalizeInterview caps oversized arrays for the write path (T2) and keeps roundId.
func TestNormalizeCapsOversizedArrays(t *testing.T) {
	tr := DefaultInterview(3)
	for i := 0; i < MaxTrackerArray+9; i++ {
		tr.Dimensions.Goal.Known = append(tr.Dimensions.Goal.Known, fmt.Sprintf("k%d", i))
		tr.Contradictions = append(tr.Contradictions, Contradiction{ContradictionID: fmt.Sprintf("c%d", i), Severity: SeverityLow, Summary: "s"})
		tr.Assumptions = append(tr.Assumptions, Assumption{ID: fmt.Sprintf("a%d", i), Text: "t", Recorded: true})
	}
	n := Normalize(tr)
	if len(n.Dimensions.Goal.Known) != MaxTrackerArray || len(n.Contradictions) != MaxTrackerArray || len(n.Assumptions) != MaxTrackerArray {
		t.Fatalf("lens = %d, %d, %d, want %d each", len(n.Dimensions.Goal.Known), len(n.Contradictions), len(n.Assumptions), MaxTrackerArray)
	}
	if n.RoundID != 3 || n.Dimensions.Goal.Known[0] != "k9" || n.Contradictions[0].ContradictionID != "c9" || n.Assumptions[0].ID != "a9" {
		t.Errorf("Normalize must keep roundId and drop the oldest: %d %v", n.RoundID, n.Dimensions.Goal.Known[0])
	}
	if len(tr.Contradictions) != MaxTrackerArray+9 {
		t.Error("Normalize must not modify its argument")
	}
	if Normalize(nil) != nil {
		t.Error("Normalize(nil) must be nil")
	}
}

// Normalize applies the reconstruct rules to each score and turns a missing array into an
// empty one, so a normalized tracker marshals [] and never null.
func TestNormalizeFailsClosedPerScore(t *testing.T) {
	n := Normalize(&Tracker{RoundID: -2, ScanRounds: -4, Dimensions: Dimensions{Goal: DimensionScore{Level: "ZZ", Known: []string{"a"}, Confidence: 9}}})
	if n.RoundID != 0 || n.ScanRounds != 0 {
		t.Errorf("negative counters must be 0, got %d and %d", n.RoundID, n.ScanRounds)
	}
	if g := n.Dimensions.Goal; g.Level != LevelLow || g.Confidence != 0 || !reflect.DeepEqual(g.Known, []string{"a"}) {
		t.Errorf("goal = %+v", g)
	}
	if strings.Contains(marshal(t, n), "null") {
		t.Errorf("a normalized tracker must not marshal null: %s", marshal(t, n))
	}
}

// T6: roundId is a non-negative integer (monotonic horizon).
func TestRoundIDIsANonNegativeInteger(t *testing.T) {
	for in, want := range map[string]int64{`"bad"`: 0, "7.9": 7, "-1": 0, "null": 0} {
		if got := ReconstructInterview(fromJSON(t, `{"roundId":`+in+`}`)).RoundID; got != want {
			t.Errorf("roundId %s = %d, want %d", in, got, want)
		}
	}
	if DefaultInterview(0).RoundID != 0 || DefaultInterview(-3).RoundID != 0 {
		t.Error("DefaultInterview must give a non-negative round")
	}
}

// A counter or confidence accepts a finite JSON number (float64, json.Number, int or int64)
// in range; every other value is not a number and fails closed.
func TestNumberCoercionFailsClosed(t *testing.T) {
	negZero := math.Copysign(0, -1)
	for _, c := range []struct {
		in   any
		want int64
	}{
		{7.9, 7}, {-1.0, 0}, {negZero, 0}, {math.NaN(), 0}, {math.Inf(1), 0}, {json.Number("12"), 12}, {json.Number("1e400"), 0},
		{json.Number("abc"), 0}, {5, 5}, {int64(-5), 0}, {int64(math.MaxInt64), math.MaxInt64}, {1e30, math.MaxInt64}, {"5", 0}, {true, 0}, {nil, 0},
	} {
		if got := roundIDNum(c.in); got != c.want {
			t.Errorf("roundIDNum(%#v) = %d, want %d", c.in, got, c.want)
		}
	}
	for _, c := range []struct {
		in   any
		want float64
	}{
		{0.5, 0.5}, {1.0, 1}, {negZero, 0}, {-0.1, 0}, {1.1, 0}, {math.NaN(), 0}, {json.Number("0.25"), 0.25}, {1, 1}, {2, 0}, {"0.5", 0}, {nil, 0},
	} {
		if got := confidence(c.in); got != c.want || math.Signbit(got) {
			t.Errorf("confidence(%#v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// ontologySchema is absent by default and on a fresh tracker, and a normalized tracker
// without one gains no key.
func TestOntologySchemaIsAbsentByDefault(t *testing.T) {
	if DefaultInterview(0).OntologySchema != nil {
		t.Fatal("a fresh tracker has no ontologySchema")
	}
	if n := Normalize(DefaultInterview(0)); n.OntologySchema != nil || strings.Contains(marshal(t, n), "ontologySchema") {
		t.Fatalf("Normalize added an ontologySchema: %s", marshal(t, n))
	}
}

// ontologySchema survives reconstruct + normalize round-trip, in the oracle's key order.
func TestOntologySchemaSurvivesReconstructAndNormalize(t *testing.T) {
	recon := ReconstructInterview(fromJSON(t, `{"roundId":1,"ontologySchema":[{"name":"User","fields":["id","email"],"relationships":[{"to":"Order","kind":"has-many"}]},{"name":"Order","fields":["id"],"relationships":[]}]}`))
	want := OntologyEntity{Name: "User", Fields: []string{"id", "email"}, Relationships: []OntologyRelationship{{To: "Order", Kind: "has-many"}}}
	if len(recon.OntologySchema) != 2 || !reflect.DeepEqual(recon.OntologySchema[0], want) {
		t.Fatalf("reconstruct = %+v", recon.OntologySchema)
	}
	norm := Normalize(recon)
	if !reflect.DeepEqual(norm.OntologySchema, recon.OntologySchema) {
		t.Fatalf("normalize changed the schema: %+v", norm.OntologySchema)
	}
	if !strings.Contains(marshal(t, norm), `"ontologySchema":[{"name":"User","fields":["id","email"],"relationships":[{"to":"Order","kind":"has-many"}]},{"name":"Order","fields":["id"],"relationships":[]}]}`) {
		t.Errorf("schema marshals out of order: %s", marshal(t, norm))
	}
}

// Malformed ontology entries are dropped, empty => nil.
func TestMalformedOntologyEntriesAreDropped(t *testing.T) {
	for _, in := range []any{"nope", nil, []any{}, fromJSON(t, `[{"fields":["x"]},{"name":""},"str",null]`)} {
		if got := ReconstructOntologySchema(in); got != nil {
			t.Errorf("ReconstructOntologySchema(%#v) = %+v, want nil", in, got)
		}
	}
	got := ReconstructOntologySchema(fromJSON(t, `[{"name":"E","fields":["a",2,"b"],"relationships":[{"to":"F","kind":"ref"},{"kind":"x"},{"to":"G"},"str"]}]`))
	want := []OntologyEntity{{Name: "E", Fields: []string{"a", "b"}, Relationships: []OntologyRelationship{{To: "F", Kind: "ref"}, {To: "G"}}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// The tracker the oracle persisted in two corpus fixtures is reproduced byte for byte by
// reconstruct + normalize + marshal. This checks the persisted tracker's key order and value
// forms only; it does not replay the fixtures, which need the scan and orchestrate commands.
func TestPersistedTrackerBytesMatchOracleFixtures(t *testing.T) {
	for _, id := range []string{"cli__scan__record_round_updates_tracker", "cli__scan__derive_from_captured_answers"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contract", "fixtures", "cxc", id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var fx struct {
			Expect struct {
				Tree map[string]struct {
					JSON json.RawMessage `json:"json"`
				} `json:"tree"`
			} `json:"expect"`
		}
		var session struct {
			Interview json.RawMessage `json:"interview"`
		}
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(fx.Expect.Tree["ws/.codexclaw/sessions/rec-s1.json"].JSON, &session); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		var want bytes.Buffer
		if err := json.Compact(&want, session.Interview); err != nil {
			t.Fatal(err)
		}
		if got := marshal(t, Normalize(ReconstructInterview(fromJSON(t, string(session.Interview))))); got != want.String() {
			t.Errorf("%s:\n got %s\nwant %s", id, got, want.String())
		}
	}
}
