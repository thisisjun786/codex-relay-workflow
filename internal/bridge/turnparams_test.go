package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// CRW-1193. The typed builder names exactly the TurnStartParams members a flow backend sets, and
// leaves every optional member that has no value out of the frame: a member the host reads as an
// override ("for this turn and subsequent turns") must never be sent as an empty string or null.
func TestTurnStartParamsOptionalFieldsOmitted(t *testing.T) {
	// Given: only the two required members.
	params, err := TurnStartParams{ThreadID: "thread-1", Input: []UserInput{TextInput("hello")}}.Map()
	if err != nil {
		t.Fatal(err)
	}
	// Then: nothing else is present, not even as an empty value.
	if got := sortedKeys(params); len(got) != 2 || got[0] != "input" || got[1] != "threadId" {
		t.Fatalf("members = %v", got)
	}

	// Given: every optional member set.
	full, err := TurnStartParams{
		ThreadID:      "thread-1",
		Input:         []UserInput{TextInput("hello")},
		Model:         "gpt-x",
		Effort:        "high",
		OutputSchema:  map[string]any{"type": "object", "additionalProperties": false},
		CWD:           "/work/tree",
		SandboxPolicy: map[string]any{"type": "readOnly", "networkAccess": false},
	}.Map()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cwd", "effort", "input", "model", "outputSchema", "sandboxPolicy", "threadId"}
	if got := sortedKeys(full); len(got) != len(want) {
		t.Fatalf("members = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("members = %v, want %v", got, want)
			}
		}
	}
	raw, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	const wantBytes = `{"cwd":"/work/tree","effort":"high","input":[{"text":"hello","type":"text"}],"model":"gpt-x","outputSchema":{"additionalProperties":false,"type":"object"},"sandboxPolicy":{"networkAccess":false,"type":"readOnly"},"threadId":"thread-1"}`
	if string(raw) != wantBytes {
		t.Fatalf("frame params:\n got %s\nwant %s", raw, wantBytes)
	}

	// Each optional member alone, at its zero value, stays out.
	for name, p := range map[string]TurnStartParams{
		"model":  {ThreadID: "t", Input: []UserInput{TextInput("x")}, Model: ""},
		"effort": {ThreadID: "t", Input: []UserInput{TextInput("x")}, Effort: ""},
		"schema": {ThreadID: "t", Input: []UserInput{TextInput("x")}, OutputSchema: nil},
		"cwd":    {ThreadID: "t", Input: []UserInput{TextInput("x")}, CWD: ""},
		"policy": {ThreadID: "t", Input: []UserInput{TextInput("x")}, SandboxPolicy: nil},
	} {
		m, err := p.Map()
		if err != nil || len(m) != 2 {
			t.Errorf("%s: %v, %v", name, m, err)
		}
	}
}

func TestTurnStartParamsRefusesWhatTheHostWouldReject(t *testing.T) {
	text := []UserInput{TextInput("x")}
	for name, p := range map[string]TurnStartParams{
		"no thread":       {Input: text},
		"no input":        {ThreadID: "t"},
		"empty text":      {ThreadID: "t", Input: []UserInput{TextInput("")}},
		"unknown input":   {ThreadID: "t", Input: []UserInput{{Type: "image"}}},
		"relative cwd":    {ThreadID: "t", Input: text, CWD: "tree"},
		"loose policy":    {ThreadID: "t", Input: text, SandboxPolicy: map[string]any{"type": "readOnly"}},
		"unmarshalable":   {ThreadID: "t", Input: text, OutputSchema: map[string]any{"bad": make(chan int)}},
		"policy relative": {ThreadID: "t", Input: text, SandboxPolicy: map[string]any{"type": "workspaceWrite", "networkAccess": false, "writableRoots": []any{"rel"}, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}},
	} {
		if m, err := p.Map(); err == nil {
			t.Errorf("%s: accepted %v", name, m)
		} else {
			var invalid *Invalid
			if !errors.As(err, &invalid) {
				t.Errorf("%s: error %T is not an Invalid", name, err)
			}
		}
	}
}

