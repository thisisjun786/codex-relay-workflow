package decisions

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// The answer path of the crw-user-decision/1 record: who answered, how the answer arrived, and
// what it may cite. The record format and its state machine live in record.go; this file holds the
// rules an answer is judged by, so the record's own vocabulary is not restated in a command.

// Vias is the answered_via vocabulary, in declaration order. The record keeps the value as text;
// a second answer path (the pump, the supervisor readback, an imported file) names its own.
func Vias() []string {
	return []string{ViaFileImport, ViaManagementMessage, ViaSupervisorReadback, ViaDots, ViaDirectAsk}
}

// The answered_via values.
const (
	ViaFileImport         = "file-import"
	ViaManagementMessage  = "management-message"
	ViaSupervisorReadback = "supervisor-readback"
	ViaDots               = "dots"
	ViaDirectAsk          = "direct-ask"
)

// The refusals an answer makes beyond the format's. Use errors.Is.
var (
	ErrMissingProvenance = errors.New("decisions: an answer names who answered it and how it arrived")
	ErrUnknownVia        = errors.New("decisions: unknown answered_via")
	ErrUnknownAnswer     = errors.New("decisions: the answer names no option of the record")
	ErrAuthorityGrade    = errors.New("decisions: the answer's authority is above the decision's")
	ErrAnswerState       = errors.New("decisions: the record is not in a state that takes an answer")
	ErrAnswerNeedsOption = errors.New("decisions: a relationship-blocking decision is answered with an option")
)

// authorityRank orders the answer classes user > delegated-management > parent. The record's
// authority.kind is the class its answer must cite, and an answer citing any other class is
// refused: a user-grade question (the class the decision document reserves for deletion, a new
// mandatory requirement, Done judgement, a host restart, hook trust, a global instruction and the
// acceptance of a security or data-loss finding) takes only a user-grade answer, and neither a
// weaker answer nor one reaching past the class the question named is accepted. The parent class
// additionally requires its ref.
var authorityRank = map[string]int{AuthorityUser: 3, AuthorityDelegatedManagement: 2, AuthorityParent: 1}

// AnswerAuthorityRank is the rank of an authority class, and whether it is one of the vocabulary.
func AnswerAuthorityRank(kind string) (int, bool) {
	rank, ok := authorityRank[kind]
	return rank, ok
}

// Answer is one answer to a record: the option or free text it chooses, and its provenance.
type Answer struct {
	Option string
	Text   string
	By     string
	Via    string
	// Authority is the class the answer cites. Its kind may be empty, which is the record's own
	// class when the record states no requirement; a non-empty kind must be one of the vocabulary
	// and must be the record's own class. A record that names a class takes an answer of that
	// class: an answer path that requires the class to be stated (decision-answer, for the user and
	// delegated-management grades) refuses an answer that leaves it empty.
	Authority Authority
}

// Answerable reports whether record is in a state an answer may move to answered: raised, or open
// (a question created but not yet shown, which the answer raises on its way to answered).
func Answerable(record Record) bool {
	return record.State == StateOpen || record.State == StateRaised
}

// answerTransition moves a record to answered through the state machine: an open question is
// raised and then answered, which is the only path the machine allows.
func answerTransition(record Record) (Record, error) {
	if record.State == StateOpen {
		if err := Transition(&record, StateRaised); err != nil {
			return Record{}, err
		}
	}
	if err := Transition(&record, StateAnswered); err != nil {
		return Record{}, err
	}
	return record, nil
}

