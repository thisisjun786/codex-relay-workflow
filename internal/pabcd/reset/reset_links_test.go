package reset

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"testing"
)

// resetLinksWorkspace makes <root>/.crw with an empty sessions directory, a
// ledger and bg/job.json, plus an outside directory holding keep.json.
func resetLinksWorkspace(t *testing.T) (root, outside string) {
	t.Helper()
	root, outside = t.TempDir(), t.TempDir()
	for p, content := range map[string]string{
		filepath.Join(root, ".crw/ledger.jsonl"):      "ledger",
		filepath.Join(root, ".crw/bg/job.json"):       "job",
		filepath.Join(root, ".crw/sessions/keep.txt"): "keep",
		filepath.Join(outside, "keep.json"):           "outside",
		filepath.Join(outside, "dir/keep.json"):       "outside dir",
	} {
		resetLinksWrite(t, p, content)
	}
	return root, outside
}

func resetLinksWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func resetLinksLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func resetLinksRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestResetLinksRemovedItself: a link whose target exists is removed itself,
// whatever the target (inside the workspace, outside it, a directory, a chain
// longer than os.Root follows); a link whose target does not exist is absent and stays.
func TestResetLinksRemovedItself(t *testing.T) {
	for _, tc := range []struct {
		name    string
		link    func(t *testing.T, root, outside string)
		removed []string // below .crw, in reset order
		stays   string   // a link below .crw that must stay and be listed absent
		kept    map[string]string
	}{
		{"absolute_inside", func(t *testing.T, root, _ string) {
			resetLinksLink(t, filepath.Join(root, ".crw/sessions/keep.txt"), filepath.Join(root, ".crw/sessions/a.json"))
		}, []string{"sessions/a.json", "ledger.jsonl"}, "", map[string]string{"sessions/keep.txt": "keep"}},
		{"outside_file", func(t *testing.T, root, outside string) {
			resetLinksLink(t, filepath.Join(outside, "keep.json"), filepath.Join(root, ".crw/sessions/a.json"))
		}, []string{"sessions/a.json", "ledger.jsonl"}, "", nil},
		{"absolute_dangling", func(t *testing.T, root, outside string) {
			resetLinksLink(t, filepath.Join(outside, "gone"), filepath.Join(root, ".crw/sessions/a.json"))
		}, []string{"ledger.jsonl"}, "sessions/a.json", nil},
		{"relative_past_sessions", func(t *testing.T, root, _ string) {
			resetLinksLink(t, "../bg/job.json", filepath.Join(root, ".crw/sessions/a.json"))
		}, []string{"sessions/a.json", "ledger.jsonl"}, "", map[string]string{"bg/job.json": "job"}},
		{"outside_directory", func(t *testing.T, root, outside string) {
			resetLinksLink(t, filepath.Join(outside, "dir"), filepath.Join(root, ".crw/interviews"))
		}, []string{"ledger.jsonl", "interviews"}, "", nil},
		{"self_loop", func(t *testing.T, root, _ string) {
			resetLinksLink(t, "loop.json", filepath.Join(root, ".crw/sessions/loop.json"))
		}, []string{"ledger.jsonl"}, "sessions/loop.json", nil},
		{"long_chain", func(t *testing.T, root, _ string) {
			dir := filepath.Join(root, ".crw/sessions")
			resetLinksLink(t, "c1", filepath.Join(dir, "a.json"))
			for i := 1; i < 9; i++ {
				resetLinksLink(t, "c"+strconv.Itoa(i+1), filepath.Join(dir, "c"+strconv.Itoa(i)))
			}
			resetLinksLink(t, "keep.txt", filepath.Join(dir, "c9"))
		}, []string{"sessions/a.json", "ledger.jsonl"}, "", map[string]string{"sessions/keep.txt": "keep", "sessions/c9": "keep"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, outside := resetLinksWorkspace(t)
			tc.link(t, root, outside)
			got, err := RunReset(root, State)
			if err != nil {
				t.Fatalf("RunReset: %v", err)
			}
			var want []string
			for _, rel := range tc.removed {
				want = append(want, filepath.Join(root, ".crw", rel))
				if _, err := os.Lstat(want[len(want)-1]); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s not removed: %v", rel, err)
				}
			}
			if !reflect.DeepEqual(got.Removed, want) {
				t.Errorf("removed %v, want %v", got.Removed, want)
			}
			if tc.stays != "" {
				link := filepath.Join(root, ".crw", tc.stays)
				if _, err := os.Lstat(link); err != nil || !slices.Contains(got.Absent, link) {
					t.Errorf("%s must stay and be absent: %v %v", tc.stays, err, got.Absent)
				}
			}
			for rel, content := range tc.kept {
				if got := resetLinksRead(t, filepath.Join(root, ".crw", rel)); got != content {
					t.Errorf("%s changed: %q", rel, got)
				}
			}
			if got := resetLinksRead(t, filepath.Join(outside, "keep.json")); got != "outside" {
				t.Errorf("outside file changed: %q", got)
			}
			if got := resetLinksRead(t, filepath.Join(outside, "dir/keep.json")); got != "outside dir" {
				t.Errorf("outside directory changed: %q", got)
			}
		})
	}
}

