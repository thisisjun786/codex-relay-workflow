package manage

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestPumpStrandedCloseCrashReplayPreservesRefusalBudget(t *testing.T) {
	h := newPumpLedgerHarness(t)
	pumpLedgerSetMode(t, h.dir, pumpLedgerDown)
	h.round()
	st := h.round()
	if st.MgmtRefused.Count != 2 {
		t.Fatalf("setup refusals: %+v", st.MgmtRefused)
	}
	pumpLedgerSetMode(t, h.dir, pumpLedgerUnknown)
	st = h.round()
	if st.Attempt == nil {
		t.Fatal("uncertain attempt was not frozen")
	}
	attempt := *st.Attempt
	base := pumpBatchID(st.Pending)
	oldThread := h.cfg.ManagementThread
	h.cfg.ManagementThread = "mgmt-thread-2"
	out, err := deliverReconcileStranded(context.Background(), h.e, h.cfg, Message{
		LogicalID: attempt.LogicalID, Thread: h.cfg.ManagementThread, Text: attempt.Text,
	})
	if err != nil || out.Receipt["status"] != deliverStranded {
		t.Fatalf("close before interruption: %+v err %v", out, err)
	}
	persisted := pumpTestReadState(t, h.cfg)
	if persisted.Attempt == nil || persisted.MgmtRefused != st.MgmtRefused {
		t.Fatalf("outbox settlement changed the pump state: %+v", persisted)
	}
	sendsBefore := len(mustSends(t, h.dir))
	st = h.round()
	if st.Attempt != nil || st.MgmtRefused.Count != 2 || st.MgmtRefused.Ordinal != 3 || len(st.Pending) != 1 || len(st.Parked) != 0 {
		t.Fatalf("crash replay spent the refusal budget: %+v", st)
	}
	if len(mustSends(t, h.dir)) != sendsBefore {
		t.Fatal("crash replay dispatched another message")
	}
	out, err = Deliver(context.Background(), h.e, h.cfg, Message{
		LogicalID: attempt.LogicalID, Thread: oldThread, Text: attempt.Text,
	})
	if err != nil || out.Receipt["status"] != deliverStranded || out.Receipt["thread"] != oldThread || out.Receipt["replayed"] != true {
		t.Fatalf("same-thread replay lost the close kind: %+v err %v", out, err)
	}
	pumpLedgerSetMode(t, h.dir, pumpLedgerUp)
	st = h.round()
	ids, threads := pumpLedgerSends(t, h.dir)
	if !st.Sent["e1"] || len(st.Pending) != 0 || len(st.Parked) != 0 || ids[len(ids)-1] != pumpMgmtOrdinalID(base, 3) || threads[len(threads)-1] != h.cfg.ManagementThread {
		t.Fatalf("replayed close did not recover on the new ordinal: ids %v threads %v state %+v", ids, threads, st)
	}
}

