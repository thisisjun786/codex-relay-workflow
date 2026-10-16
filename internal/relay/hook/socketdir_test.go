package hook

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc/tmploctest"
)

// socketDir is a temporary directory for a test whose unix socket (control.sock, a fake owner's listener) must fit a sockaddr_un
// (CRW-1393): a socket directory (tmploctest.SocketDir, below tmploc.SocketRoot), never below TMPDIR, so no TMPDIR length pushes the
// socket path past the 104 (darwin) or 108 (Linux) byte limit, which a bind or connect answers with EINVAL (CRW-1568). It is removed
// when the test ends.
func socketDir(t testing.TB, prefix string) string {
	t.Helper()
	return tmploctest.SocketDir(t, prefix)
}

// socketRootOf is the socket directory path lies in: the directory right below tmploc.SocketRoot on its way (a socketDir, or the
// fixed canonicalFixtureRoot). tmploctest.CheckSocket measures the tail below it before a bind; a path that is not below
// tmploc.SocketRoot fails the test, because its length would follow TMPDIR's.
func socketRootOf(t testing.TB, path string) string {
	t.Helper()
	rel, err := filepath.Rel(tmploc.SocketRoot, path)
	if err != nil || !filepath.IsAbs(path) || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("socket %s is not below %s: make its directory with socketDir", path, tmploc.SocketRoot)
	}
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	return filepath.Join(tmploc.SocketRoot, first)
}
