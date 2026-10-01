package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// rawResult is a result whose wire bytes are already decided.
type rawResult struct {
	sdk.ResultBase
	raw json.RawMessage
}

func (r *rawResult) MarshalJSON() ([]byte, error) { return r.raw, nil }

// frozenToolsList answers tools/list as contract/schema/bridge-mcp-tools.json freezes it: the
// tools in registration order (the SDK sorts them by name), their annotations without the
// idempotentHint the SDK always writes and the frozen annotations never set, and "tools" as the
// result's only member (no cache hints, no nextCursor). Every other method is the SDK's.
func frozenToolsList(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		result, err := next(ctx, method, req)
		if listed, ok := result.(*sdk.ListToolsResult); ok && err == nil {
			return toolsList(listed)
		}
		return result, err
	}
}

func toolsList(listed *sdk.ListToolsResult) (sdk.Result, error) {
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
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(map[string]any{"tools": tools}); err != nil {
		return nil, err
	}
	return &rawResult{raw: bytes.TrimSuffix(out.Bytes(), []byte("\n"))}, nil
}

// unknownTool answers a call of a tool this server does not have with an error result, not a
// JSON-RPC error, as the contract corpus fixes it.
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

// checkedArguments refuses, as an error result, a tools/call whose arguments the tool's input
// schema refuses, naming every problem in the schema's field order, so a refusal reads the same
// every time (the SDK's own validator names the first problem it meets in map order). A number
// field also takes a string that holds the number, which a model writing a call sends: it is
// read as that number before the schema judges it.
func checkedArguments(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		call, ok := req.(*sdk.CallToolRequest)
		if !ok || method != "tools/call" || call.Params == nil {
			return next(ctx, method, req)
		}
		fields := signatures[call.Params.Name]
		arguments := map[string]any{}
		if len(call.Params.Arguments) > 0 && string(call.Params.Arguments) != "null" {
			decoder := json.NewDecoder(bytes.NewReader(call.Params.Arguments))
			decoder.UseNumber()
			if decoder.Decode(&arguments) != nil {
				return next(ctx, method, req)
			}
		}
		var problems []string
		read := false
		for _, f := range fields {
			value, present := arguments[f.name]
			if number, ok := numberText(f.schema, value); ok {
				value, arguments[f.name], read = number, number, true
			}
			switch {
			case !present && f.required:
				problems = append(problems, f.name+" is required")
			case present:
				if problem := valueProblem(f.schema, value); problem != "" {
					problems = append(problems, f.name+" "+problem)
				}
			}
		}
		if len(problems) > 0 {
			text := "invalid arguments for " + call.Params.Name + ": " + strings.Join(problems, "; ")
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}, IsError: true}, nil
		}
		if read {
			rewritten, err := json.Marshal(arguments)
			if err != nil {
				return nil, err
			}
			call.Params.Arguments = rewritten
		}
		return next(ctx, method, req)
	}
}

// numberText is the number a string value of an integer or number field holds, if it holds one.
func numberText(schema map[string]any, value any) (json.Number, bool) {
	text, isString := value.(string)
	kind := schema["type"]
	if !isString || kind != "integer" && kind != "number" {
		return "", false
	}
	text = strings.TrimSpace(text)
	if kind == "integer" {
		// An integer field reads the string's decimal digits exactly, so a fraction stays a
		// fraction however close to an integer it is, and is refused.
		match := integerText.FindStringSubmatch(text)
		if match == nil {
			return "", false
		}
		exact, _ := new(big.Int).SetString(match[1], 10)
		return json.Number(exact.String()), true
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return "", false
	}
	return json.Number(strconv.FormatFloat(f, 'f', -1, 64)), true
}

// integerText is a string an integer field reads: a signed decimal, with an optional fraction of
// zeros ("5.0").
var integerText = regexp.MustCompile(`^([+-]?[0-9]+)(?:\.0+)?$`)

// valueProblem is why value does not validate against one field's schema, or "".
func valueProblem(schema map[string]any, value any) string {
	if anyOf, ok := schema["anyOf"].([]any); ok {
		// An optional field: null, or the first alternative.
		if value == nil {
			return ""
		}
		schema = anyOf[0].(map[string]any)
	}
	choices, isEnum := schema["enum"].([]any)
	if constant, ok := schema["const"]; ok {
		choices, isEnum = []any{constant}, true
	}
	if isEnum {
		if slices.Contains(choices, value) {
			return ""
		}
		quoted := make([]string, len(choices))
		for i, choice := range choices {
			quoted[i] = strconv.Quote(fmt.Sprint(choice))
		}
		return "must be one of " + strings.Join(quoted, ", ")
	}
	switch schema["type"] {
	case "string":
		if _, ok := value.(string); !ok {
			return "must be a string"
		}
	case "integer":
		number, ok := value.(json.Number)
		if f, err := number.Float64(); !ok || err != nil || f != math.Trunc(f) || f < math.MinInt64 || f >= math.MaxInt64 {
			return "must be an integer"
		}
	case "number":
		if number, ok := value.(json.Number); !ok {
			return "must be a number"
		} else if _, err := number.Float64(); err != nil {
			return "must be a number"
		}
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return "must be an object"
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			return "must be a list of strings"
		}
		for _, item := range items {
			if _, ok := item.(string); !ok {
				return "must be a list of strings"
			}
		}
	}
	return ""
}
