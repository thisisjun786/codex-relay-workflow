// Package exercise runs a Go runtime's two components the way a point has to be earned
// (OPS-1.3): through the concrete executables of one install, against the App Server this host
// talks to. Starting a process is not an exercise. The relay is exercised by a doctor whose
// actorReachability.socketConnect is a real connect, and the bridge by a read-only MCP session
// that lists its tools and calls its identity tool (get_capabilities), which is what
// packages/codex-thread-bridge/scripts/check_connection.py did for the Python install. Any
// connection, protocol or tool-call failure is a failed exercise. Nothing here creates work.
package exercise

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// Object is a decoded JSON object.
type Object = record.Object

// BridgeTimeout bounds one bridge session, as check_connection's 180 s subprocess budget did.
var BridgeTimeout = 180 * time.Second

// WaitDelay bounds how long a finished or killed bridge's output may be held open by a
// descendant before the session gives up on it.
var WaitDelay = 5 * time.Second

// protocolVersion is the MCP revision the session offers; the server answers with the one it
// speaks, and nothing here depends on which.
const protocolVersion = "2025-06-18"

// Relay is the relay half: `<relay> [--socket S] [--state D] doctor`, exercised when the
// doctor's socketConnect is "ok".
func Relay(ctx context.Context, executable, socket, state string, env scope.Env) Object {
	doctor := scope.Relay(ctx, []string{"doctor"}, executable, socket, state, env, false, 0)
	payload, _ := record.Get(doctor, "payload").(Object)
	reach, _ := record.Get(payload, "actorReachability").(Object)
	connect := record.Get(reach, "socketConnect")
	return Object{
		{Key: "component", Value: "codex-session-relay"},
		{Key: "command", Value: record.Get(doctor, "command")},
		{Key: "exercised", Value: connect == "ok"},
		{Key: "detail", Value: "actorReachability.socketConnect = " + reading.Show(connect) + "; a real connect is what makes this an exercise rather than a file read"},
	}
}

// Bridge is the result of one bridge session.
type Bridge struct {
	Command    []string
	Tools      []string
	Connection any // get_capabilities' structuredContent, in the order the server wrote it
	Err        error
	// ExitCode and Stderr describe the process when the session did not complete.
	ExitCode int
	Stderr   string
}

// AppServer is the App Server dimension a point records: json.dumps of get_capabilities'
// structured content (default separators, key order as the server wrote it), or nil when the
// session did not produce one.
func (b Bridge) AppServer() *string {
	if b.Err != nil || b.Connection == nil {
		return nil
	}
	text := pyjson.Dumps(b.Connection, pyjson.Options{})
	return &text
}

// Exercised is whether the session listed identityTool and called it successfully.
func (b Bridge) Exercised(identityTool string) bool {
	if b.Err != nil {
		return false
	}
	for _, name := range b.Tools {
		if name == identityTool {
			return true
		}
	}
	return false
}

// Operation is the bridge half as a point's operation reports it.
func (b Bridge) Operation(identityTool string) Object {
	detail := "listed " + fmt.Sprint(len(b.Tools)) + " tools and called " + identityTool
	if !b.Exercised(identityTool) {
		switch {
		case b.Err != nil:
			detail = b.Err.Error()
		default:
			detail = "the tool list does not contain " + identityTool
		}
	}
	tools := make([]any, len(b.Tools))
	for i, name := range b.Tools {
		tools[i] = name
	}
	command := make([]any, len(b.Command))
	for i, word := range b.Command {
		command[i] = word
	}
	return Object{
		{Key: "component", Value: "codex-thread-bridge"},
		{Key: "command", Value: command},
		{Key: "exercised", Value: b.Exercised(identityTool)},
		{Key: "toolsListed", Value: tools},
		{Key: "detail", Value: detail},
	}
}

// Session starts `<bridge> --state-dir <temporary> [--socket S]`, initializes an MCP session
// over its stdio, lists its tools and calls get_capabilities. The ledger it opens lives in a
// temporary directory removed afterwards, so the host's own ledger is never touched.
func Session(ctx context.Context, executable, socket string, env scope.Env) Bridge {
	scratch, err := os.MkdirTemp("", "crw-bridge-exercise-")
	if err != nil {
		return Bridge{Command: []string{executable}, Err: err}
	}
	defer os.RemoveAll(scratch)
	argv := []string{executable, "--state-dir", scratch}
	if socket != "" {
		argv = append(argv, "--socket", socket)
	}
	return Argv(ctx, argv, env)
}

// Argv runs the same session with a command line as given (a launcher, say), and reports the
// process's exit status and stderr when the session did not complete.
func Argv(ctx context.Context, argv []string, env scope.Env) Bridge {
	return ArgvIn(ctx, "", argv, env)
}

