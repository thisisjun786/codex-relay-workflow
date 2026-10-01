package routing

import (
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// ResolveProduct never files a defect under the observer's product by default.
func ResolveProduct(registries map[string]Object, incident Object) (any, string) {
	declared, repository := text(incident["product"]), text(incident["repository"])
	owners := []string{}
	for key, r := range registries {
		if repository != "" && contains(r["repositories"], repository) {
			owners = append(owners, key)
		}
	}
	slices.Sort(owners)
	if declared != "" {
		if _, ok := registries[declared]; !ok {
			return nil, declared + " is not a registered product"
		}
		if repository != "" && len(owners) > 0 && !slices.Contains(owners, declared) {
			return nil, fmt.Sprintf("%s was declared, but repository %s is registered to %s", declared, repository, pyvalue.Repr(owners))
		}
		return declared, declared + " was declared by the source"
	}
	if repository != "" {
		if len(owners) == 1 {
			return owners[0], fmt.Sprintf("repository %s is registered to %s", repository, owners[0])
		}
		if len(owners) > 0 {
			return nil, fmt.Sprintf("repository %s is registered to several products: %s", repository, pyvalue.Repr(owners))
		}
		return nil, fmt.Sprintf("repository %s is registered to no product", repository)
	}
	return nil, "the incident names neither a product nor a repository"
}

// WorkspaceFor keeps pending records in their declared tenant, never another product's.
func WorkspaceFor(incident, registry Object) (string, error) {
	declared := text(incident["workspace"])
	if registry == nil {
		if declared != "" {
			return declared, nil
		}
		return "unassigned", nil
	}
	if declared != "" && declared != registry["workspace"] {
		return "", malformed(fmt.Sprintf("%s files in workspace %s; the incident declares %s", registry["product"], pyvalue.Quote(registry["workspace"]), pyvalue.Quote(declared)))
	}
	return text(registry["workspace"]), nil
}

func DefectSignature(incident Object, attached any) Object {
	signature := Object{"component": incident["component"], "symptom": incident["symptom"]}
	if attached != nil && attached != "" {
		signature["issue"] = attached
	}
	if incident["origin"] == "simulated" {
		signature["simulated"] = true
	}
	return signature
}
func PendingSignature(incident Object) Object {
	signature := Object{"surface": incident["surface"], "component": incident["component"], "symptom": incident["symptom"]}
	for _, k := range []string{"product", "repository"} {
		if incident[k] != nil && incident[k] != "" {
			signature[k] = incident[k]
		}
	}
	if incident["origin"] == "simulated" {
		signature["simulated"] = true
	}
	return signature
}
func decision(disposition string, project, owner, hold any, relate []any, reopen bool, reason string) Object {
	stage := "filed"
	if disposition == "held" {
		stage = "held"
	}
	if disposition == "observe" {
		stage = "observed"
	}
	if relate == nil {
		relate = []any{}
	}
	return Object{"disposition": disposition, "stage": stage, "project": project, "owner": owner, "hold": hold, "relate": relate, "reopen": reopen, "reason": reason}
}
func owned(disposition string, binding, registry, incident Object, reason string, reopen bool, relate []any) Object {
	project := binding["project"]
	if project == nil {
		project = registry["triageProject"]
	}
	if incident["origin"] == "simulated" {
		project = object(registry["testTarget"])["project"]
	}
	if project == nil {
		return decision("held", nil, binding["ref"], "owner_project_missing", nil, false, fmt.Sprintf("%s owns this but belongs to no project and %s has no triage project", binding["ref"], registry["product"]))
	}
	return decision(disposition, project, binding["ref"], nil, relate, reopen, reason)
}
func openState(state any) bool { return state == "open" || state == "in_progress" }
func refs(bindings []Object) []any {
	out := make([]any, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, b["ref"])
	}
	return out
}

