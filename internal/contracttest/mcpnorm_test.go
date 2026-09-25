package contracttest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func frozenTools(t *testing.T) []byte {
	t.Helper()
	root, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "contract", "schema", "bridge-mcp-tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// liveTools starts the built crw bridge against a socket that does not exist, speaks MCP to it
// over raw stdio lines, and returns the tools/list result exactly as it arrived on stdout. A
// client library is deliberately not used: it would re-encode the tools it decoded.
func liveTools(t *testing.T) []byte {
	t.Helper()
	session := startStdio(t, nil)
	session.call(t, 1, "initialize", `{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"mcpnorm-test","version":"0"}}`)
	session.notify(t, "notifications/initialized")
	result := session.call(t, 2, "tools/list", `{}`)
	session.close(t)
	return result
}

// stdioSession is one `crw bridge` process driven line by line. Every stdout line it reads
// must be a JSON-RPC frame: stdout belongs to the protocol.
type stdioSession struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  *bufio.Scanner
	stderr *bytes.Buffer
}

func startStdio(t *testing.T, extraArgs []string) *stdioSession {
	t.Helper()
	binary, err := crwBinary()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	args := append([]string{"bridge", "--socket", filepath.Join(home, "absent.sock"), "--state-dir", filepath.Join(home, "state")}, extraArgs...)
	cmd := exec.Command(binary, args...)
	cmd.Env = isolatedEnv(home)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	session := &stdioSession{cmd: cmd, stdin: stdin, lines: bufio.NewScanner(stdout), stderr: &bytes.Buffer{}}
	session.lines.Buffer(nil, 16<<20)
	cmd.Stderr = session.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return session
}

func (s *stdioSession) notify(t *testing.T, method string) {
	t.Helper()
	if _, err := fmt.Fprintf(s.stdin, `{"jsonrpc":"2.0","method":%q}`+"\n", method); err != nil {
		t.Fatal(err)
	}
}

