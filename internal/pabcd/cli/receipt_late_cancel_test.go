package cli

import (
	"context"
	"testing"
)

// A cancellation that lands after the check command has returned and the source has been captured
// again still refuses publication, as the oracle's deferred signal refuses it: the runner checks the
// context at the last moment publication can still be skipped. The seam (receiptLateCancelHook)
// exists only to reach that window deterministically - the real one is a race.
func TestReceiptLateCancellationRefusesPublication(t *testing.T) {
	root := receiptRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	receiptLateCancelHook = cancel
	defer func() { receiptLateCancelHook = nil }()
	a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "exit", "0")}
	got := receiptRun(t, a, ReceiptRunOptions{Context: ctx})
	if got.Code != 1 || got.Output != "receipt test: the command did not run to completion (interrupted); no receipt written" {
		t.Fatalf("late cancellation: %#v", got)
	}
	receiptAbsent(t, root)
}
