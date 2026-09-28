package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// The real binary's host-only commands must reach the same socket as Python, not
// an injected CLI replacement or a bridge subprocess.
func Test28_BuiltBinaryHostRoundTrips(t *testing.T) {
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
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
			py := exec.Command("uv", append([]string{"run", "--no-sync", "python", "-m", "codex_session_relay.cli"}, argv...)...)
			py.Dir = repo
			pyOut, pyErr := py.CombinedOutput()
			if goErr != nil || pyErr != nil || !bytes.Equal(goOut, pyOut) {
				t.Fatalf("%s: Go %v %s; Python %v %s", name, goErr, goOut, pyErr, pyOut)
			}
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
			pyArgv := append([]string{}, argv...)
			pyArgv[1] = filepath.Join(filepath.Dir(state), "python")
			py := exec.Command("uv", append([]string{"run", "--no-sync", "python", "-m", "codex_session_relay.cli"}, pyArgv...)...)
			py.Dir = repo
			pyOut, pyErr := py.CombinedOutput()
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
			if exitCode(goErr) != exitCode(pyErr) || !bytes.Equal(goOut, pyOut) {
				t.Fatalf("refusal %v: Go %v %s Python %v %s", args, goErr, goOut, pyErr, pyOut)
			}
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
