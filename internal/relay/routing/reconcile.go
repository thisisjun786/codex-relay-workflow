package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"slices"
)

func (r *Router) redecide(ctx context.Context, product string) ([]any, error) {
	registry, err := r.Registry(ctx, product)
	if err != nil {
		return nil, err
	}
	changed := []any{}
	if registry == nil {
		return changed, nil
	}
	bindings, err := r.Bindings(ctx, product)
	if err != nil {
		return nil, err
	}
	var after any
	for {
		page, err := r.routes().Listing(ctx, product, []string{"held", "filed"}, nil, 100, after)
		if err != nil {
			return nil, err
		}
		for _, v := range list(page["routes"]) {
			answer, err := r.again(ctx, pyjson.Map(v), registry, bindings)
			if err != nil {
				return nil, err
			}
			if answer != nil {
				changed = append(changed, answer)
			}
		}
		after = page["next"]
		if after == nil {
			return changed, nil
		}
	}
}
func (r *Router) again(ctx context.Context, route, registry Object, bindings []Object) (Object, error) {
	if slices.Contains([]string{"project_proposal", "completion_mismatch", "completion_unverified"}, pyjson.Text(route["disposition"])) {
		return nil, nil
	}
	id := pyjson.Text(route["fault_id"])
	var fault Object
	var err error
	if route["stage"] == "filed" {
		fault, err = r.Ledger.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if fault["external_ref"] != nil {
			return nil, nil
		}
	}
	stored, err := r.routes().Incidents(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(stored) == 0 {
		return nil, nil
	}
	incident := pyjson.Map(stored[len(stored)-1])
	run, err := r.RunIssue(ctx, pyjson.Map(incident["context"])["run"])
	if err != nil {
		return nil, err
	}
	r.test.read(ctx, "decide")
	decision := Decide(incident, registry, bindings, run)
	if decision["disposition"] == "attach_current" {
		return nil, nil
	}
	current := pyjson.Map(route["target"])
	destination := target(decision, registry, incident, Obligations(decision, route, current["cause"], IssueLabels(incident)), current["cause"], current["unverifiedCause"])
	comparable := func(t Object, disposition any) []any {
		team := t["team"]
		if t["project"] == nil {
			team = nil
		}
		related := []string{}
		for _, v := range list(t["relate"]) {
			related = append(related, pyjson.Text(v))
		}
		slices.Sort(related)
		return []any{t["project"], t["owner"], t["hold"], team, disposition, related}
	}
	if equal(comparable(destination, decision["disposition"]), comparable(current, route["disposition"])) {
		return nil, nil
	}
	place := scope(pyjson.Text(route["workspace"]), decision["project"])
	unplaced := false
	if decision["project"] == nil {
		fault, err = r.Ledger.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		unplaced = fault["external_ref"] == nil
	}
	err = r.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		if decision["owner"] != nil {
			if _, err := r.Ledger.Adopt(ctx, id, pyjson.Text(decision["owner"]), place); err != nil {
				return err
			}
		}
		if decision["project"] != nil {
			if _, err := r.Ledger.SetWorkspaceTarget(ctx, pyjson.Text(registry["product"]), pyjson.Text(route["workspace"]), pyjson.Text(decision["project"]), pyjson.Text(destination["team"]), pyjson.Text(decision["project"])); err != nil {
				return err
			}
		}
		if decision["owner"] == nil && (decision["project"] != nil || unplaced) {
			if _, err := r.Ledger.Move(ctx, id, place); err != nil {
				return err
			}
		}
		return r.routes().Upsert(ctx, Object{"fault_id": id, "product": registry["product"], "workspace": route["workspace"], "disposition": decision["disposition"], "stage": decision["stage"], "target": destination, "origin": route["origin"], "claimed_severity": route["claimed_severity"], "detail": decision["reason"]})
	})
	if err != nil {
		if !ownerRefusal(err) {
			return nil, err
		}
		held := clone(current)
		held["hold"], held["owner"] = "owner_found_after_create", decision["owner"]
		encoded, e := Canonical(held)
		if e != nil {
			return nil, e
		}
		_, e = r.Store.Q(ctx).ExecContext(ctx, "UPDATE incident_routes SET stage=?,disposition=?,target=?,detail=?,updated_at=? WHERE fault_id=?", "held", "held", encoded, cleanError(err), r.Clock.ISO(), id)
		return Object{"faultId": id, "hold": "owner_found_after_create"}, e
	}
	if _, err = r.discharge(ctx, id); err != nil {
		return nil, err
	}
	return Object{"faultId": id, "disposition": decision["disposition"], "project": decision["project"], "owner": decision["owner"], "hold": decision["hold"]}, nil
}
func (r *Router) reconcileRoute(ctx context.Context, route Object) (Object, error) {
	empty := func() Object { return Object{"queued": []any{}, "bound": []any{}} }
	if route["stage"] != "filed" {
		return empty(), nil
	}
	if route["disposition"] == "project_proposal" {
		bound, err := r.bindConfirmed(ctx, route)
		return Object{"queued": []any{}, "bound": bound}, err
	}
	for _, v := range list(pyjson.Map(route["target"])["obligations"]) {
		if pyjson.Map(v)["state"] == "open" {
			queued, err := r.discharge(ctx, pyjson.Text(route["fault_id"]))
			return Object{"queued": queued, "bound": []any{}}, err
		}
	}
	return empty(), nil
}
func (r *Router) Reconcile(ctx context.Context, product any, limit, after any) (Object, error) {
	return r.reconcile(ctx, product, limit, after)
}
func (r *Router) reconcile(ctx context.Context, product any, limit, after any) (Object, error) {
	bound, after, err := ReadPage(limit, after, 5000)
	if err != nil {
		return nil, err
	}
	queued, made := []any{}, []any{}
	seen := int64(0)
	for seen < bound {
		page, err := r.routes().Listing(ctx, product, []string{"filed"}, nil, min(int64(100), bound-seen), after)
		if err != nil {
			return nil, err
		}
		for _, v := range list(page["routes"]) {
			seen++
			done, err := r.reconcileRoute(ctx, pyjson.Map(v))
			if err != nil {
				return nil, err
			}
			queued = append(queued, list(done["queued"])...)
			made = append(made, list(done["bound"])...)
		}
		after = page["next"]
		if after == nil {
			break
		}
	}
	return Object{"queued": queued, "bound": made, "read": seen, "next": after}, nil
}

