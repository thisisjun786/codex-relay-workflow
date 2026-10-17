package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// Freeze the inputs of this transport attempt. The lifecycle is read immediately
// before sending; a later lifecycle row cannot rewrite what this attempt observed.
func (a *attemptRun) prepareWake(ctx context.Context) {
	if !IsFlowKind(a.row.ObligationKind) {
		return
	}
	a.wake = &delivery.WakeAttempt{RequestID: a.claimed.requestID, RecipientTaskID: a.r.Recipient, DeliveryMethod: "turn/start", Unattended: !a.byID}
	if a.steerTurn != "" {
		a.wake.DeliveryMethod = "turn/steer"
	}
	a.wake.RequiredSettings = json.RawMessage(pyjson.Dumps(a.settings.Data, pyjson.Options{}))
	if want, id, err := a.observeWakeConnection(ctx); err == nil {
		a.wake.Identity = want.Identity
		a.wakeConnection = id
	}
	if life, err := a.adapter.ReadThread(ctx, a.r.Recipient); err == nil {
		a.wake.RecipientStatusBefore, _ = life.RuntimeStatus.(string)
	}
	if a.steerTurn == "" {
		var err error
		a.wakeTurns, err = a.adapter.ListTurnIDs(ctx, a.r.Recipient, 50)
		a.wakeRead = err == nil
	}
}

// observeWakeConnection reads the peer of the connection the adapter actually sends on (CRW-1664):
// evidence about a send is measured on that transport, never on a fresh dial of the named socket.
func (a *attemptRun) observeWakeConnection(ctx context.Context) (*delivery.WakeExpectation, uint64, error) {
	source, ok := a.adapter.(delivery.WakeConnectionSource)
	if !ok {
		return nil, 0, fmt.Errorf("sending adapter does not expose its connection")
	}
	connection := source.WakeConnection()
	want, err := delivery.ObserveWakeConnection(ctx, a.c.Store, a.c.Socket, a.r.Recipient, a.row.ObligationKind, os.Getpid(), a.settings, connection)
	return want, connection.ID, err
}

func (a *attemptRun) finishWake(ctx context.Context, facts delivery.Facts, receipt delivery.Obj, at string) {
	if a.wake == nil {
		return
	}
	w := a.wake
	before := w.Identity
	w.Identity = delivery.WakeIdentity{}
	defer func() {
		want, id, err := a.observeWakeConnection(ctx)
		if err == nil && id != 0 && id == a.wakeConnection && want.Identity == before {
			w.Identity = want.Identity
		} else {
			w.NewTurn = false
		}
	}()
	w.ObservedAt, w.DeliveryState, w.SendAttempted = at, facts.DeliveryState, facts.SendAttempted
	w.TurnID, _ = facts.TurnID.(string)
	// The bridge's lifecycle is closer to turn/start than our first read. A busy
	// race or a replay never proves that this attempt woke an idle recipient.
	if status, ok := receipt.Get("statusBeforeResume").(string); ok {
		w.RecipientStatusBefore = status
	}
	if a.steerTurn != "" || facts.DeliveryState != delivery.Dispatched || !a.wakeRead || receipt.Get("replayed") == true {
		return
	}
	if w.RecipientStatusBefore != "idle" && w.RecipientStatusBefore != "notLoaded" {
		return
	}
	for _, prior := range a.wakeTurns {
		if prior == w.TurnID {
			return
		}
	}
	turn, err := a.adapter.ReadTurn(ctx, a.r.Recipient, w.TurnID)
	if err != nil || turn == nil || turn.TurnID != w.TurnID {
		return
	}
	started := delivery.TurnStartedAt(turn)
	if started == nil || *started < math.Floor(a.now) {
		return
	}
	// The deferred check above restores the identity only when the same sending
	// connection still holds it; a changed or unknown connection leaves the
	// receipt recorded without measured identity (CRW-1664).
	w.NewTurn = true
}
