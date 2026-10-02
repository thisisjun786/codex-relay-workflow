package source

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) { testsupport.Main(m) }

// hermetic isolates git from the machine: no inherited repository routing or injected configuration, no user or
// system configuration, a fixed identity and dates (so commit ids are reproducible), and repository discovery
// that stops at the returned directory, which holds one test's repositories. Setenv forbids t.Parallel.
func hermetic(t *testing.T) string {
	t.Helper()
	for _, entry := range os.Environ() {
		if name, _, _ := strings.Cut(entry, "="); isRoutingVar(name) || name == "GIT_CONFIG_COUNT" || name == "GIT_CONFIG_PARAMETERS" {
			t.Setenv(name, "")
			_ = os.Unsetenv(name)
		}
	}
	base := t.TempDir()
	for name, value := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CEILING_DIRECTORIES": base,
		"GIT_AUTHOR_NAME": "fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid", "GIT_AUTHOR_DATE": "2026-01-01T00:00:00Z",
		"GIT_COMMITTER_NAME": "fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid", "GIT_COMMITTER_DATE": "2026-01-01T00:00:00Z",
	} {
		t.Setenv(name, value)
	}
	return base
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755))
	must(t, os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644))
}

// tempIn is a fresh directory below base, which repository discovery does not leave.
func tempIn(t *testing.T, base string) string {
	t.Helper()
	dir, err := os.MkdirTemp(base, "r-")
	must(t, err)
	return dir
}

// newRepo builds the oracle tests' repository: one commit holding .gitignore (ignored/), tracked.ts and sub/x.ts.
func newRepo(t *testing.T, base string) string {
	t.Helper()
	root := tempIn(t, base)
	gitIn(t, root, "init", "-q", "-b", "main", ".")
	writeFile(t, root, ".gitignore", "ignored/\n")
	writeFile(t, root, "tracked.ts", "a\n")
	writeFile(t, root, "sub/x.ts", "b\n")
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-qm", "init")
	return root
}

// hermetic must leave no inherited repository routing, or building test repositories could write into another one.
func TestHermeticIgnoresInheritedRouting(t *testing.T) {
	t.Setenv("GIT_DIR", t.TempDir())
	hermetic(t)
	if os.Getenv("GIT_DIR") != "" {
		t.Fatal("hermetic kept GIT_DIR")
	}
}
