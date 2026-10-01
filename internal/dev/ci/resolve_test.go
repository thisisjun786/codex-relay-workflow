//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolve_follows_a_link_before_a_missing_tail(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	plugin := filepath.Join(root, "plugin")
	for _, dir := range []string{outside, plugin} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../outside", filepath.Join(plugin, "link")); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		filepath.Join(plugin, "link", "missing", "skills"): filepath.Join(outside, "missing", "skills"),
		filepath.Join(plugin, "link"):                      outside,
		filepath.Join(plugin, "absent"):                    filepath.Join(plugin, "absent"),
	}
	for path, want := range cases {
		if got := resolve(path); got != want {
			t.Errorf("resolve(%q) = %q, want %q", path, got, want)
		}
	}
	if isRelativeTo(resolve(filepath.Join(plugin, "link", "missing")), plugin) {
		t.Fatal("a missing path below a link out of the plugin is judged inside it")
	}
}
