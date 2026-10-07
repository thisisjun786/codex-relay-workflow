package migrate

// apply_owned_dir_identity_test.go holds the CRW-887 cases. CRW-813 decided that a destination
// directory was this run's from the result of this run's own mkdir, but that mkdir still happened at the
// directory's final name, the private marker mode was still given by the caller after a fallible open and
// parent sync, and finishModes still adopted a foreign directory that happened to carry exactly the marker
// mode. These cases pin the creation under a temporary name of this run, the identity of the directory
// that creation made, the mode given through a handle that needs no permission on it and no name that can
// be redirected, the record that keeps finishModes from adopting a directory this run's own creation was
// refused by, and the cleanup that removes only this run's own directory. Each case names the behaviour
// it pins.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// migrateOwnedDirIdentitySessionBody is the record the cases copy; it is the JSON object a session file
// holds, written as a raw string so its quotes stay the bytes a plan classifies.
const migrateOwnedDirIdentitySessionBody = `{"phase":"P"}`

// migrateOwnedDirIdentityEntries is the source tree every case copies: one file inside one directory.
func migrateOwnedDirIdentityEntries() map[string]string {
	return map[string]string{"sessions/a.json": migrateOwnedDirIdentitySessionBody}
}

// migrateOwnedDirIdentityDarwinMkdir models the host whose mkdir keeps the permission bits of a directory
// creation mode and drops the sticky bit, which is what Darwin does and the reason the mode is given
// through the handle before the name is published. The returned function restores the real mkdir, so a
// case can run its rerun against the host's own mkdir. The seam is process-wide; no case here runs in
// parallel.
func migrateOwnedDirIdentityDarwinMkdir(t *testing.T) func() {
	t.Helper()
	real := migrateOwnedDirMkdirat
	migrateOwnedDirMkdirat = func(dirfd int, name string, mode uint32) error {
		return real(dirfd, name, mode&^uint32(unix.S_ISVTX))
	}
	t.Cleanup(func() { migrateOwnedDirMkdirat = real })
	return func() { migrateOwnedDirMkdirat = real }
}

// migrateOwnedDirIdentitySteps installs the creation-step seam and returns the function that removes it,
// so a case can run its rerun without it. The seam is process-wide; no case here runs in parallel.
func migrateOwnedDirIdentitySteps(t *testing.T, at func(step string) error) func() {
	t.Helper()
	migrateOwnedDirIdentityAt = at
	t.Cleanup(func() { migrateOwnedDirIdentityAt = nil })
	return func() { migrateOwnedDirIdentityAt = nil }
}

// migrateOwnedDirIdentityAfterFirstRead installs a seam that swaps the name the creation just read, once,
// right after that first identity read returns. This is the window the review names: the entry at the
// temporary name is observable in its parent, so another same-uid writer can put a regular file or a hard
// link there after the read and before the mode step. It returns the function that removes the seam.
func migrateOwnedDirIdentityAfterFirstRead(t *testing.T, ws string, swap func(target string)) func() {
	t.Helper()
	real := migrateOwnedDirIdentityLstat
	done := false
	migrateOwnedDirIdentityLstat = func(dirfd int, name string, st *unix.Stat_t) error {
		err := real(dirfd, name, st)
		if err == nil && !done {
			done = true
			swap(filepath.Join(ws, name))
		}
		return err
	}
	t.Cleanup(func() { migrateOwnedDirIdentityLstat = real })
	return func() { migrateOwnedDirIdentityLstat = real }
}

// migrateOwnedDirIdentityUnderUmask runs fn with the process umask set to um and restores it before
// returning, so no other case of this package sees it.
func migrateOwnedDirIdentityUnderUmask(t *testing.T, um int, fn func()) {
	t.Helper()
	old := unix.Umask(um)
	defer unix.Umask(old)
	fn()
}

// migrateOwnedDirIdentitySetRaw gives a directory a raw mode, the sticky bit included, which is how the
// migration itself gives a directory its mode.
func migrateOwnedDirIdentitySetRaw(t *testing.T, path string, raw uint32) {
	t.Helper()
	must(t, unix.Chmod(path, raw))
}

// migrateOwnedDirIdentityWantRaw fails unless the directory carries exactly the raw mode, read the way
// finishModes reads it.
func migrateOwnedDirIdentityWantRaw(t *testing.T, path string, want uint32) {
	t.Helper()
	if got := migrateOwnedDirRaw(t, path); got != want {
		t.Errorf("%s is %#o; want %#o", path, got, want)
	}
}

