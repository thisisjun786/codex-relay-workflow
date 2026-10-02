package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"modernc.org/sqlite"
)

//go:embed relay-sqlite.sql
var schema embed.FS

const SchemaVersion = "1"

// ErrLiveState is the live-state guard's refusal. The guard is test isolation only: it refuses
// a database under a live relay state directory when CRW_REFUSE_LIVE_STATE=1 is set, which
// internal/testsupport sets in every test binary that links it and so in every process such a
// test starts. The product sets nothing and opens the live state (decisions.md 46).
var ErrLiveState = errors.New("store: live state refused under CRW_REFUSE_LIVE_STATE=1 (test isolation)")

// UnenforcedIndex is one guard index the store could not install ({"index", "detail"}).
type UnenforcedIndex struct{ Index, Detail string }

// Store owns a bounded pool of connections to one durable SQLite database.
type Store struct {
	DB                *sql.DB
	Path              string
	UnenforcedIndexes []UnenforcedIndex

	// faultHook runs inside every opened transaction after its body and before COMMIT
	// (store.py fault_hook). Tests use it to die at that point; nil in production.
	faultHook func()
	// gate is write-gate.lock, held SH while a writable store is open (holdGate).
	gate *os.File
	// readOnly is Store(read_only=True): mode=ro, query_only=ON, deferred BEGIN.
	readOnly bool
}

// The frozen contract contains DDL, then guard indexes, then illustrative seed SQL.
const guardMarker = "-- GUARD_INDEXES executed separately after the DDL"
const seedMarker = "-- schema_meta seeds and assignment_settlements backfill"

// OpenOptions changes connection setup for controlled contention tests only.
type OpenOptions struct {
	BusyTimeout   time.Duration
	OnConnect     func()
	BusyRetryHook func()
	// verify is the fenced opener's check of a writable store, run on the opened database
	// before the schema script; it returns the write gate the store holds (verifyWritable).
	verify func(context.Context, *sql.DB) (*os.File, error)
}

func Open(ctx context.Context, path, socketPath string) (*Store, error) {
	if ReadOnlyCommand(ctx) {
		return openForRead(ctx, path, socketPath)
	}
	if slot, ok := ctx.Value(admittedKey{}).(*Admitted); ok {
		if s := slot.take(path, socketPath); s != nil {
			return s, nil
		}
	}
	return openFenced(ctx, path, socketPath, OpenOptions{BusyTimeout: 30 * time.Second})
}

type admittedKey struct{}

// Admitted is the one writable store a relay command admitted before its handler ran (cli.main's
// _ownership_preflight opens services.store once, and the handler uses that same store). Hold places the store in the slot; the first writable Open of
// the same database with the same socket under the slot's context is handed it instead of
// opening another, and that caller then owns and closes it.
type Admitted struct {
	mu     sync.Mutex
	store  *Store
	path   string
	socket string
}

// WithAdmitted returns ctx with an empty admission slot. Its owner calls Release when the
// command is done, which closes a held store no Open took.
func WithAdmitted(ctx context.Context) (context.Context, *Admitted) {
	slot := &Admitted{}
	return context.WithValue(ctx, admittedKey{}, slot), slot
}

// Hold places st, opened writably at path with socket, in the slot.
func (a *Admitted) Hold(st *Store, path, socket string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.store, a.path, a.socket = st, filepath.Clean(path), socket
}

// take hands over the held store to an Open of the same database with the same socket.
func (a *Admitted) take(path, socket string) *Store {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil || a.path != filepath.Clean(path) || a.socket != socket {
		return nil
	}
	s := a.store
	a.store = nil
	return s
}

// Release closes the held store if no Open took it.
func (a *Admitted) Release() error {
	a.mu.Lock()
	s := a.store
	a.store = nil
	a.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.Close()
}

