package contracttest

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// mcpHost is the Python suite's FakeServer (packages/codex-thread-bridge/tests/conftest.py)
// for the methods the mcp-tools corpus reaches, served over fakehost's real unix websocket.
// Its state is a JSON-shaped map so a fixture's host_set step addresses it exactly as the
// Python runner addresses the fake's attributes.
type mcpHost struct {
	server *fakehost.Server
	mu     sync.Mutex
	state  map[string]any
	// jsonCursors selects the given.host.cursor_format "json_turns" cursors.
	jsonCursors bool
	// order is the threads dict's insertion order, which thread/list reports as Python's
	// FakeServer does (self.threads.values()).
	order []string
}

// startMCPHost starts the fake with given.host's members. threadOrder is the key order of
// given.host.threads as the fixture file writes it, since a decoded map has lost it.
func startMCPHost(t *testing.T, config map[string]any, threadOrder []string, jsonCursors bool) *mcpHost {
	h := &mcpHost{server: fakehost.Start(t), jsonCursors: jsonCursors, state: map[string]any{
		"threads": map[string]any{}, "complete_turns": true, "goal": nil, "cursor_padding": float64(160),
	}}
	for key, value := range config {
		h.state[key] = value
	}
	for _, id := range threadOrder {
		if _, present := h.threads()[id]; present {
			h.order = append(h.order, id)
		}
	}
	if len(h.order) != len(h.threads()) {
		t.Fatalf("given.host.threads order %v does not name every thread", threadOrder)
	}
	for _, method := range []string{"thread/start", "thread/name/set", "turn/start", "thread/read", "thread/resume", "thread/turns/list", "thread/items/list", "thread/list", "thread/goal/get", "turn/steer", "thread/goal/set", "project/read"} {
		h.server.Handle(method, func(raw json.RawMessage) fakehost.Reply {
			var params map[string]any
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &params); err != nil {
					return fakehost.Reply{Error: &fakehost.RPCError{Code: -32602, Message: err.Error()}}
				}
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			reply := h.answer(method, params)
			// FakeServer interleaves a notification ahead of every response.
			reply.Before = []fakehost.Notification{{Method: "thread/status/changed", Params: map[string]any{}}}
			return reply
		})
	}
	return h
}

// set is a host_set step: the path walks the fake's state, the last key is assigned. A thread
// added this way joins the end of the insertion order, as a new dict key does in Python.
func (h *mcpHost) set(path []any, value any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(path) == 2 && path[0] == "threads" {
		if _, present := h.threads()[fmt.Sprint(path[1])]; !present {
			h.order = append(h.order, fmt.Sprint(path[1]))
		}
	}
	target := h.state
	for _, key := range path[:len(path)-1] {
		next, ok := target[fmt.Sprint(key)].(map[string]any)
		if !ok {
			return fmt.Errorf("%w: host_set path %v", ErrFixture, path)
		}
		target = next
	}
	target[fmt.Sprint(path[len(path)-1])] = value
	return nil
}

func (h *mcpHost) cursor(offset int) string {
	if h.jsonCursors {
		// json.dumps({"offset": n, "scope": {"kind": "turns"}}) with Python's separators.
		return fmt.Sprintf(`{"offset": %d, "scope": {"kind": "turns"}}`, offset)
	}
	padding, _ := h.state["cursor_padding"].(float64)
	return strconv.Itoa(offset) + ":" + strings.Repeat("x", int(padding))
}

func (h *mcpHost) offset(cursor any) (int, bool) {
	text, ok := cursor.(string)
	if cursor == nil {
		return 0, true
	}
	if !ok {
		return 0, false
	}
	if h.jsonCursors {
		var decoded struct {
			Offset int `json:"offset"`
		}
		return decoded.Offset, json.Unmarshal([]byte(text), &decoded) == nil
	}
	head, _, _ := strings.Cut(text, ":")
	offset, err := strconv.Atoi(head)
	return offset, err == nil && text == h.cursor(offset)
}

func (h *mcpHost) threads() map[string]any { return asObject(h.state["threads"]) }

func (h *mcpHost) thread(id any) map[string]any { return asObject(h.threads()[stringValue(id)]) }

