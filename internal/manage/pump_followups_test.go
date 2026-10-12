package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManageConfigAcceptsSteerOnlyWithoutManagementSettings(t *testing.T) {
	for _, active := range []bool{true, false} {
		t.Run(map[bool]string{true: "active", false: "idle"}[active], func(t *testing.T) {
			now := pumpTestNow
			pumpTestEnv(t, &now)
			observation := map[string]any{"observation": "idle"}
			if active {
				observation = map[string]any{"observation": "active", "activeTurnId": "turn-1"}
			}
			bridge, log := deliverFakeBridge(t, []map[string]any{
				{"payload": observation},
				{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
			})
			state := t.TempDir()
			coreConfigAt(t, coreConfigDocument(t, map[string]any{
				"management_thread": "mgmt-thread", "state_dir": state,
				"bridge": map[string]string{"binary": bridge, "execution_policy": "policy.json"},
				"pump":   map[string]any{"steer_only": true},
			}))
			_, code, errOut := coreConfigReportOf(t)
			if code != 0 {
				t.Fatalf("steer-only config: exit %d stderr %q, want accepted", code, errOut)
			}
			pumpTestSwapSources(t, &pumpTestSource{name: "fake", events: []pumpEvent{{ID: "e1", Kind: pumpKindQuestion, Text: "hello"}}})
			code, _, errOut = coreRunManage(t, "pump", "--once")
			if code != 0 || strings.Contains(errOut, "settings.management") {
				t.Fatalf("steer-only pump: exit %d stderr %q", code, errOut)
			}
			st := pumpTestReadState(t, &Config{StateDir: state})
			if st.Sent["e1"] != active || len(st.Parked) != 0 {
				t.Fatalf("active=%v: sent %v parked %+v", active, st.Sent, st.Parked)
			}
			if !active && (len(st.Pending) != 1 || st.Attempt != nil) {
				t.Fatalf("idle pump must wait without an attempt: pending %+v attempt %+v", st.Pending, st.Attempt)
			}
			for _, call := range deliverSendCallsOf(t, log) {
				if call["tool"] == deliverToolSend {
					t.Fatal("steer-only pump started a turn")
				}
				if call["tool"] == deliverToolSteer {
					args := call["args"].(map[string]any)
					if args["expected_settings"] != nil {
						t.Fatal("steer carries expected_settings")
					}
				}
			}
		})
	}
}

func TestPumpSteerOnlyNeverStartsAnIdleTurnAfterTheProbe(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	activeBridge, activeLog := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	idleBridge, idleLog := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	dir := t.TempDir()
	marker := filepath.Join(dir, "probed")
	bridge := filepath.Join(dir, "bridge")
	script := "#!/bin/sh\nif [ -e " + coreShellQuote(marker) + " ]; then\nexec " + coreShellQuote(idleBridge) +
		"\nfi\ntouch " + coreShellQuote(marker) + "\nexec " + coreShellQuote(activeBridge) + "\n"
	if err := os.WriteFile(bridge, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := pumpTestConfig(t, bridge)
	cfg.raw = map[string]json.RawMessage{"pump": json.RawMessage(`{"steer_only":true}`)}
	pumpTestSwapSources(t, &pumpTestSource{name: "fake", events: []pumpEvent{{ID: "e1", Kind: pumpKindQuestion, Text: "hello"}}})
	pumpRefusedRound(t, e, cfg)
	for _, call := range append(deliverSendCallsOf(t, activeLog), deliverSendCallsOf(t, idleLog)...) {
		if call["tool"] == deliverToolSend {
			t.Fatal("steer-only pump started a turn after its active probe went idle")
		}
	}
	st := pumpTestReadState(t, cfg)
	if len(st.Pending) != 1 || st.Sent["e1"] || st.MgmtRefused.Count != 0 {
		t.Fatalf("the unsent event must remain pending: %+v", st)
	}
	if st.Attempt == nil {
		t.Fatal("the idle race must preserve the frozen body")
	}
	requestID := st.Attempt.LogicalID
	if record, known, err := deliverLoad(cfg, requestID); err != nil || known {
		t.Fatalf("the idle race wrote a request id nothing was dispatched under: record %+v known %v err %v", record, known, err)
	}
	for round := 0; round < pumpQueueRefusalLimit+deliverRetryLimit; round++ {
		pumpRefusedRound(t, e, cfg)
		st = pumpTestReadState(t, cfg)
		record, known, err := deliverLoad(cfg, requestID)
		if err != nil || known || st.Attempt == nil || st.Attempt.LogicalID != requestID || st.MgmtRefused != (pumpQueueRefusal{}) || len(st.Parked) != 0 {
			t.Fatalf("idle round %d consumed a retry or refusal: record %+v known %v state %+v err %v", round, record, known, st, err)
		}
	}
	cfg.Bridge.Binary = activeBridge
	pumpRefusedRound(t, e, cfg)
	st = pumpTestReadState(t, cfg)
	if !st.Sent["e1"] || st.Attempt != nil || len(st.Pending) != 0 {
		t.Fatalf("the active turn did not receive the waiting batch: %+v", st)
	}
	for _, call := range deliverSendCallsOf(t, activeLog) {
		if call["tool"] == deliverToolSteer && call["args"].(map[string]any)["request_id"] != requestID {
			t.Fatalf("waiting spent a request id: %+v", call)
		}
	}
}

func TestPumpStrandedCloseDoesNotCountAsARefusal(t *testing.T) {
	h := newPumpLedgerHarness(t)
	pumpLedgerSetMode(t, h.dir, pumpLedgerUnknown)
	st := h.round()
	base := pumpBatchID(st.Pending)
	h.cfg.ManagementThread = "mgmt-thread-2"
	st = h.round()
	if st.Attempt != nil || st.MgmtRefused.Count != 0 {
		t.Errorf("the stranded close counted as a refusal: attempt %+v refused %+v", st.Attempt, st.MgmtRefused)
	}
	pumpLedgerSetMode(t, h.dir, pumpLedgerDown)
	for n := 1; n <= pumpQueueRefusalLimit; n++ {
		st = h.round()
		if n < pumpQueueRefusalLimit {
			if len(st.Pending) != 1 || len(st.Parked) != 0 || st.MgmtRefused.Count != n {
				t.Fatalf("after %d bridge refusals: pending %+v parked %+v refused %+v", n, st.Pending, st.Parked, st.MgmtRefused)
			}
		} else if len(st.Pending) != 0 || len(st.Parked) != 1 {
			t.Fatalf("after three bridge refusals: pending %+v parked %+v", st.Pending, st.Parked)
		}
	}
	ids, _ := pumpLedgerSends(t, h.dir)
	for n := 1; n <= pumpQueueRefusalLimit; n++ {
		if ids[1+(n-1)*3] != pumpMgmtOrdinalID(base, n) {
			t.Errorf("refusal %d reused an id: %v", n, ids)
		}
	}
	raw, err := os.ReadFile(filepath.Join(h.cfg.StateDir, pumpLogFile))
	if err != nil || !strings.Contains(string(raw), "parked after 3 refusals") {
		t.Fatalf("park log: %v\n%s", err, raw)
	}
}

func TestPumpSteerOnlyKeepsUnknownAttemptFrozen(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "outcome_unknown"}},
	})
	cfg := pumpTestConfig(t, bridge)
	cfg.raw = map[string]json.RawMessage{"pump": json.RawMessage(`{"steer_only":true}`)}
	pumpTestSwapSources(t, &pumpTestSource{name: "fake", events: []pumpEvent{{ID: "e1", Kind: pumpKindQuestion, Text: "hello"}}})
	pumpRefusedRound(t, e, cfg)
	st := pumpTestReadState(t, cfg)
	if st.Attempt == nil || st.Sent["e1"] {
		t.Fatalf("uncertain steer was not frozen: %+v", st)
	}
	frozen := *st.Attempt
	unknownBridge, unknownLog := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"status": "outcome_unknown"}},
	})
	cfg.Bridge.Binary = unknownBridge
	for round := 0; round < 3; round++ {
		pumpRefusedRound(t, e, cfg)
		st = pumpTestReadState(t, cfg)
		if st.Attempt == nil || st.Attempt.LogicalID != frozen.LogicalID || st.Attempt.Text != frozen.Text || st.MgmtRefused.Count != 0 || len(st.Parked) != 0 {
			t.Fatalf("uncertain steer lost its frozen identity: %+v", st)
		}
	}
	for _, call := range deliverSendCallsOf(t, unknownLog) {
		if call["tool"] != deliverToolOperation {
			t.Fatalf("uncertain steer was retried without proof: %+v (first calls %+v)", call, deliverSendCallsOf(t, log))
		}
	}
}

