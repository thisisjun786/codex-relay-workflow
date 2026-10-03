package mergeturn

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

// CRW-408 criterion c4, the relay side: a return request is queued for the holder through the
// same delivery channel a grant uses (the delivery engine sends it; internal/relay/delivery
// drives that half).

func returnFixture(t *testing.T) (*fx, string) {
	t.Helper()
	w, _, relationship := grantFixture(t)
	turn := w.must(w.m.Request(w.ctx, fxRepo, fxBase, fxA, alpha.TaskID, alpha.HostID, "head-a", true, ClaimOptions{Relationship: sql.NullString{String: relationship, Valid: true}}))["turnId"].(string)
	return w, turn
}

func noticeReceipt(t *testing.T, w *fx, event string) map[string]any {
	t.Helper()
	row, err := w.s.One(w.ctx, "SELECT receipt, outcome FROM events WHERE event_id = ?", event)
	if err != nil || row == nil {
		t.Fatalf("no event %s: %v", event, err)
	}
	var receipt map[string]any
	if err := json.Unmarshal([]byte(row.Get("receipt").(string)), &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func notice(t *testing.T, answer map[string]any) map[string]any {
	t.Helper()
	return asMap(t, jsonValue(t, answer["returnNotice"]))
}

func Test408_c4_a_return_request_is_queued_for_the_holder(t *testing.T) {
	w, turn := returnFixture(t)
	answer := w.must(w.m.RequestReturn(w.ctx, turn, beta.TaskID, "I have a ready candidate"))
	queued := notice(t, answer)
	event, _ := queued["eventId"].(string)
	if queued["state"] != "queued" || event == "" {
		t.Fatalf("the notice is queued: %v", queued)
	}
	if _, entry := ledgerEntry(t, answer, "return_requested"); entry != nil {
		t.Fatalf("the request keeps its ledger entry as text, not an envelope: %v", entry)
	}
	receipt := noticeReceipt(t, w, event)
	for key, want := range map[string]any{"kind": "merge_turn_return_request", "turnId": turn, "requestedBy": beta.TaskID, "recipientTaskId": alpha.TaskID, "candidateHead": "head-a", "repository": fxRepo, "baseRef": fxBase, "evidence": "I have a ready candidate"} {
		if receipt[key] != want {
			t.Fatalf("receipt %s = %v, want %v (%v)", key, receipt[key], want, receipt)
		}
	}
	delivery, err := w.s.One(w.ctx, "SELECT kind, recipient_task_id, state FROM deliveries WHERE event_id = ?", event)
	if err != nil || delivery == nil || delivery.Get("kind") != "merge_turn_grant" || delivery.Get("recipient_task_id") != alpha.TaskID || delivery.Get("state") != "queued" {
		t.Fatalf("the notice travels the grant channel to the holder: %v %v", delivery, err)
	}
	if shown := w.reading(); shown["returnRequestedAt"] == nil {
		t.Fatalf("merge-turn-show keeps reporting the request: %v", shown)
	}
}

func Test408_c4_a_repeated_request_queues_nothing_new_and_another_requester_does(t *testing.T) {
	w, turn := returnFixture(t)
	first := notice(t, w.must(w.m.RequestReturn(w.ctx, turn, beta.TaskID, "I have a ready candidate")))
	again := notice(t, w.must(w.m.RequestReturn(w.ctx, turn, beta.TaskID, "I have a ready candidate")))
	if first["eventId"] != again["eventId"] {
		t.Fatalf("a replay converges on one notice: %v %v", first, again)
	}
	other := notice(t, w.must(w.m.RequestReturn(w.ctx, turn, overseer.TaskID, "the supervisor needs the lane")))
	if other["eventId"] == first["eventId"] {
		t.Fatalf("a different requester is a different request: %v", other)
	}
	rows, err := w.s.All(w.ctx, "SELECT event_id FROM deliveries WHERE kind = 'merge_turn_grant'")
	if err != nil || len(rows) != 2 {
		t.Fatalf("two requests, two notices: %v %v", rows, err)
	}
}

func Test408_c4_the_notice_stays_current_while_the_turn_occupies_the_target(t *testing.T) {
	w, turn := returnFixture(t)
	queued := notice(t, w.must(w.m.RequestReturn(w.ctx, turn, beta.TaskID, "I have a ready candidate")))
	receipt, _ := json.Marshal(noticeReceipt(t, w, queued["eventId"].(string)))
	reading := func() string {
		why, err := GrantSupersessionFor(w.ctx, w.s, string(receipt))
		if err != nil {
			t.Fatal(err)
		}
		return why
	}
	if why := reading(); why != "" {
		t.Fatalf("a request about a held turn is current: %q", why)
	}
	w.answer(turn, alpha.TaskID)
	if why := reading(); why != "" {
		t.Fatalf("acknowledging the grant does not answer a request made after it: %q", why)
	}
	w.must(w.m.Release(w.ctx, turn, alpha.TaskID, "returned", "giving it back", ""))
	if why := reading(); why != Closed {
		t.Fatalf("a closed turn has nothing left to ask for: %q", why)
	}
}

func Test408_c4_a_request_that_cannot_be_delivered_is_recorded_not_lost(t *testing.T) {
	w := newFx(t)
	w.m.Delivery = StoreDelivery{Store: w.s}
	turn := w.held()
	queued := notice(t, w.must(w.m.RequestReturn(w.ctx, turn, beta.TaskID, "I have a ready candidate")))
	if queued["state"] != "unaddressed" || !strings.Contains(queued["reason"].(string), "no assignment") {
		t.Fatalf("a turn naming no assignment has nobody to wake: %v", queued)
	}
	if _, entry := ledgerEntry(t, w.must(w.m.Turn(w.ctx, turn)), "return_requested"); entry != nil {
		t.Fatal("unexpected envelope")
	}
	journal, err := w.s.All(w.ctx, "SELECT detail FROM journal WHERE kind = 'merge_turn_wake_unaddressed' AND subject = ?", turn)
	if err != nil || len(journal) != 1 {
		t.Fatalf("the missing address is journaled as a grant's is: %v %v", journal, err)
	}
	rows, _ := w.s.All(w.ctx, "SELECT 1 FROM deliveries")
	if len(rows) != 0 {
		t.Fatalf("nothing was queued: %v", rows)
	}
}

func Test408_c4_a_request_for_a_turn_that_holds_nothing_wakes_nobody(t *testing.T) {
	w, _ := returnFixture(t)
	waiting := w.must(w.claimOn(beta, fxB, "head-b", fxBase, true))["turnId"].(string)
	sent := notice(t, w.must(w.m.RequestReturn(w.ctx, waiting, alpha.TaskID, "give your place back")))
	if sent["state"] != "not_sent" {
		t.Fatalf("a waiting claim holds nothing to return: %v", sent)
	}
}
