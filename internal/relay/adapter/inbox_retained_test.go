package adapter

import (
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/inbox"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A receipt the host could not confirm is refused as Python refuses it (unassigned_turn, exit
// 2) and keeps the HostUnavailable it was decided on, as `raise ReceiptRefused(...) from
// error` does: the takeover inbox retains such an entry instead of recording the refusal.
// Any other read failure stays the read's own and is not retained.
func TestUnconfirmedTurn_refusal_keeps_the_host_failure_for_the_inbox(t *testing.T) {
	err := unconfirmedTurn("turn-1", &HostUnavailable{"listing not exhausted"})
	var refused *store.RefusedError
	if !errors.As(err, &refused) || refused.Reason != "unassigned_turn" || refused.Detail != "the host could not confirm turn 'turn-1': listing not exhausted" {
		t.Fatalf("%v", err)
	}
	if !inbox.Retained(err) {
		t.Fatal("a host-unconfirmed receipt refusal is not retained")
	}
	other := errors.New("broken pipe")
	if got := unconfirmedTurn("turn-1", other); got != other || inbox.Retained(got) {
		t.Fatalf("%v", got)
	}
}
