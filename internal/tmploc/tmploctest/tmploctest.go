// Package tmploctest holds the test helpers of tmploc: socket directories and paths for tests, each
// measured against tmploc.SocketTailMax whoever composes the path (CRW-1568), and the conventional
// roots test harnesses pick scratch work below (CRW-1580).
package tmploctest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
)

// socketPattern names the directory SocketPath makes.
const socketPattern = "crw-sock-"

// SocketDir is tmploc.SocketDir for a test: a fresh 0700 directory below tmploc.SocketRoot (never
// below TMPDIR), removed when the test ends. A refusal fails the test with tmploc's reason.
func SocketDir(tb testing.TB, pattern string) string {
	tb.Helper()
	dir, err := tmploc.SocketDir(pattern)
	if err != nil {
		tb.Fatalf("tmploctest: %v", err)
	}
	tb.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// SocketPath returns the path rel below a fresh socket directory (SocketDir), after CheckSocket has
// measured it. rel is a clean relative path; its parent directories are made (0700), so the path
// can be bound as it is.
func SocketPath(tb testing.TB, rel string) string {
	tb.Helper()
	if rel == "" || filepath.IsAbs(rel) || filepath.Clean(rel) != rel || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		tb.Fatalf("tmploctest: socket path %q is not a clean relative path below a socket directory", rel)
	}
	dir := SocketDir(tb, socketPattern)
	path := filepath.Join(dir, rel)
	CheckSocket(tb, dir, path)
	if parent := filepath.Dir(path); parent != dir {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			tb.Fatalf("tmploctest: socket path %s: %v", path, err)
		}
	}
	return path
}

// CheckSocket fails the test unless path lies below dir with a tail (the part after dir and its
// separator) of at most tmploc.SocketTailMax bytes, and the whole path fits tmploc.SocketBound. A
// test that composes a socket path itself calls it right before the bind, or before starting the
// process that binds, so a tail that grows fails in every run even while it still fits the OS.
func CheckSocket(tb testing.TB, dir, path string) {
	tb.Helper()
	cdir, cpath := filepath.Clean(dir), filepath.Clean(path)
	prefix := cdir + string(filepath.Separator)
	if cdir == string(filepath.Separator) {
		prefix = cdir
	}
	if !strings.HasPrefix(cpath, prefix) || len(cpath) == len(prefix) {
		tb.Fatalf("tmploctest: socket %q is not below its socket directory %q", path, dir)
	}
	rel := cpath[len(prefix):]
	if len(rel) > tmploc.SocketTailMax {
		tb.Fatalf("socket tail %q is %d bytes, over tmploc.SocketTailMax %d; raise it in internal/tmploc/socket.go", rel, len(rel), tmploc.SocketTailMax)
	}
	if len(path) > tmploc.SocketBound {
		if err := tmploc.SocketFits(cdir, len(rel)); err != nil {
			tb.Fatalf("tmploctest: socket %q: %v", path, err)
		}
		tb.Fatalf("tmploctest: socket %q is %d bytes, over the %d-byte bound of a unix socket path", path, len(path), tmploc.SocketBound)
	}
}

// MemoryBase is the memory-backed test harness's configured base, or its
// conventional candidate when no override was supplied. The harness still
// checks suitability and owns the directory it creates.
func MemoryBase(override string) string {
	if override != "" {
		return override
	}
	return "/dev/shm"
}

// FallbackDir chooses the first conventional base accepted by a test's path
// predicate. Tests whose command fixtures reject particular path words use
// this only after rejecting their configured temporary directory.
func FallbackDir(usable func(string) bool) (string, bool) {
	for _, dir := range []string{tmploc.DefaultDir, "/var/tmp"} {
		if usable(dir) {
			return dir, true
		}
	}
	return "", false
}
