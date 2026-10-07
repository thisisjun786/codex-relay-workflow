package policystore

import (
	"syscall"
)

// mkfifo makes a named pipe for the non-blocking-read test.
func mkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }
