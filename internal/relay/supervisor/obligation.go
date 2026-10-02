package supervisor

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Obligation is the fact derived from an event; a caller cannot invent a report by
// handing Stage a different copy of one of these fields.
type Obligation struct {
	Schema     string         `json:"schema"`
	ID         string         `json:"obligationId"`
	Kind       string         `json:"kind"`
	RelationID string         `json:"relationId"`
	Subject    string         `json:"subject"`
	Generation any            `json:"executionGeneration"`
	Revision   *string        `json:"revisionHash"`
	Issue      *string        `json:"issueKey"`
	Basis      map[string]any `json:"basis"`
	Detail     string         `json:"detail"`
}

type workReportFact struct {
	status, reason, summary string
	submission              int64
}

func currentReport(ctx context.Context, s *store.Store, eventID string) (workReportFact, error) {
	var r workReportFact
	err := s.Q(ctx).QueryRowContext(ctx, "SELECT cxc_status, cxc_reason, summary, submission_no FROM work_reports WHERE event_id = ? ORDER BY submission_no DESC LIMIT 1", eventID).Scan(&r.status, &r.reason, &r.summary, &r.submission)
	if errors.Is(err, sql.ErrNoRows) {
		return workReportFact{}, nil
	}
	return r, err
}

func hash32(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])[:32]
}

// eventFacts is what one event, its newest work report and its relationship's issue say: the
// input of the judgment whichever way they were read, one event at a time (FromEvent) or a
// project's at once (projectObligations).
type eventFacts struct {
	eventID, relation, revision, outcome, producer, stage string
	generation                                            int64
	suppressed                                            sql.NullString
	report                                                workReportFact
	issue                                                 sql.NullString
}

// eventRaises is whether an event can raise an obligation at all: a final, unsuppressed fact of
// a child or of the daemon's observation of one. projectEventsSQL selects on exactly this.
func eventRaises(stage string, suppressed sql.NullString, producer string) bool {
	return stage == "final" && !suppressed.Valid && (producer == "child" || producer == "daemon_observation")
}

// obligationKind is what a work-report status, else the outcome the turn ended with, makes of an
// event: a completion, a block, a decision only the user can make, or nothing.
func obligationKind(reportStatus, outcome string) string {
	switch reportStatus {
	case "DONE", "NOOP":
		return "completion"
	case "BLOCKED", "BUDGET_EXHAUSTED":
		return "blocked"
	case "UNSAFE", "NEEDS_HUMAN":
		return "decision_request"
	}
	switch outcome {
	case "ready_for_review":
		return "completion"
	case "blocked_needs_input":
		return "blocked"
	}
	return ""
}

// obligationFrom is supervision.from_event's judgment on facts already read: a final, unsuppressed
// child fact with an upward outcome or work-report status raises one obligation. Ordinary failure
// does not.
func obligationFrom(f eventFacts) *Obligation {
	if !eventRaises(f.stage, f.suppressed, f.producer) {
		return nil
	}
	kind := obligationKind(f.report.status, f.outcome)
	if kind == "" {
		return nil
	}
	subject := f.eventID
	if kind != "completion" {
		cause := pyjson.Dumps([]string{firstNonempty(f.report.status, f.outcome), f.report.reason, f.report.summary}, pyjson.Options{Compact: true, Unicode: true})
		subject = fmt.Sprintf("g%d:%s", f.generation, hash32(cause)[:16])
	}
	detail := f.report.summary
	if detail == "" {
		detail = "the turn ended " + f.outcome
	}
	var status any
	if f.report.status != "" {
		status = f.report.status
	}
	revision := f.revision
	basis := map[string]any{"table": "events", "eventId": f.eventID, "outcome": f.outcome, "cxcStatus": status}
	result := &Obligation{Schema: "supervisor-obligation/1", ID: hash32(kind + "|" + f.relation + "|" + subject), Kind: kind, RelationID: f.relation, Subject: subject, Generation: f.generation, Revision: &revision, Basis: basis, Detail: detail}
	if f.issue.Valid {
		issue := f.issue.String
		result.Issue = &issue
	}
	return result
}

