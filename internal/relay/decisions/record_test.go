package decisions

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The record format, vocabularies, fingerprint and merge, fixed by test (CRW-716 gen 2).

func TestC1SchemaAndVocabularies(t *testing.T) {
	if Schema != "crw-user-decision/1" {
		t.Fatalf("Schema = %q", Schema)
	}
	if got, want := Kinds(), []Kind{KindDesignChoice, KindDependency, KindMergeApproval, KindCleanupApproval, KindBlockedEscalation, KindPolicy}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Kinds() = %v", got)
	}
	if got, want := States(), []State{StateOpen, StateRaised, StateAnswered, StateApplied, StateWithdrawn, StateExpired}; !reflect.DeepEqual(got, want) {
		t.Fatalf("States() = %v", got)
	}
	if KindDesignChoice != "design_choice" || StateApplied != "applied" || AuthorityUser != "user" || BlockingIssue != "issue" {
		t.Fatal("a vocabulary spelling changed")
	}
}

// Every crw-user-decision/1 field has a home: a full record decodes, validates and round trips.
func TestC1RecordRoundTrips(t *testing.T) {
	const document = `{"schema":"crw-user-decision/1","decision_id":"ud-0001","fingerprint":"0123456789abcdef",
"kind":"design_choice","context":"A late receipt whose lineage was broken: reject it or accept it.",
"options":[{"id":"reject","label":"reject at emit","effect":"refused"},{"id":"accept","label":"accept and record","effect":"stored with a note"}],
"recommendation":{"option":"reject","one_line":"a receipt that cannot be placed should not become the head"},
"blocking":[{"kind":"issue","ref":"CRW-1"}],"needed_by":"2026-10-10T00:00:00Z","origin":{"issue":"CRW-716","project":"PRJ-A"},
"source":{"kind":"status_file","ref":"x.md:132"},"authority":{"kind":"user","ref":""},"state":"open",
"raised_at":"2026-10-06T00:00:00Z","raised_via":"direct-ask","seen":[{"at":"2026-10-06T00:00:00Z","source":"status_file:132"}],
"answered_at":"","answered_by":"","answered_via":"","answer_text":"","applied_at":"","applied_event":"",
"applied_generation":0,"withdrawn_reason":"","expired_reason":""}`
	var record Record
	if err := json.Unmarshal([]byte(document), &record); err != nil {
		t.Fatal(err)
	}
	if err := Validate(record); err != nil {
		t.Fatalf("a full record must validate: %v", err)
	}
	if record.Kind != KindDesignChoice || record.State != StateOpen || len(record.Options) != 2 ||
		record.Recommendation == nil || record.Recommendation.Option != "reject" || record.Origin.Project != "PRJ-A" ||
		record.Source.Kind != "status_file" || record.Blocking[0].Ref != "CRW-1" || len(record.Seen) != 1 {
		t.Fatalf("fields did not decode: %+v", record)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var again Record
	if err := json.Unmarshal(encoded, &again); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(record, again) {
		t.Fatal("a record does not survive a JSON round trip")
	}
}

func TestC1AllowedTransitions(t *testing.T) {
	want := map[State][]State{
		StateOpen:      {StateRaised, StateWithdrawn, StateExpired},
		StateRaised:    {StateAnswered, StateWithdrawn, StateExpired},
		StateAnswered:  {StateApplied, StateWithdrawn, StateExpired},
		StateApplied:   nil,
		StateWithdrawn: nil,
		StateExpired:   nil,
	}
	if got := AllowedTransitions(); !reflect.DeepEqual(got, want) {
		t.Fatalf("AllowedTransitions() = %v", got)
	}
	for _, from := range States() {
		for _, to := range States() {
			allowed := false
			for _, candidate := range want[from] {
				allowed = allowed || candidate == to
			}
			if CanTransition(from, to) != allowed {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, CanTransition(from, to), allowed)
			}
		}
	}
}

