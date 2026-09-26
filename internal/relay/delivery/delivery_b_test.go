package delivery

import (
	"math"
	"strings"
	"testing"
)

// test_delivery.py properties DEL-11..DEL-20.

func TestDEL11_each_retry_opens_a_new_attempt_and_never_replays_the_first_request(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "del11")
	f := newFixture(t, tree)
	event := f.queuedEvent(regOpts{})
	f.host.script = []string{"busy"}
	first := f.mustAttempt(event, nil)
	f.clock.Advance(3600)
	second := f.mustAttempt(event, at(f.clock.Now()))
	requireSameJSON(t, "first", first, python.Out["first"])
	requireSameJSON(t, "second", second, python.Out["second"])
	if str(first, "requestId") == str(second, "requestId") || str(second, "deliveryState") != Dispatched {
		t.Fatalf("records %v %v", first, second)
	}
	replayed := 0
	for _, s := range f.host.sends {
		if s.requestID == str(first, "requestId") {
			replayed++
		}
	}
	if replayed != 1 {
		t.Fatalf("the first request id was sent %d times", replayed)
	}
	requireSameTables(t, f, python)
}

func TestDEL12_a_dispatched_or_uncertain_delivery_is_never_claimed_again(t *testing.T) {
	t.Run("dispatched", func(t *testing.T) {
		tree := t.TempDir()
		python := runPython(t, tree, "del12", "dispatched")
		f := newFixture(t, tree)
		event := f.queuedEvent(regOpts{})
		f.mustAttempt(event, nil)
		f.clock.Advance(100000)
		if again := f.mustAttempt(event, at(f.clock.Now())); again != nil || python.Out["again"] != nil {
			t.Fatalf("claimed again: %v", again)
		}
		requireSameTables(t, f, python)
	})
	t.Run("held_uncertain after 5 clock advances", func(t *testing.T) {
		tree := t.TempDir()
		python := runPython(t, tree, "del12", "uncertain")
		f := newFixture(t, tree)
		event := f.queuedEvent(regOpts{})
		f.host.script = []string{"turn_start_fail"}
		first := f.mustAttempt(event, nil)
		requireSameJSON(t, "first", first, python.Out["first"])
		for i := 0; i < 5; i++ {
			f.clock.Advance(86400)
			if len(f.eligible()) != 0 || f.mustAttempt(event, at(f.clock.Now())) != nil {
				t.Fatal("an uncertain attempt was retried by elapsed time")
			}
		}
		if f.row(event).S("state") != HeldUncertain || f.count("SELECT COUNT(*) AS c FROM attempts") != 1 {
			t.Fatal("one attempt, still held_uncertain")
		}
		requireSameTables(t, f, python)
	})
}

func TestDEL13_flood_bounds_cap_attempts_and_pace_sends(t *testing.T) {
	t.Run("direct pre-send failures -> attempt_cap", func(t *testing.T) {
		tree := t.TempDir()
		python := runPython(t, tree, "del13", "direct")
		f := newFixture(t, tree)
		event := f.queuedEvent(regOpts{})
		var records []any
		for i := int64(0); i < f.delivery.Policy.MaxAttempts; i++ {
			f.host.script = []string{"read_fail"}
			f.clock.Advance(100000)
			records = append(records, f.mustAttempt(event, at(f.clock.Now())))
		}
		requireSameJSON(t, "records", records, python.Out["records"])
		if f.row(event).S("hold_reason") != AttemptCap {
			t.Fatal("attempt_cap")
		}
		f.clock.Advance(100000)
		if len(f.eligible()) != 0 {
			t.Fatal("a capped delivery is not eligible")
		}
		requireSameTables(t, f, python)
	})
	t.Run("reconciled pre-send failures -> attempt_cap", func(t *testing.T) {
		tree := t.TempDir()
		python := runPython(t, tree, "del13", "reconciled")
		f := newFixture(t, tree)
		event := f.queuedEvent(regOpts{})
		rc := NewReconciler(f.delivery)
		var outcomes []any
		for i := int64(0); i < f.delivery.Policy.MaxAttempts; i++ {
			f.host.script = []string{"in_progress"}
			f.clock.Advance(100000)
			record := f.mustAttempt(event, at(f.clock.Now()))
			if record == nil {
				break
			}
			request := str(record, "requestId")
			f.host.ledger[request] = Obj{{Key: "requestId", Value: request}, {Key: "status", Value: "failed"}, {Key: "error", Value: "thread/read: refused"}, {Key: "rpcError", Value: Obj{{Key: "code", Value: "internal"}, {Key: "message", Value: "refused"}}}}
			outcome, err := rc.ReconcileAttempt(f.ctx, request, f.host, at(f.clock.Now()))
			mustDo(t, err)
			outcomes = append(outcomes, outcome)
		}
		requireSameJSON(t, "outcomes", outcomes, python.Out["outcomes"])
		if f.row(event).S("hold_reason") != AttemptCap || f.count("SELECT COUNT(*) AS c FROM attempts") > f.delivery.Policy.MaxAttempts {
			t.Fatal("the reconciled path obeys the cap")
		}
		requireSameTables(t, f, python)
	})
	t.Run("min interval", func(t *testing.T) {
		tree := t.TempDir()
		python := runPython(t, tree, "del13", "interval")
		f := newFixture(t, tree)
		event := f.queuedEvent(regOpts{})
		f.host.script = []string{"read_fail"}
		f.mustAttempt(event, nil)
		mustDo(t, f.delivery.Reschedule(f.ctx, event, WithheldPreSend, f.clock.Now(), f.row(event).I("attempt_count")))
		if again := f.mustAttempt(event, at(f.clock.Now()+1)); again != nil || python.Out["again"] != nil {
			t.Fatal("the minimum interval refuses the send")
		}
		requireSameTables(t, f, python)
	})
	t.Run("hourly cap", func(t *testing.T) {
		tree := t.TempDir()
		python := runPython(t, tree, "del13", "hourly")
		f := newFixture(t, tree)
		event := f.queuedEvent(regOpts{})
		now := f.clock.Now()
		_, err := execSQL(f.ctx, f.store, "INSERT INTO recipient_rate (recipient_task_id, window_start, sends, last_send_at) VALUES (?,?,?,?)", parent, math.Floor(now/3600)*3600, f.delivery.Policy.MaxSendsPerRecipientPerHour, 0)
		mustDo(t, err)
		if again := f.mustAttempt(event, at(now)); again != nil || len(f.host.sends) != 0 {
			t.Fatal("the hourly cap refuses the send")
		}
		requireSameTables(t, f, python)
	})
}

