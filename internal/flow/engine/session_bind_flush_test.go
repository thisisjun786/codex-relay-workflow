package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
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
