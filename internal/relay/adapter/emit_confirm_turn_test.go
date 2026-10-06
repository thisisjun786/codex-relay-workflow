package adapter

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// CRW-675: confirmTurn is emit's existence check without --socket. It reads the turn read-only
// through the host the store recorded and reports a turn absent only when the listing was exhausted
// without holding it; a read it could not make is unconfirmed, which leaves emit to stage the
// receipt as before. It selects no store of its own: the socket and the state directory are the
// caller's.
func TestConfirmTurnAnswersAbsenceOnlyFromAnExhaustedListing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		script  []any
		found   bool
		confirm bool
	}{
		{name: "an exhausted listing without the turn", script: []any{}, found: false, confirm: true},
		{name: "a listing that holds the turn", script: []any{map[string]any{"id": "turn-9", "status": "completed", "startedAt": 1700000000}}, found: true, confirm: true},
		{name: "a listing whose page budget runs out", script: []any{}, found: false, confirm: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			host := fakehost.Start(t)
			cursor := any(nil)
			if !c.confirm {
				cursor = "more"
			}
			host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": c.script, "nextCursor": cursor}})
			found, confirmed, err := (hostFactory{}).confirmTurn(context.Background(), t.TempDir(), host.SocketPath, "thread-1", "turn-9")
			// confirmed is the read reaching a definite answer, and err is what it did not: a
			// failure to read is unconfirmed and never reported as absence.
			if found != c.found || confirmed != c.confirm || (err == nil) != c.confirm {
				t.Fatalf("found=%v confirmed=%v err=%v, want found=%v confirmed=%v", found, confirmed, err, c.found, c.confirm)
			}
		})
	}
	t.Run("a host that cannot be reached", func(t *testing.T) {
		t.Parallel()
		found, confirmed, _ := (hostFactory{}).confirmTurn(context.Background(), t.TempDir(), filepath.Join(t.TempDir(), "no-such-host.sock"), "thread-1", "turn-9")
		if found || confirmed {
			t.Fatalf("an unreachable host: found=%v confirmed=%v", found, confirmed)
		}
	})
}
