package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

var _ ops.NativeHost = (*agent.Backend)(nil)

// F14 (CRW-1207): the run tells the spawn hook which threads are its own. Before every turn/start of an engine thread it appends a
// line {run_id, thread_id, kind engine, call_id, role, nativeAllowance, helperPairs{explorer}, policyDigest} to the thread's file in
// the flow-bound directory and makes it durable; a line that cannot be written holds the call and no turn is sent.

// boundDir is the flow-bound directory of the isolated home.
func boundDir(t *testing.T) string {
	t.Helper()
	dir, err := bound.Dir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// boundLines reads every line of the binding file of a thread.
func boundLines(t *testing.T, thread string) []bound.Line {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(boundDir(t), thread+bound.FileSuffix))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []bound.Line
	for _, line := range bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n")) {
		var l bound.Line
		if err := json.Unmarshal(line, &l); err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		out = append(out, l)
	}
	return out
}

// syncRecorder is a bound.Writer that sees every file it makes durable.
type syncRecorder struct {
	mu     sync.Mutex
	synced map[string]int
}

func (s *syncRecorder) writer() *bound.Writer {
	return &bound.Writer{Sync: func(f *os.File) error {
		s.mu.Lock()
		s.synced[f.Name()]++
		s.mu.Unlock()
		return f.Sync()
	}}
}

func (s *syncRecorder) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.synced[path]
}

// A turn/start the host receives is preceded by the durable line of its own call; a line that cannot be written holds the call
// (host_config_flow_binding) and the host receives no turn/start.
func TestBindingWrittenBeforeTurnStart(t *testing.T) {
	t.Run("every turn", func(t *testing.T) {
		rec := &syncRecorder{synced: map[string]int{}}
		e := newEnv(t, withBound(rec.writer()))
		type seen struct {
			thread, op, lineCall string
			lines, synced        int
		}
		var mu sync.Mutex
		var observed []seen
		turns := 0
		e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
			// this runs while the host handles the turn/start: what the run promised must already be on disk
			lines := boundLines(t, r.ThreadID)
			s := seen{thread: r.ThreadID, op: r.ClientID, lines: len(lines), synced: rec.count(filepath.Join(boundDir(t), r.ThreadID+bound.FileSuffix))}
			if len(lines) > 0 {
				s.lineCall = lines[len(lines)-1].CallID
			}
			mu.Lock()
			observed = append(observed, s)
			turns++
			first := turns == 1
			mu.Unlock()
			if first {
				return fakehost.TurnPlan{Message: "not json"} // the call repairs: a second turn on the same thread
			}
			return fakehost.TurnPlan{Message: passMessage}
		})
		res := e.run(`const S = ` + verdictSchema + `
const b = await agent.call('build it', { agentType: 'executor', node: 'n1', schema: S, label: 'build' })
const r = await agent.call('review it', { agentType: 'ocx-gpt-6-1-sol', node: 'n1', head: ` + q(e.base) + `, schema: S, label: 'review' })
return [b.status, r.status]`)
		if got := value[[]string](t, res); !slices.Equal(got, []string{"completed", "completed"}) {
			t.Fatalf("%v", got)
		}
		if n := len(e.requests("turn/start")); n != 3 || len(observed) != 3 {
			t.Fatalf("%d turn/start requests, %d observed: want the first turn, its repair and the review", n, len(observed))
		}
		for _, s := range observed {
			call, _, _ := strings.Cut(s.op, "/")
			if s.lineCall != call {
				t.Errorf("turn %s: the last line of the thread's file names call %q", s.op, s.lineCall)
			}
			if s.synced < s.lines {
				t.Errorf("turn %s: %d lines in the file but %d syncs: a line was not durable when the turn was sent", s.op, s.lines, s.synced)
			}
		}
		// the repair turn gets a line of its own, for the same call
		build := calls(res)["build"]
		lines := boundLines(t, build.Meta.ThreadID)
		if len(lines) != 2 || lines[0].CallID != build.CallID || lines[1].CallID != build.CallID {
			t.Fatalf("lines of the build thread: %+v", lines)
		}
	})

	for _, c := range []struct {
		name  string
		setup func(t *testing.T) *bound.Writer
	}{
		{"line cannot be made durable", func(t *testing.T) *bound.Writer {
			return &bound.Writer{Sync: func(*os.File) error { return errors.New("injected: disk full") }}
		}},
		{"directory is not private", func(t *testing.T) *bound.Writer {
			dir := boundDir(t)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, withBound(nil))
			e.cfg.writer = c.setup(t)
			e.restart("") // the backend is built again over the writer of the case
			res := e.run(`const S = ` + verdictSchema + `
const b = await agent.call('build it', { agentType: 'executor', node: 'n1', schema: S, label: 'build' })
return [b.status, b.reason]`)
			if got := value[[]string](t, res); !slices.Equal(got, []string{"held", "host_config_flow_binding"}) {
				t.Fatalf("%v", got)
			}
			if n := e.host.Count("turn/start"); n != 0 {
				t.Fatalf("%d turn/start requests: nothing may be sent when the binding is not written", n)
			}
		})
	}
}

