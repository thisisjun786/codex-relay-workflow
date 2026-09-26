package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// rawResult is a result whose wire bytes are already decided.
type rawResult struct {
	sdk.ResultBase
	raw json.RawMessage
}

func (r *rawResult) MarshalJSON() ([]byte, error) { return r.raw, nil }

// capabilities is what FastMCP advertises at initialize, byte for byte: every list without
// change notifications, no subscriptions and no logging. The SDK's own struct omits false and
// empty members, so the bytes are written here.
const capabilities = `{"experimental":{},"prompts":{"listChanged":false},"resources":{"subscribe":false,"listChanged":false},"tools":{"listChanged":false}}`

// emptyLists are FastMCP's answers for the lists this server leaves empty. The SDK adds its
// cache hints (ttlMs, cacheScope), which Python never sends.
var emptyLists = map[string]string{
	"prompts/list":             `{"prompts":[]}`,
	"resources/list":           `{"resources":[]}`,
	"resources/templates/list": `{"resourceTemplates":[]}`,
}

// methodNotFound is the low-level MCP server's answer for a method FastMCP registers no
// handler for.
var methodNotFound = &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "Method not found"}

// pythonWire makes every reply outside tools/call read as FastMCP's does on the wire.
func pythonWire(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		switch method {
		case "logging/setLevel", "completion/complete":
			return nil, methodNotFound
		case "prompts/get":
			// FastMCP raises ValueError, which the low-level server reports with code 0.
			params, _ := req.GetParams().(*sdk.GetPromptParams)
			return nil, &jsonrpc.Error{Code: 0, Message: "Unknown prompt: " + params.Name}
		case "resources/read":
			params, _ := req.GetParams().(*sdk.ReadResourceParams)
			return nil, &jsonrpc.Error{Code: 0, Message: "Unknown resource: " + params.URI}
		}
		if body, ok := emptyLists[method]; ok {
			return &rawResult{raw: json.RawMessage(body)}, nil
		}
		result, err := next(ctx, method, req)
		if err != nil {
			return result, err
		}
		switch r := result.(type) {
		case *sdk.InitializeResult:
			return initializeLikePython(r)
		case *sdk.ListToolsResult:
			return toolsLikePython(r)
		}
		return result, nil
	}
}

// initializeLikePython writes the initialize result in FastMCP's member order with its
// capabilities.
func initializeLikePython(r *sdk.InitializeResult) (sdk.Result, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	for i, member := range []struct {
		key   string
		value any
	}{{"protocolVersion", r.ProtocolVersion}, {"capabilities", json.RawMessage(capabilities)}, {"serverInfo", r.ServerInfo}, {"instructions", r.Instructions}} {
		encoded, err := json.Marshal(member.value)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			out.WriteByte(',')
		}
		fmt.Fprintf(&out, "%q:%s", member.key, encoded)
	}
	out.WriteByte('}')
	return &rawResult{raw: out.Bytes()}, nil
}

// toolsLikePython reports tools in server.py registration order (the SDK sorts them by name),
// drops the idempotentHint the SDK always writes (server.py's READ and WRITE annotations never
// set it) and carries only "tools": no cache hints, no nextCursor.
func toolsLikePython(listed *sdk.ListToolsResult) (sdk.Result, error) {
	slices.SortStableFunc(listed.Tools, func(a, b *sdk.Tool) int {
		return slices.Index(order, a.Name) - slices.Index(order, b.Name)
	})
	raw, err := json.Marshal(listed.Tools)
	if err != nil {
		return nil, err
	}
	var tools []map[string]any
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, err
	}
	for _, tool := range tools {
		if annotations, ok := tool["annotations"].(map[string]any); ok {
			delete(annotations, "idempotentHint")
		}
	}
	// Python writes '<', '>' and '&' as themselves; json.Marshal would escape them.
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(map[string]any{"tools": tools}); err != nil {
		return nil, err
	}
	return &rawResult{raw: bytes.TrimSuffix(out.Bytes(), []byte("\n"))}, nil
}

// unknownTool is FastMCP's answer to a tool it does not have: a result flagged isError, not a
// JSON-RPC error.
func unknownTool(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		if call, ok := req.(*sdk.CallToolRequest); ok && method == "tools/call" && call.Params != nil {
			if _, known := signatures[call.Params.Name]; !known {
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "Unknown tool: " + call.Params.Name}}, IsError: true}, nil
			}
		}
		return next(ctx, method, req)
	}
}

