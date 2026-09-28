package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Existing v1 parity cases explicitly provision a synthetic Go-owned fixture.
// Production Open never initializes a missing/unfenced store. Fence refusal
// tests below call Open directly rather than this fixture constructor.
func fixtureOpen(ctx context.Context, path, socket string) (*Store, error) {
	return fixtureOpenWith(ctx, path, socket, OpenOptions{BusyTimeout: 30 * time.Second})
}
func fixtureOpenWith(ctx context.Context, path, socket string, o OpenOptions) (*Store, error) {
	resolved, err := refuseLiveState(path)
	if err != nil {
		return nil, err
	}
	if _, err = os.Stat(filepath.Join(filepath.Dir(resolved), "takeover.json")); errors.Is(err, os.ErrNotExist) {
		if e := os.MkdirAll(filepath.Dir(resolved), 0700); e != nil {
			return nil, e
		}
		f, e := os.OpenFile(resolved, os.O_CREATE|os.O_RDWR, 0600)
		if e != nil {
			return nil, e
		}
		if e = f.Close(); e != nil {
			return nil, e
		}
		seed, e := open(ctx, path, socket, o)
		if e != nil {
			return nil, e
		}
		e = testsupport.FenceFixture(ctx, seed.DB, resolved, socket, "go")
		e = errors.Join(e, seed.Close())
		if e != nil {
			return nil, e
		}
	}
	return OpenWith(ctx, path, socket, o)
}
