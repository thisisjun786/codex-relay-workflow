package manage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pumpRefusedBridge is a bridge that finds the thread idle and answers the send with a refusal
// whose sentence the pump log must carry.
func pumpRefusedBridge(t *testing.T) (bridge, log string) {
	t.Helper()
	return deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "refused", "error": "expected_settings is required"}},
	})
}

// pumpRefusedAcceptingBridge is a bridge that finds the thread idle and takes the send.
func pumpRefusedAcceptingBridge(t *testing.T) (bridge, log string) {
	t.Helper()
	return deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
}

func pumpRefusedRound(t *testing.T, e *Env, cfg *Config) {
	t.Helper()
	if _, err := pumpRound(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); err != nil {
		t.Logf("round error (the refusal is reported): %v", err)
	}
}

// CRW-1273 (1): a batch the bridge refused is sent again once the cause is fixed. The refusal
// settles the logical id in the ledger and lifts the freeze; the next attempt of the same events
// goes out under <id>-m1 (the rule of the parent queue, CRW-991) and is delivered, carrying the
// settings that were fixed.
func TestPumpRefusedBatchIsDeliveredAfterTheSettingsAreFixed(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	pumpTestSwapSources(t, &pumpTestSource{name: "fake", events: []pumpEvent{{ID: "e1", Kind: pumpKindQuestion, Text: "hello"}}})

	refusing, refusedLog := pumpRefusedBridge(t)
	cfg.Bridge.Binary = refusing
	pumpRefusedRound(t, e, cfg)
	st := pumpTestReadState(t, cfg)
	if len(st.Pending) != 1 || st.Attempt != nil {
		t.Fatalf("after the refusal: pending %d attempt %v, want the event pending and no freeze", len(st.Pending), st.Attempt)
	}
	first := pumpQueueTestSendIDs(t, refusedLog)
	if len(first) != 1 || first[0] != pumpBatchID(st.Pending) {
		t.Fatalf("the refused send used %v, want the batch id %s", first, pumpBatchID(st.Pending))
	}

	// The settings are fixed; the next round delivers the same event.
	cfg.Settings.Management = coreSettings{Model: "m1", ReasoningEffort: "low"}
	accepting, acceptedLog := pumpRefusedAcceptingBridge(t)
	cfg.Bridge.Binary = accepting
	now = now.Add(5 * time.Second)
	pumpRefusedRound(t, e, cfg)
	st = pumpTestReadState(t, cfg)
	if len(st.Pending) != 0 || st.Attempt != nil || !st.Sent["e1"] {
		t.Fatalf("after the fix: pending %+v attempt %v sent %v, want the event delivered", st.Pending, st.Attempt, st.Sent)
	}
	ids := pumpQueueTestSendIDs(t, acceptedLog)
	if len(ids) != 1 || ids[0] != first[0]+"-m1" {
		t.Fatalf("the second attempt used %v, want %s-m1", ids, first[0])
	}
	for _, call := range deliverSendCallsOf(t, acceptedLog) {
		if call["tool"] != deliverToolSend {
			continue
		}
		args, _ := call["args"].(map[string]any)
		if args["expected_settings"] == nil {
			t.Errorf("the delivered send carries no expected_settings: %v", args)
		}
	}
	if st.MgmtRefused.Count != 0 {
		t.Errorf("the refusal count survived the delivery: %+v", st.MgmtRefused)
	}
}

// CRW-1273 (1): the body carries the HH:MM of the round, so a retry in another minute is another
// message under the same id. It must not meet "already belongs to another message".
func TestPumpRefusedBatchAcrossAMinuteBoundary(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 59, 50, 0, time.UTC)
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	cfg.Settings.Management = coreSettings{Model: "m1", ReasoningEffort: "low"}
	pumpTestSwapSources(t, &pumpTestSource{name: "fake", events: []pumpEvent{{ID: "e1", Kind: pumpKindQuestion, Text: "hello"}}})

	refusing, _ := pumpRefusedBridge(t)
	cfg.Bridge.Binary = refusing
	pumpRefusedRound(t, e, cfg)
	if st := pumpTestReadState(t, cfg); len(st.Pending) != 1 {
		t.Fatalf("the refusal left pending %+v, want the event", st.Pending)
	}

	accepting, acceptedLog := pumpRefusedAcceptingBridge(t)
	cfg.Bridge.Binary = accepting
	now = now.Add(20 * time.Second) // 10:00:10: the body head reads 10:00, not 09:59
	if _, err := pumpRound(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); err != nil {
		t.Fatalf("the round after the minute changed: %v", err)
	}
	st := pumpTestReadState(t, cfg)
	if len(st.Pending) != 0 || !st.Sent["e1"] {
		t.Fatalf("pending %+v sent %v, want the event delivered across the minute", st.Pending, st.Sent)
	}
	if ids := pumpQueueTestSendIDs(t, acceptedLog); len(ids) != 1 || !strings.HasSuffix(ids[0], "-m1") {
		t.Fatalf("the second attempt used %v, want the -m1 ordinal", ids)
	}
}