// FromEvent is supervision.from_event: a final, unsuppressed child fact with an
// upward outcome or work-report status raises one obligation. Ordinary failure does not.
func (c *Channel) FromEvent(ctx context.Context, eventID string) (*Obligation, error) {
	f := eventFacts{eventID: eventID}
	err := c.Store.Q(ctx).QueryRowContext(ctx, "SELECT relationship_id, execution_generation, revision_hash, outcome, producer, stage, suppressed_reason FROM events WHERE event_id = ?", eventID).Scan(&f.relation, &f.generation, &f.revision, &f.outcome, &f.producer, &f.stage, &f.suppressed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !eventRaises(f.stage, f.suppressed, f.producer) {
		return nil, nil
	}
	if f.report, err = currentReport(ctx, c.Store, eventID); err != nil {
		return nil, err
	}
	if obligationKind(f.report.status, f.outcome) == "" {
		return nil, nil
	}
	err = c.Store.Q(ctx).QueryRowContext(ctx, "SELECT issue_key FROM relationships WHERE relationship_id = ?", f.relation).Scan(&f.issue)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return obligationFrom(f), nil
}
func firstNonempty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// PriorReport reads the first produced-report journal entry, not the first send.
func (c *Channel) PriorReport(ctx context.Context, obligationID string) (map[string]any, error) {
	entries, err := c.Store.Journal(ctx, "supervisor_report", obligationID)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	entry := entries[0]
	detail := map[string]any{}
	if entry.Detail != "" {
		if err = json.Unmarshal([]byte(entry.Detail), &detail); err != nil {
			detail = map[string]any{"detail": entry.Detail}
		}
	}
	return map[string]any{"seq": entry.ID, "at": entry.At, "detail": detail}, nil
}

// Reportable checks both independent conditions: the Linear record has not
// discharged the fact and no report of the fact was already produced.
func (c *Channel) Reportable(ctx context.Context, o Obligation) (bool, string, error) {
	prior, err := c.PriorReport(ctx, o.ID)
	if err != nil {
		return false, "", err
	}
	eventID := o.Subject
	if e, ok := o.Basis["eventId"].(string); ok {
		eventID = e
	}
	var target string
	err = c.Store.Q(ctx).QueryRowContext(ctx, "SELECT target_ref FROM sync_targets WHERE relationship_id = ? AND target = 'coordination_document'", o.RelationID).Scan(&target)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, "", err
	}
	if err == nil {
		var state, ref string
		err = c.Store.Q(ctx).QueryRowContext(ctx, "SELECT state, target_ref FROM sync_outbox WHERE relationship_id = ? AND event_id = ? AND subject_kind = 'verdict' AND target = 'coordination_document' ORDER BY rowid DESC LIMIT 1", o.RelationID, eventID).Scan(&state, &ref)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, "", err
		}
		if err == nil && state == "confirmed" && ref == target {
			return false, "already_in_the_record_the_supervisor_reads", nil
		}
	}
	if prior != nil {
		return false, "already_reported_under_this_obligation", nil
	}
	return true, "deliverability_was_not_asked_about", nil
}

func validateObligation(ctx context.Context, c *Channel, o Obligation) error {
	if o.Kind == "unreported" {
		return nil
	}
	eventID, ok := o.Basis["eventId"].(string)
	if !ok {
		return Refusal{"contradictory_observation", "the obligation handed in names no event"}
	}
	derived, err := c.FromEvent(ctx, eventID)
	if err != nil {
		return err
	}
	if derived == nil {
		return Refusal{"contradictory_observation", "the obligation handed in is not the one event " + strconv.Quote(eventID) + " raises: its obligationId differ. The packet and the journal entry would both be composed from it, so nothing was composed or recorded; stage the obligation the event raises"}
	}
	a, _ := json.Marshal(o)
	b, _ := json.Marshal(derived)
	if string(a) != string(b) {
		var given, expected map[string]any
		if err := json.Unmarshal(a, &given); err != nil {
			return err
		}
		if err := json.Unmarshal(b, &expected); err != nil {
			return err
		}
		var drift []string
		for _, field := range []string{"obligationId", "kind", "relationId", "subject", "executionGeneration", "revisionHash", "issueKey", "basis", "detail"} {
			left, _ := json.Marshal(given[field])
			right, _ := json.Marshal(expected[field])
			if string(left) != string(right) {
				drift = append(drift, field)
			}
		}
		return Refusal{"contradictory_observation", "the obligation handed in is not the one event " + strconv.Quote(eventID) + " raises: its " + strings.Join(drift, ", ") + " differ. The packet and the journal entry would both be composed from it, so nothing was composed or recorded; stage the obligation the event raises"}
	}
	return nil
}