func pumpLedgerSetObservation(t *testing.T, dir, observation, turnID string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"observation": observation, "activeTurnId": turnID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "observation.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDeliverSteerOnlyUnmeasuredRaceKeepsWaitingWithoutRetries(t *testing.T) {
	for _, observation := range []string{"active_without_in_progress_turn", "in_progress_turn_without_active_status", "notLoaded", "systemError", "unknown", "active"} {
		t.Run(observation, func(t *testing.T) {
			now := pumpTestNow
			environment := pumpTestEnv(t, &now)
			bridge, dir := pumpLedgerBridge(t)
			configuration := pumpTestConfig(t, bridge)
			message := Message{LogicalID: "waiting-message", Thread: "mgmt-thread", Text: "body", SteerOnly: true}
			outcome, err := Deliver(context.Background(), environment, configuration, message)
			if err != nil || outcome.Class != deliverClassUnknown || outcome.Receipt["status"] != deliverSteerOnlyWaiting {
				t.Fatalf("initial idle wait: %+v err %v", outcome, err)
			}
			pumpLedgerSetObservation(t, dir, observation, "")
			for round := 0; round < pumpQueueRefusalLimit*(deliverRetryLimit+3); round++ {
				outcome, err = Deliver(context.Background(), environment, configuration, message)
				if err != nil || outcome.Class != deliverClassUnknown || outcome.RequestID != message.LogicalID || outcome.Receipt["status"] != deliverSteerOnlyWaiting {
					t.Errorf("observation round %d consumed a waiting request: %+v err %v", round, outcome, err)
				}
			}
			for _, call := range deliverSendCallsOf(t, filepath.Join(dir, "calls.jsonl")) {
				if call["tool"] != deliverToolActive {
					t.Errorf("unsent observation caused reconciliation or dispatch: %+v", call)
				}
			}
			if record, known, err := deliverLoad(configuration, message.LogicalID); err != nil || known {
				t.Fatalf("observations wrote a request id nothing was dispatched under: record %+v known %v err %v", record, known, err)
			}
		})
	}
}

