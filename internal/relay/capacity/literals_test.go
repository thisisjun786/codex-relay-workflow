package capacity

import (
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
)

// The one reason word this package emits outside errors.RefusalReason: region-settle's own
// refusal of an acceptance carrying --condition (cli.py:1433), printed whole with exit 2.
// TestRegionCommands in internal/contracttest compares the printed bytes with Python's.
func Test27_region_settle_bad_invocation_is_spelled_as_python_spells_it(t *testing.T) {
	var payload *cli.PayloadExit
	if !errors.As(settleConditionRefusal(), &payload) {
		t.Fatal("not a payload exit")
	}
	if payload.Code != contract.ExitRefused || get(payload.Payload, "reason") != "bad_invocation" || get(payload.Payload, "ok") != false {
		t.Fatalf("%v", payload)
	}
}
