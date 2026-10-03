package faults_test

// The relay daemon reads its settled managed turns through supervisor.OmissionObserver, delivery's omission
// judgment, which the faults package cannot import. The tests of faults that exercise the sweep against
// real marker files install it here, the way contacts_wiring_test.go installs the contact reading.

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

func init() { faults.SetRealManagedObserver(supervisor.OmissionObserver{}) }
