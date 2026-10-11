package testsupport

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// CRW-1265: the sandbox-root rule (internal/sandboxroot) keeps a sandboxed Codex in the crw configuration's
// paths.sandbox_root, a known project root or a linked worktree. A test makes its workspaces, fake homes and
// worktrees under the temporary directory (t.TempDir, os.MkdirTemp), so the isolation names that directory as
// the sandbox root in the isolated crw configuration, the file at the configuration home's default location
// (XDG_CONFIG_HOME/crw/config.json) that every process the test starts with its environment reads. Without it the
// rule's default (crwconfig.DefaultSandboxRoot) decided, and a test passed or failed by where the host's TMPDIR lay. The rule
// itself is unchanged: the configuration is the product's own, and a test judges its paths exactly as a host
// whose sandbox_root is the temporary directory would.

// sandboxConfigFile is the isolated crw configuration file of this process, "" before isolate.
var sandboxConfigFile string

// SandboxConfig is the crw configuration file the isolation wrote (XDG_CONFIG_HOME/crw/config.json of the
// isolation root), whose paths.sandbox_root is the temporary directory the tests make their directories in. A
// harness that starts a process with an environment of its own (a configuration home it chose, or no
// configuration home at all) names this file as CRW_CONFIG so that the process reads the same rule. It is ""
// in a process that did not isolate.
func SandboxConfig() string { return sandboxConfigFile }

// SandboxConfigDocument is the crw configuration the isolation writes: sandbox_root is the real path of
// tempDir, and the system temporary locations a test falls back to when tempDir lies in a repository
// (fallbackTempParents) are sandbox.project_roots when they exist.
func SandboxConfigDocument(tempDir string) (map[string]any, error) {
	base, err := filepath.EvalSymlinks(tempDir)
	if err != nil {
		return nil, err
	}
	doc := map[string]any{"paths": map[string]any{"sandbox_root": base}}
	var projects []string
	for _, parent := range fallbackTempParents {
		if real, err := filepath.EvalSymlinks(parent); err == nil && real != base {
			projects = append(projects, real)
		}
	}
	if len(projects) > 0 {
		doc["sandbox"] = map[string]any{"project_roots": projects}
	}
	return doc, nil
}

// WriteSandboxConfig writes SandboxConfigDocument(tempDir) to path, making its directory.
func WriteSandboxConfig(path, tempDir string) error {
	doc, err := SandboxConfigDocument(tempDir)
	if err != nil {
		return err
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
