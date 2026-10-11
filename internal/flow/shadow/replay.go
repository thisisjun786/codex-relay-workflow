package shadow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace"
)

// Replay is the backend a shadow run executes on: it starts no agent. A turn of a call the trace recorded is answered with what
// the recorded call came to (its last message, its tokens, or the failure the host reported); a turn of a call the trace does
// not hold (the script asked for something the recorded run never did) is answered by the deterministic fake, and counted.
// Calls are matched by call id, which the engine derives from the run id, the call's path and its position, so the script
// that recorded the run and a script that issues the same calls in the same places meet the same answers.
type Replay struct {
	calls    map[string]trace.Call
	fallback *engine.Fake

	mu       sync.Mutex
	replayed map[string]bool
	invented map[string]bool
	batch    map[string]bool
}

// NewReplay returns the backend for the calls of one recorded run.
func NewReplay(calls []trace.Call) *Replay {
	r := &Replay{calls: map[string]trace.Call{}, fallback: engine.NewFake(), replayed: map[string]bool{}, invented: map[string]bool{}, batch: map[string]bool{}}
	for _, c := range calls {
		r.calls[c.CallID] = c
	}
	return r
}

// StartThread implements engine.Backend: a thread is a name here, never a request.
func (r *Replay) StartThread(ctx context.Context, spec engine.ThreadSpec) (engine.ThreadInfo, error) {
	return r.fallback.StartThread(ctx, spec)
}

// RunTurn implements engine.Backend.
func (r *Replay) RunTurn(ctx context.Context, spec engine.TurnSpec) (engine.TurnOutcome, error) {
	if len(spec.Schema) > 0 && strings.HasPrefix(spec.Role, "reviewer") && batchSchema(spec.Schema) {
		sum := sha256.Sum256(spec.Schema)
		r.mu.Lock()
		r.batch[hex.EncodeToString(sum[:])] = true
		r.mu.Unlock()
	}
	c, ok := r.calls[spec.CallID]
	if !ok || !replayable(c) {
		r.mu.Lock()
		r.invented[spec.CallID] = true
		r.mu.Unlock()
		return r.fallback.RunTurn(ctx, spec)
	}
	r.mu.Lock()
	r.replayed[spec.CallID] = true
	r.mu.Unlock()
	turnID := fmt.Sprintf("replay_%s/t%d", spec.CallID, spec.K)
	if spec.Started != nil {
		spec.Started(turnID)
	}
	out := engine.TurnOutcome{TurnID: turnID}
	switch {
	case c.Status == engine.StatusCompleted:
		out.Text = c.Result
		out.Tokens = engine.Tokens{Input: c.Tokens.Input, Output: c.Tokens.Output, Total: c.Tokens.Total}
		out.TokensUnmeasured = c.TokensUnmeasured
	case c.Status == engine.StatusUnknown:
		return engine.TurnOutcome{}, &engine.TransportError{Err: fmt.Errorf("the recorded call %s had no answer", c.CallID)}
	case c.Reason == engine.ReasonSchemaInvalidAfterRepair:
		out.Text = "recorded answer that does not satisfy the output schema"
	case c.Reason == engine.ReasonHostInterruptedExternal:
		out.Failed = &engine.TurnFailure{Interrupted: true, Message: "recorded interruption"}
	default:
		out.Failed = &engine.TurnFailure{CodexErrorInfo: hostKind(c.Reason), Message: "recorded failure " + c.Reason}
	}
	return out, nil
}

// replayable reports whether the recorded call ended in a way the host decided; a call the engine ended itself (a refusal of
// its own, a supersession, an open call) is decided again by the engine on the replayed run.
func replayable(c trace.Call) bool {
	switch c.Status {
	case engine.StatusCompleted, engine.StatusUnknown:
		return true
	case engine.StatusHeld, engine.StatusRefused, engine.StatusNullModel:
		return hostKind(c.Reason) != "" || c.Reason == engine.ReasonSchemaInvalidAfterRepair || c.Reason == engine.ReasonHostInterruptedExternal
	}
	return false
}

// hostKind is the codexErrorInfo variant a recorded reason came from, "" for a reason the engine itself gives.
func hostKind(reason string) string {
	switch {
	case reason == engine.ReasonBudgetExhausted:
		return "sessionBudgetExceeded"
	case reason == engine.ReasonContextWindowExceeded:
		return "contextWindowExceeded"
	}
	for _, prefix := range []string{"host_infra_", "host_config_", "host_policy_"} {
		if kind, ok := strings.CutPrefix(reason, prefix); ok {
			return kind
		}
	}
	return ""
}

// Replayed is how many calls were answered from the trace, and Invented how many were not in it.
func (r *Replay) Replayed() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.replayed) }
func (r *Replay) Invented() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.invented) }

// BatchSchemas are the digests of the batch verdict schemas the replayed run's reviewer calls sent.
func (r *Replay) BatchSchemas() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]bool{}
	for k := range r.batch {
		out[k] = true
	}
	return out
}

// probeScript sends the batch verdict schema of the helper library to a reviewer, once.
const probeScript = `import { VERDICT_BATCH } from 'crw:lib/schemas'
export const meta = { name: 'batch-schema-probe' }
await agent.call('probe', { agentType: 'reviewer', schema: VERDICT_BATCH })
return null
`

// BatchSchemas are the digests of the batch verdict schema as the engine journals them (schema_digest of a prepared row). They
// are found by sending the library's schema through the engine to a sniffing backend, so a recorded call that reviews a batch is
// told from the digest alone, whether or not the script being replayed makes such a call.
func BatchSchemas(ctx context.Context) (map[string]bool, error) {
	prog, err := script.Load(script.Source{Name: "probe.js", Text: []byte(probeScript)})
	if err != nil {
		return nil, err
	}
	backend := NewReplay(nil)
	if _, err := engine.Run(ctx, prog, engine.Config{RunID: "probe", Backend: backend, Policy: engine.FakePolicy(), OmitSeq: true}); err != nil {
		return nil, err
	}
	return backend.BatchSchemas(), nil
}
