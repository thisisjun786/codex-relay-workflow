package bridge

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

// smallFrame mirrors the Python small_frame_bridge: a 64 KiB frame limit, so 100 KiB overflows.
const smallFrame, overflow = 64 * 1024, 100 * 1024

// threadView is the Python fake's stateful thread: turns held oldest first, pages answered from
// the request's own limit, cursor, itemsView and sortDirection rather than from a script.
type threadView struct {
	CWD   string
	Turns []map[string]any
	// Oversize pads the answer to method when it returns a positive size for these params.
	Oversize func(method string, params map[string]any) int
}

func cursorAt(offset int) string { return strconv.Itoa(offset) + ":" }
func offsetOf(params map[string]any) int {
	cursor, _ := params["cursor"].(string)
	offset, _ := strconv.Atoi(strings.TrimSuffix(cursor, ":"))
	return offset
}
func page(data []any, params map[string]any) map[string]any {
	start := offsetOf(params)
	end := len(data)
	if limit, ok := params["limit"].(float64); ok && start+int(limit) < end {
		end = start + int(limit)
	}
	result := map[string]any{"data": data[min(start, len(data)):end], "nextCursor": nil, "backwardsCursor": cursorAt(start)}
	if end < len(data) {
		result["nextCursor"] = cursorAt(end)
	}
	return result
}
func (v *threadView) answer(method string, raw json.RawMessage) fakehost.Reply {
	var params map[string]any
	if err := json.Unmarshal(raw, &params); err != nil {
		return fakehost.Reply{Error: &fakehost.RPCError{Code: -32602, Message: err.Error()}}
	}
	var result map[string]any
	switch method {
	case "thread/read":
		result = map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": v.CWD, "status": map[string]any{"type": "idle"}}}
	case "thread/turns/list":
		turns := []any{}
		for i := len(v.Turns) - 1; i >= 0; i-- {
			view := map[string]any{"itemsView": params["itemsView"]}
			items := []any{}
			for key, value := range v.Turns[i] {
				view[key] = value
			}
			for _, item := range v.Turns[i]["items"].([]any) {
				kind := object(item)["type"]
				if params["itemsView"] == "summary" && (kind == "userMessage" || kind == "agentMessage") {
					items = append(items, item)
				}
			}
			view["items"] = items
			turns = append(turns, view)
		}
		result = page(turns, params)
	case "thread/items/list":
		items := []any{}
		for _, turn := range v.Turns {
			if turn["id"] == params["turnId"] {
				items = append(items, turn["items"].([]any)...)
			}
		}
		if params["sortDirection"] == "desc" {
			for l, r := 0, len(items)-1; l < r; l, r = l+1, r-1 {
				items[l], items[r] = items[r], items[l]
			}
		}
		result = page(items, params)
	default:
		return fakehost.Reply{Error: &fakehost.RPCError{Code: -32601, Message: method}}
	}
	if v.Oversize != nil {
		return fakehost.Reply{Result: result, PadBytes: v.Oversize(method, params)}
	}
	return fakehost.Reply{Result: result}
}
func message(text string) map[string]any {
	return map[string]any{"type": "userMessage", "id": "item-" + text, "text": text}
}

// smallFrameThread is the Python create("first") then send("second") thread behind a bridge
// whose frame limit is smallFrame.
func smallFrameThread(t *testing.T) (*Bridge, *fakehost.Server, *threadView) {
	t.Helper()
	host := fakehost.Start(t)
	view := &threadView{CWD: "/preserved", Turns: []map[string]any{
		{"id": "turn-1", "status": "completed", "items": []any{message("first")}},
		{"id": "turn-2", "status": "completed", "items": []any{message("second")}},
	}}
	for _, method := range []string{"thread/read", "thread/turns/list", "thread/items/list"} {
		host.Handle(method, func(raw json.RawMessage) fakehost.Reply { return view.answer(method, raw) })
	}
	client := appserver.New(host.SocketPath, appserver.DefaultBounds).LimitFrames(smallFrame)
	t.Cleanup(func() { _ = client.Close() })
	store, err := ledger.Open(filepath.Join(t.TempDir(), "operations.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return New(client, store, executionPolicy()), host, view
}
func turnIDs(t *testing.T, read map[string]any) string {
	t.Helper()
	ids := []string{}
	for _, turn := range object(read["turnsPage"])["data"].([]any) {
		ids = append(ids, fmt.Sprint(object(turn)["id"]))
	}
	return strings.Join(ids, ",")
}
func allItemsEmpty(read map[string]any) bool {
	for _, turn := range object(read["turnsPage"])["data"].([]any) {
		if items, _ := object(turn)["items"].([]any); len(items) != 0 {
			return false
		}
	}
	return true
}
