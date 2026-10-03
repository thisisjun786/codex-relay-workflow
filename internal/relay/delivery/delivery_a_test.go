package delivery

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// test_delivery.py properties DEL-1..DEL-10. Each Go run uses the fixture tree the real Python run
// of the same scenario used, so paths, hashes and ids agree with the goldens, which began as what
// Python wrote, and the whole store, the returned records and the refusals are checked against
// them (harness_test.go scenario).

func TestDEL01_an_accepted_final_event_is_queued_idempotently_and_eligible(t *testing.T) {
	tree := parityTree(t)
	expected := expectScenario(t, tree, "del01")
	f := newFixture(t, tree)
	event := f.queuedEvent(regOpts{})
	first := f.row(event)
	again, err := f.delivery.Enqueue(f.ctx, event, "", "")
	mustDo(t, err)
	expected.same("first", first)
	expected.same("again", again)
	expected.same("eligible", f.eligible())
	if f.count("SELECT COUNT(*) AS c FROM deliveries") != 1 || first.S("state") != Queued || first.S("recipient_task_id") != parent {
		t.Fatalf("one queued delivery to the parent: %v", first)
	}
	expected.tables(f)
}

func TestDEL02_a_staged_event_cannot_be_queued(t *testing.T) {
	tree := parityTree(t)
	expected := expectScenario(t, tree, "del02")
	f := newFixture(t, tree)
	rid := f.register(regOpts{})
	payload := f.readyPayload(rid, 1, []string{f.artifact("out.txt", "still working")}, 1, assigned("inProgress"))
	_, err := f.accept(payload, store.AcceptOptions{})
	mustDo(t, err)
	_, err = f.delivery.Enqueue(f.ctx, pyjson.Text(payload.Get("eventId")), "", "")
	requireReason(t, err, NotClaimable)
	expected.same("refused", refusalOf(err))
	expected.tables(f)
}

func TestDEL03_a_recipient_outside_the_authorized_scope_is_refused_before_any_transport(t *testing.T) {
	t.Run("out-of-scope recipient at enqueue", func(t *testing.T) {
		tree := parityTree(t)
		expected := expectScenario(t, tree, "del03", "scope")
		f := newFixture(t, tree)
		event := f.readyEvent(regOpts{})
		_, err := f.delivery.Enqueue(f.ctx, event, "", "somebody-else")
		requireReason(t, err, RecipientNotAuthorized)
		expected.same("refused", refusalOf(err))
		if len(f.host.sends) != 0 {
			t.Fatal("the transport was called")
		}
		expected.tables(f)
	})
	t.Run("another assignment's parent at enqueue", func(t *testing.T) {
		tree := parityTree(t)
		expected := expectScenario(t, tree, "del03", "other")
		f := newFixture(t, tree)
		f.otherAssignment()
		event := f.readyEvent(regOpts{recipients: []string{parent, "01other-parent"}})
		_, err := f.delivery.Enqueue(f.ctx, event, "", "01other-parent")
		requireReason(t, err, RecipientNotAuthorized)
		if !strings.Contains(Detail(err), "its own parent") {
			t.Fatalf("detail %q", Detail(err))
		}
		expected.same("refused", refusalOf(err))
		row, _ := f.delivery.Find(f.ctx, event)
		expected.same("row", row)
		if row != nil {
			t.Fatalf("a row was created: %v", row)
		}
	})
	t.Run("tampered row at attempt", func(t *testing.T) {
		tree := parityTree(t)
		expected := expectScenario(t, tree, "del03", "tampered")
		f := newFixture(t, tree)
		f.otherAssignment()
		event := f.queuedEvent(regOpts{recipients: []string{parent, "01other-parent"}})
		f.host.addThread("01other-parent")
		_, err := execSQL(f.ctx, f.store, "UPDATE deliveries SET recipient_task_id = ?, recipient_thread_id = ? WHERE event_id = ?", "01other-parent", "01other-parent", event)
		mustDo(t, err)
		_, err = f.attempt(event, nil)
		requireReason(t, err, RecipientNotAuthorized)
		expected.same("refused", refusalOf(err))
		expected.sends(f.host)
		if len(f.host.sends) != 0 {
			t.Fatal("nothing may reach the host")
		}
	})
	t.Run("own parent control", func(t *testing.T) {
		tree := parityTree(t)
		expected := expectScenario(t, tree, "del03", "own")
		f := newFixture(t, tree)
		f.otherAssignment()
		event := f.queuedEvent(regOpts{})
		record := f.mustAttempt(event, nil)
		expected.same("record", record)
		if pyjson.Text(record.Get("deliveryState")) != Dispatched || f.row(event).S("recipient_task_id") != parent {
			t.Fatalf("record %v", record)
		}
	})
}

