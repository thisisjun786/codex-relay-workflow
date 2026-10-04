package harness

import (
	"io"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// freezeVerb is crw pabcd freeze: interview's command over the session state, which interview cannot import.
func freezeVerb(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	return interview.FreezeCommand(args, stdout, stderr, func(cwd, sessionID string) (string, *interview.Tracker) {
		s := state.ReadState(cwd, sessionID)
		return s.Slug, s.Interview
	})
}
