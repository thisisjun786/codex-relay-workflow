package cli

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
