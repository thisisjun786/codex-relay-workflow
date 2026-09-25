// Package mcp serves the bridge's twelve tools over MCP stdio, as server.py's FastMCP server
// does: the same tool names, input schemas, annotations, instructions and replies.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/pyerr"
)

// ServerName is the name FastMCP("codex-thread-bridge") reports and the plugin declares.
const ServerName = "codex-thread-bridge"

var (
	readAnnotations  = &sdk.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)}
	writeAnnotations = &sdk.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true), OpenWorldHint: boolPtr(true)}
)

func boolPtr(value bool) *bool { return &value }

// Output is every tool's result: server.py annotates each tool as returning dict[str, Any].
type Output = map[string]any

// NewServer builds the MCP server for b. Every tool error and failed App Server call is also
// explained on log, which must never be the protocol stream.
func NewServer(b *bridge.Bridge, version string, log io.Writer) *sdk.Server {
	logger := slog.New(slog.NewTextHandler(log, nil))
	server := sdk.NewServer(&sdk.Implementation{Name: ServerName, Version: version}, &sdk.ServerOptions{Instructions: instructions, Logger: logger})
	server.AddReceivingMiddleware(pythonWire, unknownTool, preParseArguments)
	h := handlers{b: b, log: logger}
	add(server, "get_capabilities", h.getCapabilities)
	add(server, "create_thread", h.createThread)
	add(server, "create_worktree_thread", h.createWorktreeThread)
	add(server, "send_message_to_thread", h.sendMessage)
	add(server, "list_threads", h.listThreads)
	add(server, "read_thread", h.readThread)
	add(server, "wait_thread", h.waitThread)
	add(server, "get_goal", h.getGoal)
	add(server, "get_active_turn", h.getActiveTurn)
	add(server, "steer_thread", h.steerThread)
	add(server, "pause_goal", h.pauseGoal)
	add(server, "get_operation", h.getOperation)
	return server
}

// add registers one typed tool. The schemas are the frozen FastMCP ones rather than inferred
// from In, because pydantic's rendering (titles, anyOf-null, defaults) is the contract.
func add[In any](server *sdk.Server, name string, call func(context.Context, In) (map[string]any, error)) {
	annotations := writeAnnotations
	if readOnly[name] {
		annotations = readAnnotations
	}
	tool := &sdk.Tool{Name: name, Description: descriptions[name], InputSchema: inputSchema(name, signatures[name]), OutputSchema: outputSchema(name), Annotations: annotations}
	sdk.AddTool(server, tool, func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Output, error) {
		out, err := call(ctx, in)
		if err != nil {
			// FastMCP's Tool.run wraps every exception the same way.
			//lint:ignore ST1005 tools/call text is caller-visible and byte-identical to FastMCP's
			return nil, nil, fmt.Errorf("Error executing tool %s: %s", name, pythonMessage(err))
		}
		text, err := indented(out)
		if err != nil {
			return nil, nil, err
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}}, out, nil
	})
}

// indented is FastMCP's unstructured copy of a dict result: pydantic_core.to_json(indent=2),
// which, unlike json.dumps, leaves non-ASCII text unescaped.
func indented(value any) (string, error) {
	var out strings.Builder
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return "", fmt.Errorf("encode tool result: %w", err)
	}
	return strings.TrimSuffix(out.String(), "\n"), nil
}

// pythonMessage is str(error) for the error Python raises in the same place.
func pythonMessage(err error) string {
	if _, message, ok := pyerr.OSError(err); ok {
		return message
	}
	return err.Error()
}

type handlers struct {
	b   *bridge.Bridge
	log *slog.Logger
}

// explained logs a failed call on the log stream and passes it through unchanged.
func (h handlers) explained(tool string, out map[string]any, err error) (map[string]any, error) {
	if err != nil {
		h.log.Error("tool call failed", "tool", tool, "error", pythonMessage(err))
		return nil, err
	}
	if status, _ := out["status"].(string); status == "not_attempted" || status == "outcome_unknown" || status == "failed" {
		h.log.Warn("tool call recorded an unsuccessful operation", "tool", tool, "status", status, "error", out["error"])
	}
	return out, nil
}

// empty reports a supplied optional string Python would still check. The Go bridge reads ""
// as "not supplied", which Python's None is; an explicit "" is refused there by nonempty().
func empty(value *string, name string, maximum int) error {
	if value != nil && *value == "" {
		return &bridge.Invalid{Reason: fmt.Sprintf("%s must contain 1–%d characters", name, maximum)}
	}
	return nil
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// decodeObject decodes a dict argument keeping numbers exact, as Python's json does, so a
// request fingerprint hashes 1 as 1 and not 1.0. Null is nil.
func decodeObject(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		return nil, fmt.Errorf("decode object argument: %w", err)
	}
	return out, nil
}

