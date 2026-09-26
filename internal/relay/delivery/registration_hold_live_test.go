package delivery

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	_ "modernc.org/sqlite"
)

func Test21_RegistrationHold_refuses_resolved_live_state(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "xdg", "codex-session-relay", "default")
	if err := os.MkdirAll(live, 0700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(live, "relay.sqlite3")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE probe (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias.sqlite3")
	// An outside alias resolves into a temp directory shaped like live state.
	if err := os.Symlink(dbPath, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg"))
	t.Setenv("CRW_ALLOW_LIVE_STATE", "")
	called := false
	err = store.RegistrationHold(context.Background(), alias, func(conn *sql.Conn, why string) error {
		called = true
		if conn != nil || !strings.Contains(why, store.ErrLiveState.Error()) {
			t.Errorf("hold conn=%v why=%q", conn, why)
		}
		return nil
	})
	if err != nil || !called {
		t.Fatalf("hold called=%v err=%v", called, err)
	}
}
