package contracttest

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// relayHost supplies given.host through the production unix-socket adapter. Its
// archive/source filters match contract/runner/cli.py's FilteredSocket.
func relayHost(t *testing.T, config map[string]any) *fakehost.Server {
	t.Helper()
	server := fakehost.Start(t)
	threads, _ := config["threads"].(map[string]any)
	var mu sync.Mutex
	turns := 0
	methods := []string{"thread/read", "thread/list", "thread/goal/get", "thread/resume", "thread/turns/list", "thread/items/list", "turn/start"}
	for _, method := range methods {
		server.Handle(method, func(raw json.RawMessage) fakehost.Reply {
			mu.Lock()
			defer mu.Unlock()
			params := map[string]any{}
			if err := json.Unmarshal(raw, &params); err != nil {
				return fakehost.Reply{Error: &fakehost.RPCError{Message: err.Error()}}
			}
			id, _ := params["threadId"].(string)
			thread, _ := threads[id].(map[string]any)
			result := map[string]any{}
			switch method {
			case "thread/list":
				ids := []string{}
				for id := range threads {
					ids = append(ids, id)
				}
				sort.Strings(ids)
				data := []any{}
				for _, id := range ids {
					one := threads[id].(map[string]any)
					archived, _ := one["archived"].(bool)
					if archived != (params["archived"] == true) {
						continue
					}
					if cwd, ok := params["cwd"].(string); ok && cwd != "" && one["cwd"] != cwd {
						continue
					}
					source, _ := one["source"].(string)
					if source == "" {
						source = "cli"
					}
					allowed := source == "cli" || source == "vscode"
					if kinds, ok := params["sourceKinds"].([]any); ok && len(kinds) > 0 {
						allowed = false
						for _, kind := range kinds {
							allowed = allowed || kind == source
						}
					}
					if allowed {
						data = append(data, map[string]any{"id": id})
					}
				}
				start := 0
				if c, ok := params["cursor"].(string); ok {
					start, _ = strconv.Atoi(c)
				}
				limit := 50
				if n, ok := params["limit"].(float64); ok {
					limit = int(n)
				}
				end := min(start+limit, len(data))
				var cursor any
				if end < len(data) {
					cursor = strconv.Itoa(end)
				}
				result = map[string]any{"data": data[min(start, len(data)):end], "nextCursor": cursor}
			case "thread/goal/get":
				result["goal"] = thread["goal"]
			case "thread/read":
				if thread == nil {
					return fakehost.Reply{Error: &fakehost.RPCError{Message: "unknown thread"}}
				}
				result["thread"] = thread
			case "thread/resume":
				settings, _ := thread["settings"].(map[string]any)
				for k, v := range settings {
					result[k] = v
				}
				resumed := map[string]any{}
				for k, v := range thread {
					resumed[k] = v
				}
				if _, ok := resumed["environments"]; !ok {
					env := settings["environments"]
					if env == nil {
						env = []any{}
					}
					resumed["environments"] = env
				}
				result["thread"] = resumed
			case "thread/turns/list":
				data := thread["turns"]
				if data == nil {
					data = []any{}
				}
				result = map[string]any{"data": data, "nextCursor": nil}
			case "thread/items/list":
				result = map[string]any{"data": []any{}, "nextCursor": nil}
			case "turn/start":
				turns++
				turn := map[string]any{"id": fmt.Sprintf("turn-%d", turns), "status": "inProgress"}
				result["turn"] = turn
				prior, _ := thread["turns"].([]any)
				thread["turns"] = append([]any{turn}, prior...)
			}
			return fakehost.Reply{Result: result}
		})
	}
	return server
}
