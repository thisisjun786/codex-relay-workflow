//go:build dev

package cxccorpus

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// makeTree writes the files, with their directories, under root.
func makeTree(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, rel := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// vanishing is a freezeTimes set that removes the entry at gone just before stamping it, so the
// stamp fails with ENOENT where a file listed by the walk has disappeared since, as git's
// maintenance.lock does. It counts the removals it made.
func vanishing(gone string, removed *int) func(string, time.Time, time.Time) error {
	return func(path string, atime, mtime time.Time) error {
		if path == gone {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
			*removed++
		}
		return os.Chtimes(path, atime, mtime)
	}
}

// A file or directory git's background maintenance creates and removes under .git is gone when the
// walk reaches it: the walk goes on and still stamps what is left, in .git and outside it.
func TestFreezeTimes_skips_an_entry_that_vanished_inside_a_git_directory(t *testing.T) {
	for _, gone := range []string{"ws/.git/objects/maintenance.lock", "ws/.git/objects/tmp_x"} {
		t.Run(gone, func(t *testing.T) {
			root := t.TempDir()
			makeTree(t, root, "ws/.git/HEAD", "ws/.git/objects/maintenance.lock", "ws/.git/objects/pack/p", "ws/.git/objects/tmp_x/f", "ws/.git/objects/zz", "ws/a.txt")
			removed := 0
			if err := freezeTimes(root, Epoch(), vanishing(filepath.Join(root, gone), &removed)); err != nil {
				t.Errorf("a vanished entry inside .git failed the walk: %v", err)
			}
			if removed != 1 {
				t.Fatalf("the entry was removed %d times, want once", removed)
			}
			for _, kept := range []string{"ws/.git/HEAD", "ws/.git/objects/pack/p", "ws/.git/objects/zz", "ws/a.txt"} {
				info, err := os.Stat(filepath.Join(root, kept))
				if err != nil {
					t.Fatal(err)
				}
				if !info.ModTime().Equal(Epoch()) {
					t.Errorf("%s has time %v after the walk, want %v", kept, info.ModTime(), Epoch())
				}
			}
		})
	}
}

// The exception is only for what lies inside a .git directory: the same disappearance anywhere else,
// the .git entry itself and a directory merely named like it fail the walk, and so does any other
// error inside .git.
func TestFreezeTimes_still_fails_for_a_vanished_entry_outside_a_git_directory(t *testing.T) {
	for _, gone := range []string{"ws/a.txt", "ws/.git", "ws/dir.git/f"} {
		t.Run(gone, func(t *testing.T) {
			root := t.TempDir()
			makeTree(t, root, "ws/.git/objects/x", "ws/dir.git/f", "ws/a.txt")
			removed := 0
			err := freezeTimes(root, Epoch(), vanishing(filepath.Join(root, gone), &removed))
			if removed != 1 || !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("removed %d times, error %v; want one removal and an ENOENT error", removed, err)
			}
		})
	}
}

func TestFreezeTimes_still_fails_for_another_error_inside_a_git_directory(t *testing.T) {
	root := t.TempDir()
	makeTree(t, root, "ws/.git/HEAD")
	denied := errors.New("denied")
	err := freezeTimes(root, Epoch(), func(path string, _, _ time.Time) error {
		if filepath.Base(path) == "HEAD" {
			return denied
		}
		return nil
	})
	if !errors.Is(err, denied) {
		t.Errorf("freezeTimes = %v, want the stamp error", err)
	}
}

func prepareCase(t *testing.T, rt Runtime, s Scenario) (*Case, error) {
	t.Helper()
	c, err := NewCase(t.TempDir(), "CRW_HOME", s.Given)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Prepare(rt, s); err != nil {
		t.Fatal(err)
	}
	return c, setUp(c, s.Given, rt)
}

// A file the given lists that is missing still fails the preparation, whether the case names its
// time or its mode, and also inside .git: only the walk tolerates a vanished entry.
func TestSetUp_a_missing_given_still_fails_the_preparation(t *testing.T) {
	for name, given := range map[string]Given{
		"mtime":            {Mtimes: map[string]string{"ws/missing.txt": "2026-01-02T00:00:00Z"}},
		"mode":             {Modes: map[string]int{"ws/missing.txt": 0o600}},
		"mode inside .git": {Modes: map[string]int{"ws/.git/hooks/missing": 0o755}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := prepareCase(t, shRuntime{}, Scenario{ID: "cli__sh__missing_given", Given: given})
			if !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("setUp = %v, want an ENOENT error", err)
			}
		})
	}
}

// traceRuntime has every git of the case write its trace2 events to trace.
type traceRuntime struct {
	shRuntime
	trace string
}

func (rt traceRuntime) Setup(c *Case, s Scenario) error {
	c.Env = append(c.Env, "GIT_TRACE2_EVENT="+rt.trace)
	return rt.shRuntime.Setup(c, s)
}

// The commit of the given must not start git's automatic maintenance: git detaches it, and it
// would keep creating and removing files under .git (objects/maintenance.lock) while the case is
// stamped, observed and removed.
func TestSetUp_git_starts_no_automatic_maintenance(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	trace := filepath.Join(t.TempDir(), "trace2.jsonl")
	s := Scenario{ID: "cli__sh__no_maintenance", Given: Given{Git: &Git{Commit: "c"}}}
	if _, err := prepareCase(t, traceRuntime{shRuntime{git: git}, trace}, s); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var event struct {
			Event, Name string
			Argv        []string
		}
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		committed = committed || event.Event == "cmd_name" && event.Name == "commit"
		if event.Event == "child_start" && (slices.Contains(event.Argv, "maintenance") || slices.Contains(event.Argv, "gc")) {
			t.Errorf("git started %v", event.Argv)
		}
	}
	if !committed {
		t.Errorf("the trace has no commit event, so it proves nothing:\n%s", raw)
	}
}