// laxNumber is pydantic's lax-mode int/float input, as measured on pydantic 2.13: a bool is 0
// or 1; a string, stripped of surrounding whitespace, is an int when it is a signed decimal
// with single underscores between digits and an optional all-zero fraction ("5.0"), and a float
// when Python's float() reads it (exponents, "inf", "nan" and underscores between digits
// included). ok is false when pydantic refuses the value too, leaving the refusal to
// validateArguments. A float that is not finite cannot travel as JSON; it is passed on as a
// value outside every accepted range, which the bridge refuses exactly as Python refuses inf
// and nan.
func laxNumber(raw json.RawMessage, kind string) (json.RawMessage, bool) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil, false
	}
	switch v := value.(type) {
	case bool:
		if v {
			return json.RawMessage("1"), true
		}
		return json.RawMessage("0"), true
	case string:
		text := strings.TrimSpace(v)
		if kind == "integer" {
			match := integerText.FindStringSubmatch(text)
			if match == nil {
				return nil, false
			}
			digits := strings.TrimPrefix(strings.ReplaceAll(match[1], "_", ""), "+")
			if _, err := strconv.ParseInt(digits, 10, 64); err != nil {
				// Python's int is unbounded, so the value validates and the bridge's own range
				// check refuses it; any value past that range stands in for it.
				if strings.HasPrefix(digits, "-") {
					return json.RawMessage("-1000000"), true
				}
				return json.RawMessage("1000000"), true
			}
			return json.RawMessage(digits), true
		}
		if strings.ContainsAny(text, "xXpP") || misplacedUnderscore.MatchString(text) {
			return nil, false
		}
		number, err := strconv.ParseFloat(strings.ReplaceAll(text, "_", ""), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil, false
		}
		if math.IsInf(number, 0) || math.IsNaN(number) {
			return json.RawMessage("1e308"), true
		}
		encoded, err := json.Marshal(number)
		return encoded, err == nil
	}
	return nil, false
}

// integerText is what pydantic reads as an int from a string: a sign, digits with single
// underscores between them, and an optional fraction of zeros.
var integerText = regexp.MustCompile(`^([+-]?[0-9]+(?:_[0-9]+)*)(?:\.0+)?$`)

// misplacedUnderscore finds an underscore Python's float() refuses: one not between two digits.
var misplacedUnderscore = regexp.MustCompile(`(^|[^0-9])_|_([^0-9]|$)`)

// preParseArguments is FastMCP's func_metadata.pre_parse_json, followed by the argument
// validation FastMCP reports (validate.go), so a refused call never reaches the SDK's own
// schema validator, whose report names one field in map order. The pre-parse: an argument whose annotation is
// not exactly str, sent as a string holding JSON, is replaced by the decoded value unless that
// value is itself a string, a number or a boolean (a Python int). A plain str argument keeps its text, which is how a
// JSON-shaped pagination cursor reaches the host byte for byte.
func preParseArguments(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		call, ok := req.(*sdk.CallToolRequest)
		if !ok || method != "tools/call" || call.Params == nil {
			return next(ctx, method, req)
		}
		fields, known := signatures[call.Params.Name]
		arguments := map[string]json.RawMessage{}
		if len(call.Params.Arguments) > 0 && string(call.Params.Arguments) != "null" {
			if json.Unmarshal(call.Params.Arguments, &arguments) != nil {
				return next(ctx, method, req)
			}
		}
		if !known {
			return next(ctx, method, req)
		}
		changed := false
		for _, f := range fields {
			raw, present := arguments[f.name]
			if !present {
				continue
			}
			if kind := f.schema["type"]; kind == "integer" || kind == "number" {
				if coerced, ok := laxNumber(raw, kind.(string)); ok {
					arguments[f.name] = coerced
					changed = true
				}
				continue
			}
			var text string
			if f.plain || json.Unmarshal(raw, &text) != nil {
				continue
			}
			var parsed any
			if json.Unmarshal([]byte(text), &parsed) != nil {
				continue
			}
			switch parsed.(type) {
			case string, float64, bool:
				continue
			}
			arguments[f.name] = json.RawMessage(text)
			changed = true
		}
		if text := validateArguments(call.Params.Name, arguments); text != "" {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}, IsError: true}, nil
		}
		if changed {
			rewritten, err := json.Marshal(arguments)
			if err != nil {
				return nil, err
			}
			call.Params.Arguments = rewritten
		}
		return next(ctx, method, req)
	}
}

