package routing

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
)

func completionSignature(reading Object, check string, round int) Object {
	signature := Object{"subject": reading["subject"], "check": check}
	if round > 1 {
		signature["round"] = round
	}
	if reading["origin"] == "simulated" {
		signature["simulated"] = true
	}
	return signature
}
func (r *Router) mismatchRound(ctx context.Context, product, workspace string, reading Object, check string) (Object, error) {
	closed := []any{}
	for round := 1; round <= 20; round++ {
		signature := completionSignature(reading, check, round)
		id, err := r.Ledger.CanonicalID(ctx, product, "completion_mismatch", signature, workspace)
		if err != nil {
			return nil, err
		}
		row, err := r.Ledger.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if row == nil || row["state"] != "resolved" {
			return Object{"mismatch": id, "mismatchRow": row, "mismatchSignature": signature, "closed": closed}, nil
		}
		closed = append(closed, id)
	}
	return Object{"mismatch": nil, "mismatchRow": nil, "mismatchSignature": nil, "closed": closed}, nil
}
func (r *Router) recurrences(ctx context.Context, product, subject string) ([]any, any, error) {
	recurring := []any{}
	unread := []string{}
	var after any
	read := 0
	for read < 1000 {
		answer, err := r.Ledger.Snapshot(ctx, product, "product_defect", "", 100, after)
		if err != nil {
			return nil, nil, err
		}
		page := pyjson.Map(answer)
		for _, v := range list(page["faults"]) {
			read++
			row := pyjson.Map(v)
			if row["external_ref"] != subject || row["state"] != "open" {
				continue
			}
			if number(row["reopen_count"]) > 0 {
				recurring = append(recurring, Object{"faultId": row["fault_id"], "reason": fmt.Sprintf("came back after it was resolved (cycle %v)", row["cycle"])})
				continue
			}
			history, err := r.Ledger.Remediations(ctx, pyjson.Text(row["fault_id"]), 1000)
			if err != nil {
				return nil, nil, err
			}
			fixes := []Object{}
			for _, v := range history {
				h := pyjson.Map(v)
				if h["kind"] == "fix" && h["cycle"] == row["cycle"] {
					fixes = append(fixes, h)
				}
			}
			if len(fixes) > 0 {
				recurring = append(recurring, Object{"faultId": row["fault_id"], "reason": "occurred again after the fix " + pyjson.Text(fixes[len(fixes)-1]["ref"])})
			} else if len(history) >= 1000 && pyjson.Map(history[0])["cycle"] == row["cycle"] {
				unread = append(unread, pyjson.Text(row["fault_id"]))
			}
		}
		after = page["next"]
		if after == nil {
			break
		}
	}
	reasons := []string{}
	if after != nil {
		reasons = append(reasons, product+" has more than 1000 defect records; a recurrence beyond them cannot be ruled out")
	}
	if len(unread) > 0 {
		reasons = append(reasons, fmt.Sprintf("%d of %s's defects have more than 1000 remediations in their current cycle; a fix before them cannot be ruled out", len(unread), subject))
	}
	var unknown any
	if len(reasons) > 0 {
		unknown = strings.Join(reasons, "; ")
	}
	return recurring, unknown, nil
}
func completionDetail(reading, entry Object) string {
	check := pyjson.Text(entry["check"])
	claims := []string{}
	for _, name := range []string{"linearDone", "prMerged", "sessionEnded"} {
		if pyjson.Map(reading["claims"])[name] == true {
			claims = append(claims, name)
		}
	}
	claimed := strings.Join(claims, ", ")
	if claimed == "" {
		claimed = "none"
	}
	return strings.Join([]string{fmt.Sprintf("completion check %s of %s: %s", check, reading["subject"], entry["verdict"]), "reason: " + pyjson.Text(entry["reason"]), "claims: " + claimed, fmt.Sprintf("required: %s  observed: %s  origin: %s", pyvalue.Str(pyjson.Map(reading["requires"])[check]), pyjson.Map(reading["observed"])[check], reading["origin"]), fmt.Sprintf("next action: re-verify %s for %s and record the fix and the verification, or an approved exception", check, reading["subject"]), "This asks the owner to re-verify. It changes no state of the subject: a Done stays Done until somebody who owns it decides otherwise."}, "\n")
}
func completionEvidence(reading Object, check, key string) []any {
	entries := []any{Object{"kind": "completion-reading", "ref": key, "observed": Object{"check": check, "result": pyjson.Map(reading["observed"])[check], "required": pyjson.Map(reading["requires"])[check]}}}
	refs := pyjson.Map(pyjson.Map(reading["evidence"])[check])
	for _, role := range sortedKeys(refs) {
		ref := pyjson.Map(refs[role])
		entries = append(entries, Object{"kind": role, "ref": ref["ref"], "source": ref["source"]})
	}
	return entries
}
func (r *Router) CheckCompletion(ctx context.Context, value any) (Object, error) {
	reading, err := ReadReading(value)
	if err != nil {
		return nil, err
	}
	var answer Object
	err = r.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		var err error
		answer, err = r.recordReading(ctx, reading)
		return err
	})
	return answer, err
}
func (r *Router) recordReading(ctx context.Context, reading Object) (Object, error) {
	product := pyjson.Text(reading["product"])
	registry, err := r.Registry(ctx, product)
	if err != nil {
		return nil, err
	}
	if registry == nil {
		return nil, routeRefused("route_product_unknown", fmt.Sprintf("%s is not a registered product", quote.Value(product)))
	}
	simulated := reading["origin"] == "simulated"
	if simulated && registry["testTarget"] == nil {
		return nil, malformed("a simulated reading needs " + product + "'s test target")
	}
	all, err := r.Bindings(ctx, product)
	if err != nil {
		return nil, err
	}
	bindings := Object{}
	for _, b := range all {
		if b["test"] == simulated {
			bindings[pyjson.Text(b["ref"])] = b
		}
	}
	subject := pyjson.Map(bindings[pyjson.Text(reading["subject"])])
	if subject == nil || subject["kind"] != "issue" {
		test := ""
		if simulated {
			test = "test "
		}
		return nil, routeRefused("route_state_conflict", fmt.Sprintf("%s is not a bound %sissue of %s; a re-verification demand goes to the subject issue itself, so bind it as read back first", reading["subject"], test, product))
	}
	workspace := pyjson.Text(registry["workspace"])
	records := Object{}
	open := []any{}
	for _, name := range Checks {
		entry, err := r.mismatchRound(ctx, product, workspace, reading, name)
		if err != nil {
			return nil, err
		}
		id, err := r.Ledger.CanonicalID(ctx, product, "completion_unverified", completionSignature(reading, name, 1), workspace)
		if err != nil {
			return nil, err
		}
		entry["unverified"] = id
		entry["unverifiedRow"], err = r.Ledger.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		records[name] = entry
		if active(pyjson.Map(entry["mismatchRow"])["state"]) {
			open = append(open, name)
		}
	}
	recurrences, unknown, err := r.recurrences(ctx, product, pyjson.Text(reading["subject"]))
	if err != nil {
		return nil, err
	}
	r.test.read(ctx, "evaluate")
	answer := EvaluateCompletion(reading, Object{"bindings": bindings, "openMismatches": open, "recurrences": recurrences, "recurrenceUnknown": unknown})
	team, project := registry["team"], subject["project"]
	if project == nil {
		project = registry["triageProject"]
	}
	if simulated {
		team, project = pyjson.Map(registry["testTarget"])["team"], pyjson.Map(registry["testTarget"])["project"]
	}
	key, err := ReadingKey(reading)
	if err != nil {
		return nil, err
	}
	written := []any{}
	for _, v := range list(answer["checks"]) {
		entry := pyjson.Map(v)
		name, verdict := pyjson.Text(entry["check"]), pyjson.Text(entry["verdict"])
		ids := pyjson.Map(records[name])
		owner, _ := PlainTarget(Object{"team": team, "project": project, "owner": reading["subject"]})
		closed := []string{}
		for _, v := range list(ids["closed"]) {
			closed = append(closed, pyjson.Text(v))
		}
		replayed, err := r.routes().Replayed(ctx, closed, key)
		if err != nil {
			return nil, err
		}
		base := faults.Observation{Product: product, OccurrenceKey: key, Scope: scope(workspace, project), ObservedAt: reading["observedAt"], Detail: completionDetail(reading, entry), Evidence: completionEvidence(reading, name, key)}
		if verdict == "claim_withdrawn_without_closure" {
			written = append(written, Object{"check": name, "faultId": ids["mismatch"], "recorded": "standing", "ledgerState": pyjson.Map(ids["mismatchRow"])["state"]})
		} else if slices.Contains([]string{"mismatch", "exception_unverified", "requirement_changed_without_approval"}, verdict) {
			if replayed != nil {
				written = append(written, Object{"check": name, "faultId": replayed, "recorded": "replayed"})
			} else {
				if ids["mismatch"] == nil {
					return nil, routeRefused("route_state_conflict", fmt.Sprintf("%s of %s was closed 20 times and found wrong again; that is a decision for somebody, not another round", name, reading["subject"]))
				}
				if project == nil {
					return nil, routeRefused("route_state_conflict", fmt.Sprintf("%s is bound with no project and %s has no triage project; bind the subject with the project it is in, or register a triage project, and hand the reading in again", reading["subject"], product))
				}
				var adopt *faults.Adoption
				if pyjson.Map(ids["mismatchRow"]) == nil {
					adopt = &faults.Adoption{ExternalRef: pyjson.Text(reading["subject"]), Scope: scope(workspace, project)}
				}
				if _, err = r.Ledger.SetWorkspaceTarget(ctx, product, workspace, pyjson.Text(project), pyjson.Text(team), pyjson.Text(project)); err != nil {
					return nil, err
				}
				o := base
				o.FaultClass, o.Severity, o.Signature = "completion_mismatch", "degraded", pyjson.Map(ids["mismatchSignature"])
				if _, err = r.Ledger.RecordObservation(ctx, o, adopt); err != nil {
					return nil, err
				}
				id := pyjson.Text(ids["mismatch"])
				if err = r.routes().Upsert(ctx, Object{"fault_id": id, "product": product, "workspace": workspace, "disposition": "completion_mismatch", "stage": "filed", "target": owner, "origin": reading["origin"], "claimed_severity": "degraded", "detail": entry["reason"]}); err != nil {
					return nil, err
				}
				if err = r.routes().StoreIncident(ctx, id, Object{"occurrenceKey": key, "subject": reading["subject"], "check": name, "verdict": verdict, "observedAt": reading["observedAt"]}, nil, false); err != nil {
					return nil, err
				}
				row, err := r.Ledger.Get(ctx, id)
				if err != nil {
					return nil, err
				}
				written = append(written, Object{"check": name, "faultId": id, "recorded": verdict, "ledgerState": row["state"]})
			}
		}
		if verdict == "unverified" {
			o := base
			o.FaultClass, o.Severity, o.Signature = "completion_unverified", "notice", completionSignature(reading, name, 1)
			if _, err = r.Ledger.RecordObservation(ctx, o, nil); err != nil {
				return nil, err
			}
			id := pyjson.Text(ids["unverified"])
			if err = r.routes().Upsert(ctx, Object{"fault_id": id, "product": product, "workspace": workspace, "disposition": "completion_unverified", "stage": "observed", "target": owner, "origin": reading["origin"], "claimed_severity": "notice", "detail": entry["reason"]}); err != nil {
				return nil, err
			}
			written = append(written, Object{"check": name, "faultId": id, "recorded": "unverified"})
		} else if active(pyjson.Map(ids["unverifiedRow"])["state"]) {
			o := base
			o.FaultClass, o.Severity, o.Signature, o.Cleared = "completion_unverified", "notice", completionSignature(reading, name, 1), true
			if _, err = r.Ledger.RecordObservation(ctx, o, nil); err != nil {
				return nil, err
			}
			written = append(written, Object{"check": name, "faultId": ids["unverified"], "recorded": "unverified_cleared"})
		}
		closure := pyjson.Map(entry["closure"])
		if closure != nil && closure["ready"] == true && contains(open, name) {
			id := pyjson.Text(ids["mismatch"])
			fix, ref, outcome := "", "", "passed"
			if closure["exception"] != nil {
				fix = "exception: " + pyjson.Text(closure["exception"])
				ref, outcome = key, "absent"
			} else {
				fix = pyjson.Text(pyjson.Map(closure["fix"])["ref"])
				ref = pyjson.Text(pyjson.Map(closure["verification"])["ref"])
			}
			if _, err = r.Ledger.RecordRemediation(ctx, id, "fix", fix, "", "", "closure of a completion mismatch"); err != nil {
				return nil, err
			}
			if _, err = r.Ledger.RecordRemediation(ctx, id, "reverification", ref, "observation", outcome, "completion reading"); err != nil {
				return nil, err
			}
			if _, err = r.Ledger.ResolveRecord(ctx, id); err != nil {
				return nil, err
			}
			written = append(written, Object{"check": name, "faultId": id, "recorded": "closed"})
		}
	}
	answer["recorded"] = written
	return answer, nil
}
