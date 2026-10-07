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

// The replies a decision's option makes when it is chosen: the decision_reply the relay records
// for a relationship the option unblocks. A decision that blocks a relationship carries one per
// option, because applying it is a comparison of the chosen option's reply with the reply the
// relay recorded; a decision that blocks nothing makes no reply and carries none.
const (
	ReplyAnswer        = "answer"
	ReplyStop          = "stop"
	ReplySplitApproval = "split_approval"
	ReplyScopeChange   = "scope_change"
)

// Replies is the option-reply vocabulary, in declaration order.
func Replies() []string {
	return []string{ReplyAnswer, ReplyStop, ReplySplitApproval, ReplyScopeChange}
}

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
	ErrBadRaisedAt         = errors.New("decisions: raised_at is not an RFC3339 timestamp")
	ErrRecommendation      = errors.New("decisions: the recommendation names no option")
	ErrUnknownReply        = errors.New("decisions: unknown option reply")
	ErrOptionReplyRequired = errors.New("decisions: every option of a relationship-blocking decision names its reply")
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
	// Reply is the decision_reply this option makes when it is chosen (answer, stop,
	// split_approval or scope_change). It is empty for a decision that blocks no relationship, and
	// it is not part of the fingerprint: two statements of one question that differ only in the
	// reply are the same question.
	Reply string `json:"reply,omitempty"`
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

// IsReply reports whether reply is one of the option-reply vocabulary.
func IsReply(reply string) bool { return contains(Replies(), reply) }

// ValidateRaise refuses a raise whose options could never be applied: a decision that blocks a
// relationship must name, on every option, the reply that option makes, because applying it
// compares the chosen option's reply with the decision_reply the relay recorded for the receipt
// the question answered. Validate deliberately does not carry this requirement: Validate is also
// the check a record already in the table is read under, and a record written before the reply
// field existed carries relationship-blocking options without one, which must not fail the read
// (the same reason a legacy raised_at is read as it stands). Such a record can be listed,
// answered and withdrawn; it can never be applied, because its options name no reply for the
// reply event's decision to match, and the apply refuses rather than guessing.
func ValidateRaise(record Record) error {
	if err := Validate(record); err != nil {
		return err
	}
	if !decisionBlocksRelationship(record) {
		return nil
	}
	for _, option := range record.Options {
		if strings.TrimSpace(option.Reply) == "" {
			return fmt.Errorf("%w: option %q", ErrOptionReplyRequired, option.ID)
		}
	}
	return nil
}

// decisionBlocksRelationship reports whether the record names a relationship among its blocking
// subjects. That is the subject whose decision_reply event applies the record, so it is the one
// whose options must carry the reply they make.
func decisionBlocksRelationship(record Record) bool {
	for _, entry := range record.Blocking {
		if entry.Kind == BlockingRelationship {
			return true
		}
	}
	return false
}

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
		if reply := strings.TrimSpace(option.Reply); reply != "" && !contains(Replies(), reply) {
			return fmt.Errorf("%w: %q", ErrUnknownReply, option.Reply)
		}
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
	if err := checkRFC3339Field(record.NeededBy, ErrBadNeededBy); err != nil {
		return err
	}
	// raised_at is a timestamp too, checked the same way and kept as written: the value a Raise
	// returns and the value List reads back must be the same text, and only its instant orders.
	if err := checkRFC3339Field(record.RaisedAt, ErrBadRaisedAt); err != nil {
		return err
	}
	if record.Recommendation != nil && !ids[strings.TrimSpace(record.Recommendation.Option)] {
		return fmt.Errorf("%w: %q", ErrRecommendation, record.Recommendation.Option)
	}
	return nil
}

// checkRFC3339Field refuses a timestamp field that is not RFC 3339. RFC 3339's fraction separator is
// ".", but Go's parser also takes a comma, so the comma is refused explicitly; the value is
// otherwise kept as written, fractional seconds included, because a stored value must read back as
// the text it was stored with.
func checkRFC3339Field(value string, refusal error) error {
	if value == "" {
		return nil
	}
	if strings.Contains(value, ",") {
		return fmt.Errorf("%w: %q", refusal, value)
	}
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		return fmt.Errorf("%w: %q", refusal, value)
	}
	return nil
}

