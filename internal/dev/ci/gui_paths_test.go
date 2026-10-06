//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// scripts/ci/gui_paths.sh decides whether the `gui` job runs the full screen verification or ends
// without installing Node (CRW-831). It is run as CI runs it, in a temporary repository with the
// base and head the workflow passes, and its answer is read back from $GITHUB_OUTPUT. The safe
// direction is the full run: anything it cannot read selects it.

// guiPathsRepo is a fixture with one commit and a second commit that changes only the given paths.
func guiPathsRepo(t *testing.T, changed ...string) (*fixtureRepo, string, string) {
	t.Helper()
	r := newRepo(t)
	r.write("README.md", "base\n")
	r.write("internal/gui/assets/index.html", "base\n")
	r.commit()
	base := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	for _, name := range changed {
		r.write(name, "changed\n")
	}
	r.commit()
	head := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	return r, base, head
}

// guiPathsRun runs the decision script with the workflow's variables and returns its `changed`
// output line, or "" when it wrote none.
func guiPathsRun(t *testing.T, r *fixtureRepo, env []string) string {
	t.Helper()
	output := filepath.Join(t.TempDir(), "github-output")
	if err := os.WriteFile(output, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repoRoot(), "scripts", "ci", "gui_paths.sh")
	got := runEnv(t, r.root, append(os.Environ(), append(env, "GITHUB_OUTPUT="+output)...), "bash", script)
	if got.code != 0 {
		t.Fatalf("gui_paths.sh exited %d: %s%s", got.code, got.stdout, got.stderr)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "changed=") {
			return line
		}
	}
	return ""
}

// Every path the issue names selects the full run, and an unrelated change does not.
func TestGUIPathsSelectsTheFullVerification(t *testing.T) {
	for _, row := range []struct {
		name     string
		changed  []string
		wantFull bool
	}{
		{"a screen source file under web/", []string{"web/src/App.tsx"}, true},
		{"a committed asset", []string{"internal/gui/assets/assets/index-A.js"}, true},
		{"the workflow itself", []string{".github/workflows/ci.yml"}, true},
		{"the Makefile gui target", []string{"Makefile"}, true},
		{"the decision script", []string{"scripts/ci/gui_paths.sh"}, true},
		{"the drift tool", []string{"internal/dev/ci/gui_drift.go"}, true},
		{"an unrelated Go file", []string{"internal/relay/store/store.go"}, false},
		{"an unrelated doc", []string{"docs/relay/dag-plans.md"}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			r, base, head := guiPathsRepo(t, row.changed...)
			got := guiPathsRun(t, r, []string{
				"GITHUB_EVENT_NAME=pull_request",
				"PR_BASE_SHA=" + base,
				"PR_HEAD_SHA=" + head,
			})
			want := "changed=false"
			if row.wantFull {
				want = "changed=true"
			}
			if got != want {
				t.Errorf("changed list %v: got %q, want %q", row.changed, got, want)
			}
		})
	}
}

// A pull request that changes both a watched and an unwatched path still runs in full.
func TestGUIPathsSelectsTheFullVerificationOnAMixedChange(t *testing.T) {
	r, base, head := guiPathsRepo(t, "internal/relay/store/store.go", "web/package.json")
	got := guiPathsRun(t, r, []string{
		"GITHUB_EVENT_NAME=pull_request",
		"PR_BASE_SHA=" + base,
		"PR_HEAD_SHA=" + head,
	})
	if got != "changed=true" {
		t.Errorf("a mixed change: got %q, want changed=true", got)
	}
}

// A push to dev compares the commit it replaced with the one it added.
func TestGUIPathsComparesThePushRange(t *testing.T) {
	r, before, head := guiPathsRepo(t, "web/index.html")
	got := guiPathsRun(t, r, []string{
		"GITHUB_EVENT_NAME=push",
		"PUSH_BEFORE_SHA=" + before,
		"GITHUB_SHA=" + head,
	})
	if got != "changed=true" {
		t.Errorf("a dev push touching web/: got %q, want changed=true", got)
	}
	r2, before2, head2 := guiPathsRepo(t, "internal/relay/store/store.go")
	got = guiPathsRun(t, r2, []string{
		"GITHUB_EVENT_NAME=push",
		"PUSH_BEFORE_SHA=" + before2,
		"GITHUB_SHA=" + head2,
	})
	if got != "changed=false" {
		t.Errorf("a dev push touching nothing watched: got %q, want changed=false", got)
	}
}

// A manual dispatch has no base to compare with, so it runs the full verification.
func TestGUIPathsRunsInFullOnADispatch(t *testing.T) {
	r, _, _ := guiPathsRepo(t, "README.md")
	for _, env := range [][]string{
		{"GITHUB_EVENT_NAME=workflow_dispatch"},
		{"GITHUB_EVENT_NAME=push", "PUSH_BEFORE_SHA=0000000000000000000000000000000000000000"},
	} {
		if got := guiPathsRun(t, r, env); got != "changed=true" {
			t.Errorf("%v: got %q, want changed=true", env, got)
		}
	}
}

// A changed list that cannot be read selects the full run rather than ending without Node.
func TestGUIPathsFailsClosedOnAnUnreadableList(t *testing.T) {
	r, _, head := guiPathsRepo(t, "README.md")
	for _, env := range [][]string{
		{"GITHUB_EVENT_NAME=pull_request", "PR_BASE_SHA=not-a-sha", "PR_HEAD_SHA=" + head},
		{"GITHUB_EVENT_NAME=pull_request", "PR_BASE_SHA=" + strings.Repeat("0", 40), "PR_HEAD_SHA=" + head},
		{"GITHUB_EVENT_NAME=push", "PUSH_BEFORE_SHA=not-a-sha", "GITHUB_SHA=" + head},
	} {
		if got := guiPathsRun(t, r, env); got != "changed=true" {
			t.Errorf("%v: got %q, want changed=true", env, got)
		}
	}
}

// The watched paths are real: every one that is a file exists at the repository root, so a rename
// of the drift tool or the decision script cannot leave the watch list pointing at nothing.
func TestGUIPathsWatchesExistingFiles(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(), "scripts", "ci", "gui_paths.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^watched=\(([^)]*)\)$`).FindStringSubmatch(string(data))
	if m == nil {
		t.Fatal("gui_paths.sh does not define a watched array")
	}
	names := strings.Fields(m[1])
	for _, want := range []string{"web", "internal/gui/assets", ".github/workflows/ci.yml", "Makefile"} {
		if !slices.Contains(names, want) {
			t.Errorf("the watched list lacks %q: %v", want, names)
		}
	}
	for _, name := range names {
		path := filepath.Join(repoRoot(), filepath.FromSlash(name))
		if _, err := os.Stat(path); err != nil {
			t.Errorf("a watched path does not exist: %s: %v", name, err)
		}
	}
}
