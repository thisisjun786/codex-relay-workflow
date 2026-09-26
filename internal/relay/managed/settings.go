package managed

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

func validateSettings(value any, at string) error {
	m, err := object(value, registry.Required, []string{"expectedPermissionProfile"}, at)
	if err != nil {
		return err
	}
	for _, name := range []string{"model", "reasoningEffort"} {
		if err := text(m[name], at+"."+name, 500); err != nil {
			return err
		}
	}
	if m["approvalPolicy"] != "never" {
		return fmt.Errorf("%s.approvalPolicy must explicitly be never for this transport", at)
	}
	if err := existingDirectory(m["cwd"], at+".cwd"); err != nil {
		return err
	}
	roots, ok := m["runtimeWorkspaceRoots"].([]any)
	if !ok || len(roots) == 0 || len(roots) > 256 {
		return fmt.Errorf("%s.runtimeWorkspaceRoots must be a nonempty list of at most 256 entries", at)
	}
	for _, root := range roots {
		if err := existingDirectory(root, at+".runtimeWorkspaceRoots"); err != nil {
			return err
		}
	}
	policy, ok := m["sandbox"].(map[string]any)
	if !ok {
		return fmt.Errorf("%s.sandbox must carry a supported type", at)
	}
	kind, _ := policy["type"].(string)
	if kind != "readOnly" && kind != "workspaceWrite" && kind != "dangerFullAccess" {
		return fmt.Errorf("%s.sandbox must carry a supported type", at)
	}
	allowed := map[string][]string{"readOnly": {"networkAccess"}, "workspaceWrite": {"writableRoots", "networkAccess", "excludeTmpdirEnvVar", "excludeSlashTmp"}, "dangerFullAccess": {}}
	if _, err = object(policy, []string{"type"}, allowed[kind], at+".sandbox"); err != nil {
		return err
	}
	for key, value := range policy {
		if key == "type" {
			continue
		}
		if key == "writableRoots" {
			list, ok := value.([]any)
			if !ok || len(list) > 256 {
				return fmt.Errorf("%s.sandbox.writableRoots must be a list", at)
			}
			for _, root := range list {
				if err := existingDirectory(root, at+".sandbox.writableRoots"); err != nil {
					return err
				}
			}
		} else if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s.sandbox.%s must be a boolean", at, key)
		}
	}
	environments, ok := m["environments"].([]any)
	if !ok || len(environments) > 1 {
		return fmt.Errorf("%s.environments must declare empty or one local environment", at)
	}
	for _, item := range environments {
		env, err := object(item, []string{"environmentId", "cwd", "runtimeWorkspaceRoots"}, nil, at)
		if err != nil {
			return err
		}
		if env["environmentId"] != "local" {
			return fmt.Errorf("%s: selecting remote environments is unsupported", at)
		}
		if env["cwd"] != m["cwd"] || !jsonSame(env["runtimeWorkspaceRoots"], roots) {
			return fmt.Errorf("%s: local environment must match the declared cwd and roots", at)
		}
	}
	if profile, ok := m["expectedPermissionProfile"]; ok && profile != nil {
		if err := text(profile, at+".expectedPermissionProfile", 500); err != nil {
			return err
		}
	}
	contract := settings.Contract{CWD: m["cwd"].(string), Sandbox: map[string]string{"readOnly": "read-only", "workspaceWrite": "workspace-write", "dangerFullAccess": "danger-full-access"}[kind], ExpectedPolicy: policy, Model: m["model"].(string), ReasoningEffort: m["reasoningEffort"].(string)}
	for _, root := range roots {
		contract.Roots = append(contract.Roots, root.(string))
	}
	if err := contract.Validate(); err != nil {
		return err
	}
	return nil
}
func jsonSame(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func existingDirectory(v any, at string) error {
	if err := text(v, at, 4096); err != nil {
		return err
	}
	path := v.(string)
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s must be absolute", at)
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%s must name an existing directory", at)
	}
	return nil
}
