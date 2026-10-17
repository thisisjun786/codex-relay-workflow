package fakehost_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
)

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
	if len(server.SocketPath) > tmploc.SocketBound {
		t.Fatalf("socket path is %d bytes, bound %d: %s", len(server.SocketPath), tmploc.SocketBound, server.SocketPath)
	}
	dial(t, server)
}

// TestSocketPath_isNeverUnderTMPDIR replaces the removed "TMPDIR first, /tmp when long" rule
// (CRW-1568): the socket directory is made below tmploc.SocketRoot whatever TMPDIR is. The test
// makes its two TMPDIR cases itself, so the run's own TMPDIR does not matter: a short one, directly
// below tmploc.DefaultDir, where a socket would fit under TMPDIR, and a long one below it, where a
// socket would not. Each case checks the socket that SocketPath and Start make under its TMPDIR.
func TestSocketPath_isNeverUnderTMPDIR(t *testing.T) {
	short, err := tmploc.MkdirTempIn(tmploc.DefaultDir, "fh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	long := filepath.Join(short, strings.Repeat("d", 120))
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(short) != tmploc.DefaultDir {
		t.Fatalf("short TMPDIR %s is not directly below %s", short, tmploc.DefaultDir)
	}
	if err := tmploc.SocketFits(short, tmploc.SocketTailMax); err != nil {
		t.Fatalf("short TMPDIR %s holds no socket within the bound, so it is not a short case: %v", short, err)
	}
	if filepath.Dir(long) != short || len(long) <= tmploc.SocketBound {
		t.Fatalf("long TMPDIR %s is not directly below %s or is within the %d-byte bound", long, short, tmploc.SocketBound)
	}
	for _, c := range []struct{ name, dir string }{{"short", short}, {"long", long}} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("TMPDIR", c.dir)
			for name, path := range map[string]func(t *testing.T) string{
				"SocketPath": func(t *testing.T) string { return fakehost.SocketPath(t, "absent.sock") },
				"Start": func(t *testing.T) string {
					server := fakehost.Start(t)
					dial(t, server)
					return server.SocketPath
				},
			} {
				t.Run(name, func(t *testing.T) {
					got := path(t)
					if strings.HasPrefix(got, c.dir+string(filepath.Separator)) {
						t.Fatalf("socket %s lies under TMPDIR %s", got, c.dir)
					}
					if parent := filepath.Dir(filepath.Dir(got)); parent != tmploc.SocketRoot {
						t.Fatalf("socket %s is not in a directory directly below tmploc.SocketRoot %s (its directory lies in %s)", got, tmploc.SocketRoot, parent)
					}
					if len(got) > tmploc.SocketBound {
						t.Fatalf("socket path is %d bytes, bound %d: %s", len(got), tmploc.SocketBound, got)
					}
				})
			}
		})
	}
}

func TestSocketPath_isShortAndRemovedWhenTheTestEnds(t *testing.T) {
	t.Setenv("TMPDIR", longTMPDIR(t, 120))
	var path string
	t.Run("inner", func(t *testing.T) {
		path = fakehost.SocketPath(t, "absent.sock")
		if len(path) > tmploc.SocketBound {
			t.Fatalf("socket path is %d bytes, bound %d: %s", len(path), tmploc.SocketBound, path)
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
