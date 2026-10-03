package harness

import (
	"encoding/json"
	"reflect"
	"testing"
)

func doc(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func ptr[T any](v T) *T { return &v }

type obj = map[string]any

func withKeys(base obj, extra obj) obj {
	out := obj{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// The ten tests of pabcd-state/test/parse.test.ts.

func TestParseSessionStartAcceptsACanonicalRootPayload(t *testing.T) {
	got, ok := ParseSessionStart(doc(t, obj{"hook_event_name": "SessionStart", "session_id": "session-1", "cwd": "/tmp/workspace"}))
	if want := (SessionStart{SessionID: "session-1", Cwd: "/tmp/workspace"}); !ok || got != want {
		t.Fatalf("got %+v, %v", got, ok)
	}
}

func TestParseSessionStartRejectsMalformedWrongEventAndMissingInputs(t *testing.T) {
	for _, raw := range []string{
		"", "   ", "{not json", "[]",
		doc(t, obj{"session_id": "s1", "cwd": "/tmp/x"}),
		doc(t, obj{"hook_event_name": "Stop", "session_id": "s1", "cwd": "/tmp/x"}),
		doc(t, obj{"hook_event_name": "SessionStart", "cwd": "/tmp/x"}),
		doc(t, obj{"hook_event_name": "SessionStart", "session_id": "s1"}),
	} {
		if _, ok := ParseSessionStart(raw); ok {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestParseSessionStartRejectsEmptyWhitespaceOrRewrittenIdentities(t *testing.T) {
	for _, p := range []obj{
		{"session_id": "", "cwd": "/tmp/x"},
		{"session_id": " \t\n", "cwd": "/tmp/x"},
		{"session_id": "  session-1  ", "cwd": "/tmp/x"},
		{"session_id": "../session-1", "cwd": "/tmp/x"},
		{"session_id": "세션-1", "cwd": "/tmp/x"},
		{"session_id": "s1", "cwd": ""},
		{"session_id": "s1", "cwd": " \t\n"},
	} {
		if _, ok := ParseSessionStart(doc(t, withKeys(obj{"hook_event_name": "SessionStart"}, p))); ok {
			t.Errorf("accepted %v", p)
		}
	}
}

func TestIsSubagentHookPayload(t *testing.T) {
	root := obj{"hook_event_name": "UserPromptSubmit", "session_id": "s1", "cwd": "/tmp/x", "prompt": "interview me", "turn_id": "t1"}
	for name, c := range map[string]struct {
		raw  string
		want bool
	}{
		"root payload":                 {doc(t, root), false},
		"agent_type present":           {doc(t, withKeys(root, obj{"agent_type": "worker"})), true},
		"agent_id alone":               {doc(t, withKeys(root, obj{"agent_id": "a-1"})), true},
		"empty-string agent fields":    {doc(t, withKeys(root, obj{"agent_id": "", "agent_type": ""})), false},
		"non-string agent fields":      {doc(t, withKeys(root, obj{"agent_id": 7, "agent_type": nil})), false},
		"empty stdin":                  {"", false},
		"whitespace stdin":             {"   ", false},
		"malformed stdin":              {"{not json", false},
		"array stdin":                  {"[1,2]", false},
		"SubagentStop is detected too": {doc(t, obj{"hook_event_name": "SubagentStop", "session_id": "s1", "cwd": "/tmp/x", "agent_type": "worker", "agent_id": "a-1"}), true},
	} {
		if got := IsSubagentHookPayload(c.raw); got != c.want {
			t.Errorf("%s: got %v", name, got)
		}
	}
}

// The oracle has no test for the other five parsers: this pins each field rule of parse.ts.
func TestParsersKeepRequiredOptionalAndNullableFields(t *testing.T) {
	base := obj{"session_id": "s1", "cwd": "/w"}
	up, ok := ParseUserPromptSubmit(doc(t, withKeys(base, obj{"hook_event_name": "UserPromptSubmit", "prompt": "p", "transcript_path": "/t", "turn_id": "t1", "model": 5, "permission_mode": "m"})))
	if want := (UserPromptSubmit{SessionID: "s1", Cwd: "/w", Prompt: "p", TranscriptPath: ptr("/t"), TurnID: ptr("t1"), PermissionMode: ptr("m")}); !ok || !reflect.DeepEqual(up, want) {
		t.Errorf("UserPromptSubmit: %+v, %v", up, ok)
	}
	stop, ok := ParseStop(doc(t, withKeys(base, obj{"hook_event_name": "Stop", "stop_hook_active": "yes", "last_assistant_message": "done"})))
	if want := (Stop{SessionID: "s1", Cwd: "/w", LastAssistantMessage: ptr("done")}); !ok || !reflect.DeepEqual(stop, want) {
		t.Errorf("Stop: %+v, %v", stop, ok)
	}
	if stop, _ = ParseStop(doc(t, withKeys(base, obj{"hook_event_name": "Stop", "stop_hook_active": false}))); stop.StopHookActive == nil || *stop.StopHookActive {
		t.Errorf("Stop keeps a false stop_hook_active: %+v", stop)
	}
	pc, ok := ParsePostCompact(doc(t, withKeys(base, obj{"hook_event_name": "PostCompact", "trigger": "auto"})))
	if want := (PostCompact{SessionID: "s1", Cwd: "/w", Trigger: ptr("auto")}); !ok || !reflect.DeepEqual(pc, want) {
		t.Errorf("PostCompact: %+v, %v", pc, ok)
	}
	ss, ok := ParseSubagentStop(doc(t, withKeys(base, obj{"hook_event_name": "SubagentStop", "agent_type": "worker", "agent_id": "a", "stop_hook_active": true})))
	if want := (SubagentStop{SessionID: "s1", Cwd: "/w", AgentType: "worker", AgentID: ptr("a"), StopHookActive: ptr(true)}); !ok || !reflect.DeepEqual(ss, want) {
		t.Errorf("SubagentStop: %+v, %v", ss, ok)
	}
	pt, ok := ParsePostToolUse(doc(t, withKeys(base, obj{"hook_event_name": "PostToolUse", "tool_name": "x", "tool_input": obj{"a": 1.0}, "tool_use_id": "u"})))
	if want := (PostToolUse{SessionID: "s1", Cwd: "/w", ToolName: "x", ToolInput: obj{"a": 1.0}, ToolUseID: ptr("u")}); !ok || !reflect.DeepEqual(pt, want) {
		t.Errorf("PostToolUse: %+v, %v", pt, ok)
	}
	_, noPrompt := ParseUserPromptSubmit(doc(t, withKeys(base, obj{"hook_event_name": "UserPromptSubmit"})))
	_, otherEvent := ParseStop(doc(t, withKeys(base, obj{"hook_event_name": "SessionStart"})))
	_, noCwd := ParsePostCompact(doc(t, obj{"hook_event_name": "PostCompact", "session_id": "s1"}))
	_, noAgentType := ParseSubagentStop(doc(t, withKeys(base, obj{"hook_event_name": "SubagentStop"})))
	_, noToolName := ParsePostToolUse(doc(t, withKeys(base, obj{"hook_event_name": "PostToolUse"})))
	if noPrompt || otherEvent || noCwd || noAgentType || noToolName {
		t.Errorf("a payload missing a required field or of another event was accepted: %v %v %v %v %v", noPrompt, otherEvent, noCwd, noAgentType, noToolName)
	}
}
