package supervisor

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Ownership states for this package's comparisons with live Python. Every store is fenced
// (docs/port/decisions.md 14 and 30) and each runtime refuses one the other owns, so a comparison
// puts the store each runtime reads into the state a real host would have for that step - the
// runtime under test owns it - with the shared testsupport fixture API and nothing else:
//
//   - restoreSnapshot: a captured snapshot copied back to where its run kept the store.
//   - ownedCopy:       a copy of a store the other runtime owns, for a read by this one.
//   - ownCopied:       a copy the test made itself, in a directory of its own.
//
// A snapshot taken with SQLite's backup API is a new file holding the durable stamp and no
// mirror; Rehome gives such a copy the mirror its stamp implies, and HandOver moves it to the
// runtime that reads it next.

// restoreSnapshot puts a copy of a captured store snapshot at dbPath, the path the capturing run
// kept its store at and the one every captured output names, owned by owner ("python" or "go").
// The store the snapshot replaces is deleted whole - its database, WAL, shared-memory and
// rollback-journal files and the takeover.json mirror naming it - as a host deletes a store; the
// write gate and takeover lock stay, since a rendezvous inode is never replaced. The snapshot is
// then a fenced store copied into the directory, which is exactly what Rehome is for.
func restoreSnapshot(t testing.TB, dbPath string, data []byte, owner string) {
	t.Helper()
	removeStore(t, dbPath)
	if err := os.WriteFile(dbPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	ownCopied(t, dbPath, owner)
}

// ownCopied takes a copy of a fenced store that a test made in a directory of its own - a SQLite
// backup, a VACUUM INTO or a file copy, none of which carries the mirror or names this file -
// gives it the mirror its durable stamp implies and hands it to owner.
func ownCopied(t testing.TB, path, owner string) {
	t.Helper()
	testsupport.Rehome(t, path)
	testsupport.HandOver(t, path, owner)
}

// removeStore deletes the store at dbPath and the mirror beside it when the mirror names it (or
// names nothing that still exists). A mirror that belongs to another store in the directory is
// left for Rehome to refuse: every store needs a directory of its own.
func removeStore(t testing.TB, dbPath string) {
	t.Helper()
	absolute, err := filepath.Abs(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	real := absolute
	if resolved, e := filepath.EvalSymlinks(absolute); e == nil {
		real = resolved
	}
	mirror := filepath.Join(filepath.Dir(absolute), "takeover.json")
	if raw, e := os.ReadFile(mirror); e == nil {
		var record struct {
			Database struct {
				RealPath string `json:"realPath"`
			} `json:"database"`
		}
		if e = json.Unmarshal(raw, &record); e != nil {
			t.Fatalf("mirror beside %s: %v", dbPath, e)
		}
		named := record.Database.RealPath
		if _, gone := os.Lstat(named); named == real || named == absolute || errors.Is(gone, os.ErrNotExist) {
			if e = os.Remove(mirror); e != nil {
				t.Fatal(e)
			}
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		t.Fatal(e)
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if e := os.Remove(absolute + suffix); e != nil && !errors.Is(e, os.ErrNotExist) {
			t.Fatal(e)
		}
	}
}

// ownedCopy copies the store at src - its database and its WAL as they stand, so the copy reads
// what src reads - into a fresh directory of its own, and hands the copy to owner. The source is
// left untouched and may stay open in its own runtime. It returns the copy's path, named like src.
func ownedCopy(t testing.TB, src, owner string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, filepath.Base(src))
	for _, suffix := range []string{"", "-wal"} {
		if err := copyFile(src+suffix, path+suffix); err != nil && !(suffix != "" && errors.Is(err, os.ErrNotExist)) {
			t.Fatal(err)
		}
	}
	ownCopied(t, path, owner)
	return path
}

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, in.Close()) }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Close())
}

// pythonCopy is the fixture's store as it stands, in a copy Python owns, for the Python half of
// a comparison taken while the Go store stays open and owned by Go for the Go half. Every call
// copies afresh, so Python reads what Go's store holds at that moment; what Python writes stays
// in its copy, where the comparison's Python side reads it back.
func pythonCopy(t testing.TB, f *stageFixture) string {
	t.Helper()
	return ownedCopy(t, filepath.Join(f.root, "state", "relay.sqlite3"), "python")
}
