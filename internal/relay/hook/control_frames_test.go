package hook

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// socketPair is a connected pair of stream sockets: the owner's end and the peer's.
func socketPair(t *testing.T) (owner, peer *net.UnixConn) {
	t.Helper()
	// Darwin has no SOCK_CLOEXEC: mark both ends close-on-exec under ForkLock instead, so a
	// concurrent exec (the Python oracle) inherits neither.
	syscall.ForkLock.RLock()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.CloseOnExec(fds[0])
		unix.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	ends := make([]*net.UnixConn, 2)
	for i, fd := range fds {
		file := os.NewFile(uintptr(fd), "control-pair")
		conn, err := net.FileConn(file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		ends[i] = conn.(*net.UnixConn)
	}
	return ends[0], ends[1]
}

// PR #185 4128954449 review: the owner reads a request as control.py _answer reads it. Over a
// corpus of frames (testdata/control_frames.py), HandleControl answers each with the line the
// retained Python owner answers it with: the bytes decoded as json.loads decodes bytes (a UTF-8
// byte order mark skipped, UTF-16 and UTF-32 detected, a lone surrogate passed and kept as the
// one character it is), and the deadline read as CPython 3.13's datetime.fromisoformat reads it
// (every date, separator, time and offset form, a naive value's TypeError, the fields' range
// errors in 3.13's words). A request admitted past its deadline is answered with owner_paths'
// refusal, so the corpus evaluates nothing.
func TestControlReadsEveryFrameAsControlPyReadsIt(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_SESSION_RELAY_MARKER_ROOT", root)
	oracle := exec.Command(python(t), "testdata/control_frames.py", state)
	var stderr bytesBuffer
	oracle.Stderr = &stderr
	out, err := oracle.Output()
	if err != nil {
		t.Fatalf("control_frames.py: %v %s", err, stderr)
	}
	var corpus []struct{ Name, Frame, Answer string }
	if err = json.Unmarshal(out, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus) < 1000 {
		t.Fatalf("a corpus of %d frames", len(corpus))
	}
	differ := 0
	for _, c := range corpus {
		frame, err := hex.DecodeString(c.Frame)
		if err != nil {
			t.Fatal(err)
		}
		want, err := hex.DecodeString(c.Answer)
		if err != nil {
			t.Fatal(err)
		}
		owner, peer := socketPair(t)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		done := make(chan error, 1)
		go func() { done <- HandleControl(ctx, owner, state) }()
		if err = peer.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err = peer.Write(frame); err != nil {
			t.Fatal(err)
		}
		if err = peer.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		got, _ := bufio.NewReader(peer).ReadBytes('\n')
		_ = peer.Close()
		if err = <-done; err != nil {
			t.Errorf("%s: the handler failed: %v", c.Name, err)
		}
		cancel()
		if string(got) != string(want) {
			if differ++; differ <= 40 {
				t.Errorf("%s: the Go owner answered %q where the Python owner answered %q", c.Name, got, want)
			}
		}
	}
	if differ > 0 {
		t.Fatalf("%d of %d frames answered otherwise", differ, len(corpus))
	}
}

type bytesBuffer struct{ data []byte }

func (b *bytesBuffer) Write(p []byte) (int, error) { b.data = append(b.data, p...); return len(p), nil }
func (b *bytesBuffer) String() string              { return string(b.data) }
