package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// threadParams are the members thread/start and thread/fork share: the call's own cwd, the role's pair, the sandbox of
// the role, no approvals, a persistent thread and the role's prompt - and nothing that names the call. The prompt is the
// only thing written to developerInstructions: the 0.154 Thread has no such member, so an identity put there could never
// be read back.
func (b *Backend) threadParams(role, cwd, sandbox string, pair engine.Pair, limit *int64) (map[string]any, error) {
	if !filepath.IsAbs(cwd) {
		return nil, &engine.RPCError{Message: "a thread starts in an absolute working directory, got " + cwd, CodexErrorInfo: "badRequest"}
	}
	contract := settings.Contract{CWD: cwd, Sandbox: sandbox, Model: pair.Model, ReasoningEffort: pair.Effort, AutoCompactTokenLimit: limit}
	if err := contract.Validate(); err != nil {
		return nil, &engine.RPCError{Message: err.Error(), CodexErrorInfo: "badRequest"}
	}
	prompt, err := b.cfg.Prompt(role)
	if err != nil {
		return nil, &engine.RPCError{Message: err.Error(), CodexErrorInfo: "badRequest"}
	}
	params := contract.StartParams()
	params["cwd"] = cwd
	params["approvalPolicy"] = "never"
	params["ephemeral"] = false
	params["developerInstructions"] = prompt
	return params, nil
}

// StartThread implements engine.Backend: thread/start, then thread/read for the pair the thread is configured with.
func (b *Backend) StartThread(ctx context.Context, spec engine.ThreadSpec) (engine.ThreadInfo, error) {
	params, err := b.threadParams(spec.Role, spec.Cwd, spec.Sandbox, spec.Pair, spec.AutoCompactTokenLimit)
	if err != nil {
		return engine.ThreadInfo{}, err
	}
	raw, err := b.sendCreation(ctx, spec.CallID, spec.Send, "thread/start", params)
	if err != nil {
		return engine.ThreadInfo{}, requestError(err)
	}
	started, err := decodeThread(raw, "thread/start")
	if err != nil {
		return engine.ThreadInfo{}, err
	}
	b.threads.Add(started.ID)
	b.mu.Lock()
	b.tokenTotal[started.ID] = engine.Tokens{}
	b.mu.Unlock()
	return b.readback(ctx, started.ID)
}

// readback reads the pair a thread is configured with from thread/read: Thread.model and Thread.reasoningEffort, a null
// being the empty string so that it never equals the role's pair. includeTurns is never sent.
func (b *Backend) readback(ctx context.Context, id string) (engine.ThreadInfo, error) {
	raw, err := b.client.Call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false})
	if err != nil {
		return engine.ThreadInfo{}, requestError(err)
	}
	read, err := decodeThread(raw, "thread/read")
	if err != nil {
		return engine.ThreadInfo{}, err
	}
	if read.Ephemeral {
		return engine.ThreadInfo{}, &engine.RPCError{Message: fmt.Sprintf("thread %s is ephemeral: an engine thread is persistent", id), CodexErrorInfo: "badRequest"}
	}
	info := engine.ThreadInfo{ID: id, Cwd: read.Cwd}
	if read.Model != nil {
		info.Pair.Model = *read.Model
	}
	if read.ReasoningEffort != nil {
		info.Pair.Effort = *read.ReasoningEffort
	}
	return info, nil
}

var errNoTurn = errors.New("turn/start answered without a turn id")