// migrateOwnedDirIdentityWantKeptNote fails unless the plan item is reported as a directory whose mode
// was kept.
func migrateOwnedDirIdentityWantKeptNote(t *testing.T, res *ApplyResult, source string) {
	t.Helper()
	if ai := apItem(t, res, source); !strings.Contains(ai.Note, "kept its mode") {
		t.Errorf("%s must be reported as a directory that kept its mode, note = %q", source, ai.Note)
	}
}

// migrateOwnedDirIdentityWantNoTemp fails when a temporary of the package's own naming rule is left in
// dir, which is what OlderTemps would report as another run's leftover.
func migrateOwnedDirIdentityWantNoTemp(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	must(t, err)
	for _, e := range entries {
		if _, ok := tempRun(e.Name()); ok {
			t.Errorf("a temporary directory was left behind: %s", filepath.Join(dir, e.Name()))
		}
	}
}

// migrateOwnedDirIdentityRerun opens the same project roots again and applies them: a rerun is a new
// process with its own pinned roots and its own publisher.
func migrateOwnedDirIdentityRerun(t *testing.T, ws string) (*ApplyResult, error) {
	t.Helper()
	r, err := Open(Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	t.Cleanup(func() { _ = r.Close() })
	p, err := classify(r)
	must(t, err)
	return apply(r, p)
}

// migrateOwnedDirIdentityUserRerun is the same for the user scope.
func migrateOwnedDirIdentityUserRerun(t *testing.T, u, v string) (*ApplyResult, error) {
	t.Helper()
	r, err := Open(Options{Scope: ScopeUser, FromHome: u, ToHome: v})
	must(t, err)
	t.Cleanup(func() { _ = r.Close() })
	p, err := classify(r)
	must(t, err)
	return apply(r, p)
}

// C1a: a racer that creates the destination directory at exactly the private marker mode in the window
// between the lookup that found it absent and this run's own creation is still another actor's
// directory, so the run must not adopt it through the marker branch: it keeps 01700 and is reported.
func TestMigrateOwnedDirIdentityRacerMarkerModeIsNotAdopted(t *testing.T) {
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	seam := migrateOwnedDirBeforeEnsureChild
	t.Cleanup(func() { migrateOwnedDirBeforeEnsureChild = seam })
	migrateOwnedDirBeforeEnsureChild = func() {
		migrateOwnedDirBeforeEnsureChild = nil
		dir := apDst(ws, "sessions")
		mkdirs(t, dir)
		migrateOwnedDirIdentitySetRaw(t, dir, applyTempRaw)
	}
	res, err := apply(r, p)
	must(t, err)
	migrateOwnedDirIdentityWantRaw(t, apDst(ws, "sessions"), applyTempRaw)
	migrateOwnedDirIdentityWantKeptNote(t, res, "sessions")
	if got := get(t, apDst(ws, "sessions/a.json")); got != migrateOwnedDirIdentitySessionBody {
		t.Errorf("the copy must still complete: %q", got)
	}
}

// C1a: the same through the destination root. A racer's .crw at the marker mode is another actor's
// directory, so the run neither adopts it nor finishes it at the source mode.
func TestMigrateOwnedDirIdentityRacerRootMarkerModeIsNotAdopted(t *testing.T) {
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	// The racer makes the root after Open pinned the pair and before the run creates it, which is the
	// window the issue names for a root; the child-directory case above uses the package's own seam.
	dir := apDst(ws, "")
	mkdirs(t, dir)
	migrateOwnedDirIdentitySetRaw(t, dir, applyTempRaw)
	res, err := apply(r, p)
	must(t, err)
	migrateOwnedDirIdentityWantRaw(t, apDst(ws, ""), applyTempRaw)
	migrateOwnedDirIdentityWantKeptNote(t, res, ".")
}

// C1b: a racer that creates the final name between this run's temporary creation and the rename leaves
// made false, leaves no temporary behind, and keeps the mode the racer gave the directory.
func TestMigrateOwnedDirIdentityRacerAtTheRenameIsNotAdopted(t *testing.T) {
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	renames := 0
	migrateOwnedDirIdentitySteps(t, func(step string) error {
		if step == "rename" {
			if renames++; renames == 2 { // the project root's creation first, then the child's
				dir := apDst(ws, "sessions")
				mkdirs(t, dir)
				migrateOwnedDirIdentitySetRaw(t, dir, 0o750)
			}
		}
		return nil
	})
	res, err := apply(r, p)
	must(t, err)
	if renames != 2 {
		t.Fatalf("the child's rename step ran %d times, want 2", renames)
	}
	migrateOwnedDirIdentityWantRaw(t, apDst(ws, "sessions"), 0o750)
	migrateOwnedDirIdentityWantKeptNote(t, res, "sessions")
	migrateOwnedDirIdentityWantNoTemp(t, apDst(ws, ""))
	if got := get(t, apDst(ws, "sessions/a.json")); got != migrateOwnedDirIdentitySessionBody {
		t.Errorf("the copy must still complete: %q", got)
	}
}

// C2(2): a regular file put at the temporary name after the creation and before the mode step is
// refused with no mode changed. The mode goes through a handle on the directory this run created, and
// the handle cannot be opened on a regular file, so nothing chmods it. The head before this cycle
// chmodded the name before any type check.
func TestMigrateOwnedDirIdentityRegularFileAtTheTemporaryIsNotChmodded(t *testing.T) {
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	target := ""
	migrateOwnedDirIdentityAfterFirstRead(t, ws, func(name string) {
		target = name
		must(t, os.Rename(name, name+".moved"))
		must(t, os.WriteFile(name, []byte("not a directory"), 0o644))
	})
	if _, err := apply(r, p); err == nil {
		t.Fatal("a regular file at the temporary name must stop the run")
	}
	if target == "" {
		t.Fatal("the case never swapped the temporary")
	}
	fi, err := os.Stat(target)
	must(t, err)
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("a regular file at the temporary name is %v; the run must not chmod it", fi.Mode().Perm())
	}
}