// CRW-1193. The builder replaces nothing that is in use: the frames the create path and the send
// path write today are byte-for-byte what the builder renders for the same thread and message.
func TestExistingTurnStartBytesUnchanged(t *testing.T) {
	const prompt = "  exact <&> \"message\"\nline  "
	built, err := TurnStartParams{ThreadID: "thread-1", Input: []UserInput{TextInput(prompt)}}.Map()
	if err != nil {
		t.Fatal(err)
	}
	builtBytes, err := json.Marshal(built)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := json.Marshal(map[string]any{"threadId": "thread-1", "input": []any{map[string]any{"type": "text", "text": prompt}}})
	if err != nil {
		t.Fatal(err)
	}
	if string(builtBytes) != string(legacy) {
		t.Fatalf("builder %s != legacy %s", builtBytes, legacy)
	}

	// The frames the bridge really writes: the create path's prompt and the send path's message.
	b, host := testBridge(t)
	cwd := t.TempDir()
	host.Respond("thread/start", startReply(cwd))
	host.Respond("thread/name/set", fakehost.Reply{})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	host.Respond("thread/resume", startReply(cwd))
	in := createInput(cwd, "bytes")
	in.Prompt = prompt
	created, err := b.CreateThread(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := b.SendMessageToThread(context.Background(), SendMessage{RequestID: "bytes-send", ThreadID: "thread-1", Message: prompt, Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}})
	if err != nil || sent["status"] != "accepted" || created["status"] != "accepted" {
		t.Fatalf("create %v send %v err %v", created, sent, err)
	}
	var frames []string
	for _, r := range host.Requests() {
		if r.Method == "turn/start" {
			frames = append(frames, string(r.Params))
		}
	}
	if len(frames) != 2 {
		t.Fatalf("turn/start frames = %v", frames)
	}
	for i, frame := range frames {
		if frame != string(builtBytes) {
			t.Errorf("frame %d:\n got %s\nwant %s", i, frame, builtBytes)
		}
	}
}

// ---- the generated schema -------------------------------------------------------------------------

const schemaDir = "appserver/testdata/schema-0.154"
const fixtureDir = "appserver/testdata/fixtures"

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return value
}

// definitionSchema is the named definition of a generated bundle (ClientRequest.json and the like),
// resolved with the bundle's other definitions.
func definitionSchema(t *testing.T, bundle map[string]any, name string) *jsonschema.Resolved {
	t.Helper()
	return resolveSchema(t, map[string]any{"definitions": bundle["definitions"], "$ref": "#/definitions/" + name})
}

