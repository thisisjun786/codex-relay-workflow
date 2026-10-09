//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-968: the guard against a --write that deletes an entry compares whole entries, not their opening tags, and refuses where it cannot read.

// repeatedTagTree is a section whose entries share one tag, the way [todo30] repeats in the repository's own file.
func repeatedTagTree(t *testing.T, r *fixtureRepo) {
	t.Helper()
	fragment(t, r, "01-a/_section.md", "## A\n")
	fragment(t, r, "01-a/CRW-7-1.md", "- [todo30] internal/a - first - out-of-scope - evidence: none\n")
	fragment(t, r, "01-a/CRW-7-2.md", "- [todo30] internal/b - second - out-of-scope - evidence: none\n")
}

// An entry added by hand to the generated file under a tag that other entries already carry is not produced by any fragment: --write refuses, names it, and leaves the file.
func TestRefactorBacklogWriteRefusesAHandAddedEntryUnderARepeatedTag(t *testing.T) {
	r := newRepo(t)
	repeatedTagTree(t, r)
	if got := goCheck(t, r.root, nil, "refactor-backlog", "--write"); got.code != 0 {
		t.Fatalf("--write: %+v", got)
	}
	added := readBacklog(t, r) + "- [todo30] internal/c - added by hand, in no fragment - out-of-scope - evidence: none\n"
	r.write(backlogFile, added)
	got := goCheck(t, r.root, nil, "refactor-backlog", "--write")
	if got.code != 1 || !strings.Contains(got.stderr, "internal/c - added by hand") {
		t.Errorf("--write: %+v, want exit 1 naming the entry that no fragment produces", got)
	}
	expectEqual(t, "kept", readBacklog(t, r), added)
}

// The workflow the drift message prescribes still works: an entry edited in its fragment (same tag, new text) is rewritten by --write, and an entry removed from its fragment goes once its line is out of the file.
func TestRefactorBacklogWriteStillTakesAnEditedOrRemovedFragmentEntry(t *testing.T) {
	r := newRepo(t)
	repeatedTagTree(t, r)
	if got := goCheck(t, r.root, nil, "refactor-backlog", "--write"); got.code != 0 {
		t.Fatalf("--write: %+v", got)
	}
	fragment(t, r, "01-a/CRW-7-1.md", "- [todo30] internal/a - first, reworded - out-of-scope - evidence: none\n")
	if got := goCheck(t, r.root, nil, "refactor-backlog", "--write"); got.code != 0 {
		t.Fatalf("--write after an edit: %+v", got)
	}
	if !strings.Contains(readBacklog(t, r), "first, reworded") {
		t.Error("the edited entry was not written")
	}
	// An entry removed from its fragment is indistinguishable from one added by hand, so the line goes out of the generated file first.
	if err := os.Remove(filepath.Join(r.root, backlogDir, "01-a", "CRW-7-2.md")); err != nil {
		t.Fatal(err)
	}
	if got := goCheck(t, r.root, nil, "refactor-backlog", "--write"); got.code != 1 || !strings.Contains(got.stderr, "internal/b - second") {
		t.Fatalf("--write after a removal, the file still holding the entry: %+v, want exit 1 naming it", got)
	}
	r.write(backlogFile, strings.Replace(readBacklog(t, r), "- [todo30] internal/b - second - out-of-scope - evidence: none\n", "", 1))
	if got := goCheck(t, r.root, nil, "refactor-backlog", "--write"); got.code != 0 {
		t.Fatalf("--write after a removal: %+v", got)
	}
	if strings.Contains(readBacklog(t, r), "internal/b") {
		t.Error("the removed entry is still in the file")
	}
}

// A generated file that exists and cannot be read is not "nothing to lose": --write refuses and leaves it.
func TestRefactorBacklogWriteRefusesAnUnreadableGeneratedFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a file mode does not stop root from reading")
	}
	r := newRepo(t)
	repeatedTagTree(t, r)
	if got := goCheck(t, r.root, nil, "refactor-backlog", "--write"); got.code != 0 {
		t.Fatalf("--write: %+v", got)
	}
	target := filepath.Join(r.root, backlogFile)
	if err := os.Chmod(target, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o644) })
	for _, mode := range []string{"--write", "--check"} {
		got := goCheck(t, r.root, nil, "refactor-backlog", mode)
		if got.code != 1 || !strings.Contains(got.stderr, "refactor-backlog.md") {
			t.Errorf("%s: %+v, want exit 1 naming the unreadable file", mode, got)
		}
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readBacklog(t, r), "first") {
		t.Error("the refused write changed the file")
	}
}

// A tree that carries docs/port and neither the fragments nor the generated file lost the backlog: both modes refuse, and validate with them. A tree without docs/port has no backlog component
// (TestRefactorBacklogAbsentSourceIsANoOp).
func TestRefactorBacklogMissingInAPortTreeRefuses(t *testing.T) {
	r := validateRepo(t)
	r.write("docs/port/decisions.md", "# Decisions\n")
	for _, mode := range []string{"--check", "--write"} {
		got := goCheck(t, r.root, nil, "refactor-backlog", mode)
		if got.code != 1 || !strings.Contains(got.stderr, "neither") {
			t.Errorf("%s: %+v, want exit 1 saying both are missing", mode, got)
		}
	}
	if got := validate(t, r); got.code != 1 || !strings.Contains(got.stderr, "neither") {
		t.Errorf("validate: %+v, want exit 1 saying both are missing", got)
	}
}

// A stat error that is not "does not exist" (a parent directory without search permission) is no answer of "absent": the commands refuse and name it.
func TestRefactorBacklogStatErrorRefuses(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a directory mode does not stop root")
	}
	r := newRepo(t)
	repeatedTagTree(t, r)
	if got := goCheck(t, r.root, nil, "refactor-backlog", "--write"); got.code != 0 {
		t.Fatalf("--write: %+v", got)
	}
	port := filepath.Join(r.root, "docs", "port")
	if err := os.Chmod(port, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(port, 0o755) })
	for _, mode := range []string{"--check", "--write"} {
		got := goCheck(t, r.root, nil, "refactor-backlog", mode)
		if got.code != 1 || !strings.Contains(got.stderr, "permission denied") {
			t.Errorf("%s: %+v, want exit 1 with the stat error", mode, got)
		}
	}
}
