package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/decisions"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The relay's user-decision commands (CRW-737): decision-raise records a question, decision-list
// reads the records back, decision-answer records an answer with its provenance, decision-apply
// marks it applied once the decision_reply event of a relationship the decision blocks has been
// observed, and decision-withdraw retracts it. The record format, its state machine and the
// fingerprint live in internal/relay/decisions; the table lives in the DAG zone
// (store.Raise/List/Get/Update). These commands read the delivery domain's exported reader only
// (delivery.DecisionEventID): decision.go and decision_cli.go belong to another project and are
// never edited here.

// decisionBadInvocation is cli.py's bad_invocation refusal for an argument a handler refuses.
func decisionBadInvocation(detail string) error {
	return &dispatch.PayloadExit{Payload: contract.OrderedObject{{Key: "ok", Value: false}, {Key: "reason", Value: "bad_invocation"}, {Key: "detail", Value: detail}}, Code: contract.ExitRefused}
}

// decisionConflict is the refusal of a change the record's state or answer does not allow.
func decisionConflict(reason, detail string) error {
	return &dispatch.PayloadExit{Payload: contract.OrderedObject{{Key: "ok", Value: false}, {Key: "reason", Value: reason}, {Key: "detail", Value: detail}}, Code: contract.ExitRefused}
}

// decisionStore opens the selected store for the read forms (the dispatch admission hands the
// writable store over for the write forms, and openStore takes it from the same slot).
func decisionStore(ctx context.Context, services dispatch.Services) (*store.Store, error) {
	return openStore(ctx, services)
}

var decisionRaiseCommand = dispatch.Command{
	Name: "decision-raise",
	Run:  runDecisionRaise,
}

var decisionListCommand = dispatch.Command{
	Name:     "decision-list",
	ReadOnly: true,
	Run:      runDecisionList,
}

var decisionAnswerCommand = dispatch.Command{
	Name: "decision-answer",
	Run:  runDecisionAnswer,
}

var decisionApplyCommand = dispatch.Command{
	Name: "decision-apply",
	Run:  runDecisionApply,
}

var decisionWithdrawCommand = dispatch.Command{
	Name: "decision-withdraw",
	Run:  runDecisionWithdraw,
}

// decisionCommands are registered from this package's init.
var decisionCommands = []dispatch.Command{decisionRaiseCommand, decisionListCommand, decisionAnswerCommand, decisionApplyCommand, decisionWithdrawCommand}

// decisionNow is the raise/answer/apply stamp: RFC 3339 with nanoseconds, as the format requires.
func decisionNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// newDecisionID names a record: ud- and 32 hex characters from crypto/rand, so two raises of two
// different questions never collide, and the id is not derived from the question (the fingerprint
// is the question's identity; the id identifies one stored record).
func newDecisionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "ud-" + hex.EncodeToString(raw[:]), nil
}

