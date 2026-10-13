package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// CRW-1331: the rows of a session call in the store. step_published is a dispatch intent, committed before the step file is
// written; thread_started and turn_started are written in one batch; the columns the relay reads are filled from them.

type miniHost struct {
	mu     sync.Mutex
	agents map[string]engine.SessionAgent
	turns  map[string]engine.SessionTurn
}

func (h *miniHost) ReadAgent(_ context.Context, id string) (engine.SessionAgent, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a, ok := h.agents[id]
	if !ok {
		return engine.SessionAgent{}, engine.ErrSessionAgentNotFound
	}
	return a, nil
}

func (h *miniHost) Turns(_ context.Context, id string, _ int, _ bool) ([]engine.SessionTurn, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.turns[id]
	if !ok {
		return nil, engine.ErrSessionAgentNotFound
	}
	return []engine.SessionTurn{t}, nil
}

func (h *miniHost) FindByMarker(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}
func (h *miniHost) Interrupt(context.Context, string, string) error { return nil }
func (h *miniHost) Tokens(context.Context, string) (engine.Tokens, bool) {
	return engine.Tokens{Input: 3, Output: 2, Total: 5}, true
}

// atPublish is the bus whose Publish reads what the store holds at the moment a step file is written.
type atPublish struct {
	*engine.FileStepBus
	db *sql.DB
	mu sync.Mutex
	at map[string][]string
	t  *testing.T
}

func (b *atPublish) Publish(st engine.Step) error {
	if st.Action != engine.StepClose {
		b.mu.Lock()
		b.at[st.Call] = dbEvents(b.t, b.db, st.Call)
		b.mu.Unlock()
	}
	return b.FileStepBus.Publish(st)
}

type placer struct{ *engine.Fake }

func (placer) Place(req engine.PlaceRequest) (engine.Placement, error) {
	return engine.Placement{Kind: engine.PlaceCall, Path: "/w/call/" + req.CallID, Start: "base"}, nil
}
func (placer) Establish(context.Context, engine.PlaceRequest, engine.Placement) error { return nil }
func (placer) Recheck(context.Context, engine.Placement) error                        { return nil }
func (placer) Snapshot(context.Context, string) (string, error)                       { return "T0", nil }

func TestSessionRowsInTheStore(t *testing.T) {
	st, _ := newStore(t)
	j := newJournal(t, st)
	host := &miniHost{agents: map[string]engine.SessionAgent{}, turns: map[string]engine.SessionTurn{}}
	bus := &atPublish{FileStepBus: engine.NewFileStepBus(t.TempDir()), db: st.DB, at: map[string][]string{}, t: t}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		seen := map[string]bool{}
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			steps, _ := bus.Steps()
			for _, s := range steps {
				if s.Action != engine.StepSpawn || seen[s.Op] {
					continue
				}
				seen[s.Op] = true
				id := "agent-" + s.Call
				host.mu.Lock()
				host.agents[id] = engine.SessionAgent{ThreadID: id, ParentThreadID: "owner", Model: s.Model, Effort: s.ReasoningEffort, Cwd: "/orch", AgentRole: s.AgentType, Status: "idle", RolloutPath: "/r/" + id}
				host.turns[id] = engine.SessionTurn{ID: id + "-t1", Status: "completed", FirstUserText: s.Message, FinalText: `{"verdict":"verified"}`, FinalPhase: "final_answer"}
				host.mu.Unlock()
				_ = bus.Report(engine.DispatchReport{ID: s.Op, Run: s.Run, Call: s.Call, K: s.K, Attempt: s.Attempt, Agent: id})
			}
		}
	}()
	prog, err := script.Load(script.Source{Name: "t.js", Text: []byte(header + `const S = ` + schema + `
const r = await agent.call('review', { agentType: 'reviewer', node: 'n1', head: 'h1', label: 'rev', schema: S })
return [r.callId, r.status]`)})
	if err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	res, err := engine.Run(rctx, prog, engine.Config{Backend: placer{engine.NewFake()}, Policy: engine.FakePolicy(), Journal: j, RunID: runID, Now: fixedNow,
		Session: &engine.SessionConfig{Host: host, Bus: bus, Owner: "owner", Poll: time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != engine.RunCompleted {
		t.Fatalf("run %s %s", res.Status, res.Error)
	}
	var out []string
	if err := json.Unmarshal(res.Value, &out); err != nil || out[1] != "completed" {
		t.Fatalf("value %s", res.Value)
	}
	call := out[0]
	if got := bus.at[call]; !slices.Equal(got, []string{"prepared", "step_published"}) {
		t.Errorf("when the step file is written the store holds %v, want the two intents", got)
	}
	if got := dbEvents(t, st.DB, call); !slices.Equal(got, []string{"prepared", "step_published", "thread_started", "turn_started", "completed"}) {
		t.Errorf("rows %v", got)
	}
	var thread, turn, role, node string
	if err := st.DB.QueryRowContext(ctx, `SELECT thread_id, role, node_id FROM flow_calls WHERE call_id = ? AND event = 'thread_started'`, call).Scan(&thread, &role, &node); err != nil {
		t.Fatal(err)
	}
	if thread != "agent-"+call || role != "reviewer" || node != "n1" {
		t.Errorf("thread_started columns %q %q %q", thread, role, node)
	}
	var detail string
	if err := st.DB.QueryRowContext(ctx, `SELECT thread_id, turn_id, detail_json FROM flow_calls WHERE call_id = ? AND event = 'completed'`, call).Scan(&thread, &turn, &detail); err != nil {
		t.Fatal(err)
	}
	if thread != "agent-"+call || turn != "agent-"+call+"-t1" {
		t.Errorf("the completed row names thread %q turn %q", thread, turn)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT detail_json FROM flow_calls WHERE call_id = ? AND event = 'thread_started'`, call).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"model":"gpt-fake-sol"`, `"effort":"high"`, `"via":"session"`, `"cwd":"/w/call/` + call + `"`, `"host_cwd":"/orch"`} {
		if !strings.Contains(detail, want) {
			t.Errorf("thread_started detail %s lacks %s", detail, want)
		}
	}
}
