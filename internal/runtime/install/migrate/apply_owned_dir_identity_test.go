package migrate

// apply_owned_dir_identity_test.go holds the CRW-887 cases. CRW-813 decided that a destination
// directory was this run's from the result of this run's own mkdir, but that mkdir still happened at the
// directory's final name, the private marker mode was still given by the caller after a fallible open and
// parent sync, and finishModes still adopted a foreign directory that happened to carry exactly the marker
// mode. These cases pin the creation under a temporary name of this run, the mode given to that name
// before anything opens the directory, the fstat check that proves the pinned descriptor is the one this
// run created, and the record that keeps finishModes from adopting a directory another actor made inside
// the run's own window. Each case names the behaviour it pins; the ownership cases and the umask cases
// fail on the code before CRW-887, and the interrupted-creation cases fail there too, with the directory
// left without the marker and never finished.

import (
	"errors"
	"fmt"
	"io/fs"
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
// creation mode and drops the sticky bit, which is what Darwin does and the reason the mode is given to
// the temporary name before the directory is opened. The returned function restores the real mkdir, so a
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

// migrateOwnedDirIdentityUnderUmask runs fn with the process umask set to um and restores it before
// returning, so no other case of this package sees it. The umask is process-wide, and no case here runs
// in parallel.
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
// directory, so the run must not adopt it through the marker branch: it keeps 01700 and is reported. On
// the code before CRW-887 finishModes read it as an interrupted run's own directory and finished it at
// the source mode, 0755 with no note.
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
// made false, leaves no temporary behind, and keeps the mode the racer gave the directory. On the code
// before CRW-887 there was no temporary: the racer's directory stood at the name itself, so the run read
// it as its own and chmodded it.
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

// C1c: an interruption at the open of this run's temporary must not leave a directory a rerun then fails
// to recognise. The mkdir seam models Darwin, so the directory would have been left without the marker
// had the mode not been given to the temporary name before the open. The rerun recognises the directory
// this run made and finishes it at the source mode; on the code before CRW-887 the interruption left it
// unmarked and the rerun never finished it.
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
// On the code before CRW-887 mkdirat(0o1700) produced a directory its owner could not read, the
// following open failed with EACCES and the run stopped before any mode was given.
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

// C1: a kernel whose fchmodat cannot express no-follow (Linux before fchmodat2) is refused rather than
// falling back to a chmod that would follow a link another actor could put at the temporary name. The
// refusal stops the run, and the directory this run made is removed so no unmarked directory is left.
func TestMigrateOwnedDirIdentityChmodWithoutNoFollowIsRefused(t *testing.T) {
	restore := migrateOwnedDirIdentityFchmodat
	t.Cleanup(func() { migrateOwnedDirIdentityFchmodat = restore })
	var flags []int
	migrateOwnedDirIdentityFchmodat = func(dirfd int, name string, mode uint32, fl int) error {
		flags = append(flags, fl)
		if fl != 0 {
			return unix.EOPNOTSUPP
		}
		return restore(dirfd, name, mode, fl)
	}
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	_, err := apply(r, p)
	wantRefusal(t, err, ReasonUnsupported)
	if len(flags) == 0 || flags[0] != unix.AT_SYMLINK_NOFOLLOW {
		t.Errorf("the by-name chmod was asked with flags %v; want no-follow first", flags)
	}
	for _, f := range flags {
		if f == 0 {
			t.Error("the run fell back to a chmod that follows a link")
		}
	}
	if _, err := os.Lstat(apDst(ws, "")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused creation must leave no directory: %v", err)
	}
	migrateOwnedDirIdentityWantNoTemp(t, ws)
}

// C1: a creation that reached its rename and then failed is this run's directory, so a retry with the
// same pinned pair finishes its mode instead of reading it as another actor's. On the code before the
// retry fix the second apply reported the root as a denied creation and left it at the marker mode.
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

// C1: a creation whose name this run cannot read back leaves no directory behind. The read that takes
// the identity of the name this run just made is the one step between the mkdirat and the cleanup
// defer, so a failure there must remove the directory this run made rather than leave it.
func TestMigrateOwnedDirIdentityUnreadableTemporaryLeavesNothing(t *testing.T) {
	restore := migrateOwnedDirIdentityLstat
	t.Cleanup(func() { migrateOwnedDirIdentityLstat = restore })
	calls := 0
	migrateOwnedDirIdentityLstat = func(dirfd int, name string, st *unix.Stat_t) error {
		if calls++; calls == 1 {
			return unix.EIO
		}
		return restore(dirfd, name, st)
	}
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	if _, err := apply(r, p); err == nil {
		t.Fatal("a temporary this run cannot read back must stop the run")
	}
	// The root's own creation is what failed, so the root does not exist and nothing of this run's is
	// left in the worktree.
	if _, err := os.Lstat(apDst(ws, "")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a failed creation must leave no directory: %v", err)
	}
	migrateOwnedDirIdentityWantNoTemp(t, ws)
}
