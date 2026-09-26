package mergeturn

import (
	"context"
	"database/sql"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// The merge-turn-* relay commands (cli.py:1273-1380, :5051-5213), registered on the relay
// CLI's argparse surface. Every reader answers from a TargetReader, as the Python CLI builds
// MergeTurn(target_reader=TargetReader()).

type opt = registry.OptionSpec

func req(name string) opt  { return opt{Name: name, Required: true} }
func free(name string) opt { return opt{Name: name} }

func service(r *registry.Registry) *Service {
	return &Service{Store: r.Store, Registry: r, Now: r.Now, Delivery: StoreDelivery{Store: r.Store}}
}

func answer(v map[string]any, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return PythonOrder(v), nil
}

// badInvocation is the PayloadExit cli.py raises for an argument a handler refuses.
func badInvocation(detail string) error {
	return registry.PayloadExit(contract.OrderedObject{{Key: "ok", Value: false}, {Key: "reason", Value: "bad_invocation"}, {Key: "detail", Value: detail}}, contract.ExitRefused)
}

// jsonShape is cli._json_shape: decoded AND the shape begin_merge indexes into.
func jsonShape(raw, what string, wantList bool) (any, error) {
	value, err := registry.DecodeJSON(raw)
	if err != nil {
		return nil, badInvocation(what + " must be JSON: " + err.Error())
	}
	if wantList {
		items, ok := value.([]any)
		for _, item := range items {
			if _, isObject := item.(contract.OrderedObject); !isObject {
				ok = false
			}
		}
		if !ok {
			return nil, badInvocation(what + " must be a JSON list of objects")
		}
		return items, nil
	}
	if _, ok := value.(contract.OrderedObject); !ok {
		return nil, badInvocation(what + " must be a JSON object")
	}
	return value, nil
}

func show(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
	var named []string
	if p.Text("turn") != "" {
		named = append(named, "--turn")
	}
	if p.Text("parent-task") != "" {
		named = append(named, "--parent-task")
	}
	if p.Text("repository") != "" || p.Text("base-ref") != "" {
		named = append(named, "--repository with --base-ref")
	}
	if len(named) != 1 {
		which := "none"
		if len(named) > 0 {
			which = named[0]
			for _, n := range named[1:] {
				which += ", " + n
			}
		}
		return nil, badInvocation("merge-turn-show takes exactly one selector - --turn, --parent-task, or --repository with --base-ref - and this named " + which)
	}
	if (p.Text("repository") == "") != (p.Text("base-ref") == "") {
		return nil, badInvocation("a target is a repository AND a base ref; --repository and --base-ref are given together or not at all")
	}
	m := service(r)
	var result any
	switch {
	case p.Text("turn") != "":
		turn, err := m.Turn(ctx, p.Text("turn"))
		if err != nil {
			return nil, err
		}
		if turn == nil {
			return registry.WithEnforcement(r, contract.OrderedObject{{Key: "ok", Value: false}, {Key: "reason", Value: "unregistered_scope"}, {Key: "turnId", Value: p.Text("turn")}}), nil
		}
		result = turn
	case p.Text("parent-task") != "":
		claims, err := m.Outstanding(ctx, p.Text("parent-task"))
		if err != nil {
			return nil, err
		}
		result = map[string]any{"parentTaskId": p.Text("parent-task"), "claims": claims}
	default:
		target, err := m.Target(ctx, p.Text("repository"), p.Text("base-ref"))
		if err != nil {
			return nil, err
		}
		result = target
	}
	return registry.WithEnforcement(r, PythonOrder(result).(contract.OrderedObject)), nil
}

func optional(p registry.Parsed, name string) string { return p.Optional(name).String }

func init() {
	reader := TargetReader{}
	add := registry.AddCommand
	add("merge-turn-request", []opt{req("repository"), req("base-ref"), req("project"), req("task"), req("host"), free("cwd"), free("cxc-session"), req("head"), {Name: "pr", Integer: true}, free("relationship"), {Name: "ready", Flag: true}}, nil,
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			options := ClaimOptions{Relationship: p.Optional("relationship")}
			if p.Given("pr") {
				options.PR = sql.NullInt64{Int64: p.Integer("pr"), Valid: true}
			}
			return answer(service(r).Request(ctx, p.Text("repository"), p.Text("base-ref"), p.Text("project"), p.Text("task"), p.Text("host"), p.Text("head"), p.Given("ready"), options))
		})
	add("merge-turn-ready", []opt{req("turn"), req("actor"), free("head"), free("cause"), {Name: "ready", Flag: true}, {Name: "not-ready", Flag: true}}, []string{"ready", "not-ready"},
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			return answer(service(r).Ready(ctx, p.Text("turn"), p.Text("actor"), p.Given("ready"), p.Text("head"), p.Text("cause")))
		})
	add("merge-turn-acknowledge", []opt{req("turn"), req("actor"), req("grant"), req("evidence")}, nil,
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			return answer(service(r).Acknowledge(ctx, p.Text("turn"), p.Text("actor"), p.Text("grant"), p.Text("evidence")))
		})
	add("merge-turn-attest", []opt{req("turn"), req("evidence-kind"), req("idempotency-key"), req("actor"), req("evidence")}, nil,
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			return answer(service(r).Attest(ctx, p.Text("turn"), p.Text("evidence-kind"), p.Text("idempotency-key"), p.Text("actor"), p.Text("evidence")))
		})
	add("merge-turn-request-return", []opt{req("turn"), req("actor"), req("evidence")}, nil,
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			actor := p.Text("actor")
			return answer(service(r).Attest(ctx, p.Text("turn"), "return_requested", "return_requested:"+actor, actor, p.Text("evidence")))
		})
	add("merge-turn-check", []opt{req("turn"), req("actor"), req("head-sha"), req("base-sha"), req("checks"), req("review"), {Name: "required", Multi: true}}, nil,
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			checks, err := jsonShape(p.Text("checks"), "--checks", true)
			if err != nil {
				return nil, err
			}
			review, err := jsonShape(p.Text("review"), "--review", false)
			if err != nil {
				return nil, err
			}
			return answer(service(r).Check(ctx, p.Text("turn"), p.Text("actor"), p.Text("head-sha"), p.Text("base-sha"), checks, review, p.Values("required"), reader))
		})
	add("merge-turn-land", []opt{req("turn"), req("actor"), req("landed-sha"), free("observed-base-sha"), req("evidence")}, nil,
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			return answer(service(r).Land(ctx, p.Text("turn"), p.Text("actor"), p.Text("landed-sha"), optional(p, "observed-base-sha"), p.Text("evidence"), reader))
		})
	add("merge-turn-unknown", []opt{req("turn"), req("actor"), req("reason")}, nil,
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			return answer(service(r).Unknown(ctx, p.Text("turn"), p.Text("actor"), p.Text("reason")))
		})
	add("merge-turn-resolve", []opt{req("turn"), req("actor"), req("observed-base-sha"), {Name: "pr-state", Required: true, Choices: []string{"merged", "open", "closed"}}, req("evidence")}, nil,
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			return answer(service(r).Resolve(ctx, p.Text("turn"), p.Text("actor"), p.Text("observed-base-sha"), p.Text("pr-state"), p.Text("evidence"), reader))
		})
	add("merge-turn-restate-base", []opt{req("turn"), req("actor"), free("observed-base-sha"), req("evidence")}, nil,
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			return answer(service(r).RestateBase(ctx, p.Text("turn"), p.Text("actor"), optional(p, "observed-base-sha"), p.Text("evidence"), reader))
		})
	add("merge-turn-release", []opt{req("turn"), req("actor"), {Name: "disposition", Required: true, Choices: []string{"returned", "cancelled"}}, req("reason"), free("evidence")}, nil,
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			return answer(service(r).Release(ctx, p.Text("turn"), p.Text("actor"), p.Text("disposition"), p.Text("reason"), p.Text("evidence")))
		})
	add("merge-turn-withdraw", []opt{req("turn"), req("actor")}, nil,
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			return answer(service(r).Withdraw(ctx, p.Text("turn"), p.Text("actor")))
		})
	add("merge-turn-show", []opt{free("turn"), free("repository"), free("base-ref"), free("parent-task")}, nil, show)
}
