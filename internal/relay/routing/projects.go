package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

var projectPayloadKeys = strings.Fields("product workspace team familyLabel goal criteria name members components")

// ValidateProjectPayload is total over JSON and is shared by queue and pre-issue callers.
func ValidateProjectPayload(value any) []string {
	payload, ok := value.(map[string]any)
	if !ok {
		return []string{"a project_create payload is an object"}
	}
	problems := []string{}
	unknown := []string{}
	for key := range payload {
		if !slices.Contains(projectPayloadKeys, key) {
			unknown = append(unknown, key)
		}
	}
	slices.Sort(unknown)
	if len(unknown) > 0 {
		problems = append(problems, "payload carries keys this kind does not define: "+evidence.Repr(unknown))
	}
	for _, key := range projectPayloadKeys {
		v := payload[key]
		if key == "members" || key == "components" {
			values, ok := evidence.List(v)
			valid := ok && len(values) > 0
			for _, item := range values {
				name, stringValue := item.(string)
				if !stringValue || strings.TrimSpace(name) == "" {
					valid = false
				}
			}
			if !valid {
				problems = append(problems, "payload."+key+" is a non-empty list of names")
			}
		} else if name, ok := v.(string); !ok || strings.TrimSpace(name) == "" {
			problems = append(problems, "payload."+key+" is a non-blank string")
		}
	}
	if members, ok := evidence.List(payload["members"]); ok {
		stringsOnly := true
		unique := map[string]bool{}
		for _, m := range members {
			s, ok := m.(string)
			if !ok {
				stringsOnly = false
			}
			unique[s] = true
		}
		if stringsOnly {
			if len(unique) != len(members) {
				problems = append(problems, "payload.members names a fix twice")
			} else if len(members) < 2 {
				problems = append(problems, "a project needs at least two independent fixes")
			}
		}
	}
	return problems
}

// ConfirmProject requires a positive readback of the team, not merely the create block.
func ConfirmProject(expected, observed any) []string {
	payload := object(object(expected)["payload"])
	team := object(observed)["team"]
	if team == nil || team == "" || team == false {
		return []string{"the readback does not show which team the project was created in"}
	}
	if team != payload["team"] {
		return []string{fmt.Sprintf("the project was created in %s, not %s", evidence.Repr(team), evidence.Repr(payload["team"]))}
	}
	return []string{}
}

func readStored(value any) (Object, error) {
	decoder := json.NewDecoder(strings.NewReader(text(value)))
	decoder.UseNumber()
	var out Object
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
func latestIncident(ctx context.Context, s *store.Store, fault string) (Object, error) {
	row, err := s.One(ctx, "SELECT record FROM route_incidents WHERE fault_id = ? ORDER BY recorded_seq DESC LIMIT 1", fault)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return Object{}, nil
	}
	return readStored(row.Get("record"))
}

// ProjectEligibility rereads the predicate on the caller's transaction connection. It takes
// validated payloads, as projects.eligibility does; pre-issue callers validate first.
func ProjectEligibility(ctx context.Context, s *store.Store, payload Object) ([]string, error) {
	row, err := s.One(ctx, "SELECT record FROM routing_policy WHERE policy_key = ?", "project_creation")
	if err != nil {
		return nil, err
	}
	if row == nil {
		return []string{"no explicit project creation policy is enabled"}, nil
	}
	policy, err := readStored(row.Get("record"))
	if err != nil {
		return nil, err
	}
	if policy["enabled"] != true {
		return []string{"no explicit project creation policy is enabled"}, nil
	}
	members := []string{}
	counted := Object{}
	for _, id := range list(payload["members"]) {
		route, err := s.One(ctx, "SELECT stage, target, goal, product_key, workspace FROM incident_routes WHERE fault_id = ?", id)
		if err != nil {
			return nil, err
		}
		if route == nil || route.Get("product_key") != payload["product"] || route.Get("workspace") != payload["workspace"] {
			continue
		}
		latest, err := latestIncident(ctx, s, text(id))
		if err != nil {
			return nil, err
		}
		if route.Get("stage") != "held" {
			continue
		}
		target, err := readStored(route.Get("target"))
		if err != nil {
			return nil, err
		}
		if target["hold"] != "no_project" || route.Get("goal") != payload["goal"] || object(latest["goal"])["criteria"] != payload["criteria"] {
			continue
		}
		members = append(members, text(id))
		if c := text(latest["component"]); c != "" {
			counted[c] = true
		}
	}
	problems := []string{}
	minimum, _ := evidence.PyInt(policy["minIndependentFixes"])
	if int64(len(members)) < minimum {
		problems = append(problems, fmt.Sprintf("%d held defect(s) still share goal %s and its criteria; the policy needs %d", len(members), payload["goal"], minimum))
	}
	named := Object{}
	for _, c := range list(payload["components"]) {
		named[text(c)] = true
	}
	if len(members) > 0 && !slices.Equal(sortedKeys(named), sortedKeys(counted)) {
		problems = append(problems, fmt.Sprintf("the create covers %s, but its members' components are %s", evidence.Repr(sortedKeys(named)), evidence.Repr(sortedKeys(counted))))
	}
	rows, err := s.All(ctx, "SELECT fault_id, target FROM incident_routes WHERE product_key = ? AND stage = ? AND goal = ?", payload["product"], "held", payload["goal"])
	if err != nil {
		return nil, err
	}
	others := Object{}
	for _, row := range rows {
		target, err := readStored(row.Get("target"))
		if err != nil {
			return nil, err
		}
		if target["hold"] != "no_project" {
			continue
		}
		latest, err := latestIncident(ctx, s, text(row.Get("fault_id")))
		if err != nil {
			return nil, err
		}
		criteria := object(latest["goal"])["criteria"]
		if criteria != nil && criteria != payload["criteria"] {
			others[text(criteria)] = true
		}
	}
	if len(others) > 0 {
		problems = append(problems, fmt.Sprintf("held defects under %s now also declare %s; a project needs one completion contract", payload["goal"], evidence.Repr(sortedKeys(others))))
	}
	if payload["criteria"] == nil || payload["criteria"] == "" {
		problems = append(problems, "the goal declares no completion criteria")
	}
	row, err = s.One(ctx, "SELECT record FROM product_registry WHERE product_key = ?", payload["product"])
	if err != nil {
		return nil, err
	}
	if row == nil {
		problems = append(problems, fmt.Sprintf("%s is no longer registered", payload["product"]))
	} else {
		registry, err := readStored(row.Get("record"))
		if err != nil {
			return nil, err
		}
		for _, pair := range [][2]string{{"team", "team"}, {"familyLabel", "family label"}} {
			if registry[pair[0]] != payload[pair[0]] {
				problems = append(problems, fmt.Sprintf("%s's %s is now %s, not %s", payload["product"], pair[1], evidence.Repr(registry[pair[0]]), evidence.Repr(payload[pair[0]])))
			}
		}
	}
	rows, err = s.All(ctx, "SELECT record FROM product_bindings WHERE product_key = ? AND kind = 'project'", payload["product"])
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		project, err := readStored(row.Get("record"))
		if err != nil {
			return nil, err
		}
		if project["test"] == true || project["state"] != "active" {
			continue
		}
		for _, component := range list(project["components"]) {
			if _, ok := named[text(component)]; ok {
				components := make([]string, 0, len(list(payload["components"])))
				for _, c := range list(payload["components"]) {
					components = append(components, text(c))
				}
				slices.Sort(components)
				problems = append(problems, fmt.Sprintf("%s now covers %s", project["ref"], evidence.Repr(components)))
				break
			}
		}
	}
	return problems, nil
}
