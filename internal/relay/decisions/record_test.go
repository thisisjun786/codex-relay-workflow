package decisions

import (
	"crypto/sha256"
	"encoding/hex"
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
			if wantAllowed := contains(want[from], to); CanTransition(from, to) != wantAllowed {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, CanTransition(from, to), wantAllowed)
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
		{"comma fractional needed_by", func(r *Record) { r.NeededBy = "2026-10-10T00:00:00,5Z" }, ErrBadNeededBy},
		{"recommendation off the option set", func(r *Record) { r.Recommendation = &Recommendation{Option: "z"} }, ErrRecommendation},
		{"context with a separator", func(r *Record) { r.Context = "a | b" }, ErrAmbiguousField},
		{"option id with a comma", func(r *Record) { r.Options[0].ID = "a,b" }, ErrAmbiguousField},
		{"blocking ref with a colon", func(r *Record) { r.Blocking = []Blocking{{Kind: "issue", Ref: "x:y"}} }, ErrAmbiguousField},
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
		{"MiXeD Case", "mixed case"},
		{"\u0130\u03a3 stays", "\u0130\u03a3 stays"},
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
	if len(document.Vectors) < 13 {
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

// A later answer must not be dropped: merging records that differ beyond seen is refused.
func TestC4MergeRefusesDifferingAnswer(t *testing.T) {
	open := Record{Schema: Schema, Kind: KindPolicy, Context: "a question", State: StateOpen, Options: []Option{{ID: "a"}, {ID: "b"}}}
	open.Fingerprint = Fingerprint(open.Context, open.Blocking, open.Options)
	answered := open
	answered.State = StateAnswered
	answered.AnsweredAt, answered.AnsweredBy, answered.AnswerText = "2026-10-06T02:00:00Z", "task-a", "adopt"

	if _, err := Merge(open, answered); !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("merging a later answer = %v", err)
	}
	if _, err := Merge(answered, open); !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("merging an earlier open observation over an answer = %v", err)
	}
}

// CRW-791: the findings this issue fixes, written red against the CRW-716 code first.

// Two statements of different questions must not share a fingerprint: the JSON encoding keeps
// the fields apart even where the old "|"- and ","-joined material ran them together.
func TestC1FingerprintDoesNotDependOnDelimiters(t *testing.T) {
	a := Record{Schema: Schema, Kind: KindPolicy, Context: "c", State: StateOpen,
		Blocking:  []Blocking{{Kind: "issue", Ref: "x"}},
		Options:   []Option{{ID: "a"}, {ID: "issue:y|z"}, {ID: "zz"}},
		Authority: Authority{Kind: AuthorityUser}, Origin: Origin{Project: "PRJ-A"}}
	b := Record{Schema: Schema, Kind: KindPolicy, Context: "c", State: StateOpen,
		Blocking:  []Blocking{{Kind: "issue", Ref: "x|a"}, {Kind: "issue", Ref: "y"}},
		Options:   []Option{{ID: "z"}, {ID: "zz"}},
		Authority: Authority{Kind: AuthorityUser}, Origin: Origin{Project: "PRJ-A"}}
	for _, record := range []Record{a, b} {
		if err := Validate(record); err != nil {
			t.Fatalf("the vector must validate: %v", err)
		}
	}
	fa := Fingerprint(a.Context, a.Blocking, a.Options)
	fb := Fingerprint(b.Context, b.Blocking, b.Options)
	if fa == fb {
		t.Fatalf("two different questions share the fingerprint %s", fa)
	}
}

// A fractional-second RFC3339 needed_by is accepted and kept as written; the comma separator
// Go's parser also accepts is still refused.
func TestC2NeededByAcceptsFractionalSeconds(t *testing.T) {
	record := Record{Schema: Schema, Kind: KindPolicy, Context: "a question", State: StateOpen,
		Options: []Option{{ID: "a"}, {ID: "b"}}, Authority: Authority{Kind: AuthorityUser},
		NeededBy: "2026-10-10T00:00:00.5Z"}
	if err := Validate(record); err != nil {
		t.Fatalf("a fractional-second needed_by must validate: %v", err)
	}
	if record.NeededBy != "2026-10-10T00:00:00.5Z" {
		t.Fatalf("needed_by was rewritten to %q", record.NeededBy)
	}
	record.NeededBy = "2026-10-10T00:00:00,5Z"
	if err := Validate(record); !errors.Is(err, ErrBadNeededBy) {
		t.Fatalf("a comma fractional separator = %v, want ErrBadNeededBy", err)
	}
}

