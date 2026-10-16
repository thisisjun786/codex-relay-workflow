package tmploc

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The socket folder is below SocketRoot, never below TMPDIR, whether TMPDIR is short or long:
// no run moves there because of its TMPDIR, and no run leaves it because of one.
func TestSocketDir_isNeverUnderTMPDIR(t *testing.T) {
	if SocketRoot != "/tmp" || SocketBound != 103 {
		t.Fatalf("SocketRoot, SocketBound = %q, %d, want /tmp, 103", SocketRoot, SocketBound)
	}
	short := t.TempDir()
	long := longDir(t, 120)
	for _, tmpdir := range []string{short, long, "", "rel"} {
		t.Setenv("TMPDIR", tmpdir)
		dir, err := SocketDir("crw-s-")
		if err != nil {
			t.Errorf("TMPDIR %q: SocketDir: %v", tmpdir, err)
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		if filepath.Dir(dir) != SocketRoot || !strings.HasPrefix(filepath.Base(dir), "crw-s-") {
			t.Errorf("TMPDIR %q: SocketDir = %q, want a crw-s- directory directly below %s", tmpdir, dir, SocketRoot)
		}
		if tmpdir != "" && strings.HasPrefix(dir, tmpdir+"/") {
			t.Errorf("TMPDIR %q: SocketDir = %q is below TMPDIR", tmpdir, dir)
		}
		fi, err := os.Stat(dir)
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Errorf("TMPDIR %q: SocketDir %q: %v, mode %v, want a 0700 directory", tmpdir, dir, err, fi.Mode().Perm())
		}
		if err := SocketFits(dir, SocketTailMax); err != nil {
			t.Errorf("TMPDIR %q: the socket dir does not fit SocketTailMax: %v", tmpdir, err)
		}
	}
}

// withSocketRoot points the unexported root seam at root for the test.
func withSocketRoot(t *testing.T, root string) {
	t.Helper()
	old := socketRoot
	socketRoot = root
	t.Cleanup(func() { socketRoot = old })
}

// An unusable root, or one too long to leave SocketTailMax below it, fails with the reason and
// the lengths; there is no second root.
func TestSocketDir_failsWithTheReasonWhenTheRootIsUnusable(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(base, "missing")
	cases := []struct{ name, root, why string }{
		{"missing", missing, "no such file or directory"},
		{"file", file, "not a directory"},
	}
	if os.Geteuid() != 0 {
		ro := filepath.Join(base, "ro")
		if err := os.Mkdir(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
		cases = append(cases, struct{ name, root, why string }{"unwritable", ro, "permission denied"})
	} else {
		t.Log("running as root: the unwritable row cannot be built (root writes everywhere)")
	}
	for _, c := range cases {
		withSocketRoot(t, c.root)
		dir, err := SocketDir("crw-s-")
		if err == nil {
			_ = os.RemoveAll(dir)
			t.Errorf("%s: SocketDir = %q, want an error", c.name, dir)
			continue
		}
		if msg := err.Error(); !strings.HasPrefix(msg, "socket_dir_unusable: "+c.root+": ") || !strings.Contains(msg, c.why) {
			t.Errorf("%s: SocketDir error = %q, want socket_dir_unusable naming %s and %q", c.name, msg, c.root, c.why)
		}
	}
	// a root so long that SocketTailMax no longer fits below the made directory
	long := longDir(t, 80)
	withSocketRoot(t, long)
	dir, err := SocketDir("crw-s-")
	if err == nil {
		t.Fatalf("SocketDir below a %d-byte root = %q, want the budget refusal", len(long), dir)
	}
	msg := err.Error()
	for _, want := range []string{"socket_dir_unusable: " + long + ": ", "socket_path_too_long: ", strconv.Itoa(SocketBound) + "-byte bound", "tail " + strconv.Itoa(SocketTailMax)} {
		if !strings.Contains(msg, want) {
			t.Errorf("SocketDir error = %q, want it to hold %q", msg, want)
		}
	}
	if entries, err := os.ReadDir(long); err != nil || len(entries) != 0 {
		t.Errorf("the refused socket dir was left behind: %v (%v)", entries, err)
	}
	withSocketRoot(t, base)
	if _, err := SocketDir("a/b"); err == nil || !strings.HasPrefix(err.Error(), "socket_dir_unusable: ") {
		t.Errorf("SocketDir with a separator in the pattern = %v, want a refusal", err)
	}
}

// SocketFits is exact: dir + "/" + tail = SocketBound fits, one byte more does not, and the
// refusal carries the numbers.
func TestSocketFits_isExact(t *testing.T) {
	dir := "/" + strings.Repeat("d", 39) // 40 bytes
	if err := SocketFits(dir, SocketBound-len(dir)-1); err != nil {
		t.Errorf("SocketFits(%d-byte dir, tail %d) = %v, want it to fit (= %d)", len(dir), SocketBound-len(dir)-1, err, SocketBound)
	}
	tail := SocketBound - len(dir) // 104 in all
	err := SocketFits(dir, tail)
	if err == nil {
		t.Fatalf("SocketFits(%d-byte dir, tail %d) fits, want a refusal at %d bytes", len(dir), tail, SocketBound+1)
	}
	want := "socket_path_too_long: " + dir + " (40 bytes) + 1 + tail 63 = 104 bytes, over the 103-byte bound of a unix socket path by 1"
	if err.Error() != want {
		t.Errorf("SocketFits error = %q, want %q", err.Error(), want)
	}
	if err := SocketFits(dir, -1); err == nil {
		t.Errorf("SocketFits with a negative tail fits, want a refusal")
	}
	if SocketTailMax <= 0 || len(SocketRoot)+1+len("crw-sock-")+10+1+SocketTailMax > SocketBound {
		t.Errorf("SocketTailMax %d leaves no room below %s within %d bytes", SocketTailMax, SocketRoot, SocketBound)
	}
}
