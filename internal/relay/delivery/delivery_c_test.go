package delivery

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// test_delivery.py properties DEL-21..DEL-30.

func TestDEL21_a_deactivation_is_reported_during_a_backoff_and_never_shortens_it(t *testing.T) {
	t.Run("reported while a busy backoff runs, never shortened", func(t *testing.T) {
		tree := t.TempDir()
		python := runPython(t, tree, "del21", "running")
		f := newFixture(t, tree)
		event := f.queuedEvent(regOpts{})
		f.host.threads[parent].status = "active"
		if first := f.mustAttempt(event, nil); first != nil {
			t.Fatal("deferred busy")
		}
		deferred := f.row(event).F("next_eligible_at")
		f.setStatus("cancelled")
		record := f.mustAttempt(event, nil)
		requireSameJSON(t, "record", record, python.Out["record"])
		if str(record, "withheldReason") != RelationshipNotActive || f.row(event).F("next_eligible_at") < deferred {
			t.Fatalf("record %v", record)
		}
		requireSameJSON(t, "after", f.row(event).F("next_eligible_at"), python.Out["after"])
		requireSameTables(t, f, python)
	})
	t.Run("a backoff extended after the row was read still wins", func(t *testing.T) {
		tree := t.TempDir()
		python := runPython(t, tree, "del21", "extended")
		f := newFixture(t, tree)
		event := f.queuedEvent(regOpts{})
		f.setStatus("cancelled")
		stale, err := LoadRelationship(f.ctx, f.store, f.rid)
		mustDo(t, err)
		far := f.clock.Now() + 100000
		_, err = execSQL(f.ctx, f.store, "UPDATE deliveries SET next_eligible_at = ? WHERE event_id = ?", far, event)
		mustDo(t, err)
		record, err := f.delivery.WithholdInactive(f.ctx, event, stale, f.clock.Now(), 0)
		mustDo(t, err)
		requireSameJSON(t, "record", record, python.Out["record"])
		if f.row(event).F("next_eligible_at") != far {
			t.Fatal("the later deadline survives")
		}
		requireSameTables(t, f, python)
	})
}

func TestDEL22_a_superseded_relationship_is_left_to_the_supersession_path(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "del22", "superseded")
	f := newFixture(t, tree)
	event := f.queuedEvent(regOpts{})
	f.supersede("rel-bbbbbbbbbbbbbbbb")
	if record := f.mustAttempt(event, nil); record != nil || len(f.host.sends) != 0 {
		t.Fatalf("record %v", record)
	}
	if f.one("SELECT * FROM journal WHERE kind = ?", "delivery_withheld_inactive") != nil {
		t.Fatal("never tagged relationship_not_active")
	}
	requireSameTables(t, f, python)
}

func TestDEL23_a_resume_racing_the_withhold_leaves_the_delivery_alone(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "del22", "race")
	f := newFixture(t, tree)
	event := f.queuedEvent(regOpts{})
	stale, err := LoadRelationship(f.ctx, f.store, f.rid)
	mustDo(t, err)
	stale.Status = "paused"
	record, err := f.delivery.WithholdInactive(f.ctx, event, stale, f.clock.Now(), 0)
	mustDo(t, err)
	if record != nil || python.Out["record"] != nil || f.row(event).S("state") != Queued || f.one("SELECT * FROM journal WHERE kind = ?", "delivery_withheld_inactive") != nil {
		t.Fatal("a stale reading holds nothing and journals nothing")
	}
	requireSameTables(t, f, python)
}

func TestDEL24_guarded_transitions_prevent_duplicate_sends(t *testing.T) {
	t.Run("a stale busy observation cannot overwrite a dispatch", func(t *testing.T) {
		tree := t.TempDir()
		python := runPython(t, tree, "del24", "busy")
		f := newFixture(t, tree)
		event := f.queuedEvent(regOpts{})
		f.mustAttempt(event, nil)
		stale := Row{}
		for k, v := range f.row(event) {
			stale[k] = v
		}
		stale["state"], stale["attempt_count"] = Queued, int64(0)
		mustDo(t, f.delivery.DeferBusy(f.ctx, event, stale, f.clock.Now()))
		if f.row(event).S("state") != Dispatched {
			t.Fatal("the dispatch stands")
		}
		f.clock.Advance(100000)
		if again := f.mustAttempt(event, at(f.clock.Now())); again != nil || len(f.host.sends) != 1 {
			t.Fatal("sent once")
		}
		requireSameTables(t, f, python)
	})
	t.Run("reconciling an older attempt cannot reopen a dispatch", func(t *testing.T) {
		tree := t.TempDir()
		python := runPython(t, tree, "del24", "reconcile")
		f := newFixture(t, tree)
		event := f.queuedEvent(regOpts{})
		f.host.script = []string{"busy"}
		first := f.mustAttempt(event, nil)
		f.clock.Advance(3600)
		second := f.mustAttempt(event, at(f.clock.Now()))
		requireSameJSON(t, "second", second, python.Out["second"])
		outcome, err := NewReconciler(f.delivery).ReconcileAttempt(f.ctx, str(first, "requestId"), f.host, nil)
		mustDo(t, err)
		requireSameJSON(t, "reconciled", outcome, python.Out["reconciled"])
		f.clock.Advance(100000)
		if again := f.mustAttempt(event, at(f.clock.Now())); again != nil || f.row(event).S("state") != Dispatched || len(f.host.sends) != 2 {
			t.Fatal("the older attempt reopened the delivery")
		}
		requireSameTables(t, f, python)
	})
}

