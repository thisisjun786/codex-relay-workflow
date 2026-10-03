package adapter

import (
	"context"
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A turn that emit names and the host does not know is refused before the receipt intake runs
// (delivery/cli_emit.go reads the turn first), so the refusal text is shown to the caller and is
// never recorded. It is spelled as Go quotes a string.
func TestObserveTurnNamesAnAbsentTurnWithGoQuotes(t *testing.T) {
	host := fakehost.Start(t)
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{}, "nextCursor": nil}})
	t.Setenv("CODEX_SESSION_RELAY_STATE", t.TempDir())
	_, err := observeTurn(context.Background(), "", host.SocketPath, "thread-1", "turn-9")
	var refused *store.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("an absent turn is refused: %v", err)
	}
	if refused.Reason != "unassigned_turn" {
		t.Fatalf("the reason is a contract word and stays: %q", refused.Reason)
	}
	if want := `turn "turn-9" does not exist on "thread-1"`; refused.Detail != want {
		t.Fatalf("detail %q, want %q", refused.Detail, want)
	}
}

// A host that cannot confirm the turn refuses the receipt the same way, with the host's own error
// still reachable underneath.
func TestUnconfirmedTurnNamesTheTurnWithGoQuotes(t *testing.T) {
	cause := &HostUnavailable{"the host is gone"}
	err := unconfirmedTurn("turn-9", cause)
	var refused *store.RefusedError
	if !errors.As(err, &refused) || refused.Reason != "unassigned_turn" {
		t.Fatalf("an unconfirmed turn is refused as unassigned_turn: %v", err)
	}
	if want := `the host could not confirm turn "turn-9": ` + cause.Error(); refused.Detail != want {
		t.Fatalf("detail %q, want %q", refused.Detail, want)
	}
	var unavailable *HostUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatal("the host failure stays reachable through errors.As")
	}
}
