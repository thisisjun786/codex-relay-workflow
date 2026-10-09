package install_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// CRW-1004: what the pre-merge evaluation of the state backup (CRW-862, pr787-1bfd207d d1 and d3) and the review of
// its fourth generation left open. All state is temporary; nothing touches an operational store.

// d1: a log that is a link to a file outside the state directory, whose target goes between the first listing and
// the copy while the link is retargeted. The copy of the log fails with ENOENT, which the loop reads as "the log went",
// so the first listing's entry for the log never reached the comparison and the move of the link and of the resolved
// source went unseen: the backup succeeded although the state directory no longer named the file the first listing
// read. The first listing's identity and the source the copy actually read are two comparisons.
//
// sequential: replaces the state-backup seam.
func TestTheBackupRefusesALogLinkRetargetedWhenItsFirstTargetWentBeforeTheCopy(t *testing.T) {
	h, _, second, old, _ := zoneInstalled(t)
	zoneStore(t, h)
	outside := t.TempDir()
	first, replacement := filepath.Join(outside, "wal-one"), filepath.Join(outside, "wal-two")
	write(t, first, "")
	write(t, replacement, "")
	wal := filepath.Join(h.relayState, sidecarWalName)
	_ = os.Remove(wal)
	if err := os.Symlink(first, wal); err != nil {
		t.Fatal(err)
	}
	restore := install.ReplaceStateBackupListed(func() error {
		if err := os.Remove(first); err != nil {
			return err
		}
		if err := os.Remove(wal); err != nil {
			return err
		}
		return os.Symlink(replacement, wal)
	})
	defer restore()
	o := h.options()
	o.StateBackup = backupOf(h, "wal-first-target-gone")
	result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.Refused || at(result, "swapGate", "verdict") != "BLOCKED" || h.pointerTarget(t) != old {
		t.Fatalf("a log link retargeted after its first target went must refuse: exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
	if !strings.Contains(text(at(result, "swapGate", "stateBackup", "error")), "write-ahead log changed") {
		t.Fatalf("the refusal must name the changed log: %s", golden.Canon(at(result, "swapGate", "stateBackup")))
	}
}

// The log that is an ordinary file and is checkpointed away between the listing and its copy is still not a
// refusal (the case the route exists for): its identity is the same in both readings, or it is absent from the second.
//
// sequential: replaces the state-backup seam.
func TestTheBackupStillAcceptsAnOrdinaryLogThatWentAndCameBackBetweenTheListings(t *testing.T) {
	h, _, second, _, next := zoneInstalled(t)
	zoneStore(t, h)
	putSidecar(t, h, sidecarWalName, 32)
	wal := filepath.Join(h.relayState, sidecarWalName)
	restore := install.ReplaceStateBackupListed(func() error {
		if err := os.Remove(wal); err != nil {
			return err
		}
		return os.WriteFile(wal, make([]byte, 32), 0o600)
	})
	defer restore()
	o := h.options()
	o.StateBackup = backupOf(h, "wal-recreated")
	result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.OK || at(result, "promoted") != true || h.pointerTarget(t) != next {
		t.Fatalf("a log recreated in place must not refuse: exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
}

// The case the test above does not reach: the ordinary log is gone when its copy starts (so it is left out and the
// manifest says it went) and a header-only log is back in the state directory only after the copy, before the second
// listing. The second listing names a log the copy does not hold, which holds no frame, and is accepted.
//
// sequential: replaces the state-backup seams.
func TestTheBackupAcceptsAnOrdinaryLogThatWentBeforeItsCopyAndCameBackAfterIt(t *testing.T) {
	h, _, second, _, next := zoneInstalled(t)
	zoneStore(t, h)
	putSidecar(t, h, sidecarWalName, 32)
	wal := filepath.Join(h.relayState, sidecarWalName)
	restoreListed := install.ReplaceStateBackupListed(func() error { return os.Remove(wal) })
	defer restoreListed()
	restoreStep := install.ReplaceStateBackupStep(func(step string) error {
		if step == "copied" {
			return os.WriteFile(wal, make([]byte, 32), 0o600)
		}
		return nil
	})
	defer restoreStep()
	o := h.options()
	o.StateBackup = backupOf(h, "wal-back-after-copy")
	result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.OK || at(result, "promoted") != true || h.pointerTarget(t) != next {
		t.Fatalf("a header-only log back after the copy must not refuse: exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
	backup := backupOf(h, "wal-back-after-copy")
	nothingAt(t, filepath.Join(backup, sidecarWalName))
	if got := storeSidecarsOf(t, backup)[sidecarWalName]; got != "gone before its copy" {
		t.Errorf("storeSidecars[%s] = %q, want %q", sidecarWalName, got, "gone before its copy")
	}
}

// The scratch preflight charges the store's own two files and nothing that only starts with their name: a leftover
// journal, a store-like directory's contents and a backup copy of the store are not duplicated by the gate.
//
// sequential: none (a white-box reading).
func TestTheSpacePreflightChargesOnlyTheStoreAndItsLog(t *testing.T) {
	need := install.ScratchNeed(map[string]int64{
		"relay.sqlite3":                       90 << 20,
		"relay.sqlite3-wal":                   4 << 20,
		"relay.sqlite3-journal":               7 << 20,
		"relay.sqlite3.bak":                   60 << 20,
		"relay.sqlite3-archive/relay.sqlite3": 5 << 20,
		"ledger.log":                          1 << 20,
	})
	if want := int64(94 << 20); need != want {
		t.Fatalf("scratch need = %d, want %d (relay.sqlite3 and relay.sqlite3-wal only)", need, want)
	}
}

// pageSize is SQLite's default page size, which the corruption below counts in.
const pageSize = 4096

// growStore makes a store at path with enough rows and an index that the file holds several b-tree pages beyond the
// schema's, so a page in the middle belongs to a table or an index and not to the header.
func growStore(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA journal_mode=DELETE",
		"CREATE TABLE rows(id INTEGER PRIMARY KEY, k TEXT, v TEXT)",
		"CREATE INDEX rows_k ON rows(k)",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 400; i++ {
		if _, err := tx.Exec("INSERT INTO rows(k, v) VALUES(?, ?)", fmt.Sprintf("key-%04d", i), strings.Repeat("v", 200)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// damagePages overwrites whole pages of the database file at path with a byte no b-tree page begins with, so SQLite
// itself finds the damage. The pages chosen are past the schema page.
func damagePages(t *testing.T, path string, pages ...int) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range pages {
		if int64(page)*pageSize > info.Size() {
			t.Fatalf("the store has %d bytes, no page %d to damage", info.Size(), page)
		}
		if _, err := file.WriteAt(bytes.Repeat([]byte{0xFF}, pageSize), int64(page-1)*pageSize); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
}

// d3: the gate is exercised on a store whose pages are really damaged, with SQLite's own PRAGMA integrity_check and
// no substituted answer. A healthy store of the same shape passes, so the failure is the damage and not the fixture.
//
// sequential: none (private temporary directories).
func TestTheIntegrityGateFindsARealDamagedPage(t *testing.T) {
	check := func(t *testing.T, damaged bool) (string, []byte, []byte, error) {
		t.Helper()
		dest := filepath.Join(t.TempDir(), "backup")
		if err := os.Mkdir(dest, 0o700); err != nil {
			t.Fatal(err)
		}
		store := filepath.Join(dest, "relay.sqlite3")
		growStore(t, store)
		if damaged {
			damagePages(t, store, 3, 4)
		}
		before := mustRead(t, store)
		answer, err := install.SqliteIntegrityCheck(context.Background(), dest, []install.BackedUp{{Path: "relay.sqlite3", Kind: "file", Size: int64(len(before))}})
		return answer, before, mustRead(t, store), err
	}
	t.Run("a healthy store", func(t *testing.T) {
		answer, before, after, err := check(t, false)
		if err != nil || answer != "ok" {
			t.Fatalf("a healthy store: answer %q, error %v", answer, err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("the gate changed the copy")
		}
	})
	t.Run("a store with damaged pages", func(t *testing.T) {
		answer, before, after, err := check(t, true)
		if err == nil || answer == "ok" {
			t.Fatalf("the gate passed a store whose pages are damaged: answer %q, error %v", answer, err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("the gate changed the damaged copy")
		}
	})
}

// d3 end to end: the copy is damaged for real after it was verified and before the gate runs, the gate is SQLite's own,
// and the command refuses, keeps the copy and its manifest, and records that it is not a restore candidate with what
// SQLite answered.
//
// sequential: replaces the state-backup verified seam.
func TestTheBackupRefusesACopyWhoseStorePagesAreReallyDamaged(t *testing.T) {
	h, _, second, old, _ := zoneInstalled(t)
	zoneStore(t, h)
	dest := backupOf(h, "damaged-pages")
	restore := install.ReplaceStateBackupVerified(func() error {
		copyPath := filepath.Join(dest, "relay.sqlite3")
		if err := os.Chmod(copyPath, 0o600); err != nil {
			return err
		}
		damagePages(t, copyPath, 3)
		return nil
	})
	defer restore()
	o := h.options()
	o.StateBackup = dest
	result, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.Refused || at(result, "swapGate", "verdict") != "BLOCKED" || h.pointerTarget(t) != old {
		t.Fatalf("a copy with damaged pages must refuse: exit %d\n%s", code, golden.Canon(at(result, "swapGate")))
	}
	var manifest struct {
		IntegrityCheck   string `json:"integrityCheck"`
		RestoreCandidate bool   `json:"restoreCandidate"`
	}
	if err := json.Unmarshal(mustRead(t, dest+install.ManifestSuffix), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.RestoreCandidate || manifest.IntegrityCheck == "" || manifest.IntegrityCheck == "ok" {
		t.Fatalf("a copy with damaged pages must not be a restore candidate: %+v", manifest)
	}
}

// crashImage leaves path as an unclean stop leaves a store: relay.sqlite3 as it was before a commit and a
// write-ahead log that holds the commit, with no shared-memory index. It writes through a connection that never
// checkpoints, takes both files while that connection is open (the main file is untouched until a checkpoint), closes
// the connection (which checkpoints and removes the log) and puts the two images back.
func crashImage(t *testing.T, path, table string) (log []byte) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA wal_autocheckpoint=0",
		"CREATE TABLE " + table + "(x TEXT)",
		"INSERT INTO " + table + " VALUES('only in the log')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	main, log := mustRead(t, path), mustRead(t, path+"-wal")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path + "-wal", path + "-shm"} {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, main, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-wal", log, 0o600); err != nil {
		t.Fatal(err)
	}
	return log
}

// hasTable is whether the SQLite database at path holds the table, opened read-only so nothing is created.
func hasTable(t *testing.T, path, table string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = ?", table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// The operator command reads no schema before it copies, so a store an unclean stop left with commits only in its
// log is copied with that log as it lies: the copied relay.sqlite3 alone does not hold the commit, the copied log
// does, and restoreCandidate is true because the integrity check ran on a duplicate in which SQLite replays the log.
// The comments of the command and the install document say exactly this (CRW-1004).
//
// sequential: replaces the service reading.
func TestTheOperatorCommandCopiesAnUnmergedLogAsItLiesAndJudgesItReplayed(t *testing.T) {
	h := backupStateHost(t)
	state := filepath.Join(h.relayState, "relay.sqlite3")
	log := crashImage(t, state, "crw1004_unmerged")
	restore := install.ReplaceServiceReading(func(context.Context, install.Options) install.Object {
		return install.ServiceCell(scope.Stopped, true, "the service answered and reports itself not running")
	})
	defer restore()
	dest := filepath.Join(h.home, "operator-unmerged-log")
	result, code := install.BackupState(context.Background(), h.options(), dest)
	if code != install.OK {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if got := storeSidecarsOf(t, dest)[sidecarWalName]; got != "copied" {
		t.Fatalf("the log must be recorded as copied, got %q", got)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(dest, sidecarWalName)), log) {
		t.Fatal("the copied log is not the log as it lay")
	}
	var manifest struct {
		IntegrityCheck   string `json:"integrityCheck"`
		RestoreCandidate bool   `json:"restoreCandidate"`
	}
	if err := json.Unmarshal(mustRead(t, dest+install.ManifestSuffix), &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.RestoreCandidate || manifest.IntegrityCheck != "ok" {
		t.Fatalf("manifest: %+v", manifest)
	}
	// the copied main file alone does not hold the commit; with its log it does
	alone := filepath.Join(t.TempDir(), "relay.sqlite3")
	if err := os.WriteFile(alone, mustRead(t, filepath.Join(dest, "relay.sqlite3")), 0o600); err != nil {
		t.Fatal(err)
	}
	if hasTable(t, alone, "crw1004_unmerged") {
		t.Fatal("the copied relay.sqlite3 alone holds a commit that only the log held: the comment's premise is gone")
	}
	together := filepath.Join(t.TempDir(), "relay.sqlite3")
	if err := os.WriteFile(together, mustRead(t, filepath.Join(dest, "relay.sqlite3")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(together+"-wal", mustRead(t, filepath.Join(dest, sidecarWalName)), 0o600); err != nil {
		t.Fatal(err)
	}
	if !hasTable(t, together, "crw1004_unmerged") {
		t.Fatal("the copied log does not carry the commit")
	}
}
