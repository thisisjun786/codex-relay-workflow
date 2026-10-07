package migrate

// apply_owned_dir_test.go holds the CRW-813 cases. The state-copy apply used to decide that a
// destination directory was its own from a lookup that found the name absent when the roots were
// pinned, so a directory another actor created in the window before the run's own mkdir was treated as
// this run's: it was given the private marker mode and finished at the source mode, and the other
// actor's mode was lost. The apply now decides that from the result of its own mkdir, and a destination
// root it created carries the marker mode explicitly, so a rerun still recognises a root an interrupted
// first run made. Each case names the behaviour it pins; the ownership cases and the two user-root
// marker cases fail on the code before CRW-813, and the project-root marker case pins behaviour CRW-812
// already established, kept beside the user-root case so the two scopes stay covered together.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// migrateOwnedDirUserPlan classifies a user scope whose source holds entries and returns the source
// root, the destination root (which does not exist yet) and the plan. Every environment root is a
// temporary directory.
func migrateOwnedDirUserPlan(t *testing.T, entries map[string]string) (string, string, *Roots, *Plan) {
	t.Helper()
	base := isolate(t)
	u, v := filepath.Join(base, "eu"), filepath.Join(base, "ev")
	mkdirs(t, u)
	invTree(t, u, entries)
	r, err := Open(Options{Scope: ScopeUser, FromHome: u, ToHome: v})
	must(t, err)
	t.Cleanup(func() { _ = r.Close() })
	p, err := classify(r)
	must(t, err)
	return u, v, r, p
}

// migrateOwnedDirRaw reads a directory's raw mode through the same reader finishModes uses - the pinned
// handle and applyDirRaw - so a case proves what the rerun's ownership judgement would actually see,
// not a second reading taken another way.
func migrateOwnedDirRaw(t *testing.T, path string) uint32 {
	t.Helper()
	d := openDir(t, path)
	defer d.Close()
	raw, err := applyDirRaw(d)
	must(t, err)
	return raw
}

// C1: a .crw another actor makes 0700 after Open and before apply is not this run's directory, so the
// run never gives it the marker mode and keeps its mode instead of widening it to the source mode.
func TestMigrateOwnedDirKeepsAForeignProjectRootMode(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}"}, nil)
	root := apDst(ws, "")
	mkdirs(t, root)
	must(t, os.Chmod(root, 0o700))
	pub := newPub(t)
	marker := uint32(0)
	pub.at = func(step string) error {
		if step == "root" {
			marker = migrateOwnedDirRaw(t, root)
		}
		return nil
	}
	res, err := applyWith(r, p, pub)
	must(t, err)
	if marker != 0o700 {
		t.Errorf("a root this run did not create was given %#o; it must keep %#o", marker, 0o700)
	}
	if fi, err := os.Stat(root); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("a root this run did not create must keep its mode: %v %v", fi, err)
	}
	if ai := apItem(t, res, "."); ai.Note == "" {
		t.Errorf("the kept mode must be reported, note = %q", ai.Note)
	}
	if got := get(t, apDst(ws, "sessions/a.json")); got != "{\"phase\":\"P\"}" {
		t.Errorf("the copy must still complete: %q", got)
	}
}

// C1: the same for a user root another actor made 0700.
func TestMigrateOwnedDirKeepsAForeignUserRootMode(t *testing.T) {
	_, v, r, p := migrateOwnedDirUserPlan(t, map[string]string{"config.json": "{}"})
	mkdirs(t, v)
	must(t, os.Chmod(v, 0o700))
	res, err := apply(r, p)
	must(t, err)
	if fi, err := os.Stat(v); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("a user root this run did not create must keep its mode: %v %v", fi, err)
	}
	if ai := apItem(t, res, "."); ai.Note == "" {
		t.Errorf("the kept mode must be reported, note = %q", ai.Note)
	}
}

// C1: a child directory another actor makes in the window between the lookup that found it absent and
// the mkdir that would create it keeps its mode. The window is driven through the package's own seam,
// never a sleep.
func TestMigrateOwnedDirKeepsAChildDirectorysModeWhenARacerCreatesIt(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}"}, nil)
	seam := migrateOwnedDirBeforeEnsureChild
	t.Cleanup(func() { migrateOwnedDirBeforeEnsureChild = seam })
	migrateOwnedDirBeforeEnsureChild = func() {
		migrateOwnedDirBeforeEnsureChild = nil
		dir := apDst(ws, "sessions")
		mkdirs(t, dir)
		must(t, os.Chmod(dir, 0o700))
	}
	res, err := apply(r, p)
	must(t, err)
	if fi, err := os.Stat(apDst(ws, "sessions")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("a directory this run did not create must keep its mode: %v %v", fi, err)
	}
	if ai := apItem(t, res, "sessions"); ai.Note == "" {
		t.Errorf("the kept mode must be reported, note = %q", ai.Note)
	}
	if got := get(t, apDst(ws, "sessions/a.json")); got != "{\"phase\":\"P\"}" {
		t.Errorf("the copy must still complete: %q", got)
	}
}

