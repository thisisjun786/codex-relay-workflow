package main

import (
	"io"
	"os"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/affordance"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/job"
)

// componentHook is an ingress outside the PABCD stage table. Each component owns
// its stdin policy, observation name and answer; this dispatcher owns only routing.
type componentHook struct {
	ID, Event string
	Run       func(invocation, io.Reader) int
}

// componentHooks is the row-addition point for later provider-bridge and cxc-ops
// hooks. Their rows require no changes to dispatch or the bg-wake implementation.
func componentHooks() []componentHook {
	bg := func(event string) func(invocation, io.Reader) int {
		return func(c invocation, in io.Reader) int {
			cwd, err := os.Getwd()
			if err != nil {
				return 0
			}
			return job.RunHook(c.ctx, event, in, c.stdout, os.LookupEnv, cwd, time.Now)
		}
	}
	aff := func(event string) func(invocation, io.Reader) int {
		return func(c invocation, in io.Reader) int {
			cwd, _ := os.Getwd()
			return affordance.RunHook(c.ctx, event, in, c.stdout, os.LookupEnv, cwd)
		}
	}
	return []componentHook{
		{"stop-waking-on-background-completion", "stop", bg("stop")},
		{"user-prompt-submit-delivering-background-completions", "user-prompt-submit", bg("user-prompt-submit")},
		{"session-start-adopting-background-completions", "session-start", bg("session-start")},
		{"session-start-announcing-map-affordance", "session-start", aff("session-start")},
		{"post-compact-injecting-bg-terminal-affordance.post-compact", "post-compact", aff("post-compact")},
		{"post-compact-injecting-bg-terminal-affordance.user-prompt-submit", "user-prompt-submit", aff("user-prompt-submit")},
	}
}

func runComponentHook(c invocation, in io.Reader, rows []componentHook) (bool, int) {
	var id string
	switch {
	case len(c.args) == 3 && c.args[1] == "--leg":
		id = c.args[2]
	case len(c.args) == 2 && len(c.args[1]) >= 6 && c.args[1][:6] == "--leg=":
		id = c.args[1][6:]
	}
	for _, row := range rows {
		if id != "" && row.ID == id && row.Event == c.args[0] {
			return true, row.Run(c, in)
		}
	}
	return false, 0
}
