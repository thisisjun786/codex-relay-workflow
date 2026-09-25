package mcp

import (
	"strings"
)

// The input schemas FastMCP derives from server.py's tool signatures, declared field by field.
// Each helper reproduces the pydantic rendering of one Python annotation: a required str, an
// optional str (anyOf string|null, default null), a Literal, a dict, a list and a defaulted
// scalar. Field order is the signature order, which is also the order of "required".

type field struct {
	name     string
	schema   map[string]any
	required bool
	// plain is true when the Python annotation is exactly str. FastMCP pre-parses a JSON-shaped
	// string for every other annotation (func_metadata.pre_parse_json), so only these keep it.
	plain bool
}

// title is pydantic's default field title: words split on "_", each capitalised.
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
	return field{name, map[string]any{"title": title(name), "type": "string"}, true, true}
}

func optional(name string) field {
	return field{name, map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "null"}}, "default": nil, "title": title(name)}, false, false}
}

var sandboxes = []any{"read-only", "workspace-write", "danger-full-access"}

func object(name string) field {
	return field{name, map[string]any{"additionalProperties": true, "title": title(name), "type": "object"}, true, false}
}

func defaulted(name, kind string, value any) field {
	return field{name, map[string]any{"default": value, "title": title(name), "type": kind}, false, kind == "string"}
}

// inputSchema is the object schema FastMCP lists for one tool.
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

// outputSchema is FastMCP's rendering of a dict[str, Any] return annotation.
func outputSchema(tool string) map[string]any {
	return map[string]any{"additionalProperties": true, "title": tool + "DictOutput", "type": "object"}
}

// signatures lists every tool's parameters in server.py order.
var signatures = map[string][]field{
	"get_capabilities": {},
	"create_thread": {
		required("request_id"), required("cwd"), required("model"), required("reasoning_effort"),
		optional("prompt"), optional("title"),
		{"sandbox", map[string]any{"default": "read-only", "enum": sandboxes, "title": "Sandbox", "type": "string"}, false, false},
		optional("app_server_project_id"),
		{"runtime_workspace_roots", map[string]any{"anyOf": []any{map[string]any{"items": map[string]any{"type": "string"}, "type": "array"}, map[string]any{"type": "null"}}, "default": nil, "title": "Runtime Workspace Roots"}, false, false},
		{"expected_sandbox_policy", map[string]any{"anyOf": []any{map[string]any{"additionalProperties": true, "type": "object"}, map[string]any{"type": "null"}}, "default": nil, "title": "Expected Sandbox Policy"}, false, false},
		optional("policy_exception"), optional("role"),
	},
	"create_worktree_thread": {
		required("request_id"), required("source_repository"), required("starting_revision"), required("destination"),
		{"worktree_mode", map[string]any{"const": "bridge-managed-retained", "title": "Worktree Mode", "type": "string"}, true, false},
		{"sandbox", map[string]any{"enum": sandboxes, "title": "Sandbox", "type": "string"}, true, false},
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

// order is server.py's registration order, which is the order tools/list reports.
var order = []string{
	"get_capabilities", "create_thread", "create_worktree_thread", "send_message_to_thread",
	"list_threads", "read_thread", "wait_thread", "get_goal", "get_active_turn", "steer_thread",
	"pause_goal", "get_operation",
}

// readOnly names the tools registered with server.py READ; the rest carry WRITE.
var readOnly = map[string]bool{
	"get_capabilities": true, "list_threads": true, "read_thread": true, "wait_thread": true,
	"get_goal": true, "get_active_turn": true, "get_operation": true,
}
