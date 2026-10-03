// Package harness is the entry the Codex hooks and crw pabcd reach: how a hook's input is bounded and
// read, the record every invocation leaves, and the order in which a leg is let to act. It is the Go
// form of CXC v0.2.40 pabcd-state/src/{cli,parse}.ts and scripts/hook-observation.mjs. What a leg
// itself does belongs to the issue that ports it and registers its handler in legs.go.
package harness

import (
	"encoding/json"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// The payloads codex-rs writes to a hook's stdin. A field the oracle types optional is a pointer, nil
// when the key is absent or not a string (or, for a flag, not a boolean).
type (
	SessionStart     struct{ SessionID, Cwd string }
	UserPromptSubmit struct {
		SessionID, Cwd, Prompt                        string
		TranscriptPath, TurnID, Model, PermissionMode *string
	}
	Stop struct {
		SessionID, Cwd                               string
		TranscriptPath, TurnID, LastAssistantMessage *string
		StopHookActive                               *bool
	}
	PostCompact struct {
		SessionID, Cwd                  string
		TurnID, TranscriptPath, Trigger *string
	}
	SubagentStop struct {
		SessionID, Cwd, AgentType                                                   string
		AgentID, TurnID, TranscriptPath, AgentTranscriptPath, Model, PermissionMode *string
		StopHookActive                                                              *bool
		LastAssistantMessage                                                        *string
	}
	PostToolUse struct {
		SessionID, Cwd, ToolName string
		ToolInput, ToolResponse  any
		ToolUseID, TurnID        *string
	}
)

// asObject is the oracle's: the trimmed text must be one JSON object, else nothing.
func asObject(raw string) map[string]any {
	var v any
	if s := text.Trim(raw); s == "" || json.Unmarshal([]byte(s), &v) != nil {
		return nil
	}
	o, _ := v.(map[string]any)
	return o
}

func opt(o map[string]any, key string) *string {
	if s, ok := o[key].(string); ok {
		return &s
	}
	return nil
}

func optBool(o map[string]any, key string) *bool {
	if b, ok := o[key].(bool); ok {
		return &b
	}
	return nil
}

// required is the object when it names this event and holds each key as a string.
func required(raw, event string, keys ...string) (o map[string]any, vals []string, ok bool) {
	if o = asObject(raw); o == nil || o["hook_event_name"] != event {
		return nil, nil, false
	}
	for _, k := range keys {
		s, isString := o[k].(string)
		if !isString {
			return nil, nil, false
		}
		vals = append(vals, s)
	}
	return o, vals, true
}

// IsSubagentHookPayload is true when codex-rs stamped agent_id or agent_type, non-empty, into the
// input: a thread-spawned subagent's turn, whose session_id is its parent's. Any other input, an
// unparseable one included, reads as a root turn.
func IsSubagentHookPayload(raw string) bool {
	o := asObject(raw)
	id, _ := o["agent_id"].(string)
	typ, _ := o["agent_type"].(string)
	return id != "" || typ != ""
}

func ParseSessionStart(raw string) (SessionStart, bool) {
	_, v, ok := required(raw, "SessionStart", "session_id", "cwd")
	if !ok || !state.IsCanonicalSessionID(v[0]) || text.Trim(v[1]) == "" {
		return SessionStart{}, false
	}
	return SessionStart{v[0], v[1]}, true
}

func ParseUserPromptSubmit(raw string) (UserPromptSubmit, bool) {
	o, v, ok := required(raw, "UserPromptSubmit", "session_id", "cwd", "prompt")
	if !ok {
		return UserPromptSubmit{}, false
	}
	return UserPromptSubmit{v[0], v[1], v[2], opt(o, "transcript_path"), opt(o, "turn_id"), opt(o, "model"), opt(o, "permission_mode")}, true
}

func ParseStop(raw string) (Stop, bool) {
	o, v, ok := required(raw, "Stop", "session_id", "cwd")
	if !ok {
		return Stop{}, false
	}
	return Stop{v[0], v[1], opt(o, "transcript_path"), opt(o, "turn_id"), opt(o, "last_assistant_message"), optBool(o, "stop_hook_active")}, true
}

func ParsePostCompact(raw string) (PostCompact, bool) {
	o, v, ok := required(raw, "PostCompact", "session_id", "cwd")
	if !ok {
		return PostCompact{}, false
	}
	return PostCompact{v[0], v[1], opt(o, "turn_id"), opt(o, "transcript_path"), opt(o, "trigger")}, true
}

// ParseSubagentStop needs agent_type: it is the matcher's key and the gate's primary discriminator.
func ParseSubagentStop(raw string) (SubagentStop, bool) {
	o, v, ok := required(raw, "SubagentStop", "session_id", "cwd", "agent_type")
	if !ok {
		return SubagentStop{}, false
	}
	return SubagentStop{v[0], v[1], v[2], opt(o, "agent_id"), opt(o, "turn_id"), opt(o, "transcript_path"), opt(o, "agent_transcript_path"),
		opt(o, "model"), opt(o, "permission_mode"), optBool(o, "stop_hook_active"), opt(o, "last_assistant_message")}, true
}

func ParsePostToolUse(raw string) (PostToolUse, bool) {
	o, v, ok := required(raw, "PostToolUse", "session_id", "cwd", "tool_name")
	if !ok {
		return PostToolUse{}, false
	}
	return PostToolUse{v[0], v[1], v[2], o["tool_input"], o["tool_response"], opt(o, "tool_use_id"), opt(o, "turn_id")}, true
}
