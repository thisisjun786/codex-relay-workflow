package tools

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// review836Seam installs the createRoot mkdir seam for one test and restores the production value
// after it. The seam is package state, so a test that uses it must not run in parallel.
func review836Seam(t *testing.T, seam func(path string)) {
	t.Helper()
	saved := createRootBeforeMkdir
	createRootBeforeMkdir = seam
	t.Cleanup(func() { createRootBeforeMkdir = saved })
}

// review836LinkHost builds the shape the defect needs: a root spelled <base>/x/../p/q whose
// component before the ".." is a symbolic link to a sibling directory. Removing that link makes
// Lstat(<base>/x/../p) fail while <base>/p - the directory this call made - is still there, which
// is how a scan can report a path missing that is not missing. A symbolic link is used because it
// is the shape in which the component before the ".." can be taken away without taking the
// directory made below it: removing the link removes nothing else, while removing a real directory
// would first have to empty the directory this call made inside it.
func review836LinkHost(t *testing.T) (link, root, target string) {
	t.Helper()
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(base, "x")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// The spelling is kept as the caller wrote it: filepath.Join would clean the ".." away, and
	// the race is exactly about a component before that "..".
	root = link + "/../p/q"
	target = filepath.Join(base, "p")
	return link, root, target
}

// C1, C3: the component before the ".." is removed by a concurrent install while the directory
// this call made below it is still there. The scan then reports that directory missing, x is made
// again and the mkdir of the directory this call already made answers EEXIST. It is still this
// call's directory, so the record must keep it and the cleanup must leave neither x nor that
// directory behind.
func TestToolsReview836KeepsItsOwnDirectoryWhenAnAncestorVanishes(t *testing.T) {
	link, root, target := review836LinkHost(t)

	removed := false
	review836Seam(t, func(path string) {
		if path != root || removed {
			return
		}
		removed = true
		if err := os.Remove(link); err != nil {
			t.Errorf("the seam could not remove the ancestor: %v", err)
		}
	})

	created, err := createRoot(root)
	if err != nil {
		t.Fatalf("createRoot with an ancestor removed between its scan and its mkdir: %v", err)
	}
	removeCreated(created)
	for _, path := range []string{link, target} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("%s survived the cleanup of %v: %v", path, created, statErr)
		}
	}
}

// C3: the same race, but the ancestor keeps vanishing, so the recompute budget runs out. createRoot
// answers the not-exist error it gave up on and removes what it made; nothing it created is left
// under the root for the next run to trip over.
func TestToolsReview836GivesUpWithoutLeavingItsDirectories(t *testing.T) {
	link, root, target := review836LinkHost(t)

	attempts := 0
	review836Seam(t, func(path string) {
		if path != root {
			return
		}
		attempts++
		if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the seam could not remove the ancestor: %v", err)
		}
	})

	created, err := createRoot(root)
	if err == nil {
		t.Fatalf("createRoot answered success with %v while the ancestor kept vanishing", created)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("createRoot answered %v, want the not-exist error it gave up on", err)
	}
	if attempts < 2 {
		t.Errorf("createRoot reached the target mkdir %d times, want the walk retried", attempts)
	}
	for _, path := range []string{link, target} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("%s was left behind after createRoot gave up: %v", path, statErr)
		}
	}
}

// C2: the directory standing at a recorded path is no longer the one this call made, so it is not
// this call's to remove. The other install's directory is made while this call's is still there, so
// the two identities are certainly different, and a rename onto the path takes it over without
// depending on the freed inode being reused or not.
func TestToolsReview836KeepsAnotherCallsDirectoryAtARecreatedPath(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "a")
	target := filepath.Join(parent, "b")
	other := filepath.Join(base, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}

	created, err := createRoot(target)
	if err != nil {
		t.Fatalf("createRoot under a fresh root: %v", err)
	}
	// Another install takes the path over: it removes the empty directory this call made and puts
	// its own in its place. The other directory was made while this call's was still there, so the
	// two identities are certainly different however the filesystem reuses freed inodes.
	if err := os.Remove(target); err != nil {
		t.Fatalf("the other install could not empty the path: %v", err)
	}
	if err := os.Rename(other, target); err != nil {
		t.Fatalf("the other install could not take the path over: %v", err)
	}

	removeCreated(created)
	info, statErr := os.Stat(target)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("this call removed the directory another install made at %s: %v", target, statErr)
	}
}

// C1, drop side: a path another install takes over between this call's scan and its mkdir answers
// EEXIST with a different identity, so the record must drop it rather than keep it as this call's.
// The directory another install made stays; the parent this call made stays too, because it now
// holds that directory.
func TestToolsReview836DropsAPathAnotherInstallTookOver(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "a")
	target := filepath.Join(parent, "b")
	other := filepath.Join(base, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}

	taken := false
	review836Seam(t, func(path string) {
		if path != target || taken {
			return
		}
		taken = true
		if err := os.Rename(other, target); err != nil {
			t.Errorf("the seam could not take the path over: %v", err)
		}
	})

	created, err := createRoot(target)
	if err != nil {
		t.Fatalf("createRoot with a path taken over by another install: %v", err)
	}
	removeCreated(created)
	info, statErr := os.Stat(target)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("this call removed the directory another install made at %s: %v", target, statErr)
	}
}
