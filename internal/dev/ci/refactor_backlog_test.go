//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The generated refactor backlog (CRW-759): docs/port/refactor-backlog.md is assembled from the
// fragments under docs/port/refactor-backlog.d. These tests drive the real command line through
// the built crw-dev, so they observe the same surface CI does. On the baseline the check is not
// registered, so every case below but the usage-error one fails with the invalid-choice exit 2.

// backlogDrift is the refusal the issue fixes verbatim; the implementation holds it in
// refactorBacklogDrift. It is spelled out here so this file compiles before the check exists and
// the red run shows the missing check rather than a compile error.
const backlogDrift = "edit the fragments under docs/port/refactor-backlog.d and run crw-dev ci refactor-backlog --write"

const (
	backlogDir  = "docs/port/refactor-backlog.d"
	backlogFile = "docs/port/refactor-backlog.md"
)

// fragment writes one file of the source tree.
func fragment(t *testing.T, r *fixtureRepo, path, text string) {
	t.Helper()
	r.write(backlogDir+"/"+path, text)
}

// backlogTree is the fixture the order cases use: three directories, one of them holding a section
// and two entry fragments, created out of name order.
func backlogTree(t *testing.T, r *fixtureRepo) {
	t.Helper()
	fragment(t, r, "02-b/_section.md", "## B\n\nB prose.\n")
	fragment(t, r, "01-a/_section.md", "## A\n\nA prose.\n")
	fragment(t, r, "01-a/CRW-1-2.md", "- second\n")
	fragment(t, r, "01-a/CRW-1-1.md", "- first\n")
	fragment(t, r, "00-head/_section.md", "# Head\n\nPreamble text.\n")
}

// backlogWant is the document backlogTree assembles to: directories by name, inside a directory
// _section.md first and then the other fragments by name, one blank line between the section and
// its entries, one between blocks, and one final newline.
const backlogWant = "# Head\n\nPreamble text.\n" +
	"\n## A\n\nA prose.\n\n- first\n- second" +
	"\n\n## B\n\nB prose.\n"

func readBacklog(t *testing.T, r *fixtureRepo) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.root, backlogFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// --write assembles the fragments in name order, with _section.md first in its directory.
func TestRefactorBacklogWriteAssemblesInOrder(t *testing.T) {
	r := newRepo(t)
	backlogTree(t, r)
	got := goCheck(t, r.root, nil, "refactor-backlog", "--write")
	if got.code != 0 {
		t.Fatalf("--write: %+v", got)
	}
	expectEqual(t, "generated", readBacklog(t, r), backlogWant)
}

// --check passes on the file --write just produced.
func TestRefactorBacklogCheckAcceptsItsOwnOutput(t *testing.T) {
	r := newRepo(t)
	backlogTree(t, r)
	if got := goCheck(t, r.root, nil, "refactor-backlog", "--write"); got.code != 0 {
		t.Fatalf("--write: %+v", got)
	}
	expectEqual(t, "--check", goCheck(t, r.root, nil, "refactor-backlog", "--check"), result{0, "", ""})
}

// --check refuses a fragment edited without regenerating, with the message the issue fixes.
func TestRefactorBacklogCheckRefusesDrift(t *testing.T) {
	r := newRepo(t)
	backlogTree(t, r)
	if got := goCheck(t, r.root, nil, "refactor-backlog", "--write"); got.code != 0 {
		t.Fatalf("--write: %+v", got)
	}
	fragment(t, r, "01-a/CRW-1-1.md", "- first, edited\n")
	expectEqual(t, "--check", goCheck(t, r.root, nil, "refactor-backlog", "--check"), result{1, "", backlogDrift + "\n"})
}

// A tree that holds neither the fragments nor the generated file is left alone: the check is
// silent and --write creates nothing. Every validate fixture has this shape.
func TestRefactorBacklogAbsentSourceIsANoOp(t *testing.T) {
	r := newRepo(t)
	expectEqual(t, "--write", goCheck(t, r.root, nil, "refactor-backlog", "--write"), result{0, "", ""})
	expectEqual(t, "--check", goCheck(t, r.root, nil, "refactor-backlog", "--check"), result{0, "", ""})
	if _, err := os.Stat(filepath.Join(r.root, backlogFile)); !os.IsNotExist(err) {
		t.Errorf("--write created %s in a tree with no fragments", backlogFile)
	}
}

