package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

const (
	judgedPrefix = "database write judged unavailable: "
	triedPrefix  = "database write probe failed: "
)

// closedStore is a Go-owned store in an owner-only directory, closed: its database, its
// ownership mirror and its write gate, and nothing else.
func closedStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(stateDir(t), "relay.sqlite3")
	s, err := fixtureOpen(t.Context(), path, "")
	must(t, err)
	must(t, s.Close())
	return filepath.Dir(path)
}

// changedNames are the names a watched directory saw created, deleted or moved.
func changedNames(events []testsupport.DirEvent) []string {
	var names []string
	for _, op := range []string{"create", "delete", "move"} {
		names = append(names, testsupport.Names(events, op)...)
	}
	return names
}

func gateOpened(events []testsupport.DirEvent) bool {
	return slices.Contains(testsupport.Names(events, "open"), "write-gate.lock")
}

// A default probe is a read of the store: it creates no file of any kind in the state directory
// (no ".probe-" file, no SQLite sidecar) and never opens write-gate.lock, so it takes no lock on
// it. The directory is watched while the probe runs, which sees what strace would, without a
// tracer. The answer is judged, and says so.
func TestProbeDefaultCreatesNoFileAndTakesNoWriteGate(t *testing.T) {
	t.Parallel()
	dir := closedStore(t)
	watch := testsupport.WatchDir(t, dir)

	probed := Probe(t.Context(), StateSelection{Path: dir})
	events := watch.Drain()

	if a := probed.Access; !a.DBReadable || !a.DBWritable || !a.DirectoryWritable || a.Measured || a.Detail != "" {
		t.Fatalf("access %+v", a)
	}
	if names := changedNames(events); len(names) != 0 {
		t.Errorf("the default probe created, removed or moved %v (events %v)", names, events)
	}
	if gateOpened(events) {
		t.Errorf("the default probe opened the write gate (events %v)", events)
	}
	if entries, err := entryNames(dir); err != nil || !slices.Equal(entries, fencedStoreEntries) {
		t.Errorf("the default probe left %v (%v)", entries, err)
	}
}

// Beside a live writer, whose -wal and -shm exist, the default probe attaches to them and still
// creates nothing.
func TestProbeDefaultBesideALiveWriterCreatesNothingEither(t *testing.T) {
	t.Parallel()
	path := filepath.Join(stateDir(t), "relay.sqlite3")
	writer, err := fixtureOpen(t.Context(), path, "")
	must(t, err)
	t.Cleanup(func() { _ = writer.Close() })
	_, err = writer.WriteChallengeFor(t.Context(), "live")
	must(t, err)
	dir := filepath.Dir(path)
	watch := testsupport.WatchDir(t, dir)

	probed := Probe(t.Context(), StateSelection{Path: dir})
	events := watch.Drain()

	if a := probed.Access; !a.DBReadable || !a.DBWritable || a.Measured || a.Detail != "" {
		t.Fatalf("access %+v", a)
	}
	if names := changedNames(events); len(names) != 0 || gateOpened(events) {
		t.Errorf("the default probe changed %v or opened the write gate (events %v)", names, events)
	}
}

// The write probe is what asks to write: a temporary file in the state directory and the write
// gate, shared, for a write transaction that is rolled back.
func TestProbeWithWriteMeasuresByWriting(t *testing.T) {
	t.Parallel()
	dir := closedStore(t)
	watch := testsupport.WatchDir(t, dir)

	probed := ProbeWith(t.Context(), StateSelection{Path: dir}, ProbeOptions{Write: true})
	events := watch.Drain()

	if a := probed.Access; !a.DBReadable || !a.DBWritable || !a.DirectoryWritable || !a.Measured || a.Detail != "" {
		t.Fatalf("access %+v", a)
	}
	probe := func(names []string) bool {
		return slices.ContainsFunc(names, func(name string) bool { return strings.HasPrefix(name, ".probe-") })
	}
	if !probe(testsupport.Names(events, "create")) || !probe(testsupport.Names(events, "delete")) {
		t.Errorf("the write probe left no trace of its temporary file (events %v)", events)
	}
	if !gateOpened(events) {
		t.Errorf("the write probe did not take the write gate (events %v)", events)
	}
	if entries, err := entryNames(dir); err != nil || !slices.Equal(entries, fencedStoreEntries) {
		t.Errorf("the write probe left %v (%v)", entries, err)
	}
}

