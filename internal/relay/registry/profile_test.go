package registry

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// Codex 0.154 reports activePermissionProfile on every resume, a thread run from a sandbox mode
// included: {"id": ":danger-full-access", "extends": null}. A record made from a sandbox mode
// names no profile, so until 2026-10-02 every delivery to such a recipient was withheld as
// unverifiable_permission_profile. The built-in profile of the recorded sandbox type, extending
// nothing, is that sandbox and is verified with it; anything else still is not.
func TestTheRecordedSandboxsBuiltinProfileIsVerifiedWithTheSandbox(t *testing.T) {
	const fullAccess = `{"type": "dangerFullAccess"}`
	const workspace = `{"type": "workspaceWrite", "writableRoots": [], "networkAccess": false, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}`
	const readOnly = `{"type": "readOnly", "networkAccess": false}`
	cases := []struct {
		name, sandbox, recorded, reported, want string
	}{
		{"full access reported as its builtin", fullAccess, "", `{"id": ":danger-full-access", "extends": null}`, ""},
		{"full access without extends", fullAccess, "", `{"id": ":danger-full-access"}`, ""},
		{"workspace write reported as its builtin", workspace, "", `{"id": ":workspace", "extends": null}`, ""},
		{"read only reported as its builtin", readOnly, "", `{"id": ":read-only", "extends": null}`, ""},
		{"another sandbox's builtin", workspace, "", `{"id": ":danger-full-access", "extends": null}`, UnverifiablePermissionProfile},
		{"a builtin that extends another", fullAccess, "", `{"id": ":danger-full-access", "extends": ":workspace"}`, UnverifiablePermissionProfile},
		{"a custom profile", fullAccess, "", `{"id": "trusted", "extends": null}`, UnverifiablePermissionProfile},
		{"a builtin with more fields", fullAccess, "", `{"id": ":danger-full-access", "extends": null, "network": true}`, UnverifiablePermissionProfile},
		{"a builtin id alone", fullAccess, "", `":danger-full-access"`, UnverifiablePermissionProfile},
		{"a recorded profile is compared whole", fullAccess, `{"id": "trusted", "extends": null}`, `{"id": ":danger-full-access", "extends": null}`, UnverifiablePermissionProfile},
		{"a recorded profile reported as recorded", fullAccess, `{"id": "trusted", "extends": null}`, `{"id": "trusted", "extends": null}`, ""},
		{"a recorded profile the host does not report", fullAccess, `{"id": "trusted", "extends": null}`, "", SettingUnobservable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := `{"sandbox": ` + c.sandbox + `, "approvalPolicy": "never", "cwd": "/w", "runtimeWorkspaceRoots": ["/w"], "model": "m", "reasoningEffort": "xhigh", "environments": [{"environmentId": "local", "cwd": "/w", "runtimeWorkspaceRoots": ["/w"]}]`
			if c.recorded != "" {
				row += `, "expectedPermissionProfile": ` + c.recorded
			}
			response := `{"approvalPolicy": "never", "sandbox": ` + c.sandbox + `, "cwd": "/w", "runtimeWorkspaceRoots": ["/w"], "model": "m", "reasoningEffort": "xhigh", "thread": {"environments": [{"environmentId": "local", "cwd": "/w", "runtimeWorkspaceRoots": ["/w"]}]}`
			if c.reported != "" {
				response += `, "activePermissionProfile": ` + c.reported
			}
			found := TaskSettings{decoded(t, row+"}").(contract.OrderedObject)}.Mismatches(decoded(t, response+"}"), true, false, false)
			var codes []string
			for _, f := range found {
				code, _ := f.Lookup("code")
				codes = append(codes, code.(string))
			}
			if got := strings.Join(codes, ","); got != c.want {
				t.Fatalf("findings %q, want %q: %v", got, c.want, found)
			}
		})
	}
}

func decoded(t *testing.T, text string) any {
	t.Helper()
	value, err := decodeJSON([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return value
}
