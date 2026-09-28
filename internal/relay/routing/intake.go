package routing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func scope(workspace string, project any) Object {
	out := Object{}
	if workspace != "" {
		out["workspace"] = workspace
	}
	if project != nil && project != "" {
		out["projectKey"] = project
	}
	return out
}
func target(decision, registry, incident Object, owed []any, cause, claim any) Object {
	var team any
	if registry != nil {
		team = registry["team"]
		if incident["origin"] == "simulated" {
			team = object(registry["testTarget"])["team"]
		}
	}
	if owed == nil {
		owed = []any{}
	}
	related := list(decision["relate"])
	if related == nil {
		related = []any{}
	}
	return Object{"team": team, "project": decision["project"], "owner": decision["owner"], "relate": related, "hold": decision["hold"], "labels": IssueLabels(incident), "cause": cause, "unverifiedCause": claim, "obligations": owed}
}
func watched(registry, incident Object) error {
	spec := object(object(registry["surfaces"])[text(incident["surface"])])
	if spec == nil || spec["active"] != true {
		return routeRefused("route_surface_unwatched", fmt.Sprintf("%s does not watch %s; nothing from an unconnected surface is collected", registry["product"], incident["surface"]))
	}
	if incident["origin"] == "simulated" && registry["testTarget"] == nil {
		return malformed(fmt.Sprintf("a simulated incident needs %s's test target", registry["product"]))
	}
	return nil
}
func observation(product, workspace, class, severity string, signature Object, incident Object, project any) faults.Observation {
	return faults.Observation{Product: product, FaultClass: class, Severity: severity, Signature: signature, OccurrenceKey: text(incident["occurrenceKey"]), Scope: scope(workspace, project), ObservedAt: incident["observedAt"], Detail: DetailText(incident), Evidence: list(incident["evidence"])}
}

type replayed struct{ answer Object }

func (*replayed) Error() string { return "routing occurrence replay" }
func occurrenceAttempt(ctx context.Context, s *store.Store, run func() error) (err error) {
	db := s.Q(ctx)
	if _, err = db.ExecContext(ctx, "SAVEPOINT route_occurrence"); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_, rollback := db.ExecContext(ctx, "ROLLBACK TO route_occurrence")
			err = errors.Join(err, rollback)
		}
		_, release := db.ExecContext(ctx, "RELEASE route_occurrence")
		err = errors.Join(err, release)
	}()
	return run()
}
func unchanged(route, answer Object) Object {
	t := object(route["target"])
	out := clone(answer)
	for k, v := range (Object{"disposition": route["disposition"], "stage": route["stage"], "project": t["project"], "owner": t["owner"], "hold": t["hold"], "unverifiedCause": t["unverifiedCause"], "recorded": false, "publication": nil, "reason": "the ledger already recorded this occurrence; nothing changed"}) {
		out[k] = v
	}
	return out
}
func classified(incident, classification Object) Object {
	out := clone(incident)
	out["product"] = classification["product"]
	for _, k := range []string{"component", "symptom", "goal"} {
		if classification[k] != nil {
			out[k] = classification[k]
		}
	}
	return out
}

