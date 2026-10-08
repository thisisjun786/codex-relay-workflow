//go:build dev

package cxcfuzz

import (
	"os"
	"strings"
	"testing"
)

// A worker environment that names no PATH gets the PATH the requirement check searched, so the
// worker's own lookup of a requirement finds what the check found (CRW-708 generation 5, d5). Without
// it the check passes on the process PATH while the worker, launched with no PATH, cannot find the
// same command.
func TestNewPoolGivesTheWorkerTheCheckedPath(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()+string(os.PathListSeparator)+os.Getenv("PATH"))
	pool, err := NewPool(Oracle{Command: exe}, 1, DefaultTimeout, DefaultStartupTimeout, []string{"HOME=" + t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pool.Close() }()
	for _, entry := range pool.env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			if value != os.Getenv("PATH") {
				t.Fatalf("the worker PATH %q is not the checked PATH %q", value, os.Getenv("PATH"))
			}
			return
		}
	}
	t.Fatalf("the worker environment %v names no PATH", pool.env)
}
