// Package decisions holds the relay's user-decision record: the crw-user-decision/1 field set,
// the kind and state vocabularies with their allowed transitions, validation, the fingerprint
// and the in-memory merge of two records that carry the same fingerprint.
//
// It holds no store and no command; the table that persists a record and the
// decision-raise/decision-list commands are other issues that build on this format. The
// fingerprint's fixed input/output pairs live in testdata/fingerprint_vectors.json.
package decisions

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Schema is the record format's id.
const Schema = "crw-user-decision/1"

// Kind is what a question is about.
type Kind string

// The kind vocabulary.
const (
	KindDesignChoice      Kind = "design_choice"
	KindDependency        Kind = "dependency"
	KindMergeApproval     Kind = "merge_approval"
	KindCleanupApproval   Kind = "cleanup_approval"
	KindBlockedEscalation Kind = "blocked_escalation"
	KindPolicy            Kind = "policy"
)

// State is where a question stands: open, raised, answered, applied, withdrawn or expired.
type State string

// The state vocabulary, in lifecycle order.
const (
	StateOpen      State = "open"
	StateRaised    State = "raised"
	StateAnswered  State = "answered"
	StateApplied   State = "applied"
	StateWithdrawn State = "withdrawn"
	StateExpired   State = "expired"
)

// The answer classes an answer cites, and the subjects a question may block.
const (
	AuthorityUser                = "user"
	AuthorityDelegatedManagement = "delegated-management"
	AuthorityParent              = "parent"

	BlockingRelationship = "relationship"
	BlockingIssue        = "issue"
	BlockingPlanNode     = "plan_node"
	BlockingProject      = "project"
)

// MinOptions and MaxOptions bound an option set: one option is no choice, four are no decision.
const (
	MinOptions = 2
	MaxOptions = 3
)

// The named refusals. Validate, Transition and Merge wrap one of these; use errors.Is.
var (
	ErrSchema              = errors.New("decisions: the record's schema is not " + Schema)
	ErrUnknownKind         = errors.New("decisions: unknown kind")
	ErrUnknownState        = errors.New("decisions: unknown state")
	ErrEmptyContext        = errors.New("decisions: context is empty")
	ErrOptionCount         = errors.New("decisions: an option set holds 2 or 3 options")
	ErrEmptyOptionID       = errors.New("decisions: an option id is empty")
	ErrDuplicateOptionID   = errors.New("decisions: option ids are not unique")
	ErrUnknownBlockingKind = errors.New("decisions: unknown blocking kind")
	ErrUnknownAuthority    = errors.New("decisions: unknown authority kind")
	ErrBadNeededBy         = errors.New("decisions: needed_by is not an RFC3339 timestamp")
	ErrRecommendation      = errors.New("decisions: the recommendation names no option")
	ErrTransition          = errors.New("decisions: that transition is not allowed")
	ErrFingerprintMismatch = errors.New("decisions: the two records have different fingerprints")
)

// Option is one choice a person may pick.
type Option struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Effect string `json:"effect"`
}

// Recommendation is the raiser's own suggestion: advice, not a decision.
type Recommendation struct {
	Option  string `json:"option"`
	OneLine string `json:"one_line"`
}

// Blocking is a stable blocking subject, a kind and a ref rather than prose.
type Blocking struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// Origin is where the question was raised.
type Origin struct {
	Issue   string `json:"issue"`
	Project string `json:"project"`
}

// Source points at the report, event or message the question was read from.
type Source struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// Authority is the answer class an answer must cite: opaque, stored and compared.
type Authority struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// Seen is one time the question was observed; showing it again appends here.
type Seen struct {
	At     string `json:"at"`
	Source string `json:"source"`
	Note   string `json:"note,omitempty"`
}