// pythonTransport rewrites what the SDK's JSON-RPC layer adds after every handler has run: it
// appends the method to a method-not-found message, and the MCP low-level server that FastMCP
// runs on answers every such method with the bare "Method not found".
type pythonTransport struct{ sdk.Transport }

func (t pythonTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	conn, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return pythonConnection{conn}, nil
}

type pythonConnection struct{ sdk.Connection }

// clientRequests and clientNotifications are the methods of mcp 1.30.0's ClientRequest and
// ClientNotification unions, which its low-level server validates every incoming message
// against before any handler sees it.
var clientRequests = map[string]bool{
	"ping": true, "initialize": true, "completion/complete": true, "logging/setLevel": true,
	"prompts/get": true, "prompts/list": true, "resources/list": true, "resources/templates/list": true,
	"resources/read": true, "resources/subscribe": true, "resources/unsubscribe": true,
	"tools/call": true, "tools/list": true, "tasks/get": true, "tasks/result": true,
	"tasks/list": true, "tasks/cancel": true,
}

var clientNotifications = map[string]bool{
	"notifications/cancelled": true, "notifications/progress": true, "notifications/initialized": true,
	"notifications/roots/list_changed": true, "notifications/tasks/status": true,
}

// invalidRequest is what the Python server answers a request whose params are absent, null or an
// object when it names a method outside ClientRequest, or is a tools/call whose params fail
// CallToolRequestParams: its union validation fails and it reports that failure.
var invalidRequest = &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "Invalid request parameters", Data: json.RawMessage(`""`)}

// internalErrorLog is what the Python server sends instead of anything else when a message,
// request or notification, known or not, carries params that are neither null nor an object:
// its exception handler logs to the client.
var internalErrorLog = json.RawMessage(`{"level":"error","logger":"mcp.server.exception_handler","data":"Internal Server Error"}`)

// Read answers, itself, every message Python's server rejects before dispatch, so the SDK never
// sees it. Any message whose params are present but neither null nor an object fails Python's
// parsing outright: its exception handler logs to the client and nothing else is sent. Otherwise
// a request outside ClientRequest, or a tools/call that fails CallToolRequestParams, gets
// Python's invalid-params reply, and a notification outside ClientNotification is dropped, as
// Python drops it.
func (c pythonConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	for {
		message, err := c.Connection.Read(ctx)
		if err != nil {
			return message, err
		}
		request, ok := message.(*jsonrpc.Request)
		if !ok {
			return message, nil
		}
		params := bytes.TrimSpace(request.Params)
		switch {
		case len(params) > 0 && string(params) != "null" && params[0] != '{':
			err = c.Connection.Write(ctx, &jsonrpc.Request{Method: "notifications/message", Params: internalErrorLog})
		case !request.IsCall():
			if clientNotifications[request.Method] {
				return message, nil
			}
			continue
		case clientRequests[request.Method] && (request.Method != "tools/call" || validCallParams(params)):
			return message, nil
		default:
			err = c.Connection.Write(ctx, &jsonrpc.Response{ID: request.ID, Error: invalidRequest})
		}
		if err != nil {
			return nil, err
		}
	}
}

// validCallParams is whether params pass mcp 1.30.0's CallToolRequestParams: an object whose
// name is a string and whose arguments, when present, are null or an object.
func validCallParams(params json.RawMessage) bool {
	var call struct {
		Name      *json.RawMessage `json:"name"`
		Arguments json.RawMessage  `json:"arguments"`
	}
	if len(params) == 0 || json.Unmarshal(params, &call) != nil || call.Name == nil {
		return false
	}
	var name string
	if json.Unmarshal(*call.Name, &name) != nil || string(*call.Name) == "null" {
		return false
	}
	arguments := bytes.TrimSpace(call.Arguments)
	return len(arguments) == 0 || string(arguments) == "null" || arguments[0] == '{'
}

func (c pythonConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	if response, ok := message.(*jsonrpc.Response); ok {
		var wireErr *jsonrpc.Error
		if errors.As(response.Error, &wireErr) && wireErr.Code == jsonrpc.CodeMethodNotFound {
			rewritten := *response
			rewritten.Error = methodNotFound
			message = &rewritten
		}
	}
	return c.Connection.Write(ctx, message)
}
