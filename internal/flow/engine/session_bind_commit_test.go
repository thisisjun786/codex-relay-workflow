package engine

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"
)

// CRW-1684: the rows of a bind are stored before the bound reply. A subagent the run binds is counted by a `flow next` in another
// process from its stored rows (thread_started via session, and the call's turn_started), and `flow dispatched` reads the reply that
// says it is bound. The SQLite journal keeps the bind's rows in its open batch until a commit, so a bound reply sent before that
// commit leaves the next process counting nothing. The test reads the journal through a second handle at the bound reply.

// bindReplies is the bus the run replies through. It records every reply, and at the first bound one it reads the rows the journal
// has stored by then through a second handle (reopen), before the reply is delivered.
type bindReplies struct {
	*FileStepBus
	reopen func() Journal

	mu       sync.Mutex
	replies  []DispatchReply
	taken    bool
	snapshot []Event
}

func (b *bindReplies) Reply(rep DispatchReport, reply DispatchReply) error {
	b.mu.Lock()
	b.replies = append(b.replies, reply)
	if reply.Status == ReplyBound && !b.taken {
		b.taken = true
		b.snapshot = b.reopen().Events()
	}
	b.mu.Unlock()
	return b.FileStepBus.Reply(rep, reply)
}

// openSubagentsOf are the subagents that hold the owner's slots in events, sorted. It copies the events half of the rule cli/next.go
// applies (openSubagents, pinned through `flow next` by TestNextHintNamesTheOpenSubagents and session_test.go's one-call rows): the
// subagent a session thread_started opens for its call, a session_attempt_lost releases the subagent it names, and a terminal row of
// the call releases the subagents the call opened. The close steps of that rule do not apply: no close step exists before a bind's reply.
func openSubagentsOf(events []Event) []string {
	open := map[string]bool{}
	agentOf := map[string][]string{}
	for _, ev := range events {
		if ev.Kind != KindCall || ev.Op != "agent" {
			continue
		}
		switch {
		case ev.Event == EvThreadStarted && rowStr(ev.Data, "via") == "session":
			if a := rowStr(ev.Data, "thread_id"); a != "" {
				agentOf[ev.CallID] = append(agentOf[ev.CallID], a)
				open[a] = true
			}
		case ev.Event == EvSessionAttemptLost:
			delete(open, rowStr(ev.Data, "agent_id"))
		case IsTerminalEvent(ev.Event):
			for _, a := range agentOf[ev.CallID] {
				delete(open, a)
			}
		}
	}
	out := make([]string, 0, len(open))
	for a := range open {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// TestSessionBindRowsAreStoredBeforeTheBoundReply runs one session call over the SQLite journal with its owner's report as the only
// bind (noFindHost finds no subagent by marker). At the bound reply the rows of the bind are in the store: the call's thread_started
// and turn_started, one batch, and one open subagent.
func TestSessionBindRowsAreStoredBeforeTheBoundReply(t *testing.T) {
	if SQLiteDeathJournal == nil {
		t.Fatal("session_sqlite_test.go did not install the SQLite journal of the bind cases")
	}
	k := newSessionKit(t)
	first, reopen := SQLiteDeathJournal(t, "run-test")
	k.h.cfg.Journal = first
	k.jr = first
	k.cfg.Host = noFindHost{k.host}
	bus := &bindReplies{FileStepBus: k.bus, reopen: reopen}
	k.cfg.Bus = bus
	k.orch.mu.Lock()
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer { return orchAnswer{edit: "P:" + st.Op} }
	k.orch.mu.Unlock()
	body := `const r = await agent.call('build node n1', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
return [r.status, r.meta.threadId, r.meta.tokens.total]`
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := Run(ctx, k.h.load(body), k.h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := value[[]any](t, res); len(got) != 3 || got[0] != "completed" {
		t.Fatalf("the call's result %v; want completed", got)
	}

	bus.mu.Lock()
	replies, taken, snapshot := bus.replies, bus.taken, bus.snapshot
	bus.mu.Unlock()
	if len(replies) != 1 || replies[0].Status != ReplyBound || replies[0].Detail != "" {
		t.Fatalf("replies %+v; want the one bound reply sessionVerify gives", replies)
	}
	if !taken {
		t.Fatal("no bound reply was sent")
	}
	var threadStarted, turnStarted []Event
	for _, e := range snapshot {
		if e.Kind != KindCall || e.Label != "exe" {
			continue
		}
		switch e.Event {
		case EvThreadStarted:
			threadStarted = append(threadStarted, e)
		case EvTurnStarted:
			turnStarted = append(turnStarted, e)
		}
	}
	if len(threadStarted) != 1 || len(turnStarted) != 1 {
		t.Fatalf("the rows stored at the bound reply: thread_started %d, turn_started %d; want one of each", len(threadStarted), len(turnStarted))
	}
	ts, tr := threadStarted[0], turnStarted[0]
	if ts.Op != "agent" || rowStr(ts.Data, "via") != "session" || rowStr(ts.Data, "thread_id") != "agent-1" || rowInt(ts.Data, "attempt") != 1 {
		t.Errorf("thread_started %+v", ts)
	}
	if ts.Seq+1 != tr.Seq {
		t.Errorf("thread_started and turn_started are not one batch (seq %d, %d)", ts.Seq, tr.Seq)
	}
	if got := openSubagentsOf(snapshot); len(got) != 1 || got[0] != "agent-1" {
		t.Fatalf("open subagents at the bound reply %v; want [agent-1]", got)
	}
}