// ValidateAnswer judges an answer against the record it answers: the provenance must be present,
// the via must be one of the vocabulary, the answer must name an option of the record or carry
// free text, and the class it cites must be the record's own class. A record that is not open or
// raised (an answered, applied, withdrawn or expired one) takes no answer. The returned record is
// the answered one; the caller stores it.
func ValidateAnswer(record Record, answer Answer) (Record, error) {
	if strings.TrimSpace(answer.By) == "" || strings.TrimSpace(answer.Via) == "" {
		return Record{}, ErrMissingProvenance
	}
	if !contains(Vias(), answer.Via) {
		return Record{}, fmt.Errorf("%w: %q", ErrUnknownVia, answer.Via)
	}
	if !Answerable(record) {
		return Record{}, fmt.Errorf("%w: %s", ErrAnswerState, record.State)
	}
	option, text := strings.TrimSpace(answer.Option), strings.TrimSpace(answer.Text)
	if option == "" && text == "" {
		return Record{}, ErrUnknownAnswer
	}
	ids := make(map[string]bool, len(record.Options))
	for _, candidate := range record.Options {
		ids[strings.TrimSpace(candidate.ID)] = true
	}
	if option != "" && !ids[option] {
		return Record{}, fmt.Errorf("%w: %q", ErrUnknownAnswer, option)
	}
	// A decision that blocks a relationship is applied by the reply its chosen option makes, so
	// free text alone states no choice: the option is what names the reply the relay must match.
	if decisionBlocksRelationship(record) && option == "" {
		return Record{}, fmt.Errorf("%w: %q is a relationship-blocking decision", ErrAnswerNeedsOption, record.DecisionID)
	}
	required := strings.TrimSpace(record.Authority.Kind)
	kind := strings.TrimSpace(answer.Authority.Kind)
	if kind == "" {
		// No class was cited: the answer takes the record's own class. The parent class is the
		// exception, because an answer of it must name the reference it used.
		if required == AuthorityParent {
			return Record{}, fmt.Errorf("%w: a parent-class answer names its authority ref", ErrUnknownAuthority)
		}
	} else {
		if !IsAuthorityKind(kind) {
			return Record{}, fmt.Errorf("%w: %q", ErrUnknownAuthority, kind)
		}
		// A record that names no class (one a caller built without one; decision-raise always
		// names one) states no requirement, so any class may answer it. A record that names one
		// takes exactly that class: an answer of another class is refused, whichever side of it
		// the answer stands on.
		if required != "" && kind != required {
			return Record{}, fmt.Errorf("%w: %s answers a %s-class decision, which takes a %s answer", ErrAuthorityGrade, kind, required, required)
		}
		if kind == AuthorityParent && strings.TrimSpace(answer.Authority.Ref) == "" {
			return Record{}, fmt.Errorf("%w: a parent-class answer names its authority ref", ErrUnknownAuthority)
		}
		// The ref the answer cites is the ref the record holds: an answer that cites another ref
		// answers a different authority than the one the question was raised under.
		if held := strings.TrimSpace(record.Authority.Ref); held != "" && strings.TrimSpace(answer.Authority.Ref) != held {
			return Record{}, fmt.Errorf("%w: the answer cites ref %q and the record holds %q", ErrAuthorityGrade, answer.Authority.Ref, record.Authority.Ref)
		}
	}
	answered, err := answerTransition(record)
	if err != nil {
		return Record{}, err
	}
	answered.AnsweredAt = time.Now().UTC().Format(time.RFC3339Nano)
	answered.AnsweredBy = answer.By
	answered.AnsweredVia = answer.Via
	answered.AnswerText = text
	if option != "" && text == "" {
		answered.AnswerText = option
	}
	// The answer cites a class; it never rewrites the authority of the question it answers. The
	// stored record keeps the class and the ref it was raised with, so what a question required is
	// read back unchanged whatever an answer cited.
	return answered, nil
}

// Withdraw moves a record to withdrawn with its reason, or refuses the transition.
func Withdraw(record Record, reason string) (Record, error) {
	withdrawn := record
	if err := Transition(&withdrawn, StateWithdrawn); err != nil {
		return Record{}, err
	}
	withdrawn.WithdrawnReason = reason
	return withdrawn, nil
}

// Apply moves an answered record to applied, naming the event that unblocked it.
// Apply moves an answered record to applied, naming the event that unblocked it and the execution
// generation that event belongs to. The generation is read from the event itself by the caller,
// so what the record holds is the generation the reply was made in rather than a default.
func Apply(record Record, event string, generation int64) (Record, error) {
	applied := record
	if err := Transition(&applied, StateApplied); err != nil {
		return Record{}, err
	}
	applied.AppliedAt = time.Now().UTC().Format(time.RFC3339Nano)
	applied.AppliedEvent = event
	applied.AppliedGeneration = generation
	return applied, nil
}
