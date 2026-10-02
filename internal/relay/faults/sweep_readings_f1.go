package faults

import (
	"context"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
)

func f1ReadingKey(value any) string {
	r, ok := value.(map[string]any)
	if !ok || r["schema"] != "reporting-observation/1" {
		return ""
	}
	selectors, _ := r["selectors"].(map[string]any)
	rel, _ := r["relationshipId"].(string)
	turn, _ := selectors["turn"].(string)
	if strings.TrimSpace(rel) == "" || strings.TrimSpace(turn) == "" {
		return ""
	}
	return rel + "\x00" + turn
}
func (sw *Sweeper) readingFaults(ctx context.Context, product, project string, readings []any, after int) ([]Observation, []any, any, int, error) {
	if after < 0 || after > 1000 {
		return nil, nil, nil, 0, fmt.Errorf("fault_observation_malformed: readings after is an integer from 0 to 1000, not %d", after)
	}
	if len(readings) > 1000 {
		return nil, nil, nil, 0, fmt.Errorf("fault_observation_malformed: %d readings is more than the 1000 one call reads; hand them in batches", len(readings))
	}
	ordered := []any{}
	keys := []string{}
	for i, r := range readings {
		key := f1ReadingKey(r)
		if key == "" {
			key = fmt.Sprintf("#unusable:%d", i)
		}
		at := -1
		for j, k := range keys {
			if k == key {
				at = j
				break
			}
		}
		if at >= 0 {
			held, _ := ordered[at].(map[string]any)
			incoming, _ := r.(map[string]any)
			if incoming["reportingState"] == "unmeasured" {
				switch held["reportingState"] {
				case "reported", "unreported", "in_progress", "unmanaged":
					continue
				}
			}
			ordered = append(ordered[:at], ordered[at+1:]...)
			keys = append(keys[:at], keys[at+1:]...)
		}
		ordered = append(ordered, r)
		keys = append(keys, key)
	}
	total := len(ordered)
	var next any
	if after+32 < total {
		next = after + 32
	}
	observations := []Observation{}
	gaps := []any{}
	cache := map[string]map[string]any{}
	for i := after; i < total && i < after+32; i++ {
		raw := ordered[i]
		r, ok := raw.(map[string]any)
		if !ok || r["schema"] != "reporting-observation/1" {
			gaps = append(gaps, map[string]any{"gap": "reading_unusable", "reason": "not an object under reporting-observation/1"})
			continue
		}
		key := f1ReadingKey(r)
		if key == "" {
			var rel any
			if name, ok := r["relationshipId"].(string); ok {
				rel = name
			}
			gaps = append(gaps, map[string]any{"gap": "reading_unusable", "relationId": rel, "reason": "the reading names no usable relationship or turn"})
			continue
		}
		rel, turn, _ := strings.Cut(key, "\x00")
		scope := map[string]any{}
		if project != "" {
			scope["projectKey"] = project
		}
		own, e := sw.scopeOf(ctx, rel, cache)
		if e != nil {
			return nil, nil, nil, 0, e
		}
		for k, v := range own {
			scope[k] = v
		}
		state, _ := r["reportingState"].(string)
		signature := map[string]any{"relationship": rel, "turn": turn}
		entry := func(class, severity, occurrence, detail string, cleared bool, evidenceItems []any, identity map[string]any) Observation {
			return Observation{Product: product, FaultClass: class, Severity: severity, Signature: identity, OccurrenceKey: occurrence, Scope: scope, Detail: detail, Evidence: evidenceItems, Cleared: cleared}
		}
		fact := func(expected, actual, impact string, limit string, generation any) map[string]any {
			return sw.facts(expected, actual, impact, []any{limit}, map[string]any{"relationship": rel, "turn": turn, "generation": generation})
		}
		established := state == "reported" || state == "unreported" || state == "in_progress" || state == "unmanaged"
		if established {
			observations = append(observations, entry("observation_unmeasured", Notice, fmt.Sprintf("measured:%s:%s:%s", rel, turn, state), "a later reading of this turn established something", true, []any{evidence("reading", "reporting-observation/1", map[string]any{"reportingState": state})}, signature), entry("observation_unmeasured", Notice, "measured:"+rel, "a later reading of this relationship established something", true, []any{evidence("reading", "reporting-observation/1", map[string]any{"reportingState": state, "turn": turn})}, map[string]any{"relationship": rel}))
		}
		switch state {
		case "unreported":
			// A reading that says nothing is owed for the turn (a later turn was admitted, the turn has its own
			// receipt, or the report grace has not run out) is not an omission; the supervisor's readers skip it the
			// same way. Only an explicit false skips: a reading with no owed field, as the managed observer
			// produces, is filed as before.
			if r["owed"] == false {
				break
			}
			observations = append(observations, entry("report_omitted", Broken, fmt.Sprintf("observation:%s:%s", rel, turn), "an admitted turn settled without a report, so what it owed is owed", false, []any{evidence("reading", "reporting-observation/1", map[string]any{"reportingState": state, "reason": r["reason"], "executionGeneration": r["executionGeneration"]}), fact("an admitted turn settles with its report", "the turn settled without a report", "the level above is not told what the turn produced", "read through the CRW-180 reporting projection", r["executionGeneration"])}, signature))
		case "reported":
			observations = append(observations, entry("report_omitted", Broken, fmt.Sprintf("observation:%s:%s:reported", rel, turn), "a later reading of this turn found its report", true, []any{evidence("reading", "reporting-observation/1", map[string]any{"reportingState": state})}, signature))
		case "unmeasured":
			observations = append(observations, entry("observation_unmeasured", Notice, fmt.Sprintf("unmeasured:%s:%s", rel, turn), "nothing was established about whether this turn owed a report", false, []any{evidence("reading", "reporting-observation/1", map[string]any{"reportingState": state, "reason": r["reason"]}), fact("whether this turn owed a report is established", "unmeasured: "+pyStr(r["reason"]), "nobody can say whether a report is owed for this turn", "a notice: recorded, never filed", nil)}, signature))
		default:
			if !established {
				gaps = append(gaps, map[string]any{"gap": "reading_unknown_state", "relationId": rel, "reason": fmt.Sprintf("reportingState %s is not one this sweep knows", quote.Value(r["reportingState"]))})
			}
		}
	}
	return observations, gaps, next, total, nil
}
