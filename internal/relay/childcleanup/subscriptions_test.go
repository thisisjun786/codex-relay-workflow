package childcleanup

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// JSON keeps the feature-absence test runnable on the baseline before this option exists.
func descendantOptions(t *testing.T) Options {
	t.Helper()
	var opts Options
	if err := json.Unmarshal([]byte(`{"DescendantsOnly":true}`), &opts); err != nil {
		t.Fatal(err)
	}
	return opts
}
func completedHistory(s *scripted, statuses map[string]string) {
	s.srv.Handle("thread/turns/list", func(raw json.RawMessage) fakehost.Reply {
		var p struct{ ThreadID string }
		_ = json.Unmarshal(raw, &p)
		status, exists := statuses[p.ThreadID]
		if !exists {
			status = "completed"
		}
		data := []any{}
		if status != "" {
			data = append(data, map[string]any{"id": "turn-" + p.ThreadID, "status": status})
		}
		return fakehost.Reply{Result: map[string]any{"data": data}}
	})
}
func TestDescendantsOnlyFeatureKeepsRoot(t *testing.T) {
	s := newScripted(t, family()...)
	completedHistory(s, nil)
	r, err := run(t, s, descendantOptions(t))
	if err != nil || !r.Complete() || outcomes(r) != "grand:archived sub-2:archived sub-1:archived" {
		t.Fatalf("descendant-only feature absent or unsafe: err=%v report=%+v archives=%v", err, r, s.archived())
	}
	requests := s.srv.Requests()
	for i, request := range requests {
		if request.Method != "thread/archive" {
			continue
		}
		var archive, read struct{ ThreadID string }
		_ = json.Unmarshal(request.Params, &archive)
		if i == 0 || requests[i-1].Method != "thread/read" {
			t.Fatalf("archive without immediate thread/read: %+v", request)
		}
		_ = json.Unmarshal(requests[i-1].Params, &read)
		if read.ThreadID != archive.ThreadID {
			t.Fatalf("read=%s archive=%s", read.ThreadID, archive.ThreadID)
		}
	}
}
func TestDescendantReportHoldsActiveRootAlone(t *testing.T) {
	s := newScripted(t, thread{id: "child", loaded: true, rollout: true, status: "active"})
	completedHistory(s, nil)
	r, err := run(t, s, descendantOptions(t))
	if err != nil || r.Complete() || outcomes(r) != "child:held_active" || len(s.archived()) != 0 {
		t.Fatalf("F4: sole active root hold lost: err=%v report=%+v archives=%v", err, r, s.archived())
	}
}

func TestDescendantReportKeepsRootHoldFromFinalScan(t *testing.T) {
	s := newScripted(t, thread{id: "child", loaded: true, rollout: true})
	completedHistory(s, nil)
	started := func() { s.mu.Lock(); s.byID["child"].status = "active"; s.mu.Unlock() }
	r, err := Clean(context.Background(), &afterCall{Host: s.client(t), method: "thread/read", n: 2, then: started}, "child", descendantOptions(t))
	if err != nil || r.Complete() || outcomes(r) != "child:held_active" || len(s.archived()) != 0 {
		t.Fatalf("final scan root hold lost: err=%v report=%+v", err, r)
	}
}

type descendantHistoryHook struct {
	Host
	summaries int
	start     func()
}

func (h *descendantHistoryHook) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	raw, err := h.Host.Call(ctx, method, params)
	if method == "thread/turns/list" && params["threadId"] == "sub" {
		h.summaries++
		if h.summaries == 2 {
			h.start()
		}
	}
	return raw, err
}

