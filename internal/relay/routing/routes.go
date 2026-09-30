package routing

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

var targetKeys = strings.Fields("team project owner relate hold labels cause unverifiedCause obligations")

// RouteStore owns routing's rows only. Fault state and publication writes remain the ledger's.
type RouteStore struct {
	Store *store.Store
	Clock interface{ ISO() string }
}

func PlainTarget(fields Object) (Object, error) {
	v := &validator{}
	v.closed(fields, targetKeys, "target")
	if v.err != nil {
		return nil, v.err
	}
	out := Object{"team": nil, "project": nil, "owner": nil, "relate": []any{}, "hold": nil, "labels": []any{}, "cause": nil, "unverifiedCause": nil, "obligations": []any{}}
	for k, v := range fields {
		out[k] = v
	}
	return out, nil
}
func decodeRoute(row store.Row) (Object, error) {
	if row == nil {
		return nil, nil
	}
	out := Object{}
	for _, column := range row {
		out[column.Name] = column.Value
	}
	for _, key := range []string{"target", "classification", "reported"} {
		if out[key] != nil && out[key] != "" {
			value, err := readStored(out[key])
			if err != nil {
				return nil, err
			}
			out[key] = value
		} else {
			out[key] = nil
		}
	}
	unknown := []string{}
	for _, key := range sortedKeys(object(out["target"])) {
		found := false
		for _, k := range targetKeys {
			if key == k {
				found = true
				break
			}
		}
		if !found {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		return nil, &Refusal{"route_state_conflict", fmt.Sprintf("route %s carries target keys %s", out["fault_id"], evidence.Repr(unknown))}
	}
	return out, nil
}
func (r RouteStore) Get(ctx context.Context, id string) (Object, error) {
	row, err := r.Store.One(ctx, "SELECT * FROM incident_routes WHERE fault_id = ?", id)
	if err != nil {
		return nil, err
	}
	return decodeRoute(row)
}
func nullText(value any) sql.NullString {
	s, ok := value.(string)
	return sql.NullString{String: s, Valid: ok}
}
func (r RouteStore) Upsert(ctx context.Context, value Object) error {
	target, err := Canonical(value["target"])
	if err != nil {
		return err
	}
	var classification sql.NullString
	if value["classification"] != nil {
		s, err := Canonical(value["classification"])
		if err != nil {
			return err
		}
		classification = sql.NullString{String: s, Valid: true}
	}
	_, replace := value["goal"]
	stamp := r.Clock.ISO()
	return r.Store.UpsertIncidentRoute(ctx, store.IncidentRoutesRow{FaultID: text(value["fault_id"]), ProductKey: text(value["product"]), Workspace: text(value["workspace"]), Disposition: text(value["disposition"]), Stage: text(value["stage"]), Target: target, Origin: text(value["origin"]), ClaimedSeverity: text(value["claimed_severity"]), Goal: nullText(value["goal"]), Classification: classification, SupersededBy: nullText(value["superseded_by"]), Detail: sql.NullString{String: text(value["detail"]), Valid: true}, CreatedAt: stamp, UpdatedAt: stamp}, replace)
}
func incidentID(fault, key string) string {
	sum := sha256.Sum256([]byte(fault + "|" + key))
	return fmt.Sprintf("%x", sum)[:32]
}
func (r RouteStore) StoreIncident(ctx context.Context, fault string, incident Object, keep *int64, replace bool) error {
	record, err := Canonical(incident)
	if err != nil {
		return err
	}
	return r.Store.StoreRouteIncident(ctx, incidentID(fault, text(incident["occurrenceKey"])), fault, record, r.Clock.ISO(), keep, replace)
}
func (r RouteStore) Incidents(ctx context.Context, fault string) ([]any, error) {
	rows, err := r.Store.RouteIncidents(ctx, fault)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, row := range rows {
		value, err := readStored(row.Record)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}
func (r RouteStore) Replayed(ctx context.Context, closed []string, key string) (any, error) {
	for _, fault := range closed {
		row, err := r.Store.One(ctx, "SELECT incident_id FROM route_incidents WHERE incident_id = ?", incidentID(fault, key))
		if err != nil {
			return nil, err
		}
		if row != nil {
			return fault, nil
		}
	}
	return nil, nil
}
func (r RouteStore) Listing(ctx context.Context, product any, stages, dispositions []string, limit, after any) (Object, error) {
	bound, after, err := ReadPage(limit, after, 1000)
	if err != nil {
		return nil, err
	}
	clauses := []string{}
	args := []any{}
	if product != nil {
		clauses = append(clauses, "product_key = ?")
		args = append(args, product)
	}
	add := func(name string, values []string) {
		if len(values) > 0 {
			slots := []string{}
			for _, v := range values {
				slots = append(slots, "?")
				args = append(args, v)
			}
			clauses = append(clauses, name+" IN ("+strings.Join(slots, ",")+")")
		}
	}
	add("stage", stages)
	add("disposition", dispositions)
	if after != nil {
		clauses = append(clauses, "rowid > ?")
		args = append(args, after)
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	args = append(args, bound+1)
	rows, err := r.Store.All(ctx, "SELECT rowid AS seq, * FROM incident_routes"+where+" ORDER BY rowid LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	page := []any{}
	for i, row := range rows {
		if int64(i) >= bound {
			break
		}
		value, err := decodeRoute(row)
		if err != nil {
			return nil, err
		}
		page = append(page, value)
	}
	var next any
	if int64(len(rows)) > bound {
		next = object(page[len(page)-1])["seq"]
	}
	return Object{"routes": page, "next": next}, nil
}
func (r RouteStore) OutstandingProposals(ctx context.Context, limit any) ([]any, error) {
	bound, _, err := ReadPage(limit, nil, 5000)
	if err != nil {
		return nil, err
	}
	rows, err := r.Store.All(ctx, "SELECT rowid AS seq, * FROM incident_routes WHERE stage = ? AND disposition = ? ORDER BY checked_seq, rowid LIMIT ?", "filed", "project_proposal", bound)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, row := range rows {
		value, err := decodeRoute(row)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}

const unreached = "FROM incident_routes WHERE stage = ? AND disposition = ? AND fault_id NOT IN (SELECT value FROM json_each(?))"

func (r RouteStore) UnreachedProposal(ctx context.Context, product string, goal any, reached []any) (any, error) {
	if goal == nil {
		return nil, nil
	}
	row, err := r.Store.One(ctx, "SELECT fault_id "+unreached+" AND product_key = ? AND goal = ? ORDER BY rowid LIMIT 1", "filed", "project_proposal", pyjson.Dumps(reached, pyjson.Options{}), product, goal)
	if err != nil || row == nil {
		return nil, err
	}
	return row.Get("fault_id"), nil
}
func (r RouteStore) UnreachedCount(ctx context.Context, reached []any) (int64, error) {
	row, err := r.Store.One(ctx, "SELECT COUNT(*) AS n FROM (SELECT DISTINCT product_key, goal "+unreached+")", "filed", "project_proposal", pyjson.Dumps(reached, pyjson.Options{}))
	if err != nil {
		return 0, err
	}
	return row.Get("n").(int64), nil
}