// integer converts a schema-validated JSON integer, saturating instead of overflowing so the
// bridge's own range check still refuses an absurd value.
func integer(value float64) int {
	return int(math.Max(math.Min(value, math.MaxInt32), math.MinInt32))
}

func receipt[R ~map[string]any](value R, err error) (map[string]any, error) {
	return map[string]any(value), err
}

func (h handlers) getCapabilities(ctx context.Context, _ struct{}) (map[string]any, error) {
	out, err := h.b.GetCapabilities(ctx)
	return h.explained("get_capabilities", out, err)
}

type createThreadIn struct {
	RequestID       string          `json:"request_id"`
	CWD             string          `json:"cwd"`
	Model           string          `json:"model"`
	ReasoningEffort string          `json:"reasoning_effort"`
	Prompt          *string         `json:"prompt"`
	Title           *string         `json:"title"`
	Sandbox         string          `json:"sandbox"`
	ProjectID       *string         `json:"app_server_project_id"`
	Roots           *[]string       `json:"runtime_workspace_roots"`
	Policy          json.RawMessage `json:"expected_sandbox_policy"`
	Exception       *string         `json:"policy_exception"`
	Role            *string         `json:"role"`
}

func (h handlers) createThread(ctx context.Context, in createThreadIn) (map[string]any, error) {
	for _, check := range []error{empty(in.Prompt, "prompt", 100000), empty(in.Title, "title", 500), empty(in.ProjectID, "app_server_project_id", 128), empty(in.Exception, "policy_exception", 128), empty(in.Role, "role", 64)} {
		if check != nil {
			return h.explained("create_thread", nil, check)
		}
	}
	policy, err := decodeObject(in.Policy)
	if err != nil {
		return h.explained("create_thread", nil, err)
	}
	var roots []string
	if in.Roots != nil {
		roots = append([]string{}, *in.Roots...)
	}
	out, err := receipt(h.b.CreateThread(ctx, bridge.CreateThread{RequestID: in.RequestID, CWD: in.CWD, Prompt: deref(in.Prompt), Title: deref(in.Title), Sandbox: in.Sandbox, Model: in.Model, ProjectID: deref(in.ProjectID), Effort: in.ReasoningEffort, Exception: deref(in.Exception), Role: deref(in.Role), Roots: roots, Policy: policy}))
	return h.explained("create_thread", out, err)
}

type createWorktreeIn struct {
	RequestID       string          `json:"request_id"`
	Source          string          `json:"source_repository"`
	Revision        string          `json:"starting_revision"`
	Destination     string          `json:"destination"`
	Mode            string          `json:"worktree_mode"`
	Sandbox         string          `json:"sandbox"`
	Policy          json.RawMessage `json:"expected_sandbox_policy"`
	Model           string          `json:"model"`
	ReasoningEffort string          `json:"reasoning_effort"`
	Prompt          *string         `json:"prompt"`
	Title           *string         `json:"title"`
	ProjectID       *string         `json:"app_server_project_id"`
	Exception       *string         `json:"policy_exception"`
	Role            *string         `json:"role"`
}

func (h handlers) createWorktreeThread(ctx context.Context, in createWorktreeIn) (map[string]any, error) {
	for _, check := range []error{empty(in.Prompt, "prompt", 100000), empty(in.Title, "title", 100000), empty(in.ProjectID, "app_server_project_id", 100000), empty(in.Exception, "policy_exception", 128), empty(in.Role, "role", 64)} {
		if check != nil {
			return h.explained("create_worktree_thread", nil, check)
		}
	}
	policy, err := decodeObject(in.Policy)
	if err != nil {
		return h.explained("create_worktree_thread", nil, err)
	}
	out, err := receipt(h.b.CreateWorktreeThread(ctx, bridge.CreateWorktree{RequestID: in.RequestID, Source: in.Source, Revision: in.Revision, Destination: in.Destination, Mode: in.Mode, Sandbox: in.Sandbox, Prompt: deref(in.Prompt), Title: deref(in.Title), Model: in.Model, Effort: in.ReasoningEffort, ProjectID: deref(in.ProjectID), Exception: deref(in.Exception), Role: deref(in.Role), Policy: policy}))
	return h.explained("create_worktree_thread", out, err)
}

type sendMessageIn struct {
	RequestID string          `json:"request_id"`
	ThreadID  string          `json:"thread_id"`
	Message   string          `json:"message"`
	Expected  json.RawMessage `json:"expected_settings"`
	Exception *string         `json:"policy_exception"`
	Role      *string         `json:"role"`
}