func TestDEL25_receipt_recovery_keeps_the_dispatch_turn_and_its_provenance(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "del25")
	f := newFixture(t, tree)
	event := f.queuedEvent(regOpts{})
	f.host.script = []string{"in_progress"}
	record := f.mustAttempt(event, nil)
	turn := f.host.startTurn(parent, "", "inProgress", "")
	request := str(record, "requestId")
	f.host.ledger[request] = Obj{{Key: "requestId", Value: request}, {Key: "status", Value: "accepted"}, {Key: "resumed", Value: Obj{{Key: "approvalPolicy", Value: "never"}}}, {Key: "turnId", Value: turn.TurnID}}
	outcome, err := NewReconciler(f.delivery).ReconcileAttempt(f.ctx, request, f.host, nil)
	mustDo(t, err)
	requireSameJSON(t, "record", record, python.Out["record"])
	requireSameJSON(t, "reconciled", outcome, python.Out["reconciled"])
	row := f.row(event)
	if row.S("state") != Dispatched || row.S("dispatch_turn_id") != turn.TurnID || row.S("dispatch_evidence") != "transport_accepted" {
		t.Fatalf("row %v", row)
	}
	requireSameTables(t, f, python)
}

func TestDEL26_a_later_turn_needs_an_explicit_continuation_admission(t *testing.T) {
	for _, tc := range []struct{ mode, reason, detail string }{
		{"none", "unassigned_turn", "explicit continuation admission"},
		{"valid", "", ""},
		{"anchor", "unassigned_turn", "is anchored to"},
		{"thread", "unassigned_turn", ""},
		{"malformed", "malformed_receipt", ""},
		{"replay", "", ""},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			tree := t.TempDir()
			python := runPython(t, tree, "del26", tc.mode)
			f := newFixture(t, tree)
			rid := f.register(regOpts{})
			turn := turnRef{child, "turn-loop-3", "completed"}
			text := "finished after several turns"
			if tc.mode == "thread" {
				turn.thread, text = "someone-else", "payload"
			}
			payload := f.readyPayload(rid, 1, []string{f.artifact("out.txt", text)}, 1, turn)
			admission := []byte(`{"anchorTurnId": "turn-dispatch-1", "actor": "a", "reason": "b"}`)
			switch tc.mode {
			case "valid":
				admission = []byte(`{"anchorTurnId": "turn-dispatch-1", "actor": "child-loop", "reason": "PABCD cycle 3 completed this generation"}`)
			case "anchor":
				admission = []byte(`{"anchorTurnId": "some-other-execution", "actor": "a", "reason": "b"}`)
			case "malformed":
				admission = []byte(`{"anchorTurnId": "turn-dispatch-1"}`)
			case "none":
				admission = nil
			}
			stored, err := f.accept(payload, store.AcceptOptions{Continuation: admission})
			if tc.mode == "replay" {
				mustDo(t, err)
				stored, err = f.accept(payload, store.AcceptOptions{})
			}
			want := python.Out["result"].(map[string]any)
			if tc.reason != "" {
				requireReason(t, err, tc.reason)
				requireSameJSON(t, "refusal", refusalOf(err), want)
				if !strings.Contains(Detail(err), tc.detail) {
					t.Fatalf("detail %q", Detail(err))
				}
			} else {
				mustDo(t, err)
				ok := want["ok"].(map[string]any)
				if tc.mode == "replay" && (!stored.Duplicate || ok["_duplicate"] != true) {
					t.Fatal("a replay is a duplicate")
				}
				requireSameJSON(t, "stored record", loadsObj(stored.Record), withoutUnderscored(ok))
			}
			requireSameTables(t, f, python)
		})
	}
}

func withoutUnderscored(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if !strings.HasPrefix(k, "_") {
			out[k] = v
		}
	}
	return out
}

