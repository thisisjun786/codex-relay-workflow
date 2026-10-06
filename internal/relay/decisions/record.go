// Package decisions holds the relay's user-decision record: the crw-user-decision/1 field set,
// the kind and state vocabularies with their allowed transitions, validation, the fingerprint
// and the in-memory merge of two records that carry the same fingerprint. It holds no store and
// no command; the table that persists a record and the decision-raise/decision-list commands are
// other issues that build on this format. The fingerprint's fixed input/output pairs live in
// testdata/fingerprint_vectors.json.
package decisions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	ErrAmbiguousField      = errors.New("decisions: a field holds one of the fingerprint's delimiters")
	ErrControlCharacter    = errors.New("decisions: a field holds a control character")
	ErrTransition          = errors.New("decisions: that transition is not allowed")
	ErrMergeConflict       = errors.New("decisions: the two records differ beyond their seen entries")
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

// asciiLower lowercases A-Z only. Go's strings.ToLower and Python's str.lower disagree on a few
// code points, so neither is used and every other rune is left as it is.
func asciiLower(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	for _, r := range text {
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		out.WriteRune(r)
	}
	return out.String()
}

// Normalize is the fingerprint's normalization: ASCII-lowercase, collapse each whitespace run to
// one space, and drop leading and trailing whitespace.
func Normalize(text string) string {
	return strings.Join(strings.Fields(asciiLower(text)), " ")
}

// Fingerprint is the question's identity: the first 16 hex characters of the SHA-256 of the JSON
// encoding of [Normalize(context), [[kind, ref], ...] sorted, [option id, ...] sorted]. The
// encoding keeps the three parts and every list element apart, so no character inside a field can
// make two different questions share a material; Validate's delimiter refusals stay as a second
// guard. Two statements of one question that normalize alike are one record.
func Fingerprint(context string, blocking []Blocking, options []Option) string {
	subjects := make([][2]string, 0, len(blocking))
	for _, entry := range blocking {
		subjects = append(subjects, [2]string{Normalize(entry.Kind), Normalize(entry.Ref)})
	}
	sort.Slice(subjects, func(i, j int) bool {
		if subjects[i][0] != subjects[j][0] {
			return subjects[i][0] < subjects[j][0]
		}
		return subjects[i][1] < subjects[j][1]
	})
	ids := make([]string, 0, len(options))
	for _, option := range options {
		ids = append(ids, Normalize(option.ID))
	}
	sort.Strings(ids)
	material, err := json.Marshal([]any{Normalize(context), subjects, ids})
	if err != nil {
		// A string and two string slices always encode; a failure would mean the material changed
		// shape, and a fingerprint computed from nothing would be worse than stopping.
		panic("decisions: fingerprint material: " + err.Error())
	}
	sum := sha256.Sum256(material)
	return hex.EncodeToString(sum[:])[:16]
}

// Validate refuses a record that is not a well-formed crw-user-decision/1: a wrong schema id, an
// unknown kind or state, a control character in a free-text field, an empty context, an option set
// that is not 2 or 3 options with unique non-empty ids, a vocabulary miss, a needed_by that is not
// RFC3339, a recommendation naming no option, or a field carrying a fingerprint delimiter.
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
	if err := checkControlCharacters(record); err != nil {
		return err
	}
	if strings.TrimSpace(record.Context) == "" {
		return ErrEmptyContext
	}
	if strings.Contains(record.Context, "|") {
		return fmt.Errorf("%w: context contains %q", ErrAmbiguousField, "|")
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
		if strings.Contains(id, ",") {
			return fmt.Errorf("%w: option id %q contains %q", ErrAmbiguousField, id, ",")
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
		if strings.ContainsAny(entry.Ref, ":,") {
			return fmt.Errorf("%w: blocking ref %q contains %q or %q", ErrAmbiguousField, entry.Ref, ":", ",")
		}
	}
	if record.Authority.Kind != "" && !IsAuthorityKind(record.Authority.Kind) {
		return fmt.Errorf("%w: %q", ErrUnknownAuthority, record.Authority.Kind)
	}
	if record.NeededBy != "" {
		// RFC 3339's fraction separator is ".", but Go's parser also takes a comma; refuse the
		// comma explicitly and otherwise keep the value as written, fractional seconds included.
		if strings.Contains(record.NeededBy, ",") {
			return fmt.Errorf("%w: %q", ErrBadNeededBy, record.NeededBy)
		}
		if _, err := time.Parse(time.RFC3339Nano, record.NeededBy); err != nil {
			return fmt.Errorf("%w: %q", ErrBadNeededBy, record.NeededBy)
		}
	}
	if record.Recommendation != nil && !ids[strings.TrimSpace(record.Recommendation.Option)] {
		return fmt.Errorf("%w: %q", ErrRecommendation, record.Recommendation.Option)
	}
	return nil
}

