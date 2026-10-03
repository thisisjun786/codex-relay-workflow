package contracttest

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"modernc.org/sqlite"
)

func checkSQLiteContract(t *testing.T) {
	t.Helper()
	t.Run("DDL", func(t *testing.T) {
		root := t.TempDir()
		frozen, err := os.ReadFile(filepath.Join(RootMust(t), "contract/schema/relay-sqlite.sql"))
		if err != nil {
			t.Fatal(err)
		}
		embedded, err := os.ReadFile(filepath.Join(RootMust(t), "internal/relay/store/relay-sqlite.sql"))
		if err != nil || !bytes.Equal(frozen, embedded) {
			t.Fatalf("embedded DDL differs from frozen Python contract: %v", err)
		}
		// The schema the previous version created, as the committed fixture holds it: a copy is read,
		// because opening the fixture in place would leave WAL sidecars in the corpus. This version's
		// schema is that and the indexes CRW-301 added (the golden delta), nothing else.
		python := filepath.Join(root, "python", "relay.sqlite3")
		copySQLiteFixture(t, python)
		goPath := filepath.Join(root, "go", "relay.sqlite3")
		s, err := store.Open(context.Background(), goPath, "")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if got, want := sqliteMaster(t, goPath), withHistoryIndexes(t, sqliteMaster(t, python)); !reflect.DeepEqual(got, want) {
			t.Fatalf("sqlite_master differs from the previous version's plus the history indexes: Go %v, want %v", got, want)
		}
	})
	t.Run("fixture", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.sqlite3")
		copySQLiteFixture(t, path)
		// The fixture is written by hand, unfenced; the Go host owns it as its initializer would.
		testsupport.Fence(t, path, "go")
		before := sqliteMaster(t, path)
		s, err := store.Open(context.Background(), path, "")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		// The upgrade: opening a store of the previous version gains exactly the history indexes and
		// changes no other object, and opening it again changes nothing (CRW-301).
		upgraded := sqliteMaster(t, path)
		if want := withHistoryIndexes(t, before); !reflect.DeepEqual(upgraded, want) {
			t.Fatalf("opening the previous version's store: sqlite_master is %v, want the previous one plus the history indexes %v", upgraded, want)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		again, err := store.Open(context.Background(), path, "")
		if err != nil {
			t.Fatal(err)
		}
		defer again.Close()
		if !reflect.DeepEqual(upgraded, sqliteMaster(t, path)) {
			t.Fatal("a second open changed sqlite_master")
		}
		var integrity string
		if err := again.DB.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
			t.Fatalf("integrity=%q: %v", integrity, err)
		}
	})
	t.Run("pool", func(t *testing.T) {
		s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite3"), "")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		// The relay store has one connection (decisions.md section 4); each round discards it,
		// so every round's PRAGMAs come from the connection hook on a freshly dialled connection.
		if max := s.DB.Stats().MaxOpenConnections; max != 1 {
			t.Fatalf("max open connections %d", max)
		}
		for round := range 3 {
			conn, err := s.DB.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for query, want := range map[string]int{"PRAGMA foreign_keys": 1, "PRAGMA synchronous": 2, "PRAGMA busy_timeout": 30000} {
				var got int
				if err := conn.QueryRowContext(context.Background(), query).Scan(&got); err != nil || got != want {
					t.Fatalf("connection %d: %s=%d want %d: %v", round, query, got, want, err)
				}
			}
			var journal string
			if err := conn.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
				t.Fatalf("connection %d: journal=%q: %v", round, journal, err)
			}
			if _, err := conn.ExecContext(context.Background(), "PRAGMA foreign_keys=OFF"); err != nil {
				t.Fatal(err)
			}
			// Raw returning ErrBadConn removes the connection from the pool and closes it.
			if err := conn.Raw(func(any) error { return driver.ErrBadConn }); !errors.Is(err, driver.ErrBadConn) {
				t.Fatalf("discard connection: %v", err)
			}
		}
	})
	t.Run("locks", func(t *testing.T) {
		ctx := context.Background()
		path := filepath.Join(t.TempDir(), "relay.sqlite3")
		s, err := store.Open(ctx, path, "")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		// The store has one pooled connection, so the connection taken here is the store's own, with
		// the busy timeout its connection hook installed. SQLITE_BUSY comes after that timeout, 30 s
		// as the store sets it, so the probe lowers the timeout on this connection: the driver, the
		// lock and the error are the store's, only the wait is short.
		probe, err := s.DB.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		requireBusyTimeout(t, probe, storeBusyTimeout.Milliseconds())
		if _, err := probe.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", probeBusyTimeout.Milliseconds())); err != nil {
			t.Fatal(err)
		}
		requireBusyTimeout(t, probe, probeBusyTimeout.Milliseconds())
		// With no lock held the probe write succeeds, so the SQLITE_BUSY below comes from the peer's lock.
		if _, err := probe.ExecContext(ctx, lockProbeInsert); err != nil {
			t.Fatalf("the probe write on an unlocked store: %v", err)
		}
		if _, err := probe.ExecContext(ctx, "DELETE FROM schema_meta WHERE key = 'lock_probe'"); err != nil {
			t.Fatal(err)
		}
		// A second process holds the store's write lock: the SQLite locking contract is between
		// processes, whichever runtime each one is. The peer is this test binary (sqlitePeer).
		holder := sqlitePeerCommand(t, "hold", path)
		stdout, err := holder.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdin, err := holder.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := holder.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			stdin.Close()
			if holder.ProcessState == nil {
				holder.Process.Kill()
				holder.Wait()
			}
		})
		ready := make(chan string, 1)
		go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
		select {
		case line := <-ready:
			if strings.TrimSpace(line) != "LOCKED" {
				t.Fatalf("peer lock: %q", line)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("peer lock timeout")
		}
		probeCtx, cancel := context.WithTimeout(ctx, probeBudget)
		started := time.Now()
		_, err = probe.ExecContext(probeCtx, lockProbeInsert)
		waited := time.Since(started)
		cancel()
		var busy *sqlite.Error
		if !errors.As(err, &busy) || busy.Code() != 5 {
			t.Fatalf("Go should see SQLITE_BUSY: %v", err)
		}
		// A busy handler in force waits its timeout out before it gives up: failing at once means it
		// is not installed, and still waiting at the budget means the lowered timeout did not apply.
		if waited < probeBusyTimeout/2 || waited >= probeBudget {
			t.Fatalf("SQLITE_BUSY after %v with a busy timeout of %v", waited, probeBusyTimeout)
		}
		if _, err := stdin.Write([]byte("release\n")); err != nil {
			t.Fatal(err)
		}
		if err := holder.Wait(); err != nil {
			t.Fatal(err)
		}
		stdin.Close()
		// Discard the lowered connection, as the pool check does: the next one is dialled by the
		// connection hook and has the store's timeout again.
		if err := probe.Raw(func(any) error { return driver.ErrBadConn }); !errors.Is(err, driver.ErrBadConn) {
			t.Fatalf("discard the probe connection: %v", err)
		}
		conn, err := s.DB.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		requireBusyTimeout(t, conn, storeBusyTimeout.Milliseconds())
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			t.Fatal(err)
		}
		output, err := sqlitePeerCommand(t, "write", path).CombinedOutput()
		if err == nil || !strings.Contains(string(output), "database is locked") {
			t.Fatalf("the peer should see SQLITE_BUSY: %v: %s", err, output)
		}
		if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
			t.Fatal(err)
		}
		// The store has one connection: release it before asking the store anything else.
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		var integrity string
		if err := s.DB.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
			t.Fatalf("integrity=%q: %v", integrity, err)
		}
	})
}

