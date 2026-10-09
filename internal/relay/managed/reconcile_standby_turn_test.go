package managed

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// CRW-788. A creation whose turn/start answer was lost keeps an outcome_unknown receipt that lists
// turn/start among its attempted effects, while the host ran the standby turn and the turn finished.
// The receipt's own thread then shows exactly that turn, and its first user message is byte for byte
// the bootstrap the creation sends: that is what tells the standby turn this creation sent from a
// turn someone else started, and the creation continues on it instead of stopping at thread_has_turn.
//
// The receipt shape here is CRW-748's: outcome_unknown, threadId named, attemptedEffects
// [thread/start, thread/name/set, turn/start].

// standbyApp is the managed fake whose thread/turns/list answer a test scripts, so a summary listing can carry the items the standby check
// reads. scriptedApp answers the listing with rows that hold no items at all.
type standbyApp struct {
	*scriptedApp
	// summary answers thread/turns/list when set; a nil summary keeps scriptedApp's answer.
	summary func(thread string) (map[string]any, error)
}

func (h *standbyApp) HostCall(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	if method == "thread/turns/list" && h.summary != nil {
		return h.summary(pyjson.Text(params["threadId"]))
	}
	return h.scriptedApp.HostCall(ctx, method, params)
}

// summaryRow is one row of a thread/turns/list (itemsView summary) answer.
func summaryRow(id, status string, items ...any) map[string]any {
	return map[string]any{"id": id, "status": status, "items": items, "itemsView": "summary"}
}

// userText is a userMessage item whose first text part is text.
func userText(text string) map[string]any {
	return map[string]any{"type": "userMessage", "id": "user-1", "content": []any{map[string]any{"type": "text", "text": text}}}
}

// userTextFlat is a userMessage item that states its text directly, the shape the bridge's own summary-turn fixtures use
// (internal/bridge/owned_read_host_test.go's message) and the one delivery.itemText reads first.
func userTextFlat(text string) map[string]any {
	return map[string]any{"type": "userMessage", "id": "user-flat", "text": text}
}

// listing answers one thread's turns with rows and, when cursor is non-empty, a next page.
func listing(rows []any, cursor string) func(string) (map[string]any, error) {
	return func(string) (map[string]any, error) {
		out := map[string]any{"data": rows}
		if cursor != "" {
			out["nextCursor"] = cursor
		}
		return out, nil
	}
}

// standbyKit is a reconcile kit whose creation lost its turn/start answer (CRW-748's receipt) and whose thread/turns/list answer the test
// scripts.
func standbyKit(t *testing.T, outcome string, summary func(string) (map[string]any, error)) (*reconcileKit, *standbyApp) {
	t.Helper()
	k := newReconcileKit(t, outcome)
	app := &standbyApp{scriptedApp: k.host, summary: summary}
	k.start.Adapter = app
	return k, app
}

// noSecondTurn asserts the creation was not run again, not sent a standby turn and not renamed.
func (k *reconcileKit) noSecondTurn() {
	k.t.Helper()
	for _, id := range k.host.sends {
		if strings.HasPrefix(id, "managed-standby-") {
			k.t.Fatalf("a standby turn was sent to the thread: %v", k.host.sends)
		}
	}
	if len(k.host.creates) != 1 || len(k.host.named) != 0 {
		k.t.Fatalf("creations %v, names %v", k.host.creates, k.host.named)
	}
}

// The summary view may state the user message's text on the item itself rather than nesting it in content, and the bridge's own reads
// accept both. The recognised standby turn must be read the same way in either shape.
func TestStandbyTurnReadsBothItemTextShapes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		item map[string]any
		ok   bool
	}{
		{"nested content", userText(bootstrap), true},
		{"text on the item", userTextFlat(bootstrap), true},
		// CRW-842: CRW-748's recorded message, one text part with the host's empty text_elements
		// beside the text, is still the standby the creation sent.
		{"one part with text_elements", crw842CRW748UserMessage(), true},
		{"text on the item, one character off", userTextFlat(bootstrap + "!"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			k, _ := standbyKit(t, "turn-timeout", listing([]any{summaryRow("standby-turn", "completed", c.item)}, ""))
			got := k.run()
			if !c.ok {
				k.expect(got, "incomplete", "creation_unknown", "thread_has_turn")
				k.noSecondTurn()
				return
			}
			k.expect(got, "admitted", "", "adopted")
			if got["standbyTurnId"] != "standby-turn" {
				t.Fatalf("the flat item's turn was not recorded as the standby: %v", got["standbyTurnId"])
			}
			k.noSecondTurn()
		})
	}
}

