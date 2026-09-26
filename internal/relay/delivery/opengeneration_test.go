package delivery

import (
	"context"
	"database/sql"
	"testing"
)

// Carried from the todo 25 checker: the verdict path composes OpenGenerationIn inside its own
// transaction, and a replay of the same dispatch request id must return its generation rather
// than open another. Compared with registry.open_generation_in over the same fixture: the three
// answers and every table row.
func Test21_OpenGenerationIn_replays_a_dispatch_request_rather_than_opening_another(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "ogi")
	f := newFixture(t, tree)
	f.queuedEvent(regOpts{})
	open := func(request string) int64 {
		var number int64
		mustDo(t, f.store.Transaction(f.ctx, func(ctx context.Context, _ *sql.Conn) error {
			var err error
			number, err = OpenGenerationIn(ctx, f.store, f.clock, f.rid, request, "needs_changes_revision", nil)
			return err
		}))
		return number
	}
	got := map[string]any{"first": open("revision-x"), "replay": open("revision-x"), "next": open("revision-y")}
	requireSameJSON(t, "open_generation_in answers", got, python.Out)
	requireSameTables(t, f, python)
}
