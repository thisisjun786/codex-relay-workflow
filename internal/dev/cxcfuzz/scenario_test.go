//go:build dev

package cxcfuzz

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// fsEntry is one entry of an input's "fs" array.
func fsEntry(path, kind, content, target string, mode int) pyjson.Object {
	entry := pyjson.Object{{Key: "path", Value: path}, {Key: "kind", Value: kind}}
	if content != "" {
		entry = entry.Set("content", content)
	}
	if target != "" {
		entry = entry.Set("target", target)
	}
	if mode != 0 {
		entry = entry.Set("mode", mode)
	}
	return entry
}

// fsInput is an input whose "fs" array is the given entries.
func fsInput(entries ...pyjson.Object) pyjson.Object {
	list := make([]any, 0, len(entries))
	for _, entry := range entries {
		list = append(list, entry)
	}
	return pyjson.Object{{Key: "fs", Value: list}}
}

// tree is every path under root with its kind, so two materialised roots can be compared.
func tree(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		kind := "file"
		switch {
		case info.IsDir():
			kind = "dir"
		case info.Mode()&os.ModeSymlink != 0:
			kind = "symlink"
		}
		found = append(found, kind+" "+rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(found)
	return found
}

// The same input builds the same tree in the two roots a case gets.
func TestScenariosBuildTheSameTreeInBothRoots(t *testing.T) {
	input := fsInput(
		fsEntry("dir", "dir", "", "", 0o755),
		fsEntry("dir/file.txt", "file", "hello", "", 0o644),
		fsEntry("link", "symlink", "", "dir/file.txt", 0),
	)
	one, two := t.TempDir(), t.TempDir()
	if n, err := Scenarios(one, input); err != nil || n != 3 {
		t.Fatalf("one: %d entries, %v", n, err)
	}
	if n, err := Scenarios(two, input); err != nil || n != 3 {
		t.Fatalf("two: %d entries, %v", n, err)
	}
	if a, b := tree(t, one), tree(t, two); !reflect.DeepEqual(a, b) {
		t.Fatalf("%v != %v", a, b)
	}
}

// A path outside the root is refused and nothing is materialised.
func TestScenariosRefuseAPathOutsideTheRoot(t *testing.T) {
	for _, path := range []string{"/etc/passwd", "../escape", "a/../../escape"} {
		root := t.TempDir()
		if _, err := Scenarios(root, fsInput(fsEntry(path, "file", "x", "", 0o644))); err == nil {
			t.Fatalf("%q was materialised", path)
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("%q left %v", path, entries)
		}
	}
}

// A symlink target outside the root is refused.
func TestScenariosRefuseASymlinkTargetOutsideTheRoot(t *testing.T) {
	root := t.TempDir()
	if _, err := Scenarios(root, fsInput(fsEntry("link", "symlink", "", "../outside", 0))); err == nil {
		t.Fatal("an outside symlink target was materialised")
	}
	if _, err := Scenarios(root, fsInput(fsEntry("link", "symlink", "", "/etc/passwd", 0))); err == nil {
		t.Fatal("an absolute symlink target was materialised")
	}
}

// A path that resolves through a symlink standing under the root is refused too: the lexical check
// alone would let the write land outside it.
func TestScenariosRefuseAPathThatResolvesThroughAnOutsideSymlink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := Scenarios(root, fsInput(fsEntry("escape/planted", "file", "x", "", 0o644))); err == nil {
		t.Fatal("a write through an outside symlink was materialised")
	}
	if _, err := os.Stat(filepath.Join(outside, "planted")); err == nil {
		t.Fatal("the write reached outside the case root")
	}
}

// Generated content is written, never executed: a file holding a shell command leaves the
// command's effect undone.
func TestScenarioContentIsNeverExecuted(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "marker")
	if _, err := Scenarios(root, fsInput(fsEntry("run.sh", "file", "touch "+marker+"\n", "", 0o755))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the generated command ran")
	}
	raw, err := os.ReadFile(filepath.Join(root, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), marker) {
		t.Fatal("the command text was not written as content")
	}
}

// ${ROOT} stands for one case's own root, so two roots compare equal.
func TestRootReplacement(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	if got := ReplaceRoot("wrote "+one+"/file", one); got != "wrote ${ROOT}/file" {
		t.Fatalf("%q", got)
	}
	if got := ReplaceRoot(two+"/file", one); got != two+"/file" {
		t.Fatalf("%q", got)
	}
	value := pyjson.Object{{Key: "path", Value: one + "/a"}, {Key: "list", Value: []any{one + "/b"}}}
	stripped := stripRoot(value, one)
	if got := canonical(stripped); got != `{"list": ["${ROOT}/b"], "path": "${ROOT}/a"}` {
		t.Fatalf("stripped %s", got)
	}
	keyed := pyjson.Object{{Key: one + "/a", Value: "x"}}
	if got := canonical(stripRoot(keyed, one)); got != `{"${ROOT}/a": "x"}` {
		t.Fatalf("keyed %s", got)
	}
}