func TestDEL04_the_message_carries_the_event_never_a_recipient_turn_or_override(t *testing.T) {
	tree := parityTree(t)
	expected := expectScenario(t, tree, "del04")
	f := newFixture(t, tree)
	event := f.queuedEvent(regOpts{})
	preview, err := f.delivery.PreviewMessage(f.ctx, event)
	mustDo(t, err)
	again, _ := f.delivery.PreviewMessage(f.ctx, event)
	expected.same("preview", preview)
	if preview != again {
		t.Fatalf("the preview changed:\n%s\n--\n%v", preview, again)
	}
	if !strings.Contains(preview, event) || strings.Contains(strings.ReplaceAll(preview, "your own turn id", ""), "turn-") {
		t.Fatal("the message must carry the event and no recipient turn id")
	}
	record := f.mustAttempt(event, nil)
	expected.same("record", record)
	expected.sends(f.host)
	sent := f.host.sends[0]
	if sent.thread != parent {
		t.Fatalf("sent to %s", sent.thread)
	}
	for _, forbidden := range []string{"model", "effort", "sandbox", "approvalPolicy", "reasoning"} {
		if strings.Contains(sent.message, forbidden) {
			t.Fatalf("message carries %q", forbidden)
		}
	}
	expected.tables(f)
}

func TestDEL05_an_active_recipient_is_deferred_without_an_attempt_or_interruption(t *testing.T) {
	tree := parityTree(t)
	expected := expectScenario(t, tree, "del05")
	f := newFixture(t, tree)
	event := f.queuedEvent(regOpts{})
	turn := f.host.startTurn(parent, "", "inProgress", "")
	f.host.threads[parent].status = "active"
	record := f.mustAttempt(event, nil)
	expected.same("record", record)
	if record != nil {
		t.Fatalf("a busy recipient returns None: %v", record)
	}
	still, _ := f.host.ReadTurn(context.Background(), parent, turn.TurnID)
	expected.same("turn", still.Status)
	if still.Status != "inProgress" {
		t.Fatal("the running turn was interrupted")
	}
	if f.row(event).S("state") != DeferredBusy || f.count("SELECT COUNT(*) AS c FROM attempts") != 0 || len(f.host.sends) != 0 {
		t.Fatal("deferred_busy with no attempt and no send")
	}
	expected.tables(f)
}

func TestDEL06_a_transport_busy_refusal_produces_a_real_deferred_attempt(t *testing.T) {
	tree := parityTree(t)
	expected := expectScenario(t, tree, "del06")
	f := newFixture(t, tree)
	event := f.queuedEvent(regOpts{})
	f.host.script = []string{"busy"}
	record := f.mustAttempt(event, nil)
	expected.same("record", record)
	if pyjson.Text(record.Get("deliveryState")) != DeferredBusy || pyjson.Text(record.Get("sendAttempted")) != "no" || pyjson.Text(record.Get("failedOperation")) != "thread/read" {
		t.Fatalf("record %v", record)
	}
	if v, _ := record.Lookup("retrySafe"); v != true {
		t.Fatal("retrySafe")
	}
	expected.tables(f)
}

