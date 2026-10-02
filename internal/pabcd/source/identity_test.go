package source

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// The oracle's source-identity.test.ts over real git repositories: T1-T21 and the two unnumbered tests. T15's
// compile-time half (a missing case must not type-check) has no Go form.

type step func(t *testing.T, root string)

func write(rel, content string) step {
	return func(t *testing.T, root string) { writeFile(t, root, rel, content) }
}
func remove(rel string) step {
	return func(t *testing.T, root string) { must(t, os.Remove(filepath.Join(root, rel))) }
}
func git(args ...string) step { return func(t *testing.T, root string) { gitIn(t, root, args...) } }
func steps(all ...step) step {
	return func(t *testing.T, root string) {
		for _, s := range all {
			s(t, root)
		}
	}
}

func mustCompare(t *testing.T, root string, before Identity, want ComparisonKind) Identity {
	t.Helper()
	after := Capture(root, Options{})
	if got := Compare(before, after); got.Kind != want {
		t.Fatalf("compare = %+v, want %q", got, want)
	}
	return after
}

func TestSourceIdentity(t *testing.T) {
	base := hermetic(t)
	quoted := "untracked \"quote\" and space.ts"
	swap := func(t *testing.T, root string) {
		must(t, os.Rename(filepath.Join(root, "a.ts"), filepath.Join(root, "b.ts")))
	}

	// An edit to the repository, whether the identity moved, and (revert) that undoing the edit restores it.
	for _, c := range []struct {
		name                    string
		prepare, change, revert step
		want                    ComparisonKind
	}{
		{name: "T1: two captures of a clean tree are the same", want: ComparisonSame},
		{name: "T3: a new untracked file differs", change: write("new.ts", "n\n"), want: ComparisonDifferent},
		{name: "T4: a file under a gitignored path is the same (we trust .gitignore)", change: write("ignored/junk.ts", "j\n"), want: ComparisonSame},
		{name: "T5: deleting a tracked file differs", change: remove("tracked.ts"), want: ComparisonDifferent},
		{name: "T6: renaming a tracked file differs", change: git("mv", "tracked.ts", "moved.ts"), want: ComparisonDifferent},
		{name: "T7: staging without committing differs", change: steps(write("staged.ts", "s\n"), git("add", "staged.ts")), want: ComparisonDifferent},
		{name: "T8: adding then removing an untracked file restores the identity", change: write("temp.ts", "t\n"), revert: remove("temp.ts"), want: ComparisonDifferent},
		{name: "T10: reverting a dirty edit restores the identity", change: write("tracked.ts", "dirty\n"), revert: write("tracked.ts", "a\n"), want: ComparisonDifferent},
		{name: "T16: paths containing spaces and quotes are reconstructed intact", change: write(quoted, "w\n"), revert: remove(quoted), want: ComparisonDifferent},
		{name: "T17: editing a file inside an untracked directory differs", prepare: write("untracked dir/new.ts", "one\n"), change: write("untracked dir/new.ts", "two\n"), want: ComparisonDifferent},
		{name: "T18: editing a renamed file again differs while the status stays RM", prepare: steps(git("mv", "tracked.ts", "renamed.ts"), write("renamed.ts", "one\n")), change: write("renamed.ts", "two\n"), want: ComparisonDifferent},
		{name: "T19: editing again under a steady MM status differs", prepare: steps(write("tracked.ts", "staged\n"), git("add", "tracked.ts"), write("tracked.ts", "one\n")), change: write("tracked.ts", "two\n"), want: ComparisonDifferent},
		{name: "T20: swapping which path is the rename source differs", prepare: git("mv", "tracked.ts", "a.ts"), change: swap, want: ComparisonDifferent},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := newRepo(t, base)
			if c.prepare != nil {
				c.prepare(t, root)
			}
			before := Capture(root, Options{})
			if c.change != nil {
				c.change(t, root)
			}
			mustCompare(t, root, before, c.want)
			if c.revert != nil {
				c.revert(t, root)
				mustCompare(t, root, before, ComparisonSame)
			}
		})
	}

	t.Run("T2: editing a tracked file differs and marks the tree dirty", func(t *testing.T) {
		root := newRepo(t, base)
		before := Capture(root, Options{})
		write("tracked.ts", "changed\n")(t, root)
		if !mustCompare(t, root, before, ComparisonDifferent).Dirty {
			t.Fatal("tree is not marked dirty")
		}
	})
	t.Run("T9: committing changes the sha even when the tree ends up identical", func(t *testing.T) {
		root := newRepo(t, base)
		before := Capture(root, Options{})
		steps(write("tracked.ts", "c\n"), git("add", "-A"), git("commit", "-qm", "second"))(t, root)
		if mustCompare(t, root, before, ComparisonDifferent).Dirty {
			t.Fatal("tree is dirty after the commit")
		}
	})
	t.Run("T11: a non-git directory is unavailable on both sides", func(t *testing.T) {
		id := Capture(tempIn(t, base), Options{})
		if id.Kind != KindUnavailable || Compare(id, id).Kind != ComparisonUnavailable {
			t.Fatalf("id = %+v", id)
		}
	})
	t.Run("T12: one unavailable side is unavailable, never different", func(t *testing.T) {
		if got := Compare(Capture(newRepo(t, base), Options{}), Capture(tempIn(t, base), Options{})).Kind; got != ComparisonUnavailable {
			t.Fatalf("compare = %q", got)
		}
	})
	t.Run("T13: describeSource shows a short sha and the dirty marker", func(t *testing.T) {
		root := newRepo(t, base)
		clean := Describe(Capture(root, Options{}))
		write("tracked.ts", "d\n")(t, root)
		dirty := Describe(Capture(root, Options{}))
		if !regexp.MustCompile(`^[0-9a-f]{7}$`).MatchString(clean) || !regexp.MustCompile(`^[0-9a-f]{7}\+dirty$`).MatchString(dirty) {
			t.Fatalf("clean %q, dirty %q", clean, dirty)
		}
	})
	t.Run("T14: same size and same mtime but different bytes still differs", func(t *testing.T) {
		root := newRepo(t, base)
		write("tracked.ts", "aaaa\n")(t, root)
		before := Capture(root, Options{})
		stamp := time.UnixMilli(1700000000000)
		pin := func() { must(t, os.Chtimes(filepath.Join(root, "tracked.ts"), stamp, stamp)) }
		pin()
		beforePinned := Capture(root, Options{})
		write("tracked.ts", "bbbb\n")(t, root)
		pin()
		if got := Compare(before, beforePinned).Kind; got != ComparisonSame {
			t.Fatalf("pinning the mtime moved the identity: %q", got)
		}
		mustCompare(t, root, beforePinned, ComparisonDifferent)
	})
	t.Run("T15: assertNever rejects an unhandled case at runtime", func(t *testing.T) {
		handled := func(r Comparison) (string, error) {
			switch r.Kind {
			case ComparisonSame, ComparisonDifferent, ComparisonUnavailable:
				return string(r.Kind), nil
			}
			return "", AssertNever(r)
		}
		for _, kind := range []ComparisonKind{ComparisonSame, ComparisonDifferent, ComparisonUnavailable} {
			if got, err := handled(Comparison{Kind: kind}); err != nil || got != string(kind) {
				t.Fatalf("handled(%q) = %q, %v", kind, got, err)
			}
		}
		if _, err := handled(Comparison{Kind: "bogus", Detail: "a -> b"}); err == nil || err.Error() != `unhandled case: {"kind":"bogus","detail":"a -> b"}` {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("T21: MD and AD statuses are recorded through the fallback rule", func(t *testing.T) {
		root := newRepo(t, base)
		before := Capture(root, Options{})
		steps(write("tracked.ts", "staged\n"), git("add", "tracked.ts"), remove("tracked.ts"),
			write("added.ts", "added\n"), git("add", "added.ts"), remove("added.ts"))(t, root)
		if status := gitIn(t, root, "status", "--porcelain=v1"); !regexp.MustCompile(`MD tracked\.ts`).MatchString(status) || !regexp.MustCompile(`AD added\.ts`).MatchString(status) {
			t.Fatalf("status:\n%s", status)
		}
		mustCompare(t, root, before, ComparisonDifferent)
	})
	t.Run("bound identities do not equate different worktrees with identical commits", func(t *testing.T) {
		bound := func(root string) Identity {
			return Identity{Kind: KindResolved, CommitSha: "same", CapturedAt: "2026-09-07", SourceRoot: &root}
		}
		a, unbound := bound("/worktree-a"), Identity{Kind: KindResolved, CommitSha: "same", CapturedAt: "2026-09-07"}
		for want, other := range map[ComparisonKind][]Identity{ComparisonDifferent: {bound("/worktree-b"), unbound}, ComparisonSame: {bound("/worktree-a")}} {
			for _, o := range other {
				if got := Compare(a, o).Kind; got != want {
					t.Fatalf("compare(a, %v) = %q, want %q", o.SourceRoot, got, want)
				}
			}
		}
	})
	t.Run("capture ignores inherited GIT_DIR/GIT_WORK_TREE and describes cwd's own tree", func(t *testing.T) {
		// A receipt captured "for" a bound worktree must never be computed from another repository that the
		// environment happens to route git to.
		target, decoy := newRepo(t, base), newRepo(t, base)
		writeFile(t, target, "only-in-target", "x")
		t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
		t.Setenv("GIT_WORK_TREE", decoy)
		id, decoyID := Capture(target, Options{}), Capture(decoy, Options{})
		if id.Kind != KindResolved || !id.Dirty {
			t.Fatalf("target's untracked file must be seen: %+v", id)
		}
		// The decoy is clean, so a capture routed there by the environment would read clean.
		if decoyID.Kind != KindResolved || decoyID.Dirty || id.TreeHash == decoyID.TreeHash {
			t.Fatalf("decoy: %+v, target: %+v", decoyID, id)
		}
	})
}
