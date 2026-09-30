package managed

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Code the product no longer calls, kept for the tests that drive it (decision 52).

func (r startResult) admitted() contract.OrderedObject {
	return r.result("admitted", "business_accepted", nil)
}

// LastObservation reads the journal's most recent managed receipt without changing it.
func LastObservation(ctx context.Context, s *store.Store, requestID string) (any, error) {
	var raw string
	err := s.Querier(ctx).QueryRowContext(ctx, "SELECT detail FROM journal WHERE kind='managed_start_observed' AND subject=? ORDER BY rowid DESC LIMIT 1", requestID).Scan(&raw)
	if err != nil {
		return nil, err
	}
	var result any
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}