// The lock check waits for SQLITE_BUSY on a connection whose busy timeout it lowered: the store's
// own is storeBusyTimeout (internal/relay/store Open), the probe's is probeBusyTimeout, and the
// probe is given probeBudget, 20 times its timeout and a third of the store's, to return.
const (
	storeBusyTimeout = 30 * time.Second
	probeBusyTimeout = 500 * time.Millisecond
	probeBudget      = 10 * time.Second
	lockProbeInsert  = "INSERT INTO schema_meta VALUES ('lock_probe','value')"
)

// requireBusyTimeout fails the test unless conn's busy timeout is want milliseconds.
func requireBusyTimeout(t *testing.T, conn *sql.Conn, want int64) {
	t.Helper()
	var got int64
	if err := conn.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&got); err != nil || got != want {
		t.Fatalf("PRAGMA busy_timeout=%d, want %d: %v", got, want, err)
	}
}

// sqlitePeerEnv names the SQLite peer mode a re-executed test binary runs instead of the tests
// (TestMain), and sqlitePeerPath the store it opens.
const (
	sqlitePeerEnv  = "CRW_CONTRACTTEST_SQLITE_PEER"
	sqlitePeerPath = "CRW_CONTRACTTEST_SQLITE_PATH"
)

func sqlitePeerCommand(t *testing.T, mode, path string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(sqliteEnv(t), sqlitePeerEnv+"="+mode, sqlitePeerPath+"="+path)
	return cmd
}

