//go:build linux

package service

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// candidateChannel is what one case hands a candidate as fd 3: the descriptor, and
// what the controller's end does once the candidate has started and once it exited.
type candidateChannel func() (child *os.File, started, exited func() error, err error)

type candidateResult struct {
	code           int
	stdout, stderr string
	err            error
}

// candidateOutcome runs one runtime's `service run --takeover-candidate` against the
// channel the case sets up and returns its exit code, stdout and stderr.
func candidateOutcome(home string, python bool, selector string, channel candidateChannel) (r candidateResult) {
	program, argv := testBinary, []string{"relay", "--state", home + "/state", "--socket", home + "/socket", "service", "run", "--takeover-candidate", "--allow-isolated-scope"}
	if python {
		program, argv = testPython, argv[1:]
	}
	cmd := exec.Command(program, argv...)
	cmd.Env = environment(home)
	if selector != "" {
		cmd.Env = append(cmd.Env, candidateFDEnv+"="+selector)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	started, exited := func() error { return nil }, func() error { return nil }
	if channel != nil {
		var child *os.File
		if child, started, exited, r.err = channel(); r.err != nil {
			return r
		}
		cmd.ExtraFiles = []*os.File{child}
		defer child.Close()
	}
	if r.err = cmd.Start(); r.err != nil {
		return r
	}
	r.err = started()
	err := cmd.Wait()
	r.err = errors.Join(r.err, exited())
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		r.err = errors.Join(r.err, err)
	}
	if exit != nil {
		r.code = exit.ExitCode()
	}
	r.stdout, r.stderr = stdout.String(), stderr.String()
	return r
}

// socketChannel gives the candidate one end of a socket pair of the given type. The
// controller's end sends message and closes, or with hold stays open and silent until
// the candidate exits, so the candidate sees exactly those bytes.
func socketChannel(kind int, message string, hold bool) candidateChannel {
	return func() (*os.File, func() error, func() error, error) {
		pair, err := unix.Socketpair(unix.AF_UNIX, kind|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			return nil, nil, nil, err
		}
		peer := os.NewFile(uintptr(pair[0]), "controller")
		send := func() error {
			if message != "" {
				if _, err := peer.WriteString(message); err != nil {
					return err
				}
			}
			if hold {
				return nil
			}
			return peer.Close()
		}
		exited := func() error {
			if hold {
				return peer.Close()
			}
			return nil
		}
		return os.NewFile(uintptr(pair[1]), "candidate"), send, exited, nil
	}
}

// Review of audit finding 57: the frozen `service run --takeover-candidate` answers a
// missing or invalid activation channel as the retained Python fence does (cli.py
// main, takeover.receive_candidate and CandidateChannel.receive): the same exit code
// and nothing on stdout. Go keeps its diagnostic on stderr, the service log of a
// launched candidate. Each case runs both runtimes at once: the silent controller
// waits out both 20-second channel bounds together.
func Test30CandidateChannelRefusalMatchesPython(t *testing.T) {
	devnull := func() (*os.File, func() error, func() error, error) {
		f, err := os.Open(os.DevNull)
		none := func() error { return nil }
		return f, none, none, err
	}
	unconnected := func() (*os.File, func() error, func() error, error) {
		fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		none := func() error { return nil }
		if err != nil {
			return nil, none, none, err
		}
		return os.NewFile(uintptr(fd), "unconnected"), none, none, nil
	}
	stream := unix.SOCK_STREAM
	for _, c := range []struct {
		name     string
		selector string
		channel  candidateChannel
		code     int
	}{
		{"no-selector", "", nil, 2},
		{"other-selector", "4", socketChannel(stream, "", false), 2},
		{"not-a-socket", "3", devnull, 3},
		{"datagram", "3", socketChannel(unix.SOCK_DGRAM, "", false), 2},
		{"unconnected", "3", unconnected, 3},
		{"eof", "3", socketChannel(stream, "", false), 2},
		{"truncated", "3", socketChannel(stream, `{"kind":"sta`, false), 2},
		{"object-without-newline", "3", socketChannel(stream, `{"kind":"start"}`, false), 2},
		{"silent-controller", "3", socketChannel(stream, "", true), 3},
		{"empty-line", "3", socketChannel(stream, "\n", false), 3},
		{"not-json", "3", socketChannel(stream, "not json\n", false), 3},
		{"not-an-object", "3", socketChannel(stream, "[1]\n", false), 2},
		{"wrong-kind", "3", socketChannel(stream, `{"kind":"ready"}`+"\n", false), 2},
		{"record-not-an-object", "3", socketChannel(stream, `{"kind":"start","record":"x"}`+"\n", false), 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			var py, native candidateResult
			var wg sync.WaitGroup
			wg.Go(func() { py = candidateOutcome(t.TempDir(), true, c.selector, c.channel) })
			wg.Go(func() { native = candidateOutcome(t.TempDir(), false, c.selector, c.channel) })
			wg.Wait()
			if err := errors.Join(py.err, native.err); err != nil {
				t.Fatal(err)
			}
			if py.code != c.code || py.stdout != "" || py.stderr != "" {
				t.Fatalf("Python oracle: exit %d stdout %q stderr %q", py.code, py.stdout, py.stderr)
			}
			if native.code != py.code || native.stdout != "" {
				t.Fatalf("Go: exit %d stdout %q; Python: exit %d, no output", native.code, native.stdout, py.code)
			}
			if !strings.HasPrefix(native.stderr, "{\n") {
				t.Fatalf("Go dropped its diagnostic: %q", native.stderr)
			}
		})
	}
}
