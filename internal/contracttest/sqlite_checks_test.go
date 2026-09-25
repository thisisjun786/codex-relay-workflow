package contracttest

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
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
		python := filepath.Join(root, "python", "relay.sqlite3")
		if err := os.MkdirAll(filepath.Dir(python), 0700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("uv", "run", "--no-sync", "python", "-c", "from codex_session_relay.store import Store; import sys; Store(sys.argv[1])", python)
		cmd.Dir = RootMust(t)
		cmd.Env = append(sqliteEnv(t), "PYTHONPATH="+filepath.Join(cmd.Dir, "packages/codex-session-relay/src"))
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("Python Store: %v: %s", err, output)
		}
		goPath := filepath.Join(root, "go", "relay.sqlite3")
		s, err := store.Open(context.Background(), goPath, "")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if got, want := sqliteMaster(t, goPath), sqliteMaster(t, python); !reflect.DeepEqual(got, want) {
			t.Fatalf("sqlite_master differs from Python: Go %v, Python %v", got, want)
		}
	})
	t.Run("fixture", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.sqlite3")
		fixture, err := os.ReadFile(filepath.Join(RootMust(t), "contract/fixtures/sqlite-ddl/python-store.sqlite3"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, fixture, 0600); err != nil {
			t.Fatal(err)
		}
		before := sqliteMaster(t, path)
		s, err := store.Open(context.Background(), path, "")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if !reflect.DeepEqual(before, sqliteMaster(t, path)) {
			t.Fatal("Python fixture sqlite_master changed")
		}
		var integrity string
		if err := s.DB.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
			t.Fatalf("integrity=%q: %v", integrity, err)
		}
	})
	t.Run("pool", func(t *testing.T) {
		s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite3"), "")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		conns := make([]*sql.Conn, 8)
		for i := range conns {
			conns[i], err = s.DB.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conns[i].Close()
		}
		for _, conn := range conns {
			for query, want := range map[string]int{"PRAGMA foreign_keys": 1, "PRAGMA synchronous": 2, "PRAGMA busy_timeout": 30000} {
				var got int
				if err := conn.QueryRowContext(context.Background(), query).Scan(&got); err != nil || got != want {
					t.Fatalf("%s=%d want %d: %v", query, got, want, err)
				}
			}
			var journal string
			if err := conn.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
				t.Fatalf("journal=%q: %v", journal, err)
			}
		}
	})
	t.Run("locks", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.sqlite3")
		s, err := store.Open(context.Background(), path, "")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		holder := exec.Command("python3", "-c", "import sqlite3,sys; c=sqlite3.connect(sys.argv[1],isolation_level=None); c.execute('BEGIN IMMEDIATE'); print('LOCKED',flush=True); sys.stdin.readline(); c.execute('ROLLBACK')", path)
		holder.Env = sqliteEnv(t)
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
				t.Fatalf("Python lock: %q", line)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Python lock timeout")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		_, err = s.DB.ExecContext(ctx, "INSERT INTO schema_meta VALUES ('lock_probe','value')")
		cancel()
		var busy *sqlite.Error
		if !errors.As(err, &busy) || busy.Code() != 5 {
			t.Fatalf("Go should see SQLITE_BUSY: %v", err)
		}
		if _, err := stdin.Write([]byte("release\n")); err != nil {
			t.Fatal(err)
		}
		if err := holder.Wait(); err != nil {
			t.Fatal(err)
		}
		stdin.Close()
		conn, err := s.DB.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
			t.Fatal(err)
		}
		defer conn.ExecContext(context.Background(), "ROLLBACK")
		writer := exec.Command("python3", "-c", "import sqlite3,sys; c=sqlite3.connect(sys.argv[1],timeout=0.2,isolation_level=None); c.execute('BEGIN IMMEDIATE'); c.execute(\"INSERT INTO schema_meta VALUES ('lock_probe','value')\"); c.execute('COMMIT')", path)
		writer.Env = sqliteEnv(t)
		output, err := writer.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "database is locked") {
			t.Fatalf("Python should see SQLITE_BUSY: %v: %s", err, output)
		}
		if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
			t.Fatal(err)
		}
		var integrity string
		if err := s.DB.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
			t.Fatalf("integrity=%q: %v", integrity, err)
		}
	})
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
	rows, err := db.Query("SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name,tbl_name")
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
