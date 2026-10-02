package service

// SetOpenProcess installs the opener the worker-policy reading holds its process handle through,
// until the returned restore runs.
func SetOpenProcess(open func(pid int) *ProcessHandle) (restore func()) {
	previous := openProcess
	openProcess = open
	return func() { openProcess = previous }
}
