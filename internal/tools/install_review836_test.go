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

// review836RetargetHost builds the shape a retargeted parent needs: the root is spelled through a
// symbolic link and a "..", so "link/.." is the parent of the directory the link names. The two
// candidates are in different parents, so moving the link moves the ".." with it -- which a pair of
// sibling directories would not do, because ".." would name their common parent either way.
func review836RetargetHost(t *testing.T) (first, second, link, root string) {
	t.Helper()
	base := t.TempDir()
	first = filepath.Join(base, "first")
	second = filepath.Join(base, "second")
	for _, dir := range []string{first, second} {
		if err := os.MkdirAll(filepath.Join(dir, "inner"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link = filepath.Join(base, "link")
	if err := os.Symlink(filepath.Join(first, "inner"), link); err != nil {
		t.Fatal(err)
	}
	// "link/.." is <base>/first while the link points into <base>/first/inner, and <base>/second
	// after it is retargeted, so the same spelling names a different parent.
	root = link + "/../p/q"
	return first, second, link, root
}

// review836RecordFor answers the record createRoot holds for the given spelled component, or false
// when it holds none.
func review836RecordFor(created []createRootRecord, path string) (createRootRecord, bool) {
	for _, made := range created {
		if made.path == path {
			return made, true
		}
	}
	return createRootRecord{}, false
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
	if _, held := review836RecordFor(created, spelled); held {
		t.Fatalf("createRoot kept the path another install took over in the record: %v", created)
	}
	removeCreated(created)
	info, statErr := os.Stat(target)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("this call removed the directory another install made at %s: %v", target, statErr)
	}
}

// C1, C3 across a retargeted parent: the same spelled path below a ".." names one directory before
// the parent is retargeted and a different one after, and this call made both. A record keyed by the
// spelling alone would keep only the second, so the first -- still standing under the parent this
// call made it through -- would survive the cleanup. Both must be removed.
func TestToolsReview836RemovesBothDirectoriesOfARetargetedDotDot(t *testing.T) {
	first, second, link, root := review836RetargetHost(t)

	retargeted := false
	review836Seam(t, func(path string) {
		if path != root || retargeted {
			return
		}
		retargeted = true
		// The directory below the first parent has been made by now; moving the link moves the ".."
		// for the rest of the walk, so the target is made under the second parent as well.
		if err := os.Remove(link); err != nil {
			t.Errorf("the seam could not remove the link: %v", err)
		}
		if err := os.Symlink(filepath.Join(second, "inner"), link); err != nil {
			t.Errorf("the seam could not retarget the link: %v", err)
		}
	})

	created, err := createRoot(root)
	if err != nil {
		t.Fatalf("createRoot across a retargeted parent: %v", err)
	}
	removeCreated(created)
	for _, path := range []string{
		filepath.Join(first, "p"),
		filepath.Join(second, "p"),
		filepath.Join(second, "p", "q"),
	} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("%s survived the cleanup of %v: %v", path, created, statErr)
		}
	}
	// The link and the directories it points into were there before the call and are never recorded.
	if _, statErr := os.Lstat(link); statErr != nil {
		t.Errorf("the pre-existing link was removed: %v", statErr)
	}
	for _, path := range []string{filepath.Join(first, "inner"), filepath.Join(second, "inner")} {
		if info, statErr := os.Stat(path); statErr != nil || !info.IsDir() {
			t.Errorf("the pre-existing directory %s was disturbed: %v", path, statErr)
		}
	}
}

