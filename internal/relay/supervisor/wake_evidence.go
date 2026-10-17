package supervisor

import (
	"context"
	"encoding/json"
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
	if want, err := delivery.ObserveWakeConfiguration(ctx, a.c.Store, a.c.Socket, a.r.Recipient, a.row.ObligationKind, os.Getpid(), a.settings); err == nil {
		a.wake.Identity = want.Identity
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

func (a *attemptRun) finishWake(ctx context.Context, facts delivery.Facts, receipt delivery.Obj, at string) {
	if a.wake == nil {
		return
	}
	w := a.wake
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
	// Invalidate identity if the endpoint, worker executable or policy moved while
	// this send ran. The receipt is still retained as unmeasured evidence.
	want, err := delivery.ObserveWakeConfiguration(ctx, a.c.Store, a.c.Socket, a.r.Recipient, a.row.ObligationKind, os.Getpid(), a.settings)
	if err != nil || want.Identity != w.Identity {
		return
	}
	w.NewTurn = true
}
