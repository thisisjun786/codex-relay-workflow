package role

import (
	"strings"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

type FailureDecision struct {
	Code   *string `json:"code"`
	Action string  `json:"action"`
}

// decodeDispatchFailure ports fallback-errors.ts:9-44. It classifies native
// wait data; it neither retries nor searches arbitrary task output.
func decodeDispatchFailure(value any) FailureDecision {
	if s, ok := value.(string); ok {
		s = text.Trim(s)
		if parsed, err := fallbackJSON(s); err == nil {
			return decodeDispatchFailure(parsed)
		}
		switch {
		case s == "Quota exceeded. Check your plan and billing details.":
			return fallbackCodeDecision("insufficient_quota")
		case s == "exceeded retry limit, last status: 429 Too Many Requests", strings.HasPrefix(s, "rate limit exceeded: "):
			return fallbackCodeDecision("rate_limit_exceeded")
		case s == "We're currently experiencing high demand, which may cause temporary errors.":
			return fallbackCodeDecision("upstream_server_error")
		case fallbackCodeToken(s):
			return fallbackCodeDecision(s)
		case fallbackTransportPrefix(s, "Cursor rate limit exceeded", "Rate limit reached"):
			return fallbackCodeDecision("rate_limit_exceeded")
		case fallbackTransportPrefix(s, "You've hit your usage limit", "You have exceeded your current quota"):
			return fallbackCodeDecision("insufficient_quota")
		}
		return FailureDecision{Action: "unknown"}
	}
	record, ok := value.(map[string]any)
	if !ok {
		return FailureDecision{Action: "unknown"}
	}
	if code, ok := record["code"].(string); ok {
		return fallbackCodeDecision(code)
	}
	for _, key := range []string{"error", "last_error"} {
		if v, present := record[key]; present {
			return decodeDispatchFailure(v)
		}
	}
	if response, ok := record["response"].(map[string]any); ok {
		return decodeDispatchFailure(response["error"])
	}
	return FailureDecision{Action: "unknown"}
}

func fallbackJSON(s string) (any, error) {
	return pyjson.Loads(s, pyjson.LoadOptions{Map: true, Surrogates: true, Numbers: pyjson.SpelledNumbers, Deep: true})
}

func fallbackCodeDecision(code string) FailureDecision {
	action := "stop"
	switch code {
	case "insufficient_quota", "rate_limit_exceeded", "upstream_server_error", "model_not_found", "unsupported_model", "unsupported_reasoning_effort", "input_admission_refused":
		action = "next"
	}
	return FailureDecision{Code: &code, Action: action}
}

func fallbackCodeToken(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for _, c := range []byte(s[1:]) {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

func fallbackTransportPrefix(s string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if len(s) < len(prefix) {
			continue
		}
		matches := true
		for i := range len(prefix) {
			a, b := s[i], prefix[i]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		if len(s) == len(prefix) {
			return true
		}
		r, _ := utf8.DecodeRuneInString(s[len(prefix):])
		if r == '.' || r == ':' || text.Trim(string(r)) == "" {
			return true
		}
	}
	return false
}