func (r *Router) Intake(ctx context.Context, value any) (Object, error) {
	incident, err := ReadIncident(value)
	if err != nil {
		return nil, err
	}
	var answer Object
	err = r.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		var err error
		answer, err = r.intake(ctx, incident)
		return err
	})
	return answer, err
}
func (r *Router) intake(ctx context.Context, incident Object) (Object, error) {
	registries, err := r.Registries(ctx)
	if err != nil {
		return nil, err
	}
	product, why := ResolveProduct(registries, incident)
	registry := registries[text(product)]
	if registry != nil {
		if err := watched(registry, incident); err != nil {
			return nil, err
		}
	}
	workspace, err := WorkspaceFor(incident, registry)
	if err != nil {
		return nil, err
	}
	if registry == nil {
		return r.unresolved(ctx, incident, workspace, why)
	}
	return r.place(ctx, incident, registry, workspace)
}
func (r *Router) unresolved(ctx context.Context, incident Object, workspace, why string) (Object, error) {
	if err := r.ready("fault_id"); err != nil {
		return nil, err
	}
	signature := PendingSignature(incident)
	id, err := r.Ledger.CanonicalID(ctx, "unclassified", "unclassified_incident", signature, workspace)
	if err != nil {
		return nil, err
	}
	route, err := r.routes().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if route != nil && route["stage"] == "superseded" {
		classification := object(route["classification"])
		registry, err := r.Registry(ctx, text(classification["product"]))
		if err != nil {
			return nil, err
		}
		applied := classified(incident, classification)
		if err = watched(registry, applied); err != nil {
			return nil, err
		}
		workspace, err := WorkspaceFor(applied, registry)
		if err != nil {
			return nil, err
		}
		answer, err := r.place(ctx, applied, registry, workspace)
		if err == nil {
			answer["forwardedFrom"] = id
		}
		return answer, err
	}
	err = occurrenceAttempt(ctx, r.Store, func() error {
		answer, err := r.Ledger.RecordObservation(ctx, observation("unclassified", workspace, "unclassified_incident", "notice", signature, incident, nil), nil)
		if err != nil {
			return err
		}
		if route != nil && answer["recorded"] != true {
			return &replayed{answer}
		}
		if err = r.routes().Upsert(ctx, Object{"fault_id": id, "product": "unclassified", "workspace": workspace, "disposition": "pending_classification", "stage": "pending_classification", "target": target(Object{}, nil, incident, nil, nil, nil), "origin": incident["origin"], "claimed_severity": incident["severity"], "goal": object(incident["goal"])["key"], "detail": why}); err != nil {
			return err
		}
		keep := store.MaxStoredIncidents
		return r.routes().StoreIncident(ctx, id, incident, &keep, true)
	})
	var replay *replayed
	if errors.As(err, &replay) {
		return unchanged(route, Object{"faultId": id, "product": nil, "workspace": workspace}), nil
	}
	if err != nil {
		return nil, err
	}
	if incident["severity"] == "broken" {
		if _, err = r.Ledger.Notify(ctx, id, "awaiting_classification", text(incident["occurrenceKey"])); err != nil {
			return nil, err
		}
	}
	return Object{"faultId": id, "product": nil, "workspace": workspace, "disposition": "pending_classification", "stage": "pending_classification", "reason": why}, nil
}
func (r *Router) place(ctx context.Context, incident, registry Object, workspace string) (Object, error) {
	if err := r.ready("fault_id"); err != nil {
		return nil, err
	}
	cause := object(incident["cause"])
	var causeID, claim any
	var causeRow Object
	if cause != nil && cause["product"] != registry["product"] {
		row, err := r.Ledger.Get(ctx, text(cause["faultId"]))
		if err != nil {
			return nil, err
		}
		verified := row != nil && row["product"] == cause["product"]
		if verified && cause["signature"] != nil {
			signature, err := readStored(row["signature"])
			if err != nil {
				return nil, err
			}
			verified = equal(signature, cause["signature"])
		}
		why := "no fault of that product with that signature is recorded here"
		if verified {
			signature, err := readStored(row["signature"])
			if err != nil {
				return nil, err
			}
			origin := "observed"
			if signature["simulated"] == true {
				origin = "simulated"
			}
			if origin != incident["origin"] {
				verified = false
				why = fmt.Sprintf("a %s incident never counts against a %s fault", incident["origin"], origin)
			}
		}
		if verified {
			causeID, causeRow = cause["faultId"], row
		} else {
			claim = contract.OrderedObject{{Key: "product", Value: cause["product"]}, {Key: "faultId", Value: cause["faultId"]}, {Key: "why", Value: why}}
		}
	}
	return r.file(ctx, incident, registry, workspace, causeID, claim, causeRow)
}
func (r *Router) file(ctx context.Context, incident, registry Object, workspace string, cause, unverified any, causeRow Object) (Object, error) {
	product := text(registry["product"])
	bindings, err := r.Bindings(ctx, product)
	if err != nil {
		return nil, err
	}
	run, err := r.RunIssue(ctx, object(incident["context"])["run"])
	if err != nil {
		return nil, err
	}
	if r.DecisionRead != nil {
		r.DecisionRead(ctx, "decide")
	}
	decision := Decide(incident, registry, bindings, run)
	observe := decision["disposition"] == "observe"
	class, severity := "product_defect", text(incident["severity"])
	if observe {
		class, severity = "product_expected", "notice"
	}
	var attached any
	if decision["disposition"] == "attach_current" {
		attached = decision["owner"]
	}
	signature := DefectSignature(incident, attached)
	id, err := r.Ledger.CanonicalID(ctx, product, class, signature, workspace)
	if err != nil {
		return nil, err
	}
	existing, err := r.routes().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing["stage"] == "filed" {
		kept := object(existing["target"])
		decision = clone(decision)
		for k, v := range (Object{"disposition": existing["disposition"], "stage": "filed", "project": kept["project"], "owner": kept["owner"], "hold": nil, "relate": kept["relate"], "reason": "filed earlier; the ledger's record carries this occurrence"}) {
			decision[k] = v
		}
	}
	before := object(existing["target"])
	claim := unverified
	if claim == nil && cause == nil {
		claim = before["unverifiedCause"]
	}
	if cause == nil {
		cause = before["cause"]
	}
	owed := Obligations(decision, existing, cause, IssueLabels(incident))
	destination := target(decision, registry, incident, owed, cause, claim)
	firstRow, err := r.Ledger.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	first := firstRow == nil
	var adopt *faults.Adoption
	if decision["owner"] != nil && first {
		adopt = &faults.Adoption{ExternalRef: text(decision["owner"]), Scope: scope(workspace, decision["project"])}
	}
	var result Object
	record := func(project any, adoption *faults.Adoption) error {
		var err error
		result, err = r.Ledger.RecordObservation(ctx, observation(product, workspace, class, severity, signature, incident, project), adoption)
		if err != nil {
			return err
		}
		if existing != nil && result["recorded"] != true {
			return &replayed{result}
		}
		if causeRow != nil && result["recorded"] == true {
			placed, err := readStored(causeRow["scope"])
			if err != nil {
				return err
			}
			sig, err := readStored(causeRow["signature"])
			if err != nil {
				return err
			}
			row, err := r.Ledger.Get(ctx, id)
			if err != nil {
				return err
			}
			o := observation(text(causeRow["product"]), text(placed["workspace"]), text(causeRow["fault_class"]), text(causeRow["severity"]), sig, incident, placed["projectKey"])
			o.OccurrenceKey = fmt.Sprintf("affected:%s:%s:%v:%s", product, id, row["episode"], incident["occurrenceKey"])
			o.Detail = product + " was affected by this fault"
			if _, err = r.Ledger.RecordObservation(ctx, o, nil); err != nil {
				return err
			}
		}
		return r.saveIncident(ctx, id, product, workspace, decision, destination, incident)
	}
	err = occurrenceAttempt(ctx, r.Store, func() error {
		if decision["owner"] != nil && !first && before["owner"] == nil {
			if _, err := r.Ledger.Adopt(ctx, id, text(decision["owner"]), scope(workspace, decision["project"])); err != nil {
				return err
			}
		}
		if decision["project"] != nil {
			if _, err := r.Ledger.SetWorkspaceTarget(ctx, product, workspace, text(decision["project"]), text(destination["team"]), text(decision["project"])); err != nil {
				return err
			}
		}
		return record(decision["project"], adopt)
	})
	var replay *replayed
	if errors.As(err, &replay) {
		return unchanged(existing, Object{"faultId": id, "product": product, "workspace": workspace}), nil
	}
	if err != nil {
		if !ownerRefusal(err) {
			return nil, err
		}
		row, e := r.Ledger.Get(ctx, id)
		if e != nil {
			return nil, e
		}
		kept, e := readStored(row["scope"])
		if e != nil {
			return nil, e
		}
		decision = clone(decision)
		decision["disposition"], decision["stage"], decision["hold"], decision["project"], decision["reason"] = "held", "held", "owner_found_after_create", nil, fmt.Sprintf("%s owns this, but %s", decision["owner"], cleanError(err))
		destination = target(decision, registry, incident, nil, cause, claim)
		err = occurrenceAttempt(ctx, r.Store, func() error { return record(kept["projectKey"], nil) })
		if errors.As(err, &replay) {
			return unchanged(existing, Object{"faultId": id, "product": product, "workspace": workspace}), nil
		}
		if err != nil {
			return nil, err
		}
	}
	if _, err = r.discharge(ctx, id); err != nil {
		return nil, err
	}
	if decision["stage"] == "held" {
		if incident["severity"] == "broken" {
			if _, err = r.Ledger.Notify(ctx, id, "held_"+text(decision["hold"]), text(incident["occurrenceKey"])); err != nil {
				return nil, err
			}
		}
		if decision["hold"] == "no_project" {
			if _, err = r.evaluateProjects(ctx, product); err != nil {
				return nil, err
			}
		}
	}
	if existing != nil && existing["stage"] == "held" && before["hold"] == "no_project" && decision["hold"] != "no_project" {
		if _, err = r.evaluateProjects(ctx, product); err != nil {
			return nil, err
		}
	}
	if unverified != nil && incident["severity"] == "broken" {
		if _, err = r.Ledger.Notify(ctx, id, "cause_unverified", text(incident["occurrenceKey"])); err != nil {
			return nil, err
		}
	}
	return Object{"faultId": id, "product": product, "workspace": workspace, "disposition": decision["disposition"], "stage": decision["stage"], "project": decision["project"], "owner": decision["owner"], "hold": decision["hold"], "unverifiedCause": claim, "recorded": result["recorded"], "publication": result["publication"], "reason": decision["reason"]}, nil
}
func cleanError(err error) string {
	s := err.Error()
	for strings.HasPrefix(s, "transaction body: ") {
		s = strings.TrimPrefix(s, "transaction body: ")
	}
	return s
}
func ownerRefusal(err error) bool {
	s := cleanError(err)
	return strings.HasPrefix(s, "fault_adopt_conflict:") || strings.HasPrefix(s, "fault_scope_conflict:")
}
func (r *Router) saveIncident(ctx context.Context, id, product, workspace string, decision, destination, incident Object) error {
	if r.BeforeWrite != nil {
		if err := r.BeforeWrite(ctx, "save"); err != nil {
			return err
		}
	}
	if err := r.routes().Upsert(ctx, Object{"fault_id": id, "product": product, "workspace": workspace, "disposition": decision["disposition"], "stage": decision["stage"], "target": destination, "origin": incident["origin"], "claimed_severity": incident["severity"], "goal": object(incident["goal"])["key"], "detail": decision["reason"]}); err != nil {
		return err
	}
	keep := store.MaxStoredIncidents
	return r.routes().StoreIncident(ctx, id, incident, &keep, true)
}
func (r *Router) setTarget(ctx context.Context, id string, target Object) error {
	encoded, err := Canonical(target)
	if err != nil {
		return err
	}
	return r.Store.SetIncidentTarget(ctx, id, encoded, r.Clock.ISO())
}
func (r *Router) discharge(ctx context.Context, id string) ([]any, error) {
	route, err := r.routes().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	queued := []any{}
	if route == nil {
		return queued, nil
	}
	destination := object(route["target"])
	pending := []Object{}
	for _, v := range list(destination["obligations"]) {
		o := object(v)
		if o["state"] == "open" {
			pending = append(pending, o)
		}
	}
	if len(pending) == 0 {
		return queued, nil
	}
	if err = r.ready("get"); err != nil {
		return nil, err
	}
	row, err := r.Ledger.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	owned := row["external_ref"]
	if owned == nil {
		return queued, nil
	}
	err = r.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		for _, o := range pending {
			var value any
			switch o["kind"] {
			case "reopen":
				if owned != destination["owner"] {
					continue
				}
			case "add_label":
				if destination["owner"] != nil {
					continue
				}
				value = o["label"]
			default:
				other := o["toIssue"]
				if other == nil && o["toFault"] != nil {
					row, err := r.Ledger.Get(ctx, text(o["toFault"]))
					if err != nil {
						return err
					}
					other = row["external_ref"]
				}
				if other == nil {
					continue
				}
				value = Object{"type": "related", "issue": other}
			}
			if _, err := r.Ledger.RequestUpdate(ctx, id, text(o["kind"]), value); err != nil {
				return err
			}
			o["state"] = "queued"
			queued = append(queued, o)
		}
		if len(queued) > 0 {
			return r.setTarget(ctx, id, destination)
		}
		return nil
	})
	return queued, err
}