func TestPumpQueueExistingRetryCollisionPinRecovers(t *testing.T) {
	for _, scenario := range []string{"aged_idle", "active", "young_idle"} {
		t.Run(scenario, func(t *testing.T) {
			now := pumpTestNow
			e := pumpTestEnv(t, &now)
			bridge, dir := pumpLedgerBridge(t)
			cfg := pumpTestConfig(t, bridge)
			cfg.Settings.Parent = coreSettings{Model: "m1", ReasoningEffort: "low"}
			thread := "parent-1"
			name := "aaaaaaaaaaaaaaaa.txt"
			body := "a-body"
			path := pumpQueueTestNotice(t, cfg, thread, name, body)
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
			ids := mustSends(t, dir)
			if len(ids) != 3 {
				t.Fatalf("setup outage sends: %v", ids)
			}
			base := ids[0]
			cfg.Settings.Parent.ReasoningEffort = "medium"
			pin := pumpReview776QueuePin{
				LogicalID: deliverRetryRequestID(base, 1), Base: base, Names: []string{name}, Body: body,
				SHA256: pumpReview776QueueDigests([]string{name}, []string{body}),
			}
			out, err := Deliver(context.Background(), e, cfg, Message{
				LogicalID: pin.LogicalID, Thread: thread, Text: pin.Body, Role: "parent", Settings: cfg.Settings.Parent,
			})
			if err != nil || out.Class != deliverClassUnknown {
				t.Fatalf("setup fingerprint conflict: %+v err %v", out, err)
			}
			st := pumpTestReadStatePtr(t, cfg)
			st.QueueAttempt[thread] = pin
			st.QueueLegacyChecked[thread] = true
			if err := st.pumpSave(cfg); err != nil {
				t.Fatal(err)
			}
			if scenario == "active" {
				pumpLedgerSetObservation(t, dir, "active", "turn-1")
			}
			pumpLedgerSetMode(t, dir, pumpLedgerOperationError)
			before := mustSends(t, dir)
			flush()
			waiting := pumpTestReadState(t, cfg)
			if !reflect.DeepEqual(waiting.QueueAttempt[thread], pin) || waiting.QueueRefused[thread].Count != 1 || !reflect.DeepEqual(mustSends(t, dir), before) {
				t.Fatalf("uncertain lookup released or resent the pin: %+v", waiting)
			}
			pumpLedgerSetMode(t, dir, pumpLedgerUp)
			ledger := pumpLedgerLoad(dir)
			original := ledger[pin.LogicalID]
			uncertain := original
			uncertain.Receipt = map[string]any{"status": "in_progress_or_unknown", "retrySafe": false}
			ledger[pin.LogicalID] = uncertain
			pumpLedgerStore(dir, ledger)
			flush()
			waiting = pumpTestReadState(t, cfg)
			if !reflect.DeepEqual(waiting.QueueAttempt[thread], pin) || waiting.QueueRefused[thread].Count != 1 || !reflect.DeepEqual(mustSends(t, dir), before) {
				t.Fatalf("uncertain receipt released or resent the pin: %+v", waiting)
			}
			ledger[pin.LogicalID] = original
			pumpLedgerStore(dir, ledger)
			if scenario == "young_idle" {
				if err := os.Chtimes(path, now, now); err != nil {
					t.Fatal(err)
				}
				flush()
				if err := st.pumpSave(cfg); err != nil {
					t.Fatal(err)
				}
				flush()
				flush()
				waiting = pumpTestReadState(t, cfg)
				if len(waiting.QueueAttempt) != 0 || waiting.QueueRefused[thread].Count != 2 || !reflect.DeepEqual(mustSends(t, dir), before) || len(pumpQueueTestNames(t, cfg, thread)) != 1 {
					t.Fatalf("retired pin bypassed the idle gate or was counted twice: %+v", waiting)
				}
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
			}
			flush()
			st = pumpTestReadStatePtr(t, cfg)
			if len(pumpQueueTestNames(t, cfg, thread)) != 0 || len(st.QueueAttempt) != 0 {
				t.Fatalf("confirmed non-delivery did not release the old pin: state %+v sends %v", st, mustSends(t, dir))
			}
			if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpQueueDir, thread, pumpSentDir, name)); err != nil {
				t.Fatal(err)
			}
			ids = mustSends(t, dir)
			if len(ids) != len(before)+1 || ids[len(ids)-1] != pumpMgmtOrdinalID(base, 2) {
				t.Fatalf("recovery reused the collision id instead of the durable ordinal: %v", ids)
			}
			record, known, err := deliverLoad(cfg, pin.LogicalID)
			if err != nil || !known || record.State != deliverStateRefused {
				t.Fatalf("retired outbox identity: %+v known %v err %v", record, known, err)
			}
			flush()
			if !reflect.DeepEqual(mustSends(t, dir), ids) {
				t.Fatal("accepted notice was delivered twice")
			}
			later := pumpQueueTestNotice(t, cfg, thread, "bbbbbbbbbbbbbbbb.txt", "later-body")
			if err := os.Chtimes(later, old, old); err != nil {
				t.Fatal(err)
			}
			flush()
			if len(pumpQueueTestNames(t, cfg, thread)) != 0 || len(mustSends(t, dir)) != len(ids)+1 {
				t.Fatal("later notice did not drain after collision recovery")
			}
		})
	}
}