// CRW-1273 (1): the limit and the record. The third refusal of one id parks the batch's events (they
// leave pending, stay in the state with the reason and are not collected again), so the events
// behind them flow.
func TestPumpRefusedBatchIsParkedAfterTheLimit(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	pumpTestSwapSources(t, &pumpTestSource{name: "fake", events: []pumpEvent{{ID: "e1", Kind: pumpKindQuestion, Text: "hello"}}})
	var ids []string
	for round := 0; round < pumpQueueRefusalLimit; round++ {
		bridge, log := pumpRefusedBridge(t)
		cfg.Bridge.Binary = bridge
		now = now.Add(61 * time.Second)
		pumpRefusedRound(t, e, cfg)
		ids = append(ids, pumpQueueTestSendIDs(t, log)...)
	}
	base := ids[0]
	if len(ids) != 3 || ids[1] != base+"-m1" || ids[2] != base+"-m2" {
		t.Fatalf("the sends used %v, want %s then -m1 then -m2", ids, base)
	}
	st := pumpTestReadState(t, cfg)
	if len(st.Pending) != 0 || len(st.Parked) != 1 || st.Parked[0].ID != "e1" {
		t.Fatalf("pending %+v parked %+v, want e1 parked", st.Pending, st.Parked)
	}
	if !strings.Contains(st.Parked[0].Reason, "expected_settings is required") || st.MgmtRefused.Count != 0 {
		t.Errorf("parked reason %q, count %+v", st.Parked[0].Reason, st.MgmtRefused)
	}
	// The parked event is not collected again by the source that keeps producing it.
	bridge, log := pumpRefusedAcceptingBridge(t)
	cfg.Bridge.Binary = bridge
	now = now.Add(61 * time.Second)
	pumpRefusedRound(t, e, cfg)
	if st := pumpTestReadState(t, cfg); len(st.Pending) != 0 {
		t.Errorf("a parked event came back: %+v", st.Pending)
	}
	if sent := pumpQueueTestSendIDs(t, log); len(sent) != 0 {
		t.Errorf("a parked event was sent: %v", sent)
	}
	raw, err := os.ReadFile(filepath.Join(cfg.StateDir, pumpLogFile))
	if err != nil || !strings.Contains(string(raw), "management event e1 parked after 3 refusals") {
		t.Errorf("the log lacks the parked line: %v\n%s", err, raw)
	}
}

// CRW-1273 (3): the pump log's deliver line carries the bridge's refusal, on one line.
func TestPumpLogCarriesTheBridgeRefusal(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	pumpTestSwapSources(t, &pumpTestSource{name: "fake", events: []pumpEvent{{ID: "e1", Kind: pumpKindQuestion, Text: "hello"}}})
	bridge, _ := pumpRefusedBridge(t)
	cfg.Bridge.Binary = bridge
	pumpRefusedRound(t, e, cfg)
	raw, err := os.ReadFile(filepath.Join(cfg.StateDir, pumpLogFile))
	if err != nil {
		t.Fatal(err)
	}
	var deliver string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, " deliver request=") {
			deliver = line
		}
	}
	if !strings.Contains(deliver, "class=refused") || !strings.Contains(deliver, "reason=") || !strings.Contains(deliver, "expected_settings is required") {
		t.Fatalf("the deliver line = %q, want the refusal and its reason", deliver)
	}
}

