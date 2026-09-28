//go:build contracttest

package sync

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Contract-only binary injection: production never reads a test clock or token environment.
func cliOutbox(s *store.Store) *Outbox {
	o := New(s, delivery.NewFakeClock())
	o.Token = func() (string, error) { return "0123456789abcdef0123456789abcdef", nil }
	return o
}