// open configures an existing database after admission. The connection hook
// runs for every connection, not just the first one; it never creates a file.
func open(ctx context.Context, path, socketPath string, options OpenOptions) (_ *Store, err error) {
	resolved, err := refuseLiveState(path)
	if err != nil {
		return nil, err
	}
	d := &sqlite.Driver{}
	d.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, _ string) error {
		if options.OnConnect != nil {
			options.OnConnect()
		}
		// Python's sqlite3.connect installs its timeout before executing journal_mode.
		// Do the same: journal_mode may need a lock even before the schema is read.
		pragmas := []string{fmt.Sprintf("PRAGMA busy_timeout=%d", options.BusyTimeout.Milliseconds()), "PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA foreign_keys=ON"}
		busyConfigured := false
		for _, pragma := range pragmas {
			deadline := time.Now().Add(options.BusyTimeout)
			for {
				_, pragmaErr := conn.ExecContext(ctx, pragma, []driver.NamedValue{})
				if pragmaErr == nil {
					break
				}
				if ctxErr := ctx.Err(); ctxErr != nil {
					return fmt.Errorf("%s: %w", pragma, ctxErr)
				}
				// modernc does not apply SQLite's busy handler while a connection hook
				// changes journal mode. Retry only after busy_timeout was installed.
				if !busyConfigured || !strings.Contains(pragmaErr.Error(), "SQLITE_BUSY") || !time.Now().Before(deadline) {
					return fmt.Errorf("%s: %w", pragma, pragmaErr)
				}
				if options.BusyRetryHook != nil {
					options.BusyRetryHook()
				}
				timer := time.NewTimer(time.Millisecond)
				select {
				case <-ctx.Done():
					if !timer.Stop() {
						<-timer.C
					}
					return fmt.Errorf("%s: %w", pragma, ctx.Err())
				case <-timer.C:
				}
			}
			if strings.HasPrefix(pragma, "PRAGMA busy_timeout=") {
				busyConfigured = true
			}
		}
		return nil
	})
	u := url.URL{Scheme: "file", Path: resolved}
	q := u.Query()
	q.Set("mode", "rw")
	// modernc applies DSN pragmas before invoking connection hooks. Installing the
	// handler here gives journal_mode the same pre-first-statement wait Python gets
	// from sqlite3.connect(timeout=30); the hook repeats it for explicit parity.
	q.Set("_busy_timeout", fmt.Sprint(options.BusyTimeout.Milliseconds()))
	u.RawQuery = q.Encode()
	db := sql.OpenDB(dsnConnector{textGuard{d}, u.String()})
	db.SetMaxOpenConns(1)
	defer func() {
		if err != nil {
			_ = db.Close()
		}
	}()
	if err = db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("connect database: %w", err)
	}
	var gate *os.File
	if options.verify != nil {
		if gate, err = options.verify(ctx, db); err != nil {
			return nil, err
		}
		defer func() {
			if err != nil {
				err = errors.Join(err, gate.Close())
			}
		}()
	}
	ddl, err := schema.ReadFile("relay-sqlite.sql")
	if err != nil {
		return nil, fmt.Errorf("embedded schema: %w", err)
	}
	sections := strings.SplitN(string(ddl), guardMarker, 2)
	if len(sections) != 2 {
		return nil, errors.New("embedded schema lacks guard marker")
	}
	if _, err = db.ExecContext(ctx, sections[0]); err != nil {
		return nil, fmt.Errorf("initialize schema: %w", err)
	}
	// The additive DAG zone follows the v1 script and is not part of what the open validated
	// (dag_zone.go): a store that predates it gains it here. A command that declares itself read-only
	// opens the store through this path too (openForRead tries the writable open first), and it
	// must not change the schema, so it leaves the zone to the next write.
	if !ReadOnlyCommand(ctx) {
		if err = installDAGZone(ctx, db); err != nil {
			return nil, err
		}
	}
	result := &Store{DB: db, Path: pathlibSpelling(path), gate: gate}
	guards := strings.SplitN(sections[1], seedMarker, 2)
	if len(guards) != 2 {
		return nil, errors.New("embedded schema lacks seed marker")
	}
	for _, statement := range strings.Split(guards[0], ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		if _, guardErr := db.ExecContext(ctx, statement); guardErr != nil {
			name := strings.Fields(strings.TrimPrefix(statement, "CREATE UNIQUE INDEX IF NOT EXISTS "))[0]
			result.UnenforcedIndexes = append(result.UnenforcedIndexes, UnenforcedIndex{Index: name, Detail: guardErr.Error()})
		}
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	storeID, err := randomBytes(16)
	if err != nil {
		return nil, fmt.Errorf("store identity: %w", err)
	}
	for _, pair := range [][2]string{{"version", SchemaVersion}, {"store_id", hex.EncodeToString(storeID)}, {"store_created_at", now}} {
		if _, err = db.ExecContext(ctx, "INSERT OR IGNORE INTO schema_meta VALUES (?, ?)", pair[0], pair[1]); err != nil {
			return nil, fmt.Errorf("seed %s: %w", pair[0], err)
		}
	}
	if _, err = db.ExecContext(ctx, "INSERT OR IGNORE INTO assignment_settlements (relationship_id, thread_id, turn_id, terminal_status, settled_at) SELECT relationship_id, thread_id, turn_id, terminal_status, observed_at FROM observations WHERE relationship_id IS NOT NULL"); err != nil {
		return nil, fmt.Errorf("backfill settlements: %w", err)
	}
	if socketPath != "" {
		canonical, pathErr := canonicalSocket(socketPath)
		if pathErr != nil {
			return nil, pathErr
		}
		if _, err = db.ExecContext(ctx, "INSERT OR IGNORE INTO schema_meta VALUES ('socket_path', ?)", canonical); err != nil {
			return nil, fmt.Errorf("seed socket_path: %w", err)
		}
	}
	return result, nil
}