// The four refusals C2 names, plus the rest of the format's checks, each with its named error.
func TestC2ValidationRefusals(t *testing.T) {
	valid := func() Record {
		return Record{Schema: Schema, Kind: KindPolicy, Context: "a question", State: StateOpen,
			Options: []Option{{ID: "a"}, {ID: "b"}}, Origin: Origin{Project: "PRJ-A"}, Authority: Authority{Kind: AuthorityUser}}
	}
	for _, test := range []struct {
		name   string
		mutate func(*Record)
		want   error
	}{
		{"empty context", func(r *Record) { r.Context = "" }, ErrEmptyContext},
		{"blank context", func(r *Record) { r.Context = "   " }, ErrEmptyContext},
		{"one option", func(r *Record) { r.Options = r.Options[:1] }, ErrOptionCount},
		{"four options", func(r *Record) { r.Options = append(r.Options, Option{ID: "c"}, Option{ID: "d"}) }, ErrOptionCount},
		{"unknown kind", func(r *Record) { r.Kind = "nonsense" }, ErrUnknownKind},
		{"unknown state", func(r *Record) { r.State = "nonsense" }, ErrUnknownState},
		{"wrong schema", func(r *Record) { r.Schema = "crw-user-decision/2" }, ErrSchema},
		{"empty option id", func(r *Record) { r.Options[0].ID = " " }, ErrEmptyOptionID},
		{"duplicate option id", func(r *Record) { r.Options[1].ID = "a" }, ErrDuplicateOptionID},
		{"unknown blocking kind", func(r *Record) { r.Blocking = []Blocking{{Kind: "nonsense", Ref: "x"}} }, ErrUnknownBlockingKind},
		{"unknown authority", func(r *Record) { r.Authority.Kind = "nonsense" }, ErrUnknownAuthority},
		{"bad needed_by", func(r *Record) { r.NeededBy = "tomorrow" }, ErrBadNeededBy},
		{"recommendation off the option set", func(r *Record) { r.Recommendation = &Recommendation{Option: "z"} }, ErrRecommendation},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := valid()
			test.mutate(&record)
			if err := Validate(record); !errors.Is(err, test.want) {
				t.Fatalf("Validate = %v, want %v", err, test.want)
			}
		})
	}
	if err := Validate(valid()); err != nil {
		t.Fatalf("the control record must validate: %v", err)
	}
}

// A legal move happens; an illegal one is a named refusal that leaves the record where it was.
func TestC2DisallowedTransitionIsNamed(t *testing.T) {
	record := Record{Schema: Schema, Kind: KindPolicy, Context: "c", State: StateOpen, Options: []Option{{ID: "a"}, {ID: "b"}}}
	for _, step := range []State{StateRaised, StateAnswered, StateApplied} {
		if err := Transition(&record, step); err != nil || record.State != step {
			t.Fatalf("-> %s: %v %s", step, err, record.State)
		}
	}
	if err := Transition(&record, StateOpen); !errors.Is(err, ErrTransition) || record.State != StateApplied {
		t.Fatalf("applied -> open = %v (state %s)", err, record.State)
	}
}

