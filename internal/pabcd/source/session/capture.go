package session

import (
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// CaptureOptions are source.Options with the oracle's three states of ExcludeStateArtifacts: nil is unset, which a bound capture
// takes as true and an unbound one as false, and a value is kept.
type CaptureOptions struct {
	ExcludeStateArtifacts *bool
	GeneratedPaths        []string
	Now                   func() time.Time
}

// Capture is the source identity of the session's source work: the identity of the bound worktree, carrying that worktree's root
// and by default without the state directory, or the identity of cwd itself when the session has no binding (session state and
// evidence stay native; only the capture follows the worktree). A broken binding is an error, never a capture of cwd.
func Capture(cwd, sessionID string, o CaptureOptions) (source.Identity, error) {
	sourceCwd, err := Resolve(cwd, sessionID)
	if err != nil {
		return source.Identity{}, err
	}
	bound := sourceCwd != cwd
	opts := source.Options{ExcludeStateArtifacts: bound, GeneratedPaths: o.GeneratedPaths, Now: o.Now}
	if o.ExcludeStateArtifacts != nil {
		opts.ExcludeStateArtifacts = *o.ExcludeStateArtifacts
	}
	id := source.Capture(sourceCwd, opts)
	if bound {
		id.SourceRoot = &sourceCwd
	}
	return id, nil
}
