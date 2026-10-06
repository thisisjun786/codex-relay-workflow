package managed

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-842. Two post-merge P1s on the CRW-788 pull request.
//
// P1-1: a repeat of a managed start whose reservation row already records an accepted creation
// receipt (receipt_status accepted, child_task_id equal to the unknown creation receipt's thread,
// standby_turn_id set) re-read the host, saw the business turn beside the standby turn, and
// answered incomplete/creation_unknown/thread_has_turn instead of converging on admitted. The
// recorded identity is now adopted before decide(), without reading the host.
//
// P1-2 is pinned by the fragment rows in reconcile_standby_turn_test.go.

// crw842ListingFlip answers the summary listing it is given first, then the one after it: the
// repeat of a managed start sees a host that has moved on (the business turn beside the standby
// turn, or only the business turn) without the first run having to be replayed by hand.
type crw842ListingFlip struct {
	first, then func(string) (map[string]any, error)
	calls       int
}

func (f *crw842ListingFlip) summary(thread string) (map[string]any, error) {
	f.calls++
	if f.calls == 1 {
		return f.first(thread)
	}
	return f.then(thread)
}

// crw842RecordedIdentityKit is a reconcile kit whose creation lost its turn/start answer, whose
// thread/turns/list answer flips between the first run and the repeat, and whose reservation row
// the test may read and rewrite.
func crw842RecordedIdentityKit(t *testing.T, first, then func(string) (map[string]any, error)) (*reconcileKit, *crw842ListingFlip) {
	t.Helper()
	flip := &crw842ListingFlip{first: first, then: then}
	k, _ := standbyKit(t, "turn-timeout", flip.summary)
	return k, flip
}

// reservationRow reads the request's reservation row as it stands now.
func (k *reconcileKit) reservationRow() store.ManagedStartRequestsRow {
	k.t.Helper()
	row, err := k.start.Store.ManagedStartRequest(context.Background(), "managed-1")
	if err != nil {
		k.t.Fatal(err)
	}
	return row
}

// rewriteRow changes the columns the recorded-identity guard reads, so a test can pin what the
// guard does when the row does not name this creation's identity.
func (k *reconcileKit) rewriteRow(query string, args ...any) {
	k.t.Helper()
	if _, err := k.start.Store.DB.ExecContext(context.Background(), query, args...); err != nil {
		k.t.Fatal(err)
	}
}

// crw842TwoTurns is the repeat's listing once the business turn exists: the business turn beside the
// standby turn this creation sent, newest first, as the host answered the parent's reproduction.
func crw842TwoTurns(standby string) func(string) (map[string]any, error) {
	return listing([]any{summaryRow("business-B", "completed", userText("assignment")), summaryRow(standby, "completed", userText(bootstrap))}, "")
}

// crw842OneStandbyTurn is the first run's listing: the standby turn this creation sent, and nothing else.
func crw842OneStandbyTurn(standby string) func(string) (map[string]any, error) {
	return listing([]any{summaryRow(standby, "completed", userText(bootstrap))}, "")
}

// A repeat whose reservation row already records the accepted child and standby turn adopts that
// recorded identity without reading the host, and converges on admitted with no further effect.
func TestRecordedIdentityReplayAfterBusinessTurnConverges(t *testing.T) {
	t.Parallel()
	turn := "01a10ff9-a5e8-7c61-a08c-0ac0508eadb4"
	k, flip := crw842RecordedIdentityKit(t, crw842OneStandbyTurn(turn), crw842TwoTurns(turn))
	first := k.run()
	k.expect(first, "admitted", "", "adopted")
	if first["standbyTurnId"] != turn || first["childTaskId"] != "t-1" {
		t.Fatalf("first run adopted child %v standby %v", first["childTaskId"], first["standbyTurnId"])
	}
	k.effects(1, 1)

	// The row now records the identity the first run published; the repeat must not read the host.
	row := k.reservationRow()
	if row.ReceiptStatus.String != "accepted" || row.ChildTaskID.String != "t-1" || row.StandbyTurnID.String != turn {
		t.Fatalf("the reservation did not record the published identity: %+v", row)
	}
	reads := flip.calls
	hostCalls := len(k.host.calls)
	again := k.run()
	k.expect(again, "admitted", "", "adopted")
	if again["standbyTurnId"] != turn || again["childTaskId"] != "t-1" {
		t.Fatalf("the repeat changed the child or the standby turn: %v %v", again["childTaskId"], again["standbyTurnId"])
	}
	if detail := pyjson.Text(recon(again)["detail"]); !crw842ContainsAll(detail, turn, "t-1", "reservation recorded") {
		t.Fatalf("the repeat's detail does not name the recorded identity: %q", detail)
	}
	if flip.calls != reads {
		t.Fatalf("the repeat read the host's turn listing: %d calls, want %d", flip.calls, reads)
	}
	// The adoption happens before decide(), so the repeat makes no host read at all - not a
	// thread/read, not a turns listing. Reading the host and answering the same is not the fix.
	if len(k.host.calls) != hostCalls {
		t.Fatalf("the repeat read the host: %v", k.host.calls[hostCalls:])
	}
	k.noSecondTurn()
	k.effects(1, 1)
}

