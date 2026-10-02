package registry

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/projectlock"
)

// lockProjectParent is the exclusive project lock a writer of a project's parent binding holds while it
// changes the binding. It is taken after the writer's arguments are validated and before its store
// transaction, and let go when the writer returns: a managed start holds the same lock shared from before
// its last project-scope ask until its child is registered, so this writer waits for every start in that
// span and no start asks about a binding that is changing. Only the parent role of a project is a
// project's parent binding; the child and supervisor roles bind issues and initiatives and take nothing.
//
// A writer that waits out the bound answers the retryable ownership.LockWaitExpired, as the host envelope
// (exit 3), having changed nothing. The lock is taken before any store lock and must never be taken inside
// Store.Compose or another transaction: that would invert the order and stall every start of the project.
func (r *Registry) lockProjectParent(ctx context.Context, role, project string) (func(), error) {
	if role != roleParent {
		return func() {}, nil
	}
	release, err := projectlock.Exclusive(ctx, r.Store.Path, project)
	if err != nil {
		return nil, err
	}
	return func() { _ = release() }, nil
}
