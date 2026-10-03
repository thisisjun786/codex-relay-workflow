package goalplan

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// The slug rules and the error texts are replayed against the oracle in oracle_plan_test.go; these cases build the trees the
// recorded ones cannot: links, a relative cwd, and what a refused slug must not have created.

func TestGoalplanDirPath(t *testing.T) {
	dir, err := GoalplanDir("/work/proj", "plan-1")
	if want := filepath.Join("/work/proj", crwdir.DirName, GoalplansSubdir, "plan-1"); err != nil || dir != want {
		t.Fatalf("GoalplanDir = %q, %v; want %q", dir, err, want)
	}
	// A relative cwd is made absolute against the process's physical working directory, as path.resolve does, and cleaned.
	t.Chdir(t.TempDir())
	wd, err := syscall.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for cwd, root := range map[string]string{"": wd, "dir/": filepath.Join(wd, "dir"), "./a/../b": filepath.Join(wd, "b")} {
		got, err := GoalplanDir(cwd, "x")
		if want := filepath.Join(root, crwdir.DirName, GoalplansSubdir, "x"); err != nil || got != want {
			t.Errorf("GoalplanDir(%q) = %q, %v; want %q", cwd, got, err, want)
		}
	}
}

func TestGoalplanPaths(t *testing.T) {
	file, err := goalplanPath("/work", "p")
	if want := filepath.Join("/work", crwdir.DirName, GoalplansSubdir, "p", GoalplanFile); err != nil || file != want {
		t.Fatalf("goalplanPath = %q, %v; want %q", file, err, want)
	}
	ledger, err := goalplanLedgerPath("/work", "p")
	if want := filepath.Join("/work", crwdir.DirName, GoalplansSubdir, "p", GoalplanLedgerFile); err != nil || ledger != want {
		t.Fatalf("goalplanLedgerPath = %q, %v; want %q", ledger, err, want)
	}
	for _, path := range []func(string, string) (string, error){goalplanPath, goalplanLedgerPath} {
		if got, err := path("/work", "../x"); err == nil || got != "" || !strings.HasPrefix(err.Error(), "invalid goalplan slug") {
			t.Errorf("a slug that is a path gave %q, %v", got, err)
		}
	}
}

// A slug that climbs out is refused before anything is resolved, so nothing is created in the project.
func TestGoalplanDirTraversalCreatesNothing(t *testing.T) {
	cwd := t.TempDir()
	if _, err := GoalplanDir(cwd, "../../escaped"); err == nil || !strings.HasPrefix(err.Error(), "invalid goalplan slug") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(cwd); len(entries) != 0 {
		t.Fatalf("%d entries created in the project", len(entries))
	}
}

// Each of the three state paths is refused when it is a link to a directory that exists, whichever one it is, and the directory
// it points at is left alone.
func TestGoalplanDirRefusesSymlinks(t *testing.T) {
	for name, link := range map[string][]string{"state root": {crwdir.DirName}, "plans root": {crwdir.DirName, GoalplansSubdir}, "slug directory": {crwdir.DirName, GoalplansSubdir, "p"}} {
		t.Run(name, func(t *testing.T) {
			cwd, outside := t.TempDir(), t.TempDir()
			at := filepath.Join(append([]string{cwd}, link...)...)
			if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, at); err != nil {
				t.Fatal(err)
			}
			got, err := GoalplanDir(cwd, "p")
			if want := "goalplan state path must not be a symlink: " + at; err == nil || err.Error() != want || got != "" {
				t.Fatalf("GoalplanDir = %q, %v; want the error %q", got, err, want)
			}
			if entries, _ := os.ReadDir(outside); len(entries) != 0 {
				t.Fatalf("%d entries created behind the link", len(entries))
			}
		})
	}
}

// assertNotSymlink asks whether the path exists before it asks whether it is a link, and exists follows the link, so a link whose
// target is absent is not refused (oracle behaviour, listed in known-defects.md as port: kept).
func TestGoalplanDirDanglingSymlinkIsNotRefused(t *testing.T) {
	cwd, target := t.TempDir(), filepath.Join(t.TempDir(), "absent")
	if err := os.Symlink(target, filepath.Join(cwd, crwdir.DirName)); err != nil {
		t.Fatal(err)
	}
	if got, err := GoalplanDir(cwd, "p"); err != nil || got != filepath.Join(cwd, crwdir.DirName, GoalplansSubdir, "p") {
		t.Fatalf("GoalplanDir = %q, %v", got, err)
	}
	if _, err := os.Lstat(target); err == nil {
		t.Fatal("the target was created")
	}
}
