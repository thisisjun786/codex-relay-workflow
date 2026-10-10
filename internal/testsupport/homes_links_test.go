package testsupport

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1182 (testsupport snapshot, CRW-1176 follow-up): the content snapshot follows a link below a watched directory to
// the directory it names, records a link with its target, records a dangling root link with its target, marks a cycle and
// an alias, errors past the depth and entry budgets, and keeps HOME to its top-level names. Test-only helper: no production
// code reads it.

// linkHomes is a snapshot of a fresh HOME, CODEX_HOME and CRW_HOME, with the content snapshot switched on.
func linkHomes(t *testing.T) (*AccountHomes, string) {
	t.Helper()
	codex := t.TempDir()
	h := &AccountHomes{Home: t.TempDir(), Codex: codex, CRW: t.TempDir(), content: true}
	return h, codex
}

// mustContents returns the snapshot and fails the test on an error the snapshot should not report.
func mustContents(t *testing.T, h *AccountHomes) string {
	t.Helper()
	out, err := h.Contents()
	if err != nil {
		t.Fatalf("Contents: %v", err)
	}
	return out
}

func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A file under a directory link below the root is part of the snapshot: an in-place edit of it is seen.
func TestContentsFollowsALinkToADirectoryUnderTheRoot(t *testing.T) {
	h, codex := linkHomes(t)
	real := filepath.Join(codex, "real")
	writeFixture(t, filepath.Join(real, "f"), "one")
	if err := os.Symlink(real, filepath.Join(codex, "link")); err != nil {
		t.Fatal(err)
	}
	before := mustContents(t, h)
	if !strings.Contains(before, "/link/f ") {
		t.Fatalf("the snapshot does not walk the linked directory:\n%s", before)
	}
	writeFixture(t, filepath.Join(real, "f"), "two")
	if after := mustContents(t, h); after == before {
		t.Fatalf("an in-place edit of a file behind a link below the root is not seen:\n%s", after)
	}
}