// What the default probe judges and the write probe tries, case by case. Both say false where the
// store may not be written, in the words of whichever ran; the judgement never answers true where
// the stamp, the gate or the read says no. A nil list for the write probe says nothing of what it
// answers (see the file mode case).
func TestProbeJudgesWhatItDoesNotTry(t *testing.T) {
	t.Parallel()
	both := func(t *testing.T, dir string, judgedWants, triedWants []string) {
		t.Helper()
		for _, c := range []struct {
			opts  ProbeOptions
			wants []string
		}{{ProbeOptions{}, judgedWants}, {ProbeOptions{Write: true}, triedWants}} {
			if c.wants == nil {
				continue
			}
			a := ProbeWith(t.Context(), StateSelection{Path: dir}, c.opts).Access
			if a.DBWritable {
				t.Errorf("write probe %v judged a store writable that it must not: %+v", c.opts.Write, a)
			}
			for _, want := range c.wants {
				if !strings.Contains(a.Detail, want) {
					t.Errorf("write probe %v: detail %q does not carry %q", c.opts.Write, a.Detail, want)
				}
			}
		}
	}
	t.Run("an unstamped store with no write gate is the wrong store, in plain words", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Dir(unstampedStore(t))
		want := "store_owned_by_other: " + UnstampedStoreDetail
		both(t, dir, []string{judgedPrefix + want}, []string{triedPrefix + want})
	})
	t.Run("a stamped store whose write gate alone is gone keeps the gate's words", func(t *testing.T) {
		t.Parallel()
		dir := closedStore(t)
		must(t, os.Remove(filepath.Join(dir, "write-gate.lock")))
		words := []string{"store_owned_by_other:", "write gate:", "no such file"}
		both(t, dir, append([]string{judgedPrefix}, words...), append([]string{triedPrefix}, words...))
	})
	// ownership.Lock opens the gate with O_NOFOLLOW and trusts only a regular file this user owns
	// that grants no group or other access, or sits in an owner-only directory: the judgement asks
	// the same of the gate's metadata without opening it, so it never says true where the write
	// probe's lock would refuse.
	gateWords := func(prefix string) []string { return []string{prefix + "store_owned_by_other:", "write gate:"} }
	t.Run("a write gate that is a symbolic link", func(t *testing.T) {
		t.Parallel()
		dir := closedStore(t)
		gate := filepath.Join(dir, "write-gate.lock")
		must(t, os.Rename(gate, filepath.Join(dir, "elsewhere.lock")))
		must(t, os.Symlink("elsewhere.lock", gate))
		both(t, dir, gateWords(judgedPrefix), gateWords(triedPrefix))
	})
	t.Run("a write gate that is a directory", func(t *testing.T) {
		t.Parallel()
		dir := closedStore(t)
		gate := filepath.Join(dir, "write-gate.lock")
		must(t, os.Remove(gate))
		must(t, os.Mkdir(gate, 0o700))
		both(t, dir, gateWords(judgedPrefix), gateWords(triedPrefix))
	})
	t.Run("a write gate open to the group in a directory open to it", func(t *testing.T) {
		t.Parallel()
		dir := closedStore(t)
		must(t, os.Chmod(filepath.Join(dir, "write-gate.lock"), 0o666))
		must(t, os.Chmod(dir, 0o770))
		both(t, dir, gateWords(judgedPrefix), gateWords(triedPrefix))
	})
	t.Run("a stamp this runtime would not be admitted by, beside a healthy mirror and gate", func(t *testing.T) {
		t.Parallel()
		dir := closedStore(t)
		raw, err := ownership.OpenExisting(t.Context(), filepath.Join(dir, "relay.sqlite3"), "rw")
		must(t, err)
		_, err = raw.Exec("UPDATE schema_meta SET value='0' WHERE key='owner_epoch'")
		must(t, err)
		must(t, raw.Close())
		both(t, dir, []string{judgedPrefix + "store_owned_by_other:"}, []string{triedPrefix + "store_owned_by_other:"})
	})
	t.Run("a database that cannot answer the identity read is not judged writable", func(t *testing.T) {
		t.Parallel()
		dir := stateDir(t)
		other, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "relay.sqlite3"))
		must(t, err)
		_, err = other.Exec("CREATE TABLE other (x)")
		must(t, err)
		must(t, other.Close())
		must(t, os.WriteFile(filepath.Join(dir, "write-gate.lock"), nil, 0o600))
		both(t, dir, []string{"database read failed:"}, []string{"database read failed:"})
	})
	t.Run("a database file this process may not write", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("a privileged process writes whatever the mode says")
		}
		dir := closedStore(t)
		must(t, os.Chmod(filepath.Join(dir, "relay.sqlite3"), 0o400))
		// The write probe's connection falls back to a read-only one that SQLite lets begin a
		// write transaction, so what it answers here is left out (backlog).
		both(t, dir, []string{judgedPrefix + "the database file:"}, nil)
	})
	t.Run("a write gate someone holds is seen by the write probe only", func(t *testing.T) {
		t.Parallel()
		dir := closedStore(t)
		held, err := ownership.Lock(filepath.Join(dir, "write-gate.lock"), true, false)
		must(t, err)
		t.Cleanup(func() { _ = held.Close() })
		if a := Probe(t.Context(), StateSelection{Path: dir}).Access; !a.DBWritable || a.Detail != "" {
			t.Errorf("the judgement takes no lock, so it cannot see a held gate: %+v", a)
		}
		a := ProbeWith(t.Context(), StateSelection{Path: dir}, ProbeOptions{Write: true}).Access
		if a.DBWritable || !strings.Contains(a.Detail, triedPrefix) || !strings.Contains(a.Detail, "write gate") {
			t.Errorf("the write probe meets the held gate: %+v", a)
		}
	})
}

