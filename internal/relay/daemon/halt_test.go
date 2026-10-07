package daemon

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The daemon's half of CRW-848: a pass that meets the corrupting class marks the store and ends
// without writing, and a pass that starts with the marker present writes nothing at all.

// damageStorePages overwrites every page of the store after the first, which the store's next
// statement meets as SQLITE_CORRUPT (measured: code 11).
func damageStorePages(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	garbage := make([]byte, info.Size()-4096)
	for i := range garbage {
		garbage[i] = 0xCD
	}
	if _, err = f.WriteAt(garbage, 4096); err != nil {
		t.Fatal(err)
	}
}

// TestHaltDaemonMarksACorruptingObservation: the daemon's own read of the store (the observation
// pass's census) meets the class, so the pass publishes the marker with the observation site and
// ends without an error, which is what keeps the run ticking and write-free.
func TestHaltDaemonMarksACorruptingObservation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	s, err := fixtureStore(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	damageStorePages(t, path)
	d := New(s, &observationHost{status: "completed"}, &delivery.FakeClock{T: 1700000000}, nil)
	r, err := d.Tick(ctx)
	if err != nil {
		t.Fatalf("the pass returned an error instead of halting: %v", err)
	}
	if len(r.Notes) == 0 {
		t.Fatalf("the pass recorded no note: %+v", r)
	}
	state := store.HaltStateAt(path)
	if !state.Present || state.Detail != "" {
		t.Fatalf("no marker was published: %+v", state)
	}
	if state.Marker.Code != 11 || state.Marker.Site != store.HaltSiteObservation {
		t.Fatalf("marker %+v", state.Marker)
	}
	// The next pass is write-free because the marker is checked first: it attempts no statement at
	// all, so it neither fails again nor publishes a second detection.
	if _, err = d.Tick(ctx); err != nil {
		t.Fatalf("the pass after the halt errored: %v", err)
	}
	if again := store.HaltStateAt(path); again.Marker.Sequence != 1 {
		t.Fatalf("a pass after the halt attempted another write: %+v", again.Marker)
	}
}

// TestHaltDaemonWritesNothingWhileTheMarkerExists: with the marker present a pass does nothing at
// all - no host read, no settlement, no delivery, no journal row - and says so.
func TestHaltDaemonWritesNothingWhileTheMarkerExists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := fixtureStore(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	seed(t, s, "r", "parent", "child", "anchor")
	before := journalRows(t, s)
	if err = store.RecordHalt(ctx, filepath.Join(dir, "relay.sqlite3"), store.CorruptingCause{Code: 522, Message: "disk I/O error", Site: store.HaltSiteObservation}); err != nil {
		t.Fatal(err)
	}
	host := &observationHost{status: "completed"}
	d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
	r, err := d.Tick(ctx)
	if err != nil {
		t.Fatalf("the pass errored: %v", err)
	}
	if len(r.Notes) == 0 {
		t.Fatalf("the pass recorded no note: %+v", r)
	}
	if after := journalRows(t, s); after != before {
		t.Fatalf("the pass wrote %d journal rows while the marker exists", after-before)
	}
	var polls int
	if err = s.DB.QueryRow("SELECT COUNT(*) FROM poll_observations").Scan(&polls); err != nil {
		t.Fatal(err)
	}
	if polls != 0 {
		t.Fatalf("the pass wrote %d poll observations while the marker exists", polls)
	}
	if len(host.reads) != 0 {
		t.Fatalf("the pass read the host while the marker exists: %v", host.reads)
	}
}

// TestHaltDaemonWriteSiteIsRecorded: a write that meets the class is recorded with the write site.
// The failure is a real one from a damaged store, handed to the daemon's own classifier, because a
// temporary store cannot be damaged so that the observation pass reads cleanly and only the
// settlement write fails.
func TestHaltDaemonWriteSiteIsRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	s, err := fixtureStore(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	damageStorePages(t, path)
	d := New(s, &observationHost{status: "completed"}, &delivery.FakeClock{T: 1700000000}, nil)
	r := Report{Notes: []string{}}
	if !d.halted(ctx, &r, store.HaltSiteWrite, damageStoreWrite(t, s)) {
		t.Fatal("the write site was not classified")
	}
	state := store.HaltStateAt(path)
	if !state.Present || state.Detail != "" || state.Marker.Site != store.HaltSiteWrite || state.Marker.Code != 11 {
		t.Fatalf("marker %+v", state)
	}
	if len(r.Notes) == 0 {
		t.Fatalf("no note: %+v", r)
	}
}

// damageStoreWrite is the failure a write into the damaged store meets: a real *sqlite.Error of the
// corrupting class.
func damageStoreWrite(t *testing.T, s *store.Store) error {
	t.Helper()
	err := s.Transaction(context.Background(), func(ctx context.Context, conn *sql.Conn) error {
		_, e := conn.ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES('t','k','s','d')")
		return e
	})
	if err == nil {
		t.Fatal("the damaged store's write did not fail")
	}
	return err
}

// journalRows is the store's journal row count.
func journalRows(t *testing.T, s *store.Store) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM journal").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestHaltDaemonIgnoresAnotherFailure: a failure that is not the class is left to the caller
// exactly as it was, and nothing is marked.
func TestHaltDaemonIgnoresAnotherFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	s, err := fixtureStore(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	d := New(s, &observationHost{status: "completed"}, &delivery.FakeClock{T: 1700000000}, nil)
	r := Report{Notes: []string{}}
	if d.halted(ctx, &r, store.HaltSiteWrite, errors.New("boom")) {
		t.Fatal("an ordinary failure was read as the class")
	}
	if state := store.HaltStateAt(path); state.Present {
		t.Fatalf("a marker was written for an ordinary failure: %+v", state)
	}
}
