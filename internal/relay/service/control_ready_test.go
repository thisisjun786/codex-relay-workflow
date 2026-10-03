//go:build linux

package service

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// awaitControlAccepting waits until a dial of the control socket at path succeeds, which the kernel
// allows once its owner is listening (the owner's accept comes later). ListenControl binds the real
// path first and listens after, so the socket file exists, and a dial is refused with ECONNREFUSED,
// for a moment before the owner can be reached: a wait for the file alone races that gap. ENOENT
// (not bound yet) and ECONNREFUSED (bound, not listening yet) are tried again every 10 ms until
// within has passed and then returned; any other error is returned at once. The connection that got
// through is closed without a request, which is a peer that goes away before its request line: the
// handler discards what that costs (control.go), so it never reaches what Close returns and cannot
// change the daemon's result.
func awaitControlAccepting(path string, within time.Duration) error {
	for deadline := time.Now().Add(within); ; time.Sleep(10 * time.Millisecond) {
		conn, err := net.Dial("unix", path)
		if err == nil {
			return conn.Close()
		}
		if !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ECONNREFUSED) {
			return err
		}
		if time.Now().After(deadline) {
			return err
		}
	}
}

// ListenControl binds the real path before it listens, so for a moment the socket file is there and a
// dial is refused (ECONNREFUSED). A wait for the file takes that moment for ready; awaitControlAccepting
// must wait it out. The socket here is bound by hand, so the gap lasts as long as the test wants.
func TestAwaitControlAcceptingWaitsOutTheGapBetweenBindAndListen(t *testing.T) {
	home, err := os.MkdirTemp("", "t30-gap-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	path := filepath.Join(home, "control.sock")
	// Any other error is the answer at once: a path under a regular file is ENOTDIR, which no amount of waiting changes.
	plain := filepath.Join(home, "plain")
	if err = os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err = awaitControlAccepting(filepath.Join(plain, "control.sock"), 10*time.Second); !errors.Is(err, unix.ENOTDIR) || time.Since(started) > 5*time.Second {
		t.Fatalf("a path under a regular file ended after %v with %v, want ENOTDIR at once", time.Since(started), err)
	}
	// Not bound yet: the daemon has started and has not reached its bind, so the path is absent.
	started = time.Now()
	if err = awaitControlAccepting(path, 100*time.Millisecond); !errors.Is(err, unix.ENOENT) || time.Since(started) < 100*time.Millisecond {
		t.Fatalf("a path nothing has bound ended after %v with %v, want ENOENT at its deadline", time.Since(started), err)
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	if err = unix.Bind(fd, &unix.SockaddrUnix{Name: path}); err != nil {
		t.Fatal(err)
	}
	// Bound and not listening: the file is a socket, which is all the old wait looked at, and a dial is refused.
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("the bound socket's file: %v %v", info, err)
	}
	if conn, err := net.Dial("unix", path); !errors.Is(err, unix.ECONNREFUSED) {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatalf("a dial of a bound socket that is not listening: %v, want ECONNREFUSED", err)
	}
	// The wait keeps dialing through the gap and gives up with the refusal it kept getting.
	started = time.Now()
	if err = awaitControlAccepting(path, 100*time.Millisecond); !errors.Is(err, unix.ECONNREFUSED) || time.Since(started) < 100*time.Millisecond {
		t.Fatalf("the wait ended after %v with %v while the socket was only bound, want ECONNREFUSED at its deadline", time.Since(started), err)
	}
	// The owner starts listening while a wait is running: the wait runs here and the listen comes 30 ms in.
	listened := make(chan error, 1)
	time.AfterFunc(30*time.Millisecond, func() { listened <- unix.Listen(fd, 4) })
	err = awaitControlAccepting(path, 10*time.Second)
	if listenErr := <-listened; listenErr != nil {
		t.Fatal(listenErr)
	}
	if err != nil {
		t.Fatalf("the wait did not see the listener: %v", err)
	}
	// What the wait connected with was closed without a request: the owner accepts it and reads end of file.
	listener, err := net.FileListener(file)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err = listener.(*net.UnixListener).SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("the wait's connection was never queued on the listener: %v", err)
	}
	defer conn.Close()
	if err = conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if n, err := conn.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("the wait's connection sent %d bytes and ended with %v, want a close with no request", n, err)
	}
}