func TestDescendantImmediateReadSeesTurnStartedAfterHistory(t *testing.T) {
	s := newScripted(t, thread{id: "child", loaded: true, rollout: true}, thread{id: "sub", parent: "child", loaded: true, rollout: true})
	completedHistory(s, nil)
	started := func() { s.mu.Lock(); s.byID["sub"].status = "active"; s.mu.Unlock() }
	h := &descendantHistoryHook{Host: s.client(t), start: started}
	r, err := Clean(context.Background(), h, "child", descendantOptions(t))
	if err != nil || r.Complete() || len(s.archived()) != 0 || h.summaries != 2 {
		t.Fatalf("immediate active read missed: err=%v report=%+v summaries=%d", err, r, h.summaries)
	}
}
func TestDescendantsHoldRunningOrUnprovedTurns(t *testing.T) {
	for _, who := range []string{"child", "sub"} {
		for _, status := range []string{"inProgress", "failed", "interrupted", "", "unknown"} {
			t.Run(who+"/"+status, func(t *testing.T) {
				s := newScripted(t, thread{id: "child", loaded: true, rollout: true}, thread{id: "sub", parent: "child", loaded: true, rollout: true})
				completedHistory(s, map[string]string{who: status})
				r, err := run(t, s, descendantOptions(t))
				if err != nil || r.Complete() || len(s.archived()) != 0 {
					t.Fatalf("unproved turn archived: status=%q err=%v report=%+v", status, err, r)
				}
			})
		}
	}
}
func TestDescendantsHoldSubtreeWhenSubTurnsActiveAfterScan(t *testing.T) {
	s := newScripted(t, thread{id: "child", loaded: true, rollout: true}, thread{id: "sub", parent: "child", loaded: true, rollout: true})
	completedHistory(s, nil)
	started := func() { s.mu.Lock(); s.byID["sub"].status = "active"; s.mu.Unlock() }
	r, err := Clean(context.Background(), &afterCall{Host: s.client(t), method: "thread/read", n: 4, then: started}, "child", descendantOptions(t))
	if err != nil || r.Complete() || len(s.archived()) != 0 {
		t.Fatalf("running subtree archived after scan: err=%v report=%+v archives=%v", err, r, s.archived())
	}
}
func TestDescendantsRequireLoadedIdleRoot(t *testing.T) {
	for _, status := range []string{"notLoaded", "systemError"} {
		s := newScripted(t, thread{id: "child", rollout: true, status: status}, thread{id: "sub", parent: "child", loaded: true, rollout: true})
		completedHistory(s, nil)
		r, err := run(t, s, descendantOptions(t))
		if err != nil || r.Complete() || len(s.archived()) != 0 {
			t.Fatalf("root %s: err=%v report=%+v", status, err, r)
		}
	}
}

type readFailure struct {
	Host
	err error
}

func (h readFailure) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method == "thread/read" {
		return nil, h.err
	}
	return h.Host.Call(ctx, method, params)
}
func TestDescendantDiscoveryPreservesPhaseFailures(t *testing.T) {
	s := newScripted(t, family()...)
	completedHistory(s, nil)
	failure := &appserver.PhaseTimeout{Method: "thread/read", Phase: "ack", Bound: time.Second}
	r, err := Clean(context.Background(), readFailure{Host: s.client(t), err: failure}, "child", descendantOptions(t))
	if !errors.Is(err, failure) || r.Stopped == "" || len(s.archived()) != 0 {
		t.Fatalf("phase failure became indefinite hold: err=%v report=%+v", err, r)
	}
}
func TestDescendantArchiveFailureStopsBeforeAncestor(t *testing.T) {
	s := newScripted(t, thread{id: "child", loaded: true, rollout: true}, thread{id: "sub", parent: "child", loaded: true, rollout: true}, thread{id: "grand", parent: "sub", loaded: true, rollout: true, refuse: "archive refusal"})
	completedHistory(s, nil)
	r, err := run(t, s, descendantOptions(t))
	if err == nil || !strings.Contains(err.Error(), "archive refusal") || outcomes(r) != "grand:failed" || len(s.archived()) != 1 {
		t.Fatalf("archive failure mapping: err=%v report=%+v archives=%v", err, r, s.archived())
	}
}
