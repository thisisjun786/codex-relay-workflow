package contracttest

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
)

// The Python FakeServer lists threads in dict insertion order: given threads in the order the
// fixture writes them, then created ones, then any host_set adds. "thread-10" sorts before
// "thread-2" by name, so a name sort cannot pass this.
func Test_the_mcp_fake_host_lists_threads_in_insertion_order(t *testing.T) {
	given := map[string]any{"threads": map[string]any{
		"thread-2":  map[string]any{"id": "thread-2", "cwd": "/w", "status": map[string]any{"type": "idle"}, "turns": []any{}},
		"thread-10": map[string]any{"id": "thread-10", "cwd": "/w", "status": map[string]any{"type": "idle"}, "turns": []any{}},
	}}
	host := startMCPHost(t, given, []string{"thread-2", "thread-10"}, false)
	if err := host.set([]any{"threads", "b-late"}, map[string]any{"id": "b-late", "status": map[string]any{"type": "idle"}, "turns": []any{}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	client, err := appserver.Dial(ctx, host.server.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Call(ctx, "thread/start", map[string]any{"cwd": "/w"}); err != nil {
		t.Fatal(err)
	}
	raw, err := client.Call(ctx, "thread/list", map[string]any{"limit": 10})
	if err != nil {
		t.Fatal(err)
	}
	listed := asObject(mustDecode(t, raw))
	ids := []any{}
	for _, thread := range listed["data"].([]any) {
		ids = append(ids, asObject(thread)["id"])
	}
	// thread/start numbers from len(threads)+1, so the created thread is thread-4.
	if want := []any{"thread-2", "thread-10", "b-late", "thread-4"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("listed %v, want %v", ids, want)
	}
}

func mustDecode(t *testing.T, raw []byte) any {
	t.Helper()
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
