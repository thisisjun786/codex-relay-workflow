//go:build dev

package laneparity

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

// OwnProbe is a scenario for a registration CRW holds that the corpus has no fixture for: the
// GitHub post guard and the completion Stop. It is judged by Check against the contract the leg's
// own tests and known-defects entries state, not against a recording.
type OwnProbe struct {
	ID       string
	Leg      string
	Silent   bool // the probe expects no answer: a command that does nothing passes it
	Scenario cxccorpus.Scenario
	Check    func(cxccorpus.Expect) error
}

func bashPayload(command string) json.RawMessage {
	return stdinJSON(map[string]any{
		"hook_event_name": "PreToolUse", "session_id": "parity-s1", "cwd": "${WS}", "turn_id": "parity-t1",
		"tool_name": "Bash", "tool_input": map[string]any{"command": command}, "tool_use_id": "parity-call-1",
	})
}

func oneStep(e cxccorpus.Expect) (cxccorpus.StepResult, error) {
	if len(e.Steps) != 1 {
		return cxccorpus.StepResult{}, fmt.Errorf("%d steps ran, expected 1", len(e.Steps))
	}
	return e.Steps[0], nil
}

// OwnProbes are the probes of the two registrations beyond K1.
func OwnProbes() []OwnProbe {
	deny := OwnProbe{
		ID: "github-post-denies-inline-body", Leg: GitHubPostLeg,
		Scenario: cxccorpus.Scenario{Steps: []cxccorpus.Step{{Hook: GitHubPostLeg, Stdin: bashPayload(`gh pr comment 1 --body "inline text"`)}}},
		Check: func(e cxccorpus.Expect) error {
			s, err := oneStep(e)
			if err != nil {
				return err
			}
			var out struct {
				Hook struct {
					Decision string `json:"permissionDecision"`
					Reason   string `json:"permissionDecisionReason"`
				} `json:"hookSpecificOutput"`
			}
			if s.Exit != 0 || json.Unmarshal([]byte(s.StdoutBytes()), &out) != nil || out.Hook.Decision != "deny" || !strings.Contains(out.Hook.Reason, "inline-github-body") {
				return fmt.Errorf("an inline GitHub post body was not denied by rule inline-github-body: exit %d, stdout %.200q", s.Exit, s.StdoutBytes())
			}
			return nil
		},
	}
	allow := OwnProbe{
		ID: "github-post-silent-for-unrelated-command", Leg: GitHubPostLeg, Silent: true,
		Scenario: cxccorpus.Scenario{Steps: []cxccorpus.Step{{Hook: GitHubPostLeg, Stdin: bashPayload("ls -la")}}},
		Check: func(e cxccorpus.Expect) error {
			s, err := oneStep(e)
			if err != nil {
				return err
			}
			if s.Exit != 0 || s.StdoutForm != "empty" {
				return fmt.Errorf("an unrelated command was answered: exit %d, stdout %.200q", s.Exit, s.StdoutBytes())
			}
			return nil
		},
	}
	stop := OwnProbe{
		ID: "completion-stop-releases-without-settings", Leg: CompletionLeg, Silent: true,
		Scenario: cxccorpus.Scenario{Steps: []cxccorpus.Step{{Hook: CompletionLeg, Stdin: stdinJSON(map[string]any{
			"hook_event_name": "Stop", "session_id": "parity-s1", "cwd": "${WS}", "turn_id": "parity-t1",
		})}}},
		Check: func(e cxccorpus.Expect) error {
			s, err := oneStep(e)
			if err != nil {
				return err
			}
			if s.Exit != 0 || s.StdoutForm != "empty" || s.Stderr != "" {
				return fmt.Errorf("a Stop with no completion settings did not release in silence: exit %d, stdout %.200q, stderr %.200q", s.Exit, s.StdoutBytes(), s.Stderr)
			}
			return nil
		},
	}
	return []OwnProbe{deny, allow, stop}
}
