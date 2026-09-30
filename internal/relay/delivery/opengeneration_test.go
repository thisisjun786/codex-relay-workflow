package delivery

import (
	"context"
	"database/sql"
	"testing"
)

// Carried from the todo 25 checker: the verdict path composes OpenGenerationIn inside its own
// transaction, and a replay of the same dispatch request id must return its generation rather
// than open another. The three answers and every table row are checked against the golden, which
// began as registry.open_generation_in's over the same fixture.
func Test21_OpenGenerationIn_replays_a_dispatch_request_rather_than_opening_another(t *testing.T) {
	tree := parityTree(t)
	expected := expectScenario(t, tree, "ogi")
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
	expected.out(got)
	expected.tables(f)
}
