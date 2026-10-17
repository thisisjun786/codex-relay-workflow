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

func TestFlowSteerPreDispatchReadFailureRetriesSameTurn(t *testing.T) {
	for _, name := range []string{"same channel", "fresh channel"} {
		t.Run(name, func(t *testing.T) {
			f := flowRecordFixture(t)
			id := stageFlowWake(t, f)
			h, a := flowSteerHost(t)
			f.c.beforeTransport = func() {
				h.Script("thread/read", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "read connection lost"}})
				f.c.beforeTransport = nil
			}
			result, err := f.c.Attempt(f.ctx, id, a, flowSteerNow)
			if err != nil || result["deliveryState"] != delivery.DeferredBusy || result["retrySafe"] != true || result["sendAttempted"] != "no" || result["deliveryMethod"] != "turn/steer" || result["expectedTurnId"] != flowSteerTurn {
				t.Fatalf("a pre-dispatch read failure did not defer busy: %v %v", result, err)
			}
			if h.Count("turn/steer") != 0 || h.Count("turn/start") != 0 || h.Count("thread/resume") != 0 {
				t.Fatalf("a pre-dispatch failure sent requests: %v", h.Requests())
			}
			attempts, err := f.s.SupervisorAttempts(f.ctx, id)
			if err != nil || len(attempts) != 1 {
				t.Fatalf("attempt rows: %+v %v", attempts, err)
			}
			originalAttempt := attempts[0]
			receipt, err := a.b.Ledger.Get(f.ctx, attempts[0].RequestID)
			if err != nil || receipt["status"] != "not_attempted" || receipt["retrySafe"] != true {
				t.Fatalf("the ledger did not record a real not_attempted: %v %v", receipt, err)
			}
			row, err := f.c.Get(f.ctx, id)
			if err != nil || !row.NextEligibleAt.Valid || row.HoldReason.Valid {
				t.Fatalf("delivery row: %+v %v", row, err)
			}
			due := flowSteerNow + delivery.DefaultPolicy().DelayFor(1, "busy")
			if row.NextEligibleAt.Float64 != due {
				t.Fatalf("next eligible %v, want %v", row.NextEligibleAt.Float64, due)
			}
			if name == "fresh channel" {
				f.c = &Channel{Store: f.s, Linkage: f.c.Linkage, Program: f.c.Program, Socket: f.c.Socket, Settings: f.c.Settings, SettingsLoader: f.c.SettingsLoader}
			}
			if r, err := f.c.Attempt(f.ctx, id, a, due-1); err != nil {
				t.Fatalf("the pre-due attempt errored: %v %v", r, err)
			}
			if attempts, err = f.s.SupervisorAttempts(f.ctx, id); err != nil || len(attempts) != 1 || h.Count("turn/steer") != 0 {
				t.Fatalf("the pre-due attempt sent: %+v %v", attempts, err)
			}
			if row, err = f.c.Get(f.ctx, id); err != nil || row.NextEligibleAt.Float64 != due {
				t.Fatalf("the pre-due attempt moved the deadline: %+v %v", row, err)
			}
			result, err = f.c.Attempt(f.ctx, id, a, due)
			if err != nil || result["deliveryState"] != delivery.Dispatched || result["turnId"] != flowSteerTurn || result["deliveryMethod"] != "turn/steer" {
				t.Fatalf("the due retry did not dispatch into the same turn: %v %v", result, err)
			}
			if h.Count("turn/steer") != 1 || h.Count("turn/start") != 0 || h.Count("thread/resume") != 0 {
				t.Fatalf("the retry sent wrong requests: %v", h.Requests())
			}
			attempts, err = f.s.SupervisorAttempts(f.ctx, id)
			if err != nil || len(attempts) != 2 || attempts[0].RequestID == attempts[1].RequestID {
				t.Fatalf("attempt rows: %+v %v", attempts, err)
			}
			if attempts[0] != originalAttempt || attempts[0].State != delivery.DeferredBusy || attempts[0].RetrySafe == 0 || steerTarget(attempts[1].Record) != flowSteerTurn {
				t.Fatalf("the retry lost the failed attempt or the turn: %+v", attempts)
			}
			if _, err := f.c.Attempt(f.ctx, id, a, due+301); err != nil || h.Count("turn/steer") != 1 {
				t.Fatalf("a delivered steer sent again: %v", err)
			}
		})
	}
}