func TestPumpSteerOnlyObservationWaitNeverParksAndRecovers(t *testing.T) {
	harness := newPumpLedgerHarness(t)
	harness.cfg.raw = map[string]json.RawMessage{"pump": json.RawMessage(`{"steer_only":true}`)}
	harness.cfg.Settings.Management = coreSettings{}
	activeBridge, activeLog := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	marker := filepath.Join(harness.dir, "probed")
	bridge := filepath.Join(harness.dir, "race-bridge")
	script := "#!/bin/sh\nif [ -e " + coreShellQuote(marker) + " ]; then\nexec " + coreShellQuote(harness.cfg.Bridge.Binary) +
		"\nfi\ntouch " + coreShellQuote(marker) + "\nexec " + coreShellQuote(activeBridge) + "\n"
	if err := os.WriteFile(bridge, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	harness.cfg.Bridge.Binary = bridge
	state := harness.round()
	if state.Attempt == nil || len(state.Pending) != 1 {
		t.Fatalf("active-to-idle race did not freeze the unsent batch: %+v", state)
	}
	frozen := *state.Attempt
	observations := []string{"active_without_in_progress_turn", "in_progress_turn_without_active_status", "idle"}
	for round := 0; round < pumpQueueRefusalLimit*(deliverRetryLimit+4); round++ {
		pumpLedgerSetObservation(t, harness.dir, observations[round%len(observations)], "")
		if state.Attempt == nil {
			if err := os.Remove(marker); err != nil {
				t.Fatal(err)
			}
		}
		state = harness.round()
		t.Logf("round %d: pending=%d refused=%+v parked=%d", round, len(state.Pending), state.MgmtRefused, len(state.Parked))
		if len(state.Pending) != 1 || len(state.Parked) != 0 || state.Sent["e1"] || state.MgmtRefused.Count != 0 || state.MgmtRefused.Ordinal != 0 || state.Attempt == nil || state.Attempt.LogicalID != frozen.LogicalID || state.Attempt.Text != frozen.Text {
			t.Errorf("observation round %d spent a refusal or lost the frozen batch: %+v", round, state)
		}
	}
	for _, log := range []string{activeLog, filepath.Join(harness.dir, "calls.jsonl")} {
		for _, call := range deliverSendCallsOf(t, log) {
			if call["tool"] != deliverToolActive {
				t.Errorf("observation-only rounds attempted reconciliation or delivery: %+v", call)
			}
		}
	}
	if record, known, err := deliverLoad(harness.cfg, frozen.LogicalID); err != nil || known {
		t.Fatalf("unsent observations wrote a request id nothing was dispatched under: record %+v known %v err %v", record, known, err)
	}
	harness.addEvent("e2")
	state = harness.round()
	if len(state.Pending) != 2 || state.Attempt == nil || state.Attempt.Text != frozen.Text || len(state.Attempt.IDs) != 1 || state.Attempt.IDs[0] != "e1" {
		t.Fatalf("waiting blocked collection or changed the frozen batch: %+v", state)
	}
	harness.cfg.Bridge.Binary = activeBridge
	state = harness.round()
	if !state.Sent["e1"] || state.Sent["e2"] || state.Attempt != nil || len(state.Pending) != 1 || len(state.Parked) != 0 {
		t.Fatalf("active recovery did not settle only the frozen batch: %+v", state)
	}
	steers := 0
	for _, call := range deliverSendCallsOf(t, activeLog) {
		if call["tool"] == deliverToolSend {
			t.Fatal("steer-only recovery started a turn")
		}
		if call["tool"] == deliverToolSteer {
			steers++
			args := call["args"].(map[string]any)
			if args["request_id"] != frozen.LogicalID || args["message"] != frozen.Text || args["expected_turn_id"] != "turn-1" {
				t.Fatalf("recovery changed the waiting dispatch: %+v", args)
			}
		}
	}
	if steers != 1 {
		t.Fatalf("waiting batch received %d steers, want one", steers)
	}
	state = harness.round()
	if !state.Sent["e2"] || state.Attempt != nil || len(state.Pending) != 0 || len(state.Parked) != 0 || state.MgmtRefused.Count != 0 {
		t.Fatalf("subsequent collection did not drain after recovery: %+v", state)
	}
}

func TestPumpManagementOrdinalReadsLegacyRefusalCount(t *testing.T) {
	h := newPumpLedgerHarness(t)
	st := pumpNewState()
	st.Pending = []pumpEvent{{ID: "e1", Kind: pumpKindQuestion, Text: "first"}}
	firstAt := float64(h.now.Unix())
	st.FirstAt = &firstAt
	base := pumpBatchID(st.Pending)
	st.MgmtRefused = pumpQueueRefusal{ID: base, Count: 1}
	if err := st.pumpSave(h.cfg); err != nil {
		t.Fatal(err)
	}
	pumpLedgerSetMode(t, h.dir, pumpLedgerDown)
	st = h.round()
	ids, _ := pumpLedgerSends(t, h.dir)
	if len(ids) != 3 || ids[0] != pumpMgmtOrdinalID(base, 1) || st.MgmtRefused.Count != 2 || st.MgmtRefused.Ordinal != 2 {
		t.Fatalf("legacy count did not preserve the spent ordinal: ids %v refused %+v", ids, st.MgmtRefused)
	}
}

func TestPumpQueueRefusalOrdinalNeverCollidesWithDeliverRetryIDs(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, dir := pumpLedgerBridge(t)
	cfg := pumpTestConfig(t, bridge)
	cfg.Settings.Parent = coreSettings{Model: "m1", ReasoningEffort: "low"}
	path := pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "a-body")
	old := now.Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	flush := func() {
		t.Helper()
		if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
			t.Fatal(err)
		}
	}
	pumpLedgerSetMode(t, dir, pumpLedgerDown)
	flush()
	ids, _ := pumpLedgerSends(t, dir)
	if len(ids) != 3 || ids[1] != deliverRetryRequestID(ids[0], 1) || ids[2] != deliverRetryRequestID(ids[0], 2) {
		t.Fatalf("outage request ids %v, want B, B-r1, B-r2", ids)
	}
	base := ids[0]
	pumpLedgerSetMode(t, dir, pumpLedgerUp)
	cfg.Settings.Parent.ReasoningEffort = "medium"
	flush()
	if names := pumpQueueTestNames(t, cfg, "parent-1"); len(names) != 0 {
		t.Fatalf("the recovered bridge left notices queued: %v (ids %v)", names, mustSends(t, dir))
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", pumpSentDir, filepath.Base(path))); err != nil {
		t.Fatal(err)
	}
	ids, _ = pumpLedgerSends(t, dir)
	if len(ids) != 4 || ids[3] != base+"-m1" {
		t.Fatalf("recovery request ids %v, want fresh B-m1", ids)
	}
	flush()
	if len(mustSends(t, dir)) != 4 {
		t.Fatal("an accepted notice was sent twice")
	}
}