// A link to a file is read through the link: an edit of the file it names is seen, wherever that file lies.
func TestContentsReadsALinkedFileThroughTheLink(t *testing.T) {
	h, codex := linkHomes(t)
	target := filepath.Join(t.TempDir(), "real.txt")
	writeFixture(t, target, "one")
	if err := os.Symlink(target, filepath.Join(codex, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	before := mustContents(t, h)
	writeFixture(t, target, "two")
	if after := mustContents(t, h); after == before {
		t.Fatalf("an edit of the file a link names is not seen:\n%s", after)
	}
}

// A directory link that leads out of the watched root is walked too, since the watch exists to see changes to the fixture
// the link reaches (CRW-1182 decision: the walk is bounded by the budgets, not by the root).
func TestContentsWalksALinkThatLeavesTheRoot(t *testing.T) {
	h, codex := linkHomes(t)
	outside := t.TempDir()
	writeFixture(t, filepath.Join(outside, "f"), "one")
	if err := os.Symlink(outside, filepath.Join(codex, "out")); err != nil {
		t.Fatal(err)
	}
	before := mustContents(t, h)
	writeFixture(t, filepath.Join(outside, "f"), "two")
	if after := mustContents(t, h); after == before {
		t.Fatalf("an edit behind a link that leaves the root is not seen:\n%s", after)
	}
}

// A dangling root link is recorded with its target string, so a change of that target is seen; it is not an absent directory.
func TestContentsRecordsADanglingRootLinkWithItsTarget(t *testing.T) {
	h, _ := linkHomes(t)
	crw := filepath.Join(t.TempDir(), "crw-link")
	h.CRW = crw
	if err := os.Symlink("gone-one", crw); err != nil {
		t.Fatal(err)
	}
	before := mustContents(t, h)
	if !strings.Contains(before, "-> gone-one (dangling)") {
		t.Fatalf("the dangling root link does not record its target:\n%s", before)
	}
	if err := os.Remove(crw); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("gone-two", crw); err != nil {
		t.Fatal(err)
	}
	if after := mustContents(t, h); after == before {
		t.Fatalf("a change of the dangling root link's target is not seen:\n%s", after)
	}
}

// A link back to its own directory ends the walk at the link and says so: the walk terminates and the cycle is marked.
func TestContentsMarksACycleAndTerminates(t *testing.T) {
	h, codex := linkHomes(t)
	writeFixture(t, filepath.Join(codex, "f"), "one")
	if err := os.Symlink(".", filepath.Join(codex, "loop")); err != nil {
		t.Fatal(err)
	}
	out := mustContents(t, h)
	if !strings.Contains(out, "loop Lrwxrwxrwx -> . (cycle)") {
		t.Fatalf("a cycle is not marked at its link:\n%s", out)
	}
}

// Two links to one directory that is not their ancestor: the directory is walked once and the second link is an alias of it.
func TestContentsWalksACommonDirectoryOnceAndMarksTheAlias(t *testing.T) {
	h, codex := linkHomes(t)
	real := filepath.Join(codex, "real")
	writeFixture(t, filepath.Join(real, "f"), "one")
	for _, name := range []string{"a", "b"} {
		if err := os.Symlink(real, filepath.Join(codex, name)); err != nil {
			t.Fatal(err)
		}
	}
	out := mustContents(t, h)
	base := filepath.Base(codex)
	if !strings.Contains(out, base+"/a/f ") {
		t.Fatalf("the first link is not walked:\n%s", out)
	}
	if strings.Contains(out, base+"/b/f ") {
		t.Fatalf("the alias is walked a second time:\n%s", out)
	}
	if !strings.Contains(out, base+"/b Lrwxrwxrwx -> "+real+" (alias of "+base+"/a)") {
		t.Fatalf("the alias is not marked with the link it repeats:\n%s", out)
	}
}

// A directory nested deeper than the depth limit is an error: a snapshot that stopped early is never taken as complete.
func TestContentsErrorsPastTheDepthLimit(t *testing.T) {
	h, codex := linkHomes(t)
	deep := codex
	for i := 0; i <= snapshotMaxDepth; i++ {
		deep = filepath.Join(deep, fmt.Sprintf("d%d", i))
	}
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Contents(); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("a walk past the depth limit must error, got %v", err)
	}
}

// A walk past the entry budget is an error, not a partial success.
func TestContentsErrorsPastTheEntryBudget(t *testing.T) {
	h, codex := linkHomes(t)
	outside := t.TempDir()
	for i := 0; i <= snapshotMaxEntries; i++ {
		writeFixture(t, filepath.Join(outside, fmt.Sprintf("e%05d", i)), "")
	}
	if err := os.Symlink(outside, filepath.Join(codex, "many")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Contents(); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("a walk past the entry budget must error, got %v", err)
	}
}

// HOME keeps its top-level names only: a link at HOME's top level is a name, and an edit behind it is not a stray write.
// This is the exception the retrust fixture relies on, and it must hold after the links below the root are followed.
func TestContentsKeepsHomeToItsTopLevelNames(t *testing.T) {
	h, _ := linkHomes(t)
	h.watchHome = true // dirs() returns HOME itself only for a watched HOME: without it this test never reaches the HOME branch
	outside := t.TempDir()
	writeFixture(t, filepath.Join(outside, "f"), "one")
	if err := os.Symlink(outside, filepath.Join(h.Home, "link")); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(h.Home, "sub", "deep"), "one")
	before := mustContents(t, h)
	if !strings.Contains(before, "HOME: link,sub") {
		t.Fatalf("HOME is not listed by its top-level names:\n%s", before)
	}
	writeFixture(t, filepath.Join(outside, "f"), "two")
	writeFixture(t, filepath.Join(h.Home, "sub", "deep"), "two")
	if after := mustContents(t, h); after != before {
		t.Fatalf("an edit behind or below a HOME top-level name is walked into HOME:\nbefore %s\nafter  %s", before, after)
	}
	writeFixture(t, filepath.Join(h.Home, "stray"), "new")
	if after := mustContents(t, h); after == before {
		t.Fatalf("a new top-level name in HOME is not seen:\n%s", after)
	}
}

