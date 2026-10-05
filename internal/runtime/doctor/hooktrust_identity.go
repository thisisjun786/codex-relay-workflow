// Hook trust identity, ported from CXC v0.2.40 hook-trust.ts:18-38 (EVENT_LABELS,
// MATCHER_DROPPED_EVENTS), :40-47 (HookHandler) and :96-139 (assertSupportedPlatform,
// sorted, identityHash), commit 3c1459acadeb1906d97c00a598e1457327ae372d. Codex records a
// hook's trust as the sha256 of the JSON.stringify of the sorted identity;
// HookTrustIdentityHash answers that identity and nothing else: it reads no file, holds no
// package state and needs no Node.
//
// handler is one handler object of a hook document (hook-trust.ts:40-47). Its values are the
// ones an encoding/json or pyjson reader produces: string, bool, nil, float64, json.Number or
// the Go integer kinds for numbers (all read as the double JavaScript held), and map[string]any
// or []any for containers; pyjson.Object is read as an object too. A Go value of any other
// type is outside that input and is spelled with %v in a refusal.
//
// Two CXC behaviours are kept as the oracle has them, not repaired (one line each in
// docs/port-cxc/known-defects.md): an event that is a JavaScript Object.prototype member is
// accepted (the eleven function members hash with no event_name key, __proto__ with an empty
// event_name object), and an object value with an own toString member makes the type refusal
// throw the raw TypeError "Cannot convert object to primitive value".
package doctor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// hookTrustIdentityKind is what an event name resolved to (hook-trust.ts:18-29, :109): one of
// the ten labels, or a member a JavaScript object inherits from Object.prototype.
type hookTrustIdentityKind int

const (
	hookTrustIdentityLabel hookTrustIdentityKind = iota
	// A function member of Object.prototype: EVENT_LABELS[event] finds it, JSON.stringify
	// writes no event_name for it, and the identity holds only the hooks (and the matcher).
	hookTrustIdentityFunction
	// __proto__: Object.prototype itself, written as an empty object.
	hookTrustIdentityProto
)

// HookTrustIdentityHash is identityHash (hook-trust.ts:96-139): "sha256:" plus the hex
// digest of sha256 over the canonical JSON of a command hook's trust identity. The event
// label decides the identity, the matcher is dropped for UserPromptSubmit and Stop only, and
// the handler normalizes to type command with the clamped timeout and the defaulted async.
func HookTrustIdentityHash(event string, matcher *string, handler map[string]any) (string, error) {
	kind, label, known := hookTrustIdentityEvent(event)
	if !known {
		return "", errors.New("unsupported hook event: " + event)
	}
	if value, present := handler["type"]; present {
		if text, literal := value.(string); !literal || text != "command" {
			spelled, ok := hookTrustIdentityJSString(value)
			if !ok {
				return "", errors.New("Cannot convert object to primitive value")
			}
			return "", errors.New("unsupported hook handler type: " + spelled)
		}
	}
	command, ok := handler["command"].(string)
	if !ok {
		return "", errors.New("hook command must be a string")
	}
	timeout, err := hookTrustIdentityTimeout(handler["timeout"])
	if err != nil {
		return "", err
	}
	async, err := hookTrustIdentityAsync(handler["async"])
	if err != nil {
		return "", err
	}
	statusMessage, hasStatus, err := hookTrustIdentityStatusMessage(handler["statusMessage"])
	if err != nil {
		return "", err
	}
	normalized := map[string]any{
		"type":    "command",
		"command": command,
		"timeout": json.Number(hookTrustIdentityJSNumber(timeout)),
		"async":   async,
	}
	if hasStatus {
		normalized["statusMessage"] = statusMessage
	}
	identity := map[string]any{"hooks": []any{normalized}}
	switch kind {
	case hookTrustIdentityLabel:
		identity["event_name"] = label
	case hookTrustIdentityProto:
		identity["event_name"] = map[string]any{}
	}
	if !hookTrustIdentityMatcherDropped(event) && matcher != nil {
		identity["matcher"] = *matcher
	}
	canonical := pyjson.Dumps(identity, pyjson.Options{Compact: true, SortKeys: true, Unicode: true})
	digest := sha256.Sum256([]byte(canonical))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// hookTrustIdentityEvent is EVENT_LABELS[event] (hook-trust.ts:18-29, :109) with its
// prototype chain: a name the table holds is its label; the eleven function members a
// JavaScript object inherits from Object.prototype resolve to a function (kept, see the
// package comment); __proto__ resolves to Object.prototype itself; every other name has no
// entry.
func hookTrustIdentityEvent(event string) (hookTrustIdentityKind, string, bool) {
	switch event {
	case "PreToolUse":
		return hookTrustIdentityLabel, "pre_tool_use", true
	case "PostToolUse":
		return hookTrustIdentityLabel, "post_tool_use", true
	case "SessionStart":
		return hookTrustIdentityLabel, "session_start", true
	case "UserPromptSubmit":
		return hookTrustIdentityLabel, "user_prompt_submit", true
	case "Stop":
		return hookTrustIdentityLabel, "stop", true
	case "SubagentStart":
		return hookTrustIdentityLabel, "subagent_start", true
	case "SubagentStop":
		return hookTrustIdentityLabel, "subagent_stop", true
	case "PreCompact":
		return hookTrustIdentityLabel, "pre_compact", true
	case "PostCompact":
		return hookTrustIdentityLabel, "post_compact", true
	case "PermissionRequest":
		return hookTrustIdentityLabel, "permission_request", true
	case "constructor", "toString", "toLocaleString", "valueOf", "hasOwnProperty",
		"isPrototypeOf", "propertyIsEnumerable", "__defineGetter__", "__defineSetter__",
		"__lookupGetter__", "__lookupSetter__":
		return hookTrustIdentityFunction, "", true
	case "__proto__":
		return hookTrustIdentityProto, "", true
	}
	return hookTrustIdentityLabel, "", false
}

// hookTrustIdentityMatcherDropped is MATCHER_DROPPED_EVENTS.has(event) (hook-trust.ts:36).
func hookTrustIdentityMatcherDropped(event string) bool {
	return event == "UserPromptSubmit" || event == "Stop"
}

// hookTrustIdentityTimeout is Math.max(handler.timeout ?? 600, 1) (hook-trust.ts:104-107,
// :120): a missing or null timeout is 600 and anything else has to be a finite number.
func hookTrustIdentityTimeout(value any) (float64, error) {
	if value == nil {
		return 600, nil
	}
	number, ok := hookTrustIdentityFloat(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, errors.New("hook timeout must be a finite number")
	}
	return math.Max(number, 1), nil
}

// hookTrustIdentityAsync is handler.async ?? false (hook-trust.ts:104, :122).
func hookTrustIdentityAsync(value any) (bool, error) {
	if value == nil {
		return false, nil
	}
	flag, ok := value.(bool)
	if !ok {
		return false, errors.New("hook async must be a boolean")
	}
	return flag, nil
}

// hookTrustIdentityStatusMessage keeps the status message only when it is not null
// (hook-trust.ts:104, :124).
func hookTrustIdentityStatusMessage(value any) (string, bool, error) {
	if value == nil {
		return "", false, nil
	}
	message, ok := value.(string)
	if !ok {
		return "", false, errors.New("hook statusMessage must be a string")
	}
	return message, true, nil
}

// hookTrustIdentityFloat reads a JSON number as the double JavaScript held: a json.Number
// through Float64 (a spelling past the double range is an error, as the oracle's Infinity is
// not finite), every Go integer kind and float32 through float64 (JSON.parse rounds an
// integer above 2^53 the same way).
func hookTrustIdentityFloat(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int8:
		return float64(number), true
	case int16:
		return float64(number), true
	case int32:
		return float64(number), true
	case int64:
		return float64(number), true
	case uint:
		return float64(number), true
	case uint8:
		return float64(number), true
	case uint16:
		return float64(number), true
	case uint32:
		return float64(number), true
	case uint64:
		return float64(number), true
	case json.Number:
		read, err := number.Float64()
		if err != nil {
			return 0, false
		}
		return read, true
	}
	return 0, false
}

