package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// CRW-1684 (review): a bind whose rows are not stored is not a bind. Here the commit of a session bind's rows fails, as a failed SQLite
// commit does: the rows of that commit stay out of the store and every later commit is refused. The run sends no bound reply for the
// bind, and the store holds no thread_started of it.

// bindStore is a journal whose commit fails while a session bind's thread_started waits in the batch. A commit that fails stores
// nothing, and every later commit is refused, as the SQLite journal refuses them (FailingJournal).
type bindStore struct {
	*MemoryJournal
	mu        sync.Mutex
	pending   bool
	committed int
	err       error
}

func (s *bindStore) Append(e Event) Event {
	out := s.MemoryJournal.Append(e)
	if out.Kind == KindCall && out.Event == EvThreadStarted && rowStr(out.Data, "via") == "session" {
		s.mu.Lock()
		s.pending = true
		s.mu.Unlock()
	}
	return out
}

// Flush commits the batch. The commit of a batch that holds a bind fails.
func (s *bindStore) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil && s.pending {
		s.err = errors.New("the commit of the bind's rows failed")
	}
	if s.err == nil {
		s.committed, s.pending = len(s.MemoryJournal.Events()), false
	}
	return s.err
}

// Err is the first failed commit, nil while every commit succeeded.
func (s *bindStore) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// stored is the store as a second process reads it: the rows of the commits that succeeded.
func (s *bindStore) stored() Journal {
	s.mu.Lock()
	n := s.committed
	s.mu.Unlock()
	out := NewMemoryJournal()
	for _, e := range s.MemoryJournal.Events()[:n] {
		out.Append(e)
	}
	return out
}

// TestSessionBindIsNotRepliedWhenItsRowsAreNotStored runs the bind case of TestSessionBindRowsAreStoredBeforeTheBoundReply with a
// commit that fails at the bind. The run sends no bound reply, and the store a second process reads holds no thread_started of the bind.
func TestSessionBindIsNotRepliedWhenItsRowsAreNotStored(t *testing.T) {
	k := newSessionKit(t)
	store := &bindStore{MemoryJournal: NewMemoryJournal()}
	k.h.cfg.Journal = store
	k.jr = store
	k.cfg.Host = noFindHost{k.host}
	bus := &bindReplies{FileStepBus: k.bus, reopen: store.stored}
	k.cfg.Bus = bus
	k.orch.mu.Lock()
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer { return orchAnswer{edit: "P:" + st.Op} }
	k.orch.mu.Unlock()
	body := "const r = await agent.call('build node n1', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT }); return [r.status, r.meta.threadId, r.meta.tokens.total]"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := Run(ctx, k.h.load(body), k.h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "script_error" {
		t.Errorf("the run %s; want it to end with the journal's error", res.Status)
	}

	bus.mu.Lock()
	replies, taken := bus.replies, bus.taken
	bus.mu.Unlock()
	if taken {
		t.Errorf("a bound reply was sent for a bind whose rows are not stored: replies %+v", replies)
	}
	for _, e := range store.stored().Events() {
		if e.Kind == KindCall && e.Label == "exe" && e.Event == EvThreadStarted {
			t.Errorf("the store holds thread_started %+v although its commit failed", e)
		}
	}
}

// CRW-1684 (late report): a report that reaches judgeReport is answered bound only for a bind whose rows are stored. The run reads the
// call's rows to judge a report no call waits for, and those rows can be in memory only: the commit of a bind failed, a bind is in
// flight, or sessionAdopt has appended the turn_started and not flushed it yet. The cases replay the rows of one run's spawn and
// send_input repair against a store that models the commit.

// replayStore is a journal whose rows are stored by a Flush that succeeds. Once a commit has failed it keeps the first error and stores
// nothing more, as bindStore and the SQLite journal do. before runs once, at the next Events call, before the rows are read: a bind
// that lands while a report is being judged.
type replayStore struct {
	*MemoryJournal
	mu     sync.Mutex
	stable int
	fail   bool
	err    error
	before func()
}

// Events runs the one-shot before hook and then copies the rows. Flush and stored read the rows of the embedded journal and never
// run the hook.
func (s *replayStore) Events() []Event {
	s.mu.Lock()
	hook := s.before
	s.before = nil
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return s.MemoryJournal.Events()
}

// Flush stores every row appended so far, unless fail is set: then the commit fails and stores nothing, now and later.
func (s *replayStore) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil && s.fail {
		s.err = errors.New("the commit of the bind's rows failed")
	}
	if s.err == nil {
		s.stable = len(s.MemoryJournal.Events())
	}
	return s.err
}

// Err is the first failed commit, nil while every commit succeeded.
func (s *replayStore) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// stored is the store as a second process reads it: the rows of the commits that succeeded.
func (s *replayStore) stored() Journal {
	s.mu.Lock()
	n := s.stable
	s.mu.Unlock()
	out := NewMemoryJournal()
	for _, e := range s.MemoryJournal.Events()[:n] {
		out.Append(e)
	}
	return out
}

