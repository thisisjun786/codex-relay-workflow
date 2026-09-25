package bridge

import (
	"context"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"strings"
	"testing"
)

func readSetup(t *testing.T) (*Bridge, *fakehost.Server) {
	t.Helper()
	b, h := testBridge(t)
	h.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": "/preserved"}}})
	h.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-1", "items": []any{map[string]any{"text": "first"}}}}}})
	h.Respond("thread/items/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"text": "first"}}}})
	return b, h
}
func firstTurn(t *testing.T, r map[string]any) map[string]any {
	t.Helper()
	page := object(r["turnsPage"])
	if page == nil {
		t.Fatalf("missing page: %v", r)
	}
	turns, ok := page["data"].([]any)
	if !ok || len(turns) == 0 {
		t.Fatalf("missing turns: %v", r)
	}
	return object(turns[0])
}
func Test_test_items_that_will_not_arrive_at_all_are_reported_not_claimed(t *testing.T) {
	b, _, view := smallFrameThread(t)
	view.Oversize = func(method string, _ map[string]any) int {
		if method == "thread/items/list" {
			return overflow
		}
		return 0
	}
	r, err := b.ReadThread(context.Background(), "thread-1", 1, nil, 4000)
	if err != nil {
		t.Fatal(err)
	}
	turn := firstTurn(t, r)
	observation := object(r["observation"])
	note := text(object(turn["itemsDetailNote"])["note"])
	if turn["itemsDetailStatus"] != "not_observed" || turn["itemsDetail"] != nil || len(object(turn["itemsDetailNote"])["attempts"].([]any)) != 2 || !strings.Contains(note, "no query narrower than one item") || !strings.Contains(note, "limit on observation, not a fact about the thread") || object(turn["items"].([]any)[0])["text"] != "second" || observation["turnsPageStatus"] != "summary" || observation["detailTurnsRequested"] != 1 || observation["detailTurnsObserved"] != 0 || !strings.Contains(text(observation["note"]), "arrived for 0 of them") || strings.Contains(text(observation["note"]), "requested and read") {
		t.Fatalf("read=%v", r)
	}
}
func Test_test_a_turn_with_more_items_than_one_page_says_so(t *testing.T) {
	// Python narrows ITEM_PAGE to 3 over 7 items; the Go page is the real constant, so the turn
	// holds ItemPage+4 items to overflow it by the same margin.
	b, host, view := smallFrameThread(t)
	items := []any{}
	for n := range ItemPage + 4 {
		items = append(items, map[string]any{"type": "commandExecution", "command": "ls", "aggregatedOutput": fmt.Sprintf("line %d", n)})
	}
	view.Turns[1]["items"] = items
	r, err := b.ReadThread(context.Background(), "thread-1", 1, nil, 4000)
	if err != nil {
		t.Fatal(err)
	}
	turn := firstTurn(t, r)
	detail := turn["itemsDetail"].([]any)
	note := object(turn["itemsDetailNote"])
	if turn["itemsDetailStatus"] != "partial" || len(detail) != ItemPage || note["more"] != true || note["observed"] != ItemPage || !strings.Contains(text(note["note"]), "most recent items of the turn") || len(turn["items"].([]any)) != 0 || hostParams(t, host, "thread/items/list")["sortDirection"] != "desc" {
		t.Fatalf("turn=%v", turn)
	}
	for i, item := range detail {
		if want := fmt.Sprintf("line %d", i+4); object(item)["aggregatedOutput"] != want {
			t.Fatalf("detail[%d]=%v, want %s", i, item, want)
		}
	}
}
func Test_test_tool_output_inside_item_detail_is_truncated_and_marked(t *testing.T) {
	b, h := readSetup(t)
	h.Respond("thread/items/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"aggregatedOutput": strings.Repeat("y", 5000)}}}})
	r, err := b.ReadThread(context.Background(), "thread-1", 1, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	out := text(object(firstTurn(t, r)["itemsDetail"].([]any)[0])["aggregatedOutput"])
	if !strings.HasPrefix(out, strings.Repeat("y", 100)) || !strings.Contains(out, "truncated; original length 5000") {
		t.Fatalf("output length=%d suffix=%q", len(out), out)
	}
}
func Test_test_a_host_that_cannot_read_items_still_answers_the_read(t *testing.T) {
	b, h := readSetup(t)
	h.Respond("thread/items/list", fakehost.Reply{Error: &fakehost.RPCError{Code: -32601, Message: "thread/items/list"}})
	r, err := b.ReadThread(context.Background(), "thread-1", 1, nil, 4000)
	if err != nil {
		t.Fatal(err)
	}
	turn := firstTurn(t, r)
	if turn["itemsDetailStatus"] != "method_unavailable" || object(turn["itemsDetailNote"])["code"] != -32601 || object(r["observation"])["detailTurnsObserved"] != 0 || object(r["observation"])["detailTurnsRequested"] != 1 || object(turn["items"].([]any)[0])["text"] != "first" || strings.Contains(text(object(r["observation"])["note"]), "requested and read") {
		t.Fatalf("read=%v", r)
	}
	h.Respond("thread/items/list", fakehost.Reply{Error: &fakehost.RPCError{Code: -32000, Message: "nope"}})
	r, err = b.ReadThread(context.Background(), "thread-1", 1, nil, 4000)
	if err != nil {
		t.Fatal(err)
	}
	turn = firstTurn(t, r)
	if turn["itemsDetailStatus"] != "refused" || object(turn["itemsDetailNote"])["message"] != "nope" || object(turn["itemsDetailNote"])["code"] != -32000 {
		t.Fatalf("read=%v", r)
	}
}
func Test_test_a_page_that_never_arrives_is_a_gap_in_the_answer_not_a_failed_read(t *testing.T) {
	b, h := readSetup(t)
	h.Respond("thread/turns/list", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "lost"}})
	r, err := b.ReadThread(context.Background(), "thread-1", 2, nil, 4000)
	if err != nil || r["turnsPage"] != nil || object(r["observation"])["turnsPageStatus"] != "not_observed" || object(r["thread"])["id"] != "thread-1" || h.Count("thread/turns/list") != 1 {
		t.Fatalf("read=%v err=%v calls=%v", r, err, h.Requests())
	}
	attempts := object(r["observation"])["pageAttempts"].([]any)
	if len(attempts) != 1 || !strings.HasPrefix(text(object(attempts[0])["error"]), "TransportError:") || object(attempts[0])["attribution"] != "unestablished" || object(attempts[0])["frameBytes"] != nil {
		t.Fatalf("attempts=%v", attempts)
	}
}
func Test_test_item_detail_that_never_arrives_does_not_fail_a_read_that_did(t *testing.T) {
	b, h := readSetup(t)
	h.Respond("thread/items/list", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "lost"}})
	r, err := b.ReadThread(context.Background(), "thread-1", 1, nil, 4000)
	if err != nil {
		t.Fatal(err)
	}
	turn := firstTurn(t, r)
	if turn["itemsDetailStatus"] != "not_observed" || turn["itemsDetail"] != nil || object(turn["items"].([]any)[0])["text"] != "first" || object(r["observation"])["detailTurnsObserved"] != 0 || object(r["observation"])["detailTurnsRequested"] != 1 || object(r["observation"])["turnsPageStatus"] != "summary" || !strings.Contains(text(object(turn["itemsDetailNote"])["note"]), "none of this is a fact about the thread") || !strings.HasPrefix(text(object(object(turn["itemsDetailNote"])["attempts"].([]any)[0])["error"]), "TransportError:") {
		t.Fatalf("read=%v calls=%v", r, h.Requests())
	}
}
func Test_test_low_text_limit_preserves_page_cursors_and_protocol_fields(t *testing.T) {
	b, _, view := smallFrameThread(t)
	path := "/" + strings.Repeat("directory/", 30)
	itemID := "item-" + strings.Repeat("x", 120)
	view.CWD = path
	view.Turns[0]["items"] = []any{message(strings.Repeat("first", 100))}
	view.Turns[1]["items"] = []any{map[string]any{"type": "userMessage", "id": itemID, "text": strings.Repeat("second", 100)}}
	r, err := b.ReadThread(context.Background(), "thread-1", 1, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	page := object(r["turnsPage"])
	item := object(firstTurn(t, r)["items"].([]any)[0])
	if object(r["thread"])["cwd"] != path || page["nextCursor"] != cursorAt(1) || page["backwardsCursor"] != cursorAt(0) || item["id"] != itemID || !strings.HasPrefix(text(item["text"]), strings.Repeat("second", 16)) || !strings.Contains(text(item["text"]), "truncated") {
		t.Fatalf("read=%v", r)
	}
	second, err := b.ReadThread(context.Background(), "thread-1", 1, page["nextCursor"], 100)
	if err != nil || firstTurn(t, second)["id"] != "turn-1" {
		t.Fatalf("second=%v err=%v", second, err)
	}
}
