package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// The halt marker's tests (CRW-848). Every store here is a temporary one; the damage a test
// inflicts is the damage the 2026-10-06 incident had (every page after the first overwritten),
// which an open store's next statement meets as SQLITE_CORRUPT with the result code 11.

// haltFixture opens a fresh store at dir/relay.sqlite3 and returns it with its directory.
func haltFixture(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(context.Background(), filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

// damageStorePages overwrites every page of the store after the first, leaving the header (and so
// the schema read) intact and the table pages garbage.
func damageStorePages(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= 4096 {
		t.Fatalf("the store is too small to damage: %d bytes", info.Size())
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

// writeJournal is one statement the relay issues to write.
func writeJournal(t *testing.T, s *Store) error {
	t.Helper()
	return s.Transaction(context.Background(), func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES('t','k','s','d')")
		return err
	})
}

func journalRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM journal").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestHaltRefusalNameIsRegistered: the one new refusal name is in the frozen registry, the
// generated code carries it, the store's constant is that name, and the refusal exits 2.
func TestHaltRefusalNameIsRegistered(t *testing.T) {
	t.Parallel()
	if contract.RefusalStoreWriteHalted != "store_write_halted" {
		t.Fatalf("the generated code carries %q", contract.RefusalStoreWriteHalted)
	}
	if ReasonStoreWriteHalted != string(contract.RefusalStoreWriteHalted) {
		t.Fatalf("the store's constant is %q", ReasonStoreWriteHalted)
	}
	if contract.ExitRefused != 2 {
		t.Fatalf("a refusal exits %d", contract.ExitRefused)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contract", "schema", "relay-exit-codes.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		RefusalReasons map[string]string `json:"refusalReasons"`
	}
	if err = json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.RefusalReasons["STORE_WRITE_HALTED"] != "store_write_halted" {
		t.Fatalf("the registry does not hold the name: %v", schema.RefusalReasons["STORE_WRITE_HALTED"])
	}
}

// TestCorruptingFailureClass is the detection set: a real SQLite failure is of the class when its
// primary result code is SQLITE_CORRUPT (11, every extended code of it) or SQLITE_NOTADB (26), or
// when the code is SQLITE_IOERR_SHORT_READ (522). The codes come from the driver's own errors,
// never from a constructed one, because sqlite.Error's fields are unexported.
func TestCorruptingFailureClass(t *testing.T) {
	t.Parallel()
	t.Run("a damaged store's write is the class", func(t *testing.T) {
		s, dir := haltFixture(t)
		damageStorePages(t, filepath.Join(dir, "relay.sqlite3"))
		err := writeJournal(t, s)
		cause, ok := CorruptingFailure(err)
		if !ok {
			t.Fatalf("a corrupt page was not the class: %v", err)
		}
		if cause.Code != 11 || cause.Message != "database disk image is malformed" {
			t.Fatalf("cause %+v", cause)
		}
	})
	t.Run("a file that is not a database is the class", func(t *testing.T) {
		s, dir := haltFixture(t)
		path := filepath.Join(dir, "relay.sqlite3")
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("not a database at all"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Open(context.Background(), path, "")
		cause, ok := CorruptingFailure(err)
		if !ok || cause.Code != 26 {
			t.Fatalf("a file that is not a database: %+v %v", cause, err)
		}
	})
	t.Run("a constraint violation is not the class", func(t *testing.T) {
		s, _ := haltFixture(t)
		_, err := s.DB.Exec("INSERT INTO schema_meta(key,value) VALUES('writer_protocol','again')")
		if _, ok := CorruptingFailure(err); ok {
			t.Fatalf("a primary key conflict was read as the class: %v", err)
		}
	})
	t.Run("a plain failure is not the class", func(t *testing.T) {
		if _, ok := CorruptingFailure(errors.New("nope")); ok {
			t.Fatal("an ordinary error was read as the class")
		}
	})
}

// TestCorruptingDetailClass is the same class rule for a caller that holds only the failure's text,
// which is what the observation path carries (the driver appends " (N)" to SQLite's own message).
// The extended corruption codes and the IOERR short read are here because a temporary store cannot
// be made to fail with them on demand.
func TestCorruptingDetailClass(t *testing.T) {
	t.Parallel()
	for _, one := range []struct {
		detail string
		code   int
		ok     bool
	}{
		{"database disk image is malformed (11)", 11, true},
		{"database disk image is malformed (267)", 267, true},
		{"database disk image is malformed (779)", 779, true},
		{"file is not a database (26)", 26, true},
		{"disk I/O error (522)", 522, true},
		{"database disk image is malformed", 0, false},
		{"disk I/O error (10)", 0, false},
		{"database is locked (5)", 0, false},
		{"attempt to write a readonly database (8)", 0, false},
		{"UNIQUE constraint failed: x.y (2067)", 0, false},
		{"no such table: journal (1)", 0, false},
		{"", 0, false},
	} {
		cause, ok := CorruptingDetail(one.detail)
		if ok != one.ok || (ok && cause.Code != one.code) {
			t.Errorf("%q: %+v %v, want code %d ok %v", one.detail, cause, ok, one.code, one.ok)
		}
		if ok && strings.Contains(cause.Message, "(") {
			t.Errorf("%q: the message kept the driver's suffix: %q", one.detail, cause.Message)
		}
	}
}

// TestHaltMarkerRoundTrip is the writer and the reader: the marker holds the detection time, this
// process, the failure, the site and its own sequence, and it is published atomically (no temporary
// file is left beside it).
func TestHaltMarkerRoundTrip(t *testing.T) {
	t.Parallel()
	_, dir := haltFixture(t)
	cause := CorruptingCause{Code: 522, Message: "disk I/O error", Site: HaltSiteObservation}
	if err := RecordHalt(context.Background(), filepath.Join(dir, "relay.sqlite3"), cause); err != nil {
		t.Fatal(err)
	}
	state := HaltStateAt(filepath.Join(dir, "relay.sqlite3"))
	if !state.Present || state.Detail != "" {
		t.Fatalf("the marker did not read back: %+v", state)
	}
	marker := state.Marker
	if marker.Code != 522 || marker.Message != "disk I/O error" || marker.Site != HaltSiteObservation || marker.Sequence != 1 {
		t.Fatalf("marker %+v", marker)
	}
	if marker.PID != os.Getpid() || marker.Command == "" {
		t.Fatalf("the marker does not name this process: %+v", marker)
	}
	if _, err := time.Parse(time.RFC3339Nano, marker.DetectedAt); err != nil {
		t.Fatalf("detectedAt %q: %v", marker.DetectedAt, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".corruption-") {
			t.Fatalf("a temporary marker file was left behind: %s", entry.Name())
		}
	}
}

// TestHaltRecordReplacesTheWholeMarker: a later detection rewrites the marker whole, with the next
// sequence and none of the earlier record carried over.
func TestHaltRecordReplacesTheWholeMarker(t *testing.T) {
	t.Parallel()
	_, dir := haltFixture(t)
	dbPath := filepath.Join(dir, "relay.sqlite3")
	if err := RecordHalt(context.Background(), dbPath, CorruptingCause{Code: 11, Message: "malformed", Site: HaltSiteWrite}); err != nil {
		t.Fatal(err)
	}
	if err := RecordHalt(context.Background(), dbPath, CorruptingCause{Code: 522, Message: "short read", Site: HaltSiteObservation}); err != nil {
		t.Fatal(err)
	}
	marker := HaltStateAt(dbPath).Marker
	if marker.Sequence != 2 || marker.Code != 522 || marker.Message != "short read" || marker.Site != HaltSiteObservation {
		t.Fatalf("marker %+v", marker)
	}
}

// TestHaltFailClosedOnAnUnreadableMarker: a marker nobody can read is a halt, never a healthy
// store, and the refusal says so.
func TestHaltFailClosedOnAnUnreadableMarker(t *testing.T) {
	t.Parallel()
	_, dir := haltFixture(t)
	dbPath := filepath.Join(dir, "relay.sqlite3")
	if err := os.WriteFile(filepath.Join(dir, HaltMarkerName), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := HaltStateAt(dbPath)
	if !state.Present || state.Detail == "" {
		t.Fatalf("an unreadable marker did not halt: %+v", state)
	}
	refusal := HaltRefusal(state)
	if RefusalReason(refusal) != ReasonStoreWriteHalted || !strings.Contains(refusal.Error(), state.Path) {
		t.Fatalf("refusal %v", refusal)
	}
	// A detection recorded over an unreadable marker still publishes a decodable one; its sequence
	// cannot be counted from a marker that does not decode, so it starts again.
	if err := RecordHalt(context.Background(), dbPath, CorruptingCause{Code: 11, Message: "malformed", Site: HaltSiteWrite}); err != nil {
		t.Fatal(err)
	}
	if state = HaltStateAt(dbPath); state.Detail != "" || state.Marker.Sequence != 1 {
		t.Fatalf("the marker was not republished whole: %+v", state)
	}
}

// TestHaltRefusalNamesTheMarkerAndTheDetection: the refusal's detail carries the marker's path and
// what it records, which is what an operator reads.
func TestHaltRefusalNamesTheMarkerAndTheDetection(t *testing.T) {
	t.Parallel()
	_, dir := haltFixture(t)
	dbPath := filepath.Join(dir, "relay.sqlite3")
	if err := RecordHalt(context.Background(), dbPath, CorruptingCause{Code: 779, Message: "malformed", Site: HaltSiteObservation}); err != nil {
		t.Fatal(err)
	}
	state := HaltStateAt(dbPath)
	refusal := HaltRefusal(state)
	if RefusalReason(refusal) != "store_write_halted" {
		t.Fatalf("reason %q", RefusalReason(refusal))
	}
	for _, want := range []string{state.Path, "779", "malformed", HaltSiteObservation} {
		if !strings.Contains(refusal.Error(), want) {
			t.Errorf("the detail does not name %q: %v", want, refusal)
		}
	}
}

// TestHaltRecordsNothingOnACancelledContext: the marker is a durable effect, so a pass that was told
// to stop publishes none.
func TestHaltRecordsNothingOnACancelledContext(t *testing.T) {
	t.Parallel()
	_, dir := haltFixture(t)
	dbPath := filepath.Join(dir, "relay.sqlite3")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RecordHalt(ctx, dbPath, CorruptingCause{Code: 11, Message: "malformed", Site: HaltSiteWrite}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if state := HaltStateAt(dbPath); state.Present {
		t.Fatalf("a cancelled pass published the marker: %+v", state)
	}
}

// TestHaltPublishIsAtomic: at every step of the publication the marker is either the whole previous
// record or the whole new one, and the temporary file is never visible under the marker's name.
func TestHaltPublishIsAtomic(t *testing.T) {
	for _, point := range []string{"temp-written", "file-synced", "renamed", "directory-synced"} {
		t.Run(point, func(t *testing.T) {
			_, dir := haltFixture(t)
			dbPath := filepath.Join(dir, "relay.sqlite3")
			if err := RecordHalt(context.Background(), dbPath, CorruptingCause{Code: 11, Message: "first", Site: HaltSiteWrite}); err != nil {
				t.Fatal(err)
			}
			boom := errors.New("stop at " + point)
			haltFault = func(at string) error {
				if at == point {
					return boom
				}
				return nil
			}
			defer func() { haltFault = nil }()
			err := RecordHalt(context.Background(), dbPath, CorruptingCause{Code: 522, Message: "second", Site: HaltSiteObservation})
			if err == nil {
				t.Fatalf("the fault at %s did not stop the publication", point)
			}
			state := HaltStateAt(dbPath)
			if state.Detail != "" {
				t.Fatalf("a torn marker is visible: %+v", state)
			}
			if point == "renamed" || point == "directory-synced" {
				if state.Marker.Message != "second" || state.Marker.Sequence != 2 {
					t.Fatalf("the rename did not stand: %+v", state.Marker)
				}
			} else if state.Marker.Message != "first" || state.Marker.Sequence != 1 {
				t.Fatalf("the previous marker did not stand: %+v", state.Marker)
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".corruption-") {
					t.Fatalf("a temporary file was left behind: %s", entry.Name())
				}
			}
		})
	}
}

// TestHaltMarkerSitsBesideTheResolvedStore: the marker is named through the loosely resolved path,
// so a state directory reached through a symbolic link reads and writes the marker the operator's
// other commands see, beside takeover.json.
func TestHaltMarkerSitsBesideTheResolvedStore(t *testing.T) {
	t.Parallel()
	_, dir := haltFixture(t)
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := RecordHalt(context.Background(), filepath.Join(link, "relay.sqlite3"), CorruptingCause{Code: 11, Message: "malformed", Site: HaltSiteWrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, HaltMarkerName)); err != nil {
		t.Fatalf("the marker is not beside the resolved store: %v", err)
	}
	if state := HaltStateAt(filepath.Join(link, "relay.sqlite3")); !state.Present {
		t.Fatalf("the alias does not read the marker: %+v", state)
	}
}

// TestHaltMarkerCommandSurvivesHostileArgv: the recorded command line is arbitrary bytes; the
// marker stays a decodable JSON document whatever the process was invoked with.
func TestHaltMarkerCommandSurvivesHostileArgv(t *testing.T) {
	previous := haltCommand
	haltCommand = func() string { return "crw relay " + string([]byte{0xff, 0xfe}) + " --state" }
	defer func() { haltCommand = previous }()
	_, dir := haltFixture(t)
	dbPath := filepath.Join(dir, "relay.sqlite3")
	if err := RecordHalt(context.Background(), dbPath, CorruptingCause{Code: 11, Message: "malformed", Site: HaltSiteWrite}); err != nil {
		t.Fatal(err)
	}
	state := HaltStateAt(dbPath)
	if !state.Present || state.Detail != "" {
		t.Fatalf("the marker with a hostile command did not decode: %+v", state)
	}
	if !strings.ContainsRune(state.Marker.Command, '�') {
		t.Fatalf("the hostile bytes were not replaced as JSON replaces them: %q", state.Marker.Command)
	}
}

// TestHaltRefusesAWriteAdmittedBeforeTheMarker: the marker is read inside the writer lock, on the
// opened store, so a marker that appeared between the preflight and the write is still found, and
// the refused transaction writes nothing.
func TestHaltRefusesAWriteAdmittedBeforeTheMarker(t *testing.T) {
	t.Parallel()
	s, dir := haltFixture(t)
	dbPath := filepath.Join(dir, "relay.sqlite3")
	before := journalRows(t, s)
	if err := RecordHalt(context.Background(), dbPath, CorruptingCause{Code: 11, Message: "malformed", Site: HaltSiteWrite}); err != nil {
		t.Fatal(err)
	}
	err := writeJournal(t, s)
	if RefusalReason(err) != ReasonStoreWriteHalted {
		t.Fatalf("a write was admitted while the marker exists: %v", err)
	}
	if after := journalRows(t, s); after != before {
		t.Fatalf("the refused transaction wrote %d rows", after-before)
	}
}

// TestHaltIgnoresANonCorruptingFailure: only the class halts. A store that fails for another reason
// is left exactly as it was.
func TestHaltIgnoresANonCorruptingFailure(t *testing.T) {
	t.Parallel()
	s, dir := haltFixture(t)
	err := s.Transaction(context.Background(), func(ctx context.Context, conn *sql.Conn) error {
		_, e := conn.ExecContext(ctx, "INSERT INTO schema_meta(key,value) VALUES('writer_protocol','again')")
		return e
	})
	if err == nil {
		t.Fatal("the conflicting insert did not fail")
	}
	if RefusalReason(err) == ReasonStoreWriteHalted {
		t.Fatalf("a constraint violation halted the store: %v", err)
	}
	if state := HaltStateAt(filepath.Join(dir, "relay.sqlite3")); state.Present {
		t.Fatalf("a marker was written for a non-corrupting failure: %+v", state)
	}
}

// TestHaltRedactsTheRecordedCommand: the marker records the process identity and never the
// arguments, because a relay command line can carry a bearer token.
func TestHaltRedactsTheRecordedCommand(t *testing.T) {
	t.Parallel()
	for _, one := range []struct {
		argv []string
		want string
	}{
		{[]string{"crw", "relay", "--state", "/s", "daemon"}, "crw relay daemon"},
		{[]string{"crw", "relay", "session", "bind", "--claim-token", "sekrit"}, "crw relay session bind"},
		{[]string{"crw", "relay", "sync-target", "--relationship=r-1", "--token=sekrit"}, "crw relay sync-target"},
		{[]string{"crw", "relay", "--", "--claim-token", "sekrit"}, "crw relay"},
		{[]string{"crw"}, "crw"},
	} {
		if got := haltCommandWords(one.argv, haltCommandLimit); got != one.want {
			t.Errorf("%v: %q, want %q", one.argv, got, one.want)
		}
	}
	long := haltCommandWords([]string{"crw", strings.Repeat("x", 4*haltCommandLimit)}, haltCommandLimit)
	if len(long) != haltCommandLimit {
		t.Fatalf("the recorded identity is not bounded: %d bytes", len(long))
	}
}

// TestHaltRefusesAnAutocommitWrite: a statement a writable store issues outside a transaction takes
// no writer lock, so it is refused by the querier itself; reads still answer.
func TestHaltRefusesAnAutocommitWrite(t *testing.T) {
	t.Parallel()
	s, dir := haltFixture(t)
	before := journalRows(t, s)
	if err := RecordHalt(context.Background(), filepath.Join(dir, "relay.sqlite3"), CorruptingCause{Code: 11, Message: "malformed", Site: HaltSiteWrite}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Querier(context.Background()).ExecContext(context.Background(), "INSERT INTO journal(at,kind,subject,detail) VALUES('t','k','s','d')")
	if RefusalReason(err) != ReasonStoreWriteHalted {
		t.Fatalf("an autocommit write was admitted: %v", err)
	}
	if after := journalRows(t, s); after != before {
		t.Fatalf("the refused statement wrote %d rows", after-before)
	}
	rows, err := s.All(context.Background(), "SELECT COUNT(*) FROM journal")
	if err != nil || len(rows) != 1 {
		t.Fatalf("a read stopped answering: %v %v", rows, err)
	}
}

// TestHaltRefusesAWritableOpen: the schema script and the metadata seeds a writable open runs are
// writes, so a marker published between a command's preflight and its open still stops them. A
// read-only command reads through the read-only opener instead.
func TestHaltRefusesAWritableOpen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "relay.sqlite3")
	s, err := Open(context.Background(), dbPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = RecordHalt(context.Background(), dbPath, CorruptingCause{Code: 11, Message: "malformed", Site: HaltSiteWrite}); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(context.Background(), dbPath, ""); RefusalReason(err) != ReasonStoreWriteHalted {
		t.Fatalf("a writable open was admitted: %v", err)
	}
	read, err := Open(WithReadOnlyCommand(context.Background()), dbPath, "")
	if err != nil {
		t.Fatalf("a read-only command could not open the halted store: %v", err)
	}
	defer func() { _ = read.Close() }()
	if !read.ReadOnly() {
		t.Fatal("a read-only command got a writable store")
	}
	if _, err = read.All(context.Background(), "SELECT COUNT(*) FROM schema_meta"); err != nil {
		t.Fatalf("the read-only store does not answer: %v", err)
	}
}

// TestHaltFailClosedOnADanglingMarker: a marker that is a dangling symbolic link is a directory entry
// that cannot be read, so it is a halt, never an absent marker.
func TestHaltFailClosedOnADanglingMarker(t *testing.T) {
	t.Parallel()
	_, dir := haltFixture(t)
	dbPath := filepath.Join(dir, "relay.sqlite3")
	if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, HaltMarkerName)); err != nil {
		t.Fatal(err)
	}
	state := HaltStateAt(dbPath)
	if !state.Present || state.Detail == "" {
		t.Fatalf("a dangling marker did not halt: %+v", state)
	}
}
