package delivery

import (
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// test_delivery.py properties DEL-31..DEL-35.

func TestDEL31_a_restart_keeps_every_durable_record_and_resends_nothing(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "del31")
	f := newFixture(t, tree)
	first := f.queuedEvent(regOpts{})
	firstRecord := f.mustAttempt(first, nil)
	payload := f.readyPayload(f.rid, 1, []string{f.artifact("second.txt", "still in flight")}, 2, assigned("completed"))
	_, err := f.accept(payload, store.AcceptOptions{})
	mustDo(t, err)
	second := str(payload, "eventId")
	_, err = f.delivery.Enqueue(f.ctx, second, "", "")
	mustDo(t, err)
	f.host.script = []string{"transport_unknown"}
	later := f.clock.Now() + 3600
	secondRecord := f.mustAttempt(second, at(later))
	requireSameJSON(t, "first", firstRecord, python.Out["first"])
	requireSameJSON(t, "second", secondRecord, python.Out["second"])
	_, err = execSQL(f.ctx, f.store, "INSERT INTO sync_targets (relationship_id, target, target_ref, recorded_at) VALUES (?,?,?,?) ON CONFLICT(relationship_id, target) DO UPDATE SET target_ref = excluded.target_ref, recorded_at = excluded.recorded_at", f.rid, "coordination_document", "DOC-1", f.clock.ISO())
	mustDo(t, err)
	before := f.tables()
	sends := len(f.host.sends)

	mustDo(t, f.store.Close())
	reopened, err := store.Open(f.ctx, filepath.Join(tree, "gostate", "relay.sqlite3"), "")
	mustDo(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	f.store = reopened
	f.delivery = NewService(reopened, f.clock)
	requireSameJSON(t, "durable records across the restart", f.tables(), before)

	recovered, err := NewReconciler(f.delivery).RecoverOnStart(f.ctx, f.host, nil)
	mustDo(t, err)
	requireSameJSON(t, "recovered", recovered, python.Out["recovered"])
	if len(f.host.sends) != sends {
		t.Fatal("recovery sent")
	}
	awaiting, _ := get(recovered, "awaitingAck")
	if len(awaiting.([]any)) != 1 || awaiting.([]any)[0] != first {
		t.Fatalf("awaitingAck %v", awaiting)
	}
	attempts := f.row(first).I("attempt_count")
	if again := f.mustAttempt(first, at(later+3600)); again != nil || f.row(first).I("attempt_count") != attempts || len(f.host.sends) != sends {
		t.Fatal("a dispatched event replays nothing")
	}
	requireSameTables(t, f, python)
}

func TestDEL32_a_stray_declaration_on_a_completion_is_not_labelled(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "del32")
	f := newFixture(t, tree)
	event := f.queuedEvent(regOpts{})
	receipt, err := f.delivery.Receipt(f.ctx, event)
	mustDo(t, err)
	receipt = set(receipt, "criteria", []any{Obj{{Key: "id", Value: "c1"}, {Key: "verdict", Value: "verified"}, {Key: "restoration", Value: "false"}}, Obj{{Key: "id", Value: "c2"}, {Key: "verdict", Value: "verified"}, {Key: "restoration", Value: true}}})
	_, err = execSQL(f.ctx, f.store, "UPDATE events SET receipt = ? WHERE event_id = ?", dumps(receipt), event)
	mustDo(t, err)
	message, err := f.delivery.PreviewMessage(f.ctx, event)
	mustDo(t, err)
	if message != python.Out["message"] || !strings.Contains(message, "  c1: verified") || strings.Contains(message, "[restoration block]") {
		t.Fatalf("message:\n%s", message)
	}
}

func TestDEL33_a_claim_refused_on_the_shared_gap_is_rescheduled_not_failed(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "del33")
	f := newFixture(t, tree)
	event := f.queuedEvent(regOpts{})
	now := f.clock.Now()
	_, err := execSQL(f.ctx, f.store, "INSERT INTO recipient_rate (recipient_task_id, window_start, sends, last_send_at) VALUES (?,?,1,?)", parent, math.Floor(now/3600)*3600, now)
	mustDo(t, err)
	f.delivery.RateLimited = func(string, float64) bool { return false }
	if first := f.mustAttempt(event, at(now+1)); first != nil {
		t.Fatal("paced")
	}
	row := f.row(event)
	requireSameJSON(t, "row", row, python.Out["row"])
	if row.S("state") != Queued || !row.N("hold_reason") || row.I("attempt_count") != 0 || row.F("next_eligible_at") != now+1+f.delivery.Policy.MinSendInterval || f.count("SELECT COUNT(*) AS c FROM failed_operations") != 0 || len(f.host.sends) != 0 {
		t.Fatalf("rolled back and rescheduled by the gap: %v", row)
	}
	f.delivery.RateLimited = nil
	record := f.mustAttempt(event, at(row.F("next_eligible_at")))
	requireSameJSON(t, "record", record, python.Out["record"])
	requireSameTables(t, f, python)
}

