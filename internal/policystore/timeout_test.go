package policystore

import (
	"testing"
	"time"
)

// timeoutAfter is the bound the blocking-read tests wait before they fail.
func timeoutAfter(t *testing.T) <-chan time.Time {
	t.Helper()
	return time.After(5 * time.Second)
}