// The two records below differ in their answer fields but produce the same NUL-joined material,
// so the old comparison folded them; comparing field by field must refuse the merge.
func TestC3MergeComparesAnswerFieldsOneByOne(t *testing.T) {
	statement := func(via, text string) Record {
		record := Record{Schema: Schema, Kind: KindPolicy, Context: "c", State: StateOpen,
			Options: []Option{{ID: "a"}, {ID: "b"}}, Authority: Authority{Kind: AuthorityUser},
			AnsweredVia: via, AnswerText: text}
		record.Fingerprint = Fingerprint(record.Context, record.Blocking, record.Options)
		return record
	}
	a, b := statement("chat", "yes"+"\x00"+"no"), statement("chat"+"\x00"+"yes", "no")
	if _, err := Merge(a, b); !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("Merge = %v, want ErrMergeConflict", err)
	}
}

// Merge folds a second statement that differs only in fields the fingerprint does not cover:
// the stored statement is kept and the second record's seen entries are appended.
func TestC4MergeKeepsTheStoredStatement(t *testing.T) {
	first := Record{Schema: Schema, Kind: KindPolicy, Context: "a question", State: StateOpen,
		Options: []Option{{ID: "a"}, {ID: "b"}}, Authority: Authority{Kind: AuthorityUser},
		Recommendation: &Recommendation{Option: "a", OneLine: "keep the first"},
		Seen:           []Seen{{At: "2026-10-06T00:00:00Z", Source: "report:1"}}}
	first.Fingerprint = Fingerprint(first.Context, first.Blocking, first.Options)
	second := first
	second.Kind = KindDesignChoice
	second.Recommendation = &Recommendation{Option: "b", OneLine: "the second differs"}
	second.Seen = []Seen{{At: "2026-10-06T01:00:00Z", Source: "report:2"}}
	merged, err := Merge(first, second)
	if err != nil {
		t.Fatalf("a second statement that differs beyond seen must fold: %v", err)
	}
	if merged.Kind != KindPolicy || merged.Recommendation == nil || merged.Recommendation.OneLine != "keep the first" {
		t.Fatalf("merge did not keep the stored statement: %+v", merged)
	}
	if len(merged.Seen) != 2 {
		t.Fatalf("merged seen = %+v", merged.Seen)
	}
}

// A control character in any free-text field is refused with its own name; the context alone
// keeps its newline and tab, because a question is a paragraph.
func TestC3ValidateRefusesControlCharacters(t *testing.T) {
	valid := func() Record {
		return Record{Schema: Schema, Kind: KindPolicy, Context: "a question", State: StateOpen,
			DecisionID:     "ud-0001",
			Options:        []Option{{ID: "a", Label: "adopt", Effect: "stored"}, {ID: "b"}},
			Blocking:       []Blocking{{Kind: "issue", Ref: "CRW-1"}},
			Origin:         Origin{Issue: "CRW-791", Project: "PRJ-A"},
			Source:         Source{Kind: "report", Ref: "x.md:1"},
			Authority:      Authority{Kind: AuthorityUser, Ref: "jun"},
			Seen:           []Seen{{At: "2026-10-06T00:00:00Z", Source: "report:1", Note: "first"}},
			Recommendation: &Recommendation{Option: "a", OneLine: "adopt"},
			RaisedAt:       "2026-10-06T00:00:00Z", RaisedVia: "direct-ask",
			AnsweredAt: "2026-10-06T01:00:00Z", AnsweredBy: "jun", AnsweredVia: "chat",
			AnswerText: "adopt", AppliedAt: "2026-10-06T02:00:00Z", AppliedEvent: "ev-1",
			WithdrawnReason: "superseded", ExpiredReason: "passed"}
	}
	const bad = "a\x00b"
	for _, test := range []struct {
		name   string
		mutate func(*Record)
	}{
		{"context", func(r *Record) { r.Context = bad }},
		{"decision_id", func(r *Record) { r.DecisionID = bad }},
		{"option id", func(r *Record) { r.Options[0].ID = bad }},
		{"option label", func(r *Record) { r.Options[0].Label = bad }},
		{"option effect", func(r *Record) { r.Options[0].Effect = bad }},
		{"recommendation option", func(r *Record) { r.Recommendation.Option = bad }},
		{"recommendation one_line", func(r *Record) { r.Recommendation.OneLine = bad }},
		{"blocking kind", func(r *Record) { r.Blocking[0].Kind = bad }},
		{"blocking ref", func(r *Record) { r.Blocking[0].Ref = bad }},
		{"origin issue", func(r *Record) { r.Origin.Issue = bad }},
		{"origin project", func(r *Record) { r.Origin.Project = bad }},
		{"source kind", func(r *Record) { r.Source.Kind = bad }},
		{"source ref", func(r *Record) { r.Source.Ref = bad }},
		{"authority ref", func(r *Record) { r.Authority.Ref = bad }},
		{"seen source", func(r *Record) { r.Seen[0].Source = bad }},
		{"seen note", func(r *Record) { r.Seen[0].Note = bad }},
		{"raised_at", func(r *Record) { r.RaisedAt = bad }},
		{"raised_via", func(r *Record) { r.RaisedVia = bad }},
		{"answered_at", func(r *Record) { r.AnsweredAt = bad }},
		{"answered_by", func(r *Record) { r.AnsweredBy = bad }},
		{"answered_via", func(r *Record) { r.AnsweredVia = bad }},
		{"answer_text", func(r *Record) { r.AnswerText = bad }},
		{"applied_at", func(r *Record) { r.AppliedAt = bad }},
		{"applied_event", func(r *Record) { r.AppliedEvent = bad }},
		{"withdrawn_reason", func(r *Record) { r.WithdrawnReason = bad }},
		{"expired_reason", func(r *Record) { r.ExpiredReason = bad }},
		{"DEL", func(r *Record) { r.Context = "a\x7fb" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := valid()
			test.mutate(&record)
			if err := Validate(record); !errors.Is(err, ErrControlCharacter) {
				t.Fatalf("Validate = %v, want ErrControlCharacter", err)
			}
		})
	}
	record := valid()
	if err := Validate(record); err != nil {
		t.Fatalf("the control record must validate: %v", err)
	}
	record.Context = "first line\nsecond\tcolumn"
	if err := Validate(record); err != nil {
		t.Fatalf("a newline or tab in the context must validate: %v", err)
	}
}