// runDecisionRaise is decision-raise: it builds one crw-user-decision/1 record from the line and
// stores it (store.Raise folds a repeated question into the record that already carries its
// fingerprint). The observation of this raise is one seen entry, source "project:<origin-project>",
// which is how a second project sees a question first raised elsewhere.
func runDecisionRaise(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	kind := decisions.Kind(args.Text("kind"))
	if !decisions.IsKind(kind) {
		return nil, decisionBadInvocation(fmt.Sprintf("--kind %q is not a decision kind; it is one of %s", string(kind), decisionKindList()))
	}
	options, err := decisionOptions(args)
	if err != nil {
		return nil, err
	}
	// --option-reply names the decision_reply each option makes when it is chosen, so the apply
	// can match the reply the relay recorded against the option the answer chose.
	options, err = decisionOptionReplies(args, options)
	if err != nil {
		return nil, err
	}
	blocking, err := decisionBlockings(args)
	if err != nil {
		return nil, err
	}
	recommendation, err := decisionRecommendation(args, options)
	if err != nil {
		return nil, err
	}
	originProject := strings.TrimSpace(args.Text("origin-project"))
	if originProject == "" {
		return nil, decisionBadInvocation("--origin-project names the project the question was raised from")
	}
	source, err := decisionPair(args.Text("source"), "--source")
	if err != nil {
		return nil, err
	}
	authority, err := decisionAuthority(args.Text("authority"))
	if err != nil {
		return nil, err
	}
	id, err := newDecisionID()
	if err != nil {
		return nil, dispatch.Host("the decision id could not be generated: " + err.Error())
	}
	now := decisionNow()
	record := decisions.Record{
		Schema:         decisions.Schema,
		DecisionID:     id,
		Kind:           kind,
		Context:        args.Text("context"),
		Options:        options,
		Recommendation: recommendation,
		Blocking:       blocking,
		NeededBy:       strings.TrimSpace(args.Text("needed-by")),
		Origin:         decisions.Origin{Issue: strings.TrimSpace(args.Text("origin-issue")), Project: originProject},
		Source:         source,
		Authority:      authority,
		State:          decisions.StateRaised,
		RaisedAt:       now,
		RaisedVia:      strings.TrimSpace(args.Text("raised-via")),
		Seen:           []decisions.Seen{{At: now, Source: "project:" + originProject}},
	}
	record.Fingerprint = decisions.Fingerprint(record.Context, record.Blocking, record.Options)
	// The raise is held to the reply requirement the reader is not: a relationship-blocking
	// question raised now names a reply on every option, while a record written before the reply
	// field existed is still read, listed and answered.
	if err := decisions.ValidateRaise(record); err != nil {
		return nil, decisionRefusal(err)
	}
	opened, err := decisionStore(ctx, services)
	if err != nil {
		return nil, err
	}
	defer opened.Close()
	stored, merged, err := opened.Raise(ctx, record)
	if err != nil {
		return nil, decisionRefusal(err)
	}
	return contract.OrderedObject{
		{Key: "decisionId", Value: stored.DecisionID},
		{Key: "fingerprint", Value: stored.Fingerprint},
		{Key: "merged", Value: merged},
		{Key: "state", Value: string(stored.State)},
	}, nil
}

// runDecisionList is decision-list: a read of the records the filters select.
func runDecisionList(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	state := strings.TrimSpace(args.Text("state"))
	if state != "" && !decisions.IsState(decisions.State(state)) {
		return nil, decisionBadInvocation(fmt.Sprintf("--state %q is not a decision state; it is one of %s", state, decisionStateList()))
	}
	filter := store.UserDecisionFilter{State: decisions.State(state), Project: strings.TrimSpace(args.Text("project"))}
	opened, err := decisionStore(ctx, services)
	if err != nil {
		return nil, err
	}
	defer opened.Close()
	records, err := opened.List(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(records))
	for _, record := range records {
		out = append(out, decisionRecordObject(record))
	}
	return out, nil
}

// runDecisionAnswer is decision-answer: it records the answer and its provenance on the record.
func runDecisionAnswer(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	decisionID := strings.TrimSpace(args.Text("decision"))
	option, text := strings.TrimSpace(args.Text("option")), args.Text("text")
	// An option chooses one of the offered replies and free text states the answer in prose: one
	// stored answer cannot carry both, and keeping the text while dropping the choice would lose
	// the reply the apply matches. The answer names one of them.
	if option != "" && strings.TrimSpace(text) != "" {
		return nil, decisionBadInvocation("--option and --text answer a decision one way or the other: --option chooses one of the offered replies and --text states the answer in prose; give one of them")
	}
	answer := decisions.Answer{
		Option: option,
		Text:   text,
		By:     strings.TrimSpace(args.Text("by")),
		Via:    strings.TrimSpace(args.Text("via")),
	}
	if kind, ref, given := decisionAuthorityOption(args.Text("authority")); given {
		answer.Authority = decisions.Authority{Kind: kind, Ref: ref}
	}
	opened, err := decisionStore(ctx, services)
	if err != nil {
		return nil, err
	}
	defer opened.Close()
	stored, err := opened.Update(ctx, decisionID, func(txCtx context.Context, record decisions.Record) (decisions.Record, error) {
		if err := decisionAnswerProvenance(txCtx, opened, record, answer); err != nil {
			return decisions.Record{}, err
		}
		answered, err := decisions.ValidateAnswer(record, answer)
		if err != nil {
			return decisions.Record{}, decisionRefusal(err)
		}
		return answered, nil
	})
	if err != nil {
		return nil, decisionRefusal(err)
	}
	return contract.OrderedObject{
		{Key: "decisionId", Value: stored.DecisionID},
		{Key: "state", Value: string(stored.State)},
		{Key: "answeredBy", Value: stored.AnsweredBy},
		{Key: "answeredVia", Value: stored.AnsweredVia},
	}, nil
}

