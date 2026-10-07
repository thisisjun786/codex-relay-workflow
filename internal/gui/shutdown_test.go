package gui

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/policystore"
)

// TestTheShutdownWaitsForAnInFlightWrite is the pre-merge evaluation's d3: the GUI ends the process
// when its shutdown grace runs out, and a write that has already replaced the policy file must
// finish registering it first. Ending the process in between leaves the file and the wiring record
// naming different digests, which no bridge starts under, so the grace must cover the write's own
// post-publication bound.
func TestTheShutdownWaitsForAnInFlightWrite(t *testing.T) {
	server, err := New(Options{Port: guardPort, Token: guardToken, Version: "test-version"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if server.shutdown < policystore.WriteSettleBound() {
		t.Fatalf("the shutdown grace is %s, shorter than a write's settle bound %s: a first SIGINT can end the process between the policy replacement and its registration", server.shutdown, policystore.WriteSettleBound())
	}
}
