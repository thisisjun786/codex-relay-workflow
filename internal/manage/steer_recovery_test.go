package manage

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

func TestDeliverSteerOnlyLedgerRejectsChangedTurnAfterNotAttempted(t *testing.T) {
	operations, err := ledger.Open(filepath.Join(t.TempDir(), "operations.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer operations.Close()
	message := Message{LogicalID: "lost-refusal", Thread: "mgmt-thread", Text: "body", SteerOnly: true}
	_, receipt, err := operations.Begin(context.Background(), message.LogicalID, deliverToolSteer, deliverSteerArgs(message, message.LogicalID, "turn-1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	receipt["status"] = "not_attempted"
	receipt["retrySafe"] = true
	if _, err := operations.Save(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
	_, _, err = operations.Begin(context.Background(), message.LogicalID, deliverToolSteer, deliverSteerArgs(message, message.LogicalID, "turn-2"), nil)
	if !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("changed expected_turn_id: error %v, want ErrConflict", err)
	}
}

func TestDeliverSteerOnlyLostRefusalRecoversOnANewTurn(t *testing.T) {
	for _, wait := range []bool{false, true} {
		name := "active recovery"
		if wait {
			name = "idle before recovery"
		}
		t.Run(name, func(t *testing.T) {
			now := pumpTestNow
			environment := pumpTestEnv(t, &now)
			bridge, directory := pumpLedgerBridge(t)
			configuration := pumpTestConfig(t, bridge)
			pumpLedgerSetObservation(t, directory, "active", "turn-1")
			pumpLedgerSetMode(t, directory, pumpLedgerLostRefusal)
			message := Message{LogicalID: "lost-refusal", Thread: "mgmt-thread", Text: "body", SteerOnly: true}
			outcome, err := Deliver(context.Background(), environment, configuration, message)
			if err != nil || outcome.Class != deliverClassUnknown || outcome.RequestID != message.LogicalID {
				t.Fatalf("lost refusal: outcome %+v err %v", outcome, err)
			}
			pumpLedgerSetMode(t, directory, pumpLedgerUp)
			if wait {
				pumpLedgerSetObservation(t, directory, "idle", "")
				for round := 0; round < deliverRetryLimit+2; round++ {
					outcome, err = Deliver(context.Background(), environment, configuration, message)
					record := deliverRecordOf(t, configuration, message.LogicalID)
					if err != nil || outcome.Class != deliverClassUnknown || outcome.Receipt["status"] != deliverSteerOnlyWaiting || record.RequestID != message.LogicalID {
						t.Fatalf("idle round %d: outcome %+v record %+v err %v", round, outcome, record, err)
					}
				}
			}
			pumpLedgerSetObservation(t, directory, "active", "turn-2")
			outcome, err = Deliver(context.Background(), environment, configuration, message)
			wantID := deliverRetryRequestID(message.LogicalID, 1)
			if err != nil || outcome.Class != deliverClassAccepted || outcome.RequestID != wantID {
				t.Fatalf("new turn recovery: outcome %+v err %v, want accepted under %s", outcome, err, wantID)
			}
			outcome, err = Deliver(context.Background(), environment, configuration, message)
			if err != nil || outcome.Class != deliverClassAccepted || outcome.RequestID != wantID {
				t.Fatalf("accepted replay: outcome %+v err %v", outcome, err)
			}
			ids, _ := pumpLedgerSends(t, directory)
			if !reflect.DeepEqual(ids, []string{message.LogicalID, wantID}) {
				t.Fatalf("dispatch ids %v, want original and one fresh retry", ids)
			}
			for _, call := range deliverSendCallsOf(t, filepath.Join(directory, "calls.jsonl")) {
				if call["tool"] == deliverToolSend {
					t.Fatalf("steer-only started a turn: %+v", call)
				}
				if call["tool"] == deliverToolSteer && deliverSendRequestIDOf(t, call) == wantID {
					args, _ := call["args"].(map[string]any)
					if args["expected_turn_id"] != "turn-2" {
						t.Fatalf("retry did not steer the recovered turn: %+v", args)
					}
				}
			}
		})
	}
}

func TestDeliverSteerOnlyLostRefusalRecoveryRespectsRetryCap(t *testing.T) {
	now := pumpTestNow
	environment := pumpTestEnv(t, &now)
	bridge, directory := pumpLedgerBridge(t)
	configuration := pumpTestConfig(t, bridge)
	pumpLedgerSetMode(t, directory, pumpLedgerLostRefusal)
	message := Message{LogicalID: "lost-refusal", Thread: "mgmt-thread", Text: "body", SteerOnly: true}
	wantIDs := []string{message.LogicalID}
	for retry := 0; retry <= deliverRetryLimit; retry++ {
		pumpLedgerSetObservation(t, directory, "active", "turn-"+deliverRetryRequestID("test", retry))
		outcome, err := Deliver(context.Background(), environment, configuration, message)
		if retry > 0 {
			wantIDs = append(wantIDs, deliverRetryRequestID(message.LogicalID, retry))
		}
		if err != nil || outcome.Class != deliverClassUnknown || outcome.RequestID != wantIDs[retry] {
			t.Fatalf("retry %d: outcome %+v err %v, want unknown under %s", retry, outcome, err, wantIDs[retry])
		}
	}
	for replay := 0; replay < 2; replay++ {
		outcome, err := Deliver(context.Background(), environment, configuration, message)
		if err != nil || outcome.Class != deliverClassRefused || outcome.RequestID != wantIDs[deliverRetryLimit] {
			t.Fatalf("cap replay %d: outcome %+v err %v", replay, outcome, err)
		}
	}
	ids, _ := pumpLedgerSends(t, directory)
	if !reflect.DeepEqual(ids, wantIDs) {
		t.Fatalf("dispatch ids %v, want bounded retries %v", ids, wantIDs)
	}
}

func TestPumpSteerOnlyLostRefusalDrainsFrozenBatch(t *testing.T) {
	harness := newPumpLedgerHarness(t)
	harness.cfg.raw = map[string]json.RawMessage{"pump": json.RawMessage(`{"steer_only":true}`)}
	harness.cfg.Settings.Management = coreSettings{}
	pumpLedgerSetObservation(t, harness.dir, "active", "turn-1")
	pumpLedgerSetMode(t, harness.dir, pumpLedgerLostRefusal)
	state := harness.round()
	if state.Attempt == nil || len(state.Pending) != 1 || state.Sent["e1"] {
		t.Fatalf("lost refusal must leave a frozen unsettled attempt: %+v", state)
	}
	frozen := *state.Attempt
	harness.addEvent("e2")
	pumpLedgerSetObservation(t, harness.dir, "active", "turn-2")
	pumpLedgerSetMode(t, harness.dir, pumpLedgerUp)
	state = harness.round()
	if !state.Sent["e1"] || state.Attempt != nil || state.MgmtRefused.Count != 0 {
		t.Fatalf("recovery did not drain frozen batch %s: %+v", frozen.LogicalID, state)
	}
	record := deliverRecordOf(t, harness.cfg, frozen.LogicalID)
	if record.State != deliverStateAccepted || record.RequestID != deliverRetryRequestID(frozen.LogicalID, 1) || record.MessageSHA256 != deliverMessageSHA256(frozen.Text) {
		t.Fatalf("recovered batch record %+v, want original body accepted under fresh bounded retry", record)
	}
	state = harness.round()
	if !state.Sent["e2"] || len(state.Pending) != 0 || state.Attempt != nil {
		t.Fatalf("later event blocked after recovery: %+v", state)
	}
	ids, _ := pumpLedgerSends(t, harness.dir)
	if len(ids) != 3 || ids[0] != frozen.LogicalID || ids[1] != record.RequestID || ids[2] == ids[0] || ids[2] == ids[1] {
		t.Fatalf("dispatch ids %v, want refused original, recovered batch and later event", ids)
	}
}