// decisionAnswerProvenance judges who may answer a user-grade or delegated-management-grade
// decision. The class the answer cites is the class the record requires, so the answer states it
// rather than inheriting it; the recorder must be the task the relay's store-scope supervisor seat
// holds, because those grades are the ones the decision document reserves for the user (deletion,
// a new mandatory requirement, Done judgement, a host restart, hook trust, a global instruction and
// the acceptance of a security or data-loss finding); and the via is a way the user's answer
// reached the record rather than the parent's report read back. A grade below them states no such
// requirement here. Every refusal is made inside the record's transaction, so the record the
// judgment read is the row the write replaces.
func decisionAnswerProvenance(ctx context.Context, opened *store.Store, record decisions.Record, answer decisions.Answer) error {
	grade := strings.TrimSpace(record.Authority.Kind)
	if grade != decisions.AuthorityUser && grade != decisions.AuthorityDelegatedManagement {
		return nil
	}
	if strings.TrimSpace(answer.Authority.Kind) != grade {
		return decisionBadInvocation(fmt.Sprintf("a %s-class decision takes an answer that names its own class: give --authority %s", grade, grade))
	}
	if !containsString(decisionUserAnswerVias(), answer.Via) {
		return decisionBadInvocation(fmt.Sprintf("a %s-class decision is answered through %s; --via %q reads the parent's report, not the user's answer", grade, strings.Join(decisionUserAnswerVias(), ", "), answer.Via))
	}
	bound, err := (&registry.Registry{Store: opened}).StoreScopeSupervisor(ctx)
	if err != nil {
		// A store that cannot be read is a host failure, not a verdict on the recorder: the answer
		// is not recorded either way, so nothing is accepted.
		return err
	}
	// Fail closed on every shape but one: no store-scope supervisor seat bound, a contested seat
	// (more than one row, which the schema's per-role unique index should not allow), or a seat
	// held by another task all refuse. The answer comes from the seat this grade is answered from
	// or it is not accepted.
	if len(bound) != 1 {
		return decisionConflict("authority_unverified", fmt.Sprintf("a %s-class decision is answered by the task the relay's store-scope supervisor binding holds, and %d such binding(s) are live", grade, len(bound)))
	}
	if taskID, _ := get(bound[0], "taskId").(string); strings.TrimSpace(taskID) == answer.By {
		return nil
	}
	return decisionConflict("authority_unverified", fmt.Sprintf("a %s-class decision is answered by the task the relay's store-scope supervisor binding holds, and %q is not it", grade, answer.By))
}

// decisionUserAnswerVias is the via vocabulary a user-grade or delegated-management-grade answer
// arrives through. supervisor-readback is not one of them: it is the parent's report read back,
// which is what a supervisor observed rather than what the user answered.
func decisionUserAnswerVias() []string {
	return []string{decisions.ViaDots, decisions.ViaDirectAsk, decisions.ViaFileImport, decisions.ViaManagementMessage}
}

// containsString reports whether list holds value.
func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// runDecisionApply is decision-apply: it moves an answered record to applied once the event that
// unblocked the relationship it blocks is the record's own decision_reply event, and that reply has
// reached the child. Delivery alone never applies a decision (Q5): the event must be the one the
// relay computed for the receipt the decision answered, and the relay must have recorded its
// delivery as accepted by the child, so the apply is only a read of what the relay recorded.
func runDecisionApply(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	decisionID := strings.TrimSpace(args.Text("decision"))
	event := strings.TrimSpace(args.Text("event"))
	if event == "" {
		return nil, decisionBadInvocation("--event names the decision_reply event that unblocked the relationship")
	}
	opened, err := decisionStore(ctx, services)
	if err != nil {
		return nil, err
	}
	defer opened.Close()
	stored, err := opened.Update(ctx, decisionID, func(txCtx context.Context, record decisions.Record) (decisions.Record, error) {
		generation, err := decisionReplyEvent(txCtx, opened, record, event)
		if err != nil {
			return decisions.Record{}, err
		}
		applied, err := decisions.Apply(record, event, generation)
		if err != nil {
			return decisions.Record{}, decisionRefusal(err)
		}
		return applied, nil
	})
	if err != nil {
		return nil, decisionRefusal(err)
	}
	return contract.OrderedObject{
		{Key: "decisionId", Value: stored.DecisionID},
		{Key: "state", Value: string(stored.State)},
		{Key: "appliedEvent", Value: stored.AppliedEvent},
	}, nil
}