// CRW-1273 (2): a configuration that names a management thread needs both settings.management
// values, or every send fails in the bridge's argument validation. crw manage config refuses it.
func TestManageConfigRefusesEmptyManagementSettings(t *testing.T) {
	cases := []struct {
		name     string
		manage   map[string]any
		refused  bool
		mentions string
	}{
		{"no settings", map[string]any{"management_thread": "01abc"}, true, "model and reasoning_effort"},
		{"no effort", map[string]any{"management_thread": "01abc", "settings": map[string]any{"management": map[string]string{"model": "m1"}}}, true, "reasoning_effort"},
		{"no model", map[string]any{"management_thread": "01abc", "settings": map[string]any{"management": map[string]string{"reasoning_effort": "low"}}}, true, "model is empty"},
		{"both", map[string]any{"management_thread": "01abc", "settings": map[string]any{"management": map[string]string{"model": "m1", "reasoning_effort": "low"}}}, false, ""},
		{"no thread", map[string]any{"repository": "/srv/checkout"}, false, ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			coreTempHome(t)
			coreConfigAt(t, coreConfigDocument(t, test.manage))
			_, code, errOut := coreConfigReportOf(t)
			if !test.refused {
				if code != 0 {
					t.Fatalf("exit %d: %s", code, errOut)
				}
				return
			}
			if code != usageExit || !strings.Contains(errOut, "settings.management") || !strings.Contains(errOut, test.mentions) {
				t.Fatalf("exit %d stderr %q, want the refusal naming %q", code, errOut, test.mentions)
			}
		})
	}
}

// CRW-1273 (2): the pump does not start on such a configuration; a dry run only warns.
func TestPumpStartRefusesEmptyManagementSettings(t *testing.T) {
	coreTempHome(t)
	coreConfigAt(t, coreConfigDocument(t, map[string]any{"management_thread": "01abc"}))
	// The dry run goes on to a round: no source may reach the network or a tool of this host.
	pumpTestSwapSources(t, &pumpTestSource{name: "fake"})
	code, _, errOut := coreRunManage(t, "pump", "--once")
	if code != usageExit || !strings.Contains(errOut, "settings.management") {
		t.Fatalf("pump --once: exit %d stderr %q, want the refusal", code, errOut)
	}
	code, _, errOut = coreRunManage(t, "pump", "--dry-run")
	if code != 0 || !strings.Contains(errOut, "warning") || !strings.Contains(errOut, "settings.management") {
		t.Fatalf("pump --dry-run: exit %d stderr %q, want a warning and exit 0", code, errOut)
	}
}

// pumpRefusedLastDeliverLine is the last deliver line of the pump log.
func pumpRefusedLastDeliverLine(t *testing.T, cfg *Config) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cfg.StateDir, pumpLogFile))
	if err != nil {
		t.Fatal(err)
	}
	var deliver string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, " deliver request=") {
			deliver = line
		}
	}
	return deliver
}

// pumpRefusedUnknownBridge is a bridge that finds the thread idle and answers the send with an
// undetermined receipt, so the delivery core freezes the attempt and records an excerpt.
func pumpRefusedUnknownBridge(t *testing.T) (bridge, log string) {
	t.Helper()
	return deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "in_progress_or_unknown", "retrySafe": false}},
	})
}

