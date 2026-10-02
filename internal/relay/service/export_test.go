package service

import (
	"context"
	"os/exec"
)

// SetBeforeRecheck installs the hook ReadWorkerPolicy runs before it re-reads what it checked,
// until the returned restore runs.
func SetBeforeRecheck(hook func()) (restore func()) {
	previous := beforeRecheck
	beforeRecheck = hook
	return func() { beforeRecheck = previous }
}

// IdleHelper is a process of this test binary that stays alive until its stdin is closed (the
// "idle" helper): a process other than the test, found by no PATH lookup. The caller gives it a
// stdin pipe, starts it and ends it.
func IdleHelper(ctx context.Context) *exec.Cmd { return helper(ctx, "idle", "") }
