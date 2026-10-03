package mergeturn

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Pass is not implemented yet (red-first stub).
func (s *Service) Pass(ctx context.Context, turn, actor, evidence string) (map[string]any, error) {
	return nil, &store.RefusedError{Reason: string(contract.RefusalLinkNotActive), Detail: "passing a stalled turn is not implemented"}
}