// A gate another runtime created under umask 002 is 0664 inside an owner-only directory, which the
// lock trusts (decision D3): the judgement and the write probe both call that store writable.
func TestProbeTrustsAGateInAnOwnerOnlyDirectory(t *testing.T) {
	t.Parallel()
	dir := closedStore(t)
	must(t, os.Chmod(filepath.Join(dir, "write-gate.lock"), 0o664))
	for _, opts := range []ProbeOptions{{}, {Write: true}} {
		if a := ProbeWith(t.Context(), StateSelection{Path: dir}, opts).Access; !a.DBWritable || a.Detail != "" {
			t.Errorf("write probe %v: a 0664 gate in a 0700 directory: %+v", opts.Write, a)
		}
	}
}

// walStore is a store a live writer holds open whose newest commits exist ONLY in its write-ahead
// log (checkpointing off): a challenge row and a stamp this runtime would refuse (owner_epoch 0).
// A read that ignored the log would see the older, healthy stamp and none of the rows.
func walStore(t *testing.T) (dir, nonce string) {
	t.Helper()
	path := filepath.Join(stateDir(t), "relay.sqlite3")
	writer, err := fixtureOpen(t.Context(), path, "")
	must(t, err)
	t.Cleanup(func() { _ = writer.Close() })
	_, err = writer.DB.Exec("PRAGMA wal_autocheckpoint=0")
	must(t, err)
	challenge, err := writer.WriteChallengeFor(t.Context(), "wal-only")
	must(t, err)
	_, err = writer.DB.Exec("UPDATE schema_meta SET value='0' WHERE key='owner_epoch'")
	must(t, err)
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() <= walHeaderSize {
		t.Fatalf("the live log holds no frame: %v %v", info, err)
	}
	return filepath.Dir(path), challenge.Nonce
}

// crashedCopy is walStore after an unclean shutdown: the database and its log with frames, and no
// shared-memory index, beside the mirror and gate.
func crashedCopy(t *testing.T, live string) string {
	t.Helper()
	crashed := stateDir(t)
	for _, name := range []string{"relay.sqlite3", "relay.sqlite3-wal", "takeover.json", "write-gate.lock"} {
		raw, err := os.ReadFile(filepath.Join(live, name))
		must(t, err)
		must(t, os.WriteFile(filepath.Join(crashed, name), raw, 0o600))
	}
	if _, _, err := InPlaceRead(filepath.Join(crashed, "relay.sqlite3")); !errors.Is(err, ErrWALWithoutIndex) {
		t.Fatalf("the copy is not a log with frames and no index: %v", err)
	}
	return crashed
}

