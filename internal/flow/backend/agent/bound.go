package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// This file is the run's half of F14 (CRW-1207): which Codex threads are the run's own, told to the native spawn hook through
// the flow-bound files of internal/flow/bound. Before every turn/start of an engine thread, and so before the first turn of a
// thread that thread/start has just answered, the backend appends a line {run_id, thread_id, kind engine, call_id, role,
// nativeAllowance, helperPairs{explorer}, policyDigest} to the thread's file and makes it durable. A line that cannot be
// written, or a thread whose id is not the form of a thread id, sends no turn: the call is held, host_config_flow_binding.
// The thread that orchestrates the run is bound by BindOrchestrator when the run starts and released by CloseOrchestrator when it
// ends or is cancelled. The hook reads the files; it applies the helper pair of the line to the one native helper an engine
// call may spawn.

// BoundConfig turns the binding on for a backend. A backend without one writes none: the hook then takes every thread of the
// run for an ordinary one, so a process that starts or resumes runs on the App Server sets it (internal/flow/journal.ResumeEnv
// does for `crw flow resume`).
type BoundConfig struct {
	// RunID is the run the lines name.
	RunID string
	// Policy is the run's pinned role table: the explorer pair is the helper pair of an engine line, the explorer and the
	// reviewer pair those of the orchestrator's line, and its digest is the policyDigest of every line.
	Policy *engine.Policy
	// Writer appends the lines; nil is a writer over bound.Dir.
	Writer *bound.Writer
	// NativeAllowance is how many native helpers the call of a turn may spawn (0 or 1); nil gives 0 to an explorer call and to
	// the QA capture call (SP-QA-1, the turns marked qa: the run starts the oracle passes itself) and 1 to every other.
	NativeAllowance func(engine.TurnSpec) int
}

func (c *BoundConfig) validate() error {
	switch {
	case c.RunID == "":
		return errors.New("agent: the flow binding needs the run id")
	case c.Policy == nil:
		return errors.New("agent: the flow binding needs the run's policy")
	}
	for _, role := range []string{bound.HelperExplorer, bound.HelperReviewer} {
		if spec, ok := c.Policy.Roles[role]; !ok || spec.Pair.Model == "" || spec.Pair.Effort == "" {
			return fmt.Errorf("agent: the policy has no %s pair for the flow binding", role)
		}
	}
	return nil
}

func (c *BoundConfig) writer() *bound.Writer {
	if c.Writer != nil {
		return c.Writer
	}
	return &bound.Writer{}
}

func (c *BoundConfig) pair(role string) bound.Pair {
	p := c.Policy.Roles[role].Pair
	return bound.Pair{Model: p.Model, Effort: p.Effort}
}

func (c *BoundConfig) allowance(spec engine.TurnSpec) int {
	if c.NativeAllowance != nil {
		return min(1, max(0, c.NativeAllowance(spec)))
	}
	if spec.Role == bound.HelperExplorer || spec.QA {
		return 0
	}
	return 1
}

// bindError is a binding that could not be written, as an explicit answer the engine holds the call for.
func bindError(err error) error {
	return &engine.RPCError{Message: "the flow binding of the thread could not be written: " + err.Error(), CodexErrorInfo: engine.CodexErrorFlowBinding}
}

// bindTurn writes the engine line of a turn. It is called before the turn/start request is built, so a failure sends nothing.
func (b *Backend) bindTurn(spec engine.TurnSpec) error {
	c := b.cfg.Bound
	if c == nil {
		return nil
	}
	allowance := c.allowance(spec)
	line := bound.Line{
		RunID: c.RunID, ThreadID: spec.ThreadID, Kind: bound.KindEngine, CallID: spec.CallID, Role: spec.Role,
		NativeAllowance: &allowance, HelperPairs: map[string]bound.Pair{bound.HelperExplorer: c.pair(bound.HelperExplorer)},
		PolicyDigest: c.Policy.Digest,
	}
	if err := c.writer().Append(line); err != nil {
		return bindError(err)
	}
	return nil
}

var errNoBinding = errors.New("agent: the backend has no flow binding configured")

// BindOrchestrator binds the thread that orchestrates the run: it appends the opening line (the pairs of the seven roles of the
// policy, no allowance) to the thread's file, through bound.BindOrchestrator. The run calls it when it starts; until
// CloseOrchestrator, the hook gives the spawns of that thread the pair of their role.
func (b *Backend) BindOrchestrator(ctx context.Context, thread string) error {
	c := b.cfg.Bound
	if c == nil {
		return errNoBinding
	}
	pairs := map[string]bound.Pair{}
	for _, role := range bound.OrchestratorRoles {
		pairs[role] = c.pair(role)
	}
	if _, err := bound.BindOrchestrator(bound.OrchestratorBinding{Writer: c.writer(), RunID: c.RunID, Thread: thread, Digest: c.Policy.Digest, Pairs: pairs}); err != nil {
		return bindError(err)
	}
	b.mu.Lock()
	b.orchestrator = thread
	b.mu.Unlock()
	return nil
}

// CloseOrchestrator appends the closed line to the file of the thread BindOrchestrator bound, when the run ends or is
// cancelled (bound.CloseOrchestrator); the thread is an ordinary one again. It does nothing when no thread is bound.
func (b *Backend) CloseOrchestrator(ctx context.Context) error {
	c := b.cfg.Bound
	b.mu.Lock()
	thread := b.orchestrator
	b.mu.Unlock()
	if c == nil || thread == "" {
		return nil
	}
	if _, err := bound.CloseOrchestrator(c.writer(), c.RunID, thread); err != nil {
		return bindError(err)
	}
	b.mu.Lock()
	if b.orchestrator == thread {
		b.orchestrator = ""
	}
	b.mu.Unlock()
	return nil
}

// TurnItems reads the items of a turn by its recorded id, in full: the CollabAgentToolCall items among them are what the run
// joins with the hook's ledger (ops.ImportNativeSpawns). It is the ops.NativeHost of the import.
func (b *Backend) TurnItems(ctx context.Context, threadID, turnID string) ([]json.RawMessage, error) {
	readings, err := b.client.Bus().ReconcileTurns(ctx, threadID, []string{turnID})
	if err != nil {
		return nil, requestError(err)
	}
	r := readings[0]
	switch {
	case !r.Found:
		return nil, fmt.Errorf("the host lists no turn %s", turnID)
	case !r.Readable:
		return nil, &UnreadableTurnError{ThreadID: threadID, TurnID: turnID, Cause: r.ReadError}
	}
	return r.Items, nil
}

// ThreadConfig is thread/read of a thread: Thread.model and Thread.reasoningEffort, nil for a null. It is the configured pair of a
// helper a native spawn created, not per-turn telemetry. includeTurns is never sent.
func (b *Backend) ThreadConfig(ctx context.Context, threadID string) (model, effort *string, err error) {
	raw, err := b.client.Call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": false})
	if err != nil {
		return nil, nil, requestError(err)
	}
	read, err := decodeThread(raw, "thread/read")
	if err != nil {
		return nil, nil, err
	}
	return read.Model, read.ReasoningEffort, nil
}
