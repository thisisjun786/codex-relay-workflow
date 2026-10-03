package delivery

import (
	"context"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// probeKey marks a context as the one the test handed to the delivery code.
type probeKey struct{}

// ctxProbe is a host that notes, for each of the twelve Adapter methods, whether the context it was
// called with is the one the caller of the delivery code passed (a derived context counts, a fresh
// context.Background does not). Everything else is the fake host underneath.
type ctxProbe struct {
	Adapter
	called map[string]bool
	missed []string
}

func (p *ctxProbe) note(ctx context.Context, method string) {
	if p.called == nil {
		p.called = map[string]bool{}
	}
	p.called[method] = true
	if ctx.Value(probeKey{}) == nil {
		p.missed = append(p.missed, method)
	}
}

func (p *ctxProbe) ReadThread(ctx context.Context, thread string) (ThreadFacts, error) {
	p.note(ctx, "ReadThread")
	return p.Adapter.ReadThread(ctx, thread)
}
func (p *ctxProbe) IsArchived(ctx context.Context, thread string, cwd any) (*bool, error) {
	p.note(ctx, "IsArchived")
	return p.Adapter.IsArchived(ctx, thread, cwd)
}
func (p *ctxProbe) ReadGoalStatus(ctx context.Context, thread string) (any, error) {
	p.note(ctx, "ReadGoalStatus")
	return p.Adapter.ReadGoalStatus(ctx, thread)
}
func (p *ctxProbe) ListTurnIDs(ctx context.Context, thread string, limit int) ([]any, error) {
	p.note(ctx, "ListTurnIDs")
	return p.Adapter.ListTurnIDs(ctx, thread, limit)
}
func (p *ctxProbe) ReadTurn(ctx context.Context, thread, turn string) (*TurnInfo, error) {
	p.note(ctx, "ReadTurn")
	return p.Adapter.ReadTurn(ctx, thread, turn)
}
func (p *ctxProbe) SendMessage(ctx context.Context, requestID, thread, message string, settings *TaskSettings) (Obj, error) {
	p.note(ctx, "SendMessage")
	return p.Adapter.SendMessage(ctx, requestID, thread, message, settings)
}
func (p *ctxProbe) GetOperation(ctx context.Context, requestID string) (Obj, error) {
	p.note(ctx, "GetOperation")
	return p.Adapter.GetOperation(ctx, requestID)
}
func (p *ctxProbe) FindToken(ctx context.Context, thread, token string, limit int, messageOnly bool) (TokenScan, error) {
	p.note(ctx, "FindToken")
	return p.Adapter.FindToken(ctx, thread, token, limit, messageOnly)
}
func (p *ctxProbe) FindDispatchedTurn(ctx context.Context, thread, turnID string, sentAt float64) (TurnPresence, error) {
	p.note(ctx, "FindDispatchedTurn")
	return p.Adapter.FindDispatchedTurn(ctx, thread, turnID, sentAt)
}
func (p *ctxProbe) FindTokenSince(ctx context.Context, thread, token string, older []string, limit int) (TokenScan, error) {
	p.note(ctx, "FindTokenSince")
	return p.Adapter.FindTokenSince(ctx, thread, token, older, limit)
}
func (p *ctxProbe) FindTokenInTurn(ctx context.Context, thread, token, turnID string, limit int) (TokenScan, error) {
	p.note(ctx, "FindTokenInTurn")
	return p.Adapter.FindTokenInTurn(ctx, thread, token, turnID, limit)
}
func (p *ctxProbe) RecipientFingerprint(ctx context.Context, thread string) (string, error) {
	p.note(ctx, "RecipientFingerprint")
	return p.Adapter.RecipientFingerprint(ctx, thread)
}

// The delivery code hands the context of the work that asks to every host call it makes: an
// attempt (lifecycle read, turn listing, send), a reconcile of an uncertain send, an
// acknowledgement and a daemon-shaped pass over them.
func TestDeliveryPassesItsContextToEveryHostCall(t *testing.T) {
	t.Parallel()
	probe := &ctxProbe{}
	scenario := func(name string, run func(h *hl)) {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "")
			f.rid = ""
			f.ctx = context.WithValue(f.ctx, probeKey{}, true)
			h := &hl{fixture: f, name: t.Name(), ack: NewAck(f.delivery), rc: NewReconciler(f.delivery), adapter: f.host, policy: defaultTick()}
			h.checks = &TurnChecks{Reconciler: h.rc, Budget: 4}
			probe.Adapter = f.host
			run(h)
		})
	}
	// An uncertain send whose token later lands in a turn the host reports.
	scenario("token lands", func(h *hl) {
		h.parentHistory()
		event := h.queuedEvent(regOpts{})
		h.host.script = []string{"in_progress"}
		request := pyjson.Text(h.attemptOn(event, probe, nil).Get("requestId"))
		h.host.startTurn(parent, "", "completed", "..."+request+"...")
		h.reconcile(request, probe)
		h.tickWith(h.policy, probe, h.checks)
	})
	// An uncertain send that stays uncertain: the pass gates on the recipient's items and reads the
	// host for the turn the send may have started.
	scenario("stays uncertain", func(h *hl) {
		h.parentHistory()
		event := h.queuedEvent(regOpts{})
		h.host.script = []string{"in_progress"}
		request := pyjson.Text(h.attemptOn(event, probe, nil).Get("requestId"))
		h.clock.Advance(120)
		h.host.startTurn(parent, "", "completed", "another prompt")
		h.tickWith(h.policy, probe, h.checks)
		h.reconcile(request, probe)
	})
	// A delivered completion the parent acknowledges, verified and checked by a pass.
	scenario("acknowledged", func(h *hl) {
		h.parentHistory()
		event := h.queuedEvent(regOpts{})
		h.attemptOn(event, probe, nil)
		h.host.startTurn(parent, "ack-turn", "inProgress", "")
		if _, err := h.ack.Acknowledge(h.ctx, event, "ack-turn", AckProof(event, "ack-turn"), true, nil, probe); err != nil {
			t.Fatal(err)
		}
		h.clock.Advance(30)
		h.tickWith(h.policy, probe, h.checks)
	})

	// A delivered completion whose turn the host no longer lists, looked for by its token instead.
	scenario("host loses the turn", func(h *hl) {
		_, request, turn := h.dispatched()
		h.hostLoses(turn, false)
		if _, err := h.rc.CheckDispatchedTurn(h.ctx, request, probe); err != nil {
			t.Fatal(err)
		}
	})

	if len(probe.missed) > 0 {
		t.Errorf("host calls made with a context that is not the caller's: %v", probe.missed)
	}
	var reached []string
	for method := range probe.called {
		reached = append(reached, method)
	}
	slices.Sort(reached)
	want := []string{"FindDispatchedTurn", "FindToken", "FindTokenInTurn", "FindTokenSince", "GetOperation", "IsArchived", "ListTurnIDs", "ReadGoalStatus", "ReadThread", "ReadTurn", "RecipientFingerprint", "SendMessage"}
	for _, method := range want {
		if !probe.called[method] {
			t.Errorf("the scenarios never reached %s (reached %v)", method, reached)
		}
	}
}