// C2(2): the same for a hard link to another file. AT_SYMLINK_NOFOLLOW stops a link being followed, not
// a hard link being put at the name, so the identity check on the opened handle is what refuses it
// before any mode is given.
func TestMigrateOwnedDirIdentityHardLinkAtTheTemporaryIsNotChmodded(t *testing.T) {
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	other := filepath.Join(ws, "other-file")
	must(t, os.WriteFile(other, []byte("x"), 0o640))
	target := ""
	migrateOwnedDirIdentityAfterFirstRead(t, ws, func(name string) {
		target = name
		must(t, os.Remove(name))
		must(t, os.Link(other, name))
	})
	if _, err := apply(r, p); err == nil {
		t.Fatal("a hard link at the temporary name must stop the run")
	}
	if target == "" {
		t.Fatal("the case never swapped the temporary")
	}
	fi, err := os.Stat(other)
	must(t, err)
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("the hard link's inode is %v; the run must not chmod it", fi.Mode().Perm())
	}
}

// C2(3): when the identity of the name this run just created cannot be read, that directory is left in
// place and the refusal names it. Nothing is removed, so a directory another actor put at the same name
// can never be deleted. The head before this cycle treated a matching owner as proof and removed it.
func TestMigrateOwnedDirIdentityUnreadableTemporaryIsLeftAndReported(t *testing.T) {
	restore := migrateOwnedDirIdentityLstat
	t.Cleanup(func() { migrateOwnedDirIdentityLstat = restore })
	calls := 0
	var target string
	migrateOwnedDirIdentityLstat = func(dirfd int, name string, st *unix.Stat_t) error {
		if calls++; calls == 1 {
			target = name
			return unix.EIO
		}
		return restore(dirfd, name, st)
	}
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	_, err := apply(r, p)
	wantRefusal(t, err, ReasonUnreadable)
	if target == "" {
		t.Fatal("the case never reached the identity read of the temporary")
	}
	// The temporary this run made is still there, so an entry another actor could have put at that
	// name was never a removal target either, and the refusal names the leftover.
	if _, err := os.Lstat(filepath.Join(ws, target)); err != nil {
		t.Errorf("the unreadable temporary must be left in place: %v", err)
	}
	if !strings.Contains(err.Error(), target) {
		t.Errorf("the refusal must name the leftover %q, got %v", target, err)
	}
}

