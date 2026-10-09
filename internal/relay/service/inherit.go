package service

import (
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/fdsweep"
)

// ErrDescriptorsUnlisted is the sweep's refusal when no way reaches every open descriptor (see fdsweep).
var ErrDescriptorsUnlisted = fdsweep.ErrDescriptorsUnlisted

// closeOnExecInherited marks every descriptor above 2 close-on-exec before a daemon is spawned, or
// fails when it cannot, so that the start is refused (CRW-1057). The sweep is internal/relay/fdsweep,
// which the job start (CRW-1081) uses too. It is a variable so that a test can make it fail.
var closeOnExecInherited = func() error {
	if err := fdsweep.MarkInherited(); err != nil {
		return fmt.Errorf("start refused, the caller's open descriptors cannot all be kept out of the daemon: %w", err)
	}
	return nil
}
