package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// CRW-1488 (1209 trial finding 12): the run's repository tools come from the verifier's setting (verifier.go_root) and nothing
// else: the entries the verifier's own command gets (its bin first on PATH, GOROOT, GOTOOLCHAIN=local), the prefix a session
// subagent puts in front of a go command, and the fact its packet names. No go_root, or a section the verifier ignores, is no tool.
func TestRepositoryToolsFromVerifierGoRoot(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, "config.json")
	root := filepath.Join(home, "go root")
	getenv := func(k string) string {
		return map[string]string{"HOME": home, "CRW_CONFIG": config, "PATH": "/usr/local/bin:/usr/bin"}[k]
	}
	write := func(doc string) {
		t.Helper()
		if err := os.WriteFile(config, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(`{"verifier":{"go_root":"` + root + `"}}`)
	tools := repositoryTools(getenv)
	if tools == nil {
		t.Fatal("verifier.go_root gave no tools")
	}
	bin := filepath.Join(root, "bin")
	if want := []string{"PATH=" + bin + ":/usr/local/bin:/usr/bin", "GOROOT=" + root, "GOTOOLCHAIN=local"}; !slices.Equal(tools.Env, want) {
		t.Errorf("env %q, want %q", tools.Env, want)
	}
	if want := `PATH='` + bin + `':"$PATH" GOROOT='` + root + `' GOTOOLCHAIN=local`; tools.Prefix != want {
		t.Errorf("prefix %s, want %s", tools.Prefix, want)
	}
	if len(tools.Facts) != 1 || !strings.Contains(tools.Facts[0], filepath.Join(bin, "go")) || !strings.Contains(tools.Facts[0], "verifier.go_root") ||
		!strings.Contains(tools.Facts[0], config) {
		t.Errorf("facts %q do not name the go, the setting and the file", tools.Facts)
	}

	for _, doc := range []string{`{}`, `{"verifier":{"crw_dev":"tree"}}`, `{"verifier":{"go_root":"relative"}}`} {
		write(doc)
		if tools := repositoryTools(getenv); tools != nil {
			t.Errorf("%s gave tools %+v", doc, tools)
		}
	}
}