// C2(4): a kernel whose fchmodat2 is absent (Linux before 6.5 answers ENOSYS, which the runtime call
// reports as EOPNOTSUPP) still creates the directory, through the descriptor-bound /proc/self/fd chmod
// rather than a by-name chmod a swapped link could redirect. On this platform the pin always answers a
// handle, so the mode is always given through one; the case also pins that the fallback stays bound to
// the descriptor by swapping the name for a hard link right after the mode is set, which the reopen
// then refuses. The head before this cycle refused such a kernel as unsupported.
func TestMigrateOwnedDirIdentityFchmodat2AbsentUsesTheDescriptorPath(t *testing.T) {
	if !ownedDirIdentityHandleOK || !ownedDirIdentityFchmodUsesFchmodat2 {
		t.Skip("this platform has no descriptor-bound mode handle that consults fchmodat2")
	}
	restore2 := ownedDirIdentityFchmodat2
	t.Cleanup(func() { ownedDirIdentityFchmodat2 = restore2 })
	tries := 0
	ownedDirIdentityFchmodat2 = func(fd int, perm uint32) error {
		tries++
		return unix.EOPNOTSUPP
	}
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	if _, err := apply(r, p); err != nil {
		t.Fatalf("a kernel without fchmodat2 must still create the directory: %v", err)
	}
	if tries == 0 {
		t.Error("the creation never tried the descriptor-bound chmod")
	}
	migrateOwnedDirIdentityWantRaw(t, apDst(ws, ""), 0o755)
	migrateOwnedDirIdentityWantRaw(t, apDst(ws, "sessions"), 0o755)
	migrateOwnedDirIdentityWantNoTemp(t, apDst(ws, ""))
}

// C2(4): on a platform whose open cannot pin a directory without read permission, the creation checks
// the name immediately before the by-name no-follow chmod instead, and still ends at the requested mode
// with no temporary behind. The pin is forced to its no-handle answer, which is what that platform
// returns by itself.
func TestMigrateOwnedDirIdentityWithoutAHandleStillCreates(t *testing.T) {
	restore := migrateOwnedDirIdentityPin
	t.Cleanup(func() { migrateOwnedDirIdentityPin = restore })
	migrateOwnedDirIdentityPin = func(dirfd int, name string) (int, error) {
		return -1, ownedDirIdentityNoHandleErr{}
	}
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	if _, err := apply(r, p); err != nil {
		t.Fatalf("a platform without a mode handle must still create the directory: %v", err)
	}
	migrateOwnedDirIdentityWantRaw(t, apDst(ws, ""), 0o755)
	migrateOwnedDirIdentityWantRaw(t, apDst(ws, "sessions"), 0o755)
	migrateOwnedDirIdentityWantNoTemp(t, apDst(ws, ""))
}

// C1c: an interruption at the pin of this run's temporary must not leave a directory a rerun then fails
// to recognise. The mkdir seam models Darwin, so the directory would have been left without the marker
// had the mode not been given through the handle before the name is published.
func TestMigrateOwnedDirIdentityRerunAfterAFailedOpen(t *testing.T) {
	darwin := migrateOwnedDirIdentityDarwinMkdir(t)
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	opens := 0
	clearAt := migrateOwnedDirIdentitySteps(t, func(step string) error {
		if step == "open" {
			if opens++; opens == 2 { // the project root first, then the child
				return errApplyInterrupted
			}
		}
		return nil
	})
	if _, err := apply(r, p); !errors.Is(err, errApplyInterrupted) {
		t.Fatalf("the interrupted run: %v", err)
	}
	migrateOwnedDirIdentityWantNoTemp(t, apDst(ws, ""))
	// The directory the run made before the failure carries the marker, so the rerun recognises it.
	migrateOwnedDirIdentityWantRaw(t, apDst(ws, ""), applyTempRaw)
	darwin()
	clearAt()
	if _, err := migrateOwnedDirIdentityRerun(t, ws); err != nil {
		t.Fatal(err)
	}
	migrateOwnedDirIdentityWantRaw(t, apDst(ws, ""), 0o755)
	migrateOwnedDirIdentityWantRaw(t, apDst(ws, "sessions"), 0o755)
}

// C1c: the same for a failure at the parent sync right after the creation. The rename has already
// published the directory at its mode, so the rerun recognises it as this run's and finishes it.
func TestMigrateOwnedDirIdentityRerunAfterAFailedParentSync(t *testing.T) {
	darwin := migrateOwnedDirIdentityDarwinMkdir(t)
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	syncs := 0
	clearAt := migrateOwnedDirIdentitySteps(t, func(step string) error {
		if step == "sync" {
			if syncs++; syncs == 2 { // the project root first, then the child
				return errApplyInterrupted
			}
		}
		return nil
	})
	if _, err := apply(r, p); !errors.Is(err, errApplyInterrupted) {
		t.Fatalf("the interrupted run: %v", err)
	}
	migrateOwnedDirIdentityWantRaw(t, apDst(ws, "sessions"), applyTempRaw)
	darwin()
	clearAt()
	if _, err := migrateOwnedDirIdentityRerun(t, ws); err != nil {
		t.Fatal(err)
	}
	migrateOwnedDirIdentityWantRaw(t, apDst(ws, "sessions"), 0o755)
}

