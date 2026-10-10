package decisions

import (
	"errors"
	"fmt"
	"strings"
)

// Derivation (CRW-718): the raise record a supervisor obligation asks for. The caller reads the
// facts of a child's final event (the supervisor package owns that judgment) and hands them over as
// an Observation; this package owns the DTO so that it never imports the supervisor. Derive holds
// no clock, no id generator and no store: the observation time and the new record's id come in, and
// the record that results goes to the store's Raise (or, for a question already held, comes back
// merged for the caller to write).

// The statuses of a work report that ask for a decision, and the outcome of a receipt that waits for
// one. They are the spellings the supervisor's obligation kind judges on.
const (
	ReportStatusUnsafe     = "UNSAFE"
	ReportStatusNeedsHuman = "NEEDS_HUMAN"
	OutcomeBlockedNeeds    = "blocked_needs_input"
)

// SourceKindObligation is the source kind of a derived record: its ref is the supervisor
// obligation's id, the link between the question and the obligation that raised it.
const SourceKindObligation = "supervisor_obligation"

// RaisedViaObligation is the raised_via of a derived record.
const RaisedViaObligation = "supervisor-obligation"

// The named refusals Derive makes beyond the format's own. Use errors.Is.
var (
	ErrNotDecisionRequest = errors.New("decisions: the observation is neither a blocked_needs_input receipt nor an UNSAFE or NEEDS_HUMAN work report")
	ErrObservationField   = errors.New("decisions: the observation lacks a field a derived record needs")
)

// Observation is the pure DTO a caller fills from a supervisor obligation and the event behind it.
type Observation struct {
	// DecisionID names the record when a new one is derived; it is unused when Existing is given.
	DecisionID string
	// ObligationID is the supervisor obligation's id: the record's link to it.
	ObligationID string
	// EventID is the final event the obligation was raised from.
	EventID string
	// RelationshipID is the relationship the child works under; Generation is its execution generation.
	RelationshipID string
	Generation     int64
	// Issue and Project are the child's issue key (may be empty) and the project key it was raised from.
	Issue   string
	Project string
	// Outcome is the event's outcome; ReportStatus is the status of its newest work report. As in the
	// supervisor's obligation judgment, a work report of UNSAFE or NEEDS_HUMAN decides first, and
	// otherwise a blocked_needs_input outcome does.
	Outcome      string
	ReportStatus string
	// Reason and Summary are the work report's own words (or the receipt's note); Summary may be empty.
	Reason  string
	Summary string
	// ObservedAt is the RFC 3339 time of this observation; NeededBy is an optional RFC 3339 deadline.
	ObservedAt string
	NeededBy   string
	// Authority is the answer class the question takes; the zero value asks for the user.
	Authority Authority
}

// Derive builds the raise record for an observation. With no existing record it is a new record in
// the raised state, held to ValidateRaise: a question that blocks a relationship names a reply on
// every option. With the record the question is already stored as, the observation is a repeated
// one: the result is that record with one more seen entry and nothing else changed (its id,
// fingerprint, state, answer and apply fields stay), produced by Merge, whose conflict checks are
// not relaxed. An existing record of another question is ErrFingerprintMismatch.
func Derive(obs Observation, existing *Record) (Record, error) {
	fresh, err := deriveRecord(obs, existing == nil)
	if err != nil {
		return Record{}, err
	}
	if existing == nil {
		return fresh, nil
	}
	// The repeat states what the stored record already says about its answer, so the merge compares
	// like with like and only the seen entry is new.
	repeat := fresh
	repeat.State = existing.State
	repeat.AnsweredAt, repeat.AnsweredBy, repeat.AnsweredVia = existing.AnsweredAt, existing.AnsweredBy, existing.AnsweredVia
	repeat.AnswerText = existing.AnswerText
	repeat.AppliedAt, repeat.AppliedEvent, repeat.AppliedGeneration = existing.AppliedAt, existing.AppliedEvent, existing.AppliedGeneration
	repeat.WithdrawnReason, repeat.ExpiredReason = existing.WithdrawnReason, existing.ExpiredReason
	merged, err := Merge(*existing, repeat)
	if err != nil {
		return Record{}, err
	}
	// Merge fills a reply an old record never named; a repeat observation changes nothing else.
	merged.Options = append([]Option{}, existing.Options...)
	return merged, nil
}