func TestFlowSteerPreDispatchFailuresRespectBusyCap(t *testing.T) {
	f := flowRecordFixture(t)
	id := stageFlowWake(t, f)
	h, a := flowSteerHost(t)
	policy := delivery.DefaultPolicy()
	f.c.beforeTransport = func() {
		h.Script("thread/read", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "read connection lost"}})
	}
	now := flowSteerNow
	for n := int64(1); n <= policy.BusyMaxAttempts; n++ {
		result, err := f.c.Attempt(f.ctx, id, a, now)
		if err != nil || result["deliveryState"] != delivery.DeferredBusy || result["retrySafe"] != true {
			t.Fatalf("attempt %d did not defer busy: %v %v", n, result, err)
		}
		attempts, err := f.s.SupervisorAttempts(f.ctx, id)
		if err != nil || int64(len(attempts)) != n {
			t.Fatalf("attempt %d rows: %+v %v", n, attempts, err)
		}
		row, err := f.c.Get(f.ctx, id)
		if err != nil || row.AttemptCount != n {
			t.Fatalf("attempt %d row: %+v %v", n, row, err)
		}
		if !row.NextEligibleAt.Valid || row.NextEligibleAt.Float64 != now+policy.DelayFor(n, "busy") {
			t.Fatalf("attempt %d deadline: %+v", n, row)
		}
		if n < policy.BusyMaxAttempts && row.HoldReason.Valid {
			t.Fatalf("attempt %d held early: %+v", n, row)
		}
		if n == policy.BusyMaxAttempts {
			if !row.HoldReason.Valid || row.HoldReason.String != delivery.BusyCap {
				t.Fatalf("attempt %d missed the busy cap: %+v", n, row)
			}
			if row.ClaimableAt(row.NextEligibleAt.Float64 + 100000) {
				t.Fatal("the capped delivery stays claimable")
			}
		}
		now = row.NextEligibleAt.Float64
	}
	if h.Count("turn/steer") != 0 || h.Count("turn/start") != 0 || h.Count("thread/resume") != 0 {
		t.Fatalf("a capped delivery sent requests: %v", h.Requests())
	}
	f.c.beforeTransport = nil
	if _, err := f.c.Attempt(f.ctx, id, a, now+1000000); err != nil {
		t.Fatalf("the post-cap attempt errored: %v", err)
	}
	if attempts, err := f.s.SupervisorAttempts(f.ctx, id); err != nil || int64(len(attempts)) != policy.BusyMaxAttempts || h.Count("turn/steer") != 0 {
		t.Fatalf("the recovered host sent past the cap: %+v %v", attempts, err)
	}
	if row, err := f.c.Get(f.ctx, id); err != nil || !row.HoldReason.Valid || row.HoldReason.String != delivery.BusyCap {
		t.Fatalf("the cap hold did not persist: %+v %v", row, err)
	}
}

