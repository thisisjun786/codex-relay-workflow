package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// oversizedFrame is past the real 16 MiB client limit, for bridges built at the default size.
const oversizedFrame = 17 * 1024 * 1024

func Test_test_a_page_too_large_is_narrowed_and_then_dropped_to_ids(t *testing.T) {
	b, _, view := smallFrameThread(t)
	view.Oversize = func(method string, params map[string]any) int {
		if limit, _ := params["limit"].(float64); method == "thread/turns/list" && limit > 1 {
			return overflow
		}
		return 0
	}
	narrowed, err := b.ReadThread(context.Background(), "thread-1", 2, nil, 4000)
	if err != nil {
		t.Fatal(err)
	}
	observation := object(narrowed["observation"])
	attempt := object(observation["pageAttempts"].([]any)[0])
	requested := object(attempt["requested"])
	if observation["turnsPageStatus"] != "summary_narrowed" || observation["itemsView"] != "summary" || len(object(narrowed["turnsPage"])["data"].([]any)) != 1 || len(requested) != 2 || requested["itemsView"] != "summary" || requested["limit"] != 2 || attempt["attribution"] != "unestablished" || !strings.Contains(text(attempt["note"]), "not established") || attempt["frameBytes"].(int) <= attempt["limit"].(int) {
		t.Fatalf("narrowed=%v", narrowed)
	}

	view.Oversize = func(method string, params map[string]any) int {
		if method == "thread/turns/list" && params["itemsView"] == "summary" {
			return overflow
		}
		return 0
	}
	ids, err := b.ReadThread(context.Background(), "thread-1", 2, nil, 4000)
	if err != nil {
		t.Fatal(err)
	}
	observation = object(ids["observation"])
	newest := firstTurn(t, ids)
	if observation["turnsPageStatus"] != "not_loaded" || observation["itemsView"] != "notLoaded" || turnIDs(t, ids) != "turn-2,turn-1" || !allItemsEmpty(ids) || !strings.Contains(text(observation["note"]), "every turn's items field is empty") || observation["detailTurnsObserved"] != 1 || newest["itemsDetailStatus"] != "complete" || object(newest["itemsDetail"].([]any)[0])["text"] != "second" {
		t.Fatalf("ids=%v", ids)
	}

	view.Oversize = func(method string, _ map[string]any) int {
		if method == "thread/turns/list" {
			return overflow
		}
		return 0
	}
	nothing, err := b.ReadThread(context.Background(), "thread-1", 2, nil, 4000)
	if err != nil {
		t.Fatal(err)
	}
	observation = object(nothing["observation"])
	if nothing["turnsPage"] != nil || object(nothing["thread"])["id"] != "thread-1" || observation["turnsPageStatus"] != "not_observed" || len(observation["pageAttempts"].([]any)) != 3 || observation["itemsView"] != nil || observation["detailTurnsRequested"] != 0 || observation["detailTurnsObserved"] != 0 {
		t.Fatalf("nothing=%v", nothing)
	}
}

func Test_test_an_item_page_that_will_not_arrive_is_asked_again_smaller(t *testing.T) {
	b, _, view := smallFrameThread(t)
	view.Oversize = func(method string, params map[string]any) int {
		if limit, _ := params["limit"].(float64); method == "thread/items/list" && limit > 1 {
			return overflow
		}
		return 0
	}
	read, err := b.ReadThread(context.Background(), "thread-1", 1, nil, 4000)
	if err != nil {
		t.Fatal(err)
	}
	turn := firstTurn(t, read)
	note := object(turn["itemsDetailNote"])
	if turn["itemsDetailStatus"] != "narrowed" || note["requestedLimit"] != 10 || note["observedLimit"] != 1 || object(note["attempts"].([]any)[0])["attribution"] != "unestablished" || len(turn["itemsDetail"].([]any)) != 1 {
		t.Fatalf("read=%v", read)
	}
}

func Test_test_a_mutation_caught_in_someone_elses_oversized_frame_is_unknown(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	host.Respond("thread/start", startReply(cwd))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	host.Respond("thread/resume", startReply(cwd))
	input := createInput(cwd, "create")
	input.Prompt = "first"
	if receipt, err := b.CreateThread(context.Background(), input); err != nil || receipt["status"] != "accepted" {
		t.Fatalf("create=%v err=%v", receipt, err)
	}
	started := host.Count("turn/start")
	host.Script("turn/start", fakehost.Reply{Before: []fakehost.Notification{{Method: "thread/status/changed", Params: map[string]any{"padding": strings.Repeat("x", oversizedFrame)}}}})
	message := SendMessage{RequestID: "send", ThreadID: "thread-1", Message: "second", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}}
	receipt, err := b.SendMessageToThread(context.Background(), message)
	if err != nil || receipt["status"] != "outcome_unknown" || receipt["retrySafe"] != false || len(receipt["attemptedEffects"].([]string)) == 0 || !strings.HasPrefix(text(receipt["error"]), "ResponseTooLarge:") || !strings.Contains(text(receipt["error"]), "cannot be attributed to a request") || host.Count("turn/start") != started+1 {
		t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, host.Requests())
	}
}

func Test_test_a_frame_belonging_to_nobody_can_drive_the_ladder_down(t *testing.T) {
	b, host, _ := smallFrameThread(t)
	nobody := fakehost.Reply{Before: []fakehost.Notification{{Method: "thread/status/changed", Params: map[string]any{"padding": strings.Repeat("x", overflow)}}}}
	host.Script("thread/turns/list", nobody, nobody)
	read, err := b.ReadThread(context.Background(), "thread-1", 2, nil, 4000)
	if err != nil {
		t.Fatal(err)
	}
	observation := object(read["observation"])
	note := text(observation["note"])
	if observation["turnsPageStatus"] != "not_loaded" || observation["itemsView"] != "notLoaded" || turnIDs(t, read) != "turn-2,turn-1" || !allItemsEmpty(read) || firstTurn(t, read)["itemsDetailStatus"] != "complete" || observation["detailTurnsObserved"] != 1 {
		t.Fatalf("read=%v", read)
	}
	for _, value := range observation["pageAttempts"].([]any) {
		if object(value)["attribution"] != "unestablished" {
			t.Fatalf("attempt=%v", value)
		}
	}
	if !strings.Contains(note, "no page carrying items was received") || strings.Contains(note, "would fit") || !strings.Contains(note, "does not say those pages were too large") {
		t.Fatalf("observation=%v", observation)
	}
}
