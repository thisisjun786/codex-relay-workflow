package hook

import (
	"os"
	"testing"
)

// socketDir is a temporary directory for a test whose unix socket (control.sock, a fake owner's listener) must fit a sockaddr_un
// (CRW-1393): it is made under /tmp whatever TMPDIR names, so a long TMPDIR (a lane's /scratch/lt/<name>, over 100 bytes) never
// pushes the socket path past the 104 (darwin) or 108 (Linux) byte limit, which a bind or connect answers with EINVAL. It is
// removed when the test ends.
func socketDir(t testing.TB, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}