// checkControlCharacters refuses the first string field that holds a control character: U+0000 to
// U+001F or U+007F. The context alone may hold a newline or a tab, because a question is a
// paragraph. Every string the record carries as text is walked, the identifiers and the raise
// fields included; the fields with their own vocabulary or timestamp check (schema, kind, state,
// needed_by, fingerprint) are the ones it does not repeat.
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
			{"option.reply", option.Reply},
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
		{"decision_id", record.DecisionID},
		{"origin.issue", record.Origin.Issue},
		{"origin.project", record.Origin.Project},
		{"source.kind", record.Source.Kind},
		{"source.ref", record.Source.Ref},
		{"authority.kind", record.Authority.Kind},
		{"authority.ref", record.Authority.Ref},
		{"raised_at", record.RaisedAt},
		{"raised_via", record.RaisedVia},
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
// question (the same fingerprint) and carry the same answer: the ten answer fields are compared one
// by one, and a differing answer is ErrMergeConflict. Everything else the fingerprint does not
// cover — kind, options, recommendation, origin, source, authority and the raise fields — is not
// compared, so the stored statement survives and the merged record keeps the first record's values
// for them; only the second record's seen entries are appended. Each record must also be well formed
// and its stored fingerprint must match its content. The identity and answer checks run before the
// per-record validation so that a differing answer is reported as the conflict it is, rather than
// masked by a format refusal in one of the very fields the comparison covers.
//
// The two statements are held to the format differently, because they are not the same kind of
// thing. The second is this raise's own fields, and it is checked whole. The first is the row the
// question is already stored as, and it is checked with the one exemption the store's read uses
// (decodeUserDecision): a raised_at an older build could write is kept as it stands, so a question
// that lists, answers and withdraws also takes a second observation. Everything the fold newly
// writes — the appended observations, the state the caller moves, and a raised_at this raise filled
// in — is checked as the format requires.
//
// An option's reply is folded the one way the identity rule allows. The fingerprint excludes the
// reply, so two raises of one question that name different replies for the same option are the same
// question and cannot become two records; the fold is therefore refused when the stored reply and
// the incoming one are both named and differ, because applying the record compares the chosen
// option's reply with the reply the relay recorded, and a silent fold would apply the later raise's
// answer against a mapping it never offered. A stored reply an older build could not have written
// (the field is this format's) is filled from the incoming raise, and an incoming raise that names
// no reply keeps the stored one. The merged options are then checked as the format requires.
func Merge(first, second Record) (Record, error) {
	if first.Fingerprint != second.Fingerprint {
		return Record{}, fmt.Errorf("%w: %s and %s", ErrFingerprintMismatch, first.Fingerprint, second.Fingerprint)
	}
	if !sameAnswer(first, second) {
		return Record{}, fmt.Errorf("%w: %s and %s", ErrMergeConflict, first.State, second.State)
	}
	if err := Validate(second); err != nil {
		return Record{}, err
	}
	if err := validateStoredRaisedAt(first); err != nil {
		return Record{}, err
	}
	options, err := mergeOptionReplies(first.Options, second.Options)
	if err != nil {
		return Record{}, err
	}
	for _, record := range []Record{first, second} {
		if content := Fingerprint(record.Context, record.Blocking, record.Options); content != record.Fingerprint {
			return Record{}, fmt.Errorf("%w: %s is not its content's %s", ErrFingerprintMismatch, record.Fingerprint, content)
		}
	}
	merged := first
	merged.Options = options
	merged.Seen = append(append([]Seen{}, first.Seen...), second.Seen...)
	if merged.RaisedAt == first.RaisedAt {
		if err := validateStoredRaisedAt(merged); err != nil {
			return Record{}, err
		}
	} else if err := Validate(merged); err != nil {
		return Record{}, err
	}
	return merged, nil
}

// mergeOptionReplies folds the incoming raise's option replies into the stored set. The two raises
// are one question because their fingerprints match, and the fingerprint identifies an option by its
// normalized id (Normalize: ASCII-lowercased, whitespace runs collapsed), so the fold matches ids the
// same way: an option stored as HOLD is the option an incoming hold names. Matching on the raw text
// would skip the fold for a spelling the fingerprint calls the same question, silently leaving a
// reply unfilled and a conflict unseen. A stored reply and an incoming reply that are both named and
// differ is ErrMergeConflict: the stored mapping is the one an answer is applied against, and the
// fingerprint cannot separate the two raises, so the second raise is refused rather than silently
// dropped. A stored reply that is empty — a record written before the reply field existed — takes the
// incoming one, and an incoming reply that is empty keeps the stored one. A stored option set that
// two of whose ids normalize alike cannot say which option an incoming reply belongs to, so it is
// refused as the ambiguity it is rather than guessed. A reply is never written where the option it
// belongs to is not in the stored set, because the stored option set is the one the record keeps.
func mergeOptionReplies(stored, incoming []Option) ([]Option, error) {
	merged := append([]Option{}, stored...)
	at := make(map[string]int, len(merged))
	for i, option := range merged {
		key := Normalize(option.ID)
		if _, seen := at[key]; seen {
			return nil, fmt.Errorf("%w: the stored options %q and %q are one id to the fingerprint", ErrMergeConflict, merged[at[key]].ID, option.ID)
		}
		at[key] = i
	}
	for _, option := range incoming {
		index, ok := at[Normalize(option.ID)]
		if !ok {
			continue
		}
		storedReply, incomingReply := strings.TrimSpace(merged[index].Reply), strings.TrimSpace(option.Reply)
		switch {
		case storedReply == "":
			merged[index].Reply = incomingReply
		case incomingReply == "" || storedReply == incomingReply:
			// The stored reply stands: the raise names none, or names the same one.
		default:
			return nil, fmt.Errorf("%w: option %q is stored with reply %q and this raise names %q", ErrMergeConflict, merged[index].ID, merged[index].Reply, option.Reply)
		}
	}
	return merged, nil
}

// validateStoredRaisedAt is Validate with the one exemption the store's read uses (decodeUserDecision):
// a stored raised_at that is not an RFC 3339 timestamp is kept as it stands rather than failing the
// record. Every other field is checked, and the writer path still refuses a raised_at it is asked to
// store: this exemption exists only so a row an older build wrote stays readable.
func validateStoredRaisedAt(record Record) error {
	kept := record.RaisedAt
	record.RaisedAt = ""
	err := Validate(record)
	record.RaisedAt = kept
	return err
}