// decisionReplyOutcome is the events.outcome value a decision reply carries
// (delivery.DecisionReply); the string is the contract's, and the delivery package's constant of
// that name is read here so a rename there is a build error rather than a silent drift.
var decisionReplyOutcome = delivery.DecisionReply

// decisionReplyProducer is the events.producer value the relay's own writer sets on a
// decision_reply row (delivery's queueToChild); a child never writes that outcome, so requiring it
// keeps a row a child could author from applying a decision.
const decisionReplyProducer = "relay"

// decisionReplyEvent reports whether the events row named event is the decision_reply event that
// answers the receipt this decision was raised on, for a relationship the decision blocks.
//
// Several things must hold, and all are needed: the row must be the relay's own decision_reply (a
// child never writes that outcome); the relationship it answers must be one the decision blocks; the
// decision the reply carries must be the reply the option the answer chose makes (the relay records
// one decision_reply per receipt, so a question whose hold option replies stop is not applied by a
// reply that decided answer, and an answer whose chosen option carries no reply, or that names no
// option at all, cannot be matched and is refused the same way); and the relay must have recorded
// the reply's delivery as accepted by the child, because a decision is applied on the reply the
// child received rather than on the event alone.
//
// How the reply is identified depends on what the question was raised from, which is its source
// kind. A question raised on a receipt names that receipt as its source ref, so the reply must
// answer it — an older reply to another receipt of the same relationship cannot apply a later
// decision — and its id must be the relay's own delivery.DecisionEventID(relationship,
// answersEvent), read from the delivery package rather than re-derived, because decision.go belongs
// to another project. Any other source — a report, a status file, a message — names no receipt, so
// the reply is identified by the relationship and by when the relay recorded it: it must have been
// recorded at or after the decision was answered. What it returns is the execution generation the
// reply event itself was recorded under, which the record then carries as the generation it was
// applied in.
func decisionReplyEvent(ctx context.Context, opened *store.Store, record decisions.Record, event string) (int64, error) {
	row, err := opened.One(ctx, "SELECT relationship_id, outcome, producer, receipt, execution_generation, first_seen_at FROM events WHERE event_id = ?", event)
	if err != nil {
		return 0, err
	}
	if row == nil || row.Text("outcome") != decisionReplyOutcome || row.Text("producer") != decisionReplyProducer {
		return 0, decisionNotTheReplyEvent(event)
	}
	relationship := row.Text("relationship_id")
	if !decisionBlocks(record, decisions.BlockingRelationship, relationship) {
		return 0, decisionNotTheReplyEvent(event)
	}
	receipt, err := decodeJSON([]byte(row.Text("receipt")))
	if err != nil {
		return 0, decisionBadInvocation("the decision_reply event's receipt cannot be read: " + err.Error())
	}
	answers, _ := get(receipt, "answersEvent").(string)
	if answers == "" {
		return 0, decisionNotTheReplyEvent(event)
	}
	if decisionSourceIsEvent(record) {
		// The question was raised on a receipt, and decision-raise names that receipt as the
		// record's source: the reply must answer it, so an older reply to another receipt of the
		// same relationship cannot apply a later decision.
		if strings.TrimSpace(record.Source.Ref) != answers {
			return 0, decisionNotTheReplyEvent(event)
		}
		if delivery.DecisionEventID(relationship, answers) != event {
			return 0, decisionNotTheReplyEvent(event)
		}
	} else if !decisionReplyRecordedAtOrAfterTheAnswer(record, row.Text("first_seen_at")) {
		// A question raised from anything else — a report, a status file, a message — names no
		// receipt, so the reply is identified by the relationship it answers and by when the relay
		// recorded it: a reply recorded before the decision was answered cannot be the reply that
		// answers it. An instant either side cannot be read is refused rather than accepted.
		return 0, decisionNotTheReplyEvent(event)
	}
	decision, _ := get(receipt, "decision").(string)
	option, chosen := decisionChosenOption(record)
	if !chosen {
		return 0, decisionConflict("reply_contradicts_answer", fmt.Sprintf("the answer %q names no option of the decision, so the reply %q the relay recorded cannot be matched against the reply the answer chose", record.AnswerText, decision))
	}
	if strings.TrimSpace(option.Reply) != strings.TrimSpace(decision) {
		return 0, decisionConflict("reply_contradicts_answer", fmt.Sprintf("the answer chose option %q, whose reply is %q, and the relay recorded the decision %q: a decision is applied by the reply its chosen option makes", option.ID, option.Reply, decision))
	}
	// The reply applies the decision only once the relay has recorded its delivery as accepted by
	// the child: a reply that is still queued has not been observed by the child, and Q5 applies a
	// decision on the reply the child received, not on the event alone.
	delivered, err := delivery.NewService(opened, delivery.SystemClock{}).Find(ctx, event)
	if err != nil {
		return 0, err
	}
	if !decisionReplyDelivered(delivered) {
		return 0, decisionConflict("reply_not_delivered", fmt.Sprintf("the reply %q has not been delivered to the child (%s): a decision is applied once the relay records the reply's delivery as accepted", event, decisionDeliveryState(delivered)))
	}
	generation, ok := row.Get("execution_generation").(int64)
	if !ok {
		return 0, dispatch.Host(fmt.Sprintf("the decision_reply event %q carries no execution generation", event))
	}
	return generation, nil
}

