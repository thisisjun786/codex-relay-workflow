//go:build dev

package ci

import (
	"path/filepath"
	"strings"
	"testing"
)

// The tracked Python is skill assets only: no tracked .py file or script a python shebang runs
// sits outside <skill>/scripts and <skill>/examples of the skills directories. The check is the one
// `crw-dev ci validate` applies, over the tracked files only, so a scratch file breaks
// `crw-dev ci validate` but not `make test`.
func TestTrackedPythonStaysInSkillAssets(t *testing.T) {
	root := repoRoot()
	out, err := runGit(root, "ls-files", "-z")
	if err != nil {
		t.Fatal(err)
	}
	names := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	for _, e := range pythonFileErrors(root, names) {
		t.Error(e)
	}
}

// The allow-list names the skills directory the plugin manifest declares, which the metadata check
// reads, so a manifest that moves its skills cannot leave the allowance behind.
func TestSkillAssetRootIsTheDeclaredSkillsDirectory(t *testing.T) {
	root := repoRoot()
	declared, err := skillsRoot(filepath.Join(root, "plugins/crw/.codex-plugin/plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := resolve(filepath.Join(root, skillAssetRoots[0])); declared != want {
		t.Errorf("the manifest declares skills at %s, the allow-list names %s", declared, want)
	}
}
