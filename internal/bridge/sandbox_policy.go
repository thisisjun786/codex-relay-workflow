package bridge

import "path/filepath"

var policyFields = map[string][]string{
	"readOnly":         {"type", "networkAccess"},
	"dangerFullAccess": {"type"},
	"workspaceWrite":   {"type", "networkAccess", "writableRoots", "excludeTmpdirEnvVar", "excludeSlashTmp"},
}

// validateSandboxPolicy is bridge.py validate_sandbox_policy, checks in its order: the field
// set, then the flags, then writableRoots.
func validateSandboxPolicy(policy map[string]any) error {
	kind, _ := policy["type"].(string)
	fields, known := policyFields[kind]
	if !known || len(policy) != len(fields) {
		return &Invalid{"Expected sandbox policy must contain all and only its protocol fields"}
	}
	for _, key := range fields {
		if _, present := policy[key]; !present {
			return &Invalid{"Expected sandbox policy must contain all and only its protocol fields"}
		}
	}
	for _, key := range []string{"networkAccess", "excludeTmpdirEnvVar", "excludeSlashTmp"} {
		if value, present := policy[key]; present {
			if _, ok := value.(bool); !ok {
				return &Invalid{"Expected sandbox policy flags must be booleans"}
			}
		}
	}
	if value, present := policy["writableRoots"]; present {
		roots, ok := value.([]any)
		if !ok {
			return &Invalid{"Expected sandbox policy writableRoots must be absolute paths"}
		}
		for _, root := range roots {
			if path, ok := root.(string); !ok || !filepath.IsAbs(path) {
				return &Invalid{"Expected sandbox policy writableRoots must be absolute paths"}
			}
		}
	}
	return nil
}
