package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/role/spawn"
)

// CRW-1482 (1209 field trial, defect C9) end to end: a session run's executor step, the subagent the owner spawns for it on the
// fake App Server, and the SubagentStop the host sends for that subagent, through the hook's registered leg. In the trial the
// stop payload carried the owner's session and cwd, the subagent's rollout (whose first UserMessage is the spawn message) and the
// RECEIPT JSON alone as the final message; no assignment named the call worktree P, so the hook looked under the owner's native
// .crw/evidence, blocked three times and released the child with a tombstone. Now the engine records a flow assignment for the
// step and names its one receipt file in the spawn message; the hook accepts the receipt at that file without a marker line,
// the completed row records it (evidence_receipt) and the step bus keeps a copy for the acceptance path (R2 CRW-1436, R3
// CRW-1442). A receipt anywhere else is refused with a reason that names the file, and after the budget the completed row says
// missing.

const c9Receipt = `{"status":"done","summary":"implemented the parser fix","tests":["go test ./internal/x/"]}`

var (
	c9BlockRe   = regexp.MustCompile(regexp.QuoteMeta(evidence.AssignmentMarker) + `:([^\]]+)\]`)
	c9ReceiptRe = regexp.MustCompile(`Write your evidence receipt to the file (\S+) before your final message`)
)

// c9Step is the executor step of a one-call session run, its subagent held in its turn (the host shows the turn completed only
// after the SubagentStop hook released it), and the contract the step's message names.
type c9Step struct {
	k       *sessionE2E
	run     string
	st      NextStep
	agent   string
	turn    string
	rollout string
	id      string
	receipt string
	gate    chan struct{}
}