// decisionSourceIsEvent reports whether the question was raised on a receipt: the source kind
// decision-raise records for the event a question was read from. A receipt is the one source whose
// identity is the event id the reply must answer; every other source names no receipt, so the reply
// is matched by the relationship and the time it was recorded instead.
func decisionSourceIsEvent(record decisions.Record) bool {
	return strings.TrimSpace(record.Source.Kind) == decisionEventSourceKind
}

// decisionEventSourceKind is the source kind a receipt is recorded under. The value is the format's,
// written by decision-raise as the kind of the report, event or message the question was read from.
const decisionEventSourceKind = "event"

// decisionReplyRecordedAtOrAfterTheAnswer reports whether the reply event was first seen at or after
// the instant the decision was answered, both read as instants rather than compared as text. An
// instant either side cannot be read is refused: a reply whose time cannot be placed is not shown
// to have answered the decision.
func decisionReplyRecordedAtOrAfterTheAnswer(record decisions.Record, firstSeenAt string) bool {
	answered, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(record.AnsweredAt))
	if err != nil {
		return false
	}
	seen, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(firstSeenAt))
	if err != nil {
		return false
	}
	return !seen.Before(answered)
}

// decisionReplyDelivered reports whether the delivery the relay queued for the reply event has
// reached the child: the state the transport records once the child's host accepted the turn the
// message was carried in. Every other state — queued, sending, a held or uncertain send, a busy or
// withheld one, a stored-not-woken one, a superseded one — is not delivery, and an event with no
// delivery row at all is not either, so a decision applies only on a reply that actually arrived.
func decisionReplyDelivered(delivered delivery.Row) bool {
	if delivered == nil {
		return false
	}
	switch delivered.S("state") {
	case delivery.Dispatched, delivery.Acknowledged:
		return true
	}
	return false
}

// decisionDeliveryState is the delivery's state for a refusal's detail, or that no delivery was
// queued for the event.
func decisionDeliveryState(delivered delivery.Row) string {
	if delivered == nil {
		return "no delivery is queued"
	}
	return fmt.Sprintf("state %q", delivered.S("state"))
}

// decisionNotTheReplyEvent is the refusal of an event that is not the decision_reply the relay
// recorded for the receipt this decision answered.
func decisionNotTheReplyEvent(event string) error {
	return decisionConflict("disposition_conflict",
		fmt.Sprintf("%q is not the decision_reply event of a relationship this decision blocks: the record applies when the relay has recorded that reply", event))
}