// pumpSteerOnlyRace puts a bridge in front of the harness's ledger bridge: the first start (the pump's
// own probe of a round with no frozen attempt) is a bridge that answers an active turn and leaves the
// returned marker, every later start is the ledger bridge, whose observation is idle unless the test
// writes another. The thread goes idle between the pump's probe and the delivery core's own
// observation; removing the marker arms the race again.
func pumpSteerOnlyRace(t *testing.T, h *pumpLedgerHarness) (marker, probeLog string) {
	t.Helper()
	h.cfg.raw = map[string]json.RawMessage{"pump": json.RawMessage(`{"steer_only":true}`)}
	h.cfg.Settings.Management = coreSettings{}
	probe, probeLog := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
	})
	marker = filepath.Join(h.dir, "probed")
	bridge := filepath.Join(h.dir, "race-bridge")
	script := "#!/bin/sh\nif [ -e " + coreShellQuote(marker) + " ]; then\nexec " + coreShellQuote(h.cfg.Bridge.Binary) +
		"\nfi\ntouch " + coreShellQuote(marker) + "\nexec " + coreShellQuote(probe) + "\n"
	if err := os.WriteFile(bridge, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	h.cfg.Bridge.Binary = bridge
	return marker, probeLog
}

// pumpSteerOnlyDispatches is every call of the logs that is no observation: a send, a steer or a
// get_operation lookup.
func pumpSteerOnlyDispatches(t *testing.T, logs ...string) []map[string]any {
	t.Helper()
	var calls []map[string]any
	for _, log := range logs {
		for _, call := range deliverSendCallsOf(t, log) {
			if call["tool"] != deliverToolActive {
				calls = append(calls, call)
			}
		}
	}
	return calls
}

