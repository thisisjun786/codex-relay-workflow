package settings

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Fixture values copied from packages/codex-thread-bridge/tests (conftest.py, test_settings.py).
const (
	model  = "anthropic/claude-opus-5-5" // conftest MODEL
	effort = "xhigh"                     // conftest EFFORT
	opus   = "anthropic/claude-opus-5"
	cwd    = "/work/tree"
)

// writePolicy is test_settings.py WRITE_POLICY.
func writePolicy() map[string]any {
	return map[string]any{"type": "workspaceWrite", "writableRoots": []any{}, "networkAccess": false, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}
}

// hostEcho is conftest FakeServer.settings_view: what the measured host reports for a start or
// resume carrying params (sandbox as the full policy, effort echoed from config).
func hostEcho(params map[string]any) map[string]any {
	config, _ := params["config"].(map[string]any)
	workspace, _ := config["sandbox_workspace_write"].(map[string]any)
	mode, _ := params["sandbox"].(string)
	if mode == "" {
		mode = "read-only"
	}
	kind := modes[mode]
	sandbox := map[string]any{"type": kind}
	switch kind {
	case "workspaceWrite":
		pick := func(key string, fallback any) any {
			if v, ok := workspace[key]; ok {
				return v
			}
			return fallback
		}
		sandbox = map[string]any{"type": kind, "writableRoots": pick("writable_roots", []any{}), "networkAccess": pick("network_access", false), "excludeTmpdirEnvVar": pick("exclude_tmpdir_env_var", false), "excludeSlashTmp": pick("exclude_slash_tmp", false)}
	case "readOnly":
		sandbox["networkAccess"] = false
	}
	roots, ok := params["runtimeWorkspaceRoots"]
	if !ok {
		roots = []any{params["cwd"]}
	}
	answer := map[string]any{"cwd": params["cwd"], "runtimeWorkspaceRoots": roots, "approvalPolicy": "never", "sandbox": sandbox, "model": "configured-default", "reasoningEffort": "medium"}
	if v, ok := params["model"]; ok {
		answer["model"] = v
	}
	if v, ok := config["model_reasoning_effort"]; ok {
		answer["reasoningEffort"] = v
	}
	return answer
}

// created is what the bridge's create_thread sends (cwd, read-only default, contract params)
// and the host's answer to it, with overrides applied like FakeServer.override_creation.
func created(c Contract, overrides map[string]any) map[string]any {
	params := map[string]any{"cwd": c.CWD, "sandbox": "read-only"}
	for k, v := range c.StartParams() {
		params[k] = v
	}
	answer := hostEcho(params)
	for k, v := range overrides {
		answer[k] = v
	}
	return answer
}

// createContract is create_thread's contract: cwd, the read-only default and the stated pair.
func createContract(m, e string) Contract {
	return Contract{CWD: cwd, Sandbox: "read-only", Model: m, ReasoningEffort: e}
}

// carried is test_settings.py carried() plus the EXECUTION pair every send adds.
func carried() Contract {
	return Contract{Sandbox: "workspace-write", ExpectedPolicy: writePolicy(), Model: model, ReasoningEffort: effort}
}

type pythonCase struct {
	python string
	run    func(t *testing.T)
}

func runPython(t *testing.T, cases []pythonCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.python, c.run)
	}
}

func receiptOf(c Contract, response map[string]any, at string) map[string]any {
	return c.Receipt(response, at)
}

func findingsOf(receipt map[string]any) []Finding { return receipt["findings"].([]Finding) }

func sameStrings(got any, want ...string) bool {
	list, ok := got.([]string)
	return ok && slices.Equal(list, want)
}

func equal(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: got %#v want %#v", what, got, want)
	}
}

func untransmittable(t *testing.T, err error) *UntransmittableError {
	t.Helper()
	var u *UntransmittableError
	if !errors.As(err, &u) {
		t.Fatalf("want UntransmittableSetting, got %v", err)
	}
	return u
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
