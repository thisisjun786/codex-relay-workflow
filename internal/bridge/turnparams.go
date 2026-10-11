package bridge

import (
	"encoding/json"
	"maps"
	"path/filepath"
)

// UserInput is one element of turn/start's input. Only the text form exists here: it is the one
// every caller of the bridge sends, and a flow turn carries its operation marker and packet as text.
type UserInput struct {
	Type string
	Text string
}

// TextInput is a text element.
func TextInput(text string) UserInput { return UserInput{Type: "text", Text: text} }

// TurnStartParams is the typed form of the turn/start request members a flow backend sets. A member
// left at its zero value is not sent: the host reads an optional member as an override that lasts
// for the turn and every later one, so an empty string or null would change a thread's settings.
type TurnStartParams struct {
	ThreadID string
	Input    []UserInput
	// Model and Effort override the thread's pair. A flow backend leaves them empty after the pair
	// was read back from the thread, and sets them only where the call must state the pair.
	Model  string
	Effort string
	// OutputSchema is the JSON Schema object the final assistant message is constrained to.
	OutputSchema map[string]any
	// CWD is an absolute path.
	CWD string
	// SandboxPolicy is a complete protocol sandbox policy (see validateSandboxPolicy).
	SandboxPolicy map[string]any
}

// Map renders the request params, or names what the host would reject. The result is what
// appserver.Client.Call takes; its JSON form has sorted members, as every other turn/start frame the
// bridge writes does, so the minimal form (threadId and one text input) is byte-for-byte the frame
// the create, send and relay paths already send.
func (p TurnStartParams) Map() (map[string]any, error) {
	if err := nonempty(p.ThreadID, "threadId", 256); err != nil {
		return nil, err
	}
	if len(p.Input) == 0 {
		return nil, &Invalid{"input must contain at least one element"}
	}
	input := make([]any, 0, len(p.Input))
	for _, element := range p.Input {
		if element.Type != "text" {
			return nil, &Invalid{"input elements must be text"}
		}
		if element.Text == "" {
			return nil, &Invalid{"input text must not be empty"}
		}
		input = append(input, map[string]any{"type": "text", "text": element.Text})
	}
	params := map[string]any{"threadId": p.ThreadID, "input": input}
	if p.Model != "" {
		params["model"] = p.Model
	}
	if p.Effort != "" {
		params["effort"] = p.Effort
	}
	if p.CWD != "" {
		if !filepath.IsAbs(p.CWD) {
			return nil, &Invalid{"cwd must be an absolute path"}
		}
		params["cwd"] = p.CWD
	}
	if p.OutputSchema != nil {
		if _, err := json.Marshal(p.OutputSchema); err != nil {
			return nil, &Invalid{"outputSchema must be JSON: " + err.Error()}
		}
		params["outputSchema"] = p.OutputSchema
	}
	if p.SandboxPolicy != nil {
		if err := validateSandboxPolicy(p.SandboxPolicy); err != nil {
			return nil, err
		}
		params["sandboxPolicy"] = maps.Clone(p.SandboxPolicy)
	}
	return params, nil
}
