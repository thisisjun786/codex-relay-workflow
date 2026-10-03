package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// RegistrationHold, which intent-register runs, stats the store before its write admission, so a
// store that does not exist, whose directory does not, that sits below a regular file or behind
// a dangling symlink is answered with the stat's error (the golden), and nothing is created.
func TestRegistrationHold_answers_an_unstattable_store_with_the_stat_error(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	file := filepath.Join(root, "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(root, "dangling.sqlite3")
	if err := os.Symlink(filepath.Join(root, "gone.sqlite3"), dangling); err != nil {
		t.Fatal(err)
	}
	before := listTree(t, root)
	var answered [][2]string
	for _, path := range []string{
		filepath.Join(root, "no-such-store.sqlite3"),
		filepath.Join(root, "state", "no-such-store.sqlite3"),
		filepath.Join(file, "relay.sqlite3"),
		dangling,
	} {
		var registration string
		if err := RegistrationHold(t.Context(), path, func(conn *sql.Conn, why string) error {
			if conn != nil {
				t.Errorf("%s: RegistrationHold took a hold", path)
			}
			registration = why
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		answered = append(answered, [2]string{path, registration})
	}
	checkJSON(t, "store, why", answered, golden.Substitute(root, "<ROOT>"))
	if after := listTree(t, root); after != before {
		t.Fatalf("a refused hold changed the tree\nbefore: %s\nafter:  %s", before, after)
	}
}

func listTree(t *testing.T, root string) string {
	t.Helper()
	var names string
	if err := filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		names += path + "\n"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return names
}

// A writable opener that finds an existing D with no write gate reads it before deciding what
// it is: a D SQLite cannot read as a database, or cannot read at all, fails with that error as
// the command's host error (the golden), and no gate or sidecar is created for it. A readable
// legacy D still goes to the admission, which refuses it without a gate: Go never initializes
// one (decisions.md 30).
func TestOpen_reads_a_gateless_store_before_refusing_it(t *testing.T) {
	// Serial: sets process environment variables, which every other running test would see.
	t.Setenv("CRW_REFUSE_LIVE_STATE", "1")
	for _, c := range []struct {
		name  string
		write func(t *testing.T, path string)
	}{
		{"not a database", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(strings.Repeat("this is not a database\n", 64)), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"unreadable file", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("x"), 0o000); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "relay.sqlite3")
			c.write(t, path)
			before := listTree(t, filepath.Dir(path))
			opened, err := Open(t.Context(), path, "")
			if err == nil {
				_ = opened.Close()
				t.Fatal("Go opened a gateless store")
			}
			if RefusalReason(err) != "" {
				t.Fatalf("%s (reason %q)", err, RefusalReason(err))
			}
			checkText(t, "error", err.Error(), golden.Substitute(filepath.Dir(path), "<DIR>"))
			if after := listTree(t, filepath.Dir(path)); after != before {
				t.Fatalf("Go changed the directory\nbefore: %s\nafter:  %s", before, after)
			}
		})
	}
	t.Run("legacy store", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.sqlite3")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec("CREATE TABLE schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
			t.Fatal(err)
		}
		if err = db.Close(); err != nil {
			t.Fatal(err)
		}
		before := listTree(t, filepath.Dir(path))
		opened, err := Open(t.Context(), path, "")
		if err == nil {
			_ = opened.Close()
			t.Fatal("Go opened a legacy store")
		}
		if RefusalReason(err) != "store_owned_by_other" {
			t.Fatalf("legacy store: %v", err)
		}
		if after := listTree(t, filepath.Dir(path)); after != before {
			t.Fatalf("Go changed the directory\nbefore: %s\nafter:  %s", before, after)
		}
	})
}
