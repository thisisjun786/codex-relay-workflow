package dagsched

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// headCompareReportForTurn records a work report of the relationship a merge turn was claimed with, naming head. The merge
// lane compares the head a check restates with the work reports of the turn's relationship when the pull request is not on a
// forge repository (CRW-586), and nothing in the product records one (docs/port/decisions.md, section 53), so the tests whose
// repository is a local path record the head they ask the lane to check.
func headCompareReportForTurn(t *testing.T, s *store.Store, turn, head string) {
	t.Helper()
	ctx := context.Background()
	var relationship string
	if err := s.DB.QueryRowContext(ctx, "SELECT relationship_id FROM merge_turns WHERE turn_id = ?", turn).Scan(&relationship); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,base_ref,head_sha,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"event-hc-"+turn, 1, relationship, 1, "rev", "owner/repo", "dev", head, "done", "r", "1", "s", "n", "t"); err != nil {
		t.Fatal(err)
	}
}