// CRW-1273 (1, post-evaluation d1): a refusal the delivery core reports before it reads the ledger
// (an unset execution policy) says nothing about the frozen attempt, which may already have been
// accepted. The attempt stays frozen with its id and body, and once the policy is back the next round
// reconciles that id with get_operation instead of sending the events again.
func TestPumpUnprovenLocalRefusalKeepsTheFrozenAttempt(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	cfg.Settings.Management = coreSettings{Model: "m1", ReasoningEffort: "low"}
	src := &pumpTestSource{name: "fake", events: []pumpEvent{{ID: "A", Kind: pumpKindQuestion, Text: "A"}}}
	pumpTestSwapSources(t, src)

	unknown, _ := pumpRefusedUnknownBridge(t)
	cfg.Bridge.Binary = unknown
	pumpRefusedRound(t, e, cfg)
	frozen := pumpTestReadState(t, cfg)
	if frozen.Attempt == nil {
		t.Fatal("the undetermined attempt was not frozen")
	}
	want := *frozen.Attempt

	// The policy is gone, and a new event has come in: the round refuses locally.
	cfg.Bridge.ExecutionPolicy = ""
	src.events = []pumpEvent{{ID: "A", Kind: pumpKindQuestion, Text: "A"}, {ID: "B", Kind: pumpKindQuestion, Text: "B"}}
	now = now.Add(61 * time.Second)
	pumpRefusedRound(t, e, cfg)
	st := pumpTestReadState(t, cfg)
	if st.Attempt == nil || st.Attempt.LogicalID != want.LogicalID || st.Attempt.Text != want.Text {
		t.Fatalf("after the local refusal the attempt = %+v, want the frozen %+v", st.Attempt, want)
	}
	if st.MgmtRefused.Count != 0 {
		t.Errorf("an unproven refusal was counted: %+v", st.MgmtRefused)
	}

	// The policy is back: the frozen id is reconciled and not sent again.
	cfg.Bridge.ExecutionPolicy = "policy.json"
	accepting, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg.Bridge.Binary = accepting
	now = now.Add(61 * time.Second)
	pumpRefusedRound(t, e, cfg)
	calls := deliverSendCallsOf(t, log)
	if len(calls) == 0 || calls[0]["tool"] != deliverToolOperation || deliverSendRequestIDOf(t, calls[0]) != want.LogicalID {
		t.Fatalf("the round after the policy came back called %v, want get_operation of %s first", deliverSendToolsOf(t, log), want.LogicalID)
	}
	for _, id := range pumpQueueTestSendIDs(t, log) {
		if id == want.LogicalID {
			t.Errorf("the frozen batch %s was sent again", id)
		}
	}
	if st := pumpTestReadState(t, cfg); !st.Sent["A"] {
		t.Errorf("the reconciled event A is not recorded as sent: %+v", st.Sent)
	}
}

// CRW-1273 (1, post-evaluation d1): the bridge's refusal is final, but the outbox record could not be
// written, so the ledger does not prove it. The attempt stays frozen, and the next round, a minute on,
// reconciles the frozen id and body (the body's HH:MM head does not move with the clock) instead of
// forming the same id with another body.
func TestPumpRefusalWithoutOutboxProofKeepsTheFrozenAttempt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root")
	}
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	cfg.Settings.Management = coreSettings{Model: "m1", ReasoningEffort: "low"}
	pumpTestSwapSources(t, &pumpTestSource{name: "fake", events: []pumpEvent{{ID: "e1", Kind: pumpKindQuestion, Text: "hello"}}})

	refusing, _ := pumpRefusedBridge(t)
	outbox := filepath.Join(cfg.StateDir, "outbox")
	wrapper := filepath.Join(t.TempDir(), "bridge")
	script := "#!/bin/sh\nchmod 500 " + coreShellQuote(outbox) + "\nexec " + coreShellQuote(refusing) + "\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outbox, 0o700) })
	cfg.Bridge.Binary = wrapper
	pumpRefusedRound(t, e, cfg)
	if err := os.Chmod(outbox, 0o700); err != nil {
		t.Fatal(err)
	}
	st := pumpTestReadState(t, cfg)
	if st.Attempt == nil {
		t.Fatalf("the refusal the ledger does not show cleared the attempt; pending %+v", st.Pending)
	}
	frozen := *st.Attempt

	settled, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"status": "refused", "error": "expected_settings is required"}},
	})
	cfg.Bridge.Binary = settled
	now = now.Add(61 * time.Second)
	pumpRefusedRound(t, e, cfg)
	calls := deliverSendCallsOf(t, log)
	if len(calls) != 1 || calls[0]["tool"] != deliverToolOperation || deliverSendRequestIDOf(t, calls[0]) != frozen.LogicalID {
		t.Fatalf("the next round called %v, want get_operation of %s alone", deliverSendToolsOf(t, log), frozen.LogicalID)
	}
	st = pumpTestReadState(t, cfg)
	if st.Attempt != nil || st.MgmtRefused.Count != 1 || len(st.Pending) != 1 {
		t.Fatalf("after the proof: attempt %+v refusals %+v pending %d, want the refusal counted once and the event pending", st.Attempt, st.MgmtRefused, len(st.Pending))
	}
}