// decisionChosenOption is the option the record's answer chose. An answer that names an option
// stores that option's id as the answer text, so the id is what identifies the reply the answer
// made.
func decisionChosenOption(record decisions.Record) (decisions.Option, bool) {
	chosen := strings.TrimSpace(record.AnswerText)
	if chosen == "" {
		return decisions.Option{}, false
	}
	for _, option := range record.Options {
		if strings.TrimSpace(option.ID) == chosen {
			return option, true
		}
	}
	return decisions.Option{}, false
}

// decisionBlocks reports whether the record names subject as a blocking subject of that kind.
func decisionBlocks(record decisions.Record, kind, subject string) bool {
	for _, entry := range record.Blocking {
		if entry.Kind == kind && entry.Ref == subject {
			return true
		}
	}
	return false
}

// runDecisionWithdraw is decision-withdraw: it retracts a record with its reason. A withdrawn
// record takes no further answer (decisions.ValidateAnswer refuses a non-answerable state).
func runDecisionWithdraw(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	decisionID := strings.TrimSpace(args.Text("decision"))
	reason := strings.TrimSpace(args.Text("reason"))
	if reason == "" {
		return nil, decisionBadInvocation("--reason says why the decision is withdrawn")
	}
	opened, err := decisionStore(ctx, services)
	if err != nil {
		return nil, err
	}
	defer opened.Close()
	stored, err := opened.Update(ctx, decisionID, func(txCtx context.Context, record decisions.Record) (decisions.Record, error) {
		withdrawn, err := decisions.Withdraw(record, reason)
		if err != nil {
			return decisions.Record{}, decisionRefusal(err)
		}
		return withdrawn, nil
	})
	if err != nil {
		return nil, decisionRefusal(err)
	}
	return contract.OrderedObject{
		{Key: "decisionId", Value: stored.DecisionID},
		{Key: "state", Value: string(stored.State)},
		{Key: "withdrawnReason", Value: stored.WithdrawnReason},
	}, nil
}

// decisionRefusal turns a decisions package refusal into the relay's refusal envelope, choosing
// the machine-readable reason the kind of failure has.
func decisionRefusal(err error) error {
	switch {
	case errors.Is(err, decisions.ErrUnknownKind), errors.Is(err, decisions.ErrUnknownState),
		errors.Is(err, decisions.ErrUnknownAuthority), errors.Is(err, decisions.ErrUnknownBlockingKind),
		errors.Is(err, decisions.ErrUnknownVia), errors.Is(err, decisions.ErrMissingProvenance),
		errors.Is(err, decisions.ErrUnknownAnswer), errors.Is(err, decisions.ErrAuthorityGrade),
		errors.Is(err, decisions.ErrUnknownReply), errors.Is(err, decisions.ErrOptionReplyRequired),
		errors.Is(err, decisions.ErrAnswerNeedsOption),
		errors.Is(err, decisions.ErrEmptyContext), errors.Is(err, decisions.ErrOptionCount),
		errors.Is(err, decisions.ErrEmptyOptionID), errors.Is(err, decisions.ErrDuplicateOptionID),
		errors.Is(err, decisions.ErrBadNeededBy), errors.Is(err, decisions.ErrBadRaisedAt),
		errors.Is(err, decisions.ErrRecommendation), errors.Is(err, decisions.ErrSchema):
		return decisionBadInvocation(err.Error())
	case errors.Is(err, decisions.ErrAmbiguousField), errors.Is(err, decisions.ErrControlCharacter):
		return decisionBadInvocation(err.Error())
	case errors.Is(err, decisions.ErrAnswerState), errors.Is(err, decisions.ErrTransition):
		return decisionConflict("disposition_conflict", err.Error())
	case errors.Is(err, decisions.ErrMergeConflict):
		return decisionConflict("disposition_conflict", err.Error())
	case errors.Is(err, store.ErrUserDecisionAbsent):
		return decisionConflict("unregistered_relationship", err.Error())
	}
	return err
}

// decisionKindList and decisionStateList spell a vocabulary for a refusal's detail.
func decisionKindList() string {
	names := make([]string, 0, len(decisions.Kinds()))
	for _, kind := range decisions.Kinds() {
		names = append(names, string(kind))
	}
	return strings.Join(names, ", ")
}

func decisionStateList() string {
	names := make([]string, 0, len(decisions.States()))
	for _, state := range decisions.States() {
		names = append(names, string(state))
	}
	return strings.Join(names, ", ")
}

