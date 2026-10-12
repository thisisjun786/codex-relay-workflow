package hook

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// ChildBinding is the relay's record of a session for the child push guard (CRW-1284): whether the session is the child of a
// relationship in the store the completion hook's settings select (their dbPath, else the relay's state selection), and the
// parent's working directory. A home without those settings, or a selected store that does not exist, has no relay child; settings
// or a store that cannot be read are an error. The store is read in place and is never written.
func ChildBinding(ctx context.Context, session string) (child bool, parentCwd string, err error) {
	path, err := configurationPath("", nil)
	if err != nil {
		return false, "", err
	}
	config, problem, detail := ReadSettings(ctx, path)
	if problem == "config_absent" {
		return false, "", nil
	}
	if problem != "" {
		return false, "", errors.New(problem + ": " + detail)
	}
	db := pyjson.Text(config.Get("dbPath"))
	if db == "" {
		state, err := RoutingState(config)
		if err != nil {
			return false, "", err
		}
		db = filepath.Join(state, "relay.sqlite3")
	}
	if _, err := os.Stat(db); errors.Is(err, os.ErrNotExist) {
		return false, "", nil
	}
	ro, err := store.OpenStopRead(ctx, db, 2*time.Second)
	if err != nil {
		return false, "", err
	}
	defer ro.Close()
	var parent sql.NullString
	err = ro.QueryRowContext(ctx, "SELECT parent_cwd FROM relationships WHERE child_task_id = ? ORDER BY updated_at DESC LIMIT 1", session).Scan(&parent)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	return err == nil, parent.String, err
}
