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

// Standing is the project's whole answer: every relationship attached to it, archived ones
// included, and every obligation the events of those raise. It is what supervisor-standing prints
// and supervisor-stage --project stages, and the documentation says it keeps listing a report
// still owed for an archived assignment.
func (c *Channel) Standing(ctx context.Context, project string, observations []any) (map[string]any, error) {
	return c.standing(ctx, project, observations, false)
}

// standing is Standing. With visit it is what the daemon's visit to the project needs: the
// events of released relationships are not read, so their reports (which nothing can be addressed
// for) are not raised and the visit costs what the relationships that can still report cost. A
// relationship is released when it is archived and its issue has neither a live owner nor a live
// execution edge above it: archiving releases both, and they are what its report goes up by
// (StoreLinkage.Up starts from the issue). One whose issue was taken again, by its successor or
// by a new registration for the issue, can be addressed again and is not released. The answer is
// otherwise the same: it still lists every relationship, and a reading about a released one is
// still placed by its status.
func (c *Channel) standing(ctx context.Context, project string, observations []any, visit bool) (map[string]any, error) {
	relations, err := c.Store.All(ctx, "SELECT r.relationship_id,r.status,r.superseded_by FROM relationships r JOIN relationship_scope s ON s.relationship_id = r.relationship_id WHERE s.project_key = ? ORDER BY r.created_at", project)
	if err != nil {
		return nil, err
	}
	raised, err := c.projectObligations(ctx, project, visit)
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
		for _, o := range raised[id] {
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

// ReportHolds names claimable reports whose live hierarchy currently refuses them. The hierarchy is
// read once per relationship and grade of its waiting obligations (obligationGrade), so a decision the
// user owes that the seat above still hears is not named held beside a completion that it does not
// (CRW-1486/S4). The obligations of one relationship refused alike are one hold, in standing order.
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
	type waitingObligation struct {
		id    string
		grade Grade
	}
	waiting := map[string][]waitingObligation{}
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
		kind, _ := o["kind"].(string)
		waiting[rid] = append(waiting[rid], waitingObligation{id, obligationGrade(Obligation{Kind: kind})})
	}
	holds := []map[string]any{}
	for _, rid := range order {
		refused := map[Grade]*Refusal{}
		read := map[Grade]bool{}
		for _, one := range waiting[rid] {
			if read[one.grade] {
				continue
			}
			read[one.grade] = true
			_, err := c.ResolveFor(ctx, rid, one.grade)
			var refusal Refusal
			if errors.As(err, &refusal) {
				refused[one.grade] = &refusal
			} else if err != nil {
				return nil, err
			}
		}
		// One hold per distinct refusal, in the order its first obligation stands.
		var held []map[string]any
		for _, one := range waiting[rid] {
			refusal := refused[one.grade]
			if refusal == nil {
				continue
			}
			var hold map[string]any
			for _, existing := range held {
				if existing["reason"] == refusal.Reason && existing["detail"] == refusal.Detail {
					hold = existing
					break
				}
			}
			if hold == nil {
				hold = map[string]any{"schema": "supervisor-obligation/1", "gap": "report_held", "relationId": rid, "obligationIds": []string{}, "reason": refusal.Reason, "detail": refusal.Detail}
				held = append(held, hold)
			}
			hold["obligationIds"] = append(hold["obligationIds"].([]string), one.id)
		}
		holds = append(holds, held...)
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
