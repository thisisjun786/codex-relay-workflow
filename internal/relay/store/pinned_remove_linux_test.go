//go:build linux

package store

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// UnlinkPinned removes a file under a directory the child controls. The tests build a temporary tree and plant what the child can: links in every component, a FIFO, a file swapped for another one, and a
// component swapped for a link while the caller is deciding (the moment between the read and the unlink, where a removal by path would be redirected).

type pinnedTree struct {
	t                       *testing.T
	parent, root, dir, file string // file is the full path of the copy
}

func newPinnedTree(t *testing.T, content string) *pinnedTree {
	t.Helper()
	base := t.TempDir()
	tree := &pinnedTree{t: t, parent: filepath.Join(base, "parent")}
	tree.root = filepath.Join(tree.parent, "artifacts")
	tree.dir = filepath.Join(tree.root, "dag-input-manifests")
	tree.file = filepath.Join(tree.dir, "copy.json")
	if err := os.MkdirAll(tree.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tree.file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return tree
}

// remove runs UnlinkPinned over the tree with a decide that records what it was shown.
func (p *pinnedTree) remove(limit int, decide func(raw []byte) (bool, error)) (PinnedRemoval, error) {
	p.t.Helper()
	return UnlinkPinned(p.root, "dag-input-manifests", "copy.json", limit, decide)
}

func yes(raw []byte) (bool, error) { return true, nil }

// outsideTree is another place with a file of the same name and the same content that a redirected removal would reach.
func outsideTree(t *testing.T, content string) (outside, victim string) {
	t.Helper()
	outside = t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "artifacts", "dag-input-manifests"), 0o700); err != nil {
		t.Fatal(err)
	}
	victim = filepath.Join(outside, "artifacts", "dag-input-manifests", "copy.json")
	if err := os.WriteFile(victim, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return outside, victim
}

func requireStillThere(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("%s is gone: %v", path, err)
	}
}

func TestUnlinkPinnedRemovesAfterDecide(t *testing.T) {
	tree := newPinnedTree(t, "the manifest")
	var seen string
	got, err := tree.remove(1024, func(raw []byte) (bool, error) { seen = string(raw); return true, nil })
	if err != nil || got != PinnedRemoved || seen != "the manifest" {
		t.Fatalf("remove = %v %v, decide saw %q", got, err, seen)
	}
	if _, err := os.Lstat(tree.file); !os.IsNotExist(err) {
		t.Fatalf("the file is still there: %v", err)
	}
}

func TestUnlinkPinnedKeepsWhatDecideKeeps(t *testing.T) {
	tree := newPinnedTree(t, "the manifest")
	if got, err := tree.remove(1024, func([]byte) (bool, error) { return false, nil }); err != nil || got != PinnedKept {
		t.Fatalf("remove = %v %v", got, err)
	}
	requireStillThere(t, tree.file)
	boom := errors.New("the reliance check failed")
	if got, err := tree.remove(1024, func([]byte) (bool, error) { return true, boom }); !errors.Is(err, boom) || got == PinnedRemoved {
		t.Fatalf("remove = %v %v", got, err)
	}
	requireStillThere(t, tree.file)
}

func TestUnlinkPinnedAbsent(t *testing.T) {
	tree := newPinnedTree(t, "x")
	if err := os.Remove(tree.file); err != nil {
		t.Fatal(err)
	}
	calls := 0
	decide := func([]byte) (bool, error) { calls++; return true, nil }
	for name, call := range map[string]func() (PinnedRemoval, error){
		"the file": func() (PinnedRemoval, error) {
			return UnlinkPinned(tree.root, "dag-input-manifests", "copy.json", 1024, decide)
		},
		"the directory": func() (PinnedRemoval, error) { return UnlinkPinned(tree.root, "missing", "copy.json", 1024, decide) },
		"the root": func() (PinnedRemoval, error) {
			return UnlinkPinned(filepath.Join(tree.parent, "none"), "dag-input-manifests", "copy.json", 1024, decide)
		},
		"an ancestor": func() (PinnedRemoval, error) {
			return UnlinkPinned(filepath.Join(tree.parent, "none", "artifacts"), "dag-input-manifests", "copy.json", 1024, decide)
		},
	} {
		if got, err := call(); err != nil || got != PinnedAbsent {
			t.Fatalf("absent %s: %v %v", name, got, err)
		}
	}
	if calls != 0 {
		t.Fatal("decide was asked about a file that is not there")
	}
}

// A link in any component, or as the file, is refused before decide is asked, and what it points to is untouched.
func TestUnlinkPinnedRefusesALinkInAnyComponent(t *testing.T) {
	cases := map[string]struct {
		plant  func(tree *pinnedTree, outside string)
		reason string
	}{
		"the root": {func(tree *pinnedTree, outside string) {
			replaceWithLink(t, tree.root, filepath.Join(outside, "artifacts"))
		}, ReasonSymlinkComponent},
		"an ancestor": {func(tree *pinnedTree, outside string) { replaceWithLink(t, tree.parent, outside) }, ReasonSymlinkComponent},
		"the directory": {func(tree *pinnedTree, outside string) {
			replaceWithLink(t, tree.dir, filepath.Join(outside, "artifacts", "dag-input-manifests"))
		}, ReasonSymlinkComponent},
		"the file": {func(tree *pinnedTree, outside string) {
			replaceWithLink(t, tree.file, filepath.Join(outside, "artifacts", "dag-input-manifests", "copy.json"))
		}, ReasonNotARegularFile},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			tree := newPinnedTree(t, "the manifest")
			outside, victim := outsideTree(t, "the manifest")
			c.plant(tree, outside)
			calls := 0
			got, err := tree.remove(1024, func([]byte) (bool, error) { calls++; return true, nil })
			var refused *RefusedError
			if !errors.As(err, &refused) || refused.Reason != c.reason || got == PinnedRemoved || calls != 0 {
				t.Fatalf("remove = %v %v (decide asked %d times), want a refusal %s", got, err, calls, c.reason)
			}
			requireStillThere(t, victim)
		})
	}
}

