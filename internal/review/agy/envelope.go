// Adapted from agentic-code-reviewer (https://github.com/richhaase/agentic-code-reviewer),
// internal/agent/parser.go at commit a3e438e2bd1f0824c1eab88db738aa3c82c69e99,
// licensed under the Apache License 2.0 (see docs/port-acr/LICENSE). Modified for CRW.

package agy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// envelope is the JSON object agy prints with --output-format json. The fields are the ones R0 measured on agy 1.2.16; the envelope names no model.
type envelope struct {
	Status        string `json:"status"`
	Response      string `json:"response"`
	Error         string `json:"error"`
	Usage         Usage  `json:"usage"`
	DeniedActions []struct {
		Action      string `json:"action"`
		DisplayName string `json:"display_name"`
	} `json:"denied_actions"`
	StructuredOutput json.RawMessage `json:"structured_output"`
}

// hasStructuredOutput is true for a structured_output that is present and not null.
func (e envelope) hasStructuredOutput() bool {
	s := bytes.TrimSpace(e.StructuredOutput)
	return len(s) > 0 && !bytes.Equal(s, []byte("null"))
}

// parseEnvelope reads the first balanced JSON object in agy's stdout; text before or after it (a warning line) is ignored.
func parseEnvelope(stdout []byte) (envelope, error) {
	var env envelope
	start := bytes.IndexByte(stdout, '{')
	if start < 0 {
		return env, errors.New("agy's output holds no JSON envelope")
	}
	end, ok := balancedEnd(stdout, start)
	if !ok {
		return env, errors.New("agy's JSON envelope is cut off")
	}
	if err := json.Unmarshal(stdout[start:end], &env); err != nil {
		return envelope{}, fmt.Errorf("agy's JSON envelope is not valid: %w", err)
	}
	return env, nil
}

// balancedEnd is the offset just past the object that opens at s[idx], found by counting braces outside strings (ACR's extractBalanced, for objects only).
func balancedEnd(s []byte, idx int) (int, bool) {
	depth, inString, escape := 0, false, false
	for i := idx; i < len(s); i++ {
		ch := s[i]
		switch {
		case escape:
			escape = false
		case ch == '\\' && inString:
			escape = true
		case ch == '"':
			inString = !inString
		case inString:
		case ch == '{':
			depth++
		case ch == '}':
			if depth--; depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}