func TestDEL07_an_idle_recipient_gets_a_real_turn_and_a_steer_is_recorded(t *testing.T) {
	t.Run("idle -> fresh turn", func(t *testing.T) {
		tree := parityTree(t)
		expected := expectScenario(t, tree, "del07")
		f := newFixture(t, tree)
		event := f.queuedEvent(regOpts{})
		record := f.mustAttempt(event, nil)
		expected.same("record", record)
		if pyjson.Text(record.Get("deliveryState")) != Dispatched || pyjson.Text(record.Get("turnId")) == "" || f.row(event).S("state") != Dispatched {
			t.Fatalf("record %v", record)
		}
		expected.tables(f)
	})
	t.Run("running turn -> steered_observed_turn", func(t *testing.T) {
		tree := parityTree(t)
		expected := expectScenario(t, tree, "del07", "steer")
		f := newFixture(t, tree)
		event := f.queuedEvent(regOpts{})
		f.host.startTurn(parent, "already-running", "inProgress", "")
		f.host.script = []string{"steer_existing"}
		record := f.mustAttempt(event, nil)
		expected.same("record", record)
		if pyjson.Text(record.Get("turnId")) != "already-running" || pyjson.Text(record.Get("_turnOrigin")) != "steered_observed_turn" {
			t.Fatalf("record %v", record)
		}
		if v, _ := record.Lookup("_turnPreviouslyObserved"); v != true {
			t.Fatal("_turnPreviouslyObserved")
		}
		expected.tables(f)
	})
}

func TestDEL08_dispatched_is_not_delivered(t *testing.T) {
	t.Parallel()
	tree := t.TempDir()
	expected := expectScenario(t, tree, "del08")
	f := newFixture(t, tree)
	event := f.queuedEvent(regOpts{})
	f.mustAttempt(event, nil)
	item, err := f.delivery.SnapshotItem(f.ctx, event)
	mustDo(t, err)
	got := Obj{}
	for _, k := range []string{"state", "reported", "acknowledged", "phase"} {
		v, _ := item.Lookup(k)
		got = append(got, F{Key: k, Value: v})
	}
	expected.same("snapshot", got)
	if pyjson.Text(item.Get("reported")) != "dispatched_awaiting_ack" {
		t.Fatalf("snapshot %v", item)
	}
}

func TestDEL09_request_id_is_distinct_from_the_event_id(t *testing.T) {
	tree := parityTree(t)
	expected := expectScenario(t, tree, "del07")
	f := newFixture(t, tree)
	event := f.queuedEvent(regOpts{})
	record := f.mustAttempt(event, nil)
	request := pyjson.Text(record.Get("requestId"))
	expected.same("record.requestId", request)
	if request == event || !strings.HasPrefix(request, "del-") || !strings.Contains(request, event[:12]) {
		t.Fatalf("request id %q", request)
	}
}

func TestDEL10_an_unsupported_approval_policy_is_stored_not_woken_and_held(t *testing.T) {
	for _, policy := range []string{"on-request", "untrusted"} {
		t.Run(policy, func(t *testing.T) {
			tree := parityTree(t)
			expected := expectScenario(t, tree, "del10", policy)
			f := newFixture(t, tree)
			event := f.queuedEvent(regOpts{})
			f.host.threads[parent].approvalPolicy = policy
			f.host.script = []string{"approval_policy"}
			record := f.mustAttempt(event, nil)
			expected.same("record", record)
			if pyjson.Text(record.Get("deliveryState")) != InboxOnly || pyjson.Text(record.Get("recipientApprovalPolicy")) != policy {
				t.Fatalf("record %v", record)
			}
			item, err := f.delivery.SnapshotItem(f.ctx, event)
			mustDo(t, err)
			got := Obj{}
			for _, k := range []string{"state", "reported", "holdReason", "phase"} {
				v, _ := item.Lookup(k)
				got = append(got, F{Key: k, Value: v})
			}
			expected.same("snapshot", got)
			f.clock.Advance(100000)
			if len(f.eligible()) != 0 {
				t.Fatal("an inbox-only delivery is never retried")
			}
			expected.tables(f)
		})
	}
}