func lifecycleRow(f *fixture) Row {
	return f.one("SELECT * FROM recipient_lifecycle WHERE task_id = ?", parent)
}

func TestDEL14_host_lifecycle_withholds_are_deferrals_not_holds(t *testing.T) {
	for _, tc := range []struct {
		mode, reason string
		apply        func(*fakeThread, *fakeHost)
	}{
		{"archived", RecipientArchived, func(th *fakeThread, _ *fakeHost) { th.archived = boolp(true) }},
		{"paused", RecipientPaused, func(th *fakeThread, _ *fakeHost) { th.goalStatus = "paused" }},
		{"budget", RecipientBudgetLimited, func(th *fakeThread, _ *fakeHost) { th.goalStatus = "budgetLimited" }},
		{"noinput", RecipientCannotAccept, func(th *fakeThread, _ *fakeHost) { th.canAcceptInput = false }},
		{"idle", RecipientArchived, func(th *fakeThread, _ *fakeHost) { th.archived = boolp(true); th.status = "idle" }},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			tree := t.TempDir()
			python := runPython(t, tree, "del14", tc.mode)
			f := newFixture(t, tree)
			event := f.queuedEvent(regOpts{})
			tc.apply(f.host.threads[parent], f.host)
			record := f.mustAttempt(event, nil)
			if record != nil || python.Out["record"] != nil || python.Out["status"] != "active" {
				t.Fatalf("withheld returns None: %v", record)
			}
			row := f.row(event)
			if !row.N("hold_reason") || row.N("next_eligible_at") || lifecycleRow(f).S("withhold_reason") != tc.reason || len(f.host.sends) != 0 {
				t.Fatalf("deferral: %v", row)
			}
			requireSameTables(t, f, python)
		})
	}
}

func TestDEL15_an_unreadable_lifecycle_withholds_rather_than_guessing(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "del14", "unreadable")
	f := newFixture(t, tree)
	event := f.queuedEvent(regOpts{})
	f.host.readFailures["read_goal_status"] = true
	if record := f.mustAttempt(event, nil); record != nil {
		t.Fatal("withheld")
	}
	observed := lifecycleRow(f)
	if observed.S("deliverable") != "unknown" || observed.S("withhold_reason") != LifecycleUnknown || len(f.host.sends) != 0 {
		t.Fatalf("observed %v", observed)
	}
	requireSameTables(t, f, python)
}

func TestDEL16_a_later_good_observation_releases_the_withheld_delivery(t *testing.T) {
	for _, mode := range []string{"paused", "archived"} {
		t.Run(mode, func(t *testing.T) {
			tree := t.TempDir()
			python := runPython(t, tree, "del16", mode)
			f := newFixture(t, tree)
			event := f.queuedEvent(regOpts{})
			th := f.host.threads[parent]
			if mode == "paused" {
				th.goalStatus = "paused"
				f.mustAttempt(event, nil)
				th.goalStatus = "active"
			} else {
				th.archived = boolp(true)
				f.mustAttempt(event, nil)
				th.archived = boolp(false)
			}
			f.clock.Advance(f.delivery.Policy.LifecycleRecheck + 1)
			requireSameJSON(t, "eligible", f.eligible(), python.Out["eligible"])
			record := f.mustAttempt(event, at(f.clock.Now()))
			requireSameJSON(t, "record", record, python.Out["record"])
			if str(record, "deliveryState") != Dispatched {
				t.Fatal("released and dispatched")
			}
			requireSameTables(t, f, python)
		})
	}
}

