//go:build !contracttest

package sync

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func cliOutbox(s *store.Store) *Outbox { return New(s, delivery.SystemClock{}) }
