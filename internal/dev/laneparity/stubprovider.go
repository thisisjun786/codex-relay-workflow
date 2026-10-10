//go:build dev

package laneparity

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// StubProvider is a model provider that serves the OpenAI Responses API from the loopback interface
// with a script instead of a model (CRW-1082): a real Codex pointed at it with model_provider in its
// config.toml runs a whole turn, calls the tools the script names, compacts when the usage it reports
// says so, and needs no credential. It answers POST /v1/responses with a server-sent stream and
// everything else with 404. The requests it served are kept for the report.
type StubProvider struct {
	ln     net.Listener
	srv    *http.Server
	script func(StubRequest) StubReply

	mu   sync.Mutex
	seen []StubRequest
}

// StubRequest is one model request the host sent, as far as a script needs it.
type StubRequest struct {
	N       int // 1 for the first request of the provider
	Path    string
	Outputs int      // function_call_output items in the input: the tool calls the host has answered
	Texts   []string // the text of every message in the input, in order
	// ToolOutputs are the outputs of the tool calls the host has answered, in order.
	ToolOutputs []string
	Tools       []string // the names of the tools the request offers (namespaced tools as namespace.name)
	// Compaction is whether the request asks for a context checkpoint compaction (the host compacts by
	// asking the model for a summary).
	Compaction bool
}

// StubCall is a tool call the model asks for.
type StubCall struct {
	Namespace string // for a tool of a namespace (multi_agent_v1); empty otherwise
	Name      string
	Arguments string // the JSON text of the arguments
}

// StubReply is what a script answers one request with: a tool call, else a message. Tokens is the
// total usage the response reports, which the host counts against its auto-compaction limit.
type StubReply struct {
	Call   *StubCall
	Text   string
	Tokens int
}

// CompactionMarker is the text the host's compaction prompt starts with.
const CompactionMarker = "CONTEXT CHECKPOINT COMPACTION"

// StartStubProvider serves script on a free loopback port.
func StartStubProvider(script func(StubRequest) StubReply) (*StubProvider, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &StubProvider{ln: ln, script: script}
	p.srv = &http.Server{Handler: http.HandlerFunc(p.handle)}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// URL is the provider's base_url.
func (p *StubProvider) URL() string { return "http://" + p.ln.Addr().String() + "/v1" }

// Close stops the server.
func (p *StubProvider) Close() { _ = p.srv.Close() }

// Requests are the model requests served so far.
func (p *StubProvider) Requests() []StubRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]StubRequest(nil), p.seen...)
}

func (p *StubProvider) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil || r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
		http.NotFound(w, r)
		return
	}
	req := parseStubRequest(r.URL.Path, body)
	p.mu.Lock()
	req.N = len(p.seen) + 1
	p.seen = append(p.seen, req)
	p.mu.Unlock()
	reply := p.script(req)

	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	send := func(event string, v any) {
		raw, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
		if flusher != nil {
			flusher.Flush()
		}
	}
	id := fmt.Sprintf("resp_stub_%d", req.N)
	send("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id}})
	var item map[string]any
	if reply.Call != nil {
		item = map[string]any{"type": "function_call", "id": "fc_" + id, "call_id": "call_" + id, "name": reply.Call.Name, "arguments": reply.Call.Arguments}
		if reply.Call.Namespace != "" {
			item["namespace"] = reply.Call.Namespace
		}
	} else {
		item = map[string]any{"type": "message", "role": "assistant", "id": "msg_" + id,
			"content": []any{map[string]any{"type": "output_text", "text": reply.Text}}}
	}
	send("response.output_item.done", map[string]any{"type": "response.output_item.done", "item": item})
	tokens := reply.Tokens
	if tokens == 0 {
		tokens = 2
	}
	send("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "usage": map[string]any{
		"input_tokens": tokens - 1, "input_tokens_details": nil, "output_tokens": 1, "output_tokens_details": nil, "total_tokens": tokens}}})
}

func parseStubRequest(path string, body []byte) StubRequest {
	req := StubRequest{Path: path}
	var doc struct {
		Input []struct {
			Type    string `json:"type"`
			Output  any    `json:"output"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
		Tools []struct {
			Name  string `json:"name"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"tools"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return req
	}
	for _, item := range doc.Input {
		if item.Type == "function_call_output" {
			req.Outputs++
			if text, ok := item.Output.(string); ok {
				req.ToolOutputs = append(req.ToolOutputs, text)
			}
		}
		for _, c := range item.Content {
			if c.Text == "" {
				continue
			}
			req.Texts = append(req.Texts, c.Text)
			if strings.Contains(c.Text, CompactionMarker) {
				req.Compaction = true
			}
		}
	}
	for _, t := range doc.Tools {
		if len(t.Tools) == 0 {
			req.Tools = append(req.Tools, t.Name)
		}
		for _, n := range t.Tools {
			req.Tools = append(req.Tools, t.Name+"."+n.Name)
		}
	}
	return req
}
