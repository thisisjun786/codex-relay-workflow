package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
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
	// Closing stdin ends Main once it has written what it owes; draining stdout lets it.
	t.Cleanup(func() { _ = in.Close(); go func() { _, _ = io.Copy(io.Discard, out) }(); <-w.done })
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

// The protocol outside tools/call is the SDK's: a request for a method the server has no handler
// for is answered with an error under its id, a notification nobody handles gets nothing, and the
// server answers the next request.
func Test_a_method_without_a_handler_is_answered_and_the_session_goes_on(t *testing.T) {
	w := startWire(t)
	w.next(t) // initialize
	w.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	for _, id := range []string{"2", `"s-3"`} {
		w.send(t, `{"jsonrpc":"2.0","id":`+id+`,"method":"no/such","params":{}}`)
		frame := w.next(t)
		if string(frame["id"]) != id || frame["error"] == nil {
			t.Errorf("no/such under id %s: %v", id, frame)
		}
	}
	w.send(t, `{"jsonrpc":"2.0","method":"notifications/no-such","params":{"a":1}}`)
	w.send(t, `{"jsonrpc":"2.0","id":20,"method":"ping"}`)
	if frame := w.next(t); string(frame["id"]) != "20" || frame["result"] == nil {
		t.Errorf("an unknown notification was answered, or the ping was not: %v", frame)
	}
}

// tools/list is the frozen listing: "tools" its only member, the tools in registration order,
// no idempotentHint in their annotations, and '<', '>' and '&' written as themselves. The reply
// is the golden, JSON-equal (the SDK decides member order inside each tool).
func Test_tools_list_is_the_frozen_listing(t *testing.T) {
	w := startWire(t)
	w.next(t) // initialize
	w.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	w.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if !w.lines.Scan() {
		t.Fatalf("stdout ended: %v", w.lines.Err())
	}
	got := w.lines.Text()
	var frame struct {
		Result map[string][]map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(got), &frame); err != nil || len(frame.Result) != 1 {
		t.Fatalf("tools/list result %s: %v", got, err)
	}
	var names []any
	for _, tool := range frame.Result["tools"] {
		names = append(names, tool["name"])
		if annotations, _ := tool["annotations"].(map[string]any); annotations["idempotentHint"] != nil {
			t.Errorf("%s carries idempotentHint", tool["name"])
		}
	}
	if fmt.Sprint(names) != fmt.Sprint(order) {
		t.Errorf("tools listed as %v, registered as %v", names, order)
	}
	for _, escape := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if strings.Contains(got, escape) {
			t.Errorf("tools/list writes %s for the character itself", escape)
		}
	}
	var decodedGot, decodedWant any
	if err := json.Unmarshal([]byte(got), &decodedGot); err != nil {
		t.Fatal(err)
	}
	want := golden.Want(t, "tools-list", func() []byte { return []byte(got) })
	if err := json.Unmarshal(want, &decodedWant); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decodedGot, decodedWant) {
		t.Errorf("tools/list differs from the golden\n got %s\nwant %s", got, want)
	}
}