func TestDEL27_the_completion_message_is_a_verification_request_with_the_ack_instruction(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "del27", "completion")
	f := newFixture(t, tree)
	event := f.queuedEvent(regOpts{})
	message, err := f.delivery.PreviewMessage(f.ctx, event)
	mustDo(t, err)
	if message != python.Out["message"] {
		t.Fatalf("message differs from Python:\n%s\n--\n%v", message, python.Out["message"])
	}
	receipt, _ := f.delivery.Receipt(f.ctx, event)
	manifest, _ := get(receipt, "manifest")
	entry := manifest.([]any)[0].(Obj)
	for _, want := range []string{"verification request", str(receipt, "revisionHash"), str(entry, "path"), str(entry, "sha256"), "ack-proof", "show --event"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message lacks %q", want)
		}
	}
	if strings.Contains(message, "None") {
		t.Fatal("None in the message")
	}
}

// revisionFixture is DirectionalMessages._revision: acknowledged, ruled needs_changes.
func revisionFixture(t *testing.T, tree string) (*fixture, *Ack, string, string, Obj, Obj) {
	f := newFixture(t, tree)
	event := f.queuedEvent(regOpts{recipients: []string{parent, child}})
	f.mustAttempt(event, nil)
	f.clock.Advance(5)
	turn := f.host.startTurn(parent, "ack-turn", "inProgress", "")
	ack := NewAck(f.delivery)
	acked, err := ack.Acknowledge(f.ctx, event, turn.TurnID, AckProof(event, turn.TurnID), true, nil, f.host)
	mustDo(t, err)
	verdict, err := ack.RecordVerdict(f.ctx, event, "needs_changes", "verdict-1", []any{Obj{{Key: "id", Value: "c-1"}, {Key: "verdict", Value: "needs_changes"}, {Key: "note", Value: "the manifest omits the migration script"}}}, nil, nil, nil)
	mustDo(t, err)
	revision := f.one("SELECT * FROM deliveries WHERE kind = 'revision_request'").S("event_id")
	return f, ack, event, revision, acked, verdict
}

func TestDEL28_the_revision_message_asks_for_no_acknowledgement_and_says_what_to_change(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "del27", "revision")
	f, _, source, revision, acked, verdict := revisionFixture(t, tree)
	requireSameJSON(t, "ack", acked, python.Out["ack"])
	requireSameJSON(t, "verdict", verdict, python.Out["verdict"])
	message, err := f.delivery.PreviewMessage(f.ctx, revision)
	mustDo(t, err)
	if message != python.Out["message"] {
		t.Fatalf("message differs from Python:\n%s\n--\n%v", message, python.Out["message"])
	}
	for _, want := range []string{"revision request", "nothing to acknowledge", "emit --relationship", "the manifest omits the migration script", source, "--generation 2"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message lacks %q", want)
		}
	}
	for _, never := range []string{"ack-proof --event", "--ack-proof", "--ack-turn", "None"} {
		if strings.Contains(message, never) {
			t.Fatalf("message carries %q", never)
		}
	}
}

func TestDEL29_the_instruction_each_side_is_given_is_the_one_that_works(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "del27", "revision")
	f, ack, source, revision, _, _ := revisionFixture(t, tree)
	if f.one("SELECT 1 AS x FROM acks WHERE event_id = ?", source) == nil {
		t.Fatal("the parent's acknowledgement landed")
	}
	_, err := ack.Acknowledge(f.ctx, revision, "child-turn", AckProof(revision, "child-turn"), true, nil, f.host)
	requireReason(t, err, WrongDeliveryKind)
	requireSameJSON(t, "child ack refusal", refusalOf(err), python.Out["childAck"])
	r, err := LoadRelationship(f.ctx, f.store, f.rid)
	mustDo(t, err)
	if r.Generation != 2 || python.Out["generation"] != float64(2) {
		t.Fatal("the child's emit goes to generation 2")
	}
	requireSameTables(t, f, python)
}

func TestDEL30_project_key_distinguishes_projects_for_a_shared_service(t *testing.T) {
	tree := t.TempDir()
	python := runPython(t, tree, "del30")
	f := newFixture(t, tree)
	other := f.otherAssignment()
	mine := f.register(regOpts{issue: "REL-3", dispatchRequest: "dispatch-3"})
	a, err := LoadRelationship(f.ctx, f.store, mine)
	mustDo(t, err)
	b, err := LoadRelationship(f.ctx, f.store, other)
	mustDo(t, err)
	if ProjectKey(a) != python.Out["mine"] || ProjectKey(b) != python.Out["other"] || ProjectKey(b) != "/other" || ProjectKey(a) == ProjectKey(b) {
		t.Fatalf("project keys %q %q", ProjectKey(a), ProjectKey(b))
	}
}