// TestSessionLateReportIsNotAnsweredBoundWhenTheBindIsNotStored routes the report of a step that no call waits for, over the rows of a
// spawn and a send_input repair that were stored in part. A bound answer comes only when the step's turn_started is in the rows and
// the rows are stored by then; a step without its turn_started waits for its bind, and a commit that fails ends the run and asks for
// the report again.
func TestSessionLateReportIsNotAnsweredBoundWhenTheBindIsNotStored(t *testing.T) {
	// the rows are those a completed run wrote: prepared, step_published (k=1), thread_started, turn_started (k=1), step_published
	// (k=2, send_input), turn_started (k=2), completed
	k := newSessionKit(t)
	k.orch.mu.Lock()
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
		if st.K == 1 {
			return orchAnswer{text: "this is not JSON"}
		}
		return orchAnswer{}
	}
	k.orch.mu.Unlock()
	res := k.h.run("const r = await agent.call('review', { agentType: 'reviewer', label: 'rev', head: 'h1', schema: VERDICT })\nreturn [r.status, r.meta.repairUsed]")
	if got := value[[]any](t, res); got[0] != "completed" || got[1] != true {
		t.Fatalf("result %v", got)
	}
	rows := k.rowsOf("rev")
	if got := eventsOf(rows); got != repaired {
		t.Fatalf("rows %s", got)
	}
	call, agent := rows[0].CallID, rowStr(rows[2].Data, "thread_id")

	const (
		none  = "no reply"
		bound = ReplyBound
		retry = ReplyRetry
	)
	cases := []struct {
		name string
		// rows[:stored] are stored, the next pending rows are appended and not flushed, and the next inFlight rows are appended by
		// the read of the report's rows, after the router found no waiter (and the waiter of step k is registered then)
		stored, pending, inFlight int
		k                         int
		// failedBefore: a commit failed before the report arrived. failAtReply: the commit the report's judgement makes fails.
		failedBefore, failAtReply bool
		want                      string
	}{
		{name: "stored", stored: 4, k: 1, want: bound},
		{name: "send_input stored", stored: 6, k: 2, want: bound},
		{name: "send_input turn not written", stored: 5, k: 2, want: none},
		{name: "adopt turn not written", stored: 3, k: 1, want: none},
		{name: "adopt turn not stored", stored: 3, pending: 1, k: 1, want: bound},
		{name: "adopt commit fails", stored: 3, pending: 1, k: 1, failAtReply: true, want: retry},
		{name: "bind in flight", stored: 2, inFlight: 2, k: 1, want: bound},
		{name: "send_input bind in flight", stored: 5, inFlight: 1, k: 2, want: bound},
		{name: "not stored", stored: 2, pending: 2, k: 1, failedBefore: true, want: retry},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &replayStore{MemoryJournal: NewMemoryJournal()}
			for _, row := range rows[:c.stored] {
				store.MemoryJournal.Append(row)
			}
			if err := store.Flush(); err != nil {
				t.Fatal(err)
			}
			for _, row := range rows[c.stored : c.stored+c.pending] {
				store.MemoryJournal.Append(row)
			}
			store.fail = c.failAtReply || c.failedBefore
			if c.failedBefore && store.Flush() == nil {
				t.Fatal("the commit of the pending rows did not fail")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			bus := &bindReplies{FileStepBus: NewFileStepBus(t.TempDir()), reopen: store.stored}
			e := &Engine{cfg: Config{Session: &SessionConfig{Bus: bus}}, j: store, runID: rows[0].RunID, sessR: newSessionRouter(), cancel: cancel, loop: script.NewLoop()}
			var w *sessionWaiter
			if c.inFlight > 0 {
				first := c.stored + c.pending
				store.before = func() {
					w = e.sessR.register(call, c.k, 1)
					for _, row := range rows[first : first+c.inFlight] {
						store.MemoryJournal.Append(row)
					}
				}
			}

			e.route(DispatchReport{Schema: DispatchReportSchema, ID: "late-" + c.name, Run: rows[0].RunID, Call: call, K: c.k, Attempt: 1, Agent: agent})

			bus.mu.Lock()
			replies, taken, snapshot := bus.replies, bus.taken, bus.snapshot
			bus.mu.Unlock()
			if c.inFlight > 0 {
				if w == nil {
					t.Fatal("the read of the report's rows did not run the bind")
				}
				select {
				case got := <-w.ch:
					t.Errorf("the waiter registered by the bind received %+v", got)
				default:
				}
				e.sessR.unregister(call, w)
			}
			switch c.want {
			case none:
				if len(replies) != 0 {
					t.Errorf("replies %+v; want none, the report waits for the step's bind", replies)
				}
				if ctx.Err() != nil {
					t.Errorf("the run ended: %v", ctx.Err())
				}
			case bound:
				if len(replies) != 1 || replies[0].Status != ReplyBound || replies[0].Detail != "the run bound this agent already" {
					t.Fatalf("replies %+v; want one bound reply", replies)
				}
				if !taken {
					t.Fatal("no bound reply was sent")
				}
				turned, started := false, false
				for _, e := range snapshot {
					if e.Kind != KindCall || e.CallID != call || rowStr(e.Data, "via") != "session" {
						continue
					}
					switch {
					case e.Event == EvTurnStarted && rowInt(e.Data, "k") == c.k && rowInt(e.Data, "attempt") == 1:
						turned = true
					case e.Event == EvThreadStarted && rowStr(e.Data, "thread_id") == agent:
						started = true
					}
				}
				if !turned || (c.k == 1 && !started) {
					t.Errorf("the rows stored at the bound reply: turn_started of k=%d %v, thread_started %v; the bound answer needs them stored", c.k, turned, started)
				}
				if ctx.Err() != nil {
					t.Errorf("the run ended: %v", ctx.Err())
				}
			case retry:
				if len(replies) != 1 || replies[0].Status != ReplyRetry || !strings.Contains(replies[0].Detail, "is not stored") {
					t.Fatalf("replies %+v; want one retry reply that says the bind is not stored", replies)
				}
				if taken {
					t.Error("a bound reply was sent for a bind whose rows are not stored")
				}
				if n := len(store.stored().Events()); n != c.stored {
					t.Errorf("the store holds %d rows; want the %d stored before the report", n, c.stored)
				}
				if ctx.Err() == nil {
					t.Error("the run did not end with the journal's error")
				}
			}
		})
	}
}
