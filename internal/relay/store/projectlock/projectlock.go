// Package projectlock is the per-project lock that orders a managed start against every writer of the
// project's parent binding.
//
// The lock is one file per project beside the store, `managed-start-project-<sha256 of the key>.lock`.
// A managed start takes it shared from before its last project-scope ask until its child is registered
// (the starts do not disturb each other's scope, so they never wait for one another). Anything that
// changes the status or owner of the project's parent binding takes it exclusively before its
// transaction and lets go when it returns, so such a writer waits for every start in that span, and a
// start waits for the writer before it asks about the binding at all. A start of another project, and a
// writer of another project, use another file and are never held up.
//
// Order: this lock is always taken before any store transaction and never inside one. A start holds it
// and then opens store transactions; a writer takes it holding no store lock and then opens its
// transaction. A caller that took a store lock first (a writer called inside Store.Compose, say) would
// invert that and stall every start of the project for the store's busy timeout. Nothing does today.
//
// The wait is bounded as every fence lock's is (ownership.LockWait, 30 s): a holder that never lets go
// ends it with the retryable ownership.LockWaitExpired, after nothing has changed. A cancelled context
// ends it at once. The sidecar is created once and never removed or replaced, as the other lock files
// are, so two holders can never lock different inodes.
package projectlock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// Path is the project's lock file: a sidecar beside the store, named by the project key's hash and apart
// from every request lock (managed-start-<hash>.lock).
func Path(storePath, project string) string {
	digest := sha256.Sum256([]byte(project))
	return filepath.Join(filepath.Dir(storePath), "managed-start-project-"+hex.EncodeToString(digest[:])+".lock")
}

// Shared takes the project's lock for a managed start, waiting at most ownership.LockWait. The returned
// function lets it go.
func Shared(ctx context.Context, storePath, project string) (func() error, error) {
	return take(ctx, storePath, project, false, "the managed-start project lock")
}

// Exclusive takes the project's lock for a writer of the project's parent binding, waiting at most
// ownership.LockWait for every start in the span to leave it. The returned function lets it go.
func Exclusive(ctx context.Context, storePath, project string) (func() error, error) {
	return take(ctx, storePath, project, true, "the project binding lock")
}

func take(ctx context.Context, storePath, project string, exclusive bool, what string) (func() error, error) {
	// A store with no path would put the sidecar in the working directory: refuse rather than run unlocked.
	if storePath == "" {
		return nil, errors.New("the project lock is kept beside the store, and this store has no path")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := Path(storePath, project)
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Close(fd); err != nil {
		return nil, err
	}
	held, err := ownership.LockWithin(ctx, path, exclusive, what)
	if err != nil {
		return nil, err
	}
	return held.Close, nil
}