// hookTrustIdentityJSNumber is JavaScript's Number::toString (the spelling JSON.stringify
// prints): the shortest round-trip digits Go and V8 both give, in the fixed notation below
// 1e21 that JavaScript uses and the exponential one at or past it. The oracle's clamped
// timeout is at least 1, so inside the identity only the fixed branch and an exponent of at
// least two digits can occur; the other shapes are reachable through a refused type value.
func hookTrustIdentityJSNumber(number float64) string {
	switch {
	case math.IsNaN(number):
		return "NaN"
	case math.IsInf(number, 1):
		return "Infinity"
	case math.IsInf(number, -1):
		return "-Infinity"
	case number == 0:
		return "0"
	case math.Abs(number) >= 1e21 || math.Abs(number) < 1e-6:
		return hookTrustIdentityExponent(strconv.FormatFloat(number, 'e', -1, 64))
	}
	return strconv.FormatFloat(number, 'f', -1, 64)
}

// hookTrustIdentityExponent drops the leading zero Go writes for a one-digit exponent
// ("1e-07" is JavaScript's "1e-7"); an exponent of two digits or more is already shared.
func hookTrustIdentityExponent(text string) string {
	index := strings.LastIndexByte(text, 'e')
	if index < 0 {
		return text
	}
	mantissa, exponent := text[:index+1], text[index+1:]
	sign := ""
	if len(exponent) > 0 && (exponent[0] == '+' || exponent[0] == '-') {
		sign, exponent = exponent[:1], exponent[1:]
	}
	if len(exponent) > 1 && exponent[0] == '0' {
		exponent = exponent[1:]
	}
	return mantissa + sign + exponent
}

// hookTrustIdentityJSString spells a JSON value the way JavaScript's String() does, the
// spelling the oracle interpolates into its type refusal (hook-trust.ts:112). The second
// result is false where that conversion throws: an object with an own toString member has no
// callable toString to return a primitive, so the oracle's template literal throws the raw
// TypeError "Cannot convert object to primitive value" (kept; docs/port-cxc/known-defects.md).
func hookTrustIdentityJSString(value any) (string, bool) {
	switch item := value.(type) {
	case nil:
		return "null", true
	case string:
		return item, true
	case bool:
		return strconv.FormatBool(item), true
	case map[string]any:
		if _, own := item["toString"]; own {
			return "", false
		}
		return "[object Object]", true
	case pyjson.Object:
		for _, field := range item {
			if field.Key == "toString" {
				return "", false
			}
		}
		return "[object Object]", true
	case []any:
		parts := make([]string, 0, len(item))
		for _, element := range item {
			if element == nil {
				parts = append(parts, "")
				continue
			}
			text, ok := hookTrustIdentityJSString(element)
			if !ok {
				return "", false
			}
			parts = append(parts, text)
		}
		return strings.Join(parts, ","), true
	}
	if number, ok := hookTrustIdentityFloat(value); ok {
		return hookTrustIdentityJSNumber(number), true
	}
	return fmt.Sprintf("%v", value), true
}
