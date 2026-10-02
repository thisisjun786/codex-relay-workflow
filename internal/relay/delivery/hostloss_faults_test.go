package delivery

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
)

// The fault half of UnknownSendCase: fault_states() sweeps the store and records the batch
// (faultsweep.sweep + record_all), through the todo-21 subset of internal/relay/faults.

// wallClockISO is the sweep's time now, as the fault rows store a time.
func wallClockISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00") }

func (h *hl) sweeper() *faults.Sweeper {
	return &faults.Sweeper{Store: h.store, MaxAttempts: h.delivery.Policy.MaxAttempts, Now: wallClockISO,
		HostRecordPath: filepath.Join(os.Getenv("XDG_STATE_HOME"), "codex-relay-workflow", "host-record.json"),
		Installation: faults.Installation{Package: "codex-session-relay", Version: faults.RelayPackageVersion,
			Location: installationDir()},
		SupersessionReason: h.delivery.SupersessionReason,
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
	_, err = sw.RecordAll(h.ctx, &faults.Ledger{Store: h.store, Clock: h.clock}, batch)
	mustDo(h.t, err)
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
