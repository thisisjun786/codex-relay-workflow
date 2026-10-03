package adapter

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// Replay is checked inside the composing transaction, before active-status checks
// or another generation is inserted. The second call observes uncommitted writes.
func Test28_OpenGenerationInInnerReplay(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := seedIntake(t, filepath.Join(root, "state.sqlite3"), root)
	ctx := context.Background()
	err := s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		first, err := delivery.OpenGenerationIn(ctx, s, delivery.NewFakeClock(), "rel-1", "retry-request", "needs_changes_revision", nil)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE relationships SET status='paused' WHERE relationship_id='rel-1'"); err != nil {
			return err
		}
		again, err := delivery.OpenGenerationIn(ctx, s, delivery.NewFakeClock(), "rel-1", "retry-request", "needs_changes_revision", nil)
		if err != nil {
			return err
		}
		if first != 2 || again != first {
			t.Fatalf("replay opened %d then %d", first, again)
		}
		var count int
		if err := s.Querier(ctx).QueryRowContext(ctx, "SELECT COUNT(*) FROM generations WHERE dispatch_request_id='retry-request'").Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			t.Fatalf("replay rows=%d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
