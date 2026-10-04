package childcleanup

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
)

// completedIdle reads history first, then uncached identity/status immediately
// before archive. Independent clients can still start a turn after this read.
func (d *discovery) completedIdle(ctx context.Context, id string) (hold, detail string, err error) {
	raw, err := d.host.Call(ctx, "thread/turns/list", map[string]any{"threadId": id, "limit": 1, "itemsView": "summary"})
	if err != nil {
		var refusal *appserver.RPCError
		if !errors.As(err, &refusal) {
			return "", "", err
		}
	}
	var page struct{ Data []struct{ ID, Status string } }
	historyOK := err == nil && json.Unmarshal(raw, &page) == nil && len(page.Data) == 1 && page.Data[0].ID != ""
	delete(d.cache, id)
	delete(d.fail, id)
	r, ok, err := d.read(ctx, id)
	if err != nil {
		return "", "", err
	}
	if r.status == "active" || historyOK && page.Data[0].Status == "inProgress" {
		return OutcomeHeldActive, id + " has a running turn", nil
	}
	if !ok || !historyOK || r.status != "idle" || page.Data[0].Status != "completed" {
		return OutcomeHeldIncomplete, id + " is not idle with a completed latest turn", nil
	}
	return "", "", nil
}
