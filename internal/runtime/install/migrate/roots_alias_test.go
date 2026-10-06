package migrate

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

// identityOf returns the real identity of an existing directory, read before any seam replacement.
func identityOf(t *testing.T, path string) fileID {
	t.Helper()
	d, _, err := pinDir(path)
	must(t, err)
	if d == nil {
		t.Fatalf("identityOf(%s): not a directory", path)
	}
	defer d.Close()
	return d.id
}

// aliasIdentity makes alias report target's identity, which is what a bind mount does on Linux: one directory reached
// by a second name keeps its (dev, ino). The seam is process-wide, so this restores it with t.Cleanup and the tests that
// use it stay sequential.
func aliasIdentity(t *testing.T, alias, target string) {
	t.Helper()
	targetID := identityOf(t, target)
	real := dirIdentity
	dirIdentity = func(f *os.File) (fileID, error) {
		if f.Name() == alias {
			return targetID, nil
		}
		return real(f)
	}
	t.Cleanup(func() { dirIdentity = real })
}

// A destination reached through a bind mount of a source subdirectory must be refused, and nothing may be created.
func TestOpenRefusesDestinationBehindAnAlias(t *testing.T) {
	base := isolate(t)
	src, alias := base+"/source", base+"/alias"
	mkdirs(t, src+"/sub", alias)
	put(t, src+"/sub/keep", "keep", 0o600)
	aliasIdentity(t, alias, src+"/sub")
	before := tree(t, src)
	r, err := Open(Options{Scope: ScopeUser, FromHome: src, ToHome: alias + "/new"})
	if err == nil {
		_, derr := r.User.EnsureDest(0o700)
		r.Close()
		t.Fatalf("Open accepted a destination whose ancestor %s reports the identity of %s (a bind mount); EnsureDest returned %v", alias, src+"/sub", derr)
	}
	wantRefusal(t, err, ReasonOverlap)
	var re *RefusedError
	if !errors.As(err, &re) || !strings.Contains(re.Detail, "through another spelling (a bind mount)") {
		t.Errorf("refusal = %v, want the bind-mount wording", err)
	}
	if got := tree(t, src); !reflect.DeepEqual(before, got) {
		t.Errorf("a refused Open changed the source tree: %v", got)
	}
	if _, err := os.Lstat(alias + "/new"); err == nil {
		t.Error("a refused Open created the destination")
	}
}

// The reverse direction: a source that reaches the destination through a second spelling must be refused too.
func TestOpenRefusesSourceBehindAnAlias(t *testing.T) {
	base := isolate(t)
	dst, alias := base+"/dest", base+"/alias"
	mkdirs(t, dst+"/sub", alias)
	put(t, dst+"/sub/keep", "keep", 0o600)
	aliasIdentity(t, alias, dst+"/sub")
	before := tree(t, dst)
	r, err := Open(Options{Scope: ScopeUser, FromHome: alias, ToHome: dst})
	if err == nil {
		r.Close()
		t.Fatalf("Open accepted a source whose identity lies inside the destination tree")
	}
	wantRefusal(t, err, ReasonOverlap)
	if got := tree(t, dst); !reflect.DeepEqual(before, got) {
		t.Errorf("a refused Open changed the destination tree: %v", got)
	}
}

// A Codex home that reaches a selected tree through a second spelling must be refused as well.
func TestOpenRefusesCodexHomeBehindAnAlias(t *testing.T) {
	base := isolate(t)
	ws, src, other, cx := base+"/ws", base+"/src", base+"/other", base+"/cx"
	mkdirs(t, ws, src+"/sub", other, cx)
	aliasIdentity(t, cx, src+"/sub")
	r, err := Open(Options{Scope: ScopeAll, Cwd: ws, FromHome: src, ToHome: other, CodexHome: cx})
	if err == nil {
		r.Close()
		t.Fatalf("Open accepted a Codex home whose identity lies inside a selected tree")
	}
	wantRefusal(t, err, ReasonOverlap)
}

