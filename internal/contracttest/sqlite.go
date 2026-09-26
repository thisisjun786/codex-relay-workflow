package contracttest

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// runSQLite replays the sqlite-ddl fixture's seeded-store query through the real Go store.
func runSQLite(t *testing.T, scenario Scenario) (map[string]any, error) {
	t.Helper()
	home := t.TempDir()
	state := filepath.Join(home, "state")
	s, err := store.Open(context.Background(), filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		return nil, err
	}
	if err := s.Close(); err != nil {
		return nil, err
	}
	script, ok := scenario.Given["sql_seed"].(string)
	if !ok {
		return nil, fmt.Errorf("%w: sqlite-ddl missing sql_seed", ErrFixture)
	}
	if err := seedSQL(state, script); err != nil {
		return nil, err
	}
	queries, err := querySQL(state, scenario.Expect.Queries)
	if err != nil {
		return nil, err
	}
	return map[string]any{"exit": float64(0), "sql": queries}, nil
}