func resolveSchema(t *testing.T, schema map[string]any) *jsonschema.Resolved {
	t.Helper()
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// methodParams maps a bundle's methods to the definition their params are.
func methodParams(t *testing.T, bundle map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, entry := range bundle["oneOf"].([]any) {
		props := entry.(map[string]any)["properties"].(map[string]any)
		method := props["method"].(map[string]any)["enum"].([]any)[0].(string)
		if params, ok := props["params"].(map[string]any); ok {
			if ref, ok := params["$ref"].(string); ok {
				out[method] = strings.TrimPrefix(ref, "#/definitions/")
			}
		}
	}
	return out
}

// roundTrip puts a value through JSON as the wire would, so numbers and nested maps are what a
// validator reads off a frame.
func roundTrip(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// declared fails when instance has a member the definition does not declare. The generated schema
// does not forbid extra members, so a misspelt optional member would otherwise validate.
func declared(t *testing.T, bundle map[string]any, name string, instance map[string]any) {
	t.Helper()
	props := bundle["definitions"].(map[string]any)[name].(map[string]any)["properties"].(map[string]any)
	for member := range instance {
		if _, ok := props[member]; !ok {
			t.Errorf("%s has no member %q", name, member)
		}
	}
}

// CRW-1193. What the builder writes, the notifications the bus carries, the replies the reconcile
// reads and the answers the responder gives are all valid against the schema codex-cli 0.154.0
// generates for the App Server (testdata/schema-0.154, copied unmodified).
func TestTurnParamsMatchGeneratedSchema(t *testing.T) {
	client := readJSONFile(t, filepath.Join(schemaDir, "ClientRequest.json"))
	notifications := readJSONFile(t, filepath.Join(schemaDir, "ServerNotification.json"))
	requests := readJSONFile(t, filepath.Join(schemaDir, "ServerRequest.json"))
	clientParams := methodParams(t, client)

	t.Run("turn/start from the builder", func(t *testing.T) {
		if clientParams["turn/start"] != "TurnStartParams" {
			t.Fatalf("turn/start params = %q", clientParams["turn/start"])
		}
		schema := definitionSchema(t, client, "TurnStartParams")
		for name, p := range map[string]TurnStartParams{
			"minimal": {ThreadID: "019d0000-0000-7000-8000-000000000001", Input: []UserInput{TextInput("crw-flow-op: call-1/t1\npacket")}},
			"full": {
				ThreadID: "019d0000-0000-7000-8000-000000000001", Input: []UserInput{TextInput("go")},
				Model: "gpt-5.4", Effort: "high", CWD: "/state/flow/run/calls/c1/wt",
				OutputSchema:  map[string]any{"type": "object", "properties": map[string]any{"verdict": map[string]any{"type": "string"}}, "required": []any{"verdict"}, "additionalProperties": false},
				SandboxPolicy: map[string]any{"type": "workspaceWrite", "networkAccess": false, "writableRoots": []any{"/state/flow/run/calls/c1/wt"}, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false},
			},
			"read-only": {ThreadID: "t", Input: []UserInput{TextInput("go")}, SandboxPolicy: map[string]any{"type": "readOnly", "networkAccess": false}},
		} {
			params, err := p.Map()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if err := schema.Validate(roundTrip(t, params)); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			declared(t, client, "TurnStartParams", params)
		}
		// The validator is live: a frame the host would reject does not pass.
		for name, bad := range map[string]map[string]any{
			"numeric thread": {"threadId": 7, "input": []any{}},
			"no input":       {"threadId": "t"},
			"bad effort":     {"threadId": "t", "input": []any{}, "effort": 3},
			"bad policy":     {"threadId": "t", "input": []any{}, "sandboxPolicy": map[string]any{"type": "nowhere"}},
		} {
			if err := schema.Validate(roundTrip(t, bad)); err == nil {
				t.Errorf("%s passed the schema", name)
			}
		}
	})

	t.Run("notifications the bus carries", func(t *testing.T) {
		params := methodParams(t, notifications)
		for _, file := range []string{"turn-completed", "item-completed", "token-usage-updated", "thread-status-changed", "error", "turn-started"} {
			frame := readJSONFile(t, filepath.Join(fixtureDir, "notification-"+file+".json"))
			method := frame["method"].(string)
			definition, ok := params[method]
			if !ok {
				t.Errorf("%s: %s is not a notification of the schema", file, method)
				continue
			}
			if err := definitionSchema(t, notifications, definition).Validate(frame["params"]); err != nil {
				t.Errorf("%s: %v", file, err)
			}
			// Every notification the bus carries names its thread at the top level.
			if props := notifications["definitions"].(map[string]any)[definition].(map[string]any)["properties"].(map[string]any); props["threadId"] == nil {
				t.Errorf("%s: %s has no threadId", file, method)
			}
		}
	})

	t.Run("reconcile reads", func(t *testing.T) {
		host := fakehost.Start(t)
		var asked []map[string]any
		var mu sync.Mutex
		host.Handle("thread/items/list", func(params json.RawMessage) fakehost.Reply {
			var p map[string]any
			_ = json.Unmarshal(params, &p)
			mu.Lock()
			asked = append(asked, p)
			mu.Unlock()
			page := "items-list-turn-a-page1"
			if _, paged := p["cursor"]; paged {
				page = "items-list-turn-a-page2"
			}
			return fakehost.Reply{Result: readJSONFile(t, filepath.Join(fixtureDir, page+".json"))}
		})
		host.Handle("thread/turns/list", func(params json.RawMessage) fakehost.Reply {
			var p map[string]any
			_ = json.Unmarshal(params, &p)
			mu.Lock()
			asked = append(asked, p)
			mu.Unlock()
			if _, paged := p["cursor"]; paged {
				return fakehost.Reply{Result: readJSONFile(t, filepath.Join(fixtureDir, "turns-list-page2.json"))}
			}
			return fakehost.Reply{Result: readJSONFile(t, filepath.Join(fixtureDir, "turns-list-page1.json"))}
		})
		c, err := appserver.Dial(context.Background(), host.SocketPath)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.Bus().ReconcileTurns(context.Background(), "thread-1", []string{"turn-a"}); err != nil {
			t.Fatal(err)
		}
		if len(asked) != 4 {
			t.Fatalf("reads = %v", asked)
		}
		for _, p := range asked {
			definition := "ThreadTurnsListParams"
			if _, items := p["turnId"]; items {
				definition = "ThreadItemsListParams"
			}
			if err := definitionSchema(t, client, definition).Validate(roundTrip(t, p)); err != nil {
				t.Errorf("%s %v: %v", definition, p, err)
			}
			declared(t, client, definition, p)
		}
		for file, schemaFile := range map[string]string{
			"turns-list-page1.json": "v2/ThreadTurnsListResponse.json", "turns-list-page2.json": "v2/ThreadTurnsListResponse.json",
			"items-list-turn-a-page1.json": "v2/ThreadItemsListResponse.json", "items-list-turn-a-page2.json": "v2/ThreadItemsListResponse.json",
		} {
			schema := resolveSchema(t, readJSONFile(t, filepath.Join(schemaDir, schemaFile)))
			if err := schema.Validate(readJSONFile(t, filepath.Join(fixtureDir, file))); err != nil {
				t.Errorf("%s: %v", file, err)
			}
		}
	})

	t.Run("responder answers", func(t *testing.T) {
		params := methodParams(t, requests)
		responses := map[string]string{
			"item/commandExecution/requestApproval": "CommandExecutionRequestApprovalResponse.json",
			"item/fileChange/requestApproval":       "FileChangeRequestApprovalResponse.json",
			"item/permissions/requestApproval":      "PermissionsRequestApprovalResponse.json",
			"mcpServer/elicitation/request":         "McpServerElicitationRequestResponse.json",
			"execCommandApproval":                   "ExecCommandApprovalResponse.json",
			"applyPatchApproval":                    "ApplyPatchApprovalResponse.json",
			"item/tool/requestUserInput":            "ToolRequestUserInputResponse.json",
		}
		if len(fakehost.ApprovalMethods) != len(responses) {
			t.Fatalf("approval methods %v", fakehost.ApprovalMethods)
		}
		for _, method := range fakehost.ApprovalMethods {
			definition, ok := params[method]
			if !ok {
				t.Errorf("%s is not a server request of the schema", method)
				continue
			}
			// The v2 requests name their thread in threadId; the two legacy ones, in conversationId, which
			// the client's attribution (threadId only) does not read.
			props := requests["definitions"].(map[string]any)[definition].(map[string]any)["properties"].(map[string]any)
			legacy := method == "execCommandApproval" || method == "applyPatchApproval"
			if (props["threadId"] != nil) == legacy || (props["conversationId"] != nil) != legacy {
				t.Errorf("%s names its thread unexpectedly: %v", method, props)
			}
			// The fake host raises each request with the params the schema defines for it.
			raised := fakehost.SchemaServerParams(method, "thread-1", "turn-1")
			if err := definitionSchema(t, requests, definition).Validate(roundTrip(t, raised)); err != nil {
				t.Errorf("%s: the fake host's request params are outside the schema: %v", method, err)
			}
			answer, ok := appserver.DenyAnswer(appserver.ServerRequest{Method: method, ThreadID: "t"})
			if !ok {
				t.Errorf("%s: no answer", method)
				continue
			}
			if answer.Error != nil {
				if method != "item/tool/requestUserInput" {
					t.Errorf("%s: refused with an error", method)
				}
				continue
			}
			schema := resolveSchema(t, readJSONFile(t, filepath.Join(schemaDir, responses[method])))
			if err := schema.Validate(roundTrip(t, answer.Result)); err != nil {
				t.Errorf("%s: %v", method, err)
			}
		}
		wrong := resolveSchema(t, readJSONFile(t, filepath.Join(schemaDir, responses["item/commandExecution/requestApproval"])))
		if err := wrong.Validate(roundTrip(t, map[string]any{"decision": "maybe"})); err == nil {
			t.Error("the response schema accepts an unknown decision")
		}
		if _, ok := appserver.DenyAnswer(appserver.ServerRequest{Method: "item/tool/call"}); ok {
			t.Error("a non-approval request is answered")
		}
	})
}
