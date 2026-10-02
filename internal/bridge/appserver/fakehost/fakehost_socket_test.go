package fakehost_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// A unix socket path of 104 bytes or more cannot be bound or dialled on darwin (108 on Linux), so
// the shared bound is 103 bytes. The fake keeps its socket in a short directory however long
// TMPDIR is.
const socketPathBound = 103

// longTMPDIR makes a directory of at least n bytes below the test's own temporary directory.
func longTMPDIR(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	for len(dir) < n {
		dir = filepath.Join(dir, strings.Repeat("d", 40))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestStart_bindsAndServes_whenTMPDIRIsLong(t *testing.T) {
	tmp := longTMPDIR(t, 120)
	t.Setenv("TMPDIR", tmp)
	server := fakehost.Start(t)
	if len(server.SocketPath) > socketPathBound {
		t.Fatalf("socket path is %d bytes, bound %d: %s", len(server.SocketPath), socketPathBound, server.SocketPath)
	}
	dial(t, server)
}

func TestStart_keepsTheSocketUnderTMPDIR_whenItFits(t *testing.T) {
	tmp, err := os.MkdirTemp("/tmp", "fh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)
	server := fakehost.Start(t)
	if !strings.HasPrefix(server.SocketPath, tmp+string(filepath.Separator)) {
		t.Fatalf("socket %s is not under the short TMPDIR %s", server.SocketPath, tmp)
	}
	dial(t, server)
}

func TestSocketPath_isShortAndRemovedWhenTheTestEnds(t *testing.T) {
	t.Setenv("TMPDIR", longTMPDIR(t, 120))
	var path string
	t.Run("inner", func(t *testing.T) {
		path = fakehost.SocketPath(t, "absent.sock")
		if len(path) > socketPathBound {
			t.Fatalf("socket path is %d bytes, bound %d: %s", len(path), socketPathBound, path)
		}
		if filepath.Base(path) != "absent.sock" {
			t.Fatalf("socket path %s does not end in the name asked for", path)
		}
		if _, err := os.Stat(filepath.Dir(path)); err != nil {
			t.Fatalf("the socket's directory is not there: %v", err)
		}
		if _, err := net.Dial("unix", path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("dialling a socket nobody made: %v, want a missing file, not an invalid path", err)
		}
	})
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the socket's directory outlived its test: %v", err)
	}
}
