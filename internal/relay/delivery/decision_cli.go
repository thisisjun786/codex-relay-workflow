package delivery

import (
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

// decision-reply and decision-show: the parent's reply to a child's blocked_needs_input receipt, and its read-back
// (docs/relay/README.md, "Replying to a blocked receipt").

func cmdDecisionReply(c *cliRun) (any, error) {
	note := c.s("--note")
	if strings.HasPrefix(note, "@") {
		content, err := os.ReadFile(note[1:])
		if err != nil {
			return nil, dispatch.Host("--note could not be read: " + err.Error())
		}
		note = string(content)
	}
	_, ack, err := c.services()
	if err != nil {
		return nil, err
	}
	return ack.RecordDecision(c.ctx, DecisionRequest{EventID: c.s("--event"), Decision: c.s("--decision"), Turn: c.s("--decision-turn"), Note: note, CriteriaDigest: c.s("--criteria-digest")})
}

func cmdDecisionShow(c *cliRun) (any, error) {
	_, ack, err := c.services()
	if err != nil {
		return nil, err
	}
	return ack.DecisionsOf(c.ctx, c.s("--relationship"))
}