// C1d: a umask that removes owner read or write must neither stop the creation before its mode is given
// nor leave a directory without the marker. With 0477 and, separately, 0777 the project root, its child
// directory and the user root are created, end at the source mode, and a rerun finds each already at it.
func TestMigrateOwnedDirIdentityHostileUmask(t *testing.T) {
	for _, um := range []int{0o477, 0o777} {
		t.Run(fmt.Sprintf("umask %#o", um), func(t *testing.T) {
			ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
			var err error
			migrateOwnedDirIdentityUnderUmask(t, um, func() { _, err = apply(r, p) })
			must(t, err)
			migrateOwnedDirIdentityWantRaw(t, apDst(ws, ""), 0o755)
			migrateOwnedDirIdentityWantRaw(t, apDst(ws, "sessions"), 0o755)
			if _, err := migrateOwnedDirIdentityRerun(t, ws); err != nil {
				t.Fatal(err)
			}
			migrateOwnedDirIdentityWantRaw(t, apDst(ws, ""), 0o755)
			migrateOwnedDirIdentityWantRaw(t, apDst(ws, "sessions"), 0o755)

			u, v, ur, up := migrateOwnedDirUserPlan(t, map[string]string{"config.json": "{}"})
			migrateOwnedDirIdentityUnderUmask(t, um, func() { _, err = apply(ur, up) })
			must(t, err)
			migrateOwnedDirIdentityWantRaw(t, v, 0o755)
			if _, err := migrateOwnedDirIdentityUserRerun(t, u, v); err != nil {
				t.Fatal(err)
			}
			migrateOwnedDirIdentityWantRaw(t, v, 0o755)
		})
	}
}

// C1d: an interruption under a hostile umask leaves the directories carrying the marker mode, so the
// rerun recognises each as this run's and finishes it at the source mode.
func TestMigrateOwnedDirIdentityHostileUmaskAfterAnInterruption(t *testing.T) {
	for _, um := range []int{0o477, 0o777} {
		t.Run(fmt.Sprintf("umask %#o", um), func(t *testing.T) {
			ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
			pub := newPub(t)
			pub.at = apFailRename(2) // the canonical .gitignore, then the child's file
			var err error
			migrateOwnedDirIdentityUnderUmask(t, um, func() { _, err = applyWith(r, p, pub) })
			if !errors.Is(err, errApplyInterrupted) {
				t.Fatalf("the interrupted run: %v", err)
			}
			migrateOwnedDirIdentityWantRaw(t, apDst(ws, ""), applyTempRaw)
			migrateOwnedDirIdentityWantRaw(t, apDst(ws, "sessions"), applyTempRaw)
			if _, err := migrateOwnedDirIdentityRerun(t, ws); err != nil {
				t.Fatal(err)
			}
			migrateOwnedDirIdentityWantRaw(t, apDst(ws, ""), 0o755)
			migrateOwnedDirIdentityWantRaw(t, apDst(ws, "sessions"), 0o755)
		})
	}
}

// C1: a creation that reached its rename and then failed is this run's directory, so a retry with the
// same pinned pair finishes its mode instead of reading it as another actor's.
func TestMigrateOwnedDirIdentityRerunWithTheSamePinnedPairFinishesTheRoot(t *testing.T) {
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	syncs := 0
	clearAt := migrateOwnedDirIdentitySteps(t, func(step string) error {
		if step == "sync" {
			if syncs++; syncs == 1 { // the project root's creation, which this case stops
				return errApplyInterrupted
			}
		}
		return nil
	})
	if _, err := apply(r, p); !errors.Is(err, errApplyInterrupted) {
		t.Fatalf("the interrupted run: %v", err)
	}
	// The rename put the root at its name before the sync failed, so the root is this run's and keeps
	// the mode its own creation was asked for; the marker chmod that follows it never ran.
	migrateOwnedDirIdentityWantRaw(t, apDst(ws, ""), 0o700)
	clearAt()
	res, err := apply(r, p)
	must(t, err)
	migrateOwnedDirIdentityWantRaw(t, apDst(ws, ""), 0o755)
	migrateOwnedDirIdentityWantRaw(t, apDst(ws, "sessions"), 0o755)
	if ai := apItem(t, res, "."); strings.Contains(ai.Note, "kept its mode") {
		t.Errorf("the retry must finish the root this run made, note = %q", ai.Note)
	}
}