// C1: a user root this run creates carries the marker mode before any child is published, read the way
// finishModes reads it. mkdir's own result is not enough: Darwin drops the sticky bit from a
// directory's creation mode, and the marker is what tells a later run that this migration made the
// directory. The host whose mkdir drops the sticky bit is modelled through the package's own seam.
func TestMigrateOwnedDirCreatedUserRootCarriesTheMarkerBeforeAChild(t *testing.T) {
	restore := migrateOwnedDirMkdirat
	t.Cleanup(func() { migrateOwnedDirMkdirat = restore })
	migrateOwnedDirMkdirat = func(dirfd int, name string, mode uint32) error {
		// Darwin's mkdirat keeps the permission bits and drops the sticky bit of the mode it is given.
		return restore(dirfd, name, mode&^uint32(unix.S_ISVTX))
	}
	_, v, r, p := migrateOwnedDirUserPlan(t, map[string]string{"config.json": "{}"})
	pub := newPub(t)
	seen := false
	pub.at = func(step string) error {
		if step == "rename" && !seen {
			seen = true
			if raw := migrateOwnedDirRaw(t, v); raw != applyTempRaw {
				t.Errorf("the root this run made carries %#o before any child is published, want %#o", raw, applyTempRaw)
			}
		}
		return nil
	}
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !seen {
		t.Fatal("no publication ran")
	}
	if fi, err := os.Stat(v); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("the root must finish at the source mode: %v %v", fi, err)
	}
}

// C1: an interrupted first run leaves the user root it made at the marker mode, so the rerun finishes
// that root's mode instead of keeping whatever the mkdir left. The mkdir seam is the same one the case
// above uses, so the interruption happens on a host whose mkdir drops the sticky bit.
func TestMigrateOwnedDirRerunRecognisesAUserRootTheRunMade(t *testing.T) {
	restore := migrateOwnedDirMkdirat
	t.Cleanup(func() { migrateOwnedDirMkdirat = restore })
	migrateOwnedDirMkdirat = func(dirfd int, name string, mode uint32) error {
		return restore(dirfd, name, mode&^uint32(unix.S_ISVTX))
	}
	u, v, r1, p1 := migrateOwnedDirUserPlan(t, map[string]string{"config.json": "{}"})
	pub := newPub(t)
	pub.at = apFailRename(1)
	if _, err := applyWith(r1, p1, pub); !errors.Is(err, errApplyInterrupted) {
		t.Fatalf("interrupted first run: %v", err)
	}
	if raw := migrateOwnedDirRaw(t, v); raw != applyTempRaw {
		t.Fatalf("the interrupted run must leave the marker mode: %#o", raw)
	}
	// The rerun runs with the host's real mkdir again: the root already exists, so only its recorded mode
	// decides whether the run finishes it.
	migrateOwnedDirMkdirat = restore
	r2, err := Open(Options{Scope: ScopeUser, FromHome: u, ToHome: v})
	must(t, err)
	t.Cleanup(func() { _ = r2.Close() })
	p2, err := classify(r2)
	must(t, err)
	if _, err := apply(r2, p2); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(v); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("the rerun must finish the mode of the root this run made: %v %v", fi, err)
	}
	if got := get(t, filepath.Join(v, "config.json")); got != "{}" {
		t.Errorf("the rerun must finish the copy: %q", got)
	}
}

// C1: a project root this run creates carries the marker before its canonical .gitignore is published,
// the behaviour CRW-812 gave EnsureProjectRoot, kept pinned beside the user-root case.
func TestMigrateOwnedDirCreatedProjectRootCarriesTheMarker(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}"}, nil)
	pub := newPub(t)
	seen := false
	pub.at = func(step string) error {
		if step == "root" && !seen {
			seen = true
			if raw := migrateOwnedDirRaw(t, apDst(ws, "")); raw != applyTempRaw {
				t.Errorf("the root this run made carries %#o before any child is published, want %#o", raw, applyTempRaw)
			}
		}
		return nil
	}
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !seen {
		t.Fatal("the root step never ran")
	}
}