func TestC3Normalize(t *testing.T) {
	for _, test := range []struct{ in, want string }{
		{"  Schedule   THE  host  window. ", "schedule the host window."},
		{"a\tb\nc", "a b c"},
		{"", ""},
		{"   ", ""},
	} {
		if got := Normalize(test.in); got != test.want {
			t.Errorf("Normalize(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

// The testdata vector is the fixed input/output pair a second implementation is compared against.
func TestC3FingerprintVector(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "fingerprint_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Vectors []struct {
			Name        string     `json:"name"`
			Context     string     `json:"context"`
			Blocking    []Blocking `json:"blocking"`
			OptionIDs   []string   `json:"optionIds"`
			Fingerprint string     `json:"fingerprint"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Vectors) < 10 {
		t.Fatalf("the vector is too small: %d", len(document.Vectors))
	}
	for _, vector := range document.Vectors {
		options := make([]Option, len(vector.OptionIDs))
		for i, id := range vector.OptionIDs {
			options[i] = Option{ID: id}
		}
		if got := Fingerprint(vector.Context, vector.Blocking, options); got != vector.Fingerprint || len(got) != 16 {
			t.Errorf("%s: Fingerprint = %s (%d chars), want %s", vector.Name, got, len(got), vector.Fingerprint)
		}
	}
}

// Order and case must not change the fingerprint; a different question must.
func TestC3FingerprintInsensitiveToOrderAndCase(t *testing.T) {
	a := Fingerprint("Adopt the gate.", []Blocking{{Kind: "Issue", Ref: "CRW-145"}}, []Option{{ID: "Adopt"}, {ID: "defer"}})
	b := Fingerprint("adopt the gate.", []Blocking{{Kind: "issue", Ref: "crw-145"}}, []Option{{ID: "defer"}, {ID: "adopt"}})
	if a != b {
		t.Fatalf("order/case changed the fingerprint: %s != %s", a, b)
	}
	if c := Fingerprint("Adopt the gate!", nil, []Option{{ID: "adopt"}, {ID: "defer"}}); a == c {
		t.Fatal("a different context and blocking produced the same fingerprint")
	}
}

func TestC4MergeSameFingerprint(t *testing.T) {
	first := Record{Schema: Schema, Kind: KindPolicy, Context: "a question", State: StateOpen,
		Options: []Option{{ID: "a"}, {ID: "b"}}, Seen: []Seen{{At: "2026-10-06T00:00:00Z", Source: "report:1"}}}
	first.Fingerprint = Fingerprint(first.Context, first.Blocking, first.Options)
	second := first
	second.Seen = []Seen{{At: "2026-10-06T01:00:00Z", Source: "report:2"}}

	merged, err := Merge(first, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Seen) != 2 || merged.Seen[0].Source != "report:1" || merged.Seen[1].Source != "report:2" {
		t.Fatalf("merged seen = %+v, want two entries in order", merged.Seen)
	}
	if merged.Fingerprint != first.Fingerprint {
		t.Fatal("merge changed the fingerprint")
	}
}

func TestC4MergeDifferentFingerprintIsRefused(t *testing.T) {
	first := Record{Schema: Schema, Kind: KindPolicy, Context: "one", State: StateOpen, Options: []Option{{ID: "a"}, {ID: "b"}}}
	first.Fingerprint = Fingerprint(first.Context, first.Blocking, first.Options)
	other := first
	other.Context = "two"
	other.Fingerprint = Fingerprint(other.Context, other.Blocking, other.Options)

	if _, err := Merge(first, other); !errors.Is(err, ErrFingerprintMismatch) {
		t.Fatalf("merge of two different questions = %v", err)
	}
	// A stale fingerprint field must not let a different question merge.
	stale := other
	stale.Fingerprint = first.Fingerprint
	if _, err := Merge(first, stale); !errors.Is(err, ErrFingerprintMismatch) {
		t.Fatalf("merge of a stale fingerprint = %v", err)
	}
}

// Stamp fills the schema id, the open state and the fingerprint, and refuses a bad record.
func TestC1StampFillsIdentity(t *testing.T) {
	stamped, err := Stamp(Record{Kind: KindPolicy, Context: "a question", Options: []Option{{ID: "a"}, {ID: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	if stamped.Schema != Schema || stamped.State != StateOpen {
		t.Fatalf("stamped = %+v", stamped)
	}
	if want := Fingerprint("a question", nil, []Option{{ID: "a"}, {ID: "b"}}); stamped.Fingerprint != want {
		t.Fatalf("fingerprint = %s, want %s", stamped.Fingerprint, want)
	}
	if _, err := Stamp(Record{Kind: "nonsense"}); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("Stamp of a bad record = %v", err)
	}
}