// CRW-1275 (independent verification of 79d1d71a, P0): an idle race leaves a steer-only attempt that was
// never dispatched, and then the delivery core's get_active_turn fails in transport in every round while
// get_operation keeps answering. Nothing went to the bridge, so nothing is reconciled: no -r retry id and
// no refusal is spent, the ordinal does not move, and the event is never parked. When the thread is
// active again, the frozen batch is steered once, under its own id and body.
func TestPumpSteerOnlyObservationTransportFailureNeverParks(t *testing.T) {
	h := newPumpLedgerHarness(t)
	marker, probeLog := pumpSteerOnlyRace(t, h)
	st := h.round()
	if st.Attempt == nil || len(st.Pending) != 1 {
		t.Fatalf("the idle race did not freeze the unsent batch: %+v", st)
	}
	frozen := *st.Attempt
	if err := os.WriteFile(filepath.Join(h.dir, pumpLedgerObservationDown), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ledgerLog := filepath.Join(h.dir, "calls.jsonl")
	for round := 0; round < pumpQueueRefusalLimit*(deliverRetryLimit+3); round++ {
		if st.Attempt == nil {
			if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
		st = h.round()
		if len(st.Pending) != 1 || len(st.Parked) != 0 || st.MgmtRefused != (pumpQueueRefusal{}) ||
			st.Attempt == nil || st.Attempt.LogicalID != frozen.LogicalID || st.Attempt.Text != frozen.Text {
			t.Fatalf("round %d: an observation that never answered spent the unsent batch: pending %d parked %d refused %+v attempt %+v",
				round, len(st.Pending), len(st.Parked), st.MgmtRefused, st.Attempt)
		}
		if calls := pumpSteerOnlyDispatches(t, probeLog, ledgerLog); len(calls) != 0 {
			t.Fatalf("round %d: an unsent attempt was reconciled or dispatched: %+v", round, calls)
		}
	}
	if err := os.Remove(filepath.Join(h.dir, pumpLedgerObservationDown)); err != nil {
		t.Fatal(err)
	}
	pumpLedgerSetObservation(t, h.dir, "active", "turn-2")
	pumpLedgerSetMode(t, h.dir, pumpLedgerUp)
	st = h.round()
	if !st.Sent["e1"] || st.Attempt != nil || len(st.Pending) != 0 || len(st.Parked) != 0 {
		t.Fatalf("the active turn did not receive the frozen batch: %+v", st)
	}
	calls := pumpSteerOnlyDispatches(t, probeLog, ledgerLog)
	if len(calls) != 1 || calls[0]["tool"] != deliverToolSteer {
		t.Fatalf("recovery calls %+v, want exactly one steer", calls)
	}
	args := calls[0]["args"].(map[string]any)
	if args["request_id"] != frozen.LogicalID || args["message"] != frozen.Text || args["expected_turn_id"] != "turn-2" || args["expected_settings"] != nil {
		t.Fatalf("recovery steered %+v, want the frozen id %s and body into turn-2", args, frozen.LogicalID)
	}
}

// CRW-1275 (independent verification of 79d1d71a, P1): management_thread changes while a steer-only attempt
// that was never dispatched is frozen. Nothing was sent to the old thread, so there is nothing to close:
// no get_operation lookup, no refusal and no ordinal are spent, and the frozen batch goes to the
// configured thread under its own id once that thread has an active turn.
func TestPumpSteerOnlyUnsentAttemptFollowsAManagementThreadChange(t *testing.T) {
	h := newPumpLedgerHarness(t)
	_, probeLog := pumpSteerOnlyRace(t, h)
	st := h.round()
	if st.Attempt == nil || len(st.Pending) != 1 {
		t.Fatalf("the idle race did not freeze the unsent batch: %+v", st)
	}
	frozen := *st.Attempt
	h.cfg.ManagementThread = "mgmt-thread-2"
	pumpLedgerSetObservation(t, h.dir, "active", "turn-2")
	pumpLedgerSetMode(t, h.dir, pumpLedgerUp)
	st = h.round()
	if st.MgmtRefused != (pumpQueueRefusal{}) || len(st.Parked) != 0 {
		t.Fatalf("the unsent attempt was charged when the thread changed: refused %+v parked %+v", st.MgmtRefused, st.Parked)
	}
	if !st.Sent["e1"] || st.Attempt != nil || len(st.Pending) != 0 {
		t.Fatalf("the frozen batch did not reach the configured thread: %+v", st)
	}
	calls := pumpSteerOnlyDispatches(t, probeLog, filepath.Join(h.dir, "calls.jsonl"))
	if len(calls) != 1 || calls[0]["tool"] != deliverToolSteer {
		t.Fatalf("calls after the thread change %+v, want exactly one steer and no lookup", calls)
	}
	args := calls[0]["args"].(map[string]any)
	if args["thread_id"] != "mgmt-thread-2" || args["request_id"] != frozen.LogicalID || args["message"] != frozen.Text {
		t.Fatalf("the batch was steered as %+v, want the frozen id %s and body on mgmt-thread-2", args, frozen.LogicalID)
	}
	raw, err := os.ReadFile(filepath.Join(h.cfg.StateDir, pumpLogFile))
	if err != nil || strings.Contains(string(raw), "closed as stranded") {
		t.Fatalf("pump.log closed an attempt that was never sent: %v\n%s", err, raw)
	}
}

// CRW-1275: a steer-only steer that the bridge refuses as not_delivered is retried under a new request id
// into the turn the call's one observation found, without observing again, so every id the outbox holds
// is one the bridge was handed. The thread reads idle right after the first steer: a second observation
// would end the call with the retry id unsent, and the next call would reconcile the first id to its
// not_attempted receipt and steer it again into another turn, which the bridge's ledger refuses as a
// conflict for good.
func TestDeliverSteerOnlyRetrySteersTheObservedTurnWithoutObservingAgain(t *testing.T) {
	for _, tc := range []struct {
		name      string
		recovers  bool
		wantClass string
		wantIDs   []string
	}{
		{"the outage lasts", false, deliverClassRefused, []string{"retry-message", "retry-message-r1", "retry-message-r2"}},
		{"the host recovers after the first steer", true, deliverClassAccepted, []string{"retry-message", "retry-message-r1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := pumpTestNow
			environment := pumpTestEnv(t, &now)
			bridge, dir := pumpLedgerBridge(t)
			configuration := pumpTestConfig(t, bridge)
			pumpLedgerSetMode(t, dir, pumpLedgerDown)
			pumpLedgerSetObservation(t, dir, "active", "turn-1")
			if err := os.WriteFile(filepath.Join(dir, pumpLedgerObservationNext), []byte(`{"observation":"idle"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.recovers {
				if err := os.WriteFile(filepath.Join(dir, pumpLedgerModeNext), []byte(pumpLedgerUp), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			message := Message{LogicalID: "retry-message", Thread: "mgmt-thread", Text: "body", SteerOnly: true}
			outcome, err := Deliver(context.Background(), environment, configuration, message)
			if outcome.Class != tc.wantClass {
				t.Fatalf("outcome %+v err %v, want class %s", outcome, err, tc.wantClass)
			}
			observations := 0
			var steered []string
			for _, call := range deliverSendCallsOf(t, filepath.Join(dir, "calls.jsonl")) {
				args, _ := call["args"].(map[string]any)
				switch call["tool"] {
				case deliverToolActive:
					observations++
				case deliverToolSteer:
					steered = append(steered, deliverSendRequestIDOf(t, call))
					if args["expected_turn_id"] != "turn-1" {
						t.Errorf("a retry steered %+v, want the observed turn-1", args)
					}
				default:
					t.Errorf("unexpected call %+v", call)
				}
			}
			if observations != 1 || strings.Join(steered, " ") != strings.Join(tc.wantIDs, " ") {
				t.Fatalf("observations %d steers %v, want one observation and steers %v", observations, steered, tc.wantIDs)
			}
			record, known, err := deliverLoad(configuration, message.LogicalID)
			if err != nil || !known || record.RequestID != tc.wantIDs[len(tc.wantIDs)-1] {
				t.Fatalf("outbox record %+v known %v err %v, want the last dispatched id %s", record, known, err, tc.wantIDs[len(tc.wantIDs)-1])
			}
		})
	}
}
