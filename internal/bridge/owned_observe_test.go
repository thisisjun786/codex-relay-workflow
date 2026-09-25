package bridge

import (
	"context"
	"encoding/json"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"slices"
	"strings"
	"testing"
)

func Test_test_lost_initial_turn_response_retains_id_without_resend(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	host.Respond("thread/start", startReply(cwd))
	host.Respond("turn/start", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "lost"}})
	in := createInput(cwd, "lost-turn")
	in.Prompt = "hello"
	first, err := b.CreateThread(context.Background(), in)
	if err != nil || first["status"] != "outcome_unknown" || first["threadId"] != "thread-1" {
		t.Fatalf("first=%v err=%v", first, err)
	}
	_, err = b.CreateThread(context.Background(), in)
	if err != nil || host.Count("turn/start") != 1 {
		t.Fatalf("retry err=%v calls=%v", err, host.Requests())
	}
}
func Test_test_environment_mismatch_withholds_prompt(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	wrong := startReply(cwd)
	wrong.Result["sandbox"] = map[string]any{"type": "dangerFullAccess"}
	host.Respond("thread/start", wrong)
	in := createInput(cwd, "mismatch")
	in.Prompt = "hello"
	r, err := b.CreateThread(context.Background(), in)
	if err != nil || r["status"] != "failed" || r["threadId"] != "thread-1" || host.Count("turn/start") != 0 {
		t.Fatalf("receipt=%v err=%v calls=%v", r, err, host.Requests())
	}
}
func Test_test_desktop_project_id_not_found_stops_before_creation(t *testing.T) {
	b, host := testBridge(t)
	host.Respond("project/read", fakehost.Reply{Error: &fakehost.RPCError{Code: -32602, Message: "project not found"}})
	in := createInput(t.TempDir(), "project")
	in.ProjectID = "desktop-id"
	r, err := b.CreateThread(context.Background(), in)
	if err != nil || r["status"] != "failed" || host.Count("thread/start") != 0 {
		t.Fatalf("receipt=%v err=%v calls=%v", r, err, host.Requests())
	}
}
func Test_test_busy_thread_is_not_resumed_or_messaged(t *testing.T) {
	b, host := testBridge(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "active"}}}})
	r, err := b.SendMessageToThread(context.Background(), SendMessage{RequestID: "busy", ThreadID: "thread-1", Message: "hello", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}})
	if err != nil || r["status"] != "failed" || host.Count("thread/resume") != 0 || host.Count("turn/start") != 0 {
		t.Fatalf("receipt=%v err=%v calls=%v", r, err, host.Requests())
	}
}
func Test_test_interactive_approval_policy_withholds_message(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	reply := startReply(cwd)
	reply.Result["approvalPolicy"] = "on-request"
	host.Respond("thread/resume", reply)
	r, err := b.SendMessageToThread(context.Background(), SendMessage{RequestID: "send", ThreadID: "thread-1", Message: "hello", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}})
	if err != nil || r["status"] != "failed" || r["resumed"] == nil || host.Count("turn/start") != 0 {
		t.Fatalf("receipt=%v err=%v calls=%v", r, err, host.Requests())
	}
}
func Test_test_reads_and_waits_do_not_resume_or_use_other_completed_turn(t *testing.T) {
	b, host := testBridge(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1"}}})
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-1", "status": "completed", "items": []any{map[string]any{"text": strings.Repeat("hello", 100)}}}}}})
	host.Respond("thread/items/list", fakehost.Reply{Result: map[string]any{"data": []any{}}})
	read, err := b.ReadThread(context.Background(), "thread-1", 20, nil, 100)
	if err != nil || !strings.Contains(text(object(firstTurn(t, read)["items"].([]any)[0])["text"]), "truncated") {
		t.Fatalf("read=%v err=%v", read, err)
	}
	result, err := b.WaitThread(context.Background(), "thread-1", "not-this-turn", 0)
	if err != nil || result["timedOut"] != true || result["turn"] != nil {
		t.Fatalf("wait=%v err=%v", result, err)
	}
	host.Respond("thread/list", fakehost.Reply{Result: map[string]any{"data": []any{}}})
	if _, err = b.ListThreads(context.Background(), "", 20, nil); err != nil {
		t.Fatal(err)
	}
	for _, method := range hostMethods(host) {
		if !slices.Contains([]string{"thread/read", "thread/turns/list", "thread/items/list", "thread/list"}, method) {
			t.Fatalf("observation made %s: %v", method, host.Requests())
		}
	}
}
func Test_test_history_pagination(t *testing.T) {
	b, host := testBridge(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1"}}})
	host.Script("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-2"}}, "nextCursor": "next"}}, fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-1"}}}})
	host.Respond("thread/items/list", fakehost.Reply{Result: map[string]any{"data": []any{}}})
	first, err := b.ReadThread(context.Background(), "thread-1", 1, nil, 4000)
	if err != nil {
		t.Fatal(err)
	}
	cursor := object(first["turnsPage"])["nextCursor"]
	second, err := b.ReadThread(context.Background(), "thread-1", 1, cursor, 4000)
	if err != nil || object(object(first["turnsPage"])["data"].([]any)[0])["id"] != "turn-2" || object(object(second["turnsPage"])["data"].([]any)[0])["id"] != "turn-1" || hostParams(t, host, "thread/turns/list")["limit"] != float64(1) {
		t.Fatalf("first=%v second=%v err=%v", first, second, err)
	}
	var pages []map[string]any
	for _, req := range host.Requests() {
		if req.Method == "thread/turns/list" {
			var page map[string]any
			if err := json.Unmarshal(req.Params, &page); err != nil {
				t.Fatal(err)
			}
			pages = append(pages, page)
		}
	}
	if len(pages) != 2 || pages[1]["cursor"] != cursor {
		t.Fatalf("pages=%v cursor=%v", pages, cursor)
	}
}
func Test_test_a_read_never_asks_for_the_full_item_view(t *testing.T) {
	b, host := testBridge(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1"}}})
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-1"}}}})
	host.Respond("thread/items/list", fakehost.Reply{Result: map[string]any{"data": []any{}}})
	r, err := b.ReadThread(context.Background(), "thread-1", 20, nil, 4000)
	if err != nil {
		t.Fatal(err)
	}
	observation := object(r["observation"])
	if hostParams(t, host, "thread/read")["includeTurns"] != false || hostParams(t, host, "thread/turns/list")["itemsView"] != "summary" || len(observation) != 5 || observation["detailTurnsObserved"] != 1 || observation["detailTurnsRequested"] != 1 || observation["turnsPageStatus"] != "summary" || observation["itemsView"] != "summary" || !strings.Contains(text(observation["note"]), "bounded observation") || !strings.Contains(text(observation["note"]), "requested and read for the newest 1 turn") {
		t.Fatalf("result=%v calls=%v", r, host.Requests())
	}
	if methods := hostMethods(host); strings.Join(methods, ",") != "thread/read,thread/turns/list,thread/items/list" {
		t.Fatalf("methods=%v", methods)
	}
}
func Test_test_only_the_newest_turn_has_its_items_read(t *testing.T) {
	b, host := testBridge(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1"}}})
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-2"}, map[string]any{"id": "turn-1"}}}})
	host.Respond("thread/items/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"text": "second"}}}})
	r, err := b.ReadThread(context.Background(), "thread-1", 2, nil, 4000)
	if err != nil {
		t.Fatal(err)
	}
	turns := object(r["turnsPage"])["data"].([]any)
	if object(turns[0])["itemsDetailStatus"] != "complete" || object(object(turns[0])["itemsDetail"].([]any)[0])["text"] != "second" || object(turns[1])["itemsDetailStatus"] != "not_requested" || object(turns[1])["itemsDetail"] != nil || host.Count("thread/items/list") != 1 || !strings.Contains(text(object(r["observation"])["note"]), "not_requested") {
		t.Fatalf("read=%v calls=%v", r, host.Requests())
	}
}
