package mergeturn

import (
	"strings"
	"testing"
)

// CRW-408 criterion c4: a return request is one atomic fact with one notice.

func Test408_c4_a_replay_keeps_the_original_request_and_adds_no_second_record(t *testing.T) {
	w, turn := returnFixture(t)
	first := notice(t, w.must(w.m.RequestReturn(w.ctx, turn, beta.TaskID, "I have a ready candidate")))
	again := notice(t, w.must(w.m.RequestReturn(w.ctx, turn, beta.TaskID, "a different explanation")))
	if first["eventId"] != again["eventId"] || again["state"] != "queued" {
		t.Fatalf("a replay answers with the notice already queued: %v %v", first, again)
	}
	if got := noticeReceipt(t, w, first["eventId"].(string))["evidence"]; got != "I have a ready candidate" {
		t.Fatalf("the original evidence stands: %v", got)
	}
	requests := 0
	for _, item := range jsonValue(t, w.must(w.m.Turn(w.ctx, turn))["ledger"]).([]any) {
		entry := asMap(t, item)
		if entry["evidenceKind"] == "return_requested" {
			requests++
			if entry["evidence"] != "I have a ready candidate" {
				t.Fatalf("the ledger keeps the original text: %v", entry)
			}
		}
	}
	if requests != 1 {
		t.Fatalf("one request, one ledger entry: %d", requests)
	}
	queued, err := w.s.All(w.ctx, "SELECT 1 FROM journal WHERE kind = 'merge_turn_wake_queued' AND subject = ?", turn)
	delivered, err2 := w.s.All(w.ctx, "SELECT 1 FROM journal WHERE kind = 'delivery_queued'")
	if err != nil || err2 != nil || len(queued) != 1 || len(delivered) != 1 {
		t.Fatalf("one queued notice is journaled once: %v %v %v %v", queued, delivered, err, err2)
	}
}

func Test408_c4_a_notice_that_cannot_be_queued_leaves_no_request_behind(t *testing.T) {
	w, turn := returnFixture(t)
	w.m.Delivery = failingQueue{StoreDelivery{Store: w.s}}
	if _, err := w.m.RequestReturn(w.ctx, turn, beta.TaskID, "I have a ready candidate"); err == nil || !strings.Contains(err.Error(), "delivery store is unavailable") {
		t.Fatalf("the request fails on the notice that cannot be queued: %v", err)
	}
	for _, item := range jsonValue(t, w.must(w.m.Turn(w.ctx, turn))["ledger"]).([]any) {
		if asMap(t, item)["evidenceKind"] == "return_requested" {
			t.Fatal("the request was recorded without its notice")
		}
	}
	if rows, _ := w.s.All(w.ctx, "SELECT 1 FROM journal WHERE kind LIKE 'merge_turn_wake%'"); len(rows) != 0 {
		t.Fatalf("nothing journaled: %v", rows)
	}
}

func Test408_c4_an_unaddressed_request_journals_once(t *testing.T) {
	w := newFx(t)
	w.m.Delivery = StoreDelivery{Store: w.s}
	turn := w.held()
	w.must(w.m.RequestReturn(w.ctx, turn, beta.TaskID, "I have a ready candidate"))
	w.must(w.m.RequestReturn(w.ctx, turn, beta.TaskID, "I have a ready candidate"))
	journal, err := w.s.All(w.ctx, "SELECT 1 FROM journal WHERE kind = 'merge_turn_wake_unaddressed' AND subject = ?", turn)
	if err != nil || len(journal) != 1 {
		t.Fatalf("the missing address is journaled by the request, not by its replay: %v %v", journal, err)
	}
}
