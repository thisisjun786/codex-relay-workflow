package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"

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
