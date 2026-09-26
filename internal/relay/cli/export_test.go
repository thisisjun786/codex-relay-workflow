package cli

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// SetClock fixes status's clock for a comparison against Python run under the same FakeClock.
func SetClock(now float64) func() {
	previous := clockNow
	clockNow = func() float64 { return now }
	return func() { clockNow = previous }
}

// RelayProgram exposes the command prefix rendered into recovery lines.
func RelayProgram() []string { return relayProgram() }

// FaultAttention exposes the status fault block for the transaction test.
var FaultAttention = faultAttention

// AccessReceipt drives _access_receipt with a probe measured earlier, as the Python tests call
// it directly: the store may have been replaced between that probe and this read.
func AccessReceipt(ctx context.Context, state string, probed store.ProbeResult) contract.OrderedObject {
	selection, _ := store.ResolveStateDir(state, "")
	return accessReceipt(ctx, Services{Selection: selection}, probeStore(probed), probed.Access)
}
