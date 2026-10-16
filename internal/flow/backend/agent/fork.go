package agent

import (
	"context"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// ForkThread implements engine.Forker: thread/fork at the last turn the continuation resumes, into the directory the call
// runs in, with the pair of the role. The request is the 0.154 shape - threadId, lastTurnId (the turns after it are omitted
// from the fork), cwd, model and config.model_reasoning_effort of the role, ephemeral false and excludeTurns true: the
// answer carries no history, because hydrating a paginated thread with the fork is deprecated, and the history is read
// afterwards with thread/turns/list. thread/rollback is never used: it changes the history only, leaves the files, and edits
// the original thread, which an audit has to find as it was.
//
// The fork is accepted only when it is what the call asked for: its configured pair is the role's, its Thread.cwd is the
// directory it was forked into, and its history - read to the end of every page, in full - ends at the through turn, by the
// turn's id or by its operation marker. A fork that is anything else is archived and refused.
func (b *Backend) ForkThread(ctx context.Context, spec engine.ForkSpec) (engine.ThreadInfo, error) {
	params, err := b.threadParams(spec.Role, spec.QA, spec.Cwd, spec.Sandbox, spec.Pair, spec.AutoCompactTokenLimit, spec.Tools)
	if err != nil {
		return engine.ThreadInfo{}, err
	}
	params["threadId"] = spec.SourceThreadID
	params["excludeTurns"] = true
	if spec.ThroughTurnID != "" {
		params["lastTurnId"] = spec.ThroughTurnID
	}
	raw, err := b.sendCreation(ctx, spec.CallID, spec.Send, "thread/fork", params)
	if err != nil {
		return engine.ThreadInfo{}, requestError(err)
	}
	forked, err := decodeThread(raw, "thread/fork")
	if err != nil {
		return engine.ThreadInfo{}, err
	}
	b.threads.Add(forked.ID)
	info, err := b.readback(ctx, forked.ID)
	if err != nil {
		return engine.ThreadInfo{}, err
	}
	if why, err := b.forkBoundary(ctx, forked.ID, info, spec); err != nil || why != "" {
		if err != nil {
			return engine.ThreadInfo{}, requestError(err)
		}
		_ = b.ArchiveThread(ctx, forked.ID)
		return engine.ThreadInfo{}, &engine.RPCError{Message: fmt.Sprintf("the fork %s is not the boundary the call asked for: %s", forked.ID, why), CodexErrorInfo: "badRequest"}
	}
	return info, nil
}

// forkBoundary says why a fork is not at the boundary of its spec ("" when it is). The history is read to the end of its
// pages: the last turn has to be the through turn, found by its id or its marker, and it has to have ended.
func (b *Backend) forkBoundary(ctx context.Context, id string, info engine.ThreadInfo, spec engine.ForkSpec) (string, error) {
	if info.Cwd != "" && info.Cwd != spec.Cwd {
		return fmt.Sprintf("its cwd is %s, not %s", info.Cwd, spec.Cwd), nil
	}
	if spec.ThroughTurnID == "" && spec.ThroughOp == "" {
		return "", nil
	}
	turns, err := b.Turns(ctx, id)
	if err != nil {
		return "", err
	}
	if len(turns) == 0 {
		return "its history is empty", nil
	}
	last := turns[len(turns)-1]
	switch {
	case last.ID != spec.ThroughTurnID && !(spec.ThroughOp != "" && last.Readable && last.Marker == spec.ThroughOp):
		return fmt.Sprintf("its last turn is %s (%s), not %s (%s)", last.ID, last.Marker, spec.ThroughTurnID, spec.ThroughOp), nil
	case last.Status == "inProgress":
		return fmt.Sprintf("its last turn %s has not ended", last.ID), nil
	}
	return "", nil
}
