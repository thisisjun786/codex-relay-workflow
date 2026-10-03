package adapter

// WorkerObservation is the worker-policy reader managed-start admits through. The selectors are
// injected by the CLI and this never resolves host state from the environment; the reading itself
// is the service's (service.ObserveWorkerPolicy), the one reader of the worker's receipt.
import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type WorkerObservation struct{ State, Socket, Scope, Authority, Installation string }

// Read is the worker's policy and "" when the worker was observed, else nil and the reason it was
// not.
func (o WorkerObservation) Read(ctx context.Context) (map[string]any, string) {
	observation := service.ObserveWorkerPolicy(ctx, store.StateSelection{Path: o.State}, o.Socket, o.Installation, &service.ScopeRegistry{Root: o.Scope, Authority: o.Authority})
	if reason, _ := observation.Get("reason").(string); reason != "" {
		return nil, reason
	}
	policy, _ := plain(observation.Get("policy")).(map[string]any)
	return policy, ""
}
func (o WorkerObservation) Ready(ctx context.Context, request map[string]any, policy registry.RolePolicy) (string, error) {
	worker, reason := o.Read(ctx)
	if reason != "" {
		return reason, nil
	}
	if worker["state"] != "declared" {
		return "worker_policy_unconfigured", nil
	}
	if !policy.Declared {
		return "caller_policy_unconfigured", nil
	}
	if worker["digest"] != policy.Digest() {
		return "worker_policy_digest_mismatch", nil
	}
	if dumps(ordered(worker), true) != dumps(policy.Summary(), true) {
		return "worker_policy_summary_mismatch", nil
	}
	roles, _ := worker["roles"].(map[string]any)
	for _, role := range []string{"parent", "child"} {
		endpoint, _ := request[role].(map[string]any)
		settings, _ := endpoint["settings"].(map[string]any)
		expected, _ := roles[role].(map[string]any)
		if expected["expectation"] != "pair" {
			return "worker_policy_role_unsupported", nil
		}
		if !registry.SummaryAllowsPair(expected, settings["model"], settings["reasoningEffort"]) {
			return "worker_policy_pair_mismatch", nil
		}
		finding := registry.CheckRecord(ordered(settings).(contract.OrderedObject), role, policy)
		if finding != nil {
			return pyjson.Text(finding.Get("code")), nil
		}
		if role == "child" {
			if reason := mcpAdmission(policy, settings["mcpProfile"]); reason != "" {
				return reason, nil
			}
		}
	}
	return "", nil
}

// mcpAdmission is why a child's stated MCP profile is not admitted, or "". A role that declares
// profiles admits a child only when its request states one of them, so the child's record names the
// profile every later resume sends again; a role that declares none admits a child that states none.
func mcpAdmission(policy registry.RolePolicy, stated any) string {
	declared, ok := policy.BridgePolicy().Role("child")
	if !ok || declared.MCP == nil {
		if stated == nil {
			return ""
		}
		return "mcp_profile_unknown"
	}
	if stated == nil {
		return "mcp_profile_required"
	}
	name, _ := stated.(string)
	if _, listed := declared.MCP.Profiles[name]; !listed {
		return "mcp_profile_unknown"
	}
	return ""
}
