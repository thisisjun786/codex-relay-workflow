package mergeturn

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// The merge-turn-* relay commands (cli.py:1273-1380, :5051-5213), registered on the relay
// CLI's argparse surface. Every reader answers from a TargetReader, as the Python CLI builds
// MergeTurn(target_reader=TargetReader()).

func service(r *registry.Registry) *Service {
	return &Service{Store: r.Store, Registry: r, Now: r.Now, Delivery: StoreDelivery{Store: r.Store}}
}

func answer(v map[string]any, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return plain(v), nil
}

// badInvocation is the PayloadExit cli.py raises for an argument a handler refuses.
func badInvocation(detail string) error {
	return &dispatch.PayloadExit{Payload: contract.OrderedObject{{Key: "ok", Value: false}, {Key: "reason", Value: "bad_invocation"}, {Key: "detail", Value: detail}}, Code: contract.ExitRefused}
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

// showSelectors is cmd_merge_turn_show's refusal of its selectors, which it raises before
// services.merge_turn opens the store: merge-turn-show is a read-only form, so on an absent
// store this refusal, not store_absent, is the answer.
func showSelectors(p registry.Parsed) error {
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
		return badInvocation("merge-turn-show takes exactly one selector - --turn, --parent-task, or --repository with --base-ref - and this named " + which)
	}
	if (p.Text("repository") == "") != (p.Text("base-ref") == "") {
		return badInvocation("a target is a repository AND a base ref; --repository and --base-ref are given together or not at all")
	}
	return nil
}

// show is cmd_merge_turn_show after showSelectors.
func show(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
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
	return registry.WithEnforcement(r, plain(result).(contract.OrderedObject)), nil
}

func optional(p registry.Parsed, name string) string { return p.Optional(name).String }

func init() {
	reader := TargetReader{}
	add := func(name string, run func(context.Context, *registry.Registry, registry.Parsed) (any, error)) {
		registry.AddCommand(dispatch.Command{Name: name}, run)
	}
	add("merge-turn-request",
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			options := ClaimOptions{Relationship: p.Optional("relationship")}
			if p.Given("pr") {
				options.PRValue = p.Integer("pr")
			}
			return answer(service(r).Request(ctx, p.Text("repository"), p.Text("base-ref"), p.Text("project"), p.Text("task"), p.Text("host"), p.Text("head"), p.Given("ready"), options))
		})
	add("merge-turn-ready",
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			return answer(service(r).Ready(ctx, p.Text("turn"), p.Text("actor"), p.Given("ready"), p.Text("head"), p.Text("cause")))
		})
	add("merge-turn-acknowledge",
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			return answer(service(r).Acknowledge(ctx, p.Text("turn"), p.Text("actor"), p.Text("grant"), p.Text("evidence")))
		})
	add("merge-turn-attest",
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			return answer(service(r).Attest(ctx, p.Text("turn"), p.Text("evidence-kind"), p.Text("idempotency-key"), p.Text("actor"), p.Text("evidence")))
		})
	add("merge-turn-request-return",
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			actor := p.Text("actor")
			return answer(service(r).Attest(ctx, p.Text("turn"), "return_requested", "return_requested:"+actor, actor, p.Text("evidence")))
		})
	add("merge-turn-check",
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
	add("merge-turn-land",
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			return answer(service(r).Land(ctx, p.Text("turn"), p.Text("actor"), p.Text("landed-sha"), optional(p, "observed-base-sha"), p.Text("evidence"), reader))
		})
	add("merge-turn-unknown",
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			return answer(service(r).Unknown(ctx, p.Text("turn"), p.Text("actor"), p.Text("reason")))
		})
	add("merge-turn-resolve",
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			return answer(service(r).Resolve(ctx, p.Text("turn"), p.Text("actor"), p.Text("observed-base-sha"), p.Text("pr-state"), p.Text("evidence"), reader))
		})
	add("merge-turn-restate-base",
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			return answer(service(r).RestateBase(ctx, p.Text("turn"), p.Text("actor"), optional(p, "observed-base-sha"), p.Text("evidence"), reader))
		})
	add("merge-turn-release",
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			return answer(service(r).Release(ctx, p.Text("turn"), p.Text("actor"), p.Text("disposition"), p.Text("reason"), p.Text("evidence")))
		})
	add("merge-turn-withdraw",
		func(ctx context.Context, r *registry.Registry, p registry.Parsed) (any, error) {
			return answer(service(r).Withdraw(ctx, p.Text("turn"), p.Text("actor")))
		})
	registry.AddCheckedCommand(dispatch.Command{Name: "merge-turn-show", ReadOnly: true}, showSelectors, show)
}