// The reads of a diagnostic create no sidecar wherever SQLite allows, and read what the log holds:
// beside a live writer (mode=ro on its sidecars), after an unclean shutdown (the plain read it
// has always been, which may build the index) and for a clean store (immutable). Each case reads a
// value committed only to the log through the Probe, ReadOnlyRows and NonceLookup, so a read that
// ignored the log fails here.
func TestSidecarFreeReadsStillReadTheLog(t *testing.T) {
	t.Parallel()
	ctx := WithSidecarFreeReads(t.Context())
	// Each read meets the store on its own: a read that built the index of an unclean shutdown
	// would otherwise leave the next one the ordinary live-store path instead of the fallback.
	reads := map[string]func(t *testing.T, sel StateSelection, nonce string){
		"NonceLookup": func(t *testing.T, sel StateSelection, nonce string) {
			if found := NonceLookup(ctx, sel, nonce); !found.Readable || !found.Found {
				t.Errorf("NonceLookup missed a value committed to the log: %+v", found)
			}
		},
		"ReadOnlyRows": func(t *testing.T, sel StateSelection, nonce string) {
			var rows []string
			read := ReadOnlyRows(ctx, sel, "SELECT nonce FROM store_challenge WHERE written_by='wal-only'", nil, func(r RowScanner) error {
				var n string
				if err := r.Scan(&n); err != nil {
					return err
				}
				rows = append(rows, n)
				return nil
			})
			if !read.Readable || read.Detail != "" || !slices.Equal(rows, []string{nonce}) {
				t.Errorf("ReadOnlyRows missed a row committed to the log: %+v %v", read, rows)
			}
		},
		"Probe": func(t *testing.T, sel StateSelection, _ string) {
			a := ProbeWith(ctx, sel, ProbeOptions{}).Access
			if !a.DBReadable || a.DBWritable || !strings.HasPrefix(a.Detail, judgedPrefix+"store_owned_by_other:") {
				t.Errorf("the Probe did not see the stamp committed to the log: %+v", a)
			}
		},
	}
	run := func(t *testing.T, dirFor func() string, nonce string, mayCreate []string) {
		for name, read := range reads {
			t.Run(name, func(t *testing.T) {
				dir := dirFor()
				watch := testsupport.WatchDir(t, dir)
				read(t, StateSelection{Path: dir}, nonce)
				for _, changed := range changedNames(watch.Drain()) {
					if !slices.Contains(mayCreate, changed) {
						t.Errorf("a read created, removed or moved %q", changed)
					}
				}
			})
		}
	}
	t.Run("beside a live writer", func(t *testing.T) {
		dir, nonce := walStore(t)
		run(t, func() string { return dir }, nonce, nil)
	})
	t.Run("after an unclean shutdown", func(t *testing.T) {
		live, nonce := walStore(t)
		run(t, func() string { return crashedCopy(t, live) }, nonce, []string{"relay.sqlite3-shm"})
	})
}

// A database reached through a link is written in the directory the link resolves to, which is
// where SQLite builds the log and the index: the judgement asks that directory, not the selected
// one, in both directions.
func TestProbeJudgesALinkedDatabaseByItsRealDirectory(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("a privileged process writes whatever the mode says")
	}
	linked := func(t *testing.T, realMode, selectedMode os.FileMode) string {
		t.Helper()
		real := closedStore(t)
		selected := stateDir(t)
		must(t, os.Symlink(filepath.Join(real, "relay.sqlite3"), filepath.Join(selected, "relay.sqlite3")))
		must(t, os.Chmod(real, realMode))
		must(t, os.Chmod(selected, selectedMode))
		t.Cleanup(func() { _ = os.Chmod(real, 0o700); _ = os.Chmod(selected, 0o700) })
		return selected
	}
	t.Run("the database's directory cannot be written", func(t *testing.T) {
		t.Parallel()
		a := Probe(t.Context(), StateSelection{Path: linked(t, 0o500, 0o700)}).Access
		if a.DBWritable || !a.DirectoryWritable || !strings.Contains(a.Detail, judgedPrefix+"the database's directory:") {
			t.Errorf("access %+v", a)
		}
	})
	t.Run("only the selected directory cannot be written", func(t *testing.T) {
		t.Parallel()
		a := Probe(t.Context(), StateSelection{Path: linked(t, 0o700, 0o500)}).Access
		if !a.DBWritable || a.DirectoryWritable {
			t.Errorf("access %+v", a)
		}
	})
}

// A clean store keeps every commit in the database file, so the read is immutable and creates
// nothing; the value it reads is the one the closed store checkpointed.
func TestSidecarFreeReadsOfACleanStoreCreateNothing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(stateDir(t), "relay.sqlite3")
	writer, err := fixtureOpen(t.Context(), path, "")
	must(t, err)
	challenge, err := writer.WriteChallengeFor(t.Context(), "clean")
	must(t, err)
	must(t, writer.Close())
	dir := filepath.Dir(path)
	if _, params, err := InPlaceRead(path); err != nil || params.Get("immutable") != "1" {
		t.Fatalf("a closed store is read immutable: %v %v", params, err)
	}
	watch := testsupport.WatchDir(t, dir)
	found := NonceLookup(WithSidecarFreeReads(t.Context()), StateSelection{Path: dir}, challenge.Nonce)
	if !found.Readable || !found.Found {
		t.Errorf("NonceLookup of a clean store: %+v", found)
	}
	if names := changedNames(watch.Drain()); len(names) != 0 {
		t.Errorf("an immutable read changed %v", names)
	}
	if entries, err := entryNames(dir); err != nil || !slices.Equal(entries, fencedStoreEntries) {
		t.Errorf("an immutable read left %v (%v)", entries, err)
	}
}
