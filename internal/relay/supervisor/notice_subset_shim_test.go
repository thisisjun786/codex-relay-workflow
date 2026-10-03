package supervisor

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
)

// The notice subset characterization tests (notice_subset_test.go) call the pieces that move out of
// faults through these two names, so the move changes this file and nothing else in them.

func composeNoticeForTest(notice, live map[string]any, observedAt, evidence string) (map[string]any, error) {
	return faults.ComposeNotice(notice, live, observedAt, evidence)
}

func parkNoticeForTest(ctx context.Context, n NoticeChannel, messageID, reason string) error {
	return n.Ledger.ParkNotice(ctx, messageID, reason)
}
