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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
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
	conn, err := net.Dial("unix", controlPath(state))
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
	conn, err = net.Dial("unix", controlPath(state))
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
		address, release, err := hook.ControlAddress(controlPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.Dial("unix", address)
		release()
		if err != nil {
			t.Fatal(len(controlPath(dir)), err)
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
		if _, err = os.Lstat(controlPath(dir)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("closed control socket left behind at %d bytes: %v", len(controlPath(dir)), err)
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
		probe(t, controlPath(state), nil)
		probe(t, controlPath(state), []byte(`{"protocol":1,"method":"guard-`))
		// The listener still serves, and the probes' handlers finish before Close answers.
		root := t.TempDir()
		t.Setenv("CODEX_SESSION_RELAY_MARKER_ROOT", root)
		conn, err := net.Dial("unix", controlPath(state))
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
		path := controlPath(home + "/state")
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
// answered with its host record, and the Go owner answers it with the same bytes (its golden
// began as the Python owner's answers): among them the
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
		answers := served(t, controlPath(state))
		for _, failure := range failures {
			want := `{"error": "host", "detail": "` + failure.detail + `"}` + "\n"
			if string(answers[failure.name]) != want {
				t.Errorf("%s: the Go owner answered %q, not %q", failure.name, answers[failure.name], want)
			}
		}
		// The failing frames and the edge corpus are answered byte for byte as the golden holds
		// them; the served frames with the same verdict (the golden began as the Python owner's
		// answers, whose serializer spaced a verdict line its own way).
		refusals := map[string]string{}
		for _, failure := range failures {
			refusals[failure.name] = string(answers[failure.name])
		}
		for name := range edge {
			refusals["edge "+name] = string(answers["edge "+name])
		}
		verdicts := map[string]any{}
		for name := range servedFrames {
			var verdict any
			if err := json.Unmarshal(answers[name], &verdict); err != nil {
				t.Fatalf("%s: %v %q", name, err, answers[name])
			}
			verdicts[name] = verdict
		}
		golden.CheckJSON(t, "answers", refusals, golden.Substitute(home, "<HOME>"))
		golden.CheckJSON(t, "verdicts", verdicts, golden.Substitute(home, "<HOME>"))
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
		if err := json.Unmarshal(ask(t, controlPath(state), frame(params(later, ""))), &verdict); err != nil || verdict["decision"] != "release" || len(refused) != 2 {
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
		path := controlPath(home + "/state")
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
		want := `{"error": "host", "detail": "TypeError: guard params must be an object"}` + "\n"
		if answer := ask(t, path, frames["params not an object"]); string(answer) != want {
			t.Errorf("the daemon answered %q, not %q", answer, want)
		}
		hangUp(t, path, frame(params(later, "")))
		err := daemon.Wait()
		var result map[string]any
		if err != nil || json.Unmarshal(stdout.Bytes(), &result) != nil || result["ok"] != true {
			t.Fatalf("daemon after failed peers: %v\n%s%s", err, stdout.String(), stderr.String())
		}
	})
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
	// service is enabled, as on the host. Its own CLI recorded the intent until todo 44; the
	// intent file is the one Go's service enable writes (Test29ConsoleParity), written here by
	// the same code, since Go's CLI refuses to enable a service over a store Python owns.
	if _, err := (&Service{Selection: storeSelection(home)}).Enable("cli"); err != nil {
		t.Fatalf("service enable: %v", err)
	}
	return home
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
	for _, args := range [][]string{{"daemon", "--allow-isolated-scope", "--max-ticks", "0"}, {"service", "start", "--allow-isolated-scope"}, {"service", "run", "--allow-isolated-scope"}, {"service", "enable"}} {
		cmd := exec.Command(testBinary, append([]string{"relay", "--state", home + "/state", "--socket", home + "/socket"}, args...)...)
		cmd.Env = environment(home)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		raw := stdout.Bytes()
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

// controlPath is the owner's control socket in state dir S.
func controlPath(state string) string { return filepath.Join(state, "control.sock") }
