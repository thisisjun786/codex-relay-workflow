package store

import (
	"bufio"
	"context"
	"errors"
	"io"
	"modernc.org/sqlite"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const pythonHolder = `import sqlite3,sys
c=sqlite3.connect(sys.argv[1],timeout=30,isolation_level=None)
c.execute('BEGIN IMMEDIATE')
print('LOCKED',flush=True)
sys.stdin.readline()
c.execute('ROLLBACK')
`
const pythonWriter = `import sqlite3,sys
c=sqlite3.connect(sys.argv[1],timeout=0.2,isolation_level=None)
c.execute('BEGIN IMMEDIATE')
c.execute("INSERT INTO schema_meta VALUES ('probe','written')")
c.execute('COMMIT')
`

func TestLock_go_write_is_busy_when_python_holds_immediate(t *testing.T) {
	// Given: Python has acquired the real database's POSIX lock.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	store, err := open(ctx, path, "", OpenOptions{BusyTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cmd := exec.Command("python3", "-c", pythonHolder, path)
	cmd.Env = isolatedEnv(t)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if strings.TrimSpace(line) != "LOCKED" {
			t.Fatalf("Python did not lock: %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Python did not acquire lock")
	}
	// When: Go attempts a competing write with its short test-only busy timeout.
	_, err = store.DB.ExecContext(ctx, "INSERT INTO schema_meta VALUES ('probe','written')")
	// Then: contention refuses the write and there is no partial row.
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code() != 5 {
		t.Fatalf("want SQLITE_BUSY (5), got %v", err)
	}
	if _, err := io.WriteString(stdin, "release\n"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	stdin.Close()
	var count int
	if err := store.DB.QueryRowContext(ctx, "SELECT count(*) FROM schema_meta WHERE key='probe'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial write count=%d err=%v", count, err)
	}
}
func TestLock_python_write_is_busy_when_go_holds_immediate(t *testing.T) {
	// Given: Go has acquired a real WAL writer lock.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	store, err := Open(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conn, err := store.DB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	// When: Python attempts a competing write.
	cmd := exec.Command("python3", "-c", pythonWriter, path)
	cmd.Env = isolatedEnv(t)
	output, err := cmd.CombinedOutput()
	// Then: Python observes SQLite's lock error without corrupting the store.
	if err == nil || !strings.Contains(string(output), "database is locked") {
		t.Fatalf("Python should see SQLITE_BUSY: %v: %s", err, output)
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	var integrity string
	if err := store.DB.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity=%q: %v", integrity, err)
	}
}
func isolatedEnv(t *testing.T) []string {
	t.Helper()
	root := t.TempDir()
	return append(os.Environ(), "HOME="+root, "XDG_STATE_HOME="+filepath.Join(root, "state"), "XDG_DATA_HOME="+filepath.Join(root, "data"), "XDG_CONFIG_HOME="+filepath.Join(root, "config"), "CODEX_HOME="+filepath.Join(root, "codex"))
}