// Record is one crw-user-decision/1 record: every field of the format has a home here.
type Record struct {
	Schema            string          `json:"schema"`
	DecisionID        string          `json:"decision_id"`
	Fingerprint       string          `json:"fingerprint"`
	Kind              Kind            `json:"kind"`
	Context           string          `json:"context"`
	Options           []Option        `json:"options"`
	Recommendation    *Recommendation `json:"recommendation,omitempty"`
	Blocking          []Blocking      `json:"blocking"`
	NeededBy          string          `json:"needed_by,omitempty"`
	Origin            Origin          `json:"origin"`
	Source            Source          `json:"source"`
	Authority         Authority       `json:"authority"`
	State             State           `json:"state"`
	RaisedAt          string          `json:"raised_at,omitempty"`
	RaisedVia         string          `json:"raised_via,omitempty"`
	Seen              []Seen          `json:"seen"`
	AnsweredAt        string          `json:"answered_at,omitempty"`
	AnsweredBy        string          `json:"answered_by,omitempty"`
	AnsweredVia       string          `json:"answered_via,omitempty"`
	AnswerText        string          `json:"answer_text,omitempty"`
	AppliedAt         string          `json:"applied_at,omitempty"`
	AppliedEvent      string          `json:"applied_event,omitempty"`
	AppliedGeneration int64           `json:"applied_generation,omitempty"`
	WithdrawnReason   string          `json:"withdrawn_reason,omitempty"`
	ExpiredReason     string          `json:"expired_reason,omitempty"`
}

// Kinds is the kind vocabulary, in declaration order.
func Kinds() []Kind {
	return []Kind{KindDesignChoice, KindDependency, KindMergeApproval, KindCleanupApproval, KindBlockedEscalation, KindPolicy}
}

// States is the state vocabulary, in lifecycle order.
func States() []State {
	return []State{StateOpen, StateRaised, StateAnswered, StateApplied, StateWithdrawn, StateExpired}
}

var (
	authorityKinds = []string{AuthorityUser, AuthorityDelegatedManagement, AuthorityParent}
	blockingKinds  = []string{BlockingRelationship, BlockingIssue, BlockingPlanNode, BlockingProject}
)

func contains[T comparable](list []T, value T) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// IsKind, IsState, IsAuthorityKind and IsBlockingKind report vocabulary membership.
func IsKind(kind Kind) bool            { return contains(Kinds(), kind) }
func IsState(state State) bool         { return contains(States(), state) }
func IsAuthorityKind(kind string) bool { return contains(authorityKinds, kind) }
func IsBlockingKind(kind string) bool  { return contains(blockingKinds, kind) }

// AllowedTransitions is the state machine: applied, withdrawn and expired are terminal.
func AllowedTransitions() map[State][]State {
	return map[State][]State{
		StateOpen:      {StateRaised, StateWithdrawn, StateExpired},
		StateRaised:    {StateAnswered, StateWithdrawn, StateExpired},
		StateAnswered:  {StateApplied, StateWithdrawn, StateExpired},
		StateApplied:   nil,
		StateWithdrawn: nil,
		StateExpired:   nil,
	}
}

// CanTransition reports whether the state machine allows from -> to; an unknown state permits nothing.
func CanTransition(from, to State) bool {
	return IsState(from) && IsState(to) && contains(AllowedTransitions()[from], to)
}

// Transition moves the record to state, or refuses it (ErrTransition, ErrUnknownState) unchanged.
func Transition(record *Record, to State) error {
	if !IsState(record.State) {
		return fmt.Errorf("%w: %q", ErrUnknownState, record.State)
	}
	if !IsState(to) {
		return fmt.Errorf("%w: %q", ErrUnknownState, to)
	}
	if !CanTransition(record.State, to) {
		return fmt.Errorf("%w: %s -> %s", ErrTransition, record.State, to)
	}
	record.State = to
	return nil
}

// Normalize is the fingerprint's normalization: lowercase, collapse each whitespace run to one
// space, and drop leading and trailing whitespace.
func Normalize(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(text)), " ")
}

// blockingSubject is each entry's normalized kind and ref joined by a colon, sorted, comma-joined.
func blockingSubject(blocking []Blocking) string {
	entries := make([]string, 0, len(blocking))
	for _, entry := range blocking {
		entries = append(entries, Normalize(entry.Kind)+":"+Normalize(entry.Ref))
	}
	sort.Strings(entries)
	return Normalize(strings.Join(entries, ","))
}

// optionIDs is the normalized option ids, sorted and comma-joined, so their order does not matter.
func optionIDs(options []Option) string {
	ids := make([]string, 0, len(options))
	for _, option := range options {
		ids = append(ids, Normalize(option.ID))
	}
	sort.Strings(ids)
	return Normalize(strings.Join(ids, ","))
}