func (r *Router) Classify(ctx context.Context, id string, value any) (Object, error) {
	classification, err := ReadClassification(value)
	if err != nil {
		return nil, err
	}
	var answer Object
	err = r.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		route, err := r.routes().Get(ctx, id)
		if err != nil {
			return err
		}
		if route == nil || route["product_key"] != "unclassified" {
			return routeRefused("route_state_conflict", id+" is not a pending-classification record")
		}
		if route["stage"] == "superseded" {
			answer = Object{"faultId": id, "successor": route["superseded_by"], "replayed": 0, "changed": false}
			return nil
		}
		registry, err := r.Registry(ctx, text(classification["product"]))
		if err != nil {
			return err
		}
		if registry == nil {
			return routeRefused("route_product_unknown", fmt.Sprintf("%s is not a registered product", evidence.Repr(classification["product"])))
		}
		stored, err := r.routes().Incidents(ctx, id)
		if err != nil {
			return err
		}
		if len(stored) == 0 {
			return routeRefused("route_state_conflict", id+" keeps no incident")
		}
		for _, v := range stored {
			if err = watched(registry, classified(object(v), classification)); err != nil {
				return err
			}
		}
		if err = r.ready("route-classify"); err != nil {
			return err
		}
		var successor any
		for _, v := range stored {
			applied := classified(object(v), classification)
			workspace, err := WorkspaceFor(applied, registry)
			if err != nil {
				return err
			}
			placed, err := r.place(ctx, applied, registry, workspace)
			if err != nil {
				return err
			}
			successor = placed["faultId"]
		}
		o := observation("unclassified", text(route["workspace"]), "unclassified_incident", "notice", PendingSignature(object(stored[0])), object(stored[0]), nil)
		o.OccurrenceKey = "classified:" + text(successor)
		o.Detail = "classified"
		o.Evidence = []any{}
		o.ObservedAt = nil
		o.Cleared = true
		if _, err = r.Ledger.RecordObservation(ctx, o, nil); err != nil {
			return err
		}
		recorded := clone(classification)
		recorded["at"] = r.Clock.ISO()
		if err = r.routes().Upsert(ctx, Object{"fault_id": id, "product": "unclassified", "workspace": route["workspace"], "disposition": route["disposition"], "stage": "superseded", "target": route["target"], "origin": route["origin"], "claimed_severity": route["claimed_severity"], "classification": recorded, "superseded_by": successor, "detail": "classified by " + text(classification["by"])}); err != nil {
			return err
		}
		answer = Object{"faultId": id, "successor": successor, "replayed": len(stored), "changed": true}
		return nil
	})
	return answer, err
}
