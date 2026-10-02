package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// AutoSendResult is the supervisor-owned portion of one daemon tick.
type AutoSendResult struct {
	SupervisorStaged int      `json:"supervisorStaged"`
	SupervisorSent   int      `json:"supervisorSent"`
	Deferred         int      `json:"deferred"`
	Skipped          int      `json:"skipped"`
	Notes            []string `json:"notes"`
	AfterProject     string   `json:"-"`
	AfterStagedAt    string   `json:"-"`
	AfterMessageID   string   `json:"-"`
	Attempts         int      `json:"-"`
}

// AutoSend performs only the supervisor pass invoked by a daemon tick. Todo 29 owns
// cadence, service lifecycle, and persistence of the in-memory rotation cursors.
func (c *Channel) AutoSend(ctx context.Context, host SendAdapter, now float64, projectLimit, sendLimit int, afterProject, afterStagedAt, afterMessageID string) (AutoSendResult, error) {
	answer := AutoSendResult{Notes: []string{}, AfterProject: afterProject, AfterStagedAt: afterStagedAt, AfterMessageID: afterMessageID}
	projects, err := c.autoProjects(ctx, projectLimit, afterProject)
	if err != nil {
		answer.Notes = append(answer.Notes, "supervisor pass could not list projects: "+err.Error())
		return answer, nil
	}
	for _, project := range projects {
		answer.AfterProject = project
		staged, err := c.stageUnsent(ctx, project, delivery.ISOOf(now))
		if err != nil {
			answer.Notes = append(answer.Notes, fmt.Sprintf("supervisor staging failed for %s: %v", project, err))
			continue
		}
		answer.SupervisorStaged += staged
	}
	rows, err := c.autoHeads(ctx, now, sendLimit*4, afterStagedAt, afterMessageID)
	if err != nil {
		return answer, err
	}
	struggling := map[string]bool{}
	attempted := 0
	for _, row := range rows {
		if attempted >= sendLimit {
			break
		}
		answer.AfterStagedAt, answer.AfterMessageID = row.StagedAt, row.MessageID
		if struggling[row.RecipientTaskID] {
			answer.Skipped++
			continue
		}
		before, beforeErr := c.transportStarts(ctx, row.MessageID)
		record, err := c.attempt(ctx, row.MessageID, host, now, 0, "relay-daemon")
		if err != nil {
			after, countErr := c.transportStarts(ctx, row.MessageID)
			if beforeErr != nil || countErr != nil || after != before {
				attempted++
			}
			detail := err.Error()
			var refusal Refusal
			if errors.As(err, &refusal) {
				detail = refusal.Reason + ": " + refusal.Detail
			}
			answer.Notes = append(answer.Notes, fmt.Sprintf("supervisor report %s not sent: %s", row.MessageID, detail))
			answer.Deferred++
			struggling[row.RecipientTaskID] = true
			if deferErr := c.deferAutoFault(ctx, row.MessageID, now, err); deferErr != nil {
				answer.Notes = append(answer.Notes, fmt.Sprintf("supervisor report %s not deferred: %v", row.MessageID, deferErr))
			}
			continue
		}
		if record == nil {
			answer.Deferred++
			struggling[row.RecipientTaskID] = true
			continue
		}
		attempted++
		if record["deliveryState"] == "dispatched" {
			answer.SupervisorSent++
		} else {
			answer.Deferred++
			struggling[row.RecipientTaskID] = true
		}
	}
	answer.Attempts = attempted
	return answer, nil
}

// deferAutoFault mirrors the channel's defer_after_fault after an unclassified
// attempt failure. A hierarchy hold or a sending lease is already its own answer.
func (c *Channel) deferAutoFault(ctx context.Context, id string, now float64, fault error) error {
	row, err := c.Get(ctx, id)
	if err != nil {
		return err
	}
	if row.HoldReason.Valid || !row.Unsent() {
		return nil
	}
	at := delivery.ISOOf(now)
	if c.clockISO != nil {
		at = c.clockISO()
	}
	label := "Exception: " + fault.Error()
	var refusal Refusal
	if errors.As(fault, &refusal) {
		label = "DeliveryRefused: " + refusal.Reason + ": " + refusal.Detail
	}
	var host *delivery.HostError
	if errors.As(fault, &host) {
		label = host.Kind + ": " + host.Message
	}
	when := now + delivery.DefaultPolicy().LifecycleRecheck
	return c.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		changed, err := c.Store.Q(tx).ExecContext(tx, "UPDATE supervisor_messages SET next_eligible_at=?,updated_at=? WHERE message_id=? AND state=? AND hold_reason IS NULL AND next_eligible_at IS ? AND recipient_task_id=? AND attempt_count=?", when, at, id, row.State, row.NextEligibleAt, row.RecipientTaskID, row.AttemptCount)
		if err != nil {
			return err
		}
		n, err := changed.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		detail := pyjson.Dumps(contract.OrderedObject{{Key: "error", Value: label}, {Key: "retryAt", Value: when}}, pyjson.Options{})
		_, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_attempt_faulted',?,?)", at, id, detail)
		return err
	})
}

