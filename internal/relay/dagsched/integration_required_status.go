package dagsched

import (
	"context"
	"encoding/json"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

func (s *Scheduler) VerifiedMoveRecord(ctx context.Context, integrationRef, commit string) ([]byte, string, error) {
	rows, err := s.Store.Q(ctx).QueryContext(ctx, "SELECT i.detail FROM dag_integration_stages m JOIN dag_integration_stages i ON i.batch_id = m.batch_id AND i.stage = 'intent' AND i.node_id = '' WHERE m.stage = 'ref_moved' AND m.detail = ? ORDER BY i.rowid DESC", commit)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, "", err
		}
		var intent map[string]string
		if json.Unmarshal([]byte(raw), &intent) != nil || intent["verified_head"] != commit || intent["integration_ref"] != integrationRef || intent["verification_digest"] == "" {
			continue
		}
		receipt := intent["verification_record"]
		if receipt == "" {
			return nil, "", refuse(contract.RefusalDispositionConflict, "the move of %s onto %s kept no verification record; verify the head again before a required status can be published", integrationRef, commit)
		}
		record, err := DecodeVerificationRecord([]byte(receipt))
		if err != nil {
			return nil, "", err
		}
		if record.HeadCommit != commit {
			return nil, "", refuse(contract.RefusalDispositionConflict, "the retained verification of %s names head %s, not %s", integrationRef, record.HeadCommit, commit)
		}
		if intent["verification_base"] != record.BaseCommit {
			return nil, "", refuse(contract.RefusalDispositionConflict, "the retained verification of %s names base %q but the record covers %s", integrationRef, intent["verification_base"], record.BaseCommit)
		}
		if intent["verification_digest"] != sha256Digest([]byte(receipt)) && intent["verification_digest"] != record.Digest {
			return nil, "", refuse(contract.RefusalDispositionConflict, "the retained verification digest of %s matches neither the record bytes nor its canonical digest", integrationRef)
		}
		return []byte(receipt), record.BaseCommit, nil
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return nil, "", refuse(contract.RefusalDispositionConflict, "%s was moved onto %s with no retained verification evidence; verify it again before a required status can be published", integrationRef, commit)
}