func TestFlowSteerPreDispatchClassification(t *testing.T) {
	cases := map[string]struct {
		receipt delivery.Obj
		status  string
		state   string
		sent    string
		retry   bool
		op      any
		turn    any
	}{
		"pre-dispatch read failure": {
			receipt: delivery.Obj{{Key: "status", Value: "not_attempted"}},
			status:  delivery.FailedStatus, state: delivery.DeferredBusy, sent: "no", retry: true, op: "thread/read", turn: nil,
		},
		"rpc rejection": {
			receipt: delivery.Obj{{Key: "status", Value: "failed"}, {Key: "rpcError", Value: delivery.Obj{{Key: "code", Value: -32600}}}},
			status:  delivery.FailedStatus, state: delivery.DeferredBusy, sent: "no", retry: true, op: "turn/steer", turn: nil,
		},
		"steered turn mismatch": {
			receipt: delivery.Obj{{Key: "status", Value: "failed"}, {Key: "rpcError", Value: delivery.Obj{{Key: "code", Value: "steered_turn_mismatch"}}}, {Key: "steeredTurnId", Value: "other"}},
			status:  delivery.OutcomeUnknown, state: delivery.HeldUncertain, sent: "unknown", retry: false, op: "unclassified",
		},
		"steered turn mismatch absent id": {
			receipt: delivery.Obj{{Key: "status", Value: "failed"}, {Key: "rpcError", Value: delivery.Obj{{Key: "code", Value: "steered_turn_mismatch"}}}},
			status:  delivery.OutcomeUnknown, state: delivery.HeldUncertain, sent: "unknown", retry: false, op: "unclassified",
		},
		"outcome unknown": {
			receipt: delivery.Obj{{Key: "status", Value: "outcome_unknown"}},
			status:  delivery.OutcomeUnknown, state: delivery.HeldUncertain, sent: "unknown", retry: false, op: "transport",
		},
		"accepted": {
			receipt: delivery.Obj{{Key: "status", Value: "accepted"}, {Key: "steeredTurnId", Value: flowSteerTurn}, {Key: "delivery", Value: "accepted_not_applied"}},
			status:  delivery.Accepted, state: delivery.Dispatched, sent: "yes", retry: false, op: nil, turn: flowSteerTurn,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := classifyFlowSteer(c.receipt, flowSteerTurn)
			if f.ReceiptStatus != c.status || f.DeliveryState != c.state || f.SendAttempted != c.sent || f.RetrySafe != c.retry || f.FailedOperation != c.op || f.TurnID != c.turn {
				t.Errorf("classified as %+v", f)
			}
		})
	}
	t.Run("nil receipt", func(t *testing.T) {
		f := classifyFlowSteer(nil, flowSteerTurn)
		if f.DeliveryState != delivery.HeldUncertain || f.RetrySafe != false {
			t.Errorf("a lost receipt classified as %+v", f)
		}
	})
}

func TestFlowSteerRejectedTurnRemainsSuppressedAfterRestart(t *testing.T) {
	for name, reply := range map[string]fakehost.Reply{
		"review": {ErrorObject: fakehost.ErrorWithInfo(-32600, "cannot steer review", map[string]any{"activeTurnNotSteerable": map[string]any{"turnKind": "review"}})},
		"stale":  {ErrorObject: fakehost.ErrorWithInfo(-32600, "stale turn", map[string]any{"activeTurnMismatch": map[string]any{"expectedTurnId": flowSteerTurn, "actualTurnId": "new-turn"}})},
	} {
		t.Run(name, func(t *testing.T) {
			f := flowRecordFixture(t)
			id := stageFlowWake(t, f)
			h, a := flowSteerHost(t)
			h.Respond("turn/steer", reply)
			result, err := f.c.Attempt(f.ctx, id, a, flowSteerNow)
			if err != nil || result["deliveryState"] != delivery.DeferredBusy || result["retrySafe"] != true || h.Count("turn/steer") != 1 {
				t.Fatalf("the rejection did not defer: %v %v", result, err)
			}
			row, err := f.c.Get(f.ctx, id)
			if err != nil || !row.NextEligibleAt.Valid {
				t.Fatalf("delivery row: %+v %v", row, err)
			}
			f.c = &Channel{Store: f.s, Linkage: f.c.Linkage, Program: f.c.Program, Socket: f.c.Socket, Settings: f.c.Settings, SettingsLoader: f.c.SettingsLoader}
			if _, err := f.c.Attempt(f.ctx, id, a, row.NextEligibleAt.Float64); err != nil {
				t.Fatalf("the due attempt errored: %v", err)
			}
			if h.Count("turn/steer") != 1 || h.Count("turn/start") != 0 || h.Count("thread/resume") != 0 {
				t.Fatalf("a rejected turn was steered again: %v", h.Requests())
			}
		})
	}
}