// refuseLiveState resolves path and, under test isolation (CRW_REFUSE_LIVE_STATE=1), refuses it
// when it lies under a live relay state directory: ~/.local/state/codex-session-relay and
// $XDG_STATE_HOME/codex-session-relay are both live, whichever of them the environment currently
// selects. Without that variable it only resolves path: the product opens the live state.
func refuseLiveState(path string) (string, error) {
	absolute := path
	if !filepath.IsAbs(path) {
		cwd, err := unix.Getwd()
		if err != nil {
			return "", fmt.Errorf("absolute database path: %w", err)
		}
		absolute = cwd + "/" + path
	}
	resolved, err := resolvePath(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve database: %w", err)
	}
	if os.Getenv("CRW_REFUSE_LIVE_STATE") != "1" {
		return resolved, nil
	}
	// The home default is Path.home()'s, as discovery reads it; a home nothing answers fails closed.
	home, err := homeDir()
	if err != nil {
		return "", fmt.Errorf("live state: %w", err)
	}
	lives := []string{filepath.Join(home, ".local", "state", "codex-session-relay")}
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		lives = append(lives, filepath.Join(state, "codex-session-relay"))
	}
	for _, live := range lives {
		live, err := filepath.Abs(live)
		if err != nil {
			return "", fmt.Errorf("absolute live state: %w", err)
		}
		live, err = resolvePath(live)
		if errors.Is(err, syscall.ENOTDIR) {
			// A live-state root below a regular file cannot contain any DB.
			// Do not let that unrelated environment path deny an explicit store.
			continue
		}
		if err != nil {
			return "", fmt.Errorf("resolve live state: %w", err)
		}
		relative, err := filepath.Rel(live, resolved)
		if err != nil {
			return "", fmt.Errorf("relative live state: %w", err)
		}
		if relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			return "", ErrLiveState
		}
	}
	return resolved, nil
}

func randomBytes(size int) ([]byte, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

func (s *Store) Close() error {
	err := s.DB.Close()
	if s.gate != nil {
		err = errors.Join(err, s.gate.Close())
		s.gate = nil
	}
	return err
}

// Location distinguishes the database inode from the directory entry containing its WAL.
type Location struct {
	DBPath        string
	RealPath      string
	Device        uint64
	Inode         uint64
	Links         uint64
	LogDevice     uint64
	LogInode      uint64
	LogName       string
	StoreID       string
	CreatedAt     string
	SchemaVersion string
}

func (s *Store) Locate(ctx context.Context) (Location, error) {
	var result Location
	result.DBPath = s.Path
	real, err := resolvePath(s.Path)
	if err == nil {
		if _, err = os.Stat(real); err == nil {
			result.RealPath = real
		}
	}
	if err := s.q(ctx).QueryRowContext(ctx, "SELECT value FROM schema_meta WHERE key='store_id'").Scan(&result.StoreID); err != nil {
		return result, fmt.Errorf("store_id: %w", err)
	}
	if err := s.q(ctx).QueryRowContext(ctx, "SELECT value FROM schema_meta WHERE key='store_created_at'").Scan(&result.CreatedAt); err != nil {
		return result, fmt.Errorf("created_at: %w", err)
	}
	if err := s.q(ctx).QueryRowContext(ctx, "SELECT value FROM schema_meta WHERE key='version'").Scan(&result.SchemaVersion); err != nil {
		return result, fmt.Errorf("version: %w", err)
	}
	var opened string
	if err := s.q(ctx).QueryRowContext(ctx, "PRAGMA database_list").Scan(new(int), new(string), &opened); err != nil {
		return result, fmt.Errorf("database_list: %w", err)
	}
	if err == nil {
		fillLocation(&result, real, opened)
	} else {
		fillLocation(&result, "", opened)
	}
	return result, nil
}

// SchemaStatements is the schema a store installs, in the order Store installs it: the DDL
// script, then each guard index (store.py DDL and GUARD_INDEXES). The runtime swap gate applies
// them to an in-memory database to learn the schema this build declares.
func SchemaStatements() (string, []string, error) {
	raw, err := schema.ReadFile("relay-sqlite.sql")
	if err != nil {
		return "", nil, fmt.Errorf("embedded schema: %w", err)
	}
	sections := strings.SplitN(string(raw), guardMarker, 2)
	if len(sections) != 2 {
		return "", nil, errors.New("embedded schema lacks guard marker")
	}
	guards := strings.SplitN(sections[1], seedMarker, 2)
	if len(guards) != 2 {
		return "", nil, errors.New("embedded schema lacks seed marker")
	}
	var statements []string
	for _, statement := range strings.Split(guards[0], ";") {
		if statement = strings.TrimSpace(statement); statement != "" {
			statements = append(statements, statement)
		}
	}
	return sections[0], statements, nil
}

// dsnConnector opens every connection of one *sql.DB through its own driver and DSN (sql.OpenDB),
// so an open registers nothing in database/sql's process-wide driver table: a registration can
// never be removed, and a long-running process opens stores again and again.
type dsnConnector struct {
	driver driver.Driver
	dsn    string
}

func (c dsnConnector) Connect(context.Context) (driver.Conn, error) { return c.driver.Open(c.dsn) }
func (c dsnConnector) Driver() driver.Driver                        { return c.driver }
