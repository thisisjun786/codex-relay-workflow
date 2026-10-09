package testsupport

import (
	"os"
	"path/filepath"
	"testing"
)

// openDescriptors counts the descriptors this process holds, reading /proc/self/fd; the count includes the
// one descriptor the read itself opens, which is the same before and after.
func openDescriptors(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd: %v", err)
	}
	return len(entries)
}

// Create hands back a store with no descriptor of its own still open, so a test that creates several
// stores in one process does not exhaust the process's descriptor limit.
func TestCreateLeavesNoDescriptorOpen(t *testing.T) {
	dir := t.TempDir()
	before := openDescriptors(t)
	Create(t, filepath.Join(dir, "relay.sqlite3"), "", "go")
	if after := openDescriptors(t); after != before {
		t.Fatalf("Create left %d descriptor(s) open (before %d, after %d)", after-before, before, after)
	}
}
