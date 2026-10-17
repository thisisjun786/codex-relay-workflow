package supervisor

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

const flowSteerTurn = "owner-running-turn"
const flowSteerNow = 1_700_000_000.0

type flowSteerAdapter struct {
	sendHost
	b *bridge.Bridge
}

func (a *flowSteerAdapter) ActiveTurn(ctx context.Context, thread string) (map[string]any, error) {
	return a.b.ActiveTurn(ctx, thread)
}

func (a *flowSteerAdapter) SteerMessage(ctx context.Context, request, thread, turn, message string) (delivery.Obj, error) {
	r, err := a.b.SteerThread(ctx, request, thread, turn, message)
	if err != nil {
		return nil, err
	}
	if r["status"] == "accepted" {
		a.accept(request, turn, message, nil)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	v, err := registry.DecodeJSON(string(raw))
	if err != nil {
		return nil, err
	}
	return v.(delivery.Obj), nil
}

func flowSteerHost(t *testing.T) (*fakehost.Server, *flowSteerAdapter) {
	t.Helper()
	h := fakehost.Start(t)
	h.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "active"}, "canAcceptDirectInput": true}}})
	h.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": flowSteerTurn, "status": "inProgress", "startedAt": flowSteerNow - 100}}, "nextCursor": nil}})
	h.Respond("turn/steer", fakehost.Reply{Result: map[string]any{"turnId": flowSteerTurn}})
	client, err := appserver.Dial(context.Background(), h.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	a := &flowSteerAdapter{sendHost: sendHost{status: "active", turns: map[string]float64{flowSteerTurn: flowSteerNow - 100}}, b: bridge.New(client, l, nil)}
	return h, a
}

