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

// fromJSON decodes text as JSON.parse hands a value to the oracle (objects map[string]any,
// arrays []any, numbers float64).
func fromJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func marshal(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// dimsJSON is a dimensions object with the same score on every axis.
func dimsJSON(score string) string {
	return fmt.Sprintf(`{"goal":%[1]s,"constraint":%[1]s,"success":%[1]s,"ontology":%[1]s}`, score)
}

// DIMENSIONS are the four IPABCD interview axes in order.
func TestDimensionsAreTheFourInterviewAxesInOrder(t *testing.T) {
	want := [4]Dimension{"goal", "constraint", "success", "ontology"}
	var d Dimensions
	if DimensionOrder() != want || d.Score("goal") != &d.Goal || d.Score("ontology") != &d.Ontology || d.Score("other") != nil {
		t.Fatalf("DimensionOrder() = %v, want %v, and Score must name the fields", DimensionOrder(), want)
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
	if want := (DimensionScore{Level: LevelLow, Known: []string{}, Unknown: []string{"ok"}}); !reflect.DeepEqual(r.Dimensions.Goal, want) {
		t.Errorf("goal = %+v, want %+v", r.Dimensions.Goal, want)
	}
	if IsInterviewReady(r) {
		t.Error("a reconstructed corrupt tracker must not be ready")
	}
}

// reconstructInterview: legacy assumption w/o recorded -> recorded:false (T3). Only the
// boolean true records one, and a severity outside the three known ones is dropped.
func TestReconstructLegacyAssumptionIsUnrecorded(t *testing.T) {
	r := ReconstructInterview(fromJSON(t, `{"assumptions":[{"id":"a","text":"t"},{"id":"b","text":"t","recorded":true,"severity":"low","requiresUserReview":true},`+
		`{"id":"c","recorded":"true","severity":"bogus","requiresUserReview":"yes"}]}`))
	want := []Assumption{{ID: "a", Text: "t"}, {ID: "b", Text: "t", Recorded: true, Severity: SeverityLow, RequiresUserReview: true}, {ID: "c"}}
	if !reflect.DeepEqual(r.Assumptions, want) {
		t.Fatalf("assumptions = %+v, want %+v", r.Assumptions, want)
	}
}

// reconstructInterview: unknown contradiction severity -> high (blocks readiness). Without a
// string contradictionId the legacy id key is used.
func TestReconstructUnknownSeverityIsHigh(t *testing.T) {
	r := ReconstructInterview(fromJSON(t, `{"contradictions":[{"id":"c","severity":"weird","summary":"s"},{"contradictionId":5,"id":"x","severity":"medium"},{"contradictionId":"","id":"z"}]}`))
	want := []Contradiction{{"c", SeverityHigh, "s"}, {"x", SeverityMedium, ""}, {"", SeverityHigh, ""}}
	if !reflect.DeepEqual(r.Contradictions, want) {
		t.Fatalf("contradictions = %+v, want %+v", r.Contradictions, want)
	}
}

// reconstructInterview: arrays capped at MAX_TRACKER_ARRAY (T2), dropping the oldest.
func TestReconstructCapsArraysDroppingTheOldest(t *testing.T) {
	var big []any
	for i := 0; i < MaxTrackerArray+20; i++ {
		big = append(big, map[string]any{"contradictionId": fmt.Sprintf("c%d", i), "severity": "low", "summary": "s"})
	}
	c := ReconstructInterview(map[string]any{"contradictions": big}).Contradictions
	if len(c) != MaxTrackerArray || c[0].ContradictionID != "c20" || c[MaxTrackerArray-1].ContradictionID != "c69" {
		t.Fatalf("got %d entries from %v to %v, want the newest %d (c20..c69)", len(c), c[0], c[len(c)-1], MaxTrackerArray)
	}
}

// CRITICAL-1: malformed persisted contradiction/assumption entries BLOCK readiness. The
// oracle test leaves scanRounds unset, which blocks readiness by itself; here scanRounds is 1
// and a clean control is ready, so only the malformed entry can be the cause.
func TestMalformedPersistedEntriesBlockReadiness(t *testing.T) {
	build := func(contradictions, assumptions string) *Tracker {
		return ReconstructInterview(fromJSON(t, fmt.Sprintf(`{"roundId":1,"scanRounds":1,"dimensions":%s,"contradictions":%s,"assumptions":%s}`,
			dimsJSON(`{"level":"max","known":[],"unknown":[],"confidence":1}`), contradictions, assumptions)))
	}
	if !IsInterviewReady(build("[]", "[]")) {
		t.Fatal("control: a clean all-max tracker with one scan must be ready")
	}
	rc, ra := build(`["bad"]`, "[]"), build("[]", `["legacy",null]`)
	if want := []Contradiction{{Severity: SeverityHigh, Summary: "[malformed contradiction entry]"}}; !reflect.DeepEqual(rc.Contradictions, want) {
		t.Errorf("a malformed contradiction must be kept as a high blocker, got %+v", rc.Contradictions)
	}
	if want := []Assumption{{Text: "[malformed assumption entry]"}, {Text: "[malformed assumption entry]"}}; !reflect.DeepEqual(ra.Assumptions, want) {
		t.Errorf("malformed assumptions must be kept unrecorded, got %+v", ra.Assumptions)
	}
	if IsInterviewReady(rc) || IsInterviewReady(ra) {
		t.Error("malformed entries must block readiness")
	}
}

// normalizeInterview caps oversized arrays for the write path (T2), keeps roundId, applies
// the per-score fail-closed rules and leaves no nil array.
func TestNormalizeCapsOversizedArrays(t *testing.T) {
	tr := DefaultInterview(3)
	for i := 0; i < MaxTrackerArray+9; i++ {
		tr.Dimensions.Goal.Known = append(tr.Dimensions.Goal.Known, fmt.Sprintf("k%d", i))
		tr.Contradictions = append(tr.Contradictions, Contradiction{ContradictionID: fmt.Sprintf("c%d", i)})
		tr.Assumptions = append(tr.Assumptions, Assumption{ID: fmt.Sprintf("a%d", i), Recorded: true})
	}
	n := Normalize(tr)
	if len(n.Dimensions.Goal.Known) != MaxTrackerArray || len(n.Contradictions) != MaxTrackerArray || len(n.Assumptions) != MaxTrackerArray {
		t.Fatalf("lens = %d, %d, %d, want %d each", len(n.Dimensions.Goal.Known), len(n.Contradictions), len(n.Assumptions), MaxTrackerArray)
	}
	if n.RoundID != 3 || n.Dimensions.Goal.Known[0] != "k9" || n.Contradictions[0].ContradictionID != "c9" || n.Assumptions[0].ID != "a9" {
		t.Errorf("Normalize must keep roundId and drop the oldest entries")
	}
	if len(tr.Contradictions) != MaxTrackerArray+9 || Normalize(nil) != nil {
		t.Error("Normalize must not modify its argument, and nil stays nil")
	}
	bad := Normalize(&Tracker{RoundID: -2, ScanRounds: -4, Dimensions: Dimensions{Goal: DimensionScore{Level: "ZZ", Known: []string{"a"}, Confidence: 9}}})
	if bad.RoundID != 0 || bad.ScanRounds != 0 || bad.Dimensions.Goal.Level != LevelLow || bad.Dimensions.Goal.Confidence != 0 || strings.Contains(marshal(bad), "null") {
		t.Errorf("Normalize must fail closed per score and leave no null: %s", marshal(bad))
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
	for in, want := range map[any]int64{7.9: 7, -1.0: 0, negZero: 0, math.NaN(): 0, math.Inf(1): 0, json.Number("12"): 12, json.Number("1e400"): 0,
		json.Number("abc"): 0, 5: 5, int64(-5): 0, int64(math.MaxInt64): math.MaxInt64, 1e30: math.MaxInt64, "5": 0, true: 0, nil: 0} {
		if got := roundIDNum(in); got != want {
			t.Errorf("roundIDNum(%#v) = %d, want %d", in, got, want)
		}
	}
	for in, want := range map[any]float64{0.5: 0.5, 1.0: 1, negZero: 0, -0.1: 0, 1.1: 0, math.NaN(): 0, json.Number("0.25"): 0.25, 1: 1, 2: 0, "0.5": 0, nil: 0} {
		if got := confidence(in); got != want || math.Signbit(got) {
			t.Errorf("confidence(%#v) = %v, want %v", in, got, want)
		}
	}
}

// ontologySchema is absent by default and on a fresh tracker; a normalized tracker without
// one gains no key.
func TestOntologySchemaIsAbsentByDefault(t *testing.T) {
	if n := Normalize(DefaultInterview(0)); DefaultInterview(0).OntologySchema != nil || n.OntologySchema != nil || strings.Contains(marshal(n), "ontologySchema") {
		t.Fatalf("a tracker without an ontologySchema must not gain one: %s", marshal(n))
	}
}

// ontologySchema survives reconstruct + normalize round-trip, in the oracle's key order.
func TestOntologySchemaSurvivesReconstructAndNormalize(t *testing.T) {
	const schema = `[{"name":"User","fields":["id","email"],"relationships":[{"to":"Order","kind":"has-many"}]},{"name":"Order","fields":["id"],"relationships":[]}]`
	recon := ReconstructInterview(fromJSON(t, `{"roundId":1,"ontologySchema":`+schema+`}`))
	want := OntologyEntity{Name: "User", Fields: []string{"id", "email"}, Relationships: []OntologyRelationship{{To: "Order", Kind: "has-many"}}}
	if len(recon.OntologySchema) != 2 || !reflect.DeepEqual(recon.OntologySchema[0], want) {
		t.Fatalf("reconstruct = %+v", recon.OntologySchema)
	}
	if norm := Normalize(recon); !strings.Contains(marshal(norm), `"ontologySchema":`+schema) {
		t.Errorf("the schema must survive normalize in order: %s", marshal(norm))
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
// reconstruct + normalize + marshal: key order, [] arrays and number forms. It does not replay
// the fixtures, which need the scan and orchestrate commands.
func TestPersistedTrackerBytesMatchOracleFixtures(t *testing.T) {
	for _, id := range []string{"cli__scan__record_round_updates_tracker", "cli__scan__derive_from_captured_answers"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contract", "fixtures", "cxc", id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var fx struct {
			Expect struct {
				Tree map[string]struct {
					JSON struct {
						Interview json.RawMessage `json:"interview"`
					} `json:"json"`
				} `json:"tree"`
			} `json:"expect"`
		}
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatal(err)
		}
		persisted := fx.Expect.Tree["ws/.codexclaw/sessions/rec-s1.json"].JSON.Interview
		var want bytes.Buffer
		if err := json.Compact(&want, persisted); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got := marshal(Normalize(ReconstructInterview(fromJSON(t, string(persisted))))); got != want.String() {
			t.Errorf("%s:\n got %s\nwant %s", id, got, want.String())
		}
	}
}