// The line names the call, the role, how many helpers the call may spawn and the helper pair of the run's pinned policy.
func TestBindingLineCarriesRoleAllowanceAndPolicyPairs(t *testing.T) {
	e := newEnv(t, withBound(nil))
	policy := *e.policy
	policy.Digest = "sha256:pinned"
	e.policy = &policy
	e.restart("")
	res := e.run(`const S = ` + verdictSchema + `
const b = await agent.call('build it', { agentType: 'executor', node: 'n1', schema: S, label: 'build' })
const x = await agent.call('look', { agentType: 'explorer', schema: S, label: 'look' })
const r = await agent.call('review it', { agentType: 'ocx-gpt-6-1-sol', node: 'n1', head: ` + q(e.base) + `, schema: S, label: 'review' })
const c = await agent.call('audit it', { agentType: 'critic', head: ` + q(e.base) + `, schema: S, label: 'audit' })
return [b.status, x.status, r.status, c.status]`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"completed", "completed", "completed", "completed"}) {
		t.Fatalf("%v", got)
	}
	// the critic (CRW-1434) spawns no helper, as its overlay says: its line allows none
	want := map[string]struct {
		role  string
		allow int
	}{"build": {"executor", 1}, "look": {"explorer", 0}, "review": {"reviewer", 1}, "audit": {"critic", 0}}
	for label, w := range want {
		call := calls(res)[label]
		lines := boundLines(t, call.Meta.ThreadID)
		if len(lines) != 1 {
			t.Fatalf("%s: %d lines", label, len(lines))
		}
		l := lines[0]
		pair := policy.Roles["explorer"].Pair
		if l.RunID != e.runID || l.ThreadID != call.Meta.ThreadID || l.Kind != bound.KindEngine || l.CallID != call.CallID || l.Role != w.role ||
			l.NativeAllowance == nil || *l.NativeAllowance != w.allow || l.PolicyDigest != "sha256:pinned" ||
			l.HelperPairs[bound.HelperExplorer] != (bound.Pair{Model: pair.Model, Effort: pair.Effort}) || len(l.HelperPairs) != 1 {
			t.Errorf("%s: %+v (allowance %d, want %d)", label, l, l.Allowance(), w.allow)
		}
	}
	if info, err := os.Stat(boundDir(t)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Join(boundDir(t), calls(res)["build"].Meta.ThreadID+bound.FileSuffix)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file %v %v", info, err)
	}
}

// The QA capture call (SP-QA-1) is an executor call that may spawn nothing: the run says so with the allowance of its line. How
// the engine marks that call (F13) is outside this backend, so the allowance is a choice of the configuration.
func TestBindingAllowanceOverrideForTheQACall(t *testing.T) {
	e := newEnv(t, withBound(nil))
	cfg := e.boundConfig()
	cfg.NativeAllowance = func(spec engine.TurnSpec) int {
		if spec.Node == "qa" {
			return 0
		}
		return 1
	}
	e.boundOverride = cfg
	e.restart("")
	res := e.run(`const S = ` + verdictSchema + `
const q = await agent.call('capture', { agentType: 'executor', node: 'qa', schema: S, label: 'qa' })
const b = await agent.call('build it', { agentType: 'executor', node: 'n1', schema: S, label: 'build' })
return [q.status, b.status]`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"completed", "completed"}) {
		t.Fatalf("%v", got)
	}
	for label, want := range map[string]int{"qa": 0, "build": 1} {
		lines := boundLines(t, calls(res)[label].Meta.ThreadID)
		if len(lines) != 1 || lines[0].Allowance() != want {
			t.Errorf("%s: %+v, want allowance %d", label, lines, want)
		}
	}
}

// A thread whose id is not a thread id cannot be named by a binding, so its call is held and no turn is sent.
func TestBindingRefusesAThreadIDOfAnotherShape(t *testing.T) {
	e := newEnv(t, withBound(nil))
	e.threads.ThreadIDs(nil) // thr-fake-NNN
	res := e.run(`const S = ` + verdictSchema + `
const b = await agent.call('build it', { agentType: 'executor', node: 'n1', schema: S, label: 'build' })
return [b.status, b.reason]`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"held", "host_config_flow_binding"}) {
		t.Fatalf("%v", got)
	}
	if n := e.host.Count("turn/start"); n != 0 {
		t.Fatalf("%d turn/start requests", n)
	}
	if _, err := os.Stat(boundDir(t)); err == nil {
		entries, _ := os.ReadDir(boundDir(t))
		if len(entries) != 0 {
			t.Fatalf("files written for a thread that cannot be bound: %v", entries)
		}
	}
}