func replaceWithLink(t *testing.T, path, target string) {
	t.Helper()
	if err := os.Rename(path, path+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

// While the caller decides, an ancestor, the directory or the file is swapped. Every name was opened by descriptor, so the unlink lands on the directory that was read, and the tree behind the link is untouched.
func TestUnlinkPinnedIsNotRedirectedByASwapWhileDeciding(t *testing.T) {
	for name, swap := range map[string]func(tree *pinnedTree, outside string){
		"an ancestor": func(tree *pinnedTree, outside string) { replaceWithLink(t, tree.parent, outside) },
		"the root": func(tree *pinnedTree, outside string) {
			replaceWithLink(t, tree.root, filepath.Join(outside, "artifacts"))
		},
		"the directory": func(tree *pinnedTree, outside string) {
			replaceWithLink(t, tree.dir, filepath.Join(outside, "artifacts", "dag-input-manifests"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			tree := newPinnedTree(t, "the manifest")
			outside, victim := outsideTree(t, "the manifest")
			got, err := tree.remove(1024, func([]byte) (bool, error) { swap(tree, outside); return true, nil })
			if err != nil || got != PinnedRemoved {
				t.Fatalf("remove = %v %v", got, err)
			}
			requireStillThere(t, victim)
			moved := tree.file
			switch name {
			case "an ancestor":
				moved = filepath.Join(tree.parent+".moved", "artifacts", "dag-input-manifests", "copy.json")
			case "the root":
				moved = filepath.Join(tree.root+".moved", "dag-input-manifests", "copy.json")
			case "the directory":
				moved = filepath.Join(tree.dir+".moved", "copy.json")
			}
			if _, err := os.Lstat(moved); !os.IsNotExist(err) {
				t.Fatalf("the file that was read is still at %s: %v", moved, err)
			}
		})
	}
}

// While the walk itself is under way, an ancestor is swapped for a link right after it was opened: the next component is opened relative to the descriptor that was already open, so the removal still lands on the
// original tree. A walk that opened components by path would be redirected to the victim.
func TestUnlinkPinnedIsNotRedirectedByASwapDuringTheWalk(t *testing.T) {
	tree := newPinnedTree(t, "the manifest")
	outside, victim := outsideTree(t, "the manifest")
	afterPinnedComponent = func(path string) {
		if path == tree.parent {
			afterPinnedComponent = nil
			replaceWithLink(t, tree.parent, outside)
		}
	}
	t.Cleanup(func() { afterPinnedComponent = nil })
	got, err := tree.remove(1024, yes)
	if err != nil || got != PinnedRemoved {
		t.Fatalf("remove = %v %v", got, err)
	}
	requireStillThere(t, victim)
	if _, err := os.Lstat(filepath.Join(tree.parent+".moved", "artifacts", "dag-input-manifests", "copy.json")); !os.IsNotExist(err) {
		t.Fatalf("the file that was read is still there: %v", err)
	}
}

// A FIFO opens at once (O_NONBLOCK), is not a regular file and is refused; the call does not wait for a writer.
func TestUnlinkPinnedDoesNotBlockOnAFifo(t *testing.T) {
	tree := newPinnedTree(t, "x")
	if err := os.Remove(tree.file); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(tree.file, 0o600); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		got PinnedRemoval
		err error
	}
	done := make(chan answer, 1)
	go func() {
		got, err := tree.remove(1024, yes)
		done <- answer{got, err}
	}()
	select {
	case a := <-done:
		var refused *RefusedError
		if !errors.As(a.err, &refused) || refused.Reason != ReasonNotARegularFile || a.got == PinnedRemoved {
			t.Fatalf("remove = %v %v", a.got, a.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the call blocked on a FIFO")
	}
	requireStillThere(t, tree.file)
}

func TestUnlinkPinnedRefusesAFileOverTheLimit(t *testing.T) {
	tree := newPinnedTree(t, "0123456789")
	calls := 0
	got, err := tree.remove(4, func([]byte) (bool, error) { calls++; return true, nil })
	if !errors.Is(err, ErrPinnedTooLarge) || got == PinnedRemoved || calls != 0 {
		t.Fatalf("remove = %v %v (decide asked %d times)", got, err, calls)
	}
	requireStillThere(t, tree.file)
}

// The name is checked against the file that was read just before the unlink: a file put in its place while the caller decided is not removed.
func TestUnlinkPinnedKeepsAFileThatReplacedTheOneThatWasRead(t *testing.T) {
	tree := newPinnedTree(t, "the manifest")
	got, err := tree.remove(1024, func([]byte) (bool, error) {
		if err := os.Rename(tree.file, tree.file+".old"); err != nil {
			return false, err
		}
		return true, os.WriteFile(tree.file, []byte("another file"), 0o600)
	})
	if !errors.Is(err, ErrPinnedChanged) || got == PinnedRemoved {
		t.Fatalf("remove = %v %v", got, err)
	}
	raw, rerr := os.ReadFile(tree.file)
	if rerr != nil || string(raw) != "another file" {
		t.Fatalf("the file that replaced it was touched: %q %v", raw, rerr)
	}
}

func TestUnlinkPinnedRefusesARelativeRoot(t *testing.T) {
	if _, err := UnlinkPinned("artifacts", "dag-input-manifests", "copy.json", 1024, yes); err == nil {
		t.Fatal("a relative root was accepted")
	}
}
