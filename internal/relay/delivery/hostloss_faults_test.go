package delivery

import (
	"context"
	"path/filepath"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
)

// The fault half of UnknownSendCase: fault_states() sweeps the store and records the batch
// (faultsweep.sweep + record_all), through the todo-21 subset of internal/relay/faults.

func (h *hl) sweeper() *faults.Sweeper {
	return &faults.Sweeper{Store: h.store, MaxAttempts: h.delivery.Policy.MaxAttempts, Now: faults.WallClockISO,
		Installation: faults.Installation{Package: "codex-session-relay", Version: "0.1.0",
			Location: filepath.Join(repoRoot(h.t), "packages", "codex-session-relay", "src", "codex_session_relay")},
		Current: func(ctx context.Context, event string) (bool, error) {
			reason, err := h.delivery.SupersessionReason(ctx, event)
			return reason == "", err
		}}
}

// faultStates is fault_states(): the delivery_stalled faults as (state, severity), sorted.
func (h *hl) faultStates() []any {
	h.t.Helper()
	sw := h.sweeper()
	batch, err := sw.Sweep(h.ctx, "crw")
	mustDo(h.t, err)
	mustDo(h.t, sw.RecordAll(h.ctx, &faults.Ledger{Store: h.store, Clock: h.clock}, batch))
	rows, err := all(h.ctx, h.store, "SELECT state, severity FROM fault_ledger WHERE fault_class = 'delivery_stalled'")
	mustDo(h.t, err)
	var pairs [][2]string
	for _, r := range rows {
		pairs = append(pairs, [2]string{r.S("state"), r.S("severity")})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0]+"|"+pairs[i][1] < pairs[j][0]+"|"+pairs[j][1] })
	out := []any{}
	for _, p := range pairs {
		out = append(out, []any{p[0], p[1]})
	}
	return out
}

// faultIn is assertIn((state, severity), self.fault_states()).
func (h *hl) faultIn(state, severity string) bool {
	for _, p := range h.faultStates() {
		pair := p.([]any)
		if pair[0] == state && pair[1] == severity {
			return true
		}
	}
	return false
}