func TestDEL17_a_deactivation_between_precheck_and_claim_blocks_the_send(t *testing.T) {
	for _, mode := range []string{"paused", "archived", "supersede", "before"} {
		t.Run(mode, func(t *testing.T) {
			tree := t.TempDir()
			python := runPython(t, tree, "del17", mode)
			f := newFixture(t, tree)
			event := f.queuedEvent(regOpts{})
			if mode == "before" {
				f.setStatus("paused")
				if len(f.eligible()) != 0 {
					t.Fatal("a paused relationship is never eligible")
				}
				requireSameTables(t, f, python)
				return
			}
			f.host.onGoalRead = func(string) {
				f.host.onGoalRead = nil
				if mode == "supersede" {
					f.supersede("rel-bbbbbbbbbbbbbbbb")
				} else {
					f.setStatus(mode)
				}
			}
			if record := f.mustAttempt(event, nil); record != nil || python.Out["record"] != nil {
				t.Fatalf("record %v", record)
			}
			if len(f.host.sends) != 0 || f.count("SELECT COUNT(*) AS c FROM attempts") != 0 {
				t.Fatal("no send and no attempt")
			}
			requireSameTables(t, f, python)
		})
	}
}

func TestDEL18_a_deactivated_assignment_is_withheld_with_a_returned_record(t *testing.T) {
	for _, status := range []string{"cancelled", "paused", "archived"} {
		t.Run(status, func(t *testing.T) {
			tree := t.TempDir()
			python := runPython(t, tree, "del18", status)
			f := newFixture(t, tree)
			event := f.queuedEvent(regOpts{})
			f.setStatus(status)
			record := f.mustAttempt(event, nil)
			requireSameJSON(t, "record", record, python.Out["record"])
			if str(record, "withheldReason") != RelationshipNotActive || str(record, "relationshipStatus") != status || len(f.host.sends) != 0 {
				t.Fatalf("record %v", record)
			}
			row := f.row(event)
			if row.S("state") != WithheldPreSend || !row.N("hold_reason") || f.count("SELECT COUNT(*) AS c FROM attempts") != 0 {
				t.Fatal("withheld, no permanent hold, no attempt")
			}
			entry := f.one("SELECT * FROM journal WHERE kind = ? AND subject = ?", "delivery_withheld_inactive", event)
			if entry == nil || !strings.Contains(entry.S("detail"), status) {
				t.Fatal("the deactivation is journalled with its status")
			}
			requireSameTables(t, f, python)
		})
	}
}

func TestDEL19_a_stopped_assignment_reads_nothing_from_the_host(t *testing.T) {
	f := newFixture(t, "")
	event := f.queuedEvent(regOpts{})
	f.setStatus("cancelled")
	stopped := &counted{fakeHost: f.host}
	_, err := f.delivery.Attempt(f.ctx, event, stopped, at(f.clock.Now()), "")
	mustDo(t, err)
	if len(stopped.calls) != 0 || lifecycleRow(f) != nil {
		t.Fatalf("the host was read for a stopped assignment: %v", stopped.calls)
	}
	g := newFixture(t, "")
	active := g.queuedEvent(regOpts{})
	control := &counted{fakeHost: g.host}
	_, err = g.delivery.Attempt(g.ctx, active, control, at(g.clock.Now()), "")
	mustDo(t, err)
	if !strings.Contains(strings.Join(control.calls, ","), "read_thread") {
		t.Fatal("the control must see the reads an active assignment makes")
	}
}

func TestDEL20_resuming_delivers_the_same_event_once_and_an_active_one_is_untouched(t *testing.T) {
	t.Run("resume", func(t *testing.T) {
		tree := t.TempDir()
		python := runPython(t, tree, "del20", "resume")
		f := newFixture(t, tree)
		event := f.queuedEvent(regOpts{})
		f.setStatus("paused")
		f.mustAttempt(event, nil)
		f.resume()
		f.clock.Advance(f.delivery.Policy.LifecycleRecheck + 1)
		record := f.mustAttempt(event, at(f.clock.Now()))
		requireSameJSON(t, "record", record, python.Out["record"])
		if str(record, "deliveryState") != Dispatched || len(f.host.sends) != 1 {
			t.Fatal("delivered once")
		}
		requireSameTables(t, f, python)
	})
	t.Run("active", func(t *testing.T) {
		tree := t.TempDir()
		python := runPython(t, tree, "del20", "active")
		f := newFixture(t, tree)
		event := f.queuedEvent(regOpts{})
		record := f.mustAttempt(event, nil)
		requireSameJSON(t, "record", record, python.Out["record"])
		requireSameTables(t, f, python)
	})
}