func TestDEL34_exec_source_recipients_deliver_or_withhold_with_their_relationship(t *testing.T) {
	// The exec-aware archive check itself (bridge_adapter.is_archived over thread/list) is the
	// bridge adapter's, todo 28; here the host answers the archive question it would answer,
	// and discovery_cursors, which only that adapter writes, is left out of the comparison.
	for _, mode := range []string{"live", "archived", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			tree := t.TempDir()
			python := runPython(t, tree, "del34", mode)
			f := newFixture(t, tree)
			_, correction := f.correctionAfterNeedsChanges()
			archived := mode != "live"
			f.host.onArchived = func(thread string) (*bool, error) {
				if thread == child {
					return boolp(archived), nil
				}
				return boolp(false), nil
			}
			if mode == "legacy" {
				_, err := execSQL(f.ctx, f.store, "INSERT INTO failed_operations (scope_key, operation, detail, error_code, occurred_at) VALUES (?, 'lifecycle_read', 'lifecycle_unknown', 'lifecycle_unknown', ?)", correction, f.clock.ISO())
				mustDo(t, err)
				f.clock.Advance(1)
			}
			record := f.mustAttempt(correction, nil)
			requireSameJSON(t, "record", record, python.Out["record"])
			failure := f.one("SELECT * FROM failed_operations WHERE scope_key = ? AND operation = 'lifecycle_read'", correction)
			if mode == "live" {
				if str(record, "deliveryState") != Dispatched || failure != nil || f.one("SELECT archived FROM recipient_lifecycle WHERE task_id = ?", child).I("archived") != 0 {
					t.Fatal("a live exec child is delivered")
				}
			} else if f.row(correction).S("state") != WithheldPreSend || failure.S("error_code") != RecipientArchived || failure.S("relationship_id") != f.rid || failure.S("parent_task_id") != parent {
				t.Fatalf("withheld with its relationship recorded: %v", failure)
			}
			requireSameTablesExcept(t, f, python, "discovery_cursors")
		})
	}
}

func TestDEL35_a_withhold_records_its_failure_in_its_own_transition(t *testing.T) {
	for _, mode := range []string{"lifecycle", "settings", "busy"} {
		t.Run(mode, func(t *testing.T) {
			tree := t.TempDir()
			python := runPython(t, tree, "del35", mode)
			f := newFixture(t, tree)
			event := f.queuedEvent(regOpts{noSettings: mode == "settings"})
			switch mode {
			case "lifecycle":
				f.host.readFailures["is_archived"] = true
			case "busy":
				f.host.threads[parent].status = "active"
			}
			f.host.onGoalRead = func(string) {
				_, err := execSQL(f.ctx, f.store, "UPDATE deliveries SET state = 'sending', attempt_count = attempt_count + 1 WHERE event_id = ?", event)
				mustDo(t, err)
			}
			if record := f.mustAttempt(event, nil); record != nil {
				t.Fatal("None")
			}
			operation := map[string]string{"lifecycle": "lifecycle_read", "settings": "settings_check", "busy": "parent_busy"}[mode]
			if f.row(event).S("state") != Sending || f.one("SELECT * FROM failed_operations WHERE scope_key = ? AND operation = ?", event, operation) != nil {
				t.Fatal("an overtaken withhold records nothing")
			}
			requireSameTables(t, f, python)
		})
	}
	t.Run("one stamp", func(t *testing.T) {
		tree := t.TempDir()
		python := runPython(t, tree, "del35", "stamp")
		f := newFixture(t, tree)
		event := f.queuedEvent(regOpts{})
		f.host.threads[parent].archived = boolp(true)
		f.clock.OnISO = func() { f.clock.T += 0.000001 }
		if record := f.mustAttempt(event, nil); record != nil {
			t.Fatal("None")
		}
		f.clock.OnISO = nil
		if f.one("SELECT occurred_at FROM failed_operations WHERE scope_key = ? AND operation = 'lifecycle_read'", event).S("occurred_at") != f.row(event).S("updated_at") {
			t.Fatal("one stamp")
		}
		requireSameTables(t, f, python)
	})
}
