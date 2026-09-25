package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// descriptorFixture is test_store.py DescriptorIdentity: our closed store in a/, and a whole
// second store in b/ with its own identity and challenge row, standing in for the database an
// actor swaps onto this pathname. Both are real stores, so reading the wrong one would SUCCEED.
type descriptorFixture struct {
	t              *testing.T
	tmp, a         string
	path           string
	interloperPath string
	written        string
	interloperOnce string
	mine           Location
}

func newDescriptorFixture(t *testing.T) *descriptorFixture {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "xdg"))
	f := &descriptorFixture{t: t, tmp: tmp, a: filepath.Join(tmp, "a")}
	f.path = filepath.Join(f.a, "relay.sqlite3")
	f.interloperPath = filepath.Join(tmp, "b", "relay.sqlite3")
	var theirs Location
	f.written, f.mine = f.makeStore(f.path, "parent")
	f.interloperOnce, theirs = f.makeStore(f.interloperPath, "interloper")
	if theirs.Inode == f.mine.Inode {
		t.Fatal("the interloper must be another file")
	}
	for _, dir := range []string{f.a, filepath.Dir(f.interloperPath)} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("a closed store left a sidecar behind in %s: %v %v", dir, entries, err)
		}
	}
	return f
}

func (f *descriptorFixture) makeStore(path, actor string) (string, Location) {
	f.t.Helper()
	s, err := Open(context.Background(), path, "")
	if err != nil {
		f.t.Fatal(err)
	}
	written, err := s.WriteChallengeFor(context.Background(), actor)
	if err != nil {
		f.t.Fatal(err)
	}
	loc, err := s.Locate(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		f.t.Fatal(err)
	}
	return written.Nonce, loc
}

func (f *descriptorFixture) selection() StateSelection { return StateSelection{Path: f.a} }

func (f *descriptorFixture) rename(from, to string) {
	f.t.Helper()
	if err := os.Rename(from, to); err != nil {
		f.t.Fatal(err)
	}
}

// swapAroundTheConnect stands the interloper at this pathname for the duration of the first
// real connect, then puts ours back: before and after, the name agrees; the connect saw B.
func (f *descriptorFixture) swapAroundTheConnect(fired *bool) context.Context {
	saved := filepath.Join(f.tmp, "saved.sqlite3")
	parked := filepath.Join(f.tmp, "parked.sqlite3")
	return context.WithValue(context.Background(), diagnosticSeamsKey{}, diagnosticSeams{connect: func(open func() (*heldConn, error)) (*heldConn, error) {
		if *fired {
			return open()
		}
		*fired = true
		f.rename(f.path, saved)
		f.rename(f.interloperPath, f.path)
		defer func() {
			f.rename(f.path, parked)
			f.rename(saved, f.path)
		}()
		return open()
	}})
}

// moveWhileHeld runs move right after the database's own descriptor opens (the os.open seam).
func (f *descriptorFixture) moveWhileHeld(fired *bool, move func()) context.Context {
	return context.WithValue(context.Background(), diagnosticSeamsKey{}, diagnosticSeams{afterOpen: func(path string) {
		if !*fired && path == f.path {
			*fired = true
			move()
		}
	}})
}

type betweenState struct {
	moved, restored bool
	statements      []string
}

// moveBetweenTheCheckAndTheConnect is PR115-RB1's window: a live writer holds the store with a
// write-ahead log, and on the nth connect our database is moved after the leg's last check and
// restored after the leg's first statement (or its close). Every statement is recorded.
func (f *descriptorFixture) moveBetweenTheCheckAndTheConnect(nth int) (context.Context, *betweenState) {
	f.t.Helper()
	writer, err := Open(context.Background(), f.path, "")
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = writer.Close() })
	if _, err := writer.WriteChallengeFor(context.Background(), "writer"); err != nil {
		f.t.Fatal(err)
	}
	moved := filepath.Join(f.tmp, "moved.sqlite3")
	state := &betweenState{}
	calls := 0
	restore := func() {
		if state.moved && !state.restored {
			f.rename(moved, f.path)
			state.restored = true
		}
	}
	f.t.Cleanup(restore)
	ctx := context.WithValue(context.Background(), diagnosticSeamsKey{}, diagnosticSeams{
		connect: func(open func() (*heldConn, error)) (*heldConn, error) {
			calls++
			if calls != nth {
				return open()
			}
			f.rename(f.path, moved)
			state.moved = true
			conn, err := open()
			if err != nil {
				restore()
			}
			return conn, err
		},
		statement: func(query string) {
			if state.moved && !state.restored {
				state.statements = append(state.statements, query)
			}
		},
		afterStatement: restore,
		closed:         restore,
	})
	return ctx, state
}