// startC9 starts a session run of one executor call in the kit's repository, takes its spawn step, spawns its subagent with a
// real rollout and a gated turn whose final message is the RECEIPT JSON, and reports it. ignore commits a .gitignore of .crw/ to
// the repository first (CRW's own repository ignores it; a target repository may not).
func startC9(t *testing.T, ignore bool) *c9Step {
	t.Helper()
	k := newSessionE2E(t)
	t.Setenv("CRW_PABCD", "on")
	if ignore {
		if err := os.WriteFile(filepath.Join(k.repo, ".gitignore"), []byte(".crw/\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"-C", k.repo, "add", ".gitignore"}, {"-C", k.repo, "commit", "-q", "-m", "ignore crw state"}} {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
	}
	script := filepath.Join(t.TempDir(), "c9.js")
	if err := os.WriteFile(script, []byte(`export const meta = { name: 'c9', phases: ['build'] }
phase('build')
const r = await agent.call('implement the parser fix', { agentType: 'executor', node: 'n1', label: 'build' })
return [r.status, r.output]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := k.cmd("start", script, "--thread", k.owner, "--repo", k.repo, "--json")
	if code != 0 {
		t.Fatalf("start: exit %d\n%s\n%s", code, out, errOut)
	}
	var started StartAnswer
	if err := json.Unmarshal([]byte(out), &started); err != nil {
		t.Fatal(err)
	}
	s := &c9Step{k: k, run: started.Run, gate: make(chan struct{})}
	t.Cleanup(s.release)
	_, s.st = k.nextSpawn(s.run)
	if s.st.Role != "executor" || s.st.Workspace == "" || filepath.Clean(s.st.Workspace) == filepath.Clean(k.repo) {
		t.Fatalf("the executor step: %+v", s.st)
	}

	// the contract the step's message names: the block and the one receipt file, inside the call worktree P
	m := c9BlockRe.FindStringSubmatch(s.st.Message)
	r := c9ReceiptRe.FindStringSubmatch(s.st.Message)
	if m == nil || r == nil || !strings.HasPrefix(s.st.Message, "crw-flow-op: "+s.st.Op+"\nCRW-ROLE: executor\n") {
		t.Fatalf("the executor step's message names no evidence assignment:\n%s", s.st.Message)
	}
	s.id, s.receipt = m[1], r[1]
	if want := filepath.Join(s.st.Workspace, ".crw", "evidence", s.id+".md"); s.receipt != want {
		t.Fatalf("the message names the receipt %s, want %s", s.receipt, want)
	}
	if info, err := os.Lstat(filepath.Dir(s.receipt)); err != nil || !info.IsDir() {
		t.Fatalf("the receipt directory in P: %v %v", info, err)
	}
	rec, ok := evidence.RecordedAssignment(k.repo, k.owner, s.id)
	if !ok || rec.Origin != evidence.AssignmentOriginFlow || rec.Op != s.st.Op || filepath.Clean(rec.Root) != filepath.Clean(s.st.Workspace) ||
		rec.Status != evidence.AssignmentOpen || evidence.FlowReceiptPath(rec) != s.receipt {
		t.Fatalf("the flow assignment under the owner cwd: %+v (%v)", rec, ok)
	}
	if !strings.Contains(s.packet(), s.receipt) {
		t.Fatalf("the packet names no receipt file %s:\n%s", s.receipt, s.packet())
	}
	published := s.row("step_published")
	if published["evidence_assignment"] != s.id || published["evidence_ignored"] != ignore {
		t.Fatalf("step_published: %v", published)
	}

	s.agent = s.spawn()
	k.dispatched(s.run, s.st, s.agent)
	return s
}

// spawn is the owner's spawn of the step: the spawn hook allows it, and the host spawns the subagent with the message the hook
// let through, a rollout whose first own UserMessage is that message, and a turn held until release.
func (s *c9Step) spawn() string {
	k, t := s.k, s.k.t
	t.Helper()
	k.mu.Lock()
	k.tools++
	toolUse := fmt.Sprintf("call-tool-%d", k.tools)
	k.mu.Unlock()
	input, _ := json.Marshal(s.st.Tool.Arguments)
	out := spawn.RunSpawnAttachHook(`{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":`+jsonString(k.owner)+`,"tool_use_id":`+jsonString(toolUse)+
		`,"cwd":`+jsonString(k.repo)+`,"tool_input":`+string(input)+`}`, func(string) (string, bool) { return "", false })
	var hook struct {
		Out struct {
			Decision string         `json:"permissionDecision"`
			Updated  map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &hook); err != nil || hook.Out.Decision != "allow" {
		t.Fatalf("the hook did not allow the step's spawn: %q", out)
	}
	message := s.st.Message
	if m, ok := hook.Out.Updated["message"].(string); ok {
		message = m
	}
	if !strings.Contains(message, evidence.AssignmentMarker+":"+s.id+"]") {
		t.Fatalf("the spawn hook dropped the evidence block:\n%s", message)
	}
	model, _ := hook.Out.Updated["model"].(string)
	effort, _ := hook.Out.Updated["reasoning_effort"].(string)
	agentType, _ := hook.Out.Updated["agent_type"].(string)
	s.rollout = filepath.Join(t.TempDir(), "rollout.jsonl")
	agent := k.host.Threads().Spawn(fakehost.SubagentSpec{Parent: k.owner, ToolUseID: toolUse, AgentType: agentType, Model: model, Effort: effort,
		Message: message, RolloutPath: s.rollout, Plan: fakehost.TurnPlan{Message: c9Receipt, Gate: s.gate}})
	view, ok := k.host.Threads().Thread(agent)
	if !ok || len(view.Turns) != 1 {
		t.Fatalf("the subagent %s: %+v", agent, view)
	}
	s.turn = view.Turns[0].ID
	var b bytes.Buffer
	for _, line := range []any{
		map[string]any{"type": "session_meta", "payload": map[string]any{"id": agent, "session_id": k.owner}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "# AGENTS.md instructions"}}}},
		map[string]any{"type": "event_msg", "payload": map[string]any{"type": "item_completed", "thread_id": agent, "turn_id": s.turn,
			"item": map[string]any{"type": "UserMessage", "id": "u1", "content": []any{map[string]any{"type": "text", "text": message}}}}},
	} {
		raw, _ := json.Marshal(line)
		b.Write(append(raw, '\n'))
	}
	if err := os.WriteFile(s.rollout, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return agent
}

// stop is the SubagentStop the host sends for the subagent's turn, through the hook's registered leg: the owner's cwd and
// session, the subagent and its turn, its rollout and its final message.
func (s *c9Step) stop(message string) string {
	t := s.k.t
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"hook_event_name": "SubagentStop", "cwd": s.k.repo, "session_id": s.k.owner, "agent_type": "executor",
		"agent_id": s.agent, "turn_id": s.turn, "last_assistant_message": message, "agent_transcript_path": s.rollout})
	var out, stderr bytes.Buffer
	code := harness.Hook(context.Background(), []string{"subagent-stop", "--leg", "subagent-stop-verifying-evidence"},
		bytes.NewReader(raw), &out, &stderr, os.LookupEnv, harness.Legs())
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("the subagent stop hook: exit %d: %s", code, stderr.String())
	}
	return out.String()
}