func digestEntry(route, now Object) Object {
	t := pyjson.Map(route["target"])
	return Object{"faultId": route["fault_id"], "product": route["product_key"], "disposition": route["disposition"], "stage": route["stage"], "state": now["state"], "severity": now["severity"], "claimedSeverity": now["claimedSeverity"], "occurrences": now["occurrenceCount"], "issue": now["externalRef"], "project": t["project"], "owner": t["owner"], "hold": t["hold"], "unverifiedCause": t["unverifiedCause"], "origin": route["origin"], "detail": route["detail"]}
}
func severe(s Object) bool {
	return s != nil && (s["severity"] == "broken" || s["claimedSeverity"] == "broken")
}
func number(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	case json.Number:
		value, _ := n.Int64()
		return value
	}
	return 0
}
func (r *Router) Digest(ctx context.Context, limit, after any) (Object, error) {
	bound, cursor, err := ReadPage(limit, after, 5000)
	if err != nil {
		return nil, err
	}
	made, reached := []any{}, []any{}
	proposals, err := r.routes().OutstandingProposals(ctx, bound)
	if err != nil {
		return nil, err
	}
	for _, v := range proposals {
		route := pyjson.Map(v)
		done, err := r.reconcileRoute(ctx, route)
		if err != nil {
			return nil, err
		}
		made = append(made, list(done["bound"])...)
		reached = append(reached, route["fault_id"])
		if err = r.Store.CheckIncidentRoute(ctx, pyjson.Text(route["fault_id"])); err != nil {
			return nil, err
		}
	}
	linked, severes, decisions, resolutions := []any{}, []any{}, []any{}, []any{}
	routine := Object{}
	read := int64(0)
	pageSize := r.test.digestPageSize
	if pageSize == 0 {
		pageSize = 100
	}
	pageNumber := 0
	for read < bound {
		pageNumber++
		if err := r.test.digestPage(ctx, pageNumber); err != nil {
			return nil, err
		}
		page, err := r.routes().Listing(ctx, nil, nil, nil, min(pageSize, bound-read), cursor)
		if err != nil {
			return nil, err
		}
		err = r.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
			unreached := map[string]any{}
			for _, v := range list(page["routes"]) {
				read++
				route := pyjson.Map(v)
				if route["stage"] == "superseded" {
					continue
				}
				if route["disposition"] != "project_proposal" {
					done, err := r.reconcileRoute(ctx, route)
					if err != nil {
						return err
					}
					linked = append(linked, list(done["queued"])...)
				}
				route, err = r.routes().Get(ctx, pyjson.Text(route["fault_id"]))
				if err != nil {
					return err
				}
				held := route["stage"] == "held" && pyjson.Map(route["target"])["hold"] == "no_project"
				key := fmt.Sprint(route["product_key"], "|", route["goal"])
				if _, ok := unreached[key]; held && !ok {
					unreached[key], err = r.routes().UnreachedProposal(ctx, pyjson.Text(route["product_key"]), route["goal"], reached)
					if err != nil {
						return err
					}
				}
				if held && unreached[key] != nil {
					continue
				}
				fault, err := r.Ledger.Get(ctx, pyjson.Text(route["fault_id"]))
				if err != nil {
					return err
				}
				now := snapshot(route, fault)
				before := pyjson.Map(route["reported"])
				if equal(now, before) {
					continue
				}
				entry := digestEntry(route, now)
				reported := false
				if severe(now) && !severe(before) && now["state"] != "resolved" && now["state"] != "withdrawn" {
					severes = append(severes, entry)
					reported = true
				}
				decision := Attention(now)
				var previous any
				if before != nil {
					previous = Attention(before)
				}
				if decision != nil && decision != previous {
					if _, err = r.Ledger.Notify(ctx, pyjson.Text(route["fault_id"]), pyjson.Text(decision), "route:"+pyjson.Text(route["fault_id"])); err != nil {
						return err
					}
					d := clone(entry)
					d["decision"] = decision
					decisions = append(decisions, d)
					reported = true
				}
				if now["state"] == "resolved" && before != nil && before["state"] != "resolved" {
					resolutions = append(resolutions, entry)
					reported = true
				}
				if !reported {
					key := pyjson.Text(route["product_key"])
					summary := pyjson.Map(routine[key])
					if summary == nil {
						summary = Object{"records": int64(0), "newOccurrences": int64(0)}
						routine[key] = summary
					}
					summary["records"] = number(summary["records"]) + 1
					summary["newOccurrences"] = number(summary["newOccurrences"]) + max(int64(0), number(now["occurrenceCount"])-number(before["occurrenceCount"]))
				}
				encoded, err := Canonical(now)
				if err != nil {
					return err
				}
				if err = r.Store.SetIncidentReported(ctx, pyjson.Text(route["fault_id"]), encoded, r.Clock.ISO()); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		cursor = page["next"]
		if cursor == nil {
			break
		}
	}
	unreached, err := r.routes().UnreachedCount(ctx, reached)
	if err != nil {
		return nil, err
	}
	return Object{"quiet": len(severes)+len(decisions)+len(resolutions)+len(routine)+len(linked)+len(made) == 0, "severe": severes, "decisions": decisions, "resolutions": resolutions, "routine": routine, "linked": linked, "projectsBound": made, "proposalsUnreached": unreached, "read": read, "next": cursor, "limits": "routing's rows and the ledger's state in this store only. A queued write is not an issue anybody has written; pass next as after to continue."}, nil
}
