package tmploctest

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
)

// fakeTB records Fatalf and ends the calling goroutine the way testing.T does; the rest is the
// real test's.
type fakeTB struct {
	testing.TB
	fatal string
}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (f *fakeTB) Fatal(args ...any) {
	f.fatal = fmt.Sprint(args...)
	runtime.Goexit()
}

func (f *fakeTB) FailNow() {
	f.fatal = "FailNow"
	runtime.Goexit()
}

// run calls fn with a fakeTB in its own goroutine and reports the Fatalf message, if any.
func run(t *testing.T, fn func(tb testing.TB)) (fatal string) {
	t.Helper()
	f := &fakeTB{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(f)
	}()
	<-done
	return f.fatal
}

// The longest name SocketPath's own directory can have: root, "/", the pattern and MkdirTemp's
// random part (a uint32 in decimal, at most 10 digits).
var longestSocketDir = len(tmploc.SocketRoot) + 1 + len(socketPattern) + 10

// A tail one byte over SocketTailMax fails SocketPath with the constant's name, even though the
// whole path still fits the operating system's bound: the test measures the tail, not the OS.
func TestSocketPath_failsATailOverSocketTailMax_evenWhenItFitsTheOS(t *testing.T) {
	rel := strings.Repeat("s", tmploc.SocketTailMax+1)
	if longestSocketDir+1+len(rel) > tmploc.SocketBound {
		t.Fatalf("precondition: %d + 1 + %d is over SocketBound %d; the case would test the OS bound", longestSocketDir, len(rel), tmploc.SocketBound)
	}
	var got string
	fatal := run(t, func(tb testing.TB) { got = SocketPath(tb, rel) })
	want := fmt.Sprintf("socket tail %q is %d bytes, over tmploc.SocketTailMax %d; raise it in internal/tmploc/socket.go", rel, len(rel), tmploc.SocketTailMax)
	if fatal != want {
		t.Errorf("SocketPath(%d-byte rel) = %q, fatal %q, want fatal %q", len(rel), got, fatal, want)
	}
	// SocketTailMax itself is accepted, its parent directories are made, and the path binds
	rel = strings.Repeat("d", 20) + "/" + strings.Repeat("s", tmploc.SocketTailMax-21)
	var path string
	if fatal := run(t, func(tb testing.TB) { path = SocketPath(tb, rel) }); fatal != "" {
		t.Fatalf("SocketPath(%d-byte rel): %s", len(rel), fatal)
	}
	if !strings.HasPrefix(path, tmploc.SocketRoot+"/") || !strings.HasSuffix(path, "/"+rel) {
		t.Errorf("SocketPath = %q, want <socket dir>/%s", path, rel)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("binding %q (%d bytes): %v", path, len(path), err)
	}
	_ = ln.Close()
	for _, bad := range []string{"", ".", "../escape.sock", "/abs.sock"} {
		if fatal := run(t, func(tb testing.TB) { SocketPath(tb, bad) }); fatal == "" {
			t.Errorf("SocketPath(%q) passed, want a refusal", bad)
		}
	}
}

// A socket path a test composes itself is measured the same way: CheckSocket fails an over-long
// tail, passes one of exactly SocketTailMax, and fails a path that is not below the directory.
func TestCheckSocket_measuresAComposedPath(t *testing.T) {
	var dir string
	if fatal := run(t, func(tb testing.TB) { dir = SocketDir(tb, "crw-check-") }); fatal != "" {
		t.Fatal(fatal)
	}
	if len(dir)+1+tmploc.SocketTailMax+1 > tmploc.SocketBound {
		t.Fatalf("precondition: %q leaves no room for a tail over SocketTailMax within the OS bound", dir)
	}
	name := strings.Repeat("n", tmploc.SocketTailMax+1-len("state/"))
	over := dir + "/state/" + name
	fatal := run(t, func(tb testing.TB) { CheckSocket(tb, dir, over) })
	want := fmt.Sprintf("socket tail %q is %d bytes, over tmploc.SocketTailMax %d; raise it in internal/tmploc/socket.go", "state/"+name, tmploc.SocketTailMax+1, tmploc.SocketTailMax)
	if fatal != want {
		t.Errorf("CheckSocket(over) fatal = %q, want %q", fatal, want)
	}
	exact := dir + "/state/" + name[1:]
	if fatal := run(t, func(tb testing.TB) { CheckSocket(tb, dir, exact) }); fatal != "" {
		t.Errorf("CheckSocket(a %d-byte tail) = %q, want it to pass", tmploc.SocketTailMax, fatal)
	}
	for _, outside := range []string{"/elsewhere/control.sock", dir + "-x/control.sock", dir, dir + "/../control.sock", filepath.Dir(dir)} {
		fatal := run(t, func(tb testing.TB) { CheckSocket(tb, dir, outside) })
		if !strings.Contains(fatal, "is not below its socket directory") {
			t.Errorf("CheckSocket(%q) fatal = %q, want the not-below refusal", outside, fatal)
		}
	}
	// a directory that is not a socket dir is measured too: the whole path must fit the OS bound
	longDir := "/" + strings.Repeat("l", tmploc.SocketBound-10)
	fatal = run(t, func(tb testing.TB) { CheckSocket(tb, longDir, longDir+"/state/c.sock") })
	if !strings.Contains(fatal, "socket_path_too_long: ") {
		t.Errorf("CheckSocket below a %d-byte dir fatal = %q, want socket_path_too_long", len(longDir), fatal)
	}
}

// The directory SocketDir makes is gone once the test that asked for it ends, and it is below
// SocketRoot, not below TMPDIR.
func TestSocketDir_isRemovedWhenTheTestEnds(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	var dir, path string
	t.Run("inner", func(t *testing.T) {
		dir = SocketDir(t, "crw-gone-")
		path = filepath.Join(dir, "x.sock")
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		_ = ln.Close()
		if err := os.WriteFile(filepath.Join(dir, "left"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if dir == "" || filepath.Dir(dir) != tmploc.SocketRoot {
		t.Fatalf("SocketDir = %q, want a directory below %s", dir, tmploc.SocketRoot)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("socket dir %q after the test ended: %v, want it removed", dir, err)
	}
}
