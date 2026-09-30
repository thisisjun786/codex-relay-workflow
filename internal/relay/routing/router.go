package routing

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Router reads product snapshots and records decisions atomically with the existing ledger.
type Router struct {
	Store  *store.Store
	Clock  faults.Clock
	Ledger *faults.Ledger
	// test holds the seams the package's tests set; production leaves it zero.
	test routerSeams
}

// routerSeams let this package's tests observe transaction-bound decisions, inject failures and
// page the digest in smaller pages; each is a no-op while unset.
type routerSeams struct {
	beforeWrite      func(context.Context, string) error
	beforeDigestPage func(context.Context, int) error
	digestPageSize   int64
	decisionRead     func(context.Context, string)
}

func (s routerSeams) write(ctx context.Context, step string) error {
	if s.beforeWrite == nil {
		return nil
	}
	return s.beforeWrite(ctx, step)
}

func (s routerSeams) digestPage(ctx context.Context, page int) error {
	if s.beforeDigestPage == nil {
		return nil
	}
	return s.beforeDigestPage(ctx, page)
}

func (s routerSeams) read(ctx context.Context, decision string) {
	if s.decisionRead != nil {
		s.decisionRead(ctx, decision)
	}
}

func New(s *store.Store, clock faults.Clock) *Router {
	faults.InstallProductDeclarations()
	return &Router{Store: s, Clock: clock, Ledger: &faults.Ledger{Store: s, Clock: clock}}
}
func (r *Router) routes() RouteStore           { return RouteStore{Store: r.Store, Clock: r.Clock} }
func routeRefused(reason, detail string) error { return &Refusal{reason, detail} }
func (r *Router) Registry(ctx context.Context, product string) (Object, error) {
	row, err := r.Store.One(ctx, "SELECT record FROM product_registry WHERE product_key = ?", product)
	if err != nil || row == nil {
		return nil, err
	}
	return readStored(row.Get("record"))
}
func (r *Router) Registries(ctx context.Context) (map[string]Object, error) {
	rows, err := r.Store.ProductRegistries(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]Object{}
	for _, row := range rows {
		v, err := readStored(row.Record)
		if err != nil {
			return nil, err
		}
		out[row.ProductKey] = v
	}
	return out, nil
}
func (r *Router) Bindings(ctx context.Context, product string) ([]Object, error) {
	rows, err := r.Store.ProductBindings(ctx, product)
	if err != nil {
		return nil, err
	}
	out := []Object{}
	for _, row := range rows {
		v, err := readStored(row.Record)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
func (r *Router) Policy(ctx context.Context) (Object, error) {
	row, err := r.Store.One(ctx, "SELECT record FROM routing_policy WHERE policy_key = ?", "project_creation")
	if err != nil || row == nil {
		return nil, err
	}
	return readStored(row.Get("record"))
}
func (r *Router) RegisterProduct(ctx context.Context, value any) (Object, error) {
	registry, err := ReadRegistry(value)
	if err != nil {
		return nil, err
	}
	var changed []any
	revised := Object{"cancelled": []any{}, "queued": []any{}}
	err = r.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		product := text(registry["product"])
		before, err := r.Registry(ctx, product)
		if err != nil {
			return err
		}
		if before != nil && before["workspace"] != registry["workspace"] {
			row, err := r.Store.One(ctx, "SELECT 1 FROM incident_routes WHERE product_key = ? LIMIT 1", product)
			if err != nil {
				return err
			}
			if row != nil {
				return routeRefused("route_state_conflict", fmt.Sprintf("%s has routed faults in workspace %s; the workspace is part of their identity, so a registry naming %s would file the same defects again", product, before["workspace"], registry["workspace"]))
			}
		}
		if before != nil && !equal(before["testTarget"], registry["testTarget"]) {
			used, err := r.Store.One(ctx, "SELECT 1 FROM incident_routes WHERE product_key=? AND origin='simulated' LIMIT 1", product)
			if err != nil {
				return err
			}
			bindings, err := r.Bindings(ctx, product)
			if err != nil {
				return err
			}
			test := used != nil
			for _, b := range bindings {
				test = test || b["test"] == true
			}
			if test {
				return routeRefused("route_state_conflict", fmt.Sprintf("%s has simulated routes or test bindings on its test target %s; they would be left on a target the product no longer names", product, evidence.Repr(before["testTarget"])))
			}
		}
		target := object(registry["testTarget"])
		if target != nil {
			real := []string{}
			bindings, err := r.Bindings(ctx, product)
			if err != nil {
				return err
			}
			for _, b := range bindings {
				where := b["project"]
				if b["kind"] == "project" {
					where = b["ref"]
				}
				if b["test"] != true && where == target["project"] {
					real = append(real, text(b["ref"]))
				}
			}
			rows, err := r.Store.All(ctx, "SELECT fault_id FROM incident_routes WHERE product_key=? AND origin!='simulated' AND json_extract(target, '$.project')=? LIMIT 5", product, target["project"])
			if err != nil {
				return err
			}
			for _, row := range rows {
				real = append(real, text(row.Get("fault_id")))
			}
			if len(real) > 0 {
				return routeRefused("route_state_conflict", fmt.Sprintf("%s does real work in %s (%s); a test target there would share that project's target with it", product, target["project"], strings.Join(real, ", ")))
			}
		}
		encoded, err := Canonical(registry)
		if err != nil {
			return err
		}
		if err = r.Store.RegisterProduct(ctx, product, encoded, r.Clock.ISO()); err != nil {
			return err
		}
		changed = []any{}
		if before != nil {
			changed, err = r.redecide(ctx, product)
			if err != nil {
				return err
			}
			evaluation, err := r.evaluateProjects(ctx, product)
			if err != nil {
				return err
			}
			revised = Object{"cancelled": evaluation["cancelled"], "queued": evaluation["queued"]}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := clone(registry)
	out["redecided"], out["projectsRevised"] = changed, revised
	return out, nil
}
func (r *Router) Bind(ctx context.Context, value any) (Object, error) {
	var binding Object
	var changed, queued any
	err := r.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		product := object(value)["product"]
		registry, err := r.Registry(ctx, text(product))
		if err != nil {
			return err
		}
		if registry == nil {
			return routeRefused("route_product_unknown", fmt.Sprintf("%s is not a registered product; register it first", evidence.Repr(product)))
		}
		r.test.read(ctx, "binding")
		binding, err = ReadBinding(value, registry)
		if err != nil {
			return err
		}
		encoded, err := Canonical(binding)
		if err != nil {
			return err
		}
		if err = r.Store.BindProduct(ctx, store.ProductBindingsRow{ProductKey: text(binding["product"]), Kind: text(binding["kind"]), Ref: text(binding["ref"]), Record: encoded, ObservedAt: nullText(binding["observedAt"]), RecordedAt: r.Clock.ISO()}); err != nil {
			return err
		}
		changed, err = r.redecide(ctx, text(product))
		if err != nil {
			return err
		}
		evaluation, err := r.evaluateProjects(ctx, text(product))
		if err != nil {
			return err
		}
		queued = evaluation["queued"]
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := clone(binding)
	out["redecided"], out["projectsQueued"] = changed, queued
	return out, nil
}
func (r *Router) SetPolicy(ctx context.Context, value any) (Object, error) {
	policy, err := ReadPolicy(value)
	if err != nil {
		return nil, err
	}
	withdrawn := Object{}
	err = r.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		encoded, err := Canonical(policy)
		if err != nil {
			return err
		}
		if err = r.Store.SetRoutingPolicy(ctx, "project_creation", encoded, text(policy["basis"]), r.Clock.ISO()); err != nil {
			return err
		}
		registries, err := r.Registries(ctx)
		if err != nil {
			return err
		}
		for _, key := range registryKeys(registries) {
			cancelled, err := r.withdrawProjects(ctx, key)
			if err != nil {
				return err
			}
			if len(cancelled) > 0 {
				withdrawn[key] = cancelled
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := clone(policy)
	out["withdrawn"] = withdrawn
	return out, nil
}
func (r *Router) ShowProducts(ctx context.Context, product any) (Object, error) {
	registries, err := r.Registries(ctx)
	if err != nil {
		return nil, err
	}
	if product != nil {
		registry, ok := registries[text(product)]
		if !ok {
			return nil, routeRefused("route_product_unknown", fmt.Sprintf("%s is not a registered product", evidence.Repr(product)))
		}
		registries = map[string]Object{text(product): registry}
	}
	out := []any{}
	for _, key := range registryKeys(registries) {
		record := clone(registries[key])
		record["coverage"] = Coverage(record)
		bindings, err := r.Bindings(ctx, key)
		if err != nil {
			return nil, err
		}
		record["bindings"] = bindings
		out = append(out, record)
	}
	policy, err := r.Policy(ctx)
	if err != nil {
		return nil, err
	}
	var p any
	if policy != nil {
		p = policy
	}
	return Object{"products": out, "policy": p}, nil
}
func (r *Router) RunIssue(ctx context.Context, run any) (any, error) {
	if text(run) == "" {
		return nil, nil
	}
	row, err := r.Store.One(ctx, "SELECT issue_key FROM relationships WHERE relationship_id=?", run)
	if err != nil || row == nil {
		return nil, err
	}
	return row.Get("issue_key"), nil
}
func clone(m Object) Object {
	out := Object{}
	for k, v := range m {
		out[k] = v
	}
	return out
}
func equal(a, b any) bool {
	return pyjson.Dumps(a, pyjson.Options{Compact: true, SortKeys: true, Unicode: true}) == pyjson.Dumps(b, pyjson.Options{Compact: true, SortKeys: true, Unicode: true})
}
func registryKeys(registries map[string]Object) []string {
	m := Object{}
	for k := range registries {
		m[k] = true
	}
	return sortedKeys(m)
}
func active(state any) bool { return state == "observed" || state == "open" || state == "fix_pending" }
func snapshot(route, fault Object) Object {
	target := object(route["target"])
	count := fault["occurrence_count"]
	if count == nil {
		count = int64(0)
	}
	return Object{"stage": route["stage"], "disposition": route["disposition"], "hold": target["hold"], "project": target["project"], "unverifiedCause": target["unverifiedCause"], "claimedSeverity": route["claimed_severity"], "state": fault["state"], "severity": fault["severity"], "occurrenceCount": count, "externalRef": fault["external_ref"], "linkState": fault["linkState"]}
}
func (r *Router) Show(ctx context.Context, product any, attention bool, limit, after any) (Object, error) {
	page, err := r.routes().Listing(ctx, product, nil, nil, limit, after)
	if err != nil {
		return nil, err
	}
	shown, proposals := []any{}, []any{}
	for _, value := range list(page["routes"]) {
		route := object(value)
		fault, err := r.Ledger.Get(ctx, text(route["fault_id"]))
		if err != nil {
			return nil, err
		}
		now := snapshot(route, fault)
		waiting := Attention(now)
		target := object(route["target"])
		entry := Object{"faultId": route["fault_id"], "product": route["product_key"], "workspace": route["workspace"], "disposition": route["disposition"], "stage": route["stage"], "hold": target["hold"], "unverifiedCause": target["unverifiedCause"], "project": target["project"], "owner": target["owner"], "team": target["team"], "origin": route["origin"], "classification": route["classification"], "supersededBy": route["superseded_by"], "detail": route["detail"], "attention": waiting, "ledger": Object{"state": now["state"], "severity": now["severity"], "occurrences": now["occurrenceCount"], "issue": now["externalRef"], "linkState": now["linkState"], "linkedProject": fault["linkedProject"]}}
		if !attention || waiting != nil {
			if route["disposition"] == "project_proposal" {
				proposals = append(proposals, entry)
			} else {
				shown = append(shown, entry)
			}
		}
	}
	return Object{"routes": shown, "projects": proposals, "attention": attention, "next": page["next"], "limits": "routing's rows and the ledger's state in this store only; a queued write is not an issue anybody has written, and a confirmed one is not an issue anybody read."}, nil
}