// CRW-1273 (3, post-evaluation d2): a local refusal before any bridge call is logged with its own
// error, not with the outbox excerpt an earlier attempt of the same frozen batch left behind.
func TestPumpLogReasonIsTheCurrentLocalRefusal(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	cfg.Settings.Management = coreSettings{Model: "m1", ReasoningEffort: "low"}
	pumpTestSwapSources(t, &pumpTestSource{name: "fake", events: []pumpEvent{{ID: "e1", Kind: pumpKindQuestion, Text: "hello"}}})

	unknown, _ := pumpRefusedUnknownBridge(t)
	cfg.Bridge.Binary = unknown
	pumpRefusedRound(t, e, cfg)
	if line := pumpRefusedLastDeliverLine(t, cfg); !strings.Contains(line, "class=unknown") || !strings.Contains(line, "in_progress_or_unknown") {
		t.Fatalf("the first deliver line = %q, want the undetermined answer as the reason", line)
	}

	cfg.Bridge.ExecutionPolicy = ""
	now = now.Add(61 * time.Second)
	pumpRefusedRound(t, e, cfg)
	line := pumpRefusedLastDeliverLine(t, cfg)
	if !strings.Contains(line, "class=refused") || !strings.Contains(line, "bridge.execution_policy is not set") || strings.Contains(line, "in_progress_or_unknown") {
		t.Fatalf("the deliver line = %q, want the current local refusal and not the earlier excerpt", line)
	}
}

// CRW-1273 (1, post-evaluation d3): the missing-settings failure as the real bridge produces it. A
// send without expected_settings is a tool error (argument validation, before the ledger), which the
// delivery core reads as undetermined and reconciles with get_operation: the bridge holds no such
// request id, so the batch is sent again under a new request id, twice, and the next reconcile spends
// the retry cap and settles the refusal. The refusal is counted, and after the settings are fixed the
// batch goes out under the next ordinal and is delivered.
func TestPumpMissingSettingsToolErrorIsReconciledThenRecovered(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	pumpTestSwapSources(t, &pumpTestSource{name: "fake", events: []pumpEvent{{ID: "e1", Kind: pumpKindQuestion, Text: "hello"}}})
	sendError := map[string]any{"isError": true, "text": "Error: expected_settings is required"}
	unknownID := map[string]any{"isError": true, "text": "get_operation: " + deliverUnknownRequestID}
	idle := map[string]any{"payload": map[string]any{"observation": "idle"}}
	round := func(steps ...map[string]any) []map[string]any {
		bridge, log := deliverFakeBridge(t, steps)
		cfg.Bridge.Binary = bridge
		now = now.Add(61 * time.Second)
		pumpRefusedRound(t, e, cfg)
		return deliverSendCallsOf(t, log)
	}

	round(idle, sendError) // the first send: a tool error, undetermined, frozen
	st := pumpTestReadState(t, cfg)
	if st.Attempt == nil {
		t.Fatal("the undetermined send did not freeze the attempt")
	}
	base := st.Attempt.LogicalID
	round(unknownID, idle, sendError) // not recorded by the bridge: sent again under <id>-r1
	round(unknownID, idle, sendError) // and under <id>-r2
	calls := round(unknownID)         // the retry cap is spent: the refusal settles
	if len(calls) != 1 || calls[0]["tool"] != deliverToolOperation {
		t.Fatalf("the settling round called %v, want get_operation alone", calls)
	}
	st = pumpTestReadState(t, cfg)
	if st.Attempt != nil || st.MgmtRefused.ID != base || st.MgmtRefused.Count != 1 || len(st.Pending) != 1 {
		t.Fatalf("after the settled refusal: attempt %+v refusals %+v pending %d, want the batch %s counted once and pending", st.Attempt, st.MgmtRefused, len(st.Pending), base)
	}

	cfg.Settings.Management = coreSettings{Model: "m1", ReasoningEffort: "low"}
	calls = round(idle, map[string]any{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}})
	st = pumpTestReadState(t, cfg)
	if !st.Sent["e1"] || len(st.Pending) != 0 || st.Attempt != nil {
		t.Fatalf("after the fix: sent %v pending %d attempt %+v, want e1 delivered", st.Sent, len(st.Pending), st.Attempt)
	}
	if len(calls) != 2 || deliverSendRequestIDOf(t, calls[1]) != base+"-m1" {
		t.Fatalf("the delivering round called %v, want one send under %s-m1", calls, base)
	}
}