// Decide is placement.decide over validated readings. The run owner comes from the relay,
// not the incident's claim. Input objects are never changed.
func Decide(incident, registry Object, bindings []Object, runIssue any) Object {
	simulated := incident["origin"] == "simulated"
	usable := []Object{}
	issues := map[string]Object{}
	for _, b := range bindings {
		if b["test"] == simulated {
			usable = append(usable, b)
			if b["kind"] == "issue" {
				issues[text(b["ref"])] = b
			}
		}
	}
	component, symptom := incident["component"], incident["symptom"]
	if incident["expected"] != nil {
		return decision("observe", nil, nil, nil, nil, false, fmt.Sprintf("an expected state (%s) is recorded for an operator and never filed", incident["expected"]))
	}
	same := []Object{}
	keys := make([]string, 0, len(issues))
	for k := range issues {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, ref := range keys {
		b := issues[ref]
		if contains(b["components"], component) && contains(b["symptoms"], symptom) {
			same = append(same, b)
		}
	}
	notes := []string{}
	attached := currentIssue(incident, issues, runIssue, &notes)
	if attached != nil {
		others := []any{}
		for _, b := range same {
			if b["ref"] != attached["ref"] {
				others = append(others, b["ref"])
			}
		}
		reason := fmt.Sprintf("failure evidence for the current issue %s, from its own managed run", attached["ref"])
		if len(others) > 0 {
			reason += fmt.Sprintf("; linked to %s, which own the same symptom", pyvalue.Repr(others))
		}
		return owned("attach_current", attached, registry, incident, reason, false, others)
	}
	opens, done := []Object{}, []Object{}
	for _, b := range same {
		if openState(b["state"]) {
			opens = append(opens, b)
		}
		if b["state"] == "done" {
			done = append(done, b)
		}
	}
	if len(opens) == 1 {
		return owned("accumulate", opens[0], registry, incident, fmt.Sprintf("%s is open for the same component and symptom", opens[0]["ref"]), false, nil)
	}
	if len(opens) > 1 {
		return decision("held", nil, nil, "ambiguous_owner", nil, false, "several open issues claim this symptom: "+pyvalue.Repr(refs(opens)))
	}
	if len(done) == 1 {
		return owned("reopen", done[0], registry, incident, fmt.Sprintf("%s was completed and the same defect came back", done[0]["ref"]), true, nil)
	}
	if len(done) > 1 {
		return decision("held", nil, nil, "ambiguous_owner", nil, false, "several completed issues claim this symptom: "+pyvalue.Repr(refs(done)))
	}
	relate := []any{}
	disposition := "new_issue"
	regression := object(incident["context"])["regressionOf"]
	if regression != nil {
		fixed := []Object{}
		for _, ref := range keys {
			b := issues[ref]
			if b["state"] == "done" && b["fixRef"] == regression {
				fixed = append(fixed, b)
			}
		}
		if len(fixed) == 1 {
			relate = append(relate, fixed[0]["ref"])
			disposition = "follow_up"
			notes = append(notes, fmt.Sprintf("a regression of %s, which closed %s", regression, fixed[0]["ref"]))
		} else {
			notes = append(notes, fmt.Sprintf("regressionOf %s names no single completed issue, so nothing is linked without evidence", pyvalue.Repr(regression)))
		}
	}
	project, hold, why := projectFor(incident, registry, usable)
	reason := strings.Join(append(notes, why), "; ")
	if hold != nil {
		return decision("held", nil, nil, hold, relate, false, reason)
	}
	return decision(disposition, project, nil, nil, relate, false, reason)
}
func currentIssue(incident Object, issues map[string]Object, runIssue any, notes *[]string) Object {
	current := object(incident["context"])["currentIssue"]
	if current == nil {
		return nil
	}
	surface := incident["surface"]
	note := ""
	b := issues[text(current)]
	switch {
	case surface != "dev_run" && surface != "verification":
		note = fmt.Sprintf("a %s incident is never attached to a current issue", surface)
	case runIssue == nil:
		note = fmt.Sprintf("no managed run recorded here belongs to %s, so nothing is attached to it", current)
	case runIssue != current:
		note = fmt.Sprintf("the run belongs to %s, not %s, so nothing is attached", runIssue, current)
	case b == nil:
		note = fmt.Sprintf("the current issue %s is not bound to this product", current)
	case !openState(b["state"]):
		note = fmt.Sprintf("the current issue %s is %s", current, b["state"])
	case !contains(b["components"], incident["component"]):
		note = fmt.Sprintf("%s is outside the current issue's scope", incident["component"])
	default:
		return b
	}
	*notes = append(*notes, note)
	return nil
}
func projectFor(incident, registry Object, bindings []Object) (any, any, string) {
	if incident["origin"] == "simulated" {
		return object(registry["testTarget"])["project"], nil, "simulated: the test target project"
	}
	component := incident["component"]
	covering := []Object{}
	for _, b := range bindings {
		if b["kind"] == "project" && b["state"] == "active" && contains(b["components"], component) {
			covering = append(covering, b)
		}
	}
	slices.SortFunc(covering, func(a, b Object) int { return strings.Compare(text(a["ref"]), text(b["ref"])) })
	if len(covering) == 1 {
		return covering[0]["ref"], nil, fmt.Sprintf("%s covers %s", covering[0]["ref"], component)
	}
	goal := object(incident["goal"])["key"]
	if len(covering) > 1 {
		chosen := []Object{}
		for _, b := range covering {
			if goal != nil && b["goal"] == goal {
				chosen = append(chosen, b)
			}
		}
		if len(chosen) == 1 {
			return chosen[0]["ref"], nil, fmt.Sprintf("%s covers %s and %s", chosen[0]["ref"], component, goal)
		}
		return nil, "ambiguous_project", fmt.Sprintf("several projects cover %s: %s", component, pyvalue.Repr(refs(covering)))
	}
	if registry["triageProject"] != nil {
		return registry["triageProject"], nil, "no project covers it; the triage project holds it until a project owns it"
	}
	return nil, "no_project", fmt.Sprintf("no project covers %s and %s has no triage project", component, registry["product"])
}
func IssueLabels(incident Object) []any {
	if incident["repository"] != nil {
		return []any{incident["repository"]}
	}
	return []any{}
}
func DetailText(incident Object) string {
	lines := []string{fmt.Sprintf("surface: %s  phase: %s  origin: %s", incident["surface"], incident["phase"], incident["origin"])}
	d := object(incident["detail"])
	for _, pair := range [][2]string{{"impact", "impact"}, {"expected", "expected"}, {"actual", "actual"}, {"reproduction", "reproduce with"}, {"owner", "current owner"}, {"nextAction", "next action"}} {
		if d[pair[0]] != nil {
			lines = append(lines, pair[1]+": "+text(d[pair[0]]))
		}
	}
	lines = append(lines, "execution: not approved. Filing this starts no work; approval, assignment, the fix and reverification are separate steps.")
	return strings.Join(lines, "\n")
}
