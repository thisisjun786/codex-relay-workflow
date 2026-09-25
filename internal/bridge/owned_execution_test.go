package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
)

// The Python test_execution.py pairs: MODEL/EFFORT is the approved pair, UNAPPROVED is only
// reachable through the "one-task" exception, and the parent role runs PARENT_MODEL/EFFORT.
const (
	pyModel, pyEffort          = "anthropic/claude-opus-5-5", "xhigh"
	pyUnapproved               = "openai/gpt-6-astra"
	parentModel, parentEffort  = "anthropic/claude-opus-5-5", "xhigh"
	supersededM, supersededEff = "devin/swe-2", "max"
)

// guardedBridge is policy_for(cwd) from test_execution.py: efforts scoped per model plus one
// directory-bound exception.
func guardedBridge(t *testing.T, cwd string) (*Bridge, *fakehost.Server) {
	t.Helper()
	return policyBridge(t, `{"allowed":[{"model":"`+pyModel+`","efforts":["`+pyEffort+`"]},{"model":"openai/gpt-5.6-sol","efforts":["high"]}],"exceptions":{"one-task":{"model":"`+pyUnapproved+`","reasoningEffort":"high","cwd":[`+jsonQuote(cwd)+`],"reason":"approved"}}}`)
}

// rolesBridge is roles_policy() from test_execution.py.
func rolesBridge(t *testing.T) (*Bridge, *fakehost.Server) {
	t.Helper()
	return policyBridge(t, `{"allowed":[{"model":"`+pyModel+`","efforts":["`+pyEffort+`"]},{"model":"`+supersededM+`","efforts":["`+supersededEff+`"]}],"roles":{"supervisor":{"expectation":"record"},"parent":{"model":"`+parentModel+`","reasoningEffort":"`+parentEffort+`"},"child":{"model":"`+pyModel+`","reasoningEffort":"`+pyEffort+`"}}}`)
}

func policyBridge(t *testing.T, policy string) (*Bridge, *fakehost.Server) {
	t.Helper()
	b, host := testBridge(t)
	parsed, err := execution.FromBytes([]byte(policy), "test")
	if err != nil {
		t.Fatal(err)
	}
	b.Policy = parsed
	return b, host
}

func jsonQuote(value string) string { encoded, _ := json.Marshal(value); return string(encoded) }

// pairStart answers thread/start and turn/start reporting the given pair.
func pairStart(host *fakehost.Server, cwd, model, effort string) {
	start := startReply(cwd)
	start.Result["model"], start.Result["reasoningEffort"] = model, effort
	host.Respond("thread/start", start)
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
}

func Test_test_a_creation_for_the_wrong_role_never_reaches_the_host(t *testing.T) {
	for _, scenario := range []struct{ role, model, effort, code string }{
		{"parent", supersededM, supersededEff, execution.RoleMismatch},
		{"parent", parentModel, supersededEff, execution.RoleMismatch},
		{"reviewer", pyModel, pyEffort, execution.RoleUnknown},
	} {
		t.Run(scenario.role+scenario.effort, func(t *testing.T) {
			b, host := rolesBridge(t)
			input := createInput(t.TempDir(), "wrong-role")
			input.Role, input.Model, input.Effort, input.Prompt = scenario.role, scenario.model, scenario.effort, "work"
			_, err := b.CreateThread(context.Background(), input)
			var refusal *execution.Refusal
			if !errors.As(err, &refusal) || refusal.Code != scenario.code || len(hostMethods(host)) != 0 {
				t.Fatalf("err=%v calls=%v", err, host.Requests())
			}
			if _, err := b.GetOperation(context.Background(), input.RequestID); err == nil {
				t.Fatal("refused request acquired ledger id")
			}
		})
	}
}