// C2(1): the same-Pair retry must not transfer ownership to a replacement root. The first parent sync
// fails, the root this run created is moved aside, and a different 0700 root is put at the same path.
// The retry must leave that root 0700 and report it as one whose mode was kept. The head before this
// cycle remembered only a flag, so the retry took the replacement for this run's and finished it at the
// source mode, 0755.
func TestMigrateOwnedDirIdentityReplacedRootIsNotAdoptedOnRetry(t *testing.T) {
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	syncs := 0
	clearAt := migrateOwnedDirIdentitySteps(t, func(step string) error {
		if step == "sync" {
			if syncs++; syncs == 1 { // the project root's creation, which this case stops
				return errApplyInterrupted
			}
		}
		return nil
	})
	if _, err := apply(r, p); !errors.Is(err, errApplyInterrupted) {
		t.Fatalf("the interrupted run: %v", err)
	}
	root := apDst(ws, "")
	// Another actor takes the name: this run's root is moved aside and a 0700 root is put there.
	must(t, os.Rename(root, root+".ours"))
	mkdirs(t, root)
	migrateOwnedDirIdentitySetRaw(t, root, 0o700)
	clearAt()
	res, err := apply(r, p)
	must(t, err)
	migrateOwnedDirIdentityWantRaw(t, root, 0o700)
	migrateOwnedDirIdentityWantKeptNote(t, res, ".")
}

