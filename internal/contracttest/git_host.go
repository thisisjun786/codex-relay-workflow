package contracttest

import (
	"encoding/json"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func (r *gitBridgeRun) startReply() map[string]any {
	return map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": r.args.Destination, "status": map[string]any{"type": "idle"}, "turns": []any{}, "model": r.args.Model, "reasoningEffort": r.args.Effort}, "cwd": r.args.Destination, "model": r.args.Model, "reasoningEffort": r.args.Effort, "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "runtimeWorkspaceRoots": []any{r.args.Destination}}
}

func (r *gitBridgeRun) configureHost() {
	given := asObject(r.given["host"])
	r.host.Respond("project/read", fakehost.Reply{Result: map[string]any{"project": map[string]any{"id": "project-1"}}})
	r.host.Handle("thread/name/set", func(_ json.RawMessage) fakehost.Reply {
		if asObject(r.given["host"])["drop_after"] == "thread/name/set" {
			return fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "lost"}}
		}
		return fakehost.Reply{}
	})
	r.host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": nil}})
	r.host.Respond("thread/items/list", fakehost.Reply{Result: map[string]any{"data": []any{}}})
	// Answered from the turns this fake recorded, newest first, so a thread view that expects no
	// turns fails if a turn was in fact started.
	r.host.Handle("thread/turns/list", func(raw json.RawMessage) fakehost.Reply {
		var params map[string]any
		if err := json.Unmarshal(raw, &params); err != nil {
			r.t.Error(err)
			return fakehost.Reply{}
		}
		turns, _ := asObject(r.threads[stringValue(params["threadId"])])["turns"].([]any)
		data := []any{}
		for i := len(turns) - 1; i >= 0; i-- {
			data = append(data, turns[i])
		}
		return fakehost.Reply{Result: map[string]any{"data": data}}
	})
	r.host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": r.args.Destination, "model": r.args.Model, "reasoningEffort": r.args.Effort, "status": map[string]any{"type": "idle"}}}})
	r.host.Handle("thread/start", func(raw json.RawMessage) fakehost.Reply {
		var params map[string]any
		if err := json.Unmarshal(raw, &params); err != nil {
			r.t.Error(err)
			return fakehost.Reply{}
		}
		if reject := asObject(asObject(given["reject"])["thread/start"]); reject != nil {
			return fakehost.Reply{ErrorObject: reject}
		}
		settings := map[string]any{"cwd": params["cwd"], "model": params["model"], "reasoningEffort": asObject(params["config"])["model_reasoning_effort"], "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "runtimeWorkspaceRoots": []any{params["cwd"]}}
		// Numbered like the Python fake, so a second creation is a second thread, not a rewrite.
		id := fmt.Sprintf("thread-%d", len(r.threads)+1)
		thread := map[string]any{"id": id, "cwd": params["cwd"], "status": map[string]any{"type": "idle"}, "turns": []any{}, "model": settings["model"], "reasoningEffort": settings["reasoningEffort"]}
		r.threads[id] = thread
		created := map[string]any{"thread": thread}
		for key, value := range settings {
			created[key] = value
		}
		for key, value := range asObject(given["override_creation"]) {
			created[key] = value
		}
		if asObject(r.given["host"])["drop_after"] == "thread/start" {
			return fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "lost"}}
		}
		return fakehost.Reply{Result: created}
	})
	r.host.Handle("turn/start", func(raw json.RawMessage) fakehost.Reply {
		if reject := asObject(asObject(given["reject"])["turn/start"]); reject != nil {
			return fakehost.Reply{ErrorObject: reject}
		}
		var params map[string]any
		if err := json.Unmarshal(raw, &params); err != nil {
			r.t.Error(err)
			return fakehost.Reply{}
		}
		thread := asObject(r.threads[stringValue(params["threadId"])])
		items := []any{map[string]any{"type": "agentMessage", "text": asObject(params["input"].([]any)[0])["text"]}}
		turn := map[string]any{"id": "turn-1", "status": "completed", "items": items}
		thread["turns"] = append(thread["turns"].([]any), turn)
		if asObject(r.given["host"])["drop_after"] == "turn/start" {
			return fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "lost"}}
		}
		return fakehost.Reply{Result: map[string]any{"turn": turn}}
	})
}
