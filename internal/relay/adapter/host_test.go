package adapter

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The real binary's host-only commands must reach the host's socket, not an injected CLI
// replacement or a bridge subprocess.
func Test28_BuiltBinaryHostRoundTrips(t *testing.T) {
	root := t.TempDir()
	binary, alias := suiteBinary, suiteAlias
	host := fakehost.Start(t)
	for _, name := range []string{"deliver", "recover", "verify-acks"} {
		for _, program := range []string{binary, alias} {
			state := filepath.Join(root, name+filepath.Base(program))
			argv := []string{"--state", state, "--socket", host.SocketPath, name}
			goArgs := argv
			if program == binary {
				goArgs = append([]string{"relay"}, argv...)
			}
			goCmd := exec.Command(program, goArgs...)
			goOut, goErr := goCmd.CombinedOutput()
			if goErr != nil {
				t.Fatalf("%s: Go %v %s", name, goErr, goOut)
			}
			expectJSON(t, name+" "+filepath.Base(program), processExit{Stdout: string(goOut)}, golden.Substitute(host.SocketPath, "<host-socket>"))
		}
	}
	for _, args := range [][]string{{"deliver", "--event", "unknown"}, {"reconcile", "--request-id", "unknown"}, {"claim", "--event", "unknown"}, {"ack", "--event", "nope", "--ack-turn", "t", "--ack-proof", "p"}, {"ack", "--event", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--ack-turn", " ", "--ack-proof", "p"}} {
		for _, program := range []string{binary, alias} {
			state := filepath.Join(root, "refusal", args[0], filepath.Base(program), "go")
			argv := append([]string{"--state", state, "--socket", host.SocketPath}, args...)
			goArgs := argv
			if program == binary {
				goArgs = append([]string{"relay"}, argv...)
			}
			goOut, goErr := exec.Command(program, goArgs...).CombinedOutput()
			exitCode := func(err error) int {
				if e, ok := err.(*exec.ExitError); ok {
					return e.ExitCode()
				}
				if err == nil {
					return 0
				}
				t.Fatal(err)
				return -1
			}
			expectJSON(t, "refusal "+strings.Join(args, " ")+" "+filepath.Base(program), processExit{Code: exitCode(goErr), Stdout: string(goOut)}, golden.Substitute(host.SocketPath, "<host-socket>"))
		}
	}
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	host.Respond("thread/resume", fakehost.Reply{Result: resume()})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "socket-turn"}}})
	a, err := Open(host.SocketPath, filepath.Join(root, "socket-ledger"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	settings := &delivery.TaskSettings{Data: ordered(authorized()).(delivery.Obj)}
	receipt, err := a.SendMessage("real-socket-send", "thread-1", "hello", settings)
	if err != nil {
		t.Fatal(err)
	}
	if field(receipt, "status") != "accepted" || field(receipt, "turnId") != "socket-turn" {
		t.Fatalf("%s", dumps(receipt, false))
	}
	methods := []string{}
	for _, r := range host.Requests() {
		methods = append(methods, r.Method)
	}
	raw, _ := json.Marshal(methods)
	if string(raw) != `["initialize","initialized","thread/read","thread/resume","turn/start"]` {
		t.Fatalf("RPC sequence %s", raw)
	}
	if err := a.RequireLedger(context.Background(), a.identity); err != nil {
		t.Fatal(err)
	}
}

// deliver and verify-acks read --limit as the integer argparse parsed (a *big.Int), not only as
// their int64 default: a given limit is served, and one past what SQLite binds is the host
// error, exit 3, never a panic.
func TestHostCommandsReadAGivenLimit(t *testing.T) {
	root := t.TempDir()
	host := fakehost.Start(t)
	for _, c := range []struct {
		argv  []string
		exit  int
		field string
	}{
		{[]string{"deliver", "--limit", "2"}, 0, "attempts"},
		{[]string{"verify-acks", "--limit", "3"}, 0, ""},
		{[]string{"deliver", "--limit", "99999999999999999999"}, 3, "error"},
		{[]string{"verify-acks", "--limit", "-99999999999999999999"}, 3, "error"},
	} {
		state := filepath.Join(root, strings.Join(c.argv, "-"))
		out, err := exec.Command(suiteBinary, append([]string{"relay", "--state", state, "--socket", host.SocketPath}, c.argv...)...).CombinedOutput()
		code := 0
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		var answer map[string]any
		if code != c.exit || json.Unmarshal(out, &answer) != nil {
			t.Fatalf("%v: exit %d, want %d: %s", c.argv, code, c.exit, out)
		}
		if c.field != "" {
			if _, ok := answer[c.field]; !ok {
				t.Fatalf("%v: no %q in %s", c.argv, c.field, out)
			}
		}
		if c.exit == 3 && answer["error"] != "host" {
			t.Fatalf("%v: %s", c.argv, out)
		}
	}
}
