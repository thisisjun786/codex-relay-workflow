//go:build linux

package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"golang.org/x/sys/unix"
)

func Test30ControlSocketRealSurface(t *testing.T) {
	state := t.TempDir()
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	server, err := ListenControl(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", ControlPath(state))
	if err != nil {
		t.Fatal(err)
	}
	// The owner evaluates only under its own marker root (hook ownerPaths), configured as the
	// relay is.
	root := t.TempDir()
	t.Setenv("CODEX_SESSION_RELAY_MARKER_ROOT", root)
	answer, err := hook.RequestGuard(ctx, conn, hook.Object{}, hook.GuardOptions{Root: root, Mode: hook.Observe, Now: "2026-01-01T00:00:00Z"})
	_ = conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	if get(answer, "decision") != "release" || get(answer, "state") != "unmanaged" {
		t.Fatal(answer)
	}
	// control.sock serves guard-evaluate only, as Python's GuardServer does: decision-25
	// ingress is the queued command's own file publication, so an inbox-submit request is
	// rejected like any other method, never acknowledged.
	conn, err = net.Dial("unix", ControlPath(state))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Write([]byte("{\"protocol\":1,\"method\":\"inbox-submit\",\"params\":{}}\n")); err != nil {
		t.Fatal(err)
	}
	var rejected map[string]any
	if err = json.NewDecoder(conn).Decode(&rejected); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if !reflect.DeepEqual(rejected, map[string]any{"protocol": float64(1), "requestRejected": true}) {
		t.Fatal(rejected)
	}
	if err = server.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(state, 0770); err != nil {
		t.Fatal(err)
	}
	if server, err = ListenControl(ctx, state); err == nil {
		_ = server.Close()
		t.Fatal("unsafe socket parent accepted")
	}
	// Audit finding 39: the clients' rule, not an exact mode. A 0750 state directory
	// left by an older build serves, and a path longer than sockaddr_un holds is bound
	// and dialed through /proc/self/fd as Python's GuardServer and Stop adapter do.
	long := filepath.Join(state, strings.Repeat("d", 60), strings.Repeat("e", 60))
	if err = os.MkdirAll(long, 0700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{state, long} {
		if err = os.Chmod(dir, 0750); err != nil {
			t.Fatal(err)
		}
		server, err = ListenControl(ctx, dir)
		if err != nil {
			t.Fatal(dir, err)
		}
		address, release, err := hook.ControlAddress(ControlPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.Dial("unix", address)
		release()
		if err != nil {
			t.Fatal(len(ControlPath(dir)), err)
		}
		if _, err = conn.Write([]byte("{\"protocol\":1,\"method\":\"inbox-submit\",\"params\":{}}\n")); err != nil {
			t.Fatal(err)
		}
		var answer map[string]any
		err = json.NewDecoder(conn).Decode(&answer)
		_ = conn.Close()
		if err != nil || answer["requestRejected"] != true {
			t.Fatal(answer, err)
		}
		if err = server.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err = os.Lstat(ControlPath(dir)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("closed control socket left behind at %d bytes: %v", len(ControlPath(dir)), err)
		}
	}
}

// PR #185 4128954449: a client that connects to control.sock and goes away before it
// finishes a request line asked nothing, so it is no handler failure. The listener closes
// cleanly, and a daemon segment whose work succeeded exits 0 rather than exit 3 "EOF".
func Test30ControlDisconnectBeforeARequestIsNoFailure(t *testing.T) {
	probe := func(t *testing.T, path string, partial []byte) {
		t.Helper()
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		if len(partial) > 0 {
			if _, err = conn.Write(partial); err != nil {
				t.Fatal(err)
			}
		}
		if err = conn.Close(); err != nil {
			t.Fatal(err)
		}
	}
	home, err := os.MkdirTemp("", "t30-probe-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Run("listener", func(t *testing.T) {
		state := filepath.Join(home, "listener")
		if err := os.Mkdir(state, 0700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		server, err := ListenControl(ctx, state)
		if err != nil {
			t.Fatal(err)
		}
		probe(t, ControlPath(state), nil)
		probe(t, ControlPath(state), []byte(`{"protocol":1,"method":"guard-`))
		// The listener still serves, and the probes' handlers finish before Close answers.
		root := t.TempDir()
		t.Setenv("CODEX_SESSION_RELAY_MARKER_ROOT", root)
		conn, err := net.Dial("unix", ControlPath(state))
		if err != nil {
			t.Fatal(err)
		}
		answer, err := hook.RequestGuard(ctx, conn, hook.Object{}, hook.GuardOptions{Root: root, Mode: hook.Observe, Now: "2026-01-01T00:00:00Z"})
		_ = conn.Close()
		if err != nil || get(answer, "decision") != "release" {
			t.Fatal(answer, err)
		}
		if err = server.Close(); err != nil {
			t.Fatalf("a disconnect before a request line failed the listener: %v", err)
		}
	})
	t.Run("daemon", func(t *testing.T) {
		daemon := exec.Command(testBinary, "relay", "--state", home+"/state", "--socket", home+"/socket", "daemon", "--deadline", "2", "--allow-isolated-scope")
		daemon.Env = environment(home)
		var stdout, stderr bytes.Buffer
		daemon.Stdout, daemon.Stderr = &stdout, &stderr
		if err := daemon.Start(); err != nil {
			t.Fatal(err)
		}
		path := ControlPath(home + "/state")
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
				break
			}
			if time.Now().After(deadline) {
				_ = daemon.Process.Kill()
				_ = daemon.Wait()
				t.Fatalf("the daemon never bound control.sock: %s %s", stdout.String(), stderr.String())
			}
		}
		probe(t, path, nil)
		err := daemon.Wait()
		var result map[string]any
		if err != nil || json.Unmarshal(stdout.Bytes(), &result) != nil || result["ok"] != true {
			t.Fatalf("daemon after a probe: %v\n%s%s", err, stdout.String(), stderr.String())
		}
	})
}

// PR #185 4128954449, the rule it cites (control.py GuardServer._serve): nothing a peer or the
// kernel does ends or fails the owner. Every frame below is one the retained Python owner
// answers with its host record, and the Go owner answers it with the same bytes: among them the
// nesting edge of GuardServer's serving thread (9996 containers decode, 9997 raise, and a
// refusal raised within four levels of the edge, or a NaN or over-long integer at it, raises
// RecursionError on the way), a naive deadline, a mode or now that is not a string, and a peer
// whose request line is not complete when control.py's 5 s read bound expires, whether it sent
// nothing more or trickled the line in parts. A corpus of refusals and values at 9989 to 9997
// open containers gets the same answers from both. Frames Python serves (one led by a UTF-8
// byte order mark, a protocol of true or 1.0, a truthy noRecord that is not true) Go serves with
// the same verdict. A peer that hangs up after a complete request is skipped, and a failed
// accept is retried. Both owners still serve the next Stop afterwards, and none of it reaches
// Go's Close, so a daemon segment whose work succeeded exits 0. (Every other reading of a frame
// is TestControlReadsEveryFrameAsControlPyReadsIt, over control.py's _answer.)
func Test30ControlPeerFailuresAreAnsweredAsPythonAnswersThem(t *testing.T) {
	home, err := os.MkdirTemp("", "t30-peer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	root := filepath.Join(home, "markers")
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SESSION_RELAY_MARKER_ROOT", root)
	frame := func(params string) []byte {
		return []byte(`{"protocol":1,"method":"guard-evaluate","params":` + params + "}\n")
	}
	params := func(deadline string, extra string) string {
		return `{"markerRoot":` + strconv.Quote(root) + `,"stopInput":{},"mode":"observe","now":"2026-01-01T00:00:00Z","noRecord":true,"deadline":` + deadline + extra + `}`
	}
	later := strconv.Quote(time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano))
	failures := []struct{ name, detail string }{
		{"not json", "JSONDecodeError: Expecting value: line 1 column 1 (char 0)"},
		{"nested past the scanner", "RecursionError: maximum recursion depth exceeded while decoding a JSON array from a unicode string"},
		{"nested past a goroutine stack", "RecursionError: maximum recursion depth exceeded while decoding a JSON array from a unicode string"},
		{"nested to the serving thread's edge", "TypeError: guard request must be an object"},
		{"nested one past the serving thread's edge", "RecursionError: maximum recursion depth exceeded while decoding a JSON array from a unicode string"},
		{"not an object", "TypeError: guard request must be an object"},
		{"no params", "KeyError: 'params'"},
		{"params not an object", "TypeError: guard params must be an object"},
		{"no stop input", "KeyError: 'stopInput'"},
		{"stop input not an object", "TypeError: stop input must be an object"},
		{"null deadline", "TypeError: guard deadline must be a string"},
		{"unparseable deadline", "ValueError: Invalid isoformat string: 'soon'"},
		{"expired deadline", "TimeoutError: guard request deadline expired"},
		{"naive deadline", "TypeError: can't subtract offset-naive and offset-aware datetimes"},
		{"socketPath not a string", "TypeError: guard socketPath must be a string"},
		{"program not a string", "TypeError: guard program must be a string"},
		{"mode not a string", "TypeError: guard mode must be a string"},
		{"now not a string", "TypeError: guard now must be a string"},
		{"silent past the read timeout", "TimeoutError: timed out"},
		{"trickled past the read timeout", "TimeoutError: timed out"},
		{"a refusal at the edge", "RecursionError: maximum recursion depth exceeded while calling a Python object"},
		{"a refusal one level inside the edge", "RecursionError: maximum recursion depth exceeded"},
		{"a refusal two levels inside the edge", "RecursionError: maximum recursion depth exceeded"},
		{"a refusal three levels inside the edge", "RecursionError: maximum recursion depth exceeded while calling a Python object"},
		{"a refusal four levels inside the edge", "JSONDecodeError: Expecting ',' delimiter: line 1 column 9995 (char 9994)"},
		{"a missing value at the edge", "RecursionError: maximum recursion depth exceeded while calling a Python object"},
		{"a missing value one level inside the edge", "JSONDecodeError: Expecting value: line 1 column 9996 (char 9995)"},
		{"NaN at the edge", "RecursionError: maximum recursion depth exceeded while calling a Python object"},
		{"NaN one level inside the edge", "TypeError: guard request must be an object"},
		{"an over-long integer at the edge", "RecursionError: maximum recursion depth exceeded while calling a Python object"},
		{"an over-long integer one level inside the edge", "ValueError: Exceeds the limit (4300 digits) for integer string conversion: value has 4301 digits; use sys.set_int_max_str_digits() to increase the limit"},
	}
	nested := func(depth int, inner string, closed bool) []byte {
		frame := strings.Repeat("[", depth) + inner
		if closed {
			frame += strings.Repeat("]", depth)
		}
		return []byte(frame + "\n")
	}
	frames := map[string][]byte{
		"not json":                      []byte("not json\n"),
		"nested past the scanner":       []byte(`{"params":` + strings.Repeat("[", 200000) + "\n"),
		"nested past a goroutine stack": []byte(`{"params":` + strings.Repeat("[", 5000000) + "\n"),
		// The edge is the serving thread's, measured through GuardServer: a CPython whose C
		// stack budget or call depth differs fails here first.
		"nested to the serving thread's edge":       []byte(strings.Repeat("[", 9996) + strings.Repeat("]", 9996) + "\n"),
		"nested one past the serving thread's edge": []byte(strings.Repeat("[", 9997) + strings.Repeat("]", 9997) + "\n"),
		"not an object":            []byte("[1]\n"),
		"no params":                []byte(`{"protocol":1,"method":"guard-evaluate"}` + "\n"),
		"params not an object":     frame("[]"),
		"no stop input":            frame("{}"),
		"stop input not an object": frame(`{"stopInput":[]}`),
		"null deadline":            frame(params("null", "")),
		"unparseable deadline":     frame(params(`"soon"`, "")),
		"expired deadline":         frame(params(`"2020-01-01T00:00:00+00:00"`, "")),
		"naive deadline":           frame(params(`"2999-01-01T00:00:00"`, "")),
		// No line end: the owner waits for the rest of the line until its read timeout.
		"silent past the read timeout": []byte(`{"protocol":1`),
		"socketPath not a string":      frame(params(later, `,"socketPath":1`)),
		"program not a string":         frame(params(later, `,"program":["crw"]`)),
		// A later key replaces an earlier one in both owners, as in a Python dict.
		"mode not a string": frame(params(later, `,"mode":5`)),
		"now not a string":  frame(params(later, `,"now":0`)),
		// The edge of the serving thread's budget: the calls that raise a refusal, or read a
		// constant, draw on it too.
		"a refusal at the edge":                          nested(9996, "1 2", false),
		"a refusal one level inside the edge":            nested(9995, "1 2", false),
		"a refusal two levels inside the edge":           nested(9994, "1 2", false),
		"a refusal three levels inside the edge":         nested(9993, "1 2", false),
		"a refusal four levels inside the edge":          nested(9992, "1 2", false),
		"a missing value at the edge":                    nested(9996, "x", false),
		"a missing value one level inside the edge":      nested(9995, "x", false),
		"NaN at the edge":                                nested(9996, "NaN", true),
		"NaN one level inside the edge":                  nested(9995, "NaN", true),
		"an over-long integer at the edge":               nested(9996, strings.Repeat("1", 4301), true),
		"an over-long integer one level inside the edge": nested(9995, strings.Repeat("1", 4301), true),
	}
	// Every kind of refusal and value the scanner meets, at 9989 to 9997 open containers; the
	// frame's line end is whitespace after its last token.
	edge := map[string][]byte{}
	for depth := 9989; depth <= 9997; depth++ {
		open, closed := strings.Repeat("[", depth-1), strings.Repeat("]", depth-1)
		long := strings.Repeat("1", 4301)
		for kind, request := range map[string]string{
			"a missing value": open + "[x" + closed, "the end of the frame": open + "[",
			"a comma": open + "[1 2" + closed, "NaN": open + "[NaN]" + closed, "Infinity": open + "[Infinity]" + closed,
			"-Infinity": open + "[-Infinity]" + closed, "an over-long integer": open + "[" + long + "]" + closed,
			"an over-long negative integer": open + "[-" + long + "]" + closed,
			"a control character":           open + "[\"\x01\"]" + closed, "a bad escape": open + `["\q"]` + closed,
			"a bad unicode escape": open + `["\uzzzz"]` + closed, "a colon": open + `{"a" 1}` + closed,
			"a trailing comma in an array": open + "[1,]" + closed, "a trailing comma in an object": open + `{"a":1,}` + closed,
			"a property name": open + "{1}" + closed, "an object value": open + `{"a":}` + closed,
			"an object comma": open + `{"a":1 "b":2}` + closed, "a refusal after a close": open + "[[]x" + closed,
			"a string": open + `["s"]` + closed, "a number": open + "[1.5e3]" + closed, "true": open + "[true]" + closed,
			"an object": open + `{"a":1}` + closed, "an empty array": open + "[]" + closed, "extra data": open + "[]" + closed + "]x",
		} {
			edge[fmt.Sprintf("%s at %d", kind, depth)] = []byte(request + "\n")
		}
	}
	ask := func(t *testing.T, path string, request []byte) []byte {
		t.Helper()
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err = conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err = conn.Write(request); err != nil {
			t.Fatal(err)
		}
		line, _ := bufio.NewReader(conn).ReadBytes('\n')
		return line
	}
	hangUp := func(t *testing.T, path string, request []byte) {
		t.Helper()
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = conn.Write(request); err != nil {
			t.Fatal(err)
		}
		if err = conn.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// trickle sends a request line in parts 3 s apart, each well inside a read's 5 s, the whole
	// line past them: the bound is on the line, so the owner answers at 5 s.
	trickle := func(t *testing.T, path string) []byte {
		t.Helper()
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err = conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
			t.Fatal(err)
		}
		request := frame(params(later, ""))
		go func() {
			for i, part := range [][]byte{request[:10], request[10:20], request[20:]} {
				if i > 0 {
					time.Sleep(3 * time.Second)
				}
				if _, err := conn.Write(part); err != nil {
					return // answered and closed before the line was complete
				}
			}
		}()
		line, _ := bufio.NewReader(conn).ReadBytes('\n')
		return line
	}
	// Served, not refused: a frame led by a UTF-8 byte order mark (json.loads decodes bytes as
	// utf-8-sig), a protocol control.py compares equal to 1, and a noRecord it reads as true,
	// which downgrades hold to observe.
	servedFrames := map[string][]byte{
		"marked":         append([]byte("\xef\xbb\xbf"), frame(params(later, ""))...),
		"protocol true":  []byte(`{"protocol":true,"method":"guard-evaluate","params":` + params(later, "") + "}\n"),
		"protocol 1.0":   []byte(`{"protocol":1.0,"method":"guard-evaluate","params":` + params(later, "") + "}\n"),
		"noRecord 1":     frame(params(later, `,"mode":"hold","noRecord":1`)),
		"noRecord false": frame(params(later, `,"mode":"hold","noRecord":false`)),
	}
	// served asks every failing frame, the edge corpus and the served frames, then hangs up
	// after a complete request, then asks a Stop the owner must still answer with a verdict.
	served := func(t *testing.T, path string) map[string][]byte {
		t.Helper()
		answers := map[string][]byte{}
		for _, failure := range failures {
			if failure.name == "trickled past the read timeout" {
				answers[failure.name] = trickle(t, path)
				continue
			}
			answers[failure.name] = ask(t, path, frames[failure.name])
		}
		for name, request := range edge {
			answers["edge "+name] = ask(t, path, request)
		}
		for name, request := range servedFrames {
			answers[name] = ask(t, path, request)
			var verdict map[string]any
			if err := json.Unmarshal(answers[name], &verdict); err != nil || verdict["decision"] != "release" {
				t.Errorf("the owner did not serve %s: %q %v", name, answers[name], err)
			}
		}
		var verdict map[string]any
		if json.Unmarshal(answers["noRecord 1"], &verdict) != nil || verdict["modeDowngraded"] != "hold_requires_a_recorded_observation" {
			t.Errorf("a noRecord of 1 did not ask for no record: %q", answers["noRecord 1"])
		}
		hangUp(t, path, frame(params(later, "")))
		verdict = nil
		if err := json.Unmarshal(ask(t, path, frame(params(later, ""))), &verdict); err != nil || verdict["decision"] != "release" {
			t.Errorf("the owner no longer serves a Stop after its failed peers: %v %v", verdict, err)
		}
		return answers
	}
	python := func(t *testing.T) map[string][]byte {
		t.Helper()
		state := filepath.Join(home, "python")
		if err := os.Mkdir(state, 0700); err != nil {
			t.Fatal(err)
		}
		owner := exec.Command(filepath.Join(testRoot, ".venv/bin/python"), "-c", `import sys
from codex_session_relay.control import GuardServer
server = GuardServer(sys.argv[1])
print("bound", flush=True)
sys.stdin.read()
server.close()`, state)
		stdin, err := owner.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := owner.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		owner.Stderr = &stderr
		if err = owner.Start(); err != nil {
			t.Fatal(err)
		}
		if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "bound\n" {
			_ = owner.Process.Kill()
			_ = owner.Wait()
			t.Fatalf("the Python owner never bound control.sock: %q %v %s", line, err, stderr.String())
		}
		answers := served(t, ControlPath(state))
		_ = stdin.Close()
		if err = owner.Wait(); err != nil {
			t.Fatalf("the Python owner: %v %s", err, stderr.String())
		}
		return answers
	}(t)
	t.Run("listener", func(t *testing.T) {
		state := filepath.Join(home, "go")
		if err := os.Mkdir(state, 0700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		server, err := ListenControl(ctx, state)
		if err != nil {
			t.Fatal(err)
		}
		answers := served(t, ControlPath(state))
		for _, failure := range failures {
			want := `{"error": "host", "detail": "` + failure.detail + `"}` + "\n"
			if string(python[failure.name]) != want {
				t.Errorf("%s: the Python owner answered %q, not %q", failure.name, python[failure.name], want)
			}
			if string(answers[failure.name]) != string(python[failure.name]) {
				t.Errorf("%s: the Go owner answered %q where the Python owner answered %q", failure.name, answers[failure.name], python[failure.name])
			}
		}
		for name := range edge {
			if string(answers["edge "+name]) != string(python["edge "+name]) {
				t.Errorf("%s: the Go owner answered %q where the Python owner answered %q", name, answers["edge "+name], python["edge "+name])
			}
		}
		// The same verdict; each owner spaces its verdict line as its own serializer does.
		for name := range servedFrames {
			var goVerdict, pythonVerdict any
			if json.Unmarshal(answers[name], &goVerdict) != nil || json.Unmarshal(python[name], &pythonVerdict) != nil || !reflect.DeepEqual(goVerdict, pythonVerdict) {
				t.Errorf("%s: the Go owner answered %q where the Python owner answered %q", name, answers[name], python[name])
			}
		}
		if err = server.Close(); err != nil {
			t.Fatalf("a failed peer failed the listener: %v", err)
		}
	})
	t.Run("accept", func(t *testing.T) {
		// control.py backs off 50 ms after EMFILE, ENFILE, ENOBUFS or ENOMEM and accepts again.
		state := filepath.Join(home, "accept")
		if err := os.Mkdir(state, 0700); err != nil {
			t.Fatal(err)
		}
		var refused []error
		accept := acceptControl
		acceptControl = func(listener *net.UnixListener) (net.Conn, error) {
			if len(refused) < 2 {
				e := &net.OpError{Op: "accept", Net: "unix", Err: os.NewSyscallError("accept4", []unix.Errno{unix.EMFILE, unix.ENOBUFS}[len(refused)])}
				refused = append(refused, e)
				return nil, e
			}
			return accept(listener)
		}
		defer func() { acceptControl = accept }()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		server, err := ListenControl(ctx, state)
		if err != nil {
			t.Fatal(err)
		}
		var verdict map[string]any
		if err := json.Unmarshal(ask(t, ControlPath(state), frame(params(later, ""))), &verdict); err != nil || verdict["decision"] != "release" || len(refused) != 2 {
			t.Errorf("the owner stopped accepting after %v: %v %v", refused, verdict, err)
		}
		if err = server.Close(); err != nil {
			t.Fatalf("a refused accept failed the listener: %v", err)
		}
	})
	t.Run("daemon", func(t *testing.T) {
		daemon := exec.Command(testBinary, "relay", "--state", home+"/state", "--socket", home+"/socket", "daemon", "--deadline", "2", "--allow-isolated-scope")
		daemon.Env = environment(home)
		var stdout, stderr bytes.Buffer
		daemon.Stdout, daemon.Stderr = &stdout, &stderr
		if err := daemon.Start(); err != nil {
			t.Fatal(err)
		}
		path := ControlPath(home + "/state")
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
				break
			}
			if time.Now().After(deadline) {
				_ = daemon.Process.Kill()
				_ = daemon.Wait()
				t.Fatalf("the daemon never bound control.sock: %s %s", stdout.String(), stderr.String())
			}
		}
		if answer := ask(t, path, frames["params not an object"]); string(answer) != string(python["params not an object"]) {
			t.Errorf("the daemon answered %q where the Python owner answered %q", answer, python["params not an object"])
		}
		hangUp(t, path, frame(params(later, "")))
		err := daemon.Wait()
		var result map[string]any
		if err != nil || json.Unmarshal(stdout.Bytes(), &result) != nil || result["ok"] != true {
			t.Fatalf("daemon after failed peers: %v\n%s%s", err, stdout.String(), stderr.String())
		}
	})
}
func takeoverCLI(t *testing.T, home string, args ...string) (map[string]any, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	argv := append([]string{"relay", "--state", home + "/state", "--socket", home + "/socket", "takeover"}, args...)
	cmd := exec.CommandContext(ctx, testBinary, argv...)
	cmd.Env = environment(home)
	raw, err := cmd.CombinedOutput()
	t.Logf("CLI %q stdout=%s error=%v", argv, raw, err)
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("%v %s", err, raw)
		}
		code = exit.ExitCode()
	}
	if code != 0 {
		// The candidate writes to the state directory's service log.
		if log, e := os.ReadFile(home + "/state/daemon.log"); e == nil {
			t.Logf("daemon.log:\n%s", log)
		}
	}
	var result map[string]any
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("%v %s", err, raw)
	}
	return result, code
}
func seedTakeover(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "t30-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(home); err != nil {
			t.Error(err)
		}
		if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("temporary state survived cleanup: %v", err)
		}
		t.Logf("CLEANUP removed %s", home)
	})
	path := home + "/state/relay.sqlite3"
	testsupport.Create(t, path, home+"/socket", "python")
	r, err := ownership.ReadRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := ScopeRegistry{Root: home + "/scopes", Authority: "isolated"}
	if err = scope.Prepare(); err != nil {
		t.Fatal(err)
	}
	key := scope.Key(home + "/socket")
	r.ScopeKey = &key
	if err = ownership.Publish(path, r, nil); err != nil {
		t.Fatal(err)
	}
	// Both candidates are the service supervisor, so the retained Python owner's
	// service is enabled, as on the host; its own CLI records the intent.
	enable := exec.Command(testPython, "--state", home+"/state", "--socket", home+"/socket", "service", "enable")
	enable.Env = environment(home)
	if raw, err := enable.CombinedOutput(); err != nil {
		t.Fatalf("python service enable: %v %s", err, raw)
	}
	return home
}

