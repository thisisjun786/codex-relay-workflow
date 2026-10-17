package compile

import (
	"fmt"
	"regexp"
	"strings"

	flowdelivery "github.com/thisisjun786/codex-relay-workflow/internal/flow/delivery"
)

// This only suggests an annotation. Prose matching never removes a requirement
// from the crosscheck: only valid, explicit delivery_criteria do that.
var possibleDeliveryCriterion = regexp.MustCompile(`(?i)(\bpush(?:ed|ing)?\b.{0,80}\b(?:origin|remote)\b|\b(?:origin|remote)\b.{0,80}\b(?:push(?:ed|ing)?|integrat(?:ed|ion)|branch)\b|\b(?:issues?|milestones?)\b.{0,60}\bclos(?:ed|e|ure)\b|(?:이슈|마일스톤).{0,40}(?:닫|종료)|origin.{0,80}(?:통합|반영))`)

// DeliveryWarnings are shared by compile and start. The latter selects the
// milestone being started; an empty selector covers every milestone.
func DeliveryWarnings(p Project, milestone string) []Warning {
	var warnings []Warning
	for i, m := range p.Milestones {
		if milestone != "" && m.Key != milestone {
			continue
		}
		path := fmt.Sprintf("milestones[%d].delivery_criteria", i)
		valid, problems := flowdelivery.Valid(m.Criteria, m.DeliveryCriteria)
		marked := map[string]bool{}
		var ids []string
		for _, criterion := range valid {
			marked[criterion.ID] = true
			ids = append(ids, criterion.ID)
		}
		if len(ids) > 0 {
			warnings = append(warnings, Warning{
				Project: p.Key, Rule: "delivery_after_publish", Path: path,
				Detail: fmt.Sprintf("milestone %s criteria %s are not judged by the pre-publication crosscheck; after publication read them back with crw flow audit --run RUN --delivery", m.Key, strings.Join(ids, ", ")),
			})
		}
		for _, problem := range problems {
			warnings = append(warnings, Warning{Project: p.Key, Rule: "delivery_criteria_invalid", Path: path, Detail: problem})
		}
		for j, text := range m.Criteria {
			id := fmt.Sprintf("c%d", j+1)
			if marked[id] || !possibleDeliveryCriterion.MatchString(text) {
				continue
			}
			warnings = append(warnings, Warning{
				Project: p.Key, Rule: "delivery_criterion_unmarked", Path: fmt.Sprintf("milestones[%d].criteria[%d]", i, j),
				Detail: fmt.Sprintf("milestone %s criterion %s may describe post-publication delivery; mark it in delivery_criteria only if the whole criterion is a delivery condition, and split mixed functional and delivery requirements. It remains in the build crosscheck without a valid annotation", m.Key, id),
			})
		}
	}
	return warnings
}
