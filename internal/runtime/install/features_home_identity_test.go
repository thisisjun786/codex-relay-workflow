package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1144: the Codex home is resolved once, through the kernel, and the CLI the command runs, the managed-key edits, the
// backup, the manifest and the lock all use that one identity. A CODEX_HOME that holds a symlink followed by ".." names the
// directory the kernel reaches, not the one a lexical join folds to.
func TestFeaturesEnableUsesOneCodexHomeIdentity(t *testing.T) {
	t.Parallel()
	h := newFeatureHome(t, "[memories]\ngenerate_memories = true\n")
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "real", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real", "sub"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	sentinel := "# sentinel\n[memories]\ngenerate_memories = true\n"
	lexical, physical := filepath.Join(base, "config.toml"), filepath.Join(base, "real", "config.toml")
	for _, p := range []string{lexical, physical} {
		if err := os.WriteFile(p, []byte(sentinel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h.env = h.env.With("CODEX_HOME", filepath.Join(base, "link")+"/..").With("CRW499_FAKE_USE_CODEX_HOME", "1")
	code, out, errOut := h.run("enable")
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// The fixture is a supported home: the command must succeed, so a regression that refuses every symlink or ".." home before
	// editing cannot pass (CRW-1144).
	if code != 0 {
		t.Fatalf("enable on a supported symlink/.. home exited %d: %q", code, errOut)
	}
	if read(lexical) != sentinel {
		t.Fatalf("the lexical home was edited while Codex edited the physical one (stdout %q):\n%s", out, read(lexical))
	}
	got := read(physical)
	if !strings.Contains(got, "dedicated_tools = true") || !strings.Contains(got, "multi_agent = true") {
		t.Fatalf("the physical config does not hold both the flags and the managed key:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(base, "real", ".crw-install.json")); err != nil {
		t.Fatalf("the manifest is not beside the physical config: %v", err)
	}
}