func (c *Channel) autoProjects(ctx context.Context, limit int, after string) ([]string, error) {
	if limit <= 0 {
		return []string{}, nil
	}
	base := "SELECT DISTINCT project_key FROM relationship_scope WHERE project_key IS NOT NULL"
	rows, err := c.Store.All(ctx, base+" AND project_key > ? ORDER BY project_key LIMIT ?", after, limit)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, limit)
	seen := map[string]bool{}
	for _, row := range rows {
		project := row.Get("project_key").(string)
		seen[project] = true
		out = append(out, project)
	}
	if len(out) < limit && after != "" {
		rows, err = c.Store.All(ctx, base+" AND project_key <= ? ORDER BY project_key LIMIT ?", after, limit-len(out))
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			project := row.Get("project_key").(string)
			if !seen[project] {
				out = append(out, project)
				seen[project] = true
			}
		}
	}
	return out, nil
}

func (c *Channel) OmissionReadings(ctx context.Context, project, at string, grace float64) ([]map[string]any, error) {
	return c.OmissionReadingsExcept(ctx, project, at, grace, nil)
}

func (c *Channel) OmissionReadingsExcept(ctx context.Context, project, at string, grace float64, supplied []map[string]any) ([]map[string]any, error) {
	covered := map[string]bool{}
	for _, reading := range supplied {
		if o := ObservationObligation(reading); o != nil {
			covered[o.ID] = true
		}
	}
	ids, err := c.Store.ScopedRelationships(ctx, project)
	if err != nil {
		return nil, err
	}
	readings := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		reading := delivery.DeriveOmission(ctx, c.Store, store.PathlibParent(c.Store.Path), id, "", at, grace)
		if objText(reading, "reportingState") == "unreported" && objBool(reading, "owed") {
			plain := orderedMap(reading)
			if o := ObservationObligation(plain); o != nil && !covered[o.ID] {
				readings = append(readings, plain)
			}
		}
	}
	return readings, nil
}

func (c *Channel) StageUnsent(ctx context.Context, project, at string, grace float64) (map[string]any, error) {
	readings, err := c.OmissionReadings(ctx, project, at, grace)
	if err != nil {
		return nil, err
	}
	return c.stageUnsentWithReadings(ctx, project, at, readings)
}

