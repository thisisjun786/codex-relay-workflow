package managed

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type armedReplayKey struct{}

// ArmedReplay reports eligibility proven by Run against an existing, exact-fingerprint
// armed/attached request. It is never a field supplied by the caller's request.
func ArmedReplay(ctx context.Context) bool {
	eligible, _ := ctx.Value(armedReplayKey{}).(bool)
	return eligible
}

// PropagateArmedReplay copies proven eligibility onto a service-owned context without
// transferring the caller's cancellation or allowing a caller to manufacture eligibility.
func PropagateArmedReplay(target, source context.Context) context.Context {
	return context.WithValue(target, armedReplayKey{}, ArmedReplay(source))
}

func replayState(state string) bool { return state == "create_armed" || state == "attached" }

// Guards re-prove eligibility on their unchanged store before turn/start. An invalid
// proof clears the exception; readiness and the existing guard checks still decide.
func recheckReplay(ctx context.Context, read *store.ReadOnly, id Identity) context.Context {
	var fingerprint, state string
	err := read.QueryRowContext(ctx, "SELECT request_fingerprint,state FROM managed_start_requests WHERE request_id=?", id.RequestID).Scan(&fingerprint, &state)
	return context.WithValue(ctx, armedReplayKey{}, err == nil && fingerprint == id.Fingerprint && replayState(state))
}
