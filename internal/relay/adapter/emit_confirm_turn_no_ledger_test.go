package adapter

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-680: confirmTurn reads the turn without a ledger. CRW-675 left one behind: it went through
// adapter.Open, which opens the operations ledger before the read, so a refused emit left a state
// file in the directory that serves the socket. This test is serial because it sets the state-home
// environment the socket's directory is discovered under (t.Setenv refuses a parallel test).
func TestCRW680_confirmTurn_creates_no_operations_ledger(t *testing.T) {
	state := t.TempDir()
	t.Setenv("CODEX_SESSION_RELAY_STATE", state)
	host := fakehost.Start(t)
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-9", "status": "completed", "startedAt": 1700000000}}, "nextCursor": nil}})
	found, confirmed, err := (hostFactory{}).confirmTurn(context.Background(), state, host.SocketPath, "thread-1", "turn-9")
	if err != nil || !found || !confirmed {
		t.Fatalf("found=%v confirmed=%v err=%v", found, confirmed, err)
	}
	selection, err := store.ResolveStateDir("", host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(selection.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "operations-") {
			t.Fatalf("confirmTurn left %s in the socket's state directory", entry.Name())
		}
	}
}