// awaitControl waits, bounded, for the activated owner's worker to bind control.sock:
// the candidate supervisor releases its readiness listener once activated.
func awaitControl(t *testing.T, state string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", ControlPath(state), time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("control socket never served: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// activeHolder asserts the published holder is the live service supervisor of the
// named build, serving the stable guard RPC through its worker.
func activeHolder(t *testing.T, home, build string) ownership.Identity {
	t.Helper()
	record, err := ownership.ReadRecord(home + "/state/relay.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != "active" || record.Holder == nil || record.Holder.Build != build {
		t.Fatalf("holder: %+v", record)
	}
	process(t, record.Holder.PID)
	run := read(home + "/state/daemon.json")
	if num(get(run, "pid")) != record.Holder.PID {
		t.Fatalf("holder %d is not the service supervisor %v", record.Holder.PID, get(run, "pid"))
	}
	if sid, e := unix.Getsid(record.Holder.PID); e != nil || sid != record.Holder.PID {
		t.Fatalf("holder did not lead its own session: sid=%d %v", sid, e)
	}
	awaitControl(t, home+"/state")
	if build != ownership.PythonBuild {
		// The Go holder supervises: its worker runs on the service's launch declaration
		// even though this controller's environment names no execution policy.
		deadline := time.Now().Add(15 * time.Second)
		for {
			run = read(home + "/state/daemon.json")
			receipt := read(home + "/state/worker-policy.json")
			worker, _ := get(receipt, "worker").(Object)
			policy, _ := get(receipt, "policy").(Object)
			if truth(get(run, "workerPid")) && equal(get(worker, "pid"), get(run, "workerPid")) {
				if get(policy, "state") != "declared" {
					t.Fatalf("worker launched without the declared policy: %v", receipt)
				}
				// Decision 28: workers inherit neither the channel nor its selector.
				environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", num(get(run, "workerPid"))))
				if err != nil || bytes.Contains(environ, []byte(candidateFDEnv+"=")) {
					t.Fatalf("worker environment carries the activation channel: %v", err)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("holder never supervised a worker: %v %v", run, receipt)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := net.Dial("unix", home+"/state/control.sock")
	if err != nil {
		t.Fatal(err)
	}
	answer, err := hook.RequestGuard(ctx, conn, hook.Object{}, hook.GuardOptions{Root: ownerMarkers(home), Mode: hook.Observe})
	_ = conn.Close()
	if err != nil || get(answer, "decision") != "release" {
		t.Fatal(answer, err)
	}
	return *record.Holder
}

// ownerMarkers is the marker root a daemon started in environment(home) resolves for itself
// (no CODEX_SESSION_RELAY_MARKER_ROOT, XDG_STATE_HOME=home/xdg-state): its control.sock
// evaluates Stops under that root only (hook ownerPaths, control.py owner_paths).
func ownerMarkers(home string) string {
	return home + "/xdg-state/codex-session-marker"
}

// Audit findings 0, 3, 4, 8, 11, 13, 17, 19, 32, 55 (decisions D1, D6): the todo-42
// sequence on one physical store. Go activates as the service supervisor, the retained
// Python fence build is launched as its own `service run --takeover-candidate` by the
// Go controller and activates at epoch 3, and Go takes the store back at epoch 4.
func Test30TakeoverBuiltCLI(t *testing.T) {
	home := seedTakeover(t)
	before, err := ownership.Physical(home + "/state/relay.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	policy := home + "/policy.json"
	if err = os.WriteFile(policy, []byte(`{"roles":{"parent":{"model":"test-model","reasoningEffort":"high"},"child":{"model":"test-model","reasoningEffort":"high"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	declare := exec.Command(testPython, "--state", home+"/state", "--socket", home+"/socket", "service", "declare", "--execution-policy", policy)
	declare.Env = environment(home)
	if raw, err := declare.CombinedOutput(); err != nil {
		t.Fatalf("python service declare: %v %s", err, raw)
	}
	for _, args := range [][]string{{"status", "--json"}, {"begin", "--to", "go"}, {"drain"}, {"transfer"}, {"activate"}, {"activate"}} {
		result, code := takeoverCLI(t, home, args...)
		if code != 0 {
			t.Fatalf("%v: %d %+v", args, code, result)
		}
		if args[0] == "activate" {
			if result["owner"] != "go" || result["phase"] != "active" || result["epoch"] != float64(2) {
				t.Fatal(result)
			}
		}
	}
	activeHolder(t, home, "dev")
	// No locator: refused before the reverse CAS, while Go still owns and serves.
	result, code := takeoverCLI(t, home, "rollback", "--to", "python")
	if code != 2 || result["reason"] != "store_owned_by_other" || !strings.Contains(text(result["detail"]), "--python-relay") {
		t.Fatalf("rollback without a Python locator: %d %+v", code, result)
	}
	status, code := takeoverCLI(t, home, "status", "--json")
	if code != 0 || status["owner"] != "go" || status["phase"] != "active" || status["epoch"] != float64(2) {
		t.Fatal(code, status)
	}
	// Audit finding 17: a Python client under the Go owner queues its ACK durably
	// (decision 25); the retained Python candidate the controller launches replays it.
	event := strings.Repeat("0", 32)
	queue := exec.Command(testPython, "--state", home+"/state", "ack", "--event", event, "--ack-turn", "parent-turn", "--ack-proof", "proof-1")
	queue.Env = environment(home)
	if raw, err := queue.Output(); err != nil || !strings.Contains(string(raw), "durably_queued") {
		t.Fatalf("python ack under the Go owner: %v %s", err, raw)
	}
	entry := home + "/state/takeover-inbox/ack." + event
	if _, err = os.Stat(entry); err != nil {
		t.Fatal(err)
	}
	result, code = takeoverCLI(t, home, "rollback", "--to", "python", "--python-relay", testPython)
	if code != 0 || result["owner"] != "python" || result["phase"] != "active" || result["epoch"] != float64(3) {
		t.Fatalf("rollback: %d %+v", code, result)
	}
	activeHolder(t, home, ownership.PythonBuild)
	if _, err = os.Stat(entry); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the Python candidate left the queued entry: %v", err)
	}
	stamp, cleanup, err := ownership.CopySnapshot(home + "/state/relay.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	snapshot, err := ownership.OpenExisting(t.Context(), stamp, "ro")
	if err != nil {
		t.Fatal(err)
	}
	var marker string
	err = snapshot.QueryRow("SELECT value FROM schema_meta WHERE key=?", "inbox:ack."+event).Scan(&marker)
	_ = snapshot.Close()
	if err != nil || !strings.Contains(marker, "payloadDigest") {
		t.Fatalf("no replay marker: %q %v", marker, err)
	}
	for _, args := range [][]string{{"begin", "--to", "go"}, {"drain"}, {"transfer"}, {"activate"}} {
		if result, code := takeoverCLI(t, home, args...); code != 0 {
			t.Fatalf("%v: %d %+v", args, code, result)
		}
	}
	status, code = takeoverCLI(t, home, "status", "--json")
	if code != 0 || status["owner"] != "go" || status["phase"] != "active" || status["epoch"] != float64(4) {
		t.Fatal(code, status)
	}
	activeHolder(t, home, "dev")
	after, err := ownership.Physical(home + "/state/relay.sqlite3")
	if err != nil || after != before {
		t.Fatal(after, err)
	}
}
func Test30StatusSchema(t *testing.T) {
	home := seedTakeover(t)
	status, code := takeoverCLI(t, home, "status", "--json")
	if code != 0 {
		t.Fatal(status)
	}
	raw, err := os.ReadFile(filepath.Join(testRoot, "contract/schema/records.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]struct {
		Keys    []string            `json:"keys"`
		Actions map[string][]string `json:"actions"`
	}
	if err = json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	// The frozen action set is exactly what the native command accepts (decision D2
	// adds abort). Against an absent store a known action fails on the store, an
	// unknown one as usage.
	actions := schema["takeoverStatus"].Actions
	if actions["abort"] == nil {
		t.Fatalf("takeover actions not frozen: %v", actions)
	}
	for _, action := range []string{"bogus", "candidate", "abort", "status"} {
		_, known := actions[action]
		cmd := exec.Command(testBinary, "relay", "--state", home+"/absent", "--socket", home+"/socket", "takeover", action)
		cmd.Env = environment(home)
		raw, err := cmd.Output()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || (exit.ExitCode() == 4) == known || known != !strings.Contains(string(raw), "unknown takeover action") {
			t.Fatalf("%s known=%t: %v %s", action, known, err, raw)
		}
	}
	for action := range actions {
		cmd := exec.Command(testBinary, "relay", "--state", home+"/absent", "--socket", home+"/socket", "takeover", action, "--to", map[string]string{"begin": "go", "rollback": "python"}[action])
		cmd.Env = environment(home)
		if raw, _ := cmd.Output(); strings.Contains(string(raw), "unknown takeover action") || strings.Contains(string(raw), "\"usage\"") {
			t.Fatalf("frozen action %s is not accepted: %s", action, raw)
		}
	}
	keys := map[string]bool{}
	for k := range status {
		keys[k] = true
	}
	expected := map[string]bool{}
	for _, k := range schema["takeoverStatus"].Keys {
		expected[k] = true
	}
	if !reflect.DeepEqual(keys, expected) {
		t.Fatal(keys, expected)
	}
}
func Test30ForeignOwnerCLIRefusesWithoutDBChanges(t *testing.T) {
	home := seedTakeover(t)
	path := home + "/state/relay.sqlite3"
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(testBinary, "relay", "--state", home+"/state", "--socket", home+"/socket", "daemon", "--allow-isolated-scope", "--max-ticks", "0")
	cmd.Env = environment(home)
	raw, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatal(err, string(raw))
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("foreign opener changed DB", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err = os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("foreign opener created sidecar", suffix, err)
		}
	}
	// Audit finding 54: the ownership preflight refuses before any service lock,
	// record or log exists, for the daemon and for every service action but status.
	// service status stays read-only and answers under a foreign owner, as in Python.
	status := exec.Command(testBinary, "relay", "--state", home+"/state", "--socket", home+"/socket", "service", "status")
	status.Env = environment(home)
	if raw, err := status.Output(); err != nil {
		t.Fatalf("service status under a foreign owner: %v %s", err, raw)
	}
	for _, args := range [][]string{{"daemon", "--allow-isolated-scope", "--max-ticks", "0"}, {"service", "start", "--allow-isolated-scope"}, {"service", "run", "--allow-isolated-scope"}, {"service", "run", "--takeover-candidate", "--allow-isolated-scope"}, {"service", "enable"}} {
		cmd := exec.Command(testBinary, append([]string{"relay", "--state", home + "/state", "--socket", home + "/socket"}, args...)...)
		cmd.Env = environment(home)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		raw := stdout.Bytes()
		if slices.Contains(args, "--takeover-candidate") {
			// A failed candidate run prints nothing on stdout, as Python's does; its
			// document is on stderr, the launched candidate's service log.
			if stdout.Len() != 0 {
				t.Fatalf("%v wrote stdout: %s", args, raw)
			}
			raw = stderr.Bytes()
		}
		var refused map[string]any
		if !errors.As(err, &exit) || exit.ExitCode() != 2 || json.Unmarshal(raw, &refused) != nil || refused["error"] != "refused" || refused["reason"] != "store_owned_by_other" {
			t.Fatalf("%v: %v %s", args, err, raw)
		}
		for _, name := range []string{"daemon.json", "daemon.log"} {
			if _, err = os.Stat(home + "/state/" + name); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%v left %s behind: %v", args, name, err)
			}
		}
		if after, err := os.ReadFile(path); err != nil || string(before) != string(after) {
			t.Fatalf("%v changed the database: %v", args, err)
		}
		if leftovers, _ := filepath.Glob(home + "/scopes/*.json"); len(leftovers) != 0 {
			t.Fatalf("%v registered a scope: %v", args, leftovers)
		}
	}
}

// Channel peer closes before active publication: Ready must refuse and close
// admission, not leave a writable unadvertised candidate. net.Pipe gives an
// exact EOF signal without sleeps.
func Test30ActivationChannelClosure(t *testing.T) {
	home := seedTakeover(t)
	path := home + "/state/relay.sqlite3"
	for _, args := range [][]string{{"begin", "--to", "go"}, {"transfer"}} {
		r, c := takeoverCLI(t, home, args...)
		if c != 0 {
			t.Fatal(r)
		}
	}
	r, err := ownership.ReadRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	channel := CandidateChannel{conn: client, Record: r}
	defer client.Close()
	done := make(chan error, 1)
	go func() { _, e := bufio.NewReader(server).ReadBytes('\n'); done <- errors.Join(e, server.Close()) }()
	if err = channel.Ready(t.Context(), "test-build"); err == nil {
		t.Fatal("unadvertised candidate continued after EOF")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func Test30ControllerCrashProcess(t *testing.T) {
	home := os.Getenv("CRW30_CONTROLLER_CRASH_HOME")
	if home == "" {
		return
	}
	t.Setenv(ScopeEnv, home+"/scopes")
	c, err := NewTakeover(t.Context(), store.StateSelection{Path: home + "/state"}, home+"/socket", "test", TakeoverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c.Fault = func(step, point string) error {
		if step == "transfer" && point == "db-committed" {
			os.Exit(91)
		}
		return nil
	}
	t.Fatalf("controller did not reach crash point: %v", c.Transfer(t.Context()))
}

func Test30ActivationEOFRequiresMatchingPublishedHolder(t *testing.T) {
	for _, matches := range []bool{false, true} {
		t.Run(map[bool]string{false: "other-holder", true: "matching-holder"}[matches], func(t *testing.T) {
			home := seedTakeover(t)
			t.Setenv(ScopeEnv, home+"/scopes")
			for _, args := range [][]string{{"begin", "--to", "go"}, {"transfer"}} {
				if answer, code := takeoverCLI(t, home, args...); code != 0 {
					t.Fatal(answer)
				}
			}
			path := home + "/state/relay.sqlite3"
			r, err := ownership.ReadRecord(path)
			if err != nil {
				t.Fatal(err)
			}
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			if err = client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			channel := CandidateChannel{conn: client, Record: r}
			done := make(chan error, 1)
			go func() { done <- channel.Ready(t.Context(), "candidate-build") }()
			var ready candidateMessage
			if err = json.NewDecoder(server).Decode(&ready); err != nil {
				t.Fatal(err)
			}
			r.Phase, r.Holder = "active", &ready.Identity
			if !matches {
				r.Holder.PID++
			}
			if err = ownership.Publish(path, r, nil); err != nil {
				t.Fatal(err)
			}
			if err = server.Close(); err != nil {
				t.Fatal(err)
			}
			if err = <-done; (err == nil) != matches {
				t.Fatalf("published holder matches=%t: %v", matches, err)
			}
		})
	}
}

func Test30BuiltCLIRecoversCommittedCrash(t *testing.T) {
	home := seedTakeover(t)
	if result, code := takeoverCLI(t, home, "begin", "--to", "go"); code != 0 {
		t.Fatal(result)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^Test30ControllerCrashProcess$")
	child.Env = append(os.Environ(), "CRW30_CONTROLLER_CRASH_HOME="+home)
	raw, err := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 91 {
		t.Fatalf("controller crash: %v %s", err, raw)
	}
	status, code := takeoverCLI(t, home, "status", "--json")
	if code != 0 || status["owner"] != "go" || status["phase"] != "starting" || status["jsonStale"] != true {
		t.Fatal(code, status)
	}
	status, code = takeoverCLI(t, home, "activate")
	if code != 0 || status["owner"] != "go" || status["phase"] != "active" || status["jsonStale"] != false {
		t.Fatal(code, status)
	}
	r, err := ownership.ReadRecord(home + "/state/relay.sqlite3")
	if err != nil || r.Holder == nil {
		t.Fatal(r, err)
	}
	process(t, r.Holder.PID)
}

// Drive the actual daemon's inherited channel. EOF is the event, not a sleep;
// a pidfd observes exit and flock acquisition proves every writer lock is gone.
func Test30RealCandidateControllerEOF(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-active", true: "after-active"}[published], func(t *testing.T) {
			home := seedTakeover(t)
			t.Setenv(ScopeEnv, home+"/scopes")
			path := home + "/state/relay.sqlite3"
			for _, args := range [][]string{{"begin", "--to", "go"}, {"transfer"}} {
				if result, code := takeoverCLI(t, home, args...); code != 0 {
					t.Fatal(result)
				}
			}
			r, err := ownership.ReadRecord(path)
			if err != nil {
				t.Fatal(err)
			}
			identity := ProcessIdentity("")
			r.Controller = &identity
			if err = ownership.Publish(path, r, nil); err != nil {
				t.Fatal(err)
			}
			pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			parent := os.NewFile(uintptr(pair[0]), "controller")
			child := os.NewFile(uintptr(pair[1]), "candidate")
			defer child.Close()
			conn, err := net.FileConn(parent)
			if err = errors.Join(err, parent.Close()); err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err = conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
				t.Fatal(err)
			}
			// Audit findings 21, 29, 52: the candidate reached through a symlinked state
			// directory routes to the same physical store.
			if err = os.Symlink(home+"/state", home+"/statelink"); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(testBinary, "relay", "--state", home+"/statelink", "--socket", home+"/socket", "service", "run", "--takeover-candidate", "--allow-isolated-scope")
			cmd.Env = append(environment(home), candidateFDEnv+"=3")
			cmd.ExtraFiles = []*os.File{child}
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
			if err = child.Close(); err != nil {
				t.Fatal(err)
			}
			h := process(t, cmd.Process.Pid)
			if err = json.NewEncoder(conn).Encode(candidateMessage{Kind: "start", Record: r}); err != nil {
				t.Fatal(err)
			}
			var ready candidateMessage
			if err = json.NewDecoder(conn).Decode(&ready); err != nil {
				t.Fatal(err)
			}
			if ready.Kind != "ready" || ready.Identity.PID != cmd.Process.Pid || ready.StoreID != r.StoreID || ready.Epoch != r.Epoch {
				t.Fatal(ready)
			}
			// Decision 30: readiness follows recovery and control-socket binding.
			if info, err := os.Lstat(ControlPath(home + "/state")); err != nil || info.Mode()&os.ModeSocket == 0 {
				t.Fatalf("ready before control.sock was bound: %v", err)
			}
			var events *watch
			if published {
				r.Phase, r.Holder = "active", &ready.Identity
				if err = ownership.Publish(path, r, nil); err != nil {
					t.Fatal(err)
				}
				events = watchDir(t, home+"/state")
			}
			if err = conn.Close(); err != nil {
				t.Fatal(err)
			}
			var worker *ProcessHandle
			if published {
				// Decision 30: the activated candidate releases its readiness listener, then
				// supervises. Its recorded worker is awaited, never raced: the guard below
				// reaches the worker's own control.sock, not a readiness listener closing
				// under it, and the interrupt lands while that worker holds the inherited
				// locks, as it does whenever the scheduler lets the supervisor spawn first.
				var run Object
				events.until(t, func() bool { run = read(home + "/state/daemon.json"); return truth(get(run, "workerPid")) })
				worker = process(t, num(get(run, "workerPid")))
				awaitControl(t, home+"/state")
				client, err := net.DialTimeout("unix", ControlPath(home+"/state"), 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				answer, err := hook.RequestGuard(ctx, client, hook.Object{}, hook.GuardOptions{Root: ownerMarkers(home), Mode: hook.Observe})
				_ = client.Close()
				if err != nil || get(answer, "decision") != "release" || h.Wait(0) || worker.Wait(0) {
					t.Fatal(answer, err)
				}
				// Decision 42: the interrupt reaches the supervisor alone, which passes it on
				// to its worker and exits only after that worker.
				if !h.Send(unix.SIGINT) {
					t.Fatal(h.Detail)
				}
			}
			if !h.Wait(5 * time.Second) {
				if published {
					t.Fatal("the interrupted supervisor outlived its interrupt while its worker ran")
				}
				t.Fatal("candidate outlived controller EOF without active publication")
			}
			if worker != nil && !worker.Wait(0) {
				t.Fatal("the supervisor exited before its worker, which holds the inherited locks")
			}
			scope := ScopeRegistry{Root: home + "/scopes", Authority: "isolated"}
			for _, lockPath := range []string{home + "/state/daemon.lock", scope.path(home+"/socket", ".lock"), home + "/state/write-gate.lock"} {
				f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
				if err = errors.Join(err, f.Close()); err != nil {
					t.Fatal("candidate retained lock", lockPath, err)
				}
			}
			t.Logf("CLEANUP candidate pid=%d exited; daemon/scope/write-gate locks released", cmd.Process.Pid)
		})
	}
}

// Audit findings 23, 53: daemon.json keeps the spelling the Python service was given.
// Drain compares state directory and socket by identity, reads a relative socket from
// the live holder's own cwd, and still refuses a different scope. Python writes the
// record (RelayService.new_record); a real process stands in for its holder.
func Test30DrainComparesRecordedIdentity(t *testing.T) {
	for _, tc := range []struct{ name, state, socket string }{
		{"symlinked-spellings", "statelink", "socketlink"},
		{"relative-socket", "state", "socket"},
		{"tilde-socket", "state", "~/socket"},
		{"other-scope", "state", "/elsewhere/socket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := seedTakeover(t)
			t.Setenv(ScopeEnv, home+"/scopes")
			t.Setenv("HOME", home)
			for _, link := range [][2]string{{home + "/state", home + "/statelink"}, {home + "/socket", home + "/socketlink"}} {
				if err := os.Symlink(link[0], link[1]); err != nil {
					t.Fatal(err)
				}
			}
			record, err := ownership.ReadRecord(home + "/state/relay.sqlite3")
			if err != nil {
				t.Fatal(err)
			}
			holder := exec.Command("sleep", "30")
			holder.Dir = home
			if err = holder.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
			socket := tc.socket
			if tc.name == "symlinked-spellings" {
				socket = home + "/" + tc.socket
			}
			script := "import sys\nfrom codex_session_relay.service import RelayService\nfrom codex_session_relay.store import resolve_state_dir\n" +
				"service = RelayService(resolve_state_dir(sys.argv[1]), socket_path=sys.argv[2], store_id=sys.argv[3])\n" +
				"service.write_record(service.new_record(pid=int(sys.argv[4])))"
			python := exec.Command(filepath.Join(filepath.Dir(testPython), "python"), "-c", script, home+"/"+tc.state, socket, record.StoreID, strconv.Itoa(holder.Process.Pid))
			python.Env = append(environment(home), "PYTHONDONTWRITEBYTECODE=1")
			if raw, err := python.CombinedOutput(); err != nil {
				t.Fatal(err, string(raw))
			}
			c, err := NewTakeover(t.Context(), store.StateSelection{Path: home + "/state"}, home+"/socket", "test", TakeoverOptions{})
			if err != nil {
				t.Fatal(err)
			}
			err = c.Runtime.Drain(t.Context(), record)
			if tc.name == "other-scope" {
				if err == nil || !strings.Contains(err.Error(), "daemon identity disagrees") {
					t.Fatalf("a different scope was drained: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if state := holder.Wait(); state == nil {
				t.Fatal("holder was not stopped")
			}
		})
	}
}

// pythonCheckStart is the retained fence's own admission preflight on the store.
func pythonCheckStart(t *testing.T, home string) error {
	t.Helper()
	cmd := exec.Command(filepath.Join(filepath.Dir(testPython), "python"), "-c", "import sys\nfrom codex_session_relay import ownership\nownership.check_start(sys.argv[1])", home+"/state/relay.sqlite3")
	cmd.Env = append(environment(home), "PYTHONDONTWRITEBYTECODE=1")
	if raw, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, raw)
	}
	return nil
}

// Audit findings 2, 6, 10 and 12 (decisions D2, D6): the built CLI aborts a pre-CAS
// transition back to active Python, whose own fence admits again. A Go candidate that
// cannot drain the takeover inbox (a corrupt entry fails closed, decision 25) refuses
// readiness: ownership stays starting, abort refuses after the CAS, and the reverse begin
// of that failed candidate aborts back to starting, never to active.
func Test30AbortAndFailedCandidateBuiltCLI(t *testing.T) {
	home := seedTakeover(t)
	golden, err := os.ReadFile(filepath.Join(testRoot, "contract/golden/takeover-inbox/ack.json"))
	if err != nil {
		t.Fatal(err)
	}
	if result, code := takeoverCLI(t, home, "begin", "--to", "go"); code != 0 {
		t.Fatal(result)
	}
	if err = pythonCheckStart(t, home); err == nil || !strings.Contains(err.Error(), "draining") {
		t.Fatalf("Python admission open while draining: %v", err)
	}
	result, code := takeoverCLI(t, home, "abort")
	if code != 0 || result["owner"] != "python" || result["phase"] != "active" || result["epoch"] != float64(1) || result["transition"] != nil {
		t.Fatalf("abort: %d %+v", code, result)
	}
	if err = pythonCheckStart(t, home); err != nil {
		t.Fatalf("Python admission did not reopen after abort: %v", err)
	}
	for _, args := range [][]string{{"begin", "--to", "go"}, {"drain"}, {"transfer"}} {
		if result, code := takeoverCLI(t, home, args...); code != 0 {
			t.Fatalf("%v: %d %+v", args, code, result)
		}
	}
	// A grammar-valid entry whose bytes do not validate is storage corruption: the
	// candidate's drain fails closed and it never becomes ready.
	inbox := home + "/state/takeover-inbox"
	if err = os.MkdirAll(inbox, 0700); err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Replace(golden, []byte("sha256:c40a"), []byte("sha256:c40b"), 1)
	if err = os.WriteFile(inbox+"/ack.event-1", corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if result, code = takeoverCLI(t, home, "activate"); code == 0 {
		t.Fatalf("candidate became ready over a corrupt entry: %+v", result)
	}
	if log, e := os.ReadFile(home + "/state/daemon.log"); e != nil || !bytes.Contains(log, []byte("inbox payload digest or identifier disagrees")) {
		t.Fatalf("the candidate did not fail on the corrupt entry: %v %s", e, log)
	}
	status, code := takeoverCLI(t, home, "status", "--json")
	if code != 0 || status["owner"] != "go" || status["phase"] != "starting" || status["epoch"] != float64(2) {
		t.Fatal(code, status)
	}
	result, code = takeoverCLI(t, home, "abort")
	if code != 2 || !strings.Contains(text(result["detail"]), "takeover activate") {
		t.Fatalf("abort after the CAS: %d %+v", code, result)
	}
	// Review of decision D2: the reverse transfer from this failed candidate fails
	// before its CAS (a busy daemon.lock), and abort returns it to starting, never to
	// active: no Go candidate became ready, so no Go service may serve over the entry.
	lock, err := os.OpenFile(home+"/state/daemon.lock", os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	result, code = takeoverCLI(t, home, "rollback", "--to", "python", "--python-relay", testPython)
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	if code == 0 {
		t.Fatalf("rollback past a busy daemon.lock: %+v", result)
	}
	status, code = takeoverCLI(t, home, "status", "--json")
	if code != 0 || status["owner"] != "go" || status["phase"] != "draining" || status["epoch"] != float64(2) {
		t.Fatal(code, status)
	}
	result, code = takeoverCLI(t, home, "abort")
	if code != 0 || result["owner"] != "go" || result["phase"] != "starting" || result["epoch"] != float64(2) || result["holder"] != nil || result["jsonStale"] != false {
		t.Fatalf("abort of the reverse begin: %d %+v", code, result)
	}
	start := exec.Command(testBinary, "relay", "--state", home+"/state", "--socket", home+"/socket", "service", "start", "--allow-isolated-scope")
	start.Env = environment(home)
	raw, err := start.Output()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(raw), "only the designated candidate may enter starting") {
		t.Fatalf("ordinary Go service started over an unactivated owner: %v %s", err, raw)
	}
	if kept, err := os.ReadFile(inbox + "/ack.event-1"); err != nil || !bytes.Equal(kept, corrupt) {
		t.Fatalf("the corrupt entry was not kept for the operator: %v", err)
	}
}

// Todo 31 (decision 25, cutover.md Step 6): while the store drains toward Go, the retained
// Python fence's clients queue their acknowledgments durably (100 acks and a
// fault-notification-ack, one Python process); the Go candidate replays every entry under its
// starting permit, with its App Server socket as host, before it reports readiness. After
// activation the inbox is empty and each entry has its marker with the answer the direct
// command gives (exit 2: nothing it acknowledges exists), and a writable command of the active
// Go owner applies nothing again. The domain-row QA runs in process (internal/relay/cli), where
// no App Server is needed to apply a receipt.
func Test31GoCandidateDrainsPythonQueuedEntriesBuiltCLI(t *testing.T) {
	home := seedTakeover(t)
	policy := home + "/policy.json"
	if err := os.WriteFile(policy, []byte(`{"roles":{"parent":{"model":"test-model","reasoningEffort":"high"},"child":{"model":"test-model","reasoningEffort":"high"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	declare := exec.Command(testPython, "--state", home+"/state", "--socket", home+"/socket", "service", "declare", "--execution-policy", policy)
	declare.Env = environment(home)
	if raw, err := declare.CombinedOutput(); err != nil {
		t.Fatalf("python service declare: %v %s", err, raw)
	}
	if result, code := takeoverCLI(t, home, "begin", "--to", "go"); code != 0 {
		t.Fatal(result)
	}
	script := `import contextlib, io, json, sys
from codex_session_relay import cli
answers = []
argvs = [["ack", "--event", "%032x" % (0xa0 + n), "--ack-turn", "parent-turn", "--ack-proof", "proof-1"] for n in range(100)]
argvs.append(["fault-notification-ack", "--notification", "notice-1", "--token", "token-1", "--ref", "receipt-1"])
for argv in argvs:
    out = io.StringIO()
    with contextlib.redirect_stdout(out):
        code = cli.main(["--state", sys.argv[1], *argv])
    answers.append([code, json.loads(out.getvalue())])
json.dump(answers, sys.stdout)
`
	queue := exec.Command(filepath.Join(filepath.Dir(testPython), "python"), "-c", script, home+"/state")
	queue.Env = append(environment(home), "PYTHONDONTWRITEBYTECODE=1")
	raw, err := queue.Output()
	if err != nil {
		t.Fatalf("python queue: %v %s", err, raw)
	}
	var answers [][2]any
	if err = json.Unmarshal(raw, &answers); err != nil || len(answers) != 101 {
		t.Fatalf("%v %s", err, raw)
	}
	for _, answer := range answers {
		if object, _ := answer[1].(map[string]any); answer[0] != float64(0) || object["status"] != "durably_queued" {
			t.Fatalf("a Python acknowledgment during draining was not queued: %v", answer)
		}
	}
	entries := func() []string {
		t.Helper()
		all, err := os.ReadDir(home + "/state/takeover-inbox")
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, entry := range all {
			if !strings.HasPrefix(entry.Name(), ".") {
				names = append(names, entry.Name())
			}
		}
		return names
	}
	if n := len(entries()); n != 101 {
		t.Fatalf("%d entries queued", n)
	}
	for _, args := range [][]string{{"drain"}, {"transfer"}, {"activate"}} {
		if result, code := takeoverCLI(t, home, args...); code != 0 {
			t.Fatalf("%v: %d %+v", args, code, result)
		}
	}
	activeHolder(t, home, "dev")
	if left := entries(); len(left) != 0 {
		t.Fatalf("the Go candidate became ready over queued entries: %v", left)
	}
	markers := func() map[string]string {
		t.Helper()
		snapshot, cleanup, err := ownership.CopySnapshot(home + "/state/relay.sqlite3")
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		db, err := ownership.OpenExisting(t.Context(), snapshot, "ro")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := db.Query("SELECT key, value FROM schema_meta WHERE key LIKE 'inbox%'")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var key, value string
			if err = rows.Scan(&key, &value); err != nil {
				t.Fatal(err)
			}
			out[key] = value
		}
		return out
	}
	before := markers()
	if len(before) != 101 {
		t.Fatalf("%d markers", len(before))
	}
	// Each marker is the direct command's own answer, which the active Go owner still gives.
	direct := func(argv ...string) map[string]any {
		t.Helper()
		cmd := exec.Command(testBinary, append([]string{"relay", "--state", home + "/state", "--socket", home + "/socket"}, argv...)...)
		cmd.Env = environment(home)
		raw, _ := cmd.Output()
		var answer map[string]any
		if err := json.Unmarshal(raw, &answer); err != nil {
			t.Fatalf("%v %s", err, raw)
		}
		return answer
	}
	for key, argv := range map[string][]string{
		"inbox:ack." + strings.Repeat("0", 30) + "a0": {"ack", "--event", strings.Repeat("0", 30) + "a0", "--ack-turn", "parent-turn", "--ack-proof", "proof-1"},
		"inbox:fault-notification-ack.notice-1":       {"fault-notification-ack", "--notification", "notice-1", "--token", "token-1", "--ref", "receipt-1"},
	} {
		var marker map[string]any
		if err = json.Unmarshal([]byte(before[key]), &marker); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if answer := direct(argv...); marker["exit"] != float64(2) || !reflect.DeepEqual(marker["answer"], answer) {
			t.Fatalf("%s: marker %v, direct answer %v", key, marker, answer)
		}
	}
	if after := markers(); !reflect.DeepEqual(after, before) {
		t.Fatal("a writable command of the active owner changed the markers")
	}
}

// PR #185 thread 4128457226: while Go owns the store the retained Python CLI queues its
// receipts in S/takeover-inbox, which the Go owner applies at its next drain (todo 31), not
// at the commit. takeover commit ends every way back to a Python owner (rollback_allowed=0),
// so it refuses while an entry the replay would read is still queued, changes nothing, and
// names the recovery; the writable Go command it names drains the entry, and the commit then
// closes the way back over an empty inbox. A name outside the entry grammar blocks nothing.
func Test30CommitRefusesOverQueuedInboxEntries(t *testing.T) {
	home := seedTakeover(t)
	for _, args := range [][]string{{"begin", "--to", "go"}, {"drain"}, {"transfer"}, {"activate"}} {
		if result, code := takeoverCLI(t, home, args...); code != 0 {
			t.Fatalf("%v: %d %+v", args, code, result)
		}
	}
	relay := func(argv ...string) ([]byte, error) {
		t.Helper()
		cmd := exec.Command(testBinary, append([]string{"relay", "--state", home + "/state", "--socket", home + "/socket"}, argv...)...)
		cmd.Env = environment(home)
		return cmd.Output()
	}
	// No Go daemon runs while the entry is queued, so only the recovery below can drain it
	// (a worker drains at its start, which would race the queueing).
	if raw, err := relay("service", "stop"); err != nil {
		t.Fatalf("go service stop: %v %s", err, raw)
	}
	event := strings.Repeat("1", 32)
	queue := exec.Command(testPython, "--state", home+"/state", "ack", "--event", event, "--ack-turn", "parent-turn", "--ack-proof", "proof-1")
	queue.Env = environment(home)
	if raw, err := queue.Output(); err != nil || !strings.Contains(string(raw), "durably_queued") {
		t.Fatalf("python ack under the Go owner: %v %s", err, raw)
	}
	entry := home + "/state/takeover-inbox/ack." + event
	// Names the replay never reads, so never removes: an unpublished temporary, one outside
	// the grammar and one over 200 characters. None of them is counted.
	ignored := []string{".tmp-ack." + event + "-1-00", "not an entry", strings.Repeat("a", 201)}
	for _, name := range ignored {
		if err := os.WriteFile(home+"/state/takeover-inbox/"+name, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	result, code := takeoverCLI(t, home, "commit")
	if detail := text(result["detail"]); code != 2 || result["reason"] != "store_owned_by_other" || !strings.Contains(detail, "takeover inbox holds 1 queued entries the Go owner has not applied yet") || !strings.Contains(detail, "store-challenge --write") || !strings.Contains(detail, "restart the Go service") {
		t.Fatalf("commit over a queued entry: %d %+v", code, result)
	}
	status, code := takeoverCLI(t, home, "status", "--json")
	if code != 0 || status["owner"] != "go" || status["phase"] != "active" || status["epoch"] != float64(2) || status["rollbackAllowed"] != true || status["jsonStale"] != false {
		t.Fatalf("commit changed the store: %d %+v", code, status)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("queued entry: %v", err)
	}
	// The named recovery: a writable Go relay command drains the inbox before its handler.
	if raw, err := relay("store-challenge", "--write", "--actor", "t30"); err != nil {
		t.Fatalf("go store-challenge --write: %v %s", err, raw)
	}
	if _, err := os.Stat(entry); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the Go drain left the queued entry: %v", err)
	}
	snapshot, cleanup, err := ownership.CopySnapshot(home + "/state/relay.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	db, err := ownership.OpenExisting(t.Context(), snapshot, "ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var raw string
	var marker map[string]any
	if err = db.QueryRow("SELECT value FROM schema_meta WHERE key=?", "inbox:ack."+event).Scan(&raw); err != nil || json.Unmarshal([]byte(raw), &marker) != nil || marker["exit"] != float64(2) {
		t.Fatalf("the Go drain applied no marker: %v %s", err, raw)
	}
	result, code = takeoverCLI(t, home, "commit")
	if code != 0 || result["owner"] != "go" || result["phase"] != "active" || result["epoch"] != float64(2) {
		t.Fatalf("commit after the drain: %d %+v", code, result)
	}
	status, code = takeoverCLI(t, home, "status", "--json")
	if code != 0 || status["owner"] != "go" || status["phase"] != "active" || status["epoch"] != float64(2) || status["rollbackAllowed"] != false {
		t.Fatalf("commit after the drain: %d %+v", code, status)
	}
	for _, name := range ignored {
		if _, err := os.Stat(home + "/state/takeover-inbox/" + name); err != nil {
			t.Fatalf("a name outside the entry grammar was removed: %v", err)
		}
	}
}

// Audit finding 31: the controller bounds readiness, which spans recovery, by its own
// --ready-timeout rather than the 20-second channel bound. A retained entry point that
// never answers is given up on at that bound and ownership stays starting.
func Test30ReadyTimeoutBoundsSilentCandidate(t *testing.T) {
	home := seedTakeover(t)
	for _, args := range [][]string{{"begin", "--to", "go"}, {"drain"}, {"transfer"}, {"activate"}} {
		if result, code := takeoverCLI(t, home, args...); code != 0 {
			t.Fatalf("%v: %d %+v", args, code, result)
		}
	}
	silent := home + "/silent-relay"
	// It reads the channel and never answers; the controller's close ends it at once.
	if err := os.WriteFile(silent, []byte("#!/bin/sh\nexec cat <&3 >/dev/null\n"), 0700); err != nil {
		t.Fatal(err)
	}
	// Proactive sweep: a locator that is missing, a directory or not executable, and a
	// malformed bound are refused before any durable edge; Go keeps serving.
	if err := os.WriteFile(home+"/plain-file", nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--python-relay", home + "/missing"}, {"--python-relay", home}, {"--python-relay", home + "/plain-file"}} {
		if result, code := takeoverCLI(t, home, append([]string{"rollback", "--to", "python"}, args...)...); code != 2 || !strings.Contains(text(result["detail"]), "--python-relay") {
			t.Fatalf("%v: %d %+v", args, code, result)
		}
	}
	for _, bound := range []string{"0", "-1", "nan", "inf", "soon", "1e300"} {
		if result, code := takeoverCLI(t, home, "rollback", "--to", "python", "--python-relay", silent, "--ready-timeout", bound); code != 4 {
			t.Fatalf("--ready-timeout %s: %d %+v", bound, code, result)
		}
	}
	for _, args := range [][]string{{"abort", "--to", "go"}, {"abort", "extra"}} {
		if result, code := takeoverCLI(t, home, args...); code != 4 {
			t.Fatalf("%v: %d %+v", args, code, result)
		}
	}
	if status, code := takeoverCLI(t, home, "status", "--json"); code != 0 || status["owner"] != "go" || status["phase"] != "active" || status["epoch"] != float64(2) {
		t.Fatal(code, status)
	}
	started := time.Now()
	result, code := takeoverCLI(t, home, "rollback", "--to", "python", "--python-relay", silent, "--ready-timeout", "1")
	if code == 0 || time.Since(started) > 12*time.Second {
		t.Fatalf("silent candidate: %d after %s %+v", code, time.Since(started), result)
	}
	status, code := takeoverCLI(t, home, "status", "--json")
	if code != 0 || status["owner"] != "python" || status["phase"] != "starting" || status["holder"] != nil {
		t.Fatal(code, status)
	}
	if result, code = takeoverCLI(t, home, "status", "--ready-timeout", "1"); code != 4 {
		t.Fatalf("--ready-timeout accepted outside activate and rollback: %d %+v", code, result)
	}
	if result, code = takeoverCLI(t, home, "activate", "--python-relay", "relative/relay"); code != 4 {
		t.Fatalf("relative --python-relay accepted: %d %+v", code, result)
	}
}

// Devin 4127894020 (decision 28): the built Go controller waits for readiness under its own
// --ready-timeout, so a Python candidate whose recovery outlasts the channel bound must still
// accept activation: its start bound does not carry over into the exchange after ready. The
// retained Python entry point runs unchanged except for two test-only injections, a 1 s
// channel bound and a 2 s recovery, so the test stays fast.
func Test30PythonCandidateActivatesAfterRecoveryLongerThanTheChannelBound(t *testing.T) {
	home := seedTakeover(t)
	for _, args := range [][]string{{"begin", "--to", "go"}, {"drain"}, {"transfer"}, {"activate"}} {
		if result, code := takeoverCLI(t, home, args...); code != 0 {
			t.Fatalf("%v: %d %+v", args, code, result)
		}
	}
	slow := home + "/slow-relay"
	script := "#!" + filepath.Join(testRoot, ".venv/bin/python") + `
import sys, time
from codex_session_relay import takeover
takeover.CHANNEL_TIMEOUT = 1.0
ready = takeover.CandidateChannel.ready
def slow(self):
    time.sleep(2.0)
    return ready(self)
takeover.CandidateChannel.ready = slow
from codex_session_relay.cli import main
sys.exit(main())
`
	if err := os.WriteFile(slow, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	result, code := takeoverCLI(t, home, "rollback", "--to", "python", "--python-relay", slow, "--ready-timeout", "30")
	if code != 0 || result["owner"] != "python" || result["phase"] != "active" || result["epoch"] != float64(3) {
		t.Fatalf("rollback with a slow recovery: %d %+v", code, result)
	}
	activeHolder(t, home, ownership.PythonBuild)
}

// Decision D6: the drained Go holder is a supervisor waiting on its worker. Drain
// interrupts both together, so the supervisor exits gracefully (it clears its own
// record) instead of being killed after the escalation bound.
func Test30DrainStopsGoSupervisorGracefully(t *testing.T) {
	home := seedTakeover(t)
	for _, args := range [][]string{{"begin", "--to", "go"}, {"drain"}, {"transfer"}, {"activate"}} {
		if result, code := takeoverCLI(t, home, args...); code != 0 {
			t.Fatalf("%v: %d %+v", args, code, result)
		}
	}
	awaitControl(t, home+"/state")
	record, err := ownership.ReadRecord(home + "/state/relay.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for !truth(get(read(home+"/state/daemon.json"), "workerPid")) {
		if time.Now().After(deadline) {
			t.Fatal("supervisor never started a worker")
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Setenv(ScopeEnv, home+"/scopes")
	c, err := NewTakeover(t.Context(), store.StateSelection{Path: home + "/state"}, home+"/socket", "test", TakeoverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err = c.Runtime.Drain(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	run := read(home + "/state/daemon.json")
	if elapsed := time.Since(started); elapsed > 4*time.Second || get(run, "pid") != nil || get(run, "stoppedAt") == nil {
		t.Fatalf("supervisor was not drained gracefully after %s: %v", elapsed, run)
	}
}