// C2(3): a run interrupted while creating the destination root leaves its temporary in the directory that
// holds the root, and the next run reports it there. The classifier walks only the destination root and
// what is below it, so without this the leftover was invisible to the report even though OlderTemps names
// it. The head before this cycle reported nothing for it.
func TestMigrateOwnedDirIdentityRootTemporaryIsReportedOnTheNextRun(t *testing.T) {
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	restore := migrateOwnedDirIdentityLstat
	t.Cleanup(func() { migrateOwnedDirIdentityLstat = restore })
	calls := 0
	migrateOwnedDirIdentityLstat = func(dirfd int, name string, st *unix.Stat_t) error {
		if calls++; calls == 1 {
			return unix.EIO
		}
		return restore(dirfd, name, st)
	}
	if _, err := apply(r, p); err == nil {
		t.Fatal("the case needs the root creation to fail")
	}
	migrateOwnedDirIdentityLstat = restore
	// The next run is a new process: its own roots and its own plan.
	again, err := Open(Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	t.Cleanup(func() { _ = again.Close() })
	plan, err := classify(again)
	must(t, err)
	found := false
	for _, it := range plan.Items {
		if _, ok := tempRun(filepath.Base(it.Source)); ok && it.Disposition == DispSkip {
			found = true
		}
	}
	if !found {
		t.Errorf("the next run must report the root's leftover temporary in the plan, got %d items", len(plan.Items))
	}
}

// C2(1): a root this process created keeps its identity for the whole run, so a name that later holds a
// different directory is never taken for it. The handle the creation made is held until the roots close,
// which is what stops the kernel from freeing that inode and handing it to another directory.
func TestMigrateOwnedDirIdentityCreatedRootHandleIsHeld(t *testing.T) {
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	if _, err := apply(r, p); err != nil {
		t.Fatal(err)
	}
	if r.Project.createdDir == nil {
		t.Fatal("the run must hold the handle of the root its own creation made")
	}
	if r.Project.createdDir.id != r.Project.created {
		t.Errorf("the held handle is %v, want the recorded identity %v", r.Project.createdDir.id, r.Project.created)
	}
	// The name still holds that very directory, and the handle still pins it.
	if fi, err := os.Stat(apDst(ws, "")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("the root must be the directory this run created: %v %v", fi, err)
	}
}

// C2(1): the run holds the handle of the root its own creation made for the rest of the run, and the
// handle is really open on that directory - a closed descriptor would report an error, and the recorded
// identity would then be reusable by another directory.
func TestMigrateOwnedDirIdentityCreatedRootHandleIsOpen(t *testing.T) {
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	if _, err := apply(r, p); err != nil {
		t.Fatal(err)
	}
	held := r.Project.createdDir
	if held == nil {
		t.Fatal("the run must hold the handle of the root its own creation made")
	}
	if held.id != r.Project.created {
		t.Errorf("the held handle is %v, want the recorded identity %v", held.id, r.Project.created)
	}
	var st unix.Stat_t
	if err := unix.Fstat(held.fd(), &st); err != nil {
		t.Errorf("the held handle must still be open on the created directory: %v", err)
	}
	if (fileID{uint64(st.Dev), uint64(st.Ino)}) != r.Project.created {
		t.Errorf("the held handle is not the directory this run created: %v", st)
	}
	if _, err := os.Stat(apDst(ws, "")); err != nil {
		t.Errorf("the root must exist: %v", err)
	}
}

// C2(1): a retry that fails at the parent sync twice still finishes the root this run created on the
// third attempt. The root is pinned from the second attempt on, so the retry path must report it as
// this run's own from the identity it recorded rather than as an existing directory, and that retry's
// own parent sync must be the one this case fails second - not a child's creation later in the run.
// The head before this cycle failed a child's sync as the second error, so the case passed without
// exercising the repeated root sync it names.
func TestMigrateOwnedDirIdentityRepeatedSyncFailureStillFinishesTheRoot(t *testing.T) {
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	syncs := 0
	rootSyncs := 0
	migrateOwnedDirIdentitySteps(t, func(step string) error {
		if step == "sync" {
			syncs++
			// The root's own sync is the first sync of an attempt: it runs before any child directory
			// is created, so a sync that arrives before any child exists is the root's.
			if syncs <= 2 {
				if _, err := os.Lstat(apDst(ws, "sessions")); err != nil {
					rootSyncs++
				}
				return errApplyInterrupted
			}
		}
		return nil
	})
	for i := range 2 {
		if _, err := apply(r, p); !errors.Is(err, errApplyInterrupted) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if rootSyncs != 2 {
		t.Fatalf("the case must fail the root's own parent sync twice, it failed it %d times", rootSyncs)
	}
	res, err := apply(r, p)
	must(t, err)
	migrateOwnedDirIdentityWantRaw(t, apDst(ws, ""), 0o755)
	if ai := apItem(t, res, "."); strings.Contains(ai.Note, "kept its mode") {
		t.Errorf("the third attempt must finish the root this run made, note = %q", ai.Note)
	}
}

// C2(4): the pin answers the platform's own answer, so the fchmodat2 case's skip and the by-name case's
// path are chosen by the platform rather than assumed.
func TestMigrateOwnedDirIdentityPinAnswersThePlatform(t *testing.T) {
	fd, err := migrateOwnedDirIdentityPin(-1, "x")
	if ownedDirIdentityHandleOK {
		if err == nil {
			t.Errorf("the pin must fail on an invalid descriptor, got fd %d", fd)
		}
		return
	}
	if !ownedDirIdentityNoHandle(err) {
		t.Errorf("a platform without a handle must answer ownedDirIdentityNoHandle, got %v", err)
	}
}

// C2(1): a same-Pair retry must not adopt a root another actor put at the name, even when the retry
// itself pinned that replacement before a later step failed. The lookup that saw the root absent is not
// repeated, so the pair has to remember it: without that the third attempt reads the replacement as a
// directory that merely existed already and adopts it through the marker branch. The head before this
// cycle took the adoption path and left the replacement at the source mode with no note.
func TestMigrateOwnedDirIdentityReplacedRootIsNotAdoptedOnALaterRetry(t *testing.T) {
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	root := apDst(ws, "")
	syncs := 0
	clearAt := migrateOwnedDirIdentitySteps(t, func(step string) error {
		if step == "sync" {
			if syncs++; syncs == 1 { // the project root's creation, which this case stops
				return errApplyInterrupted
			}
		}
		return nil
	})
	if _, err := apply(r, p); !errors.Is(err, errApplyInterrupted) {
		t.Fatalf("the interrupted run: %v", err)
	}
	migrateOwnedDirIdentityWantRaw(t, root, 0o700)
	// Another actor takes the name: this run's root is moved aside and a 01700 root is put there.
	must(t, os.Rename(root, root+".ours"))
	mkdirs(t, root)
	migrateOwnedDirIdentitySetRaw(t, root, applyTempRaw)
	// The second attempt pins the replacement and then fails at the publisher's root step, so the
	// replacement is the root the pair holds when the third attempt begins.
	pub := newPub(t)
	pub.at = func(step string) error {
		if step == "root" {
			return errApplyInterrupted
		}
		return nil
	}
	if _, err := applyWith(r, p, pub); !errors.Is(err, errApplyInterrupted) {
		t.Fatalf("the second attempt: %v", err)
	}
	clearAt()
	res, err := apply(r, p)
	must(t, err)
	migrateOwnedDirIdentityWantRaw(t, root, applyTempRaw)
	migrateOwnedDirIdentityWantKeptNote(t, res, ".")
}

// C2(2): the pin this run takes on its temporary is held across the read handle's open, so device and
// inode equality proves the handle names the directory the mode was just given to. The head before this
// cycle released the pin first, so a replacement that took the name could be handed back as this run's.
// The swap is driven through the creation-step seam, never a sleep.
func TestMigrateOwnedDirIdentityPinIsHeldAcrossTheReadHandle(t *testing.T) {
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	swapped := false
	migrateOwnedDirIdentitySteps(t, func(step string) error {
		if step != "held" || swapped {
			return nil
		}
		swapped = true
		// The temporary this run created is the only entry of the package's naming rule in the workspace.
		entries, err := os.ReadDir(ws)
		must(t, err)
		target := ""
		for _, e := range entries {
			if _, ok := tempRun(e.Name()); ok {
				target = filepath.Join(ws, e.Name())
			}
		}
		if target == "" {
			t.Fatal("the case found no temporary to swap")
		}
		var before unix.Stat_t
		must(t, unix.Lstat(target, &before))
		// The racer removes the directory this run created and puts its own at the same name. While the
		// run holds its pin, the removed directory's inode is still referenced and cannot be handed out
		// again, so the replacement cannot carry the identity this run recorded.
		must(t, os.Remove(target))
		mkdirs(t, target)
		var after unix.Stat_t
		must(t, unix.Lstat(target, &after))
		if (fileID{uint64(after.Dev), uint64(after.Ino)}) == (fileID{uint64(before.Dev), uint64(before.Ino)}) {
			t.Skip("this filesystem reused the inode of the removed directory")
		}
		return nil
	})
	if _, err := apply(r, p); err == nil {
		t.Fatal("a replacement put at the temporary while the pin was held must stop the run")
	}
	if !swapped {
		t.Fatal("the run never held the pin across the read handle's open")
	}
}

// C2(3): the leftover a run interrupted while creating the destination root is reported at its own
// location. It sits beside the root, not inside it, so the item carries that location rather than a
// destination-root-relative path that would place it inside the root. The head before this cycle
// reported it with the holder's leaf name as a destination inside the root.
func TestMigrateOwnedDirIdentityRootTemporaryIsReportedAtItsOwnLocation(t *testing.T) {
	ws, _, _ := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	leftover := filepath.Join(ws, tempName("ABCDEFGHIJKLMNOPQRSTUVWXYZ", 1))
	mkdirs(t, leftover)
	again, err := Open(Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	t.Cleanup(func() { _ = again.Close() })
	plan, err := classify(again)
	must(t, err)
	for _, it := range plan.Items {
		if it.Reason != inventoryReasonOldTemp || filepath.Base(it.Source) != filepath.Base(leftover) {
			continue
		}
		if it.Source != leftover {
			t.Errorf("the leftover must be reported at %q, got source %q", leftover, it.Source)
		}
		if it.Destination != "" {
			t.Errorf("a leftover beside the root has no destination-root-relative path, got %q", it.Destination)
		}
		return
	}
	t.Errorf("the next run must report the root's leftover temporary, got %d items", len(plan.Items))
}

// C2(3): that leftover is reported whatever the destination root's own state. Another actor can create
// the root after the interrupted run left the temporary beside it, and the report must still name it.
// The head before this cycle read the holder only while the root or its mapped directory was missing, so
// the leftover vanished from the report once the root existed.
func TestMigrateOwnedDirIdentityRootTemporaryIsReportedAfterTheRootExists(t *testing.T) {
	ws, _, _ := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	leftover := filepath.Join(ws, tempName("ABCDEFGHIJKLMNOPQRSTUVWXYZ", 1))
	mkdirs(t, leftover)
	mkdirs(t, apDst(ws, ""))
	again, err := Open(Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	t.Cleanup(func() { _ = again.Close() })
	plan, err := classify(again)
	must(t, err)
	for _, it := range plan.Items {
		if it.Reason == inventoryReasonOldTemp && filepath.Base(it.Source) == filepath.Base(leftover) {
			return
		}
	}
	t.Errorf("the leftover must be reported even when the root exists, got %d items", len(plan.Items))
}