func stageFlowWake(t *testing.T, f *stageFixture) string {
	t.Helper()
	r, err := f.c.StageDirect(f.ctx, DirectMessage{Kind: KindSessionStepWaiting, Relationship: "run:" + flowRecordRun,
		Subject: FlowSubject(flowRecordRun, "wake-1"), Summary: "run is ready", Text: "continue the run", At: f.at}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r["messageId"].(string)
}

func TestFlowBusyOwnerSteersSameTurn(t *testing.T) {
	for _, kind := range []string{KindSessionStepWaiting, KindFlowMilestoneReport, KindFlowProjectDecisionRequest, KindFlowJunDecisionRequest, KindFlowCompletion} {
		t.Run(kind, func(t *testing.T) {
			f := flowRecordFixture(t)
			var id string
			if kind == KindSessionStepWaiting {
				id = stageFlowWake(t, f)
			} else {
				id = f.stageFlow(t, kind, flowRecordRun, "rep-1")
			}
			h, a := flowSteerHost(t)
			result, err := f.c.Attempt(f.ctx, id, a, flowSteerNow)
			if err != nil || result["deliveryState"] != delivery.Dispatched || result["turnId"] != flowSteerTurn || result["deliveryMethod"] != "turn/steer" {
				t.Fatalf("busy owner did not receive same-turn steer: result=%v err=%v steer=%d start=%d", result, err, h.Count("turn/steer"), h.Count("turn/start"))
			}
			if h.Count("turn/steer") != 1 || h.Count("turn/start") != 0 || h.Count("thread/resume") != 0 {
				t.Fatalf("steer opened/resumed a turn: %v", h.Requests())
			}
			attempts, err := f.s.SupervisorAttempts(f.ctx, id)
			if err != nil || len(attempts) != 1 || attempts[0].TurnID.String != flowSteerTurn || !strings.Contains(attempts[0].Record, `"deliveryMethod": "turn/steer"`) {
				t.Fatalf("missing steer receipt: %+v %v", attempts, err)
			}
			for _, req := range h.Requests() {
				if req.Method != "turn/steer" {
					continue
				}
				var p struct {
					ExpectedTurnID string `json:"expectedTurnId"`
					Input          []struct {
						Text string `json:"text"`
					} `json:"input"`
				}
				if err := json.Unmarshal(req.Params, &p); err != nil || p.ExpectedTurnID != flowSteerTurn || len(p.Input) != 1 || p.Input[0].Text != attempts[0].Message {
					t.Fatalf("steer changed guard/frozen bytes: %s %v", req.Params, err)
				}
			}
			rows := f.flowRows(t, flowRecordRun)
			if len(rows) != 1 || rows[0].payload["result"] != FlowDeliveryAccepted || rows[0].payload["turn_id"] != flowSteerTurn || rows[0].payload["delivery_method"] != "turn/steer" {
				t.Fatalf("missing flow steer record: %+v", rows)
			}
			read, err := f.c.ReadBack(f.ctx, id, flowSteerTurn, Proof(id, flowSteerTurn), "", a, flowSteerNow+1)
			if err != nil || read["verified"] != "host_read" {
				t.Fatalf("same old turn failed readback: %v %v", read, err)
			}
			if _, err := f.c.Attempt(f.ctx, id, a, flowSteerNow+200); err != nil || h.Count("turn/steer") != 1 || len(f.flowRows(t, flowRecordRun)) != 1 {
				t.Fatalf("message delivered twice: %v", err)
			}
		})
	}
}

func TestFlowBusyOwnerUnsteerableWaitsForIdle(t *testing.T) {
	f := flowRecordFixture(t)
	id := stageFlowWake(t, f)
	h, a := flowSteerHost(t)
	h.Respond("turn/steer", fakehost.Reply{ErrorObject: fakehost.ErrorWithInfo(-32600, "cannot steer review", map[string]any{"activeTurnNotSteerable": map[string]any{"turnKind": "review"}})})
	result, err := f.c.Attempt(f.ctx, id, a, flowSteerNow)
	if err != nil || h.Count("turn/steer") != 1 || result["deliveryState"] != delivery.DeferredBusy || result["retrySafe"] != true {
		t.Fatalf("unsteerable turn not deferred: %v %v steer=%d", result, err, h.Count("turn/steer"))
	}
	// The same rejected turn is not tried again when the backoff becomes due.
	row, _ := f.c.Get(f.ctx, id)
	if _, err := f.c.Attempt(f.ctx, id, a, row.NextEligibleAt.Float64); err != nil || h.Count("turn/steer") != 1 || h.Count("turn/start") != 0 {
		t.Fatalf("retried the rejected turn: %v", err)
	}
	a.status = "idle"
	row, _ = f.c.Get(f.ctx, id)
	result, err = f.c.Attempt(f.ctx, id, a, row.NextEligibleAt.Float64)
	if err != nil || result["deliveryState"] != delivery.Dispatched || len(a.sends) != 1 || h.Count("turn/steer") != 1 {
		t.Fatalf("idle fallback failed: %v %v", result, err)
	}
}

func TestFlowSteerKeepsAckAndLifecycleFences(t *testing.T) {
	for _, scene := range []string{"ack", "racing-ack", "archived", "non-flow"} {
		t.Run(scene, func(t *testing.T) {
			f := flowRecordFixture(t)
			var id string
			if scene == "non-flow" {
				_, r := f.staged(t)
				id = r["messageId"].(string)
			} else {
				id = f.stageFlow(t, KindFlowMilestoneReport, flowRecordRun, "rep-1")
			}
			h, a := flowSteerHost(t)
			switch scene {
			case "ack":
				ackFlowReport1612(t, f, flowRecordRun, "rep-1")
			case "racing-ack":
				f.c.beforeTransport = func() { ackFlowReport1612(t, f, flowRecordRun, "rep-1") }
			case "archived":
				a.archived = true
			}
			if _, err := f.c.Attempt(f.ctx, id, a, flowSteerNow); err != nil || h.Count("turn/steer") != 0 || len(a.sends) != 0 {
				t.Fatalf("fence sent: %v", err)
			}
			row, err := f.c.Get(f.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if (scene == "ack" || scene == "racing-ack") && row.State != "read" {
				t.Fatalf("ack not settled: %+v", row)
			}
		})
	}
}

func TestFlowSteerMissingTurnReplyIsUncertain(t *testing.T) {
	f := flowRecordFixture(t)
	id := stageFlowWake(t, f)
	h, a := flowSteerHost(t)
	h.Respond("turn/steer", fakehost.Reply{Result: map[string]any{}})
	r, err := f.c.Attempt(f.ctx, id, a, flowSteerNow)
	if err != nil || r["deliveryState"] != delivery.HeldUncertain || r["retrySafe"] != false {
		t.Fatalf("missing turn retried: %v %v", r, err)
	}
	if _, err := f.c.Attempt(f.ctx, id, a, flowSteerNow+200); err != nil || h.Count("turn/steer") != 1 {
		t.Fatalf("uncertain delivery repeated: %v", err)
	}
}

func TestFlowSteerRecoversAnUnsettledAttemptWithoutResending(t *testing.T) {
	f := flowRecordFixture(t)
	id := stageFlowWake(t, f)
	h, a := flowSteerHost(t)
	// Model a process losing its settle after the host accepted the steer. The
	// transport-start transaction must already hold the method and guarded turn.
	if _, err := f.s.DB.ExecContext(f.ctx, `CREATE TRIGGER refuse_steer_settle BEFORE UPDATE ON supervisor_attempts
		WHEN NEW.state='dispatched' BEGIN SELECT RAISE(ABORT, 'lost settle'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.Attempt(f.ctx, id, a, flowSteerNow); err == nil || h.Count("turn/steer") != 1 {
		t.Fatalf("missing failed settle: %v", err)
	}
	if _, err := f.s.DB.ExecContext(f.ctx, "DROP TRIGGER refuse_steer_settle"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.Attempt(f.ctx, id, a, flowSteerNow+301); err != nil || h.Count("turn/steer") != 1 {
		t.Fatalf("stranded attempt resent: %v", err)
	}
	rows := f.flowRows(t, flowRecordRun)
	if len(rows) != 1 || rows[0].payload["result"] != FlowDeliveryUnknown || rows[0].payload["delivery_method"] != "turn/steer" {
		t.Fatalf("recovery lost method: %+v", rows)
	}
	r, err := f.c.ReadBack(f.ctx, id, flowSteerTurn, Proof(id, flowSteerTurn), "", a, flowSteerNow+302)
	if err != nil || r["verified"] != "host_read" || r["reconciled"] == nil {
		t.Fatalf("stranded steer readback: %v %v", r, err)
	}
	rows = f.flowRows(t, flowRecordRun)
	if len(rows) != 2 || rows[1].payload["result"] != FlowDeliveryAccepted || rows[1].payload["delivery_method"] != "turn/steer" || rows[1].payload["turn_id"] != flowSteerTurn {
		t.Fatalf("reconciliation lost receipt: %+v", rows)
	}
}
