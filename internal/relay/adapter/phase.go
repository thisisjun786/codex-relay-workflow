package adapter

import (
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// The bridge retains typed phase information; the relay renders the Python caller's
// detail rather than changing the existing bridge error contract.
func phaseText(e *appserver.PhaseTimeout) string {
	seconds := pyjson.Float(e.Bound.Seconds())
	switch e.Phase {
	case "establish":
		return e.Method + ": connection establishment exceeded " + seconds + "s; no " + e.Method + " frame was sent"
	case "transmit":
		return e.Method + ": the request frame did not drain within " + seconds + "s and the connection was retired; response unavailable; do not resend"
	default:
		return e.Method + ": response unavailable; do not resend"
	}
}

// notSent reports a request the transport certainly never delivered: the client withheld it before
// writing its frame, or the connection could not be established. A lost answer is not one.
func notSent(err error) bool {
	var phase *appserver.PhaseTimeout
	return appserver.Withheld(err) || (errors.As(err, &phase) && phase.Phase == "establish")
}