// The thread that orchestrates a run gets an opening line (helper pairs explorer and reviewer, no allowance) when the run starts
// and a closed line when it ends or is cancelled; a closed thread is an ordinary one again.
func TestOrchestratorBindingOpensAndCloses(t *testing.T) {
	e := newEnv(t, withBound(nil))
	const orchestrator = "0190b000-0000-7000-8000-00000000000a"
	if err := e.backend.BindOrchestrator(context.Background(), orchestrator); err != nil {
		t.Fatal(err)
	}
	lines := boundLines(t, orchestrator)
	explorer, reviewer := e.policy.Roles["explorer"].Pair, e.policy.Roles["reviewer"].Pair
	if len(lines) != 1 || lines[0].Kind != bound.KindOrchestrator || lines[0].RunID != e.runID || lines[0].NativeAllowance != nil ||
		lines[0].HelperPairs[bound.HelperExplorer] != (bound.Pair{Model: explorer.Model, Effort: explorer.Effort}) ||
		lines[0].HelperPairs[bound.HelperReviewer] != (bound.Pair{Model: reviewer.Model, Effort: reviewer.Effort}) {
		t.Fatalf("opening line %+v", lines)
	}
	if got := bound.Lookup(orchestrator); got.State != bound.Orchestrator {
		t.Fatalf("an open run's orchestrator is %v", got.State)
	}
	if err := e.backend.CloseOrchestrator(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lines = boundLines(t, orchestrator); len(lines) != 2 || lines[1].Kind != bound.KindClosed || lines[1].RunID != e.runID {
		t.Fatalf("lines after the close: %+v", lines)
	}
	if got := bound.Lookup(orchestrator); got.State != bound.Standalone {
		t.Fatalf("a closed run's orchestrator is %v", got.State)
	}
	// closing again, or a run that never opened one, writes nothing
	if err := e.backend.CloseOrchestrator(context.Background()); err != nil || len(boundLines(t, orchestrator)) != 2 {
		t.Fatalf("second close: %v", err)
	}
}

// The backend is the host of the run's import: the CollabAgentToolCall items of a turn and the configuration of the thread a
// native spawn created are read from the host by their ids, and an item the hook never decided is unbound_spawn.
func TestBackendReadsTheSpawnsOfATurnForTheImport(t *testing.T) {
	for _, c := range []struct {
		name, effort, decision string
		escalates              int
	}{
		{"receiver reads the applied pair", "high", ops.ObservationObserved, 0},
		{"receiver reads another effort", "low", ops.ObservationPairMismatch, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, withBound(nil))
			applied := e.policy.Roles["explorer"].Pair
			receiver := e.threads.Seed(fakehost.SeedThread{Model: applied.Model, Effort: c.effort})
			e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
				return fakehost.TurnPlan{Message: passMessage, Items: []map[string]any{
					{"type": "collabAgentToolCall", "id": "tool-1", "tool": "spawnAgent", "status": "completed", "senderThreadId": r.ThreadID,
						"receiverThreadIds": []any{receiver}, "agentsStates": map[string]any{}, "model": applied.Model, "reasoningEffort": applied.Effort, "prompt": nil},
					{"type": "collabAgentToolCall", "id": "tool-ghost", "tool": "spawnAgent", "status": "completed", "senderThreadId": r.ThreadID,
						"receiverThreadIds": []any{}, "agentsStates": map[string]any{}, "model": nil, "reasoningEffort": nil, "prompt": nil},
				}}
			})
			res := e.run(`const S = ` + verdictSchema + `
const b = await agent.call('build it', { agentType: 'executor', node: 'n1', schema: S, label: 'build' })
return b.status`)
			if got := value[string](t, res); got != "completed" {
				t.Fatal(got)
			}
			call := calls(res)["build"]
			dir := boundDir(t)
			// what the hook records for the one spawn it decided
			if err := bound.AppendDecision(dir, bound.Decision{RunID: e.runID, ThreadID: call.Meta.ThreadID, ThreadKind: bound.KindEngine, CallID: call.CallID, ToolUseID: "tool-1",
				Decision: bound.DecisionAllowed, HelperRole: bound.HelperExplorer, RequestedModel: "someone-elses", RequestedEffort: "low", AppliedModel: applied.Model, AppliedEffort: applied.Effort}); err != nil {
				t.Fatal(err)
			}
			got, err := ops.ImportNativeSpawns(context.Background(), e.backend, dir,
				ops.NativeTurn{RunID: e.runID, ThreadID: call.Meta.ThreadID, CallID: call.CallID, TurnID: call.Meta.TurnIDs[0], Node: "n1"}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			byTool := map[string]ops.SpawnRow{}
			for _, r := range got.Rows {
				if r.Kind == ops.NativeKindObservation {
					byTool[r.ToolUseID] = r
				}
			}
			if r := byTool["tool-1"]; r.Decision != c.decision || r.RequestedEffort != "low" || r.AppliedEffort != applied.Effort {
				t.Fatalf("tool-1: %+v", r)
			}
			if r := byTool["tool-ghost"]; r.Decision != ops.ObservationUnbound {
				t.Fatalf("tool-ghost: %+v", r)
			}
			reasons := map[string]int{}
			for _, esc := range got.Escalations {
				reasons[esc.Reason]++
			}
			if reasons[ops.ObservationUnbound] != 1 || reasons[ops.ObservationPairMismatch] != c.escalates {
				t.Fatalf("escalations %v", reasons)
			}
		})
	}
}
