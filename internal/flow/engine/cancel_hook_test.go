package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// ctxHook records the context each RunFinished call is given: whether it was already done and the deadline it carries.
type ctxHook struct {
	mu       sync.Mutex
	finished []string
	err      error
	deadline time.Time
	hasDL    bool
}

func (h *ctxHook) RunStarted(context.Context, string) error     { return nil }
func (h *ctxHook) BatchCommitted(context.Context, string) error { return nil }
func (h *ctxHook) RunFinished(ctx context.Context, _, status string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.finished = append(h.finished, status)
	h.err = ctx.Err()
	h.deadline, h.hasDL = ctx.Deadline()
	return ctx.Err()
}

// TestFinishHookOfACancelledRunGetsALiveContext (CRW-1573, D3.5): the run's context is cancelled while its call is in flight, so the run
// ends cancelled; the finish hook is still told on a context the cancel did not cancel, bounded by a deadline at most 30 s away (finishHookGrace),
// and the journal holds no upward_hook_failed row (the `context canceled` failure of the incident). A completed run's hook keeps the
// run's own context (no deadline the engine added).
func TestFinishHookOfACancelledRunGetsALiveContext(t *testing.T) {
	prog, err := script.Load(script.Source{Name: "t.js", Text: []byte("export const meta = { name: 't' }\nconst a = await agent.call('hi', { agentType: 'executor' })\nreturn a.status\n")})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fake := NewFake()
		fake.Handler = func(r FakeRequest) FakeReply {
			if r.Kind == "turn" {
				cancel()
				time.Sleep(200 * time.Millisecond)
			}
			return FakeReply{}
		}
		j := NewMemoryJournal()
		h := &ctxHook{}
		res, err := Run(ctx, prog, Config{RunID: "run-hook", Backend: fake, Policy: FakePolicy(), Journal: j, Upward: h})
		if err != nil {
			t.Fatal(err)
		}
		if res.ErrorKind != ErrKindCancelled {
			t.Fatalf("the run did not end cancelled: %+v", res)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if len(h.finished) != 1 {
			t.Fatalf("the finish hook was told %d times: %v", len(h.finished), h.finished)
		}
		if h.err != nil {
			t.Errorf("the finish hook of a cancelled run got a done context: %v", h.err)
		}
		if !h.hasDL || h.deadline.After(time.Now().Add(30*time.Second)) {
			t.Errorf("the finish hook's context: deadline %v (set %v), want one at most 30 s away", h.deadline, h.hasDL)
		}
		for _, ev := range j.Events() {
			if ev.Kind == KindFlow && ev.Event == EvUpwardHookFailed {
				t.Errorf("an upward_hook_failed row: %v", ev.Data)
			}
		}
	})
	t.Run("completed", func(t *testing.T) {
		h := &ctxHook{}
		res, err := Run(context.Background(), prog, Config{RunID: "run-hook", Backend: NewFake(), Policy: FakePolicy(), Journal: NewMemoryJournal(), Upward: h})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != RunCompleted {
			t.Fatalf("run %+v", res)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if len(h.finished) != 1 || h.err != nil || h.hasDL {
			t.Errorf("the finish hook of a completed run: told %v, err %v, deadline %v %v", h.finished, h.err, h.hasDL, h.deadline)
		}
	})
}
