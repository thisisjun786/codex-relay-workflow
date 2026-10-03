package mergeturn

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// HoldingLimitSeconds is how long a holding turn may stay silent before it reads as stalled.
const HoldingLimitSeconds = 1200

// ProgressSteps are the steps a holder records progress for.
var ProgressSteps = []string{"base_refresh", "ci_started", "ci_polled", "ci_result", "merge_attempt"}

// Progress is not implemented yet (red-first stub).
func (s *Service) Progress(ctx context.Context, turn, actor, step, evidence string) (map[string]any, error) {
	return nil, &store.RefusedError{Reason: string(contract.RefusalLinkNotActive), Detail: "progress records are not implemented"}
}