// call sends one request and returns its result, reading (and checking) every frame before it.
// The read is bounded by the process: a server that stops answering exits or is killed by the
// test's own deadline.
func (s *stdioSession) call(t *testing.T, id int, method, params string) json.RawMessage {
	t.Helper()
	if _, err := fmt.Fprintf(s.stdin, `{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`+"\n", id, method, params); err != nil {
		t.Fatal(err)
	}
	for s.lines.Scan() {
		var frame struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int            `json:"id"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(s.lines.Bytes(), &frame); err != nil || frame.JSONRPC != "2.0" {
			t.Fatalf("stdout carried a non-MCP line %q", s.lines.Text())
		}
		if frame.ID != nil && *frame.ID == id {
			if frame.Error != nil {
				t.Fatalf("%s: %s", method, frame.Error)
			}
			return frame.Result
		}
	}
	t.Fatalf("%s: stdout ended before the answer (%v; stderr %s)", method, s.lines.Err(), s.stderr.String())
	return nil
}

// close ends the session by closing stdin, and fails on anything else written to stdout.
func (s *stdioSession) close(t *testing.T) {
	t.Helper()
	if err := s.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	for s.lines.Scan() {
		t.Fatalf("stdout carried %q after the last answer", s.lines.Text())
	}
	if err := s.cmd.Wait(); err != nil {
		t.Fatalf("crw bridge exited with %v (stderr %s)", err, s.stderr.String())
	}
}

func Test_a_live_tools_list_from_the_built_binary_equals_the_frozen_contract(t *testing.T) {
	live := liveTools(t)
	if err := EqualMCPTools(frozenTools(t), live); err != nil {
		t.Fatal(err)
	}
	// Stronger than the contract requires today: with nothing normalised at all, the decoded
	// live result is the decoded frozen file, member for member.
	var want, got any
	if err := json.Unmarshal(frozenTools(t), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(live, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("the live tools/list result differs from contract/schema/bridge-mcp-tools.json before normalisation:\n%s", live)
	}
}

func Test_the_normaliser_forgives_only_schema_key_order_and_default_additional_properties(t *testing.T) {
	// Given the frozen listing rewritten with every forgiven difference
	var document map[string]any
	if err := json.Unmarshal(frozenTools(t), &document); err != nil {
		t.Fatal(err)
	}
	for _, tool := range document["tools"].([]any) {
		input := asObject(asObject(tool)["inputSchema"])
		input["$schema"] = "https://json-schema.org/draft/2020-12/schema"
		input["additionalProperties"] = true
		delete(asObject(asObject(tool)["outputSchema"]), "additionalProperties")
	}
	// Re-encoding sorts every key, so key order differs from the file too.
	rewritten, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	// Then the two still compare equal
	if err := EqualMCPTools(frozenTools(t), rewritten); err != nil {
		t.Fatal(err)
	}
}

func Test_the_normaliser_fails_every_semantic_difference(t *testing.T) {
	property := func(document map[string]any, tool, name string) map[string]any {
		for _, entry := range document["tools"].([]any) {
			if asObject(entry)["name"] == tool {
				return asObject(asObject(asObject(asObject(entry)["inputSchema"])["properties"])[name])
			}
		}
		t.Fatalf("no %s.%s", tool, name)
		return nil
	}
	schemaOf := func(document map[string]any, tool string) map[string]any {
		for _, entry := range document["tools"].([]any) {
			if asObject(entry)["name"] == tool {
				return asObject(asObject(entry)["inputSchema"])
			}
		}
		t.Fatalf("no %s", tool)
		return nil
	}
	mutations := map[string]func(map[string]any){
		"type":           func(d map[string]any) { property(d, "read_thread", "limit")["type"] = "number" },
		"default":        func(d map[string]any) { property(d, "list_threads", "limit")["default"] = float64(21) },
		"cursor default": func(d map[string]any) { property(d, "list_threads", "cursor")["default"] = nil },
		"enum value": func(d map[string]any) {
			property(d, "create_thread", "sandbox")["enum"] = []any{"read-only", "workspace-write"}
		},
		"const": func(d map[string]any) {
			property(d, "create_worktree_thread", "worktree_mode")["const"] = "desktop-managed"
		},
		"required entry dropped": func(d map[string]any) { schemaOf(d, "create_thread")["required"] = []any{"request_id", "cwd", "model"} },
		"required entry added":   func(d map[string]any) { schemaOf(d, "list_threads")["required"] = []any{"cwd"} },
		"nullable made plain":    func(d map[string]any) { delete(property(d, "create_thread", "prompt"), "anyOf") },
		"closed object":          func(d map[string]any) { schemaOf(d, "get_goal")["additionalProperties"] = false },
		"property renamed": func(d map[string]any) {
			props := asObject(schemaOf(d, "get_goal")["properties"])
			props["threadId"] = props["thread_id"]
			delete(props, "thread_id")
		},
		"annotation": func(d map[string]any) {
			asObject(asObject(d["tools"].([]any)[0])["annotations"])["readOnlyHint"] = false
		},
		"tool order":  func(d map[string]any) { tools := d["tools"].([]any); tools[0], tools[1] = tools[1], tools[0] },
		"description": func(d map[string]any) { asObject(d["tools"].([]any)[2])["description"] = "changed" },
		"additionalProperties false item": func(d map[string]any) {
			asObject(property(d, "create_thread", "expected_sandbox_policy")["anyOf"].([]any)[0])["additionalProperties"] = false
		},
		"extra top-level member":      func(d map[string]any) { d["ttlMs"] = float64(0) },
		"extra top-level cache scope": func(d map[string]any) { d["cacheScope"] = "public" },
		"extra member on a tool":      func(d map[string]any) { asObject(d["tools"].([]any)[0])["title"] = "Capabilities" },
		"extra annotation": func(d map[string]any) {
			asObject(asObject(d["tools"].([]any)[0])["annotations"])["idempotentHint"] = false
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			var document map[string]any
			if err := json.Unmarshal(frozenTools(t), &document); err != nil {
				t.Fatal(err)
			}
			mutate(document)
			changed, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if err := EqualMCPTools(frozenTools(t), changed); err == nil || !strings.HasPrefix(err.Error(), "mcpnorm: ") {
				t.Fatalf("a %s change was accepted: %v", name, err)
			}
		})
	}
}