// The material is the exact JSON the encoding rule names: an encoder with different escaping or
// spacing would fingerprint the same question differently, so the bytes are pinned here and the
// same rule is stated in testdata/fingerprint_vectors.json.
func TestC1FingerprintMaterialIsThePinnedJSONEncoding(t *testing.T) {
	material, err := json.Marshal([]any{
		"r&d <window>",
		[][2]string{{"issue", "x"}},
		[]string{"later", "now"},
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `["r\u0026d \u003cwindow\u003e",[["issue","x"]],["later","now"]]`
	if string(material) != want {
		t.Fatalf("the encoding rule changed: material = %s", material)
	}
	sum := sha256.Sum256(material)
	if got := hex.EncodeToString(sum[:])[:16]; got != "42ec25d4de714b9a" {
		t.Fatalf("the vector is not this material's fingerprint: %s", got)
	}
	if got := Fingerprint("R&D <window>", []Blocking{{Kind: "issue", Ref: "x"}}, []Option{{ID: "now"}, {ID: "later"}}); got != "42ec25d4de714b9a" {
		t.Fatalf("Fingerprint = %s", got)
	}
}

// CRW-737: raised_at is a timestamp the format checks, as needed_by is.

// A fractional-second RFC3339 raised_at is accepted and kept as written; a comma fraction, a
// date without a time and free text are refused with ErrBadRaisedAt.
func TestCRW737RaisedAtIsAnRFC3339Timestamp(t *testing.T) {
	valid := func() Record {
		return Record{Schema: Schema, Kind: KindPolicy, Context: "a question", State: StateOpen,
			Options: []Option{{ID: "a"}, {ID: "b"}}, Authority: Authority{Kind: AuthorityUser}}
	}
	record := valid()
	record.RaisedAt = "2026-10-06T00:00:00.5Z"
	if err := Validate(record); err != nil {
		t.Fatalf("a fractional-second raised_at must validate: %v", err)
	}
	if record.RaisedAt != "2026-10-06T00:00:00.5Z" {
		t.Fatalf("raised_at was rewritten to %q", record.RaisedAt)
	}
	record = valid()
	record.RaisedAt = "2026-10-06T09:00:00+09:00"
	if err := Validate(record); err != nil {
		t.Fatalf("an offset raised_at must validate: %v", err)
	}
	for _, at := range []string{"not a time", "2026-10-06T00:00:00,5Z", "2026-10-06", "2026-10-06T00:00:00"} {
		record = valid()
		record.RaisedAt = at
		if err := Validate(record); !errors.Is(err, ErrBadRaisedAt) {
			t.Errorf("Validate with raised_at %q = %v, want ErrBadRaisedAt", at, err)
		}
	}
	// An absent raised_at is legal: a record a caller builds before its raise has no stamp yet.
	record = valid()
	if err := Validate(record); err != nil {
		t.Fatalf("an absent raised_at must validate: %v", err)
	}
}