// checkControlCharacters refuses the first free-text field that holds a control character: U+0000
// to U+001F or U+007F. The context alone may hold a newline or a tab, because a question is a
// paragraph. The fields walked are the ones the format carries as free text.
func checkControlCharacters(record Record) error {
	// hasControl reports a control character; allowLines exempts a newline and a tab.
	hasControl := func(text string, allowLines bool) bool {
		for _, r := range text {
			if allowLines && (r == '\n' || r == '\t') {
				continue
			}
			if r < 0x20 || r == 0x7f {
				return true
			}
		}
		return false
	}
	refuse := func(name, value string, allowLines bool) error {
		if hasControl(value, allowLines) {
			return fmt.Errorf("%w: %s", ErrControlCharacter, name)
		}
		return nil
	}
	if err := refuse("context", record.Context, true); err != nil {
		return err
	}
	if record.Recommendation != nil {
		if err := refuse("recommendation.option", record.Recommendation.Option, false); err != nil {
			return err
		}
		if err := refuse("recommendation.one_line", record.Recommendation.OneLine, false); err != nil {
			return err
		}
	}
	for _, option := range record.Options {
		for _, field := range []struct{ name, value string }{
			{"option.id", option.ID},
			{"option.label", option.Label},
			{"option.effect", option.Effect},
		} {
			if err := refuse(field.name, field.value, false); err != nil {
				return err
			}
		}
	}
	for _, entry := range record.Blocking {
		for _, field := range []struct{ name, value string }{
			{"blocking.kind", entry.Kind},
			{"blocking.ref", entry.Ref},
		} {
			if err := refuse(field.name, field.value, false); err != nil {
				return err
			}
		}
	}
	for _, seen := range record.Seen {
		for _, field := range []struct{ name, value string }{
			{"seen.at", seen.At},
			{"seen.source", seen.Source},
			{"seen.note", seen.Note},
		} {
			if err := refuse(field.name, field.value, false); err != nil {
				return err
			}
		}
	}
	for _, field := range []struct{ name, value string }{
		{"origin.issue", record.Origin.Issue},
		{"origin.project", record.Origin.Project},
		{"source.kind", record.Source.Kind},
		{"source.ref", record.Source.Ref},
		{"authority.kind", record.Authority.Kind},
		{"authority.ref", record.Authority.Ref},
		{"answered_at", record.AnsweredAt},
		{"answered_by", record.AnsweredBy},
		{"answered_via", record.AnsweredVia},
		{"answer_text", record.AnswerText},
		{"applied_at", record.AppliedAt},
		{"applied_event", record.AppliedEvent},
		{"withdrawn_reason", record.WithdrawnReason},
		{"expired_reason", record.ExpiredReason},
	} {
		if err := refuse(field.name, field.value, false); err != nil {
			return err
		}
	}
	return nil
}

// sameAnswer is what a second statement must agree on, so merging cannot drop a later answer. The
// fields are compared one by one: a material joining them would let two different answers collide.
func sameAnswer(first, second Record) bool {
	return first.State == second.State &&
		first.AnsweredAt == second.AnsweredAt &&
		first.AnsweredBy == second.AnsweredBy &&
		first.AnsweredVia == second.AnsweredVia &&
		first.AnswerText == second.AnswerText &&
		first.AppliedAt == second.AppliedAt &&
		first.AppliedEvent == second.AppliedEvent &&
		first.AppliedGeneration == second.AppliedGeneration &&
		first.WithdrawnReason == second.WithdrawnReason &&
		first.ExpiredReason == second.ExpiredReason
}

// Merge folds a second statement of the same question into the first. The two must name the same
// question (the same fingerprint) and carry the same answer, and the second record's seen entries
// are appended to the first record's. The stored statement survives: its kind, options,
// recommendation and origin are the ones the merged record keeps. A statement that differs beyond
// its seen entries is refused with ErrMergeConflict rather than overwritten, so no stale field and
// no later answer is lost. Each record must also be well formed and its stored fingerprint must
// match its content. The identity and answer checks run before the per-record validation so that a
// differing answer is reported as the conflict it is, rather than masked by a format refusal in
// one of the very fields the comparison covers.
func Merge(first, second Record) (Record, error) {
	if first.Fingerprint != second.Fingerprint {
		return Record{}, fmt.Errorf("%w: %s and %s", ErrFingerprintMismatch, first.Fingerprint, second.Fingerprint)
	}
	if !sameAnswer(first, second) {
		return Record{}, fmt.Errorf("%w: %s and %s", ErrMergeConflict, first.State, second.State)
	}
	for _, record := range []Record{first, second} {
		if err := Validate(record); err != nil {
			return Record{}, err
		}
		if content := Fingerprint(record.Context, record.Blocking, record.Options); content != record.Fingerprint {
			return Record{}, fmt.Errorf("%w: %s is not its content's %s", ErrFingerprintMismatch, record.Fingerprint, content)
		}
	}
	merged := first
	merged.Seen = append(append([]Seen{}, first.Seen...), second.Seen...)
	return merged, nil
}
