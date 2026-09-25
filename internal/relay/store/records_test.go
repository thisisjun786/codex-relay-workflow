package store

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func recordStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	store, err := Open(context.Background(), filepath.Join(root, "db", "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func TestTransaction_rolls_back_when_process_dies_before_commit(t *testing.T) {
	// Given: a child process that signals immediately after its write inside BEGIN IMMEDIATE.
	if path := os.Getenv("CRW_CRASH_DB"); path != "" {
		db, err := sql.Open("sqlite", "file:"+path+"?mode=rw")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(context.Background(), `INSERT INTO journal (at,kind,subject,detail) VALUES ('t','crash','s','d')`); err != nil {
			t.Fatal(err)
		}
		fmt.Println("written")
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
	store := recordStore(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestTransaction_rolls_back_when_process_dies_before_commit$")
	cmd.Env = append(os.Environ(), "CRW_CRASH_DB="+store.Path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	// When: the parent receives the write signal and kills the process before COMMIT.
	line := bufio.NewScanner(out)
	if !line.Scan() || line.Text() != "written" {
		t.Fatalf("child did not write: %q: %v", line.Text(), line.Err())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait() // a killed process exits unsuccessfully by design.
	// Then: a distinct connection finds no durable row.
	entries, err := store.Journal(context.Background(), "crash", "s")
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial journal: %+v: %v", entries, err)
	}
}

func TestTransaction_rolls_back_when_body_fails(t *testing.T) {
	// Given: a fresh durable store.
	store := recordStore(t)
	ctx := context.Background()
	failure := errors.New("interrupted")
	// When: a process reports failure after writing but before commit.
	err := store.Transaction(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `INSERT INTO journal (at,kind,subject,detail) VALUES ('t','k','s','d')`); err != nil {
			return err
		}
		return failure
	})
	// Then: a separate connection sees no partial record.
	if !errors.Is(err, failure) {
		t.Fatalf("failure lost: %v", err)
	}
	entries, err := store.Journal(ctx, "k", "s")
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial journal: %+v, %v", entries, err)
	}
}

func TestTransaction_rolls_back_when_commit_fails(t *testing.T) {
	// Given: a deferred foreign-key constraint checked only at COMMIT.
	store := recordStore(t)
	ctx := context.Background()
	_, err := store.DB.ExecContext(ctx, `CREATE TABLE commit_guard (id INTEGER PRIMARY KEY, parent INTEGER REFERENCES missing_parent(id) DEFERRABLE INITIALLY DEFERRED)`)
	if err != nil {
		t.Fatal(err)
	}
	// When: committing a row violating the deferred constraint.
	err = store.Transaction(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO commit_guard (id, parent) VALUES (1, 1)`)
		return err
	})
	// Then: the commit fails and the transaction releases its lock.
	if err == nil {
		t.Fatal("commit succeeded")
	}
	var count int
	if err := store.DB.QueryRowContext(ctx, `SELECT count(*) FROM commit_guard`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial committed row: %d: %v", count, err)
	}
	if err := store.AppendJournal(ctx, JournalEntry{At: "t", Kind: "k", Subject: "s", Detail: "d"}); err != nil {
		t.Fatalf("transaction remains locked: %v", err)
	}
}

func TestChallenge_reads_python_compatible_row(t *testing.T) {
	// Given: a fresh database.
	store := recordStore(t)
	ctx := context.Background()
	expected := Challenge{Nonce: "nonce", WrittenBy: "worker", WrittenAt: "2026-09-25T00:00:00Z"}
	// When: a challenge is written and read through another connection.
	if err := store.WriteChallenge(ctx, expected); err != nil {
		t.Fatal(err)
	}
	got, err := store.Challenge(ctx, expected.Nonce)
	// Then: all persisted fields retain their values.
	if err != nil || got != expected {
		t.Fatalf("challenge = %+v: %v", got, err)
	}
}

func TestJournal_preserves_payload_bytes_when_written(t *testing.T) {
	// Given: a JSON payload whose field order is significant.
	store := recordStore(t)
	ctx := context.Background()
	payload := `{"z":1,"a":2}`
	// When: it is appended to the journal.
	if err := store.AppendJournal(ctx, JournalEntry{At: "t", Kind: "audit", Subject: "event", Detail: payload}); err != nil {
		t.Fatal(err)
	}
	entries, err := store.Journal(ctx, "audit", "event")
	// Then: persisted bytes are not reserialized or reordered.
	if err != nil || len(entries) != 1 || entries[0].Detail != payload {
		t.Fatalf("journal = %+v: %v", entries, err)
	}
}

func TestRelationship_reads_python_compatible_row(t *testing.T) {
	// Given: a relationship as inserted by Python's registry.
	store := recordStore(t)
	ctx := context.Background()
	_, err := store.DB.ExecContext(ctx, `INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES ('r','CRW-1','active','parent','host','child','host',2,'[]','[]','t','t')`)
	if err != nil {
		t.Fatal(err)
	}
	// When: reading the relationship for a status command.
	got, err := store.Relationship(ctx, "r")
	// Then: the generation and identity match the stored row.
	if err != nil || got.ID != "r" || got.Generation != 2 || got.ChildTaskID != "child" {
		t.Fatalf("relationship = %+v: %v", got, err)
	}
}

func TestOpen_does_not_write_host_state_when_record_queries_run(t *testing.T) {
	// Given: a confined test home and state directory.
	store := recordStore(t)
	// When: a missing event is queried.
	_, err := store.Event(context.Background(), "absent")
	// Then: absence is distinguishable from a valid row; no host paths are used.
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing event: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(store.Path), "relay.sqlite3")); err != nil {
		t.Fatal(err)
	}
}
