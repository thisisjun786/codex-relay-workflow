//go:build dev

package laneparity

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// post sends a Responses request to the stub and returns the decoded events of the stream.
func post(t *testing.T, p *StubProvider, path, body string) (int, []map[string]any) {
	t.Helper()
	resp, err := http.Post(strings.TrimSuffix(p.URL(), "/v1")+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var events []map[string]any
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			var ev map[string]any
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				t.Fatalf("event %q: %v", data, err)
			}
			events = append(events, ev)
		}
	}
	return resp.StatusCode, events
}

// The stub answers a request with the stream a host expects: created, one finished output item (a
// tool call or a message), completed with the usage; the script sees what the host sent.
func TestStubProvider_servesAScriptedTurn(t *testing.T) {
	p, err := StartStubProvider(func(r StubRequest) StubReply {
		switch {
		case r.Compaction:
			return StubReply{Text: "summary"}
		case r.Outputs == 0:
			return StubReply{Call: &StubCall{Name: "exec_command", Arguments: `{"cmd":"true"}`}, Tokens: 900}
		case r.Outputs == 1:
			return StubReply{Call: &StubCall{Namespace: "multi_agent_v1", Name: "spawn_agent", Arguments: `{}`}}
		}
		return StubReply{Text: "done"}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	code, ev := post(t, p, "/v1/responses", `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}],`+
		`"tools":[{"type":"function","name":"exec_command"},{"type":"namespace","name":"multi_agent_v1","tools":[{"name":"spawn_agent"}]}]}`)
	if code != 200 || len(ev) != 3 || ev[0]["type"] != "response.created" || ev[2]["type"] != "response.completed" {
		t.Fatalf("%d %+v", code, ev)
	}
	item := ev[1]["item"].(map[string]any)
	if item["type"] != "function_call" || item["name"] != "exec_command" || item["arguments"] != `{"cmd":"true"}` || item["call_id"] == "" {
		t.Errorf("item %+v", item)
	}
	if usage := ev[2]["response"].(map[string]any)["usage"].(map[string]any); usage["total_tokens"] != float64(900) {
		t.Errorf("usage %+v", usage)
	}

	_, ev = post(t, p, "/v1/responses", `{"input":[{"type":"function_call_output","call_id":"c","output":"approval policy is Never"}]}`)
	if item := ev[1]["item"].(map[string]any); item["namespace"] != "multi_agent_v1" || item["name"] != "spawn_agent" {
		t.Errorf("namespaced call %+v", item)
	}
	_, ev = post(t, p, "/v1/responses", `{"input":[{"type":"function_call_output"},{"type":"function_call_output"}]}`)
	if item := ev[1]["item"].(map[string]any); item["type"] != "message" || item["role"] != "assistant" {
		t.Errorf("message %+v", item)
	}
	_, ev = post(t, p, "/v1/responses", `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"You are performing a `+CompactionMarker+`. Summarise."}]}]}`)
	if item := ev[1]["item"].(map[string]any); item["type"] != "message" {
		t.Errorf("compaction %+v", item)
	}

	got := p.Requests()
	if len(got[1].ToolOutputs) != 1 || got[1].ToolOutputs[0] != "approval policy is Never" {
		t.Errorf("tool outputs %q", got[1].ToolOutputs)
	}
	if len(got) != 4 || got[0].N != 1 || got[0].Outputs != 0 || len(got[0].Texts) != 1 || got[0].Texts[0] != "go" ||
		strings.Join(got[0].Tools, ",") != "exec_command,multi_agent_v1.spawn_agent" || got[1].Outputs != 1 || got[2].Outputs != 2 || !got[3].Compaction || got[0].Compaction {
		t.Errorf("requests %+v", got)
	}
}

// Nothing but the Responses endpoint is served: a host that asks for a model list or posts elsewhere
// gets a 404 and the script is not consulted.
func TestStubProvider_servesNothingElse(t *testing.T) {
	calls := 0
	p, err := StartStubProvider(func(StubRequest) StubReply { calls++; return StubReply{} })
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, path := range []string{"/v1/models", "/v1/chat/completions", "/"} {
		if code, _ := post(t, p, path, `{}`); code != 404 {
			t.Errorf("%s: %d", path, code)
		}
	}
	resp, err := http.Get(p.URL() + "/responses")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 || calls != 0 || len(p.Requests()) != 0 {
		t.Errorf("GET %d, %d script calls, %d requests", resp.StatusCode, calls, len(p.Requests()))
	}
	if !strings.HasPrefix(p.URL(), "http://127.0.0.1:") {
		t.Errorf("the provider is not on the loopback interface: %s", p.URL())
	}
}
