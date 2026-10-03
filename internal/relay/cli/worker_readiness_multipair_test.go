package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-454: a worker whose policy lets the child run on two pairs is ready for a child on either
// pair, and for no other; a role with one pair is asked as before.
func TestWorkerReadinessAcceptsEveryPairOfAMultiPairRole(t *testing.T) {
	text := strings.ReplaceAll("{'roles':{'parent':{'model':'anthropic/claude-opus-5-5','reasoningEffort':'xhigh'},"+
		"'child':{'pairs':[{'model':'anthropic/claude-sonnet-5-5','reasoningEffort':'xhigh'},{'model':'gpt-6.1-sol','reasoningEffort':'xhigh'}]}}}", "'", "\"")
	file := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	summary := registry.ResolveRolePolicy(map[string]string{execution.EnvPolicy: file}).Summary()
	observation := contract.OrderedObject{{Key: "observed", Value: true}, {Key: "reason", Value: nil}, {Key: "policy", Value: summary}}
	requirements := func(role, model, effort string) any {
		value, err := store.LoadsJSON([]byte(strings.ReplaceAll("[{'role':'"+role+"','model':'"+model+"','reasoningEffort':'"+effort+"'}]", "'", "\"")))
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	reason := func(role, model, effort string) any {
		answer := cli.WorkerReadiness(observation, requirements(role, model, effort), file)
		for _, field := range answer {
			if field.Key == "reason" {
				return field.Value
			}
		}
		return "no reason field"
	}
	for _, row := range []struct {
		role, model, effort string
		want                any
	}{
		{"child", "anthropic/claude-sonnet-5-5", "xhigh", nil},
		{"child", "gpt-6.1-sol", "xhigh", nil},
		{"parent", "anthropic/claude-opus-5-5", "xhigh", nil},
		{"child", "gpt-6.1-sol", "high", "worker_policy_pair_mismatch"},
		{"child", "anthropic/claude-opus-5-5", "xhigh", "worker_policy_pair_mismatch"},
		{"parent", "gpt-6.1-sol", "xhigh", "worker_policy_pair_mismatch"},
	} {
		if got := reason(row.role, row.model, row.effort); got != row.want {
			t.Errorf("%s %s %s: reason %v, want %v", row.role, row.model, row.effort, got, row.want)
		}
	}
}