// TestResetLinksVerdictFollowsPinnedDirectory moves the pinned sessions
// directory away and puts another one in its place, as can happen between
// resetPin and the removal: the verdict must concern the pinned directory.
func TestResetLinksVerdictFollowsPinnedDirectory(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		pinnedHas    bool // keep.txt exists in the pinned directory
		replacement  bool // the replacement directory holds an a.json file
		failClosed   bool
		removed      bool
	}{
		{"existing_target_replacement_empty", "keep.txt", true, false, false, true},
		{"dangling_target_replacement_has_file", "keep.txt", false, true, false, false},
		{"escaping_target_fails_closed", "", true, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, outside := t.TempDir(), t.TempDir()
			target := tc.target
			if target == "" {
				target = filepath.Join(outside, "keep.json")
				resetLinksWrite(t, target, "outside")
			}
			sessions, moved := filepath.Join(base, "sessions"), filepath.Join(base, "moved")
			if err := os.Mkdir(sessions, 0o755); err != nil {
				t.Fatal(err)
			}
			resetLinksLink(t, target, filepath.Join(sessions, "a.json"))
			if tc.pinnedHas {
				resetLinksWrite(t, filepath.Join(sessions, "keep.txt"), "keep")
			}
			parent, err := os.OpenRoot(base)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			observed, err := parent.Lstat("sessions")
			if err != nil {
				t.Fatal(err)
			}
			pinned, err := resetPin(parent, "sessions", observed)
			if err != nil {
				t.Fatal(err)
			}
			defer pinned.Close()
			if err := os.Rename(sessions, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(sessions, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.replacement {
				resetLinksWrite(t, filepath.Join(sessions, "a.json"), "replacement")
			}
			var got ResetResult
			err = resetRmIfExists(pinned, "a.json", "a.json", &got)
			_, linkErr := os.Lstat(filepath.Join(moved, "a.json"))
			switch {
			case tc.failClosed:
				if err == nil || len(got.Removed) != 0 || linkErr != nil {
					t.Errorf("must fail closed and keep the link: %v %+v %v", err, got, linkErr)
				}
			case tc.removed:
				if err != nil || !reflect.DeepEqual(got.Removed, []string{"a.json"}) || !errors.Is(linkErr, os.ErrNotExist) {
					t.Errorf("must remove the pinned link: %v %+v %v", err, got, linkErr)
				}
			default:
				if err != nil || !reflect.DeepEqual(got.Absent, []string{"a.json"}) || linkErr != nil {
					t.Errorf("must keep the pinned dangling link: %v %+v %v", err, got, linkErr)
				}
			}
			if tc.replacement {
				if got := resetLinksRead(t, filepath.Join(sessions, "a.json")); got != "replacement" {
					t.Errorf("replacement changed: %q", got)
				}
			}
		})
	}
}

// TestResetLinksRawCwd resets through a cwd that holds a link and "..": the
// root's path is used as given, so the OS resolves it physically.
func TestResetLinksRawCwd(t *testing.T) {
	base, outside := t.TempDir(), t.TempDir()
	resetLinksWrite(t, filepath.Join(outside, "keep.json"), "outside")
	resetLinksWrite(t, filepath.Join(base, "actual/child/placeholder"), "x")
	link := filepath.Join(base, "actual/.crw/sessions/a.json")
	resetLinksWrite(t, filepath.Join(base, "actual/.crw/sessions/keep.txt"), "keep")
	resetLinksLink(t, filepath.Join(outside, "keep.json"), link)
	resetLinksLink(t, filepath.Join(base, "actual/child"), filepath.Join(base, "alias"))
	if _, err := RunReset(base+"/alias/..", State); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("link not removed: %v", err)
	}
	if got := resetLinksRead(t, filepath.Join(outside, "keep.json")); got != "outside" {
		t.Errorf("outside file changed: %q", got)
	}
}