// decisionOptions reads --option id=label:effect (2 or 3 times).
func decisionOptions(args dispatch.Args) ([]decisions.Option, error) {
	values := args.Strings("option")
	options := make([]decisions.Option, 0, len(values))
	for _, value := range values {
		id, rest, ok := strings.Cut(value, "=")
		if !ok {
			return nil, decisionBadInvocation(fmt.Sprintf("--option %q is id=label:effect", value))
		}
		label, effect, ok := strings.Cut(rest, ":")
		if !ok {
			return nil, decisionBadInvocation(fmt.Sprintf("--option %q is id=label:effect", value))
		}
		options = append(options, decisions.Option{ID: id, Label: label, Effect: effect})
	}
	return options, nil
}

// decisionBlockings reads --blocking kind=ref, repeated.
func decisionBlockings(args dispatch.Args) ([]decisions.Blocking, error) {
	values := args.Strings("blocking")
	blockings := make([]decisions.Blocking, 0, len(values))
	for _, value := range values {
		kind, ref, ok := strings.Cut(value, "=")
		if !ok {
			return nil, decisionBadInvocation(fmt.Sprintf("--blocking %q is kind=ref", value))
		}
		blockings = append(blockings, decisions.Blocking{Kind: kind, Ref: ref})
	}
	return blockings, nil
}

// decisionOptionReplies reads --option-reply id=action, repeated, and attaches each reply to the
// option it names. A reply that names no option of this raise is refused: the reply belongs to one
// of the choices the question offers, and an option id the raise does not carry names none.
func decisionOptionReplies(args dispatch.Args, options []decisions.Option) ([]decisions.Option, error) {
	for _, value := range args.Strings("option-reply") {
		id, action, ok := strings.Cut(value, "=")
		if !ok {
			return nil, decisionBadInvocation(fmt.Sprintf("--option-reply %q is id=action", value))
		}
		id = strings.TrimSpace(id)
		index := -1
		for i := range options {
			if strings.TrimSpace(options[i].ID) == id {
				index = i
				break
			}
		}
		if index < 0 {
			return nil, decisionBadInvocation(fmt.Sprintf("--option-reply %q names no option of this raise", id))
		}
		options[index].Reply = strings.TrimSpace(action)
	}
	return options, nil
}

// decisionRecommendation reads --recommend id:one line.
func decisionRecommendation(args dispatch.Args, options []decisions.Option) (*decisions.Recommendation, error) {
	value, given := args.String("recommend")
	if !given || strings.TrimSpace(value) == "" {
		return nil, nil
	}
	option, oneLine, ok := strings.Cut(value, ":")
	if !ok {
		return nil, decisionBadInvocation(fmt.Sprintf("--recommend %q is id:one line", value))
	}
	return &decisions.Recommendation{Option: option, OneLine: oneLine}, nil
}

// decisionPair reads a kind=ref option.
func decisionPair(value, flag string) (decisions.Source, error) {
	kind, ref, ok := strings.Cut(value, "=")
	if !ok || strings.TrimSpace(kind) == "" {
		return decisions.Source{}, decisionBadInvocation(fmt.Sprintf("%s is kind=ref", flag))
	}
	return decisions.Source{Kind: kind, Ref: ref}, nil
}

// decisionAuthority reads the raise's --authority kind[=ref].
func decisionAuthority(value string) (decisions.Authority, error) {
	kind, ref, given := decisionAuthorityOption(value)
	if !given || kind == "" {
		return decisions.Authority{}, decisionBadInvocation("--authority is kind[=ref]; the answer class this question requires")
	}
	if !decisions.IsAuthorityKind(kind) {
		return decisions.Authority{}, decisionBadInvocation(fmt.Sprintf("--authority %q is not a decision authority; it is one of %s", kind, decisionAuthorityList()))
	}
	// The parent class is the one class whose ref is part of what it is: an answer of it must name
	// the parent authority it used, and a question raised without that ref can never take a
	// checkable parent-class answer. Refusing it here keeps the requirement on the question rather
	// than only on the answer.
	if kind == decisions.AuthorityParent && ref == "" {
		return decisions.Authority{}, decisionBadInvocation("--authority parent=ref names the parent authority this question takes; the parent class carries its ref")
	}
	return decisions.Authority{Kind: kind, Ref: ref}, nil
}