func (h handlers) sendMessage(ctx context.Context, in sendMessageIn) (map[string]any, error) {
	for _, check := range []error{empty(in.Exception, "policy_exception", 128), empty(in.Role, "role", 64)} {
		if check != nil {
			return h.explained("send_message_to_thread", nil, check)
		}
	}
	expected, err := decodeObject(in.Expected)
	if err != nil {
		return h.explained("send_message_to_thread", nil, err)
	}
	out, err := receipt(h.b.SendMessageToThread(ctx, bridge.SendMessage{RequestID: in.RequestID, ThreadID: in.ThreadID, Message: in.Message, Exception: deref(in.Exception), Role: deref(in.Role), Expected: expected}))
	return h.explained("send_message_to_thread", out, err)
}

// cursorArgument is server.py's `cursor or None`: the empty default means the first page.
func cursorArgument(cursor string) any {
	if cursor == "" {
		return nil
	}
	return cursor
}

type listThreadsIn struct {
	CWD    *string `json:"cwd"`
	Limit  float64 `json:"limit"`
	Cursor string  `json:"cursor"`
}

func (h handlers) listThreads(ctx context.Context, in listThreadsIn) (map[string]any, error) {
	if in.CWD != nil && *in.CWD == "" {
		// Python's absolute_directory("") refuses; the Go bridge reads "" as no filter.
		return h.explained("list_threads", nil, &bridge.Invalid{Reason: "cwd must be an existing absolute directory on the App Server host"})
	}
	out, err := h.b.ListThreads(ctx, deref(in.CWD), integer(in.Limit), cursorArgument(in.Cursor))
	return h.explained("list_threads", out, err)
}

type readThreadIn struct {
	ThreadID     string  `json:"thread_id"`
	Limit        float64 `json:"limit"`
	Cursor       string  `json:"cursor"`
	MaxTextChars float64 `json:"max_text_chars"`
}

func (h handlers) readThread(ctx context.Context, in readThreadIn) (map[string]any, error) {
	out, err := h.b.ReadThread(ctx, in.ThreadID, integer(in.Limit), cursorArgument(in.Cursor), integer(in.MaxTextChars))
	return h.explained("read_thread", out, err)
}

type waitThreadIn struct {
	ThreadID       string  `json:"thread_id"`
	TurnID         string  `json:"turn_id"`
	TimeoutSeconds float64 `json:"timeout_seconds"`
}

func (h handlers) waitThread(ctx context.Context, in waitThreadIn) (map[string]any, error) {
	seconds := in.TimeoutSeconds
	if seconds < 0 || seconds > 50 {
		// Keeps the bridge's own refusal while never overflowing a Duration.
		seconds = math.Copysign(51, seconds)
	}
	out, err := h.b.WaitThread(ctx, in.ThreadID, in.TurnID, time.Duration(seconds*float64(time.Second)))
	return h.explained("wait_thread", out, err)
}

type threadIn struct {
	ThreadID string `json:"thread_id"`
}

func (h handlers) getGoal(ctx context.Context, in threadIn) (map[string]any, error) {
	out, err := h.b.GetGoal(ctx, in.ThreadID)
	return h.explained("get_goal", out, err)
}

func (h handlers) getActiveTurn(ctx context.Context, in threadIn) (map[string]any, error) {
	out, err := h.b.ActiveTurn(ctx, in.ThreadID)
	return h.explained("get_active_turn", out, err)
}

type steerIn struct {
	RequestID      string `json:"request_id"`
	ThreadID       string `json:"thread_id"`
	ExpectedTurnID string `json:"expected_turn_id"`
	Message        string `json:"message"`
}

func (h handlers) steerThread(ctx context.Context, in steerIn) (map[string]any, error) {
	out, err := receipt(h.b.SteerThread(ctx, in.RequestID, in.ThreadID, in.ExpectedTurnID, in.Message))
	return h.explained("steer_thread", out, err)
}

type pauseIn struct {
	RequestID string `json:"request_id"`
	ThreadID  string `json:"thread_id"`
}

func (h handlers) pauseGoal(ctx context.Context, in pauseIn) (map[string]any, error) {
	out, err := receipt(h.b.PauseGoal(ctx, in.RequestID, in.ThreadID))
	return h.explained("pause_goal", out, err)
}

type operationIn struct {
	RequestID string `json:"request_id"`
}

func (h handlers) getOperation(ctx context.Context, in operationIn) (map[string]any, error) {
	out, err := receipt(h.b.GetOperation(ctx, in.RequestID))
	return h.explained("get_operation", out, err)
}
