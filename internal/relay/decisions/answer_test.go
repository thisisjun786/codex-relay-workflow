package decisions

import (
	"errors"
	"reflect"
	"testing"
)

// CRW-737: the answer path of the record. The format (record.go) owns the field set, the state
// machine and the fingerprint; this file holds the rules an answer is judged by.

func answerRecord(state State, authority Authority) Record {
	record := Record{
		Schema:     Schema,
		DecisionID: "ud-0001",
		Kind:       KindPolicy,
		Context:    "Which window does the host update take?",
		Options:    []Option{{ID: "now", Label: "now", Effect: "at once"}, {ID: "later", Label: "later", Effect: "deferred"}},
		Origin:     Origin{Project: "PRJ-A"},
		Source:     Source{Kind: "report", Ref: "report:1"},
		Authority:  authority,
		State:      state,
		RaisedAt:   "2026-10-06T00:00:00Z",
		Seen:       []Seen{{At: "2026-10-06T00:00:00Z", Source: "project:PRJ-A"}},
	}
	record.Fingerprint = Fingerprint(record.Context, record.Blocking, record.Options)
	return record
}

// A well-formed answer moves the record to answered and carries the provenance.
func TestCRW737AnswerRecordsItsProvenance(t *testing.T) {
	for _, state := range []State{StateOpen, StateRaised} {
		record := answerRecord(state, Authority{Kind: AuthorityUser})
		answered, err := ValidateAnswer(record, Answer{Option: "now", By: "task-a", Via: ViaDirectAsk})
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		if answered.State != StateAnswered || answered.AnsweredBy != "task-a" || answered.AnsweredVia != ViaDirectAsk ||
			answered.AnswerText != "now" || answered.AnsweredAt == "" {
			t.Fatalf("%s: %+v", state, answered)
		}
		// The answer does not change the question it answers.
		if answered.Fingerprint != record.Fingerprint || answered.DecisionID != record.DecisionID || answered.Context != record.Context {
			t.Fatalf("%s: the answer changed the record's identity: %+v", state, answered)
		}
	}
}

// Every way an answer is refused, each with its named error.
func TestCRW737AnswerRefusals(t *testing.T) {
	base := func() Record { return answerRecord(StateRaised, Authority{Kind: AuthorityUser}) }
	for _, test := range []struct {
		name   string
		mutate func(*Record)
		answer Answer
		want   error
	}{
		{"no by", func(*Record) {}, Answer{Option: "now", Via: ViaDirectAsk}, ErrMissingProvenance},
		{"no via", func(*Record) {}, Answer{Option: "now", By: "task-a"}, ErrMissingProvenance},
		{"blank by", func(*Record) {}, Answer{Option: "now", By: "  ", Via: ViaDirectAsk}, ErrMissingProvenance},
		{"unknown via", func(*Record) {}, Answer{Option: "now", By: "task-a", Via: "carrier-pigeon"}, ErrUnknownVia},
		{"no option and no text", func(*Record) {}, Answer{By: "task-a", Via: ViaDirectAsk}, ErrUnknownAnswer},
		{"option not in the record", func(*Record) {}, Answer{Option: "nosuch", By: "task-a", Via: ViaDirectAsk}, ErrUnknownAnswer},
		{"unknown authority", func(*Record) {}, Answer{Option: "now", By: "task-a", Via: ViaDirectAsk, Authority: Authority{Kind: "nonsense"}}, ErrUnknownAuthority},
		{"a withdrawn record", func(r *Record) { r.State = StateWithdrawn }, Answer{Option: "now", By: "task-a", Via: ViaDirectAsk}, ErrAnswerState},
		{"an answered record", func(r *Record) { r.State = StateAnswered }, Answer{Option: "now", By: "task-a", Via: ViaDirectAsk}, ErrAnswerState},
		{"an applied record", func(r *Record) { r.State = StateApplied }, Answer{Option: "now", By: "task-a", Via: ViaDirectAsk}, ErrAnswerState},
		{"an expired record", func(r *Record) { r.State = StateExpired }, Answer{Option: "now", By: "task-a", Via: ViaDirectAsk}, ErrAnswerState},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := base()
			test.mutate(&record)
			if _, err := ValidateAnswer(record, test.answer); !errors.Is(err, test.want) {
				t.Fatalf("ValidateAnswer = %v, want %v", err, test.want)
			}
		})
	}
}

// An answer may not outrank the decision it answers: the user class is the strongest, and a
// delegated-management answer cannot settle a user-class question.
func TestCRW737AnswerMayNotOutrankTheDecision(t *testing.T) {
	for _, test := range []struct {
		decision string
		answer   string
		ref      string
		want     error
	}{
		{AuthorityUser, AuthorityUser, "", nil},
		{AuthorityUser, AuthorityDelegatedManagement, "", ErrAuthorityGrade},
		{AuthorityUser, AuthorityParent, "plan", ErrAuthorityGrade},
		{AuthorityDelegatedManagement, AuthorityUser, "", ErrAuthorityGrade},
		{AuthorityDelegatedManagement, AuthorityDelegatedManagement, "", nil},
		{AuthorityDelegatedManagement, AuthorityParent, "plan", ErrAuthorityGrade},
		{AuthorityParent, AuthorityUser, "", ErrAuthorityGrade},
		{AuthorityParent, AuthorityDelegatedManagement, "", ErrAuthorityGrade},
		{AuthorityParent, AuthorityParent, "plan", nil},
		{AuthorityParent, AuthorityParent, "", ErrUnknownAuthority},
	} {
		t.Run(test.decision+"+"+test.answer+"+"+test.ref, func(t *testing.T) {
			record := answerRecord(StateRaised, Authority{Kind: test.decision, Ref: ""})
			_, err := ValidateAnswer(record, Answer{Option: "now", By: "task-a", Via: ViaDirectAsk,
				Authority: Authority{Kind: test.answer, Ref: test.ref}})
			if test.want == nil && err != nil {
				t.Fatalf("a legal answer: %v", err)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("ValidateAnswer = %v, want %v", err, test.want)
			}
		})
	}
}