func Test_test_a_corrected_role_request_succeeds_under_the_same_id(t *testing.T) {
	b, host := rolesBridge(t)
	cwd := t.TempDir()
	input := createInput(cwd, "retried")
	input.Role, input.Model, input.Effort, input.Prompt = "parent", supersededM, supersededEff, "work"
	if _, err := b.CreateThread(context.Background(), input); err == nil {
		t.Fatal("wrong role accepted")
	}
	input.Model, input.Effort = parentModel, parentEffort
	pairStart(host, cwd, parentModel, parentEffort)
	receipt, err := b.CreateThread(context.Background(), input)
	start := hostParams(t, host, "thread/start")
	expectation := object(object(receipt["executionPolicy"])["roleExpectation"])
	if err != nil || receipt["status"] != "accepted" || start["model"] != parentModel || object(start["config"])["model_reasoning_effort"] != parentEffort || object(receipt["executionPolicy"])["role"] != "parent" || expectation["model"] != parentModel {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

func Test_test_an_unapproved_pair_is_refused_before_any_call(t *testing.T) {
	cwd := t.TempDir()
	for _, pair := range []struct{ model, effort, field string }{
		{pyUnapproved, "high", "model"},
		{pyModel, "low", "reasoning_effort"},
		{pyModel, "high", "reasoning_effort"}, // crossed: two approved values, one unapproved pair
	} {
		t.Run(pair.model+"/"+pair.effort, func(t *testing.T) {
			b, host := guardedBridge(t, cwd)
			input := createInput(cwd, "blocked")
			input.Model, input.Effort, input.Prompt = pair.model, pair.effort, "work"
			_, err := b.CreateThread(context.Background(), input)
			var refusal *execution.Refusal
			if !errors.As(err, &refusal) || refusal.Code != execution.NotAllowed || refusal.Field != pair.field || len(hostMethods(host)) != 0 {
				t.Fatalf("err=%v calls=%v", err, host.Requests())
			}
		})
	}
}

func Test_test_a_cited_exception_that_does_not_exist_or_does_not_match_sends_nothing(t *testing.T) {
	cwd := t.TempDir()
	other := filepath.Join(cwd, "elsewhere")
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	b, host := guardedBridge(t, cwd)
	for _, scenario := range []struct{ exception, model, effort, directory, code string }{
		{"invented", pyUnapproved, "high", cwd, execution.ExceptionUnknown},
		{"one-task", pyModel, "high", cwd, execution.NotAllowed},
		{"one-task", pyUnapproved, "max", cwd, execution.NotAllowed},
		{"one-task", pyUnapproved, "high", other, execution.ExceptionOutOfScope},
	} {
		input := createInput(scenario.directory, "blocked-"+scenario.exception+scenario.effort)
		input.Exception, input.Model, input.Effort = scenario.exception, scenario.model, scenario.effort
		_, err := b.CreateThread(context.Background(), input)
		var refusal *execution.Refusal
		if !errors.As(err, &refusal) || refusal.Code != scenario.code {
			t.Fatalf("scenario=%v err=%v", scenario, err)
		}
	}
	if len(hostMethods(host)) != 0 {
		t.Fatalf("calls=%v", host.Requests())
	}
}

func Test_test_an_unavailable_model_is_an_error_and_not_a_substitution(t *testing.T) {
	b, host := testBridge(t)
	input := createInput(t.TempDir(), "gone")
	input.Prompt = "work"
	host.Respond("thread/start", fakehost.Reply{Error: &fakehost.RPCError{Code: -32602, Message: "model is not available"}})
	receipt, err := b.CreateThread(context.Background(), input)
	if err != nil || receipt["status"] != "failed" || !strings.Contains(text(receipt["error"]), "model is not available") || host.Count("thread/start") != 1 || host.Count("turn/start") != 0 {
		t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, host.Requests())
	}
}

func Test_test_a_worktree_launch_for_the_wrong_role_creates_nothing(t *testing.T) {
	b, host := rolesBridge(t)
	root := t.TempDir()
	destination := filepath.Join(root, "never-created")
	input := CreateWorktree{RequestID: "worktree-wrong-role", Source: root, Revision: strings.Repeat("0", 40), Destination: destination, Mode: "bridge-managed-retained", Sandbox: "workspace-write", Policy: map[string]any{"type": "workspaceWrite", "networkAccess": false, "writableRoots": []any{}, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}, Model: supersededM, Effort: supersededEff, Role: "parent"}
	_, err := b.CreateWorktreeThread(context.Background(), input)
	var refusal *execution.Refusal
	if !errors.As(err, &refusal) || refusal.Code != execution.RoleMismatch || len(hostMethods(host)) != 0 {
		t.Fatalf("err=%v calls=%v", err, host.Requests())
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("checkout created: %v", err)
	}
	if _, err := b.GetOperation(context.Background(), input.RequestID); err == nil {
		t.Fatal("refused worktree acquired ledger id")
	}
}