// C2 through the real walk: a parent repointed before the target's mkdir must not make the cleanup
// disturb a directory that was already there under the parent the walk started with. The
// pre-existing directory was made while this call's was not there at all, so the two identities
// differ and only the identity comparison keeps it.
func TestToolsReview836RepointedParentLeavesPreExistingDirectoriesAlone(t *testing.T) {
	first, second, link, root := review836RetargetHost(t)
	// A directory another install left under the first parent, at the very path this call's target
	// would have taken there.
	peer := filepath.Join(first, "p")
	if err := os.Mkdir(peer, 0o755); err != nil {
		t.Fatal(err)
	}

	repointed := false
	review836Seam(t, func(path string) {
		// The target's parent is resolved after this seam runs, so the link is moved before the walk
		// resolves the parent the target will be made under.
		if path != root || repointed {
			return
		}
		repointed = true
		if err := os.Remove(link); err != nil {
			t.Errorf("the seam could not remove the link: %v", err)
		}
		if err := os.Symlink(filepath.Join(second, "inner"), link); err != nil {
			t.Errorf("the seam could not repoint the link: %v", err)
		}
	})

	created, err := createRoot(root)
	if err != nil {
		t.Fatalf("createRoot across a repointed parent: %v", err)
	}
	removeCreated(created)
	// The other install's directory is not this call's to remove.
	info, statErr := os.Stat(peer)
	if statErr != nil || !info.IsDir() {
		t.Errorf("this call removed the directory another install made at %s: %v", peer, statErr)
	}
	// The directory this call made under the parent it actually used is removed.
	if _, statErr := os.Lstat(filepath.Join(second, "p")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the directory this call made survived the cleanup: %v", statErr)
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
	created := []createRootRecord{{path: target, info: info}}

	removeCreated(created)
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("removeCreated removed a regular file whose identity matched the record: %v", err)
	}
}

// C2: the caller's spelling is resolved afresh by the kernel on every use, so it still reaches the
// directory this call made after the physical parent has been renamed. A record that only kept the
// parent location read at mkdir time would reopen a name that no longer exists and abandon a
// directory this call made; dev removed it through the spelling.
func TestToolsReview836RemovesThroughTheSpellingWhenAParentMoves(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(filepath.Join(real, "inner"), link); err != nil {
		t.Fatal(err)
	}
	// "link/.." resolves to <base>/real, so the directory this call makes is <base>/real/p.
	root := link + "/../p/q"
	moved := filepath.Join(base, "moved")

	movedOnce := false
	review836Seam(t, func(path string) {
		if path != root || movedOnce {
			return
		}
		movedOnce = true
		// The physical parent is renamed and the link is pointed at its new place, so the spelling
		// still reaches the same directory while the parent location read earlier no longer does.
		if err := os.Rename(real, moved); err != nil {
			t.Errorf("the seam could not rename the parent: %v", err)
		}
		if err := os.Remove(link); err != nil {
			t.Errorf("the seam could not remove the link: %v", err)
		}
		if err := os.Symlink(filepath.Join(moved, "inner"), link); err != nil {
			t.Errorf("the seam could not repoint the link: %v", err)
		}
	})

	created, err := createRoot(root)
	if err != nil {
		t.Fatalf("createRoot across a moved parent: %v", err)
	}
	removeCreated(created)
	// The directory this call made under the moved parent is gone, reached by the spelling.
	for _, path := range []string{filepath.Join(moved, "p"), filepath.Join(moved, "p", "q")} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("%s survived the cleanup of %v: %v", path, created, statErr)
		}
	}
	// The pre-existing directories are never this call's to remove. real was renamed to moved by the
	// seam, so only the moved tree is expected to remain.
	for _, path := range []string{moved, filepath.Join(moved, "inner")} {
		if info, statErr := os.Stat(path); statErr != nil || !info.IsDir() {
			t.Errorf("the pre-existing directory %s was disturbed: %v", path, statErr)
		}
	}
}

// C5: making the root must not need more of the parent than making a directory in it does. A parent
// that is writable and searchable but not readable can be used with os.Mkdir, so the walk must keep
// working there; requiring a readable parent would turn a valid configuration into a host failure.
func TestToolsReview836CreatesUnderAParentWithoutReadPermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not apply to root")
	}
	base := t.TempDir()
	drop := filepath.Join(base, "drop")
	if err := os.Mkdir(drop, 0o300); err != nil {
		t.Fatal(err)
	}
	// The temporary tree is removed after the test, which needs the mode back.
	t.Cleanup(func() { _ = os.Chmod(drop, 0o700) })
	target := filepath.Join(drop, "downloads")

	created, err := createRoot(target)
	if err != nil {
		t.Fatalf("createRoot under a write and search only parent: %v", err)
	}
	if len(created) == 0 {
		t.Fatalf("createRoot recorded nothing for %s", target)
	}
	// The directory was really made, so the mode did not silently stand in for it.
	if info, statErr := os.Stat(target); statErr != nil || !info.IsDir() {
		t.Fatalf("the root was not made under the restricted parent: %v", statErr)
	}
	removeCreated(created)
	if _, statErr := os.Lstat(target); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("%s survived the cleanup of %v: %v", target, created, statErr)
	}
}