// ArgvIn is Argv started in dir, as a declared MCP server with a working directory is started.
func ArgvIn(ctx context.Context, dir string, argv []string, env scope.Env) Bridge {
	result := Bridge{Command: argv}
	run, cancel := context.WithTimeout(ctx, BridgeTimeout)
	defer cancel()
	cmd := exec.CommandContext(run, argv[0], argv[1:]...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var stderr strings.Builder
	cmd.Stderr = stderrOf(&stderr)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		result.Err = err
		return result
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		result.Err = err
		return result
	}
	// A descendant that inherited stdout keeps the pipe open after the bridge is killed, so a
	// read blocked on it would outlive the deadline. The session's own reader is closed the moment
	// the session's time is up, which ends that read whatever the command's other streams are.
	// WaitDelay then bounds Wait itself: os/exec closes the pipes it copies (stderr) that long
	// after the deadline or the exit, but it closes the stdout pipe only while such a copying
	// goroutine exists, so it is not what bounds the session.
	cmd.WaitDelay = WaitDelay
	if err := cmd.Start(); err != nil {
		result.Err = err
		return result
	}
	stopClosing := context.AfterFunc(run, func() { _ = stdout.Close() })
	defer stopClosing()
	session := &session{in: stdin, out: bufio.NewReaderSize(stdout, 1<<16)}
	result.Tools, result.Connection, result.Err = session.run()
	_ = stdin.Close()
	waited := waitBriefly(cmd, cancel)
	if result.Err != nil {
		var exit *exec.ExitError
		if errors.As(waited, &exit) {
			result.ExitCode = exit.ExitCode()
		}
		result.Stderr = stderr.String()
		if said := strings.TrimSpace(result.Stderr); said != "" {
			result.Err = fmt.Errorf("%w (stderr: %s)", result.Err, tail(said, 500))
		}
	}
	return result
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// waitBriefly lets the server exit on its closed stdin, and ends it when it does not.
func waitBriefly(cmd *exec.Cmd, cancel context.CancelFunc) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		cancel()
		return <-done
	}
}

// stderrOf is where the bridge's stderr goes: the first 4000 bytes, kept for the report. It is a
// variable only so that a test can send stderr nowhere, which leaves os/exec no copying goroutine
// and proves the session's deadline does not depend on one.
var stderrOf = func(into *strings.Builder) io.Writer { return &limited{w: into, left: 4000} }

type limited struct {
	w    io.Writer
	left int
}

func (l *limited) Write(p []byte) (int, error) {
	if l.left > 0 {
		n := min(len(p), l.left)
		_, _ = l.w.Write(p[:n])
		l.left -= n
	}
	return len(p), nil
}

type session struct {
	in  io.Writer
	out *bufio.Reader
	id  int
}

func (s *session) send(message map[string]any) error {
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	_, err = s.in.Write(append(raw, '\n'))
	return err
}

// call sends one request and reads lines until its response, skipping notifications and
// server requests; the result is decoded in the order the server wrote it.
func (s *session) call(method string, params any) (Object, error) {
	s.id++
	id := s.id
	if err := s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, fmt.Errorf("%s: the bridge stopped reading its input: %w", method, err)
	}
	for {
		line, err := s.out.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) == 0 {
			if err != nil {
				return nil, fmt.Errorf("%s: the bridge closed its output before answering", method)
			}
			continue
		}
		value, decodeErr := reading.Decode(line)
		message, ok := value.(Object)
		if decodeErr != nil || !ok {
			return nil, fmt.Errorf("%s: the bridge wrote a line that is not a JSON-RPC message", method)
		}
		answered, has := record.Lookup(message, "id")
		if !has || record.Get(message, "method") != nil || !sameID(answered, id) {
			continue
		}
		if failure, ok := record.Get(message, "error").(Object); ok {
			return nil, fmt.Errorf("%s: the bridge answered with an error: %s", method, pyjson.Dumps(failure, pyjson.Options{}))
		}
		result, ok := record.Get(message, "result").(Object)
		if !ok {
			return nil, fmt.Errorf("%s: the bridge's answer carries no result object", method)
		}
		return result, nil
	}
}

func sameID(v any, id int) bool {
	switch n := v.(type) {
	case int64:
		return n == int64(id)
	case float64:
		return n == float64(id)
	}
	return false
}

func (s *session) run() ([]string, any, error) {
	if _, err := s.call("initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "crw install", "version": "1"},
	}); err != nil {
		return nil, nil, err
	}
	if err := s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		return nil, nil, err
	}
	listed, err := s.call("tools/list", map[string]any{})
	if err != nil {
		return nil, nil, err
	}
	var tools []string
	entries, _ := record.Get(listed, "tools").([]any)
	for _, entry := range entries {
		if name, ok := record.Get(asObject(entry), "name").(string); ok {
			tools = append(tools, name)
		}
	}
	called, err := s.call("tools/call", map[string]any{"name": "get_capabilities", "arguments": map[string]any{}})
	if err != nil {
		return tools, nil, err
	}
	if pyvalue.Truthy(record.Get(called, "isError")) {
		return tools, nil, errors.New("get_capabilities answered with an error: " + pyjson.Dumps(record.Get(called, "content"), pyjson.Options{}))
	}
	connection := record.Get(called, "structuredContent")
	if connection == nil {
		return tools, nil, errors.New("get_capabilities answered without structured content")
	}
	return tools, connection, nil
}

func asObject(v any) Object {
	o, _ := v.(Object)
	return o
}
