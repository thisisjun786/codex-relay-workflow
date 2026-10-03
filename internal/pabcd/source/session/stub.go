package session

import (
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// Red-phase stub: the API with no behaviour, so that the tests run and fail on their assertions. Replaced by the port.

type refusal string

func (r refusal) Error() string { return string(r) }

type Binding struct {
	Version                                                  int
	OwnerSessionID, NativeCwd, SourceRoot, CommonDir, GitDir string
}

type worktree struct{ root, commonDir, gitDir string }

type CaptureOptions struct {
	ExcludeStateArtifacts *bool
	GeneratedPaths        []string
}

type GateResult struct {
	OK     bool
	Reason string
}

var errTodo = errors.New("not implemented")

func canonical(string) (string, error)            { return "", errTodo }
func encode(Binding) ([]byte, error)              { return nil, errTodo }
func publish(string, []byte) error                { return errTodo }
func gitIdentity(string) (worktree, error)        { return worktree{}, errTodo }
func Resolve(string, string) (string, error)      { return "", errTodo }
func Bind(string, string, string) (string, error) { return "", errTodo }
func Capture(string, string, CaptureOptions) (source.Identity, error) {
	return source.Identity{}, errTodo
}
func CheckBound(string, string) GateResult { return GateResult{Reason: "not implemented"} }