// sqlitePeer is the second process of the locks check. "hold" takes the write lock with BEGIN
// IMMEDIATE, prints LOCKED and keeps it until a line arrives on stdin; "write" tries to write
// with a 200 ms busy timeout and exits 1 printing the error when the lock is held elsewhere.
func sqlitePeer(mode, path string) int {
	ctx := context.Background()
	dsn := "file:" + path
	if mode == "write" {
		dsn += "?_pragma=busy_timeout(200)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	switch mode {
	case "hold":
		fmt.Println("LOCKED")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		_, err = conn.ExecContext(ctx, "ROLLBACK")
	case "write":
		if _, err = conn.ExecContext(ctx, "INSERT INTO schema_meta VALUES ('lock_probe','value')"); err == nil {
			_, err = conn.ExecContext(ctx, "COMMIT")
		}
	default:
		err = fmt.Errorf("unknown SQLite peer mode %q", mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// copySQLiteFixture writes the committed Python store (contract/fixtures/sqlite-ddl) to path.
func copySQLiteFixture(t *testing.T, path string) {
	t.Helper()
	fixture, err := testsupport.FrozenStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
}

func RootMust(t *testing.T) string {
	t.Helper()
	root, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	return root
}
func sqliteEnv(t *testing.T) []string {
	t.Helper()
	root := t.TempDir()
	return append(os.Environ(), "HOME="+root, "XDG_STATE_HOME="+filepath.Join(root, "state"), "XDG_DATA_HOME="+filepath.Join(root, "data"), "XDG_CONFIG_HOME="+filepath.Join(root, "config"), "CODEX_HOME="+filepath.Join(root, "codex"))
}
func sqliteMaster(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT type,name,tbl_name,sql FROM sqlite_master WHERE name NOT LIKE 'dag\\_%' ESCAPE '\\' AND tbl_name NOT LIKE 'dag\\_%' ESCAPE '\\' ORDER BY type,name,tbl_name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var kind, name, table string
		var text sql.NullString
		if err := rows.Scan(&kind, &name, &table, &text); err != nil {
			t.Fatal(err)
		}
		values = append(values, kind+"\x00"+name+"\x00"+table+"\x00"+strings.Join(strings.Fields(text.String), " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return values
}

// withHistoryIndexes is a catalog in sqliteMaster's form with the indexes CRW-301 added (the golden delta,
// contract/schema/relay-sqlite-history-indexes.json) put where they sort.
func withHistoryIndexes(t *testing.T, catalog []string) []string {
	t.Helper()
	indexes, err := testsupport.HistoryIndexes()
	if err != nil || len(indexes) == 0 {
		t.Fatalf("the golden delta: %d indexes: %v", len(indexes), err)
	}
	out := append([]string(nil), catalog...)
	for _, index := range indexes {
		out = append(out, "index\x00"+index.Name+"\x00"+index.Table+"\x00"+strings.Join(strings.Fields(index.SQL), " "))
	}
	sort.Strings(out)
	return out
}