// The same convergence holds when the process stopped after the business turn was accepted and
// before admission: the retained business receipt is read, not sent again.
func TestRecordedIdentityStopBeforeAdmissionConverges(t *testing.T) {
	t.Parallel()
	turn := "01a10ff9-a5e8-7c61-a08c-0ac0508eadb4"
	// The repeat's listing holds only the business turn: the standby turn is not in this view.
	k, _ := crw842RecordedIdentityKit(t, crw842OneStandbyTurn(turn), listing([]any{summaryRow("business-B", "completed", userText("assignment"))}, ""))
	k.expect(k.run(), "admitted", "", "adopted")
	k.effects(1, 1)

	// The admission the stopped process never recorded.
	k.rewriteRow("DELETE FROM generation_turns WHERE turn_id = ?", "business")
	k.rewriteRow("UPDATE managed_start_requests SET state = 'attached' WHERE request_id = ?", "managed-1")

	again := k.run()
	k.expect(again, "admitted", "", "adopted")
	if again["standbyTurnId"] != turn || again["childTaskId"] != "t-1" || again["businessTurnId"] != "business" {
		t.Fatalf("the repeat did not converge on the recorded identity: %v", again)
	}
	k.noSecondTurn()
	k.effects(1, 1) // the creation and the business turn only: the business turn is not sent twice
}

// A row that does not name this creation's identity keeps today's answer.
func TestRecordedIdentityMismatchKeepsTodaysAnswer(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		query string
		args  []any
	}{
		{"another child", "UPDATE managed_start_requests SET state = 'create_armed', child_task_id = ? WHERE request_id = ?", []any{"other-thread", "managed-1"}},
		{"no standby turn", "UPDATE managed_start_requests SET state = 'create_armed', standby_turn_id = NULL WHERE request_id = ?", []any{"managed-1"}},
		{"the receipt was not accepted", "UPDATE managed_start_requests SET state = 'create_armed', receipt_status = 'partial' WHERE request_id = ?", []any{"managed-1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			turn := "01a10ff9-a5e8-7c61-a08c-0ac0508eadb4"
			k, _ := crw842RecordedIdentityKit(t, crw842OneStandbyTurn(turn), crw842TwoTurns(turn))
			k.expect(k.run(), "admitted", "", "adopted")
			k.rewriteRow(c.query, c.args...)
			got := k.run()
			k.expect(got, "incomplete", "creation_unknown", "thread_has_turn")
			// The answer must come from observe(), not from the reservation: the recorded
			// identity is not adopted, so the detail is observe()'s own and never names the
			// reservation.
			if detail := pyjson.Text(recon(got)["detail"]); strings.Contains(detail, "reservation recorded") {
				t.Fatalf("an identity the row does not record was adopted: %q", detail)
			}
			k.noSecondTurn()
			k.effects(1, 1)
		})
	}
}

// crw842ContainsAll reports whether text holds every want.
func crw842ContainsAll(text string, want ...string) bool {
	for _, w := range want {
		if !strings.Contains(text, w) {
			return false
		}
	}
	return true
}

// A receipt that names no thread at all is never matched to the recorded identity, however
// accepted the row is: the row is adopted only for the very thread the unknown receipt names.
func TestRecordedIdentityThreadlessReceiptIsNotAdopted(t *testing.T) {
	t.Parallel()
	turn := "01a10ff9-a5e8-7c61-a08c-0ac0508eadb4"
	k, _ := crw842RecordedIdentityKit(t, crw842OneStandbyTurn(turn), crw842TwoTurns(turn))
	k.expect(k.run(), "admitted", "", "adopted")
	k.effects(1, 1)

	// The host's creation receipt loses the thread it named, as the scan-found shape has it.
	create, _ := OperationIDs("managed-1")
	delete(k.host.operations[create], "threadId")

	got := k.run()
	if detail := pyjson.Text(recon(got)["detail"]); strings.Contains(detail, "reservation recorded") {
		t.Fatalf("a threadless receipt adopted the recorded identity: %q", detail)
	}
	if got["state"] != "incomplete" || pyjson.Text(got["reason"]) != "creation_unknown" {
		t.Fatalf("a threadless receipt did not fall through to today's answer: %v/%v", got["state"], got["reason"])
	}
	if got["standbyTurnId"] != turn || got["childTaskId"] != "t-1" {
		t.Fatalf("the reported identity changed: %v %v", got["childTaskId"], got["standbyTurnId"])
	}
	k.noSecondTurn()
	k.effects(1, 1)
}

// crw842UserParts is a userMessage item whose content is exactly the parts given: the shape a message
// with more than one text part, or with a part that is not text, reaches the recogniser in.
func crw842UserParts(parts ...map[string]any) map[string]any {
	content := make([]any, 0, len(parts))
	for _, part := range parts {
		content = append(content, part)
	}
	return map[string]any{"type": "userMessage", "id": "user-parts", "content": content}
}

// crw842TextPart is one content part carrying text.
func crw842TextPart(text string) map[string]any { return map[string]any{"type": "text", "text": text} }

// crw842UserTextAndContent is a userMessage item that states its own text and carries content beside it.
func crw842UserTextAndContent(text string, parts ...map[string]any) map[string]any {
	item := crw842UserParts(parts...)
	item["text"] = text
	return item
}

// crw842CRW748UserMessage is CRW-748's real recorded first user message: one text part carrying the
// bootstrap, with the host's empty text_elements beside the text. It is the shape that must keep
// being recognised.
func crw842CRW748UserMessage() map[string]any {
	return crw842UserParts(map[string]any{"type": "text", "text": bootstrap, "text_elements": []any{}})
}
