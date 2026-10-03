package childcleanup

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// thread is one thread of the scripted App Server. One without a rollout is refused by thread/archive, as codex-cli 0.154.0 does for an archived thread and for a sub-thread archived with its
// parent (unloadWhenRefused makes the host drop the thread while it refuses).
type thread struct {
	id, parent, status          string
	loaded, rollout, unreadable bool
	refuse                      string // thread/archive fails with this message
	unloadWhenRefused           bool
}

// scripted is a fake App Server that answers thread/loaded/list, thread/read and thread/archive from its threads.
type scripted struct {
	srv         *fakehost.Server
	mu          sync.Mutex
	byID        map[string]*thread
	pageSize    int
	archives    []string
	includeTurn int
}

func newScripted(t *testing.T, threads ...thread) *scripted {
	t.Helper()
	s := &scripted{srv: fakehost.Start(t), byID: map[string]*thread{}}
	for i := range threads {
		th := threads[i]
		if th.status == "" {
			th.status = "idle"
		}
		s.byID[th.id] = &th
	}
	s.srv.Handle("thread/loaded/list", s.loadedList)
	s.srv.Handle("thread/read", s.read)
	s.srv.Handle("thread/archive", s.archive)
	return s
}

func (s *scripted) client(t *testing.T) *appserver.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	c, err := appserver.Dial(ctx, s.srv.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func failure(code int, message string) fakehost.Reply {
	return fakehost.Reply{Error: &fakehost.RPCError{Code: code, Message: message}}
}

func (s *scripted) loadedList(raw json.RawMessage) fakehost.Reply {
	var p struct{ Cursor *string }
	_ = json.Unmarshal(raw, &p)
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id, th := range s.byID {
		if th.loaded {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	start, end, next := 0, len(ids), any(nil)
	if p.Cursor != nil {
		start, _ = strconv.Atoi(*p.Cursor)
	}
	if s.pageSize > 0 && start+s.pageSize < len(ids) {
		end = start + s.pageSize
		next = strconv.Itoa(end)
	}
	return fakehost.Reply{Result: map[string]any{"data": ids[start:end], "nextCursor": next}}
}

func (s *scripted) read(raw json.RawMessage) fakehost.Reply {
	var p struct {
		ThreadID     string
		IncludeTurns bool
	}
	_ = json.Unmarshal(raw, &p)
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.IncludeTurns {
		s.includeTurn++
	}
	th := s.byID[p.ThreadID]
	if th == nil || th.unreadable {
		return failure(-32600, "thread not found: "+p.ThreadID)
	}
	var parent any
	if th.parent != "" {
		parent = th.parent
	}
	return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": th.id, "parentThreadId": parent, "status": map[string]any{"type": th.status}}}}
}

func (s *scripted) archive(raw json.RawMessage) fakehost.Reply {
	var p struct{ ThreadID string }
	_ = json.Unmarshal(raw, &p)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.archives = append(s.archives, p.ThreadID)
	th := s.byID[p.ThreadID]
	switch {
	case th == nil || !th.rollout:
		if th != nil && th.unloadWhenRefused {
			th.loaded = false
		}
		return failure(-32600, "no rollout found for thread id "+p.ThreadID)
	case th.refuse != "":
		return failure(-32603, th.refuse)
	}
	th.loaded = false
	return fakehost.Reply{Result: map[string]any{}}
}

func (s *scripted) archived() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.archives...)
}