func shallow(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

// settingsView is FakeServer.settings_view: what the host reports about a thread's settings.
func settingsView(params map[string]any) map[string]any {
	config := asObject(params["config"])
	workspace := asObject(config["sandbox_workspace_write"])
	mode := map[string]string{"read-only": "readOnly", "workspace-write": "workspaceWrite", "danger-full-access": "dangerFullAccess"}[stringOr(params["sandbox"], "read-only")]
	var sandbox map[string]any
	switch mode {
	case "workspaceWrite":
		sandbox = map[string]any{"type": mode, "writableRoots": valueOr(workspace["writable_roots"], []any{}), "networkAccess": valueOr(workspace["network_access"], false), "excludeTmpdirEnvVar": valueOr(workspace["exclude_tmpdir_env_var"], false), "excludeSlashTmp": valueOr(workspace["exclude_slash_tmp"], false)}
	case "readOnly":
		sandbox = map[string]any{"type": mode, "networkAccess": false}
	default:
		sandbox = map[string]any{"type": mode}
	}
	return map[string]any{
		"cwd": params["cwd"], "runtimeWorkspaceRoots": valueOr(params["runtimeWorkspaceRoots"], []any{params["cwd"]}),
		"approvalPolicy": "never", "sandbox": sandbox, "model": valueOr(params["model"], "configured-default"),
		"reasoningEffort": valueOr(config["model_reasoning_effort"], "medium"),
	}
}

func valueOr(value, fallback any) any {
	if value == nil {
		return fallback
	}
	return value
}

func stringOr(value any, fallback string) string {
	if text, ok := value.(string); ok {
		return text
	}
	return fallback
}

func turnView(turn map[string]any, view string) map[string]any {
	items, _ := turn["items"].([]any)
	kept := []any{}
	for _, item := range items {
		kind := asObject(item)["type"]
		if view == "full" || (view == "summary" && (kind == "userMessage" || kind == "agentMessage")) {
			kept = append(kept, item)
		}
	}
	out := shallow(turn)
	out["items"], out["itemsView"] = kept, view
	return out
}

func page(h *mcpHost, all []any, start int, limit any) map[string]any {
	end := start + int(valueOr(limit, float64(len(all))).(float64))
	window := []any{}
	for i := start; i < end && i < len(all); i++ {
		window = append(window, all[i])
	}
	var next any
	if end < len(all) {
		next = h.cursor(end)
	}
	return map[string]any{"data": window, "nextCursor": next}
}

func (h *mcpHost) answer(method string, params map[string]any) fakehost.Reply {
	result := map[string]any{}
	switch method {
	case "thread/start":
		id := fmt.Sprintf("thread-%d", len(h.threads())+1)
		thread := map[string]any{"id": id, "cwd": params["cwd"], "status": map[string]any{"type": "idle"}, "turns": []any{}}
		if params["projectId"] != nil {
			thread["projectId"] = params["projectId"]
		}
		h.threads()[id] = thread
		h.order = append(h.order, id)
		view := settingsView(params)
		thread["settings"], thread["model"], thread["reasoningEffort"] = view, view["model"], view["reasoningEffort"]
		for key, value := range view {
			result[key] = value
		}
		reported := shallow(thread)
		delete(reported, "settings")
		result["thread"] = reported
	case "thread/name/set":
		h.thread(params["threadId"])["name"] = params["name"]
	case "turn/start":
		thread := h.thread(params["threadId"])
		turns, _ := thread["turns"].([]any)
		status := "completed"
		if h.state["complete_turns"] == false {
			status = "inProgress"
		}
		input, _ := params["input"].([]any)
		var text any
		if len(input) > 0 {
			text = asObject(input[0])["text"]
		}
		turn := map[string]any{"id": fmt.Sprintf("turn-%d", len(turns)+1), "status": status, "items": []any{map[string]any{"type": "agentMessage", "text": text}}}
		thread["turns"] = append(turns, turn)
		result["turn"] = turn
	case "thread/read":
		thread := shallow(h.thread(params["threadId"]))
		if params["includeTurns"] != true {
			thread["turns"] = []any{}
		}
		result["thread"] = thread
	case "thread/resume":
		thread := shallow(h.thread(params["threadId"]))
		retained := shallow(asObject(thread["settings"]))
		retained["approvalPolicy"], retained["approvalsReviewer"] = "never", "user"
		thread["turns"] = []any{}
		result = retained
		result["thread"] = thread
	case "thread/turns/list":
		stored, _ := h.thread(params["threadId"])["turns"].([]any)
		start, ok := h.offset(params["cursor"])
		if !ok {
			return fakehost.Reply{Error: &fakehost.RPCError{Code: -32602, Message: "invalid cursor"}}
		}
		view := stringOr(params["itemsView"], "full")
		turns := []any{}
		for _, turn := range slices.Backward(stored) {
			turns = append(turns, turnView(asObject(turn), view))
		}
		result = page(h, turns, start, params["limit"])
		result["backwardsCursor"] = h.cursor(start)
	case "thread/items/list":
		items := []any{}
		stored, _ := h.thread(params["threadId"])["turns"].([]any)
		for _, turn := range stored {
			if params["turnId"] == nil || params["turnId"] == asObject(turn)["id"] {
				turnItems, _ := asObject(turn)["items"].([]any)
				items = append(items, turnItems...)
			}
		}
		if params["sortDirection"] == "desc" {
			slices.Reverse(items)
		}
		start, _ := h.offset(params["cursor"])
		result = page(h, items, start, params["limit"])
		result["backwardsCursor"] = h.cursor(start)
	case "thread/list":
		threads := []any{}
		for _, name := range h.order {
			thread := shallow(h.thread(name))
			thread["turns"] = []any{}
			threads = append(threads, thread)
		}
		start, _ := h.offset(params["cursor"])
		result = page(h, threads, start, params["limit"])
	case "thread/goal/get":
		result["goal"] = h.state["goal"]
	case "turn/steer":
		result["turnId"] = params["expectedTurnId"]
	case "thread/goal/set":
		updated := shallow(asObject(h.state["goal"]))
		if params["status"] != nil {
			updated["status"] = params["status"]
		}
		h.state["goal"] = updated
		result["goal"] = updated
	case "project/read":
		result["project"] = map[string]any{"id": params["projectId"]}
	}
	return fakehost.Reply{Result: result}
}

// calls is FakeServer.calls as the Python mcp runner reports it: [method, params] pairs,
// restricted to the methods its observation keeps.
func (h *mcpHost) calls(t *testing.T) []any {
	kept := map[string]bool{"thread/start": true, "thread/resume": true, "turn/start": true, "thread/turns/list": true, "thread/list": true, "turn/steer": true}
	out := []any{}
	for _, request := range h.server.Requests() {
		if !kept[request.Method] {
			continue
		}
		var params any = map[string]any{}
		if len(request.Params) > 0 {
			if err := json.Unmarshal(request.Params, &params); err != nil {
				t.Fatal(err)
			}
		}
		out = append(out, []any{request.Method, params})
	}
	return out
}
