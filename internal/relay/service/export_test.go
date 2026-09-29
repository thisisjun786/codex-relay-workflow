package service

// SetBeforeRecheck installs the hook ReadWorkerPolicy runs before it re-reads what it checked,
// until the returned restore runs.
func SetBeforeRecheck(hook func()) (restore func()) {
	previous := beforeRecheck
	beforeRecheck = hook
	return func() { beforeRecheck = previous }
}
