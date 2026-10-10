package session

import (
	"context"
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
	id, _, err := capture(prober{}, cwd, sessionID, o)
	return id, err
}

// CaptureContext is Capture for a caller that must not leave work running behind it (the Stop hook, which gives up waiting): the
// git probes of the binding and of the capture are owned by ctx (their process groups are killed when it ends and reaped before
// the call returns), and the hashing stops once it has ended. probe is why the identity is unavailable when git could not
// answer (source.CaptureContext's error; nil for a resolved identity): source.NotARepository(probe) is a directory in no
// repository, any other value a probe that failed. err is a binding or source-root error, as in Capture.
func CaptureContext(ctx context.Context, cwd, sessionID string, o CaptureOptions) (id source.Identity, probe, err error) {
	return capture(prober{ctx}, cwd, sessionID, o)
}

func capture(p prober, cwd, sessionID string, o CaptureOptions) (source.Identity, error, error) {
	sourceCwd, err := resolve(p, cwd, sessionID)
	if err != nil {
		return source.Identity{}, nil, err
	}
	bound := sourceCwd != cwd
	opts := source.Options{ExcludeStateArtifacts: bound, GeneratedPaths: o.GeneratedPaths, Now: o.Now}
	if o.ExcludeStateArtifacts != nil {
		opts.ExcludeStateArtifacts = *o.ExcludeStateArtifacts
	}
	var id source.Identity
	var probe error
	if p.ctx != nil {
		id, probe = source.CaptureContext(p.ctx, sourceCwd, opts)
	} else {
		id, probe = source.CaptureProbed(sourceCwd, opts)
	}
	if bound {
		id.SourceRoot = &sourceCwd
	}
	return id, probe, nil
}
