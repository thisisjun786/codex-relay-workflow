package engine

import (
	"context"
	"fmt"
	"strings"
)

// forkOrCheckpoint is the continuation of a thread of the previous process, inside a suffix: it runs on a fork of the
// handle's thread at its through turn. When the thread or the turn cannot be read there, the continuation starts a new thread
// of the same role in the same directory instead, with a checkpoint packet built from the journal's record of the turns up to
// that turn. The packet is part of the call's context (its digest is in the checkpoint_started row); the files are the same
// either way, because both threads run in the restored worktree.
func (c *callRun) forkOrCheckpoint(ctx context.Context) (CallResult, bool) {
	if c.sourceReadable(ctx) {
		return c.forkThread(ctx)
	}
	p := c.plan
	packet := checkpointPacket(c.e.j, p.resume.ThreadID, p.resume.ThroughTurnID)
	c.preface = packet + "\n\n"
	c.e.j.Append(Event{RunID: c.e.runID, Kind: KindFlow, CallID: c.id, Path: c.path.id, Position: c.position, Op: "agent", Label: c.label, Event: EvCheckpointStarted,
		Data: map[string]any{"call_id": c.id, "thread_id": p.resume.ThreadID, "through_turn_id": p.resume.ThroughTurnID, "checkpoint_digest": digestJSON([]byte(packet)),
			"packet": packet, "cwd": c.placement.Path}})
	return c.startThread(ctx)
}

// checkpointPreface is the checkpoint packet the call's newest attempt started its thread with, as its checkpoint_started row
// recorded it, followed by the blank line that separates it from the first turn's input; "" for a call whose thread is not a
// checkpoint thread. A creation the next process recovers (the thread its lost answer left, the request it proves absent and
// sends again) gets the first turn of the thread from here, because the packet is held in memory by the process that built it.
func (c *callRun) checkpointPreface() string {
	preface := ""
	for _, ev := range c.e.j.Events() {
		if ev.CallID != c.id {
			continue
		}
		switch {
		case ev.Kind == KindCall && ev.Event == EvPrepared:
			preface = ""
		case ev.Kind == KindFlow && ev.Event == EvCheckpointStarted:
			if packet := rowStr(ev.Data, "packet"); packet != "" {
				preface = packet + "\n\n"
			}
		}
	}
	return preface
}

// sourceReadable is whether the handle's thread can be read to its through turn on the host, and the turn has ended. A
// backend that cannot be asked is taken to read it.
func (c *callRun) sourceReadable(ctx context.Context) bool {
	host, ok := c.e.cfg.Backend.(ReconHost)
	if !ok {
		return true
	}
	turns, err := host.Turns(ctx, c.plan.resume.ThreadID)
	if err != nil {
		return false
	}
	for _, t := range turns {
		if t.ID == c.plan.resume.ThroughTurnID {
			return t.Status != "inProgress"
		}
	}
	return false
}

// checkpointPacket is the conversation of a thread through a turn as the journal recorded it (conversationPrefix: for a session
// subagent, the conversation it continues too): each turn's input, and the result of the call the turn ended.
func checkpointPacket(j Journal, threadID, throughTurn string) string {
	prefix := conversationPrefix(j, threadID, throughTurn)
	inputs := map[string]string{}
	results := map[string]string{}
	for _, ev := range j.Events() {
		if ev.Kind != KindCall || ev.Op != "agent" {
			continue
		}
		switch ev.Event {
		case EvPrepared:
			for k := range results {
				if strings.HasPrefix(k, ev.CallID+"/") {
					delete(results, k)
				}
			}
		case EvTurnRequested, EvStepPublished:
			inputs[rowStr(ev.Data, "op")] = rowStr(ev.Data, "input")
		case EvCompleted:
			results[ev.CallID] = rowStr(ev.Data, "result")
		}
	}
	var b strings.Builder
	b.WriteString("This thread continues earlier work whose conversation can no longer be read. The record of that conversation follows, oldest turn first; the files in your working directory are the state it left.\n")
	for i, t := range prefix {
		op := rowStr(t, "op")
		fmt.Fprintf(&b, "\n[turn %d] %s\n", i+1, strings.TrimSpace(inputs[op]))
		callID, _, _ := strings.Cut(op, "/t")
		if i == len(prefix)-1 || !strings.HasPrefix(rowStr(prefix[i+1], "op"), callID+"/") {
			if r, ok := results[callID]; ok {
				fmt.Fprintf(&b, "[answer of the call] %s\n", r)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
