package adapter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// CRW-454: managed-start admits a child on any pair its role declares. The built relay CLI runs
// over the scripted app-server with a policy that lets the child run on Sonnet or on SOL.

var twoPairManagedPolicy = strings.ReplaceAll("{'allowed':[{'model':'anthropic/claude-opus-5-5','efforts':['xhigh']},{'model':'anthropic/claude-sonnet-5-5','efforts':['xhigh']},{'model':'gpt-6.1-sol','efforts':['xhigh']}],"+
	"'roles':{'parent':{'model':'anthropic/claude-opus-5-5','reasoningEffort':'xhigh'},"+
	"'child':{'pairs':[{'model':'anthropic/claude-sonnet-5-5','reasoningEffort':'xhigh'},{'model':'gpt-6.1-sol','reasoningEffort':'xhigh'}]}}}", "'", "\"")

// parentOn is a request change that puts the parent's settings on the parent's own pair.
func (c *managedCLI) parentOn(model, effort string) func(map[string]any) {
	return func(request map[string]any) {
		settings := map[string]any{}
		for k, v := range c.settings {
			settings[k] = v
		}
		settings["model"], settings["reasoningEffort"] = model, effort
		request["parent"].(map[string]any)["settings"] = settings
	}
}

func TestManagedStartCreatesTheChildOnEitherPairOfATwoPairRole(t *testing.T) {
	for _, pair := range []struct{ name, model string }{{"sonnet", "anthropic/claude-sonnet-5-5"}, {"sol", "gpt-6.1-sol"}} {
		t.Run(pair.name, func(t *testing.T) {
			c := newManagedCLIOn(t, twoPairManagedPolicy, pair.model, "xhigh")
			stdout, stderr, code := c.start(c.request(c.parentOn("anthropic/claude-opus-5-5", "xhigh")))
			var admitted map[string]any
			if err := json.Unmarshal([]byte(stdout), &admitted); err != nil {
				t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
			}
			if code != contract.ExitOk || admitted["state"] != "admitted" || admitted["childTaskId"] != "managed-child" || stderr != "" {
				t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
			}
			var started map[string]any
			for _, request := range c.host.Requests() {
				if request.Method == "thread/start" {
					if err := json.Unmarshal(request.Params, &started); err != nil {
						t.Fatal(err)
					}
				}
			}
			if n := c.host.Count("thread/start"); n != 1 || started["model"] != pair.model {
				t.Fatalf("thread/start x%d with %v, want one start on %s", n, started, pair.model)
			}
		})
	}
}

// A pair the role does not declare is refused at the worker-policy preflight, before the app-server
// is asked for a thread, even though the allowlist approves the model on its own effort.
func TestManagedStartRefusesAPairTheChildRoleDoesNotDeclare(t *testing.T) {
	for _, pair := range []struct{ name, model, effort string }{
		{"sonnet at another effort", "anthropic/claude-sonnet-5-5", "high"},
		{"the parent's pair", "anthropic/claude-opus-5-5", "xhigh"},
	} {
		t.Run(pair.name, func(t *testing.T) {
			c := newManagedCLIOn(t, twoPairManagedPolicy, pair.model, pair.effort)
			stdout, stderr, code := c.start(c.request(c.parentOn("anthropic/claude-opus-5-5", "xhigh")))
			var refused map[string]any
			if err := json.Unmarshal([]byte(stdout), &refused); err != nil {
				t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
			}
			if code != contract.ExitRefused || refused["state"] != "refused" || refused["stage"] != "preflight" || refused["reason"] != "worker_policy_pair_mismatch" {
				t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
			}
			if n := c.host.Count("thread/start"); n != 0 {
				t.Fatalf("the refused request asked the app-server to start %d threads", n)
			}
		})
	}
}