// C1, the location a recorded component is reached by: the record must reach the directory through
// the parent the mkdir ran under, so the cleanup still finds it after a component before a ".." has
// vanished. The parent is taken as text and resolved by the kernel, so the record keeps the ".."
// instead of the different directory filepath.Clean would name.
func TestToolsReview836RecordsALocationThatSurvivesAVanishedComponent(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	inner := filepath.Join(real, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	// The link points one level down, so "link/.." is <base>/real and not <base>: the kernel
	// resolves the root to <base>/real/p, while a lexical clean of "link/../p" would name <base>/p.
	if err := os.Symlink(inner, link); err != nil {
		t.Fatal(err)
	}
	root := link + "/../p/q"
	target := filepath.Join(real, "p")

	created, err := createRoot(root)
	if err != nil {
		t.Fatalf("createRoot under a link and a dot-dot: %v", err)
	}
	made, found := review836RecordFor(created, link+"/../p")
	if !found {
		t.Fatalf("createRoot recorded %v, want the dot-dot component", created)
	}
	if made.parent != real || made.name != "p" {
		t.Fatalf("the record reaches %q by %q, want %q by \"p\"", made.parent, made.name, real)
	}
	removeCreated(created)
	// The link is not this call's: it was there before the call and is never recorded.
	for _, path := range []string{target, filepath.Join(target, "q")} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("%s survived the cleanup of %v: %v", path, created, statErr)
		}
	}
	// A clean of the spelling would have named <base>/p, which this call never made.
	if _, statErr := os.Lstat(filepath.Join(base, "p")); statErr == nil {
		t.Errorf("the root was resolved by a lexical clean to %s", filepath.Join(base, "p"))
	}
	if _, statErr := os.Lstat(link); statErr != nil {
		t.Errorf("the pre-existing link was removed: %v", statErr)
	}
}

// C3, the parent-handle side: the walk resolves a parent location and opens it, and the parent is then
// renamed away while a new directory takes the old name before the entry is made. The mkdir then fills
// the moved directory, which neither recorded name reaches, so the entry must not be left behind: it is
// removed through the very handle it was made with, and the walk recomputes instead of keeping a record
// that names a directory it cannot reach.
func TestToolsReview836RemovesWhatItMadeUnderAMovedParent(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "p")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(base, "moved")
	target := filepath.Join(parent, "q")

	movedOnce := false
	saved := createRootAfterParentOpened
	createRootAfterParentOpened = func(component string) {
		if component != target || movedOnce {
			return
		}
		movedOnce = true
		if err := os.Rename(parent, moved); err != nil {
			t.Errorf("the seam could not move the parent: %v", err)
		}
		if err := os.Mkdir(parent, 0o755); err != nil {
			t.Errorf("the seam could not put a new parent in its place: %v", err)
		}
	}
	t.Cleanup(func() { createRootAfterParentOpened = saved })

	created, err := createRoot(target)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("createRoot across a moved parent: %v", err)
	}
	removeCreated(created)
	// The entry the mkdir made in the moved directory is not this walk's to leave behind.
	if _, statErr := os.Lstat(filepath.Join(moved, "q")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the entry made under the moved parent survived: %v", statErr)
	}
}

// C3, the fallback side: a parent location that cannot be opened must still be recorded, because it is
// the second name the cleanup reaches the directory by when the spelling has stopped resolving. Dropping
// it abandons a directory this call made and cannot reach again.
func TestToolsReview836KeepsTheParentLocationWhenItCannotBeOpened(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not apply to root")
	}
	base := t.TempDir()
	drop := filepath.Join(base, "drop")
	if err := os.Mkdir(drop, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(drop, 0o700) })
	target := filepath.Join(drop, "downloads")

	created, err := createRoot(target)
	if err != nil {
		t.Fatalf("createRoot under a write and search only parent: %v", err)
	}
	made, found := review836RecordFor(created, target)
	if !found {
		t.Fatalf("createRoot recorded %v, want %s", created, target)
	}
	if made.parent != drop || made.name != "downloads" {
		t.Fatalf("the record kept parent %q and name %q, want %q and \"downloads\"", made.parent, made.name, drop)
	}
	removeCreated(created)
	if _, statErr := os.Lstat(target); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("%s survived the cleanup of %v: %v", target, created, statErr)
	}
}

