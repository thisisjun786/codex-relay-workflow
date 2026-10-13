package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

// childSandboxSettings is a complete settings record whose sandbox is the policy object given.
func childSandboxSettings(cwd, sandbox string) string {
	return fmt.Sprintf(`{"sandbox": %s, "approvalPolicy": "never", "cwd": %q, "runtimeWorkspaceRoots": [%q], "model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh", "environments": [{"environmentId": "local", "cwd": %q, "runtimeWorkspaceRoots": [%q]}]}`, sandbox, cwd, cwd, cwd, cwd)
}

func registerWithSandboxes(t *testing.T, parentSandbox, childSandbox string) (code int, out map[string]any, state string) {
	t.Helper()
	home := t.TempDir()
	state = filepath.Join(home, "state")
	var stdout, stderr bytes.Buffer
	code = dispatch.Execute(ctx(), "codex-session-relay", []string{"--state", state, "register",
		"--parent-task", "01parent-task", "--parent-host", "host-a", "--child-task", "01child-task", "--child-host", "host-a",
		"--issue", "REL-1", "--artifact-root", filepath.Join(home, "work"), "--allowed-recipient", "01parent-task",
		"--dispatch-request-id", "dispatch-1", "--dispatch-turn-id", "turn-dispatch-1",
		"--parent-settings", childSandboxSettings("/parent", parentSandbox), "--child-settings", childSandboxSettings(filepath.Join(home, "work"), childSandbox)}, &stdout, &stderr)
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("register exit %d: stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	return code, out, state
}

// CRW-1298: a child whose sandbox cannot reach the relay daemon's control.sock (a readOnly one, or a workspaceWrite one
// without network access) is refused at register --child-settings as an input error, before anything is written, and
// the refusal names the two sandboxes that reach it.
func TestRegisterRefusesAChildSandboxThatCannotReachTheDaemon(t *testing.T) {
	for name, sandbox := range map[string]string{
		"read-only":                         `{"type": "readOnly"}`,
		"read-only-with-network":            `{"type": "readOnly", "networkAccess": true}`,
		"workspace-write-no-network":        `{"type": "workspaceWrite", "networkAccess": false}`,
		"workspace-write-network-defaulted": `{"type": "workspaceWrite"}`,
	} {
		t.Run(name, func(t *testing.T) {
			code, out, state := registerWithSandboxes(t, `{"type": "dangerFullAccess"}`, sandbox)
			detail, _ := out["detail"].(string)
			if code != 2 || out["error"] != "refused" || out["reason"] != UnsupportedSandboxType {
				t.Fatalf("expected the register input refusal (exit 2, %s), got %d %v", UnsupportedSandboxType, code, out)
			}
			for _, want := range []string{"danger-full-access", "workspace-write with network access", "control.sock"} {
				if !strings.Contains(detail, want) {
					t.Errorf("the refusal does not name %q: %s", want, detail)
				}
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Errorf("a refused registration created state: %v", err)
			}
		})
	}
}

// CRW-1298: the two sandboxes that reach the daemon still register, and the parent's settings are not checked.
func TestRegisterAcceptsAChildSandboxThatReachesTheDaemon(t *testing.T) {
	for name, sandbox := range map[string]string{
		"danger-full-access":           `{"type": "dangerFullAccess"}`,
		"workspace-write-with-network": `{"type": "workspaceWrite", "networkAccess": true}`,
	} {
		t.Run(name, func(t *testing.T) {
			// the parent is readOnly: only the child's sandbox is judged
			code, out, state := registerWithSandboxes(t, `{"type": "readOnly"}`, sandbox)
			if code != 0 || out["authorizedSettings"].(map[string]any)["01child-task"] != "recorded" {
				t.Fatalf("expected the registration to be recorded, got %d %v", code, out)
			}
			if n := count(t, state, "SELECT COUNT(*) FROM relationships"); n != 1 {
				t.Fatalf("relationships = %d", n)
			}
		})
	}
}
