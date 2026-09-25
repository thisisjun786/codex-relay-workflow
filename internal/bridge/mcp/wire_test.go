package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"testing"
)

// wire drives Main over raw newline-delimited JSON so a test sees exactly the bytes a host
// receives, which a client library would decode and re-encode.
type wire struct {
	in    *io.PipeWriter
	lines *bufio.Scanner
	done  chan int
}

func startWire(t *testing.T) *wire {
	t.Helper()
	home, env := isolated(t)
	serverIn, in := io.Pipe()
	out, serverOut := io.Pipe()
	w := &wire{in: in, lines: bufio.NewScanner(out), done: make(chan int, 1)}
	w.lines.Buffer(nil, 16<<20)
	go func() {
		w.done <- Main(t.Context(), []string{"--socket", filepath.Join(home, "absent.sock"), "--state-dir", filepath.Join(home, "ledger")}, env, serverIn, serverOut, io.Discard)
		_ = serverOut.Close()
	}()
	t.Cleanup(func() { _ = in.Close(); <-w.done })
	w.send(t, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"wire","version":"0"}}}`)
	return w
}

func (w *wire) send(t *testing.T, line string) {
	t.Helper()
	if _, err := fmt.Fprintln(w.in, line); err != nil {
		t.Fatal(err)
	}
}

// next is the next frame on stdout, decoded only into raw members.
func (w *wire) next(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	if !w.lines.Scan() {
		t.Fatalf("stdout ended: %v", w.lines.Err())
	}
	var frame map[string]json.RawMessage
	if err := json.Unmarshal(w.lines.Bytes(), &frame); err != nil {
		t.Fatalf("non-JSON frame %q", w.lines.Text())
	}
	return frame
}

// Python's replies, captured from `python -m codex_thread_bridge.server` (mcp 1.30.0) on the
// same requests. Only the initialize result's fields outside capabilities are left out:
// serverInfo.version (0.1.0 here, the mcp library version there) is pending a decision.
func Test_protocol_replies_outside_tools_call_are_pythons_bytes(t *testing.T) {
	w := startWire(t)
	var initialize struct {
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(w.next(t)["result"], &initialize); err != nil {
		t.Fatal(err)
	}
	if got, want := string(initialize.Capabilities), `{"experimental":{},"prompts":{"listChanged":false},"resources":{"subscribe":false,"listChanged":false},"tools":{"listChanged":false}}`; got != want {
		t.Errorf("initialize capabilities\n got %s\nwant %s", got, want)
	}
	w.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	for _, tc := range []struct{ request, member, want string }{
		{`{"jsonrpc":"2.0","id":2,"method":"prompts/list"}`, "result", `{"prompts":[]}`},
		{`{"jsonrpc":"2.0","id":3,"method":"resources/list"}`, "result", `{"resources":[]}`},
		{`{"jsonrpc":"2.0","id":4,"method":"resources/templates/list"}`, "result", `{"resourceTemplates":[]}`},
		{`{"jsonrpc":"2.0","id":5,"method":"logging/setLevel","params":{"level":"info"}}`, "error", `{"code":-32601,"message":"Method not found"}`},
		{`{"jsonrpc":"2.0","id":6,"method":"completion/complete","params":{"ref":{"type":"ref/prompt","name":"x"},"argument":{"name":"a","value":"b"}}}`, "error", `{"code":-32601,"message":"Method not found"}`},
		{`{"jsonrpc":"2.0","id":7,"method":"prompts/get","params":{"name":"x"}}`, "error", `{"code":0,"message":"Unknown prompt: x"}`},
		{`{"jsonrpc":"2.0","id":8,"method":"resources/read","params":{"uri":"file:///nope"}}`, "error", `{"code":0,"message":"Unknown resource: file:///nope"}`},
	} {
		w.send(t, tc.request)
		frame := w.next(t)
		if got := string(frame[tc.member]); got != tc.want {
			t.Errorf("%s\n got %s: %s\nwant %s: %s", tc.request, tc.member, frame[tc.member], tc.member, tc.want)
		}
	}
	// tools/list carries its tools and nothing else at the top level.
	w.send(t, `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`)
	var listed map[string]json.RawMessage
	if err := json.Unmarshal(w.next(t)["result"], &listed); err != nil {
		t.Fatal(err)
	}
	if _, ok := listed["tools"]; !ok || len(listed) != 1 {
		keys := []string{}
		for key := range listed {
			keys = append(keys, key)
		}
		t.Errorf("tools/list result members %v, want only tools", keys)
	}
}

// An unknown method, answered as `python -m codex_thread_bridge.server` (mcp 1.30.0) answers
// it, bytes captured from its stdout. The low-level server validates every request against the
// ClientRequest union first, so a request (an id is present) naming any method outside it --
// unknown, server-to-client, or a notification's method -- fails that validation, while an
// unknown notification is dropped without a reply.
func Test_an_unknown_method_is_answered_as_python_answers_it(t *testing.T) {
	w := startWire(t)
	w.next(t) // initialize
	w.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	for _, tc := range []struct{ request, want string }{
		{`{"jsonrpc":"2.0","id":2,"method":"no/such"}`, `{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"Invalid request parameters","data":""}}`},
		{`{"jsonrpc":"2.0","id":"s-3","method":"no/such","params":{"a":1}}`, `{"jsonrpc":"2.0","id":"s-3","error":{"code":-32602,"message":"Invalid request parameters","data":""}}`},
		{`{"jsonrpc":"2.0","id":10,"method":"no/such","params":null}`, `{"jsonrpc":"2.0","id":10,"error":{"code":-32602,"message":"Invalid request parameters","data":""}}`},
		{`{"jsonrpc":"2.0","id":12,"method":"server/discover"}`, `{"jsonrpc":"2.0","id":12,"error":{"code":-32602,"message":"Invalid request parameters","data":""}}`},
		{`{"jsonrpc":"2.0","id":13,"method":"subscriptions/listen","params":{}}`, `{"jsonrpc":"2.0","id":13,"error":{"code":-32602,"message":"Invalid request parameters","data":""}}`},
		{`{"jsonrpc":"2.0","id":17,"method":"sampling/createMessage","params":{}}`, `{"jsonrpc":"2.0","id":17,"error":{"code":-32602,"message":"Invalid request parameters","data":""}}`},
		{`{"jsonrpc":"2.0","id":18,"method":"notifications/initialized"}`, `{"jsonrpc":"2.0","id":18,"error":{"code":-32602,"message":"Invalid request parameters","data":""}}`},
		// A known method with no handler keeps the bare "Method not found".
		{`{"jsonrpc":"2.0","id":14,"method":"tasks/get","params":{"taskId":"x"}}`, `{"jsonrpc":"2.0","id":14,"error":{"code":-32601,"message":"Method not found"}}`},
	} {
		w.send(t, tc.request)
		if got := w.lines.Scan() && w.lines.Text() == tc.want; !got {
			t.Errorf("%s\n got %s\nwant %s", tc.request, w.lines.Text(), tc.want)
		}
	}
	// An unknown notification gets nothing: the next frame is the ping's answer.
	w.send(t, `{"jsonrpc":"2.0","method":"no/such"}`)
	w.send(t, `{"jsonrpc":"2.0","method":"notifications/no-such","params":{"a":1}}`)
	w.send(t, `{"jsonrpc":"2.0","id":20,"method":"ping"}`)
	if frame := w.next(t); string(frame["id"]) != "20" {
		t.Errorf("an unknown notification was answered: %v", frame)
	}
	// An unknown request whose params are not an object gets no response, only the log
	// notification Python's exception handler sends (JSON-equal; Python writes "jsonrpc" last).
	for _, request := range []string{`{"jsonrpc":"2.0","id":15,"method":"no/such","params":[]}`, `{"jsonrpc":"2.0","id":11,"method":"no/such","params":"x"}`} {
		w.send(t, request)
		frame := w.next(t)
		want := map[string]json.RawMessage{"jsonrpc": json.RawMessage(`"2.0"`), "method": json.RawMessage(`"notifications/message"`), "params": json.RawMessage(`{"level":"error","logger":"mcp.server.exception_handler","data":"Internal Server Error"}`)}
		if len(frame) != len(want) || string(frame["jsonrpc"]) != string(want["jsonrpc"]) || string(frame["method"]) != string(want["method"]) || string(frame["params"]) != string(want["params"]) {
			t.Errorf("%s\n got %v", request, frame)
		}
	}
	w.send(t, `{"jsonrpc":"2.0","id":21,"method":"ping"}`)
	if frame := w.next(t); string(frame["id"]) != "21" {
		t.Errorf("a response followed the log notification: %v", frame)
	}
}
