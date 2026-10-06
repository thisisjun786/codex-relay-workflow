package hook

import (
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// RED STUB: the goal-complete place of the dispatcher, before CRW-752 fills the body. This file is replaced by
// the real port in the next commit; it exists only so the new tests fail by assertion rather than by a compile
// error, which is the red evidence the issue asks for.
type goalCompleteDeps struct {
	Invocation func() (string, error)
	ReadState  func(cwd, sessionID string) (state.State, bool)
}

func goalCompleteProcessDeps() goalCompleteDeps {
	return goalCompleteDeps{
		Invocation: func() (string, error) { return host.Invocation(os.LookupEnv) },
		ReadState:  state.ReadStateStrict,
	}
}

func goalCompleteApplyGuard(p goalGatePreToolUse, pabcdEnabled bool, deps goalCompleteDeps) string {
	return ""
}
