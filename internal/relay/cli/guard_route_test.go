package cli_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
)

// listen binds path, through /proc/self/fd when a sockaddr_un cannot hold it.
func listen(t *testing.T, path string) net.Listener {
	t.Helper()
	address, release, err := hook.ControlAddress(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	listener, err := net.Listen("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

// A socket file nobody listens on: a control.sock its daemon left behind.
func staleSocket(t *testing.T, path string) {
	t.Helper()
	listener := listen(t, path)
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
}

// An owner that reads the request, answers reply (nothing when it is empty) and closes.
func fakeOwner(t *testing.T, path, reply string) func() {
	t.Helper()
	listener := listen(t, path)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_, _ = bufio.NewReader(conn).ReadString('\n')
		_, _ = io.WriteString(conn, reply)
		_ = conn.Close()
	}()
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	return func() {
		_ = listener.Close()
		<-done
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

// guard-evaluate is answered by the owner of the store its Stop reads, in both runtimes
// (backlog before todo 42, cutover.md "Go finding owner=python"): a Stop whose evaluation reads
// its receipt is sent to <state>/control.sock, and the owner's answer is the command's. Where no
// owner can be asked and the other runtime owns the store, each runtime refuses with the fence's
// words (no socket, a socket nobody listens on); an owner that says nothing readable is a host
// error; under its own store and with no daemon, each evaluates in-process. The Go CLI answers
// what the fence answered (recorded: see oracleRun) byte for byte, each against a store the other
// runtime owns. (A live Python owner answering both runtimes' requests on its control socket
// left with the Python runtime, todo 44; the Go owner's control server is
// internal/relay/control's.)
func TestGuardEvaluate_routes_to_the_owners_control_socket_as_the_fence_does(t *testing.T) {
	home := pythonHome(t)
	type fixture struct{ Root, Stop, Now string }
	build := func(name string) (string, fixture) {
		state := filepath.Join(home, name, "state")
		var f fixture
		built := pythonFixture(t, "guard "+name, filepath.Join(home, name), nil, func() (string, error) {
			return matrixPython(t, "guard", filepath.Join(home, name, "guard"), filepath.Join(state, "relay.sqlite3")), nil
		})
		if err := json.Unmarshal([]byte(built), &f); err != nil {
			t.Fatal(err)
		}
		return state, f
	}
	// python holds a store the fence owns, golang one Go owns.
	python, pf := build("python-owned")
	golang, gf := build("go-owned")
	restamp(t, golang, "go")
	argv := func(state string, f fixture, root string) []string {
		return []string{"--state", state, "guard-evaluate", "--marker-root", root, "--stop-input", f.Stop, "--now", f.Now, "--no-record"}
	}
	same := func(what string, got, want answer, code int) {
		t.Helper()
		if got != want || got.code != code {
			t.Errorf("%s:\n go     %d %s\n python %d %s", what, got.code, got.stdout, want.code, want.stdout)
		}
	}
	unanswered := func(detail string) string {
		return "{\n  \"error\": \"refused\",\n  \"reason\": \"store_owned_by_other\",\n  \"detail\": \"the owner could not answer guard-evaluate: " + detail + "\"\n}\n"
	}

	// No owner listens: each runtime refuses a store the other owns, and evaluates its own.
	got := goCLI(t, argv(python, pf, pf.Root)...)
	fence := pythonCLI(t, argv(golang, gf, gf.Root), argv(python, pf, pf.Root))
	same("no control.sock, the other runtime's store", got, fence[0], 2)
	if got.stdout != unanswered("[Errno 2] No such file or directory") {
		t.Errorf("no control.sock: %s", got.stdout)
	}
	same("no daemon, this runtime's own store", goCLI(t, argv(golang, gf, gf.Root)...), fence[1], 0)

	// A control.sock its daemon left behind.
	staleSocket(t, filepath.Join(python, "control.sock"))
	staleSocket(t, filepath.Join(golang, "control.sock"))
	got = goCLI(t, argv(python, pf, pf.Root)...)
	same("a socket nobody listens on", got, pythonCLI(t, argv(golang, gf, gf.Root))[0], 2)
	if got.stdout != unanswered("[Errno 111] Connection refused") {
		t.Errorf("a stale control.sock: %s", got.stdout)
	}
	for _, state := range []string{python, golang} {
		if err := os.Remove(filepath.Join(state, "control.sock")); err != nil {
			t.Fatal(err)
		}
	}

	// An owner that answers nothing readable (nothing, not UTF-8, not JSON, or null), and one
	// whose error record the fence cannot look up (an unhashable kind), after the request was
	// sent: host errors, never a refusal.
	for _, owner := range []struct{ name, reply, detail string }{
		{"an owner that answers nothing", "", "the owner closed control.sock without a readable guard-evaluate answer"},
		{"an owner that answers bytes that are not JSON", "\xff not json\n", "the owner closed control.sock without a readable guard-evaluate answer"},
		// JSON once each byte that is not UTF-8 is replaced with U+FFFD, and still unreadable:
		// neither an answer printed with exit 0 nor an error record's own exit status.
		{"an owner that answers an object that is not UTF-8", "{\"a\": \"\xff\"}\n", "the owner closed control.sock without a readable guard-evaluate answer"},
		{"an owner that answers a refusal that is not UTF-8", "{\"error\": \"refused\", \"reason\": \"\xff\"}\n", "the owner closed control.sock without a readable guard-evaluate answer"},
		{"an owner that answers a host error that is not UTF-8", "{\"error\": \"host\", \"detail\": \"\xc3\"}\n", "the owner closed control.sock without a readable guard-evaluate answer"},
		{"an owner that answers a string that is not UTF-8", "\"\xff\"\n", "the owner closed control.sock without a readable guard-evaluate answer"},
		// One byte over the 64 MiB frame limit, where those bytes are a whole JSON string.
		{"an owner that answers over the frame limit", "\"" + strings.Repeat("a", 64<<20-1) + "\"\n", "the owner closed control.sock without a readable guard-evaluate answer"},
		// The fence reads a JSON null as no answer at all (socket_guard's `if value is None`).
		{"an owner that answers null", "null\n", "the owner closed control.sock without a readable guard-evaluate answer"},
		{"an owner whose error kind is a list", "{\"error\": [\"refused\"]}\n", "TypeError: unhashable type: 'list'"},
		// The fence's json.loads takes 9998 nested containers and raises RecursionError from
		// 9999, which leaves socket_guard for cli.main's host error: at 9999 and 10000, where Go's
		// decoder still reads the answer, and above 10000, where it no longer does.
		{"an owner that answers 9999 nested arrays", strings.Repeat("[", 9999) + strings.Repeat("]", 9999) + "\n", "RecursionError: maximum recursion depth exceeded while decoding a JSON array from a unicode string"},
		{"an owner that answers 9999 nested objects", strings.Repeat("{\"a\": ", 9998) + "{}" + strings.Repeat("}", 9998) + "\n", "RecursionError: maximum recursion depth exceeded while decoding a JSON object from a unicode string"},
		{"an owner that answers 10000 nested arrays", strings.Repeat("[", 10000) + strings.Repeat("]", 10000) + "\n", "RecursionError: maximum recursion depth exceeded while decoding a JSON array from a unicode string"},
		{"an owner that answers 10001 nested arrays", strings.Repeat("[", 10001) + strings.Repeat("]", 10001) + "\n", "RecursionError: maximum recursion depth exceeded while decoding a JSON array from a unicode string"},
		// The fence decodes the bytes before it parses them: at any depth, bytes that are not
		// UTF-8 are no answer.
		{"an owner that answers 9999 nested arrays that are not UTF-8", strings.Repeat("[", 9999) + "\"\xff\"" + strings.Repeat("]", 9999) + "\n", "the owner closed control.sock without a readable guard-evaluate answer"},
	} {
		closed := fakeOwner(t, filepath.Join(python, "control.sock"), owner.reply)
		got = goCLI(t, argv(python, pf, pf.Root)...)
		closed()
		closed = fakeOwner(t, filepath.Join(golang, "control.sock"), owner.reply)
		want := pythonCLI(t, argv(golang, gf, gf.Root))[0]
		closed()
		same(owner.name, got, want, 3)
		if !strings.Contains(got.stdout, owner.detail) {
			t.Errorf("%s: %s", owner.name, got.stdout)
		}
	}
}
