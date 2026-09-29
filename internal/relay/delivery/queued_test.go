package delivery

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A queued ack replays with the drainer's host when the drainer has a socket, as cmd_ack runs
// with the drainer's adapter and reconciler: the proof check that precedes any host read then
// refuses a malformed event ID as a host error (the entry is kept, as Python keeps it), where
// the host-less replay records the offline refusal. The replay never opens the store again: it
// runs inside the drainer's own transaction.
func TestApplyQueued_ack_uses_the_drainers_host(t *testing.T) {
	state := t.TempDir()
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	previous := QueuedAckHost
	defer func() { QueuedAckHost = previous }()
	var opened []string
	QueuedAckHost = func(ctx context.Context, socket string, clock Clock) (Adapter, func() error, error) {
		opened = append(opened, socket)
		return newFakeHost(NewFakeClock()), func() error { return nil }, nil
	}
	argv := []string{"--event=event-1", "--ack-turn=parent-turn", "--ack-proof=proof-1"}
	apply := func(socket string) (any, int, error) {
		var answer any
		var code int
		var failure error
		err := st.Compose(t.Context(), func(ctx context.Context, _ *sql.Conn) error {
			answer, code, failure = ApplyQueued(ctx, st, "ack", argv, socket)
			return nil
		})
		return answer, code, errors.Join(err, failure)
	}
	answer, code, err := apply("")
	if err != nil || code != 2 || len(opened) != 0 {
		t.Fatalf("offline: %v %d %v %v", answer, code, err, opened)
	}
	_, code, err = apply("/run/app.sock")
	if err == nil || err.Error() != "ValueError: event id must be 32 lowercase hex characters" || code != 3 || len(opened) != 1 || opened[0] != "/run/app.sock" {
		t.Fatalf("with a host: %d %v %v", code, err, opened)
	}
}