// The generated file without its fragments is a tree the check cannot assemble: both modes refuse
// and --write leaves the committed file alone rather than deleting it.
func TestRefactorBacklogTargetWithoutSourceRefuses(t *testing.T) {
	r := newRepo(t)
	r.write(backlogFile, backlogWant)
	for _, mode := range []string{"--check", "--write"} {
		got := goCheck(t, r.root, nil, "refactor-backlog", mode)
		if got.code != 1 || !strings.Contains(got.stderr, "is missing") {
			t.Errorf("%s: %+v, want exit 1 naming the missing source", mode, got)
		}
	}
	expectEqual(t, "kept", readBacklog(t, r), backlogWant)
}

// Fragments without the generated file: --check refuses and --write recreates it byte-exactly.
func TestRefactorBacklogSourceWithoutTarget(t *testing.T) {
	r := newRepo(t)
	backlogTree(t, r)
	expectEqual(t, "--check", goCheck(t, r.root, nil, "refactor-backlog", "--check"), result{1, "", backlogDrift + "\n"})
	if got := goCheck(t, r.root, nil, "refactor-backlog", "--write"); got.code != 0 {
		t.Fatalf("--write: %+v", got)
	}
	expectEqual(t, "recreated", readBacklog(t, r), backlogWant)
}

// Neither mode, both modes, a stray argument and an unknown flag are usage errors.
func TestRefactorBacklogUsageErrors(t *testing.T) {
	r := newRepo(t)
	backlogTree(t, r)
	for _, args := range [][]string{
		{"refactor-backlog"},
		{"refactor-backlog", "--write", "--check"},
		{"refactor-backlog", "--check", "extra"},
		{"refactor-backlog", "--bogus"},
	} {
		if got := goCheck(t, r.root, nil, args[0], args[1:]...); got.code != 2 {
			t.Errorf("%v: %+v, want exit 2", args, got)
		}
	}
}

// An empty fragment and a directory with no _section.md are authoring mistakes the assembler names
// rather than silently dropping.
func TestRefactorBacklogRefusesMalformedFragments(t *testing.T) {
	r := newRepo(t)
	backlogTree(t, r)
	fragment(t, r, "01-a/CRW-9-9.md", "")
	got := goCheck(t, r.root, nil, "refactor-backlog", "--write")
	if got.code != 1 || !strings.Contains(got.stderr, "CRW-9-9.md") {
		t.Errorf("empty fragment: %+v, want exit 1 naming it", got)
	}
	os.Remove(filepath.Join(r.root, backlogDir, "01-a", "CRW-9-9.md"))
	os.Remove(filepath.Join(r.root, backlogDir, "01-a", "_section.md"))
	got = goCheck(t, r.root, nil, "refactor-backlog", "--write")
	if got.code != 1 || !strings.Contains(got.stderr, "01-a") {
		t.Errorf("missing _section.md: %+v, want exit 1 naming the directory", got)
	}
}

// validate runs the check, so a drifted backlog fails the job CI gates on.
func TestValidateRunsTheBacklogCheck(t *testing.T) {
	r := validateRepo(t)
	backlogTree(t, r)
	r.write(backlogFile, "# stale\n")
	got := validate(t, r)
	if got.code != 1 || !strings.Contains(got.stderr, backlogDrift) {
		t.Errorf("validate: %+v, want exit 1 naming the drift", got)
	}
}

// The repository's own fragments assemble to the committed file. This asserts the invariant and
// not the bytes, so it stays valid after every legitimate backlog change.
func TestTheCheckedInBacklogMatchesItsFragments(t *testing.T) {
	expectEqual(t, "--check", goCheck(t, repoRoot(), nil, "refactor-backlog", "--check"), result{0, "", ""})
}

// An entry fragment written without a trailing newline must not fuse with the next one: each
// entry is one bullet line, however its file was written.
func TestRefactorBacklogEntryWithoutTrailingNewline(t *testing.T) {
	r := newRepo(t)
	fragment(t, r, "01-a/_section.md", "## A\n")
	fragment(t, r, "01-a/CRW-2-1.md", "- first")
	fragment(t, r, "01-a/CRW-2-2.md", "- second\n")
	if got := goCheck(t, r.root, nil, "refactor-backlog", "--write"); got.code != 0 {
		t.Fatalf("--write: %+v", got)
	}
	expectEqual(t, "generated", readBacklog(t, r), "## A\n\n- first\n- second\n")
}
