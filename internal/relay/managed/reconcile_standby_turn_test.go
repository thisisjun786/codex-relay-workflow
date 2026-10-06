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

// A completed standby turn the creation sent is recognised and adopted, and the run goes on to the business turn exactly as it does for a
// creation receipt that carried the turn id.
func TestStandbyTurnACompletedTurnTheCreationSentIsAdopted(t *testing.T) {
	t.Parallel()
	turn := "01a10ff9-a5e8-7c61-a08c-0ac0508eadb4"
	k, _ := standbyKit(t, "turn-timeout", listing([]any{summaryRow(turn, "completed", userText(bootstrap))}, ""))
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
