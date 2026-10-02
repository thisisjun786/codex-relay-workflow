//go:build !linux

package service

import (
	"fmt"
	"os"
)

// runPlatformHelper has no role here: the controller and the leader-gone worker are Linux helpers
// (reap_test.go, which only Linux builds).
func runPlatformHelper(role, _ string) int {
	fmt.Fprintf(os.Stderr, "unknown helper %q\n", role)
	return 2
}