// fileLines are the snapshot lines of the regular file named base (a "/<base> " path that carries a digest).
func fileLines(out, base string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "/"+base+" -rw") && strings.Contains(l, "sha256:") {
			lines = append(lines, l)
		}
	}
	return lines
}

// One directory reached as a plain directory and through a link is read once, whichever of the two the walk meets first:
// the link name sorts before the plain name ("a" < "real") or after it ("z" > "real").
func TestContentsReadsADirectoryOnceWhetherTheLinkComesBeforeOrAfterIt(t *testing.T) {
	for _, linkName := range []string{"a", "z"} {
		t.Run(linkName, func(t *testing.T) {
			h, codex := linkHomes(t)
			real := filepath.Join(codex, "real")
			writeFixture(t, filepath.Join(real, "f"), "one")
			if err := os.Symlink(real, filepath.Join(codex, linkName)); err != nil {
				t.Fatal(err)
			}
			before := mustContents(t, h)
			if got := fileLines(before, "f"); len(got) != 1 {
				t.Fatalf("the directory's file is read %d times, want once:\n%s", len(got), before)
			}
			if !strings.Contains(before, "(alias of ") {
				t.Fatalf("the second path to the directory is not marked as an alias:\n%s", before)
			}
			writeFixture(t, filepath.Join(real, "f"), "two")
			if after := mustContents(t, h); after == before {
				t.Fatalf("an edit in a directory reached by a plain path and a link is not seen:\n%s", after)
			}
		})
	}
}

// A 40 MiB fixture reached by a plain path and a link fits the 64 MiB byte budget because it is read once.
func TestContentsCountsAnAliasedFixtureOnceAgainstTheByteBudget(t *testing.T) {
	h, codex := linkHomes(t)
	real := filepath.Join(codex, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "big"), make([]byte, 40<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(codex, "a")); err != nil {
		t.Fatal(err)
	}
	mustContents(t, h)
}

// A file read through the byte budget is capped: a snapshot past the budget is an error.
func TestContentsErrorsPastTheByteBudget(t *testing.T) {
	h, codex := linkHomes(t)
	for _, n := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(codex, n), make([]byte, snapshotMaxBytes/2+1), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.Contents(); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("a walk past the byte budget must error, got %v", err)
	}
}

// chain builds n nested directories below root's directory: each of the n levels is a directory reached through a link
// (kind 'l'), a plain directory ('p'), or alternating ones ('m'). The deepest holds the file leaf.
func chain(t *testing.T, root string, n int, kind byte) {
	t.Helper()
	cur := root
	for i := 1; i <= n; i++ {
		linked := kind == 'l' || (kind == 'm' && i%2 == 1)
		name := fmt.Sprintf("d%d", i)
		if linked {
			target := filepath.Join(t.TempDir(), name)
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(cur, name)); err != nil {
				t.Fatal(err)
			}
			cur = target
			continue
		}
		cur = filepath.Join(cur, name)
		if err := os.MkdirAll(cur, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, filepath.Join(cur, "leaf"), "x")
}

// The depth limit counts a linked directory like a plain one: 32 directories are walked, 33 are an error, for plain,
// linked and mixed chains alike.
func TestContentsDepthLimitIsThirtyTwoDirectoriesForPlainLinkedAndMixedChains(t *testing.T) {
	for _, kind := range []byte{'p', 'l', 'm'} {
		for _, n := range []int{snapshotMaxDepth, snapshotMaxDepth + 1} {
			t.Run(fmt.Sprintf("%c%d", kind, n), func(t *testing.T) {
				h, codex := linkHomes(t)
				chain(t, codex, n, kind)
				out, err := h.Contents()
				if n <= snapshotMaxDepth {
					if err != nil || !strings.Contains(out, "/leaf ") {
						t.Fatalf("%d directories are within the limit, got err %v\n%s", n, err, out)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "depth") {
					t.Fatalf("%d directories must be a depth error, got %v", n, err)
				}
			})
		}
	}
}
