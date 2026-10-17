package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// Steering is optional: older/read-only hosts retain the busy deferral path.
type flowSteerer interface {
	ActiveTurn(context.Context, string) (map[string]any, error)
	SteerMessage(context.Context, string, string, string, string) (delivery.Obj, error)
}

func (a *attemptRun) selectFlowSteer(ctx context.Context, lifecycle delivery.Lifecycle) (bool, error) {
	host, ok := a.adapter.(flowSteerer)
	if !ok || !IsFlowKind(a.row.ObligationKind) || lifecycle.Archived == nil || lifecycle.Detail != "" {
		return false, nil
	}
	active, err := host.ActiveTurn(ctx, a.r.Recipient)
	if err != nil || active["steerable"] != true {
		return false, nil
	}
	turn, _ := active["activeTurnId"].(string)
	if turn == "" {
		return false, nil
	}
	// A definite rejection waits out this turn, including across relay restarts.
	// Busy deferrals create no attempt, so the last rejected steer remains visible.
	last, err := a.c.Store.LatestSupervisorAttempt(ctx, a.id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil && last.RetrySafe != 0 && steerTarget(last.Record) == turn && !steerFailedBeforeDispatch(last.Record) {
		return false, nil
	}
	a.steerTurn = turn
	return true, nil
}

func steerTarget(record string) string {
	var r struct {
		Method string `json:"deliveryMethod"`
		Turn   string `json:"expectedTurnId"`
	}
	if json.Unmarshal([]byte(record), &r) != nil || r.Method != "turn/steer" {
		return ""
	}
	return r.Turn
}

func steerFailedBeforeDispatch(record string) bool {
	var r struct {
		ReceiptStatus   string `json:"transportReceiptStatus"`
		FailedOperation string `json:"failedOperation"`
		SendAttempted   string `json:"sendAttempted"`
	}
	return json.Unmarshal([]byte(record), &r) == nil && r.ReceiptStatus == delivery.FailedStatus && r.FailedOperation == "thread/read" && r.SendAttempted == "no"
}

// A rejected steer is safe to defer. A lost response or a response naming a
// different turn is uncertain and must never be resent under a new request id.
func classifyFlowSteer(receipt delivery.Obj, expected string) delivery.Facts {
	f := delivery.Classify(receipt)
	if receipt == nil {
		return f
	}
	status := receipt.Get("status")
	turn := receipt.Get("steeredTurnId")
	rpc, _ := receipt.Get("rpcError").(delivery.Obj)
	if status == "accepted" && turn == expected && receipt.Get("delivery") == "accepted_not_applied" {
		f.ReceiptStatus, f.DeliveryState, f.SendAttempted = delivery.Accepted, delivery.Dispatched, "yes"
		f.TurnID, f.FailedOperation, f.RetrySafe = turn, nil, false
	} else if status == "not_attempted" {
		f.ReceiptStatus, f.DeliveryState, f.SendAttempted = delivery.FailedStatus, delivery.DeferredBusy, "no"
		f.TurnID, f.FailedOperation, f.RetrySafe = nil, "thread/read", true
	} else if status == "failed" && rpc != nil && rpc.Get("code") != "steered_turn_mismatch" && turn == nil {
		f.ReceiptStatus, f.DeliveryState, f.SendAttempted = delivery.FailedStatus, delivery.DeferredBusy, "no"
		f.TurnID, f.FailedOperation, f.RetrySafe = nil, "turn/steer", true
	}
	return f
}