// blocked asserts the stop was blocked with the reason that names the receipt file.
func (s *c9Step) blocked(out string, attempt int) {
	t := s.k.t
	t.Helper()
	var v struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Decision != "block" {
		t.Fatalf("stop %d: want a block, got %q", attempt, out)
	}
	if want := evidence.VerifierDirectiveFor(attempt, s.receipt); v.Reason != want || !strings.Contains(v.Reason, s.receipt) {
		t.Fatalf("stop %d: reason %q, want %q", attempt, v.Reason, want)
	}
}

// release lets the subagent's turn end (the host shows it completed once the hook released the child).
func (s *c9Step) release() {
	select {
	case <-s.gate:
	default:
		close(s.gate)
	}
}

// finish releases the turn and waits for the run's end.
func (s *c9Step) finish() {
	t := s.k.t
	t.Helper()
	s.release()
	deadline := time.Now().Add(90 * time.Second)
	for {
		ans := s.k.next(s.run, 5)
		if ans.State == NextEnded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run did not end: %+v", ans)
		}
	}
	s.k.waitProcess()
}

// row is the data of the call's newest row of the event.
func (s *c9Step) row(event string) map[string]any {
	t := s.k.t
	t.Helper()
	var data map[string]any
	s.k.readStore(func(st *store.Store) {
		var detail string
		if err := st.DB.QueryRow(`SELECT detail_json FROM flow_calls WHERE call_id = ? AND run_id = ? AND event = ? ORDER BY event_seq DESC LIMIT 1`,
			s.st.Call, s.run, event).Scan(&detail); err != nil {
			t.Fatalf("call %s %s: %v", s.st.Call, event, err)
		}
		if err := json.Unmarshal([]byte(detail), &data); err != nil {
			t.Fatalf("call %s %s: %v\n%s", s.st.Call, event, err, detail)
		}
	})
	return data
}

// events is the call's row sequence.
func (s *c9Step) events() string {
	var events []string
	s.k.readStore(func(st *store.Store) {
		rows, err := st.DB.Query(`SELECT event FROM flow_calls WHERE call_id = ? AND run_id = ? ORDER BY event_seq`, s.st.Call, s.run)
		if err != nil {
			s.k.t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var e string
			_ = rows.Scan(&e)
			events = append(events, e)
		}
	})
	return strings.Join(events, ",")
}

func (s *c9Step) packet() string {
	raw, err := os.ReadFile(s.st.Packet)
	if err != nil {
		s.k.t.Fatalf("the packet: %v", err)
	}
	return string(raw)
}

