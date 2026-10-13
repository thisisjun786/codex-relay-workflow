package sessionhost_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/sessionhost"
)

// ParseMarker reads the operation a session step names on the first line of its message, `crw-flow-op: <call>/t<k>-a<n>`,
// and the App Server backend's `<call>/t<k>`; anything else is no marker.
func TestParseMarker(t *testing.T) {
	cases := []struct {
		text string
		want sessionhost.Marker
		ok   bool
	}{
		{"crw-flow-op: c_9e9b06649478/t1-a1\nCRW-ROLE: explorer\n\nbody", sessionhost.Marker{Call: "c_9e9b06649478", K: 1, Attempt: 1}, true},
		{"crw-flow-op: c_abc/t12-a3", sessionhost.Marker{Call: "c_abc", K: 12, Attempt: 3}, true},
		{"crw-flow-op: c_abc/t2\n\ninput", sessionhost.Marker{Call: "c_abc", K: 2}, true},
		{"crw-flow-op: c_abc/t2-a1\r\nnext", sessionhost.Marker{Call: "c_abc", K: 2, Attempt: 1}, true},
		{"crw-flow-op: c_abc/t2-a1  \nnext", sessionhost.Marker{Call: "c_abc", K: 2, Attempt: 1}, true},
		{"crw-flow-op: c_abc/t0-a1", sessionhost.Marker{}, false},
		{"crw-flow-op: c_abc/t1-a0", sessionhost.Marker{}, false},
		{"crw-flow-op: c_abc/t01-a1", sessionhost.Marker{}, false},
		{"crw-flow-op: c_abc/t1-a01", sessionhost.Marker{}, false},
		{"crw-flow-op: c_abc/t1-a", sessionhost.Marker{}, false},
		{"crw-flow-op: c_abc/t1-b2", sessionhost.Marker{}, false},
		{"crw-flow-op: c_abc/t1/x", sessionhost.Marker{}, false},
		{"crw-flow-op: /t1-a1", sessionhost.Marker{}, false},
		{"crw-flow-op: c abc/t1-a1", sessionhost.Marker{}, false},
		{"crw-flow-op:c_abc/t1-a1", sessionhost.Marker{}, false},
		{"CRW-ROLE: explorer\ncrw-flow-op: c_abc/t1-a1", sessionhost.Marker{}, false},
		{" crw-flow-op: c_abc/t1-a1", sessionhost.Marker{}, false},
		{"", sessionhost.Marker{}, false},
		{"TASK: Read-only design audit", sessionhost.Marker{}, false},
		{"crw-flow-op: " + strings.Repeat("c", 65) + "/t1-a1", sessionhost.Marker{}, false},
	}
	for _, c := range cases {
		got, ok := sessionhost.ParseMarker(c.text)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseMarker(%q) = %+v %v, want %+v %v", c.text, got, ok, c.want, c.ok)
		}
	}
	m := sessionhost.Marker{Call: "c_abc", K: 3, Attempt: 2}
	if m.Op() != "c_abc/t3-a2" || (sessionhost.Marker{Call: "c_abc", K: 3}).Op() != "c_abc/t3" {
		t.Errorf("Op: %q %q", m.Op(), sessionhost.Marker{Call: "c_abc", K: 3}.Op())
	}
	if got, ok := sessionhost.ParseOp("c_abc/t3-a2"); !ok || got != m {
		t.Errorf("ParseOp: %+v %v", got, ok)
	}
	if _, ok := sessionhost.ParseOp("crw-flow-op: c_abc/t3-a2"); ok {
		t.Errorf("ParseOp took a whole marker line")
	}
}

func items(raw ...string) []json.RawMessage {
	out := make([]json.RawMessage, len(raw))
	for i, r := range raw {
		out[i] = json.RawMessage(r)
	}
	return out
}

// FinalAnswer is the last final_answer agent message of a turn; from a provider that sets no phase it is the last message
// without one; a commentary message is never the answer.
func TestFinalAnswer(t *testing.T) {
	user := `{"type":"userMessage","id":"u","content":[{"type":"text","text":"q"}]}`
	cases := []struct {
		name        string
		items       []json.RawMessage
		text, phase string
		ok          bool
		backendText string
	}{
		{"final answer", items(user, `{"type":"agentMessage","id":"1","text":"looking","phase":"commentary"}`, `{"type":"agentMessage","id":"2","text":"done","phase":"final_answer"}`), "done", "final_answer", true, "done"},
		{"the last of two final answers", items(`{"type":"agentMessage","id":"1","text":"a","phase":"final_answer"}`, `{"type":"agentMessage","id":"2","text":"b","phase":"final_answer"}`), "b", "final_answer", true, "b"},
		{"a final answer before a later commentary", items(`{"type":"agentMessage","id":"1","text":"a","phase":"final_answer"}`, `{"type":"agentMessage","id":"2","text":"c","phase":"commentary"}`), "a", "final_answer", true, "a"},
		{"a final answer before a later null phase", items(`{"type":"agentMessage","id":"1","text":"a","phase":"final_answer"}`, `{"type":"agentMessage","id":"2","text":"n","phase":null}`), "a", "final_answer", true, "a"},
		{"null phase", items(user, `{"type":"agentMessage","id":"1","text":"x","phase":null}`, `{"type":"agentMessage","id":"2","text":"y"}`), "y", "", true, "y"},
		{"null phase before commentary", items(`{"type":"agentMessage","id":"1","text":"x","phase":null}`, `{"type":"agentMessage","id":"2","text":"c","phase":"commentary"}`), "x", "", true, "c"},
		{"commentary only", items(user, `{"type":"agentMessage","id":"1","text":"c","phase":"commentary"}`), "", "", false, "c"},
		{"no message", items(user, `{"type":"reasoning","id":"r"}`), "", "", false, ""},
		{"undecodable items", items(`7`, `{"type":"agentMessage","id":"1","text":"z","phase":"final_answer"}`), "z", "final_answer", true, "z"},
	}
	for _, c := range cases {
		text, phase, ok := sessionhost.FinalAnswer(c.items)
		if text != c.text || phase != c.phase || ok != c.ok {
			t.Errorf("%s: FinalAnswer = %q %q %v, want %q %q %v", c.name, text, phase, ok, c.text, c.phase, c.ok)
		}
		// the App Server backend's own pick is unchanged: the last final answer, else the last message
		if got := sessionhost.PickMessage(sessionhost.ItemMessages(c.items)); got != c.backendText {
			t.Errorf("%s: PickMessage = %q, want %q", c.name, got, c.backendText)
		}
	}
}