// Fingerprint is the question's identity: the first 16 hex characters of the SHA-256 of
// normalize(context), the blocking subject and the sorted option ids, joined by "|". Two
// statements of one question that normalize alike, block the same subjects and offer the same
// options share a fingerprint and are one record.
func Fingerprint(context string, blocking []Blocking, options []Option) string {
	material := Normalize(context) + "|" + blockingSubject(blocking) + "|" + optionIDs(options)
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])[:16]
}

// Stamp fills the schema id, the open state and the content's fingerprint, refusing a bad record.
func Stamp(record Record) (Record, error) {
	if record.Schema == "" {
		record.Schema = Schema
	}
	if record.State == "" {
		record.State = StateOpen
	}
	if err := Validate(record); err != nil {
		return Record{}, err
	}
	record.Fingerprint = Fingerprint(record.Context, record.Blocking, record.Options)
	return record, nil
}

// Validate refuses a record that is not a well-formed crw-user-decision/1: a wrong schema id, an
// unknown kind or state, an empty context, an option set that is not 2 or 3 options with unique
// non-empty ids, a blocking kind outside the vocabulary, an answer class that is set but outside
// the vocabulary, a needed_by that is not RFC3339, or a recommendation naming no option.
func Validate(record Record) error {
	if record.Schema != Schema {
		return fmt.Errorf("%w: %q", ErrSchema, record.Schema)
	}
	if !IsKind(record.Kind) {
		return fmt.Errorf("%w: %q", ErrUnknownKind, record.Kind)
	}
	if !IsState(record.State) {
		return fmt.Errorf("%w: %q", ErrUnknownState, record.State)
	}
	if strings.TrimSpace(record.Context) == "" {
		return ErrEmptyContext
	}
	if len(record.Options) < MinOptions || len(record.Options) > MaxOptions {
		return fmt.Errorf("%w: got %d", ErrOptionCount, len(record.Options))
	}
	ids := make(map[string]bool, len(record.Options))
	for _, option := range record.Options {
		id := strings.TrimSpace(option.ID)
		if id == "" {
			return ErrEmptyOptionID
		}
		if ids[id] {
			return fmt.Errorf("%w: %q", ErrDuplicateOptionID, id)
		}
		ids[id] = true
	}
	for _, entry := range record.Blocking {
		if !IsBlockingKind(entry.Kind) {
			return fmt.Errorf("%w: %q", ErrUnknownBlockingKind, entry.Kind)
		}
	}
	if record.Authority.Kind != "" && !IsAuthorityKind(record.Authority.Kind) {
		return fmt.Errorf("%w: %q", ErrUnknownAuthority, record.Authority.Kind)
	}
	if record.NeededBy != "" {
		if _, err := time.Parse(time.RFC3339, record.NeededBy); err != nil {
			return fmt.Errorf("%w: %q", ErrBadNeededBy, record.NeededBy)
		}
	}
	if record.Recommendation != nil && !ids[strings.TrimSpace(record.Recommendation.Option)] {
		return fmt.Errorf("%w: %q", ErrRecommendation, record.Recommendation.Option)
	}
	return nil
}

// Merge folds a second statement of the same question into the first: they must carry the same
// fingerprint and the second one's seen entries are appended to the first one's, so a question
// raised again is one record with a longer seen. No store is involved. Each stored fingerprint
// is checked against its content, so a stale field cannot merge two different questions.
func Merge(first, second Record) (Record, error) {
	if err := Validate(first); err != nil {
		return Record{}, err
	}
	if err := Validate(second); err != nil {
		return Record{}, err
	}
	for _, record := range []Record{first, second} {
		if content := Fingerprint(record.Context, record.Blocking, record.Options); content != record.Fingerprint {
			return Record{}, fmt.Errorf("%w: %s is not its content's %s", ErrFingerprintMismatch, record.Fingerprint, content)
		}
	}
	if first.Fingerprint != second.Fingerprint {
		return Record{}, fmt.Errorf("%w: %s and %s", ErrFingerprintMismatch, first.Fingerprint, second.Fingerprint)
	}
	merged := first
	merged.Seen = append(append([]Seen{}, first.Seen...), second.Seen...)
	return merged, nil
}