func (f *descriptorFixture) sidecarsOfTheMovedName() []string {
	entries, err := os.ReadDir(f.tmp)
	if err != nil {
		f.t.Fatal(err)
	}
	var found []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "moved.sqlite3-") {
			found = append(found, entry.Name())
		}
	}
	return found
}

func requireRefusedTheMove(t *testing.T, detail string) {
	t.Helper()
	if !strings.Contains(detail, "no longer the file at") && !strings.Contains(detail, "rather than the database at") {
		t.Fatalf("the refusal did not name the move: %q", detail)
	}
}

func TestDescriptorIdentity_python_properties(t *testing.T) {
	t.Run("test_rows_read_through_a_swap_are_never_attributed_to_this_store", func(t *testing.T) {
		f := newDescriptorFixture(t)
		fired := false
		answer := ReadChallengeRows(f.swapAroundTheConnect(&fired), f.selection())
		if !fired {
			t.Fatal("the seam never fired")
		}
		for _, row := range answer.Rows {
			if row.WrittenBy == "interloper" {
				t.Fatalf("interloper rows attributed to this store: %+v", answer)
			}
		}
		if len(answer.Rows) != 0 || answer.Device != 0 || answer.Detail == "" {
			t.Fatalf("answer %+v", answer)
		}
	})
	t.Run("test_a_nonce_read_through_a_swap_cannot_prove_the_measured_store", func(t *testing.T) {
		f := newDescriptorFixture(t)
		fired := false
		answer := NonceLookup(f.swapAroundTheConnect(&fired), f.selection(), f.interloperOnce)
		if !fired || answer.Found {
			t.Fatalf("fired %v answer %+v", fired, answer)
		}
		if got := CompareStore(f.mine, CompareExpectations{Inode: f.mine.PhysicalIdentity(), Nonce: &answer}); got.SameStore == Proven {
			t.Fatalf("graded %+v", got)
		}
	})
	t.Run("test_a_database_that_lost_its_name_while_held_is_refused", func(t *testing.T) {
		f := newDescriptorFixture(t)
		fired := false
		answer := ReadChallengeRows(f.moveWhileHeld(&fired, func() { f.rename(f.interloperPath, f.path) }), f.selection())
		if !fired || answer.Readable || len(answer.Rows) != 0 || answer.Device != 0 || !strings.Contains(answer.Detail, "no longer the file at") {
			t.Fatalf("fired %v answer %+v", fired, answer)
		}
	})
	t.Run("test_a_relocated_database_is_refused_before_it_can_leave_a_log_behind", func(t *testing.T) {
		f := newDescriptorFixture(t)
		fired := false
		moved := filepath.Join(f.tmp, "moved.sqlite3")
		answer := ReadChallengeRows(f.moveWhileHeld(&fired, func() { f.rename(f.path, moved) }), f.selection())
		if !fired || answer.Readable || !strings.Contains(answer.Detail, "no longer the file at") || len(f.sidecarsOfTheMovedName()) != 0 {
			t.Fatalf("fired %v answer %+v sidecars %v", fired, answer, f.sidecarsOfTheMovedName())
		}
	})
	t.Run("test_the_probe_re_asks_between_its_read_and_its_write", func(t *testing.T) {
		f := newDescriptorFixture(t)
		moved := filepath.Join(f.tmp, "moved.sqlite3")
		fired := false
		ctx := context.WithValue(context.Background(), diagnosticSeamsKey{}, diagnosticSeams{connect: func(open func() (*heldConn, error)) (*heldConn, error) {
			conn, err := open()
			if !fired {
				fired = true
				f.rename(f.path, moved)
			}
			return conn, err
		}})
		report := Probe(ctx, f.selection())
		t.Cleanup(func() { f.rename(moved, f.path) })
		if !fired || report.Access.DBWritable || report.Access.DBReadable || report.Store.StoreID != "" || report.Store.Inode != 0 {
			t.Fatalf("fired %v report %+v", fired, report)
		}
		if !strings.Contains(report.Access.Detail, "moved while it was being read") || len(f.sidecarsOfTheMovedName()) != 0 {
			t.Fatalf("detail %q sidecars %v", report.Access.Detail, f.sidecarsOfTheMovedName())
		}
	})
	t.Run("test_the_write_probe_asks_which_file_it_opened_before_it_can_write", func(t *testing.T) {
		f := newDescriptorFixture(t)
		ctx, state := f.moveBetweenTheCheckAndTheConnect(2)
		report := Probe(ctx, f.selection())
		if !state.restored || len(f.sidecarsOfTheMovedName()) != 0 || len(state.statements) != 0 || report.Access.DBWritable {
			t.Fatalf("state %+v sidecars %v report %+v", state, f.sidecarsOfTheMovedName(), report)
		}
		requireRefusedTheMove(t, report.Access.Detail)
	})
	t.Run("test_the_probe_read_leg_asks_before_its_first_select", func(t *testing.T) {
		f := newDescriptorFixture(t)
		ctx, state := f.moveBetweenTheCheckAndTheConnect(1)
		report := Probe(ctx, f.selection())
		if !state.restored || len(f.sidecarsOfTheMovedName()) != 0 || len(state.statements) != 0 || report.Access.DBReadable || report.Store.StoreID != "" {
			t.Fatalf("state %+v sidecars %v report %+v", state, f.sidecarsOfTheMovedName(), report)
		}
		requireRefusedTheMove(t, report.Access.Detail)
	})
	t.Run("test_read_only_rows_asks_before_the_callers_statement", func(t *testing.T) {
		f := newDescriptorFixture(t)
		ctx, state := f.moveBetweenTheCheckAndTheConnect(1)
		answer := ReadChallengeRows(ctx, f.selection())
		if !state.restored || len(f.sidecarsOfTheMovedName()) != 0 || len(state.statements) != 0 || answer.Readable || len(answer.Rows) != 0 {
			t.Fatalf("state %+v answer %+v", state, answer)
		}
		requireRefusedTheMove(t, answer.Detail)
	})
	t.Run("test_nonce_lookup_asks_before_it_looks", func(t *testing.T) {
		f := newDescriptorFixture(t)
		ctx, state := f.moveBetweenTheCheckAndTheConnect(1)
		answer := NonceLookup(ctx, f.selection(), f.written)
		if !state.restored || len(f.sidecarsOfTheMovedName()) != 0 || len(state.statements) != 0 || answer.Found || answer.Readable {
			t.Fatalf("state %+v answer %+v", state, answer)
		}
		requireRefusedTheMove(t, answer.Detail)
	})
	t.Run("test_the_ordinary_probe_describes_the_file_it_held", func(t *testing.T) {
		f := newDescriptorFixture(t)
		report := Probe(context.Background(), f.selection())
		store := report.Store
		real, err := filepath.EvalSymlinks(f.path)
		if err != nil {
			t.Fatal(err)
		}
		if !report.Access.DBReadable || !report.Access.DBWritable || report.Access.Detail != "" || store.RealPath != real || store.Device != f.mine.Device || store.Inode != f.mine.Inode || store.StoreID != f.mine.StoreID {
			t.Fatalf("report %+v mine %+v", report, f.mine)
		}
		if entries, err := os.ReadDir(f.a); err != nil || len(entries) != 1 {
			t.Fatalf("diagnosis left a write-ahead log behind: %v %v", entries, err)
		}
	})
	t.Run("test_a_store_reached_through_a_symlinked_directory_still_reads", func(t *testing.T) {
		f := newDescriptorFixture(t)
		alias := filepath.Join(f.tmp, "alias")
		if err := os.Symlink(f.a, alias); err != nil {
			t.Fatal(err)
		}
		answer := NonceLookup(context.Background(), StateSelection{Path: alias}, f.written)
		if !answer.Found || answer.Device != f.mine.Device || answer.Inode != f.mine.Inode {
			t.Fatalf("alias %+v", answer)
		}
	})
	t.Run("test_the_descriptor_is_released_on_every_path", func(t *testing.T) {
		f := newDescriptorFixture(t)
		held := func() int {
			entries, err := os.ReadDir("/proc/self/fd")
			if err != nil {
				t.Fatal(err)
			}
			return len(entries)
		}
		before := held()
		// The refusal path: the descriptor is opened and then refused as moved, which is the one
		// branch that returns without ever reaching a connection.
		refusing := context.WithValue(context.Background(), diagnosticSeamsKey{}, diagnosticSeams{afterOpen: func(string) {
			f.rename(f.path, f.path+".away")
		}})
		restore := func() {
			if _, err := os.Stat(f.path + ".away"); err == nil {
				f.rename(f.path+".away", f.path)
			}
		}
		broken := filepath.Join(f.tmp, "broken")
		if err := os.MkdirAll(broken, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(broken, "relay.sqlite3"), []byte("not a database"), 0o600); err != nil {
			t.Fatal(err)
		}
		for range 25 {
			_ = ReadChallengeRows(context.Background(), f.selection())
			_ = ReadChallengeRows(context.Background(), StateSelection{Path: broken})
			_ = NonceLookup(context.Background(), f.selection(), "absent")
			_ = ReadChallengeRows(context.Background(), StateSelection{Path: filepath.Join(f.tmp, "nothing-here")})
			if got := ReadChallengeRows(refusing, f.selection()); got.Readable {
				t.Fatalf("the refusal path was not taken: %+v", got)
			}
			restore()
			if got := NonceLookup(refusing, f.selection(), "absent"); got.Readable {
				t.Fatalf("the refusal path was not taken: %+v", got)
			}
			restore()
		}
		if after := held(); after > before+1 {
			t.Fatalf("a descriptor was not released: %d -> %d", before, after)
		}
	})
	t.Run("test_a_missing_store_is_answered_rather_than_raised", func(t *testing.T) {
		f := newDescriptorFixture(t)
		answer := ReadChallengeRows(context.Background(), StateSelection{Path: filepath.Join(f.tmp, "nothing-here")})
		if answer.Readable || len(answer.Rows) != 0 || answer.Device != 0 || answer.Detail == "" {
			t.Fatalf("answer %+v", answer)
		}
	})
	t.Run("test_a_held_database_that_cannot_be_identified_is_not_described", func(t *testing.T) {
		f := newDescriptorFixture(t)
		ctx := context.WithValue(context.Background(), diagnosticSeamsKey{}, diagnosticSeams{identity: func(int) (heldIdentity, bool) { return heldIdentity{}, false }})
		report := Probe(ctx, f.selection())
		s := report.Store
		if !report.Access.DBExists || s.Device != 0 || s.Inode != 0 || s.Links != 0 || s.StoreID != "" || report.Access.DBReadable || report.Access.DBWritable || !strings.Contains(report.Access.Detail, "could not be identified") {
			t.Fatalf("report %+v", report)
		}
	})
	t.Run("test_a_refused_hold_states_no_identity_at_all", func(t *testing.T) {
		f := newDescriptorFixture(t)
		ctx := context.WithValue(context.Background(), diagnosticSeamsKey{}, diagnosticSeams{hold: func(string) (*os.File, string, string) { return nil, "", "refused for the test" }})
		report := Probe(ctx, f.selection())
		s := report.Store
		if !report.Access.DBExists || s.Device != 0 || s.Inode != 0 || s.Links != 0 || report.Access.DBReadable {
			t.Fatalf("report %+v", report)
		}
		requireVerdict(t, CompareStore(s, CompareExpectations{Inode: f.mine.PhysicalIdentity()}), Unproven)
		if _, err := os.Stat(filepath.Join(f.tmp, "moved.sqlite3")); !os.IsNotExist(err) {
			t.Fatalf("moved file exists: %v", err)
		}
	})
	t.Run("test_a_relocated_store_is_unproven_rather_than_a_different_store", func(t *testing.T) {
		f := newDescriptorFixture(t)
		moved := filepath.Join(f.tmp, "moved.sqlite3")
		fired := false
		report := Probe(f.moveWhileHeld(&fired, func() { f.rename(f.path, moved) }), f.selection())
		t.Cleanup(func() { f.rename(moved, f.path) })
		s := report.Store
		if !fired || !report.Access.DBExists || !strings.Contains(report.Access.Detail, "could not be held open") || s.Device != 0 || s.Inode != 0 || s.Links != 0 || report.Access.DBReadable {
			t.Fatalf("fired %v report %+v", fired, report)
		}
		requireVerdict(t, CompareStore(s, CompareExpectations{Inode: f.mine.PhysicalIdentity()}), Unproven)
	})
	t.Run("a_move_and_return_inside_the_connect_is_named_by_sqlite", func(t *testing.T) {
		// _opened_elsewhere's case: the readlinks on both sides of the connect see this store,
		// and only SQLite's own account of the file it opened can refuse it.
		f := newDescriptorFixture(t)
		moved := filepath.Join(f.tmp, "moved.sqlite3")
		fired := false
		ctx := context.WithValue(context.Background(), diagnosticSeamsKey{}, diagnosticSeams{connect: func(open func() (*heldConn, error)) (*heldConn, error) {
			if fired {
				return open()
			}
			fired = true
			f.rename(f.path, moved)
			defer f.rename(moved, f.path)
			conn, err := open()
			if err == nil {
				// Force the lazy open to happen while the store is away.
				err = conn.conn.PingContext(context.Background())
			}
			return conn, err
		}})
		answer := NonceLookup(ctx, f.selection(), f.written)
		if !fired || answer.Found || answer.Readable {
			t.Fatalf("fired %v answer %+v", fired, answer)
		}
		requireRefusedTheMove(t, answer.Detail)
	})
	moveAfterConnect := func(f *descriptorFixture, fired *bool) context.Context {
		moved := filepath.Join(f.tmp, "moved.sqlite3")
		f.t.Cleanup(func() {
			if *fired {
				f.rename(moved, f.path)
			}
		})
		return context.WithValue(context.Background(), diagnosticSeamsKey{}, diagnosticSeams{connect: func(open func() (*heldConn, error)) (*heldConn, error) {
			conn, err := open()
			if !*fired {
				*fired = true
				f.rename(f.path, moved)
			}
			return conn, err
		}})
	}
	t.Run("test_a_store_that_moves_during_the_read_withdraws_the_answer", func(t *testing.T) {
		f := newDescriptorFixture(t)
		fired := false
		answer := ReadChallengeRows(moveAfterConnect(f, &fired), f.selection())
		if !fired || answer.Readable || len(answer.Rows) != 0 || answer.Device != 0 {
			t.Fatalf("fired %v answer %+v", fired, answer)
		}
		requireRefusedTheMove(t, answer.Detail)
	})
	t.Run("test_a_nonce_read_while_the_store_moves_is_not_proof", func(t *testing.T) {
		f := newDescriptorFixture(t)
		fired := false
		answer := NonceLookup(moveAfterConnect(f, &fired), f.selection(), f.written)
		if !fired || answer.Readable || answer.Found {
			t.Fatalf("fired %v answer %+v", fired, answer)
		}
		if got := CompareStore(f.mine, CompareExpectations{Inode: f.mine.PhysicalIdentity(), Nonce: &answer}); got.SameStore == Proven {
			t.Fatalf("graded %+v", got)
		}
	})
}

func TestReadChallengeRows_refuses_when_store_moves_after_the_statement(t *testing.T) {
	// Given: a store that is renamed only after the read's connection closed, so the query and
	// every earlier binding check succeed against the file still at this pathname.
	f := newDescriptorFixture(t)
	moved := filepath.Join(f.tmp, "moved.sqlite3")
	fired := false
	t.Cleanup(func() {
		if fired {
			f.rename(moved, f.path)
		}
	})
	ctx := context.WithValue(context.Background(), diagnosticSeamsKey{}, diagnosticSeams{closed: func() {
		if !fired {
			fired = true
			f.rename(f.path, moved)
		}
	}})
	// When: the challenge rows are read.
	answer := ReadChallengeRows(ctx, f.selection())
	// Then: only the closing relocation check can withdraw the rows and identity.
	if !fired || answer.Readable || len(answer.Rows) != 0 || answer.Inode != 0 || !strings.Contains(answer.Detail, "no longer the file at") {
		t.Fatalf("fired %v answer %+v", fired, answer)
	}
}
