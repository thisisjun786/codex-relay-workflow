package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const standingLimits = "derived from this store's rows only. It says what is owed upward, never that a supervisor received anything, and never that the project is complete. A turn that ended without reporting has no row here at all, so it is present only when a reading of it was passed in: one taken through the marker (reporting-show), or one this store derives from the declarations a child's relay recorded here (omitted.derive), which the supervisor channel and supervisor-standing pass in themselves"

func (c *Channel) Standing(ctx context.Context, project string, observations []any) (map[string]any, error) {
	relations, err := c.Store.All(ctx, "SELECT r.relationship_id,r.status,r.superseded_by FROM relationships r JOIN relationship_scope s ON s.relationship_id = r.relationship_id WHERE s.project_key = ? ORDER BY r.created_at", project)
	if err != nil {
		return nil, err
	}
	names := []any{}
	statuses := map[string]store.Row{}
	for _, row := range relations {
		id := row.Get("relationship_id").(string)
		names = append(names, id)
		statuses[id] = row
	}
	standing := []any{}
	gaps := []any{}
	seen := map[string]bool{}
	carry := func(gap map[string]any) {
		raw, _ := json.Marshal(gap)
		for _, g := range gaps {
			other, _ := json.Marshal(g)
			if string(raw) == string(other) {
				return
			}
		}
		gaps = append(gaps, gap)
	}
	add := func(o *Obligation, status any, superceded any) error {
		if seen[o.ID] {
			return nil
		}
		decided, err := c.Selection(ctx, *o, "", nil)
		if err != nil {
			return err
		}
		if decided["standing"] != "standing" {
			return nil
		}
		seen[o.ID] = true
		raw, _ := json.Marshal(o)
		var entry map[string]any
		if err = json.Unmarshal(raw, &entry); err != nil {
			return err
		}
		entry["basis"] = o.Basis
		entry["executionGeneration"] = o.Generation
		entry["decision"] = decided
		entry["relationshipStatus"] = status
		if o.Kind != "unreported" {
			entry["supersededBy"] = superceded
		}
		standing = append(standing, entry)
		return nil
	}
	for _, idAny := range names {
		id := idAny.(string)
		about := statuses[id]
		events, err := c.Store.All(ctx, "SELECT event_id FROM events WHERE relationship_id = ? ORDER BY first_seen_at", id)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			o, err := c.FromEvent(ctx, event.Get("event_id").(string))
			if err != nil {
				return nil, err
			}
			if o == nil {
				continue
			}
			if err = add(o, about.Get("status"), about.Get("superseded_by")); err != nil {
				return nil, err
			}
		}
	}
	for _, value := range observations {
		reading, ok := value.(map[string]any)
		if !ok {
			carry(unusableReportReading(value))
			continue
		}
		scope := reading["relationshipId"]
		if scope != nil && !reportNamed(scope) {
			carry(unusableReportReading(reading))
			continue
		}
		if s, ok := scope.(string); ok && strings.TrimSpace(s) != "" {
			if _, exists := statuses[s]; !exists {
				continue
			}
		}
		if reading["schema"] != "reporting-observation/1" || !validReportState(reading["reportingState"]) {
			carry(unusableReportReading(reading))
			continue
		}
		switch reading["reportingState"] {
		case "reported", "in_progress", "unmanaged":
			continue
		case "unmeasured":
			carry(map[string]any{"schema": "supervisor-obligation/1", "gap": "reporting_unmeasured", "relationId": scope, "reason": reading["reason"], "detail": "nothing was established about whether a report was owed here"})
			continue
		}
		if reading["owed"] == false {
			continue
		}
		o := ObservationObligation(reading)
		if o == nil {
			carry(unusableReportReading(reading))
			continue
		}
		var status any
		if row, ok := statuses[o.RelationID]; ok {
			status = row.Get("status")
		}
		if err := add(o, status, nil); err != nil {
			return nil, err
		}
	}
	return map[string]any{"schema": "supervisor-obligation/1", "projectKey": project, "relations": names, "standing": standing, "gaps": gaps, "limits": standingLimits}, nil
}

func projectStateValue(value any) any {
	switch v := value.(type) {
	case contract.OrderedObject:
		out := make(map[string]any, len(v))
		for _, field := range v {
			out[field.Key] = projectStateValue(field.Value)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = projectStateValue(item)
		}
		return out
	default:
		return value
	}
}

// ReportHolds names claimable reports whose live hierarchy currently refuses them.
func (c *Channel) ReportHolds(ctx context.Context, standing map[string]any) ([]map[string]any, error) {
	obligations, _ := standing["standing"].([]any)
	newest := map[string]string{}
	rows, err := c.Store.All(ctx, "SELECT obligation_id,state FROM supervisor_messages ORDER BY staged_at")
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		newest[row.Get("obligation_id").(string)] = row.Get("state").(string)
	}
	waiting := map[string][]string{}
	order := []string{}
	for _, raw := range obligations {
		o, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := o["obligationId"].(string)
		state, staged := newest[id]
		if !staged {
			d, _ := o["decision"].(map[string]any)
			if d["report"] != true {
				continue
			}
		} else if !store.SupervisorUnsent(state) {
			continue
		}
		rid, _ := o["relationId"].(string)
		if _, ok := waiting[rid]; !ok {
			order = append(order, rid)
		}
		waiting[rid] = append(waiting[rid], id)
	}
	holds := []map[string]any{}
	for _, rid := range order {
		_, err := c.Resolve(ctx, rid)
		var refusal Refusal
		if errors.As(err, &refusal) {
			holds = append(holds, map[string]any{"schema": "supervisor-obligation/1", "gap": "report_held", "relationId": rid, "obligationIds": waiting[rid], "reason": refusal.Reason, "detail": refusal.Detail})
		} else if err != nil {
			return nil, err
		}
	}
	return holds, nil
}

func (c *Channel) projectAssignmentState(ctx context.Context, project string) (map[string]any, error) {
	state := (&registry.Registry{Store: c.Store}).ProjectState(ctx, project)
	return projectStateValue(state).(map[string]any), nil
}
func (c *Channel) StatusAnswer(ctx context.Context, project string, observations []any) (map[string]any, error) {
	answer, err := c.Standing(ctx, project, observations)
	if err != nil {
		return nil, err
	}
	answer["answeredBecause"] = "an explicit status request is a separate path from automatic notification; it is answered whether or not a wake would have been suppressed"
	state, err := c.projectAssignmentState(ctx, project)
	if err != nil {
		return nil, err
	}
	answer["projectState"] = state
	return answer, nil
}
