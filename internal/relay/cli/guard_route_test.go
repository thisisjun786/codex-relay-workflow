package cli_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
)

// guardOwner is a live Python owner's control server (control.GuardServer) on state, evaluating
// under root, that counts the requests it answers.
type guardOwner struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

func startGuardOwner(t *testing.T, state, root string) *guardOwner {
	t.Helper()
	script := `import sys
from codex_session_relay import control
answered = []
original = control.GuardServer._answer
def counted(self, connection):
    answered.append(1)
    return original(self, connection)
control.GuardServer._answer = counted
server = control.GuardServer(sys.argv[1])
print("ready", flush=True)
sys.stdin.readline()
server.close()
print(len(answered), flush=True)
`
	cmd := exec.Command(filepath.Join(repositoryRoot(t), ".venv", "bin", "python"), "-c", script, state)
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1", "CODEX_SESSION_RELAY_MARKER_ROOT="+root)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	owner := &guardOwner{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}
	if line, err := owner.stdout.ReadString('\n'); err != nil || line != "ready\n" {
		_ = cmd.Process.Kill()
		t.Fatalf("the Python owner did not start: %q %v", line, err)
	}
	return owner
}

// stop closes the owner's control server and answers how many requests it answered.
func (o *guardOwner) stop(t *testing.T) string {
	t.Helper()
	if _, err := io.WriteString(o.stdin, "stop\n"); err != nil {
		t.Fatal(err)
	}
	count, err := o.stdout.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if err := o.cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(count)
}

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
// error; under its own store and with no daemon, each evaluates in-process. The Go CLI and the
// live fence answer byte for byte: against one live Python owner, and each against a store the
// other runtime owns.
func TestGuardEvaluate_routes_to_the_owners_control_socket_as_the_fence_does(t *testing.T) {
	home := pythonHome(t)
	type fixture struct{ Root, Stop, Now string }
	build := func(name string) (string, fixture) {
		state := filepath.Join(home, name, "state")
		var f fixture
		if err := json.Unmarshal([]byte(matrixPython(t, "guard", filepath.Join(home, name, "guard"), filepath.Join(state, "relay.sqlite3"))), &f); err != nil {
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

	// An owner that answers nothing readable (nothing, not JSON, or null), and one whose error
	// record the fence cannot look up (an unhashable kind), after the request was sent: host
	// errors, never a refusal.
	for _, owner := range []struct{ name, reply, detail string }{
		{"an owner that answers nothing", "", "the owner closed control.sock without a readable guard-evaluate answer"},
		{"an owner that answers bytes that are not JSON", "\xff not json\n", "the owner closed control.sock without a readable guard-evaluate answer"},
		// The fence reads a JSON null as no answer at all (socket_guard's `if value is None`).
		{"an owner that answers null", "null\n", "the owner closed control.sock without a readable guard-evaluate answer"},
		{"an owner whose error kind is a list", "{\"error\": [\"refused\"]}\n", "TypeError: unhashable type: 'list'"},
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

	// A live Python owner answers both runtimes' requests: its verdict under its own marker root,
	// and its host error for a request naming another one.
	owner := startGuardOwner(t, python, pf.Root)
	got = goCLI(t, argv(python, pf, pf.Root)...)
	fence = pythonCLI(t, argv(python, pf, pf.Root))
	if count := owner.stop(t); count != "2" {
		t.Errorf("the owner answered %s requests, not the two routed to it", count)
	}
	same("routed to the owner", got, fence[0], 0)
	elsewhere := filepath.Join(home, "owner-markers")
	owner = startGuardOwner(t, python, elsewhere)
	got = goCLI(t, argv(python, pf, pf.Root)...)
	fence = pythonCLI(t, argv(python, pf, pf.Root))
	owner.stop(t)
	same("routed to an owner under another marker root", got, fence[0], 3)
	if !strings.Contains(got.stdout, "control.sock evaluates Stops only under this owner's marker root "+elsewhere) {
		t.Errorf("the owner's marker root: %s", got.stdout)
	}

	// Where no owner answers and the store is its own, each runtime checks the store's ownership
	// on the read-only Stop path before it evaluates: its own store in phase draining is refused.
	setPhase(t, filepath.Join(python, "relay.sqlite3"), "draining")
	setPhase(t, filepath.Join(golang, "relay.sqlite3"), "draining")
	got = goCLI(t, argv(golang, gf, gf.Root)...)
	same("this runtime's own store, draining", got, pythonCLI(t, argv(python, pf, pf.Root))[0], 2)
	if object(t, got.stdout)["reason"] != "store_owned_by_other" {
		t.Errorf("a draining store: %s", got.stdout)
	}
}