// A completed standby turn the creation sent is recognised and adopted, and the run goes on to the business turn exactly as it does for a
// creation receipt that carried the turn id.
func TestStandbyTurnACompletedTurnTheCreationSentIsAdopted(t *testing.T) {
	t.Parallel()
	turn := "01a10ff9-a5e8-7c61-a08c-0ac0508eadb4"
	// The repeat meets a host that has moved on: the business turn this run went on to send is
	// listed beside the standby turn, so the two-turn listing must not talk the engine out of the
	// identity its own reservation recorded (CRW-842).
	flip := &crw842ListingFlip{
		first: listing([]any{summaryRow(turn, "completed", userText(bootstrap))}, ""),
		then:  listing([]any{summaryRow("business-B", "completed", userText("assignment")), summaryRow(turn, "completed", userText(bootstrap))}, ""),
	}
	k, _ := standbyKit(t, "turn-timeout", flip.summary)
	got := k.run()
	k.expect(got, "admitted", "", "adopted")
	if got["standbyTurnId"] != turn || got["childTaskId"] != "t-1" {
		t.Fatalf("adopted child %v standby %v", got["childTaskId"], got["standbyTurnId"])
	}
	if detail := pyjson.Text(recon(got)["detail"]); !strings.Contains(detail, turn) || !strings.Contains(detail, "standby turn") {
		t.Fatalf("detail %q does not name the standby turn", detail)
	}
	k.noSecondTurn()
	k.effects(1, 1) // the creation, then the business turn only
	// A repeat reaches the same thread: the engine never rewrites the host's receipt, so it still
	// says outcome_unknown and nothing may depend on an earlier run having changed it. The identity
	// the reservation recorded is what the repeat converges on, and no further host effect is taken.
	again := k.run()
	k.expect(again, "admitted", "", "adopted")
	if again["standbyTurnId"] != turn || again["childTaskId"] != "t-1" {
		t.Fatalf("a repeat changed the child or the standby turn: %v %v", again["childTaskId"], again["standbyTurnId"])
	}
	if detail := pyjson.Text(recon(again)["detail"]); !strings.Contains(detail, "reservation recorded") {
		t.Fatalf("the repeat did not adopt the recorded identity: %q", detail)
	}
	k.noSecondTurn()
	k.effects(1, 1)
}

// A standby turn still running leaves the request pending, to be read again, and sends nothing.
func TestStandbyTurnInProgressIsPending(t *testing.T) {
	t.Parallel()
	k, _ := standbyKit(t, "turn-timeout", listing([]any{summaryRow("standby-running", "inProgress", userText(bootstrap))}, ""))
	got := k.run()
	k.expect(got, "incomplete", "creation_unknown", "pending")
	if recon(got)["repeatAfter"] == nil {
		t.Fatalf("a pending answer says when to repeat: %v", recon(got))
	}
	k.noSecondTurn()
	k.effects(1, 0)
}