func jsonString(v string) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func TestC9(t *testing.T) {
	t.Run("ReceiptAccepted", func(t *testing.T) {
		s := startC9(t, true)
		body := "$ go test ./internal/x/ -run TestParser\nFAIL (red before)\n$ go test ./internal/x/\nok (green after)\n"
		if err := os.WriteFile(s.receipt, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		// the final message is the RECEIPT JSON alone, as the packet asks: no EVIDENCE_RECORDED line after it
		if out := s.stop(c9Receipt); out != "" {
			t.Fatalf("the receipt at the file the message names was not accepted: %q", out)
		}
		if c := evidence.ReadCounter(s.k.repo, s.k.owner, s.agent, s.turn); c.Attempts != 0 || c.Spent() {
			t.Fatalf("attempts after the release: %+v", c)
		}
		if _, err := os.Stat(filepath.Join(s.k.repo, ".crw", "evidence")); !os.IsNotExist(err) {
			t.Fatalf("the hook looked in or wrote the owner's native evidence directory: %v", err)
		}
		s.finish()

		if got := s.events(); !strings.HasPrefix(got, "prepared,step_published,") || !strings.HasSuffix(got, "thread_started,turn_started,completed") {
			t.Fatalf("the call's rows: %s", got)
		}
		started := s.row("thread_started")
		if started["thread_id"] != s.agent || started["evidence_cwd"] != s.k.repo {
			t.Fatalf("thread_started: %v", started)
		}
		receipt, _ := s.row("completed")["evidence_receipt"].(map[string]any)
		sum := sha256.Sum256([]byte(body))
		digest := hex.EncodeToString(sum[:])
		bus := engine.NewFileStepBus(RunDir(s.k.w.state, s.run))
		copyPath := bus.FilePath(s.st.Call, "evidence-"+s.id+".md")
		if receipt["state"] != "accepted" || receipt["assignment"] != s.id || receipt["path"] != s.receipt || receipt["sha256"] != digest ||
			receipt["agent_id"] != s.agent || receipt["turn_id"] != s.turn || receipt["turn_match"] != true || receipt["ignored"] != true ||
			receipt["copy"] != copyPath {
			t.Fatalf("the completed row's evidence_receipt: %v (want the copy %s)", receipt, copyPath)
		}
		// the acceptance path reads the bus copy, which outlives P, and the record the hook claimed
		if raw, err := os.ReadFile(copyPath); err != nil || string(raw) != body {
			t.Fatalf("the bus copy: %q %v", raw, err)
		}
		got, ok := evidence.AcceptedReceipt(s.k.repo, s.k.owner, s.id)
		if !ok || string(got.Bytes) != body || got.Digest != digest || got.AgentID != s.agent || got.TurnID != s.turn {
			t.Fatalf("AcceptedReceipt = %+v, %v", got, ok)
		}
		if res, ok := ReadResult(s.k.w.state, s.run); !ok || res.Status != engine.RunCompleted {
			t.Fatalf("the result: %+v %v", res, ok)
		}
	})

	t.Run("Refused", func(t *testing.T) {
		s := startC9(t, false)
		// stop 1: no receipt anywhere
		s.blocked(s.stop(c9Receipt), 1)
		// stop 2: a receipt forged in the owner's native .crw/evidence, which the native root alone would take
		forged := filepath.Join(s.k.repo, ".crw", "evidence", s.id+".md")
		if err := os.MkdirAll(filepath.Dir(forged), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(forged, []byte("forged"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !evidence.HasValidReceipt(s.k.repo, forged) {
			t.Fatal("the forged receipt is not one the native root would take")
		}
		s.blocked(s.stop(c9Receipt+"\nEVIDENCE_RECORDED: "+forged), 2)
		// stop 3: a receipt of this assignment's name in another run's worktree (a checkout of the same repository)
		other := filepath.Join(t.TempDir(), "other-run-wt")
		if out, err := exec.Command("git", "clone", "-q", s.k.repo, other).CombinedOutput(); err != nil {
			t.Fatalf("git clone: %v\n%s", err, out)
		}
		elsewhere := filepath.Join(other, ".crw", "evidence", s.id+".md")
		if err := os.MkdirAll(filepath.Dir(elsewhere), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(elsewhere, []byte("another run's receipt"), 0o644); err != nil {
			t.Fatal(err)
		}
		s.blocked(s.stop(c9Receipt+"\nEVIDENCE_RECORDED: "+elsewhere), 3)
		// the budget is spent: the next stop releases the child with its tombstone, as before
		if out := s.stop(c9Receipt); out != "" {
			t.Fatalf("the spent budget still blocks: %q", out)
		}
		if !evidence.HasTombstone(s.k.repo, s.k.owner, evidence.Payload{AgentType: "executor", AgentID: s.agent, TurnID: s.turn, LastAssistantMessage: c9Receipt}) {
			t.Fatal("no tombstone after the budget")
		}
		if rec, ok := evidence.RecordedAssignment(s.k.repo, s.k.owner, s.id); !ok || rec.Status != evidence.AssignmentOpen || rec.AgentID != "" {
			t.Fatalf("a refused receipt claimed the record: %+v (%v)", rec, ok)
		}
		s.finish()

		receipt, _ := s.row("completed")["evidence_receipt"].(map[string]any)
		if detail, _ := receipt["detail"].(string); receipt["state"] != "missing" || receipt["assignment"] != s.id || receipt["ignored"] != false || receipt["copy"] != nil || detail == "" {
			t.Fatalf("the completed row's evidence_receipt after the tombstone release: %v", receipt)
		}
		if _, err := os.Stat(engine.NewFileStepBus(RunDir(s.k.w.state, s.run)).FilePath(s.st.Call, "evidence-"+s.id+".md")); !os.IsNotExist(err) {
			t.Fatalf("a refused receipt was copied to the step bus: %v", err)
		}
	})
}