// C2, d1: a recovery that finds the entry it made replaced by a regular file must leave that file alone.
// The seam replaces the directory with a file holding data, the way a peer does between the mkdir and the
// identity read; the cleanup must neither remove the file nor report success for an entry it does not hold.
func TestToolsReview836RecoveryNeverRemovesAFile(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "a")
	target := filepath.Join(parent, "b")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}

	replaced := false
	saved := createRootAfterMkdir
	createRootAfterMkdir = func(component string) {
		if component != target || replaced {
			return
		}
		replaced = true
		if err := os.Remove(target); err != nil {
			t.Errorf("the seam could not empty the made directory: %v", err)
		}
		if err := os.WriteFile(target, []byte("keep\n"), 0o644); err != nil {
			t.Errorf("the seam could not put a file in its place: %v", err)
		}
	}
	t.Cleanup(func() { createRootAfterMkdir = saved })

	created, err := createRoot(target)
	if err != nil {
		t.Logf("createRoot answered %v", err)
	}
	removeCreated(created)
	raw, readErr := os.ReadFile(target)
	if readErr != nil || string(raw) != "keep\n" {
		t.Fatalf("the file a peer put at %s was removed or changed: %q %v", target, raw, readErr)
	}
}

// C2, d2: a directory this call recorded keeps its inode while the record stands, so a directory another
// install makes at the same name after the recorded one is removed can never be taken for this call's.
// The recorded directory is removed behind the record's back, then a new one is made at its name.
func TestToolsReview836HeldDirectoryIsNotReusedWhileRecorded(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "a")
	target := filepath.Join(parent, "b")

	created, err := createRoot(target)
	if err != nil {
		t.Fatalf("createRoot under a fresh root: %v", err)
	}
	made, found := review836RecordFor(created, target)
	if !found {
		t.Fatalf("createRoot recorded %v, want %s", created, target)
	}
	if err := os.Remove(target); err != nil {
		t.Fatalf("the recorded directory could not be removed behind the record: %v", err)
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("another install could not make a directory at the path: %v", err)
	}
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(made.info, info) {
		t.Fatalf("the directory another install made at %s has the identity this call recorded", target)
	}
	removeCreated(created)
	if _, statErr := os.Lstat(target); statErr != nil {
		t.Fatalf("this call removed the directory another install made at %s: %v", target, statErr)
	}
}

// C3, d3: a link that is retargeted after the parent was opened and before the entry is made must not
// leave this call's directory in the parent the link used to name. The entry is removed and the walk
// recomputes under the parent the spelling names now, so the success is at the requested location.
func TestToolsReview836RetargetAfterParentOpenedMakesItUnderTheSpelling(t *testing.T) {
	first, second, link, root := review836RetargetHost(t)
	component := link + "/../p"

	retargeted := false
	saved := createRootAfterParentOpened
	createRootAfterParentOpened = func(path string) {
		if path != component || retargeted {
			return
		}
		retargeted = true
		if err := os.Remove(link); err != nil {
			t.Errorf("the seam could not remove the link: %v", err)
		}
		if err := os.Symlink(filepath.Join(second, "inner"), link); err != nil {
			t.Errorf("the seam could not retarget the link: %v", err)
		}
	}
	t.Cleanup(func() { createRootAfterParentOpened = saved })

	created, err := createRoot(root)
	if err != nil {
		t.Fatalf("createRoot across a retargeted parent: %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(first, "p")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("the directory made under the old parent was left behind: %v", statErr)
	}
	if info, statErr := os.Lstat(filepath.Join(second, "p")); statErr != nil || !info.IsDir() {
		t.Fatalf("the directory was not made where the spelling now names: %v", statErr)
	}
	removeCreated(created)
	for _, path := range []string{filepath.Join(second, "p"), filepath.Join(second, "p", "q")} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("%s survived the cleanup of %v: %v", path, created, statErr)
		}
	}
}
