package mcp

import (
	"strings"
)

// The tools' input schemas, frozen in contract/schema/bridge-mcp-tools.json and declared here
// field by field: a required string, an optional string (anyOf string|null, default null), an
// enum, an object, a list and a defaulted scalar, each titled as the frozen schema titles it.
// Field order is the frozen order, which is also the order of "required".

type field struct {
	name     string
	schema   map[string]any
	required bool
}

// title is a field's title in the frozen schema: words split on "_", each capitalised.
func title(name string) string {
	words := strings.Split(name, "_")
	for i, word := range words {
		if word != "" {
			words[i] = strings.ToUpper(word[:1]) + word[1:]
		}
	}
	return strings.Join(words, " ")
}

func required(name string) field {
	return field{name, map[string]any{"title": title(name), "type": "string"}, true}
}

func optional(name string) field {
	return field{name, map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "null"}}, "default": nil, "title": title(name)}, false}
}

var sandboxes = []any{"read-only", "workspace-write", "danger-full-access"}

func object(name string) field {
	return field{name, map[string]any{"additionalProperties": true, "title": title(name), "type": "object"}, true}
}

func defaulted(name, kind string, value any) field {
	return field{name, map[string]any{"default": value, "title": title(name), "type": kind}, false}
}

// inputSchema is the object schema tools/list gives one tool.
func inputSchema(tool string, fields []field) map[string]any {
	properties := map[string]any{}
	requiredNames := []any{}
	for _, f := range fields {
		properties[f.name] = f.schema
		if f.required {
			requiredNames = append(requiredNames, f.name)
		}
	}
	schema := map[string]any{"properties": properties, "title": tool + "Arguments", "type": "object"}
	if len(requiredNames) > 0 {
		schema["required"] = requiredNames
	}
	return schema
}

// outputSchema is every tool's output schema: an object of any members.
func outputSchema(tool string) map[string]any {
	return map[string]any{"additionalProperties": true, "title": tool + "DictOutput", "type": "object"}
}

// signatures lists every tool's parameters in the frozen order.
var signatures = map[string][]field{
	"get_capabilities": {},
	"create_thread": {
		required("request_id"), required("cwd"), required("model"), required("reasoning_effort"),
		optional("prompt"), optional("title"),
		{"sandbox", map[string]any{"default": "read-only", "enum": sandboxes, "title": "Sandbox", "type": "string"}, false},
		optional("app_server_project_id"),
		{"runtime_workspace_roots", map[string]any{"anyOf": []any{map[string]any{"items": map[string]any{"type": "string"}, "type": "array"}, map[string]any{"type": "null"}}, "default": nil, "title": "Runtime Workspace Roots"}, false},
		{"expected_sandbox_policy", map[string]any{"anyOf": []any{map[string]any{"additionalProperties": true, "type": "object"}, map[string]any{"type": "null"}}, "default": nil, "title": "Expected Sandbox Policy"}, false},
		optional("policy_exception"), optional("role"),
	},
	"create_worktree_thread": {
		required("request_id"), required("source_repository"), required("starting_revision"), required("destination"),
		{"worktree_mode", map[string]any{"const": "bridge-managed-retained", "title": "Worktree Mode", "type": "string"}, true},
		{"sandbox", map[string]any{"enum": sandboxes, "title": "Sandbox", "type": "string"}, true},
		object("expected_sandbox_policy"),
		required("model"), required("reasoning_effort"),
		optional("prompt"), optional("title"), optional("app_server_project_id"), optional("policy_exception"), optional("role"),
	},
	"send_message_to_thread": {
		required("request_id"), required("thread_id"), required("message"), object("expected_settings"),
		optional("policy_exception"), optional("role"),
	},
	"list_threads":    {optional("cwd"), defaulted("limit", "integer", 20), defaulted("cursor", "string", "")},
	"read_thread":     {required("thread_id"), defaulted("limit", "integer", 10), defaulted("cursor", "string", ""), defaulted("max_text_chars", "integer", 4000)},
	"wait_thread":     {required("thread_id"), required("turn_id"), defaulted("timeout_seconds", "number", 20)},
	"get_goal":        {required("thread_id")},
	"get_active_turn": {required("thread_id")},
	"steer_thread":    {required("request_id"), required("thread_id"), required("expected_turn_id"), required("message")},
	"pause_goal":      {required("request_id"), required("thread_id")},
	"get_operation":   {required("request_id")},
}

// order is the registration order, which is the order tools/list reports.
var order = []string{
	"get_capabilities", "create_thread", "create_worktree_thread", "send_message_to_thread",
	"list_threads", "read_thread", "wait_thread", "get_goal", "get_active_turn", "steer_thread",
	"pause_goal", "get_operation",
}

// readOnly names the tools annotated read-only; the rest carry the write annotations.
var readOnly = map[string]bool{
	"get_capabilities": true, "list_threads": true, "read_thread": true, "wait_thread": true,
	"get_goal": true, "get_active_turn": true, "get_operation": true,
}