// A record that names no class states no requirement, so any class may answer it; a parent-class
// answer still needs its reference.
func TestCRW737AnswerAgainstAnEmptyAuthorityKind(t *testing.T) {
	record := answerRecord(StateRaised, Authority{})
	for _, kind := range []string{AuthorityUser, AuthorityDelegatedManagement} {
		if _, err := ValidateAnswer(record, Answer{Option: "now", By: "task-a", Via: ViaDirectAsk, Authority: Authority{Kind: kind}}); err != nil {
			t.Fatalf("a %s answer to a record with no authority kind: %v", kind, err)
		}
	}
	if _, err := ValidateAnswer(record, Answer{Option: "now", By: "task-a", Via: ViaDirectAsk, Authority: Authority{Kind: AuthorityParent, Ref: "plan"}}); err != nil {
		t.Fatalf("a parent-class answer to a record with no authority kind: %v", err)
	}
	if _, err := ValidateAnswer(record, Answer{Option: "now", By: "task-a", Via: ViaDirectAsk, Authority: Authority{Kind: AuthorityParent}}); !errors.Is(err, ErrUnknownAuthority) {
		t.Fatalf("a parent-class answer with no ref: %v", err)
	}
}

// A free-text answer is stored as its text, and an option answer as the option id.
func TestCRW737AnswerTextAndOption(t *testing.T) {
	record := answerRecord(StateRaised, Authority{Kind: AuthorityUser})
	answered, err := ValidateAnswer(record, Answer{Text: "between runs", By: "task-a", Via: ViaFileImport})
	if err != nil || answered.AnswerText != "between runs" {
		t.Fatalf("free text: %v %+v", err, answered)
	}
	answered, err = ValidateAnswer(record, Answer{Option: "now", Text: "because it is idle", By: "task-a", Via: ViaDots})
	if err != nil || answered.AnswerText != "because it is idle" {
		t.Fatalf("option and text: %v %+v", err, answered)
	}
}

// Withdraw moves a record to withdrawn with its reason and refuses a second withdrawal; apply
// moves an answered record to applied and refuses an un-answered one.
func TestCRW737WithdrawAndApplyTransitions(t *testing.T) {
	record := answerRecord(StateRaised, Authority{Kind: AuthorityUser})
	withdrawn, err := Withdraw(record, "the plan moved on")
	if err != nil || withdrawn.State != StateWithdrawn || withdrawn.WithdrawnReason != "the plan moved on" {
		t.Fatalf("withdraw: %v %+v", err, withdrawn)
	}
	if _, err := Withdraw(withdrawn, "again"); !errors.Is(err, ErrTransition) {
		t.Fatalf("a second withdrawal = %v, want ErrTransition", err)
	}
	if _, err := Apply(record, "ev-1", 1); !errors.Is(err, ErrTransition) {
		t.Fatalf("applying a raised record = %v, want ErrTransition", err)
	}
	answered, err := ValidateAnswer(record, Answer{Option: "now", By: "task-a", Via: ViaDirectAsk})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := Apply(answered, "ev-1", 1)
	if err != nil || applied.State != StateApplied || applied.AppliedEvent != "ev-1" || applied.AppliedAt == "" {
		t.Fatalf("apply: %v %+v", err, applied)
	}
	if _, err := Apply(applied, "ev-2", 2); !errors.Is(err, ErrTransition) {
		t.Fatalf("a second apply = %v, want ErrTransition", err)
	}
}

// The via vocabulary is the one the decision document fixes.
func TestCRW737ViaVocabulary(t *testing.T) {
	want := []string{"file-import", "management-message", "supervisor-readback", "dots", "direct-ask"}
	if got := Vias(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Vias() = %v", got)
	}
}

// An answer that cites no class takes the record's own class; a parent-class record still needs
// the reference its answer used, so an answer that names none is refused there.
func TestCRW737AnswerWithoutACitedClassTakesTheRecordsClass(t *testing.T) {
	for _, required := range []string{AuthorityUser, AuthorityDelegatedManagement} {
		record := answerRecord(StateRaised, Authority{Kind: required})
		answered, err := ValidateAnswer(record, Answer{Option: "now", By: "task-a", Via: ViaDirectAsk})
		if err != nil {
			t.Fatalf("a %s-class record answered without a cited class: %v", required, err)
		}
		if answered.State != StateAnswered || answered.AnsweredBy != "task-a" {
			t.Fatalf("answered %+v", answered)
		}
	}
	record := answerRecord(StateRaised, Authority{Kind: AuthorityParent})
	if _, err := ValidateAnswer(record, Answer{Option: "now", By: "task-a", Via: ViaDirectAsk}); !errors.Is(err, ErrUnknownAuthority) {
		t.Fatalf("a parent-class record answered without a cited class: %v", err)
	}
}