// decisionAuthorityOption splits kind[=ref]; given is false when the option was not on the line.
func decisionAuthorityOption(value string) (kind, ref string, given bool) {
	if strings.TrimSpace(value) == "" {
		return "", "", false
	}
	kind, ref, _ = strings.Cut(value, "=")
	return strings.TrimSpace(kind), strings.TrimSpace(ref), true
}

func decisionAuthorityList() string {
	return strings.Join([]string{decisions.AuthorityUser, decisions.AuthorityDelegatedManagement, decisions.AuthorityParent}, ", ")
}

// decisionRecordObject is one record as JSON, field for field in the format's order.
func decisionRecordObject(record decisions.Record) contract.OrderedObject {
	options := make([]any, 0, len(record.Options))
	for _, option := range record.Options {
		entry := contract.OrderedObject{{Key: "id", Value: option.ID}, {Key: "label", Value: option.Label}, {Key: "effect", Value: option.Effect}}
		if option.Reply != "" {
			entry = append(entry, contract.Field{Key: "reply", Value: option.Reply})
		}
		options = append(options, entry)
	}
	blocking := make([]any, 0, len(record.Blocking))
	for _, entry := range record.Blocking {
		blocking = append(blocking, contract.OrderedObject{{Key: "kind", Value: entry.Kind}, {Key: "ref", Value: entry.Ref}})
	}
	seen := make([]any, 0, len(record.Seen))
	for _, observation := range record.Seen {
		entry := contract.OrderedObject{{Key: "at", Value: observation.At}, {Key: "source", Value: observation.Source}}
		if observation.Note != "" {
			entry = append(entry, contract.Field{Key: "note", Value: observation.Note})
		}
		seen = append(seen, entry)
	}
	object := contract.OrderedObject{
		{Key: "schema", Value: record.Schema},
		{Key: "decision_id", Value: record.DecisionID},
		{Key: "fingerprint", Value: record.Fingerprint},
		{Key: "kind", Value: string(record.Kind)},
		{Key: "context", Value: record.Context},
		{Key: "options", Value: options},
	}
	if record.Recommendation != nil {
		object = append(object, contract.Field{Key: "recommendation", Value: contract.OrderedObject{
			{Key: "option", Value: record.Recommendation.Option}, {Key: "one_line", Value: record.Recommendation.OneLine}}})
	}
	object = append(object,
		contract.Field{Key: "blocking", Value: blocking},
		contract.Field{Key: "needed_by", Value: nullableText(record.NeededBy)},
		contract.Field{Key: "origin", Value: contract.OrderedObject{{Key: "issue", Value: nullableText(record.Origin.Issue)}, {Key: "project", Value: nullableText(record.Origin.Project)}}},
		contract.Field{Key: "source", Value: contract.OrderedObject{{Key: "kind", Value: record.Source.Kind}, {Key: "ref", Value: record.Source.Ref}}},
		contract.Field{Key: "authority", Value: contract.OrderedObject{{Key: "kind", Value: nullableText(record.Authority.Kind)}, {Key: "ref", Value: nullableText(record.Authority.Ref)}}},
		contract.Field{Key: "state", Value: string(record.State)},
		contract.Field{Key: "raised_at", Value: nullableText(record.RaisedAt)},
		contract.Field{Key: "raised_via", Value: nullableText(record.RaisedVia)},
		contract.Field{Key: "seen", Value: seen},
		contract.Field{Key: "answered_at", Value: nullableText(record.AnsweredAt)},
		contract.Field{Key: "answered_by", Value: nullableText(record.AnsweredBy)},
		contract.Field{Key: "answered_via", Value: nullableText(record.AnsweredVia)},
		contract.Field{Key: "answer_text", Value: nullableText(record.AnswerText)},
		contract.Field{Key: "applied_at", Value: nullableText(record.AppliedAt)},
		contract.Field{Key: "applied_event", Value: nullableText(record.AppliedEvent)},
		contract.Field{Key: "applied_generation", Value: record.AppliedGeneration},
		contract.Field{Key: "withdrawn_reason", Value: nullableText(record.WithdrawnReason)},
		contract.Field{Key: "expired_reason", Value: nullableText(record.ExpiredReason)},
	)
	return object
}