// CollabCalls decodes the collabAgentToolCall items of a turn (the shape ops/native.go reads), with the prompt.
func TestCollabCalls(t *testing.T) {
	got := sessionhost.CollabCalls(items(
		`{"type":"userMessage","id":"u","content":[]}`,
		`{"type":"collabAgentToolCall","id":"exec-1","tool":"spawnAgent","status":"completed","senderThreadId":"o","receiverThreadIds":["c1"],"prompt":"crw-flow-op: c_a/t1-a1","model":"m","reasoningEffort":"high","agentsStates":{}}`,
		`{"type":"collabAgentToolCall","id":"exec-2","tool":"wait","status":"completed","senderThreadId":"o","receiverThreadIds":["c1"],"agentsStates":{}}`,
		`not json`,
	))
	if len(got) != 2 || got[0].ID != "exec-1" || got[0].Tool != "spawnAgent" || got[0].ReceiverThreads[0] != "c1" || got[0].Prompt == nil || *got[0].Prompt != "crw-flow-op: c_a/t1-a1" ||
		*got[0].Model != "m" || *got[0].ReasoningEffort != "high" || got[1].Tool != "wait" || got[1].Prompt != nil {
		t.Errorf("CollabCalls: %+v", got)
	}
}

// Tokens is the last token_count total of a rollout, read only; a rollout that is absent or has no count is unmeasured, and the
// cut end of a write that did not finish is left out.
func TestTokens(t *testing.T) {
	dir := t.TempDir()
	count := func(input, cached, output, reasoning, total int) string {
		return `{"timestamp":"2026-10-13T13:53:07.558Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":` +
			itoa(input) + `,"cached_input_tokens":` + itoa(cached) + `,"output_tokens":` + itoa(output) + `,"reasoning_output_tokens":` + itoa(reasoning) +
			`,"total_tokens":` + itoa(total) + `},"last_token_usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"model_context_window":950000},"rate_limits":{}}}`
	}
	write := func(name string, lines ...string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(strings.Join(lines, "")), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	meta := `{"type":"session_meta","payload":{"id":"x","source":{"subagent":{}}}}` + "\n"
	rateOnly := `{"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{}}}` + "\n"
	big := `{"type":"response_item","payload":{"type":"message","content":[{"type":"output_text","text":"` + strings.Repeat("x", 200000) + `"}]}}` + "\n"
	full := write("full.jsonl", meta, count(10, 5, 3, 1, 13)+"\n", big, count(788563, 693071, 28794, 22198, 817357)+"\n", rateOnly, `{"type":"event_msg","payload":{"type":"token_cou`)
	got, measured, err := sessionhost.Tokens(full)
	want := sessionhost.Usage{Input: 788563, CachedInput: 693071, Output: 28794, ReasoningOutput: 22198, Total: 817357}
	if err != nil || !measured || got != want {
		t.Errorf("Tokens = %+v %v %v, want %+v", got, measured, err, want)
	}
	for name, path := range map[string]string{
		"absent":        filepath.Join(dir, "absent.jsonl"),
		"no count":      write("none.jsonl", meta, big),
		"only rates":    write("rates.jsonl", meta, rateOnly),
		"empty path":    "",
		"cut last line": write("cut.jsonl", meta, count(1, 1, 1, 1, 2)),
	} {
		if got, measured, err := sessionhost.Tokens(path); err != nil || measured || got != (sessionhost.Usage{}) {
			t.Errorf("%s: %+v %v %v", name, got, measured, err)
		}
	}
	// a committed line that is not JSON is skipped, not a failure
	if got, measured, err := sessionhost.Tokens(write("junk.jsonl", "{not json token_count\n", count(4, 0, 2, 0, 6)+"\n")); err != nil || !measured || got.Total != 6 {
		t.Errorf("junk line: %+v %v %v", got, measured, err)
	}
	// a directory is a read error
	if _, measured, err := sessionhost.Tokens(dir); err == nil || measured {
		t.Errorf("a directory: %v %v", measured, err)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