// deriveRecord is the record one observation states on its own.
func deriveRecord(obs Observation, needsID bool) (Record, error) {
	fromReport := obs.ReportStatus == ReportStatusUnsafe || obs.ReportStatus == ReportStatusNeedsHuman
	if !fromReport && obs.Outcome != OutcomeBlockedNeeds {
		return Record{}, fmt.Errorf("%w: outcome %q, work report %q", ErrNotDecisionRequest, obs.Outcome, obs.ReportStatus)
	}
	required := map[string]string{"obligation id": obs.ObligationID, "event id": obs.EventID, "relationship id": obs.RelationshipID, "project": obs.Project, "observed at": obs.ObservedAt}
	if needsID {
		required["decision id"] = obs.DecisionID
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return Record{}, fmt.Errorf("%w: %s", ErrObservationField, name)
		}
	}
	if err := checkRFC3339Field(obs.ObservedAt, ErrBadRaisedAt); err != nil {
		return Record{}, err
	}
	authority := obs.Authority
	if authority.Kind == "" {
		authority.Kind = AuthorityUser
	}
	options, blocking := derivedChoices(obs)
	note := "receipt " + OutcomeBlockedNeeds
	if fromReport {
		note = "work report " + obs.ReportStatus
	}
	record := Record{
		Schema:      Schema,
		DecisionID:  obs.DecisionID,
		Kind:        KindBlockedEscalation,
		Context:     derivedContext(obs, fromReport),
		Options:     options,
		Blocking:    blocking,
		NeededBy:    obs.NeededBy,
		Origin:      Origin{Issue: obs.Issue, Project: obs.Project},
		Source:      Source{Kind: SourceKindObligation, Ref: obs.ObligationID},
		Authority:   authority,
		State:       StateRaised,
		RaisedAt:    obs.ObservedAt,
		RaisedVia:   RaisedViaObligation,
		Seen:        []Seen{{At: obs.ObservedAt, Source: "event:" + obs.EventID, Note: note}},
	}
	record.Fingerprint = Fingerprint(record.Context, record.Blocking, record.Options)
	if err := ValidateRaise(record); err != nil {
		return Record{}, err
	}
	return record, nil
}

// derivedChoices are the options and the blocking subject of a derived question. A child waiting on
// a blocked_needs_input receipt is answered by a decision reply on its relationship, so the question
// blocks that relationship and each option names the reply it makes. A work report that did not end
// on such a receipt has no reply to give, so its question blocks the issue (else the project) and
// its options name none.
func derivedChoices(obs Observation) ([]Option, []Blocking) {
	if obs.Outcome == OutcomeBlockedNeeds {
		return []Option{
				{ID: ReplyAnswer, Label: "Answer the child", Effect: "The answer is recorded as a decision reply and the child continues in the same generation.", Reply: ReplyAnswer},
				{ID: ReplyStop, Label: "Stop the child", Effect: "A stop is recorded as a decision reply and the child's work ends at this receipt.", Reply: ReplyStop},
			},
			[]Blocking{{Kind: BlockingRelationship, Ref: obs.RelationshipID}}
	}
	options := []Option{
		{ID: "proceed", Label: "Let the work proceed", Effect: "The decision to proceed is recorded for the person who owns the work."},
		{ID: "stop", Label: "Stop the work", Effect: "The decision to stop is recorded for the person who owns the work."},
	}
	if strings.TrimSpace(obs.Issue) != "" {
		return options, []Blocking{{Kind: BlockingIssue, Ref: obs.Issue}}
	}
	return options, []Blocking{{Kind: BlockingProject, Ref: obs.Project}}
}

// derivedContext states the question from facts that are the same each time the obligation is
// observed (relationship, generation, status, reason, summary), so a repeated observation is the same
// fingerprint. The format refuses the fingerprint's delimiter and control characters in a context
// (a newline and a tab excepted), so those are replaced rather than carried.
func derivedContext(obs Observation, fromReport bool) string {
	statement := strings.TrimSpace(obs.Summary)
	if statement == "" {
		statement = "the turn ended " + obs.Outcome
	}
	head := fmt.Sprintf("The child of relationship %s stopped at generation %d on a %s receipt and waits for a decision", obs.RelationshipID, obs.Generation, OutcomeBlockedNeeds)
	if fromReport {
		head = fmt.Sprintf("The child of relationship %s reported %s at generation %d", obs.RelationshipID, obs.ReportStatus, obs.Generation)
	}
	if reason := strings.TrimSpace(obs.Reason); reason != "" {
		head += " (" + reason + ")"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == '|':
			return '/'
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0x7f:
			return ' '
		}
		return r
	}, head+": "+statement)
}
