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

// The scan reports the components that are missing, and createRoot decides from a read taken at the
// moment of the decision rather than from the scan's own list: the list is a moment of its own, and
// a component above a ".." can come back between that moment and the walk. This test pins the scan's
// contract (the components it found missing, outermost first, and nothing for a root that exists).
func TestToolsReview836ScanReportsTheAbsenceItObserved(t *testing.T) {
	base := t.TempDir()
	existing := filepath.Join(base, "existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(existing, "a", "b")

	missing := rootComponents(target)
	want := []string{filepath.Join(existing, "a"), target}
	if strings.Join(missing, ",") != strings.Join(want, ",") {
		t.Fatalf("the scan found %v, want %v", missing, want)
	}
	// A root that already exists has nothing missing.
	if missing := rootComponents(existing); len(missing) != 0 {
		t.Fatalf("the scan of an existing root answered %v", missing)
	}
}

// C1, drop side on a path this call already recorded: after the call recorded a directory, another
// install takes the path over with a directory of its own. The call's next walk reaches that path
// again -- the component before the ".." vanished and the path was recomputed as missing -- and its
// mkdir answers EEXIST with a different identity, so the path must leave the record and the other
// install's directory must survive the cleanup. The replacement is empty, so what keeps it is the
// identity comparison and not os.Remove refusing a non-empty directory.
func TestToolsReview836DropsARecordedPathAnotherInstallTookOver(t *testing.T) {
	component, root, target := review836RaceHost(t)
	spelled := component + "/../p"
	// The other install's directory is made while this call's is still there, so the two identities
	// are certainly different however the filesystem reuses freed inodes; it is then renamed into
	// the recorded path, which takes the path over without depending on that reuse.
	peer := filepath.Join(filepath.Dir(component), "peer")
	if err := os.Mkdir(peer, 0o755); err != nil {
		t.Fatal(err)
	}

	phase := 0
	review836Seam(t, func(path string) {
		switch {
		case path == root && phase == 0:
			// Round 0: the concurrent install takes the component before the ".." back, so this
			// call's mkdir of the target answers not-exist and the walk recomputes.
			phase = 1
			if err := os.Remove(component); err != nil {
				t.Errorf("the seam could not remove the ancestor: %v", err)
			}
		case path == spelled && phase == 1:
			// Round 1: the target is recomputed as missing, but the path is one this call already
			// recorded. The concurrent install replaces it with a directory of its own, so this
			// call's mkdir answers EEXIST and the identity there is not the recorded one.
			phase = 2
			if err := os.Remove(target); err != nil {
				t.Errorf("the seam could not empty the recorded path: %v", err)
			}
			if err := os.Rename(peer, target); err != nil {
				t.Errorf("the seam could not take the recorded path over: %v", err)
			}
		}
	})

	created, err := createRoot(root)
	if err != nil {
		t.Fatalf("createRoot with a recorded path taken over: %v", err)
	}
	for _, made := range created {
		if made.path == spelled {
			t.Fatalf("createRoot kept the path another install took over in the record: %v", created)
		}
	}
	removeCreated(created)
	info, statErr := os.Stat(target)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("this call removed the directory another install made at %s: %v", target, statErr)
	}
}

// The parent's location is read before the mkdir, so it can be stale: the parent can be repointed
// between that read and the mkdir. A location that then names a different object than the mkdir
// acted on is not this call's directory and must not be recorded, because the removal would delete
// an object this call never made. The object the mkdir acted on is the one at the spelled path, so
// that is what the read is checked against.
func TestToolsReview836IdentityRefusesALocationThatNamesAnotherObject(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "first")
	second := filepath.Join(base, "second")
	for _, dir := range []string{first, second} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(first, link); err != nil {
		t.Fatal(err)
	}
	// The peer's directory stands at the location the stale parent names.
	peer := filepath.Join(first, "q")
	if err := os.Mkdir(peer, 0o755); err != nil {
		t.Fatal(err)
	}
	// The parent is repointed, and the mkdir then makes its directory through the new target.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}
	made := filepath.Join(second, "q")
	if err := os.Mkdir(made, 0o755); err != nil {
		t.Fatal(err)
	}

	// first is the parent read before the mkdir; the mkdir then ran through the repointed link.
	location, info, err := componentIdentity(filepath.Join(link, "q"), first)
	if err != nil {
		t.Fatalf("componentIdentity under a repointed parent: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("componentIdentity answered a %v", info.Mode().Type())
	}
	madeInfo, statErr := os.Lstat(made)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if !os.SameFile(madeInfo, info) {
		t.Fatalf("componentIdentity recorded %s, which is not the directory the mkdir made (%s)", location, made)
	}
	peerInfo, statErr := os.Lstat(peer)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if os.SameFile(peerInfo, info) {
		t.Fatalf("componentIdentity recorded the peer's directory at the stale location %s", peer)
	}
}

// C2, not-a-directory side: only a directory is ever this call's to remove. The record here names
// the regular file standing at the path and claims that file's own identity, so nothing but the
// directory check keeps the removal from deleting it: a record whose identity matches is not by
// itself a licence to remove whatever is there.
func TestToolsReview836RemoveCreatedLeavesANonDirectoryAlone(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "a", "b")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() {
		t.Fatal("the test did not place a regular file at the recorded path")
	}
	created := []createRootRecord{{path: target, resolved: target, info: info}}

	removeCreated(created)
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("removeCreated removed a regular file whose identity matched the record: %v", err)
	}
}
