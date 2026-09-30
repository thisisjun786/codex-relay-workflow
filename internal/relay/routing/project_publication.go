package routing

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

var declarationOnce sync.Once

func init() { faults.SetProductDeclarationInstaller(registerDeclarations) }

func registerDeclarations() {
	declarationOnce.Do(func() {
		if err := faults.RegisterKind("project_create", faults.KindPolicy{Creates: true, Target: "team", Evidence: "block", Validate: ValidateProjectPayload, Confirm: ConfirmProject, PreIssue: func(input map[string]any) any {
			ctx := input["context"].(context.Context)
			s := input["store"].(*store.Store)
			problems, err := issuable(ctx, s, object(input["fault"]), object(input["publication"])["payload"])
			if err != nil {
				return err
			}
			if len(problems) > 0 {
				return Object{"cancel": strings.Join(problems, "; ")}
			}
			return nil
		}}); err != nil {
			panic(err)
		}
	})
}
func issuable(ctx context.Context, s *store.Store, fault Object, value any) ([]string, error) {
	if problems := ValidateProjectPayload(value); len(problems) > 0 {
		return problems, nil
	}
	payload := object(value)
	signature, err := readStored(fault["signature"])
	if err != nil {
		return nil, err
	}
	scope, err := readStored(fault["scope"])
	if err != nil {
		return nil, err
	}
	ours := fault["product"] == payload["product"] && fault["fault_class"] == "project_needed" && equal(signature, Object{"goal": payload["goal"]}) && scope["workspace"] == payload["workspace"]
	var row store.Row
	if ours {
		row, err = s.One(ctx, "SELECT disposition, goal FROM incident_routes WHERE fault_id=?", fault["fault_id"])
		if err != nil {
			return nil, err
		}
	}
	if row == nil || row.Get("disposition") != "project_proposal" || row.Get("goal") != payload["goal"] {
		return []string{fmt.Sprintf("%v is not the project proposal routing made for %s's goal %s", fault["fault_id"], payload["product"], payload["goal"])}, nil
	}
	return ProjectEligibility(ctx, s, payload)
}
func (r *Router) EvaluateProjects(ctx context.Context, product string) (Object, error) {
	registry, err := r.Registry(ctx, product)
	if err != nil {
		return nil, err
	}
	if registry == nil {
		return nil, routeRefused("route_product_unknown", fmt.Sprintf("%s is not a registered product", evidence.Repr(product)))
	}
	return r.evaluateProjects(ctx, product)
}
func (r *Router) evaluateProjects(ctx context.Context, product string) (Object, error) {
	var answer Object
	err := r.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		registry, err := r.Registry(ctx, product)
		if err != nil {
			return err
		}
		policy, err := r.Policy(ctx)
		if err != nil {
			return err
		}
		cancelled, err := r.withdrawProjects(ctx, product)
		if err != nil {
			return err
		}
		if registry == nil || policy == nil || policy["enabled"] != true {
			answer = Object{"queued": []any{}, "skipped": []any{}, "cancelled": cancelled, "reason": "no explicit project creation policy is enabled"}
			return nil
		}
		r.test.read(ctx, "groups")
		groups := Object{}
		var after any
		for {
			page, err := r.routes().Listing(ctx, product, []string{"held"}, nil, 100, after)
			if err != nil {
				return err
			}
			for _, v := range list(page["routes"]) {
				route := object(v)
				if object(route["target"])["hold"] != "no_project" {
					continue
				}
				latest, err := latestIncident(ctx, r.Store, text(route["fault_id"]))
				if err != nil {
					return err
				}
				goal := object(latest["goal"])
				if text(goal["key"]) == "" || text(goal["criteria"]) == "" {
					continue
				}
				key := text(goal["key"])
				group := object(groups[key])
				if group == nil {
					group = Object{"criteria": Object{}, "members": Object{}, "components": Object{}, "workspace": route["workspace"]}
					groups[key] = group
				}
				object(group["criteria"])[text(goal["criteria"])] = true
				object(group["members"])[text(route["fault_id"])] = true
				object(group["components"])[text(latest["component"])] = true
			}
			after = page["next"]
			if after == nil {
				break
			}
		}
		queued, skipped := []any{}, []any{}
		for _, goal := range sortedKeys(groups) {
			group := object(groups[goal])
			criteria := sortedKeys(object(group["criteria"]))
			if len(criteria) > 1 {
				skipped = append(skipped, Object{"goal": goal, "reasons": []any{fmt.Sprintf("the held defects under %s declare different completion criteria %s; a project needs one", goal, evidence.Repr(criteria))}})
				continue
			}
			payload := Object{"product": product, "workspace": group["workspace"], "team": registry["team"], "familyLabel": registry["familyLabel"], "goal": goal, "criteria": criteria[0], "name": fmt.Sprintf("%s · %s", registry["familyLabel"], goal), "members": sortedKeys(object(group["members"])), "components": sortedKeys(object(group["components"]))}
			problems, err := ProjectEligibility(ctx, r.Store, payload)
			if err != nil {
				return err
			}
			if len(problems) > 0 {
				skipped = append(skipped, Object{"goal": goal, "reasons": problems})
				continue
			}
			id, err := r.Ledger.CanonicalID(ctx, product, "project_needed", Object{"goal": goal}, text(group["workspace"]))
			if err != nil {
				return err
			}
			rows, err := r.Ledger.Publications(ctx, id, "project_create", nil, 100, nil)
			if err != nil {
				return err
			}
			live := []Object{}
			revisable, changed := true, false
			for _, v := range rows {
				row := object(v)
				if row["state"] == "cancelled" {
					continue
				}
				live = append(live, row)
				revisable = revisable && slices.Contains([]string{"pending", "failed", "claimed"}, text(row["state"]))
				changed = changed || !equal(row["payload"], payload)
			}
			if len(live) > 0 && !(revisable && changed) {
				skipped = append(skipped, Object{"goal": goal, "reasons": []any{"a project create for this goal is already queued or done"}})
				continue
			}
			trigger := "need:" + goal
			for _, row := range live {
				if _, err = r.Ledger.Cancel(ctx, text(row["publication_id"]), "the goal's members changed; queued again with them"); err != nil {
					return err
				}
			}
			if _, err = r.Ledger.SetWorkspaceTarget(ctx, product, text(group["workspace"]), ProjectsScope, text(registry["team"]), ""); err != nil {
				return err
			}
			o := faults.Observation{Product: product, FaultClass: "project_needed", Severity: "notice", Signature: Object{"goal": goal}, OccurrenceKey: trigger, Scope: scope(text(group["workspace"]), ProjectsScope), Detail: fmt.Sprintf("%d independent fixes share %s: {%s}", len(list(payload["members"])), goal, evidence.Repr(criteria[0]))}
			if _, err = r.Ledger.RecordObservation(ctx, o, nil); err != nil {
				return err
			}
			if err := r.test.write(ctx, "queue"); err != nil {
				return err
			}
			if _, err = r.Ledger.Queue(ctx, id, "project_create", trigger, payload); err != nil {
				return err
			}
			destination, _ := PlainTarget(Object{"team": registry["team"], "labels": []any{registry["familyLabel"]}})
			if err = r.routes().Upsert(ctx, Object{"fault_id": id, "product": product, "workspace": group["workspace"], "disposition": "project_proposal", "stage": "filed", "target": destination, "origin": "observed", "claimed_severity": "notice", "goal": goal, "detail": "project for " + goal + " queued under the explicit policy"}); err != nil {
				return err
			}
			queued = append(queued, Object{"goal": goal, "faultId": id, "trigger": trigger, "members": payload["members"]})
		}
		answer = Object{"queued": queued, "skipped": skipped, "cancelled": cancelled}
		return nil
	})
	return answer, err
}
func (r *Router) withdrawProjects(ctx context.Context, product string) ([]any, error) {
	cancelled := []any{}
	var after any
	for {
		page, err := r.routes().Listing(ctx, product, []string{"filed"}, []string{"project_proposal"}, 100, after)
		if err != nil {
			return nil, err
		}
		for _, v := range list(page["routes"]) {
			route := object(v)
			id := text(route["fault_id"])
			fault, err := r.Ledger.Get(ctx, id)
			if err != nil {
				return nil, err
			}
			rows, err := r.Ledger.Publications(ctx, id, "project_create", nil, 100, nil)
			if err != nil {
				return nil, err
			}
			for _, v := range rows {
				row := object(v)
				if !slices.Contains([]string{"pending", "failed", "claimed"}, text(row["state"])) {
					continue
				}
				problems, err := issuable(ctx, r.Store, fault, row["payload"])
				if err != nil {
					return nil, err
				}
				if len(problems) > 0 {
					if _, err = r.Ledger.Cancel(ctx, text(row["publication_id"]), strings.Join(problems, "; ")); err != nil {
						return nil, err
					}
					cancelled = append(cancelled, Object{"faultId": id, "publicationId": row["publication_id"], "reasons": problems})
				}
			}
			fresh, err := r.Ledger.Publications(ctx, id, "project_create", nil, 100, nil)
			if err != nil {
				return nil, err
			}
			all := len(rows) > 0
			for _, v := range fresh {
				all = all && object(v)["state"] == "cancelled"
			}
			if all {
				if err = r.Store.SettleIncidentRoute(ctx, id, "observed", "every create it queued was cancelled before issue", r.Clock.ISO()); err != nil {
					return nil, err
				}
			}
		}
		after = page["next"]
		if after == nil {
			return cancelled, nil
		}
	}
}
func (r *Router) bindConfirmed(ctx context.Context, route Object) ([]any, error) {
	id, product := text(route["fault_id"]), text(route["product_key"])
	rows, err := r.Ledger.Publications(ctx, id, "project_create", nil, 100, nil)
	if err != nil {
		return nil, err
	}
	bindings, err := r.Bindings(ctx, product)
	if err != nil {
		return nil, err
	}
	bound, tested := Object{}, Object{}
	for _, b := range bindings {
		if b["kind"] == "project" {
			if b["test"] == true {
				tested[text(b["ref"])] = true
			} else {
				bound[text(b["ref"])] = true
			}
		}
	}
	registry, err := r.Registry(ctx, product)
	if err != nil {
		return nil, err
	}
	tested[text(object(registry["testTarget"])["project"])] = true
	made := []any{}
	stranded, testTarget := false, false
	for _, v := range rows {
		row := object(v)
		ref := text(row["external_ref"])
		payload := object(row["payload"])
		if row["state"] != "confirmed" || ref == "" || (bound[ref] == true && object(route["target"])["project"] == ref) {
			continue
		}
		if bound[ref] != true && tested[ref] == true {
			testTarget = true
			continue
		}
		if bound[ref] != true && payload["team"] != registry["team"] {
			stranded = true
			continue
		}
		err = r.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
			if bound[ref] != true {
				name := payload["name"]
				if name == nil || name == "" {
					name = ref
				}
				if _, err := r.Bind(ctx, Object{"schema": "product-binding/1", "product": product, "kind": "project", "ref": ref, "title": name, "state": "active", "components": absent(payload["components"], []any{}), "goal": payload["goal"], "source": fmt.Sprintf("created by product routing (publication %s)", row["publication_id"])}); err != nil {
					return err
				}
				made = append(made, ref)
			}
			destination := clone(object(route["target"]))
			destination["project"], destination["hold"] = ref, nil
			return r.setTarget(ctx, id, destination)
		})
		if err != nil {
			return nil, err
		}
	}
	if testTarget || stranded {
		destination := clone(object(route["target"]))
		destination["hold"] = "project_team_changed"
		if testTarget {
			destination["hold"] = "project_is_test_target"
		}
		return made, r.setTarget(ctx, id, destination)
	}
	all := len(rows) > 0
	created := []string{}
	for _, v := range rows {
		row := object(v)
		all = all && (row["state"] == "confirmed" || row["state"] == "cancelled")
		if row["state"] == "confirmed" && row["external_ref"] != nil {
			created = append(created, text(row["external_ref"]))
		}
	}
	if all {
		slices.Sort(created)
		detail := "every create it queued was cancelled before issue"
		if len(created) > 0 {
			detail = "project " + strings.Join(created, ", ") + " created and bound"
		}
		if err = r.Store.SettleIncidentRoute(ctx, id, "observed", detail, r.Clock.ISO()); err != nil {
			return nil, err
		}
	}
	return made, nil
}
