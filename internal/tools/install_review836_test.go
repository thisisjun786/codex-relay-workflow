package tools

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// review836RaceHost builds the shape the defect needs: the root is spelled <base>/x/../p/q and the
// component before the ".." does not exist yet, so createRoot makes that component itself. A
// concurrent install can then remove the component this call made while the directory this call
// made below it is still there. The scan then reports that directory missing although it exists,
// which is the state in which a mkdir of it answers EEXIST.
func review836RaceHost(t *testing.T) (component, root, target string) {
	t.Helper()
	base := t.TempDir()
	component = filepath.Join(base, "x")
	// The spelling is kept as the caller wrote it: filepath.Join would clean the ".." away, and
	// the race is exactly about a component before that "..".
	root = component + "/../p/q"
	target = filepath.Join(base, "p")
	return component, root, target
}

// C1, C3: the component before the ".." is removed by a concurrent install while the directory
// this call made below it is still there. The scan then reports that directory missing, x is made
// again and the mkdir of the directory this call already made answers EEXIST. It is still this
// call's directory, so the record must keep it and the cleanup must leave neither x nor that
// directory behind.
func TestToolsReview836KeepsItsOwnDirectoryWhenAnAncestorVanishes(t *testing.T) {
	component, root, target := review836RaceHost(t)

	removed := false
	review836Seam(t, func(path string) {
		if path != root || removed {
			return
		}
		removed = true
		if err := os.Remove(component); err != nil {
			t.Errorf("the seam could not remove the ancestor: %v", err)
		}
	})

	created, err := createRoot(root)
	if err != nil {
		t.Fatalf("createRoot with an ancestor removed between its scan and its mkdir: %v", err)
	}
	removeCreated(created)
	for _, path := range []string{component, target} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("%s survived the cleanup of %v: %v", path, created, statErr)
		}
	}
}

// C3: the same race, but the ancestor keeps vanishing, so the recompute budget runs out. createRoot
// answers the not-exist error it gave up on and removes what it made; nothing it created is left
// under the root for the next run to trip over.
func TestToolsReview836GivesUpWithoutLeavingItsDirectories(t *testing.T) {
	component, root, target := review836RaceHost(t)

	attempts := 0
	review836Seam(t, func(path string) {
		if path != root {
			return
		}
		attempts++
		if err := os.Remove(component); err != nil && !errors.Is(err, fs.ErrNotExist) {
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
	for _, path := range []string{component, target} {
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

// The scan answers the error it stopped on together with the components it found missing, because
// that observation is the evidence createRoot uses to decide that a recorded path is gone. A second
// read of the path cannot replace it: another install can make the path again in between, and the
// fresh read would then report the peer's directory as present and leave a stale claim on it.
func TestToolsReview836ScanReportsTheAbsenceItObserved(t *testing.T) {
	base := t.TempDir()
	existing := filepath.Join(base, "existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(existing, "a", "b")

	missing, err := rootComponents(target)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the scan answered %v, want the not-exist error it stopped on", err)
	}
	want := []string{filepath.Join(existing, "a"), target}
	if strings.Join(missing, ",") != strings.Join(want, ",") {
		t.Fatalf("the scan found %v, want %v", missing, want)
	}
	// A root that already exists has nothing missing and no absence to report.
	missing, err = rootComponents(existing)
	if err != nil || len(missing) != 0 {
		t.Fatalf("the scan of an existing root answered %v, %v", missing, err)
	}
}
