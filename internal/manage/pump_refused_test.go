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
// goes out under <id>-r1 (the rule of the parent queue, CRW-991) and is delivered, carrying the
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
	if len(ids) != 1 || ids[0] != first[0]+"-r1" {
		t.Fatalf("the second attempt used %v, want %s-r1", ids, first[0])
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
	if ids := pumpQueueTestSendIDs(t, acceptedLog); len(ids) != 1 || !strings.HasSuffix(ids[0], "-r1") {
		t.Fatalf("the second attempt used %v, want the -r1 ordinal", ids)
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
	if len(ids) != 3 || ids[1] != base+"-r1" || ids[2] != base+"-r2" {
		t.Fatalf("the sends used %v, want %s then -r1 then -r2", ids, base)
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
