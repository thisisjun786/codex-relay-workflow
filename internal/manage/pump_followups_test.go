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
	record, known, err := deliverLoad(cfg, st.Attempt.LogicalID)
	if err != nil || !known || record.State != "waiting" {
		t.Fatalf("idle race must wait without an uncertain dispatch: record %+v known %v err %v", record, known, err)
	}
	requestID := record.RequestID
	for round := 0; round < pumpQueueRefusalLimit+deliverRetryLimit; round++ {
		pumpRefusedRound(t, e, cfg)
		st = pumpTestReadState(t, cfg)
		record, _, err = deliverLoad(cfg, st.Attempt.LogicalID)
		if err != nil || record.State != "waiting" || record.RequestID != requestID || st.MgmtRefused.Count != 0 || len(st.Parked) != 0 {
			t.Fatalf("idle round %d consumed a retry or refusal: record %+v state %+v err %v", round, record, st, err)
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
	cfg.raw = map[string]json.RawMessage{"pump": json.RawMessage(`{"steer_only":true}`)}
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
