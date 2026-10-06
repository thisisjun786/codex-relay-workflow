package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// hostReadAllowedMethods is every method crw manage host-read may read.
var hostReadAllowedMethods = []string{"thread/read", "thread/list", "thread/loaded/list", "thread/turns/list", "mcpServerStatus/list"}

// hostReadConfig is a Config whose relay socket is socket; no configuration file loader exists yet.
func hostReadConfig(socket string) *Config {
	return &Config{Relay: coreRelay{Socket: socket}, raw: map[string]json.RawMessage{}}
}

// hostReadEnv points HOME, CODEX_HOME and XDG_STATE_HOME at a fresh short tree and links the command's
// default socket path to socket (absent when socket is empty). Not t.TempDir(): its path carries the
// test name, which would push the socket past the 108-byte unix socket bound.
func hostReadEnv(t *testing.T, socket string) {
	t.Helper()
	home, err := os.MkdirTemp("", "crw686-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	control := filepath.Join(home, "codex", "app-server-control")
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("XDG_STATE_HOME", "")
	if socket != "" {
		if err := os.Symlink(socket, filepath.Join(control, "app-server-control.sock")); err != nil {
			t.Fatal(err)
		}
	}
}

func hostReadRun(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errOut strings.Builder
	return Run(context.Background(), args, strings.NewReader(""), &out, &errOut), out.String()
}

// hostReadOut is the JSON object crw manage host-read writes.
type hostReadOut struct {
	OK     bool            `json:"ok"`
	Method string          `json:"method"`
	Reason string          `json:"reason"`
	Detail string          `json:"detail"`
	Result json.RawMessage `json:"result"`
}

func hostReadDecode(t *testing.T, out string) hostReadOut {
	t.Helper()
	var got hostReadOut
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the output is not one JSON object: %v\n%s", err, out)
	}
	return got
}

// C1: every write method is refused as method_not_read_only exit 2 before any connection (a live fake
// records no request; with no socket the same refusal replaces host_unreachable). HostRead too.
func TestHostReadRefusesWriteMethods(t *testing.T) {
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{})
	for _, socket := range []string{host.SocketPath, ""} {
		hostReadEnv(t, socket)
		for _, method := range []string{"thread/resume", "turn/start", "turn/steer", "thread/archive", "thread/unarchive"} {
			code, out := hostReadRun(t, "host-read", "--method", method)
			got := hostReadDecode(t, out)
			if code != usageExit || got.OK || got.Reason != "method_not_read_only" || got.Method != method {
				t.Fatalf("socket %q, %s: exit %d, output %s", socket, method, code, out)
			}
		}
	}
	if _, err := HostRead(context.Background(), hostReadConfig(host.SocketPath), "thread/archive", nil); err == nil {
		t.Fatal("HostRead accepted thread/archive")
	}
	if requests := host.Requests(); len(requests) != 0 {
		t.Fatalf("a refused method reached the host: %+v", requests)
	}
}

// C2: every allow-listed method returns the fake's result bytes unchanged, --params reaches the host
// unchanged, an omitted --params sends the empty object, HostRead returns the host's bytes, and the
// envelope splices a result in rather than compacting it.
func TestHostReadReturnsAllowedResultsVerbatim(t *testing.T) {
	host := fakehost.Start(t)
	for _, method := range hostReadAllowedMethods {
		host.Respond(method, fakehost.Reply{Result: map[string]any{"marker": method, "n": float64(1)}})
	}
	hostReadEnv(t, host.SocketPath)
	for _, method := range hostReadAllowedMethods {
		code, out := hostReadRun(t, "host-read", "--method", method, "--params", `{"threadId":"t1","includeTurns":false}`)
		got := hostReadDecode(t, out)
		want := `{"marker":"` + method + `","n":1}`
		if code != 0 || !got.OK || got.Method != method || string(got.Result) != want {
			t.Fatalf("%s: exit %d, result %s, want %s", method, code, got.Result, want)
		}
	}
	if code, out := hostReadRun(t, "host-read", "--method", "thread/list"); code != 0 {
		t.Fatalf("thread/list without --params: exit %d, output %s", code, out)
	}
	params, empty := map[string]any{}, false
	for _, request := range host.Requests() {
		if request.Method == "thread/read" {
			if err := json.Unmarshal(request.Params, &params); err != nil {
				t.Fatal(err)
			}
		}
		empty = empty || (request.Method == "thread/list" && string(request.Params) == "{}")
	}
	if params["threadId"] != "t1" || params["includeTurns"] != false || !empty {
		t.Fatalf("params thread/read %v, thread/list sent {}: %v", params, empty)
	}
	raw, err := HostRead(context.Background(), hostReadConfig(host.SocketPath), "thread/read", nil)
	if err != nil || string(raw) != `{"marker":"thread/read","n":1}` {
		t.Fatalf("HostRead returned %s, %v", raw, err)
	}
	var envelope strings.Builder
	hostReadWriteResult(&envelope, "thread/read", json.RawMessage(`{ "a" : "<b>" }`))
	if want := `{"ok":true,"method":"thread/read","result":{ "a" : "<b>" }}` + "\n"; envelope.String() != want {
		t.Fatalf("the envelope changed the host's bytes: %q", envelope.String())
	}
}

// C4: with no socket the command exits 3 and reports host_unreachable; a host that answers with an
// error is host_error carrying the host's message, also exit 3; an unusable command line is exit 2.
func TestHostReadFailuresAndCommandLine(t *testing.T) {
	hostReadEnv(t, "")
	if code, out := hostReadRun(t, "host-read", "--method", "thread/read"); code != 3 || hostReadDecode(t, out).Reason != "host_unreachable" {
		t.Fatalf("no socket: exit %d, output %s", code, out)
	}
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Error: &fakehost.RPCError{Code: -32600, Message: "no such thread"}})
	hostReadEnv(t, host.SocketPath)
	code, out := hostReadRun(t, "host-read", "--method", "thread/read")
	if got := hostReadDecode(t, out); code != 3 || got.OK || got.Reason != "host_error" || !strings.Contains(got.Detail, "no such thread") {
		t.Fatalf("host error: exit %d, output %s", code, out)
	}
	hostReadEnv(t, "")
	for _, args := range [][]string{
		{"host-read"},
		{"host-read", "--method"},
		{"host-read", "--method", "thread/read", "--params", "{not json"},
		{"host-read", "--method", "thread/read", "--nope", "x"},
	} {
		if code, _ := hostReadRun(t, args...); code != usageExit {
			t.Errorf("%q: exit %d, want %d", args, code, usageExit)
		}
	}
	if code, out := hostReadRun(t, "host-read", "-h"); code != 0 || !strings.Contains(out, "usage: crw manage host-read") {
		t.Fatalf("-h: exit %d, output %q", code, out)
	}
}
