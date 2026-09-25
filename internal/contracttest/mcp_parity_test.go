package contracttest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Test_every_mcp_reply_equals_the_python_servers_whole_json replays the steps recorded by
// internal/bridge/mcp/testdata/gen_mcp_python.py through the built `crw bridge` against the Go
// port of the same FakeServer, and compares each whole reply -- isError, the text content
// (parsed when it is JSON) and structuredContent -- with what the Python server sent: every
// receipt, refusal and read reply the tools produce, success and failure alike.
func Test_every_mcp_reply_equals_the_python_servers_whole_json(t *testing.T) {
	root, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "internal", "bridge", "mcp", "testdata", "mcp_python.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recorded struct {
		Steps   []map[string]any `json:"steps"`
		Results []any            `json:"results"`
	}
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	binary, err := crwBinary()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	caseDir := filepath.Join(home, "case")
	if err := os.Mkdir(caseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	host := startMCPHost(t, nil, nil, false)
	cmd := commandFor(binary, host.server.SocketPath, filepath.Join(caseDir, "state"), home)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	session, err := sdk.NewClient(&sdk.Implementation{Name: "parity", Version: "0"}, nil).Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	for i, step := range recorded.Steps {
		if step["tool"] == "get_active_turn" {
			// The generator makes the thread's newest turn run before this step.
			if err := host.set([]any{"threads", "thread-1", "status"}, map[string]any{"type": "active", "activeFlags": []any{}}); err != nil {
				t.Fatal(err)
			}
			host.mu.Lock()
			turns := host.thread("thread-1")["turns"].([]any)
			asObject(turns[len(turns)-1])["status"] = "inProgress"
			host.mu.Unlock()
		}
		arguments, err := resolve(step["arguments"], nil, caseDir)
		if err != nil {
			t.Fatal(err)
		}
		result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: stringValue(step["tool"]), Arguments: arguments})
		if err != nil {
			t.Fatalf("step %d %v: %v", i, step["tool"], err)
		}
		got := scrubReply(t, result, caseDir, host.server.SocketPath)
		if want := recorded.Results[i]; !reflect.DeepEqual(got, want) {
			gotJSON, _ := json.MarshalIndent(got, "", " ")
			wantJSON, _ := json.MarshalIndent(want, "", " ")
			t.Errorf("step %d %v differs from Python\n got: %s\nwant: %s", i, step["tool"], gotJSON, wantJSON)
		}
	}
}

// scrubReply is gen_mcp_python.py's view of a reply: text content parsed when it is JSON, paths
// replaced by <HOME> and <SOCKET>, wall-clock fields dropped.
func scrubReply(t *testing.T, result *sdk.CallToolResult, home, socket string) any {
	t.Helper()
	content := []any{}
	for _, part := range result.Content {
		text := part.(*sdk.TextContent).Text
		entry := map[string]any{"type": "text"}
		var parsed any
		if json.Unmarshal([]byte(text), &parsed) == nil {
			entry["json"] = scrubValue(parsed, home, socket)
		} else {
			entry["text"] = scrubValue(text, home, socket)
		}
		content = append(content, entry)
	}
	structured, err := jsonValue(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"error": result.IsError, "content": content, "structured": scrubValue(structured, home, socket)}
}

func scrubValue(value any, home, socket string) any {
	switch v := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, item := range v {
			if key != "startedAt" && key != "updatedAt" && key != "at" {
				out[key] = scrubValue(item, home, socket)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = scrubValue(item, home, socket)
		}
		return out
	case string:
		return strings.ReplaceAll(strings.ReplaceAll(v, socket, "<SOCKET>"), home, "<HOME>")
	default:
		return value
	}
}
