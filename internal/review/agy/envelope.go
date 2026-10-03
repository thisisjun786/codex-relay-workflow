package agy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// parseEnvelope reads stdout as the one JSON object agy prints. Anything else (no object, a second object, text around it) is an error: a call whose output
// cannot be read whole is not read at all.
func parseEnvelope(stdout []byte) (envelope, error) {
	var env envelope
	dec := json.NewDecoder(bytes.NewReader(stdout))
	if err := dec.Decode(&env); err != nil {
		return envelope{}, fmt.Errorf("agy's output is not a JSON envelope: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return envelope{}, errors.New("agy's output holds more than one JSON envelope")
	}
	return env, nil
}