// Every answer that is not exactly one completed or in-progress bootstrap turn keeps today's answer: the thread still has a turn that
// cannot be told from the standby, or the listing cannot be read at all.
func TestStandbyTurnAnythingElseKeepsTodaysAnswer(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		outcome string
		summary func(string) (map[string]any, error)
		why     string
	}{
		{"two turns", "turn-timeout", listing([]any{summaryRow("a", "completed", userText(bootstrap)), summaryRow("b", "completed", userText(bootstrap))}, ""), "thread_has_turn"},
		{"text differs by one character", "turn-timeout", listing([]any{summaryRow("a", "completed", userText(bootstrap+"!"))}, ""), "thread_has_turn"},
		{"failed", "turn-timeout", listing([]any{summaryRow("a", "failed", userText(bootstrap))}, ""), "thread_has_turn"},
		{"interrupted", "turn-timeout", listing([]any{summaryRow("a", "interrupted", userText(bootstrap))}, ""), "thread_has_turn"},
		{"no user message", "turn-timeout", listing([]any{summaryRow("a", "completed", map[string]any{"type": "agentMessage", "id": "m1"})}, ""), "thread_has_turn"},
		{"input it cannot read", "turn-timeout", listing([]any{summaryRow("a", "completed", map[string]any{"type": "userMessage", "id": "u1", "content": []any{}})}, ""), "thread_has_turn"},
		// CRW-842: the standby is the whole first user message, not its first text part.
		{"a second text part beside the bootstrap", "turn-timeout", listing([]any{summaryRow("a", "completed", crw842UserParts(crw842TextPart(bootstrap), crw842TextPart("!")))}, ""), "thread_has_turn"},
		{"a part that is not text", "turn-timeout", listing([]any{summaryRow("a", "completed", crw842UserParts(crw842TextPart(bootstrap), map[string]any{"type": "image"}))}, ""), "thread_has_turn"},
		{"the item text differs from the content", "turn-timeout", listing([]any{summaryRow("a", "completed", crw842UserTextAndContent(bootstrap, crw842TextPart(bootstrap+"!")))}, ""), "thread_has_turn"},
		{"the item text with two content parts", "turn-timeout", listing([]any{summaryRow("a", "completed", crw842UserTextAndContent(bootstrap, crw842TextPart(bootstrap), crw842TextPart("!")))}, ""), "thread_has_turn"},
		// CRW-935: a field that is present but disagrees or cannot be read is not an absent one.
		{"an explicitly empty item text beside a content bootstrap", "turn-timeout", listing([]any{summaryRow("a", "completed", crw842UserTextAndContent("", crw842TextPart(bootstrap)))}, ""), "thread_has_turn"},
		{"content that is not a list beside a bootstrap item text", "turn-timeout", listing([]any{summaryRow("a", "completed", map[string]any{"type": "userMessage", "id": "user-text", "text": bootstrap, "content": "not a list"})}, ""), "thread_has_turn"},
		{"content that is an object beside a bootstrap item text", "turn-timeout", listing([]any{summaryRow("a", "completed", map[string]any{"type": "userMessage", "id": "user-text", "text": bootstrap, "content": map[string]any{"type": "text", "text": bootstrap}})}, ""), "thread_has_turn"},
		{"an item text that is not text beside a content bootstrap", "turn-timeout", listing([]any{summaryRow("a", "completed", map[string]any{"type": "userMessage", "id": "user-text", "text": 5, "content": []any{crw842TextPart(bootstrap)}})}, ""), "thread_has_turn"},
		{"a next page", "turn-timeout", listing([]any{summaryRow("a", "completed", userText(bootstrap))}, "more"), "thread_has_turn"},
		{"the listing fails", "turn-timeout", func(string) (map[string]any, error) { return nil, errors.New("host unavailable") }, "unobservable"},
		{"a scan found the thread", "lost-applied", listing([]any{summaryRow("a", "completed", userText(bootstrap))}, ""), "thread_has_turn"},
		{"the receipt never sent turn/start", "name-timeout", listing([]any{summaryRow("a", "completed", userText(bootstrap))}, ""), "thread_has_turn"},
		{"the receipt kept no effects", "saved-title", listing([]any{summaryRow("a", "completed", userText(bootstrap))}, ""), "thread_has_turn"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			k, _ := standbyKit(t, c.outcome, c.summary)
			for range 2 {
				got := k.run()
				k.expect(got, "incomplete", "creation_unknown", c.why)
				if got["standbyTurnId"] != nil {
					t.Fatalf("a turn was recorded as the standby: %v", got["standbyTurnId"])
				}
			}
			k.noSecondTurn()
			if k.host.sent != 0 {
				t.Fatalf("a stopped start sent: %v", k.host.sends)
			}
		})
	}
}
