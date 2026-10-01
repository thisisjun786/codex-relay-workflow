//go:build linux

package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// PR #185 4128954449: nothing a peer or the kernel does ends or fails the owner. Every frame
// below is one the owner cannot serve, and it answers each with the relay's host record, exactly
// {"error": "host", "detail": <why>}: a frame that is not JSON, nests past the owner's cap (or
// past a goroutine stack), is not an object, lacks params or a stop input, carries a deadline
// that is not a string, not an RFC 3339 time, naive or past, or a mode, now, socketPath or program
// that is not a string, and a peer whose request line is not complete when the 5 s read bound
// expires, whether it sent nothing more or trickled the line in parts. noRecord true is served
// and asks for no record (hold downgrades to observe). A peer that hangs up after a complete
// request is skipped, and a failed accept is retried. The owner still serves the next Stop
// afterwards, and none of it reaches Close, so a daemon segment whose work succeeded exits 0.
// (The reading of each frame is hook's TestControlAnswersEveryRequestItCannotServeWithTheHostRecord.)
func Test30ControlPeerFailuresAreAnsweredWithTheHostRecord(t *testing.T) {
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
	failures := []string{"not json", "nested past the cap", "nested past a goroutine stack", "not an object", "no params",
		"params not an object", "no stop input", "stop input not an object", "null deadline", "unparseable deadline",
		"expired deadline", "naive deadline", "socketPath not a string", "program not a string", "mode not a string",
		"now not a string", "silent past the read timeout", "trickled past the read timeout"}
	frames := map[string][]byte{
		"not json":                      []byte("not json\n"),
		"nested past the cap":           []byte(strings.Repeat("[", 9997) + strings.Repeat("]", 9997) + "\n"),
		"nested past a goroutine stack": []byte(`{"params":` + strings.Repeat("[", 5000000) + "\n"),
		"not an object":                 []byte("[1]\n"),
		"no params":                     []byte(`{"protocol":1,"method":"guard-evaluate"}` + "\n"),
		"params not an object":          frame("[]"),
		"no stop input":                 frame("{}"),
		"stop input not an object":      frame(`{"stopInput":[]}`),
		"null deadline":                 frame(params("null", "")),
		"unparseable deadline":          frame(params(`"soon"`, "")),
		"expired deadline":              frame(params(`"2020-01-01T00:00:00+00:00"`, "")),
		"naive deadline":                frame(params(`"2999-01-01T00:00:00"`, "")),
		// No line end: the owner waits for the rest of the line until its read timeout.
		"silent past the read timeout": []byte(`{"protocol":1`),
		"socketPath not a string":      frame(params(later, `,"socketPath":1`)),
		"program not a string":         frame(params(later, `,"program":["crw"]`)),
		// A later key replaces an earlier one.
		"mode not a string": frame(params(later, `,"mode":5`)),
		"now not a string":  frame(params(later, `,"now":0`)),
	}
	hostRecord := func(t *testing.T, name string, answer []byte) {
		t.Helper()
		var record map[string]any
		if err := json.Unmarshal(answer, &record); err != nil {
			t.Errorf("%s: the owner answered %q", name, answer)
			return
		}
		if detail, ok := record["detail"].(string); len(record) != 2 || record["error"] != "host" || !ok || detail == "" {
			t.Errorf("%s: the owner answered %q, not the host record", name, answer)
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
	// Served: noRecord true asks for no record, which downgrades hold to observe.
	servedFrames := map[string][]byte{
		"noRecord true":  frame(params(later, `,"mode":"hold","noRecord":true`)),
		"noRecord false": frame(params(later, `,"mode":"hold","noRecord":false`)),
	}
	// served asks every failing frame and the served frames, then hangs up after a complete
	// request, then asks a Stop the owner must still answer with a verdict.
	served := func(t *testing.T, path string) map[string][]byte {
		t.Helper()
		answers := map[string][]byte{}
		for _, failure := range failures {
			if failure == "trickled past the read timeout" {
				answers[failure] = trickle(t, path)
				continue
			}
			answers[failure] = ask(t, path, frames[failure])
		}
		for name, request := range servedFrames {
			answers[name] = ask(t, path, request)
			var verdict map[string]any
			if err := json.Unmarshal(answers[name], &verdict); err != nil || verdict["decision"] != "release" {
				t.Errorf("the owner did not serve %s: %q %v", name, answers[name], err)
			}
		}
		var verdict map[string]any
		if json.Unmarshal(answers["noRecord true"], &verdict) != nil || verdict["modeDowngraded"] != "hold_requires_a_recorded_observation" {
			t.Errorf("a noRecord of true did not ask for no record: %q", answers["noRecord true"])
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
			hostRecord(t, failure, answers[failure])
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
		hostRecord(t, "params not an object", ask(t, path, frames["params not an object"]))
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