func (c *Channel) stageUnsentWithReadings(ctx context.Context, project, at string, readings []map[string]any) (map[string]any, error) {
	values := make([]any, len(readings))
	byObligation := make(map[string]map[string]any, len(readings))
	for i, reading := range readings {
		values[i] = reading
		if o := ObservationObligation(reading); o != nil {
			byObligation[o.ID] = reading
		}
	}
	// The visit reads only what can still report (standing's visit scope); a parent staging by hand
	// (StageStanding, supervisor-standing) still gets the whole project.
	standing, err := c.standing(ctx, project, values, true)
	if err != nil {
		return nil, err
	}
	// Match stage_standing's second scope read: autosend's project-wrap cursor observes both
	// omission derivation and the standing/staging pass independently.
	if _, err := c.Store.ScopedRelationships(ctx, project); err != nil {
		return nil, err
	}
	staged, refused := []any{}, []any{}
	skipped := 0
	for _, value := range standing["standing"].([]any) {
		o := obligationFromStanding(value.(map[string]any))
		reading := byObligation[o.ID]
		var state string
		var hold, frozen sql.NullString
		err := c.Store.Q(ctx).QueryRowContext(ctx, "SELECT state,hold_reason,reading FROM supervisor_messages WHERE obligation_id=? ORDER BY staged_at DESC LIMIT 1", o.ID).Scan(&state, &hold, &frozen)
		if err == nil && !(store.SupervisorUnsent(state) && (!hold.Valid || hold.String == "superseded_by_report" || hold.String == "hierarchy_unresolved")) {
			skipped++
			continue
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil && frozen.Valid {
			var retained map[string]any
			decoder := json.NewDecoder(strings.NewReader(frozen.String))
			decoder.UseNumber()
			if err := decoder.Decode(&retained); err != nil {
				return nil, err
			}
			if generation, ok := retained["executionGeneration"].(json.Number); ok {
				n, err := generation.Int64()
				if err != nil {
					return nil, err
				}
				retained["executionGeneration"] = n
			}
			reading = retained
		}
		if raised := ObservationObligation(reading); raised != nil {
			o = *raised
		}
		answer, err := c.StageWithReading(ctx, o, reading, "", at)
		if refusal := new(Refusal); errors.As(err, refusal) {
			refused = append(refused, map[string]any{"obligationId": o.ID, "kind": o.Kind, "reason": refusal.Reason, "detail": refusal.Detail})
		} else if err != nil {
			return nil, err
		} else {
			staged = append(staged, answer)
		}
	}
	return map[string]any{"schema": channelVersion, "projectKey": project, "staged": staged, "refused": refused, "skipped": skipped, "gaps": standing["gaps"]}, nil
}
func (c *Channel) stageUnsent(ctx context.Context, project, at string) (int, error) {
	answer, err := c.StageUnsent(ctx, project, at, 300)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, value := range answer["staged"].([]any) {
		stage := value.(StageResult)
		if stage["staged"] == true || stage["readdressed"] == true || stage["restated"] == true {
			count++
		}
	}
	return count, nil
}

func orderedMap(o delivery.Obj) map[string]any {
	out := map[string]any{}
	for _, f := range o {
		out[f.Key] = plainDeliveryValue(f.Value)
	}
	return out
}
func plainDeliveryValue(value any) any {
	switch v := value.(type) {
	case delivery.Obj:
		return orderedMap(v)
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = plainDeliveryValue(v[i])
		}
		return out
	default:
		return value
	}
}
func objText(o delivery.Obj, key string) string {
	for _, f := range o {
		if f.Key == key {
			s, _ := f.Value.(string)
			return s
		}
	}
	return ""
}
func objBool(o delivery.Obj, key string) bool {
	for _, f := range o {
		if f.Key == key {
			b, _ := f.Value.(bool)
			return b
		}
	}
	return false
}

func (c *Channel) autoHeads(ctx context.Context, now float64, limit int, afterAt, afterID string) ([]store.SupervisorMessagesRow, error) {
	if limit <= 0 {
		return []store.SupervisorMessagesRow{}, nil
	}
	// Select heads before applying the window: one recipient's backlog must not
	// consume the page, and an expired sending lease is recovered by Attempt.
	eligible := store.SupervisorAttemptableSQL
	heads := "SELECT m.message_id,m.recipient_task_id,m.staged_at FROM supervisor_messages m WHERE " + eligible("m") +
		" AND NOT EXISTS (SELECT 1 FROM supervisor_messages o WHERE o.recipient_task_id=m.recipient_task_id AND " + eligible("o") +
		" AND (o.staged_at<m.staged_at OR (o.staged_at=m.staged_at AND o.message_id<m.message_id)))"
	read := func(predicate string, args ...any) ([]store.Row, error) {
		return c.Store.All(ctx, heads+predicate+" ORDER BY m.staged_at,m.message_id LIMIT ?", append([]any{now, now, now, now}, args...)...)
	}
	var rows []store.Row
	var err error
	if afterAt == "" {
		rows, err = read("", limit)
	} else {
		rows, err = read(" AND (m.staged_at>? OR (m.staged_at=? AND m.message_id>?))", afterAt, afterAt, afterID, limit)
		if err == nil && len(rows) < limit {
			var wrapped []store.Row
			wrapped, err = read(" AND (m.staged_at<? OR (m.staged_at=? AND m.message_id<=?))", afterAt, afterAt, afterID, limit-len(rows))
			seen := map[string]bool{}
			for _, row := range rows {
				seen[row.Get("message_id").(string)] = true
			}
			for _, row := range wrapped {
				if !seen[row.Get("message_id").(string)] {
					rows = append(rows, row)
				}
			}
		}
	}
	if err != nil {
		return nil, err
	}
	out := make([]store.SupervisorMessagesRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.SupervisorMessagesRow{MessageID: row.Get("message_id").(string), RecipientTaskID: row.Get("recipient_task_id").(string), StagedAt: row.Get("staged_at").(string)})
	}
	return out, nil
}

func (c *Channel) transportStarts(ctx context.Context, id string) (int, error) {
	var n int
	err := c.Store.Q(ctx).QueryRowContext(ctx, "SELECT COUNT(*) FROM supervisor_attempts WHERE message_id=? AND transport_started_at IS NOT NULL", id).Scan(&n)
	return n, err
}