// A destination whose identities are all outside the source tree stays accepted.
func TestOpenAcceptsATreeOutsideTheSource(t *testing.T) {
	base := isolate(t)
	src, dst := base+"/src", base+"/dst"
	mkdirs(t, src+"/a/b", dst+"/c/d")
	put(t, src+"/a/b/one", "1", 0o600)
	put(t, dst+"/c/d/two", "2", 0o600)
	r, err := Open(Options{Scope: ScopeUser, FromHome: src, ToHome: dst})
	must(t, err)
	defer r.Close()
	if r.User.Source == nil || r.User.Dest == nil {
		t.Fatal("Open did not pin both existing roots")
	}
}

// A link inside a tree is skipped, never followed: following it would put the destination's identity inside the source
// tree and refuse an otherwise disjoint pair.
func TestOpenDoesNotFollowALinkInsideATree(t *testing.T) {
	base := isolate(t)
	src, dst := base+"/src", base+"/dst"
	mkdirs(t, src, dst)
	must(t, os.Symlink(dst, src+"/link-to-dest"))
	r, err := Open(Options{Scope: ScopeUser, FromHome: src, ToHome: dst})
	must(t, err)
	r.Close()
}

// A tree the walk cannot read refuses (fail closed) rather than passing an unproven comparison.
func TestOpenRefusesAnUnreadableTreeDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a directory whose mode is 0")
	}
	base := isolate(t)
	src, dst := base+"/src", base+"/dst"
	mkdirs(t, src+"/locked", dst)
	put(t, src+"/locked/file", "x", 0o600)
	must(t, os.Chmod(src+"/locked", 0o000))
	t.Cleanup(func() { _ = os.Chmod(src+"/locked", 0o755) })
	_, err := Open(Options{Scope: ScopeUser, FromHome: src, ToHome: dst})
	wantRefusal(t, err, ReasonOverlap)
}

// A destination that holds a directory of the source tree below its own root reaches into the source tree: the same
// directory is reachable below both roots, so a write under the destination would land inside the source tree. A bind
// mount below a selected root is invisible to the root chains, so the two trees' identity sets must be compared too.
func TestOpenRefusesASharedDescendantBetweenTrees(t *testing.T) {
	base := isolate(t)
	src, dst := base+"/src", base+"/dst"
	mkdirs(t, src+"/sub", dst+"/inside")
	put(t, src+"/sub/keep", "keep", 0o600)
	aliasIdentity(t, dst+"/inside", src+"/sub")
	before := tree(t, src)
	r, err := Open(Options{Scope: ScopeUser, FromHome: src, ToHome: dst})
	if err == nil {
		r.Close()
		t.Fatalf("Open accepted a destination holding %s, the identity of %s inside the source tree", dst+"/inside", src+"/sub")
	}
	wantRefusal(t, err, ReasonOverlap)
	if got := tree(t, src); !reflect.DeepEqual(before, got) {
		t.Errorf("a refused Open changed the source tree: %v", got)
	}
}

// Two spellings of one directory can show different submounts, so the walk must visit every spelling instead of
// deduplicating on identity alone: a mount below the second spelling is still inside the source tree.
func TestOpenWalksEverySpellingOfADirectory(t *testing.T) {
	base := isolate(t)
	src, dst := base+"/src", base+"/dst"
	mkdirs(t, src+"/plan/first", src+"/plan/second/mounted", dst)
	aliasIdentity(t, src+"/plan/first", src+"/plan/second")
	aliasIdentity(t, dst, src+"/plan/second/mounted")
	r, err := Open(Options{Scope: ScopeUser, FromHome: src, ToHome: dst})
	if err == nil {
		r.Close()
		t.Fatalf("Open accepted a destination reachable below the source tree through the second spelling %s", src+"/plan/second")
	}
	wantRefusal(t, err, ReasonOverlap)
}

// A destination that shares a directory with the source tree below its root reaches into the source tree: the same
