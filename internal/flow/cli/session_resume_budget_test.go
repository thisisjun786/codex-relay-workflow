package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1380 merge of b2 (CRW-1278+1327) into D2 (CRW-1372): a resumed session run under a ceiling keeps both meanings of its started
// row and of what follows it. The process that resumes the run (the run process of `crw flow resume RUN`, and the foreground
// `crw flow resume RUN SCRIPT` through engine.ResumeHold) records one started row that names its process lock (`process`, CRW-1372)
// and its own ceiling (`token_budget`, CRW-1327): what the spec's budget leaves after what the run's journal shows it spent, narrowed
// by a --token-budget (CRW-1278). Only after that row (and its policy_repinned row) does it write the owner's orchestrator line, with
// the pairs the row pinned; nothing else writes it. What the earlier process spent is counted once: RunSpent over the journal, the
// script's budget.spent() and the result's tokensSpent are the earlier 70 tokens, not 0 and not twice.

// budgetSpentScript makes no call: its value is the budget as the resumed process reads it.
const budgetSpentScript = "export const meta = { name: 'spent' }\nreturn [budget.spent(), budget.total, budget.run]\n"

// observedRow is a row a resumed process appended and what the owner's line and the row's process lock were right after it.
type observedRow struct {
	event      engine.Event
	line       bound.Result
	lockHeld   bool
	processTok string
}

// observedJournal records, for every row the process appends, the owner's line once the row is in the journal.
type observedJournal struct {
	engine.Journal
	state, run, owner string
	mu                sync.Mutex
	rows              []observedRow
}

func (j *observedJournal) Append(e engine.Event) engine.Event {
	out := j.Journal.Append(e)
	row := observedRow{event: out, line: bound.Lookup(j.owner)}
	if out.Kind == engine.KindRun && out.Event == "started" {
		row.processTok = str(out.Data, "process")
		row.lockHeld = processHeld(j.state, j.run, row.processTok)
	}
	j.mu.Lock()
	j.rows = append(j.rows, row)
	j.mu.Unlock()
	return out
}

func (j *observedJournal) SetOnCommit(notify func()) {
	if n, ok := j.Journal.(engine.CommitNotifier); ok {
		n.SetOnCommit(notify)
	}
}

func (j *observedJournal) Err() error {
	if f, ok := j.Journal.(engine.FailingJournal); ok {
		return f.Err()
	}
	return nil
}

func (j *observedJournal) observed() []observedRow {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]observedRow(nil), j.rows...)
}

// openObserved opens the run's stored journal for a resumed process, as the run process and the engine's resume open it, with its
// session environment over an idle host.
func openObserved(ctx context.Context, state, run, owner string) (*observedJournal, *engine.SessionConfig, func() error, error) {
	st, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		return nil, nil, nil, err
	}
	j, err := journal.Open(ctx, st, run, journal.Identity{}, journal.Options{})
	if err != nil {
		_ = st.Close()
		return nil, nil, nil, err
	}
	session := &engine.SessionConfig{Host: idleSessionHost{}, Bus: engine.NewFileStepBus(RunDir(state, run)), Owner: owner,
		Poll: 20 * time.Millisecond, WakeAfter: time.Hour}
	return &observedJournal{Journal: j, state: state, run: run, owner: owner}, session, func() error { return errJoin(j.Close(), st.Close()) }, nil
}

func TestResumedSessionRunUnderACeilingRecordsItsProcessAndBudgetThenBindsTheOwner(t *testing.T) {
	for i, c := range []struct {
		name  string
		extra []string // the resume's command line after the run id
		// the resumed process's ceiling: the spec's 100 less the 70 spent is 30 left; 70 + 30, or 70 + 20 asked
		ceiling int64
	}{
		{name: "run process", ceiling: 100},
		{name: "script", extra: []string{"SCRIPT"}, ceiling: 100},
		{name: "script narrowed", extra: []string{"SCRIPT", "--token-budget", "20"}, ceiling: 90},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			w := newWorld(t)
			td := w.deps()
			run, owner := "run-0123456789f"+string(rune('1'+i)), "01a30000-0000-7000-8000-0000000000f1"
			script := filepath.Join(t.TempDir(), "spent.js")
			if err := os.WriteFile(script, []byte(budgetSpentScript), 0o644); err != nil {
				t.Fatal(err)
			}
			spec := Spec{Schema: SpecSchema, RunID: run, Workflow: "spent", Script: script, Thread: owner, Backend: BackendSession, TokenBudget: 100,
				CreatedAt: "2026-10-14T00:00:00Z"}
			if err := WriteSpec(w.state, spec); err != nil {
				t.Fatal(err)
			}
			src, err := spec.Load()
			if err != nil {
				t.Fatal(err)
			}
			// the first process: its started row under the spec's 100 tokens names its own process, one agent call of the run spent 70,
			// and the operator paused it; it closed the owner's line as it ended
			pinned, rows := pinnedSessionTable()
			j, err := journal.Open(ctx, w.st, run, journal.Identity{PlanID: "plan-1", PlanRevision: 3, ActorTaskID: owner, EpochNonce: "n"}, journal.Options{Immediate: true})
			if err != nil {
				t.Fatal(err)
			}
			j.Append(engine.Event{RunID: run, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "spent", "script_digest": src.Program.Digest,
				"args_digest": argsDigest([]byte("null")), "backend": engine.BackendSession, "policy_digest": pinned.Digest, "role_pairs": rows,
				"process": "0123456789abcdef", "token_budget": int64(100)}})
			j.Append(engine.Event{RunID: run, Kind: engine.KindCall, Op: "agent", CallID: "c-earlier", Label: "earlier", Event: engine.EvCompleted,
				Data: map[string]any{"tokens": engine.Tokens{Input: 60, Output: 10, Total: 70}}})
			j.Append(engine.Event{RunID: run, Kind: engine.KindFlow, Event: engine.EvRunPaused, Data: map[string]any{"by": "operator", "reason": "crw flow pause"}})
			if err := j.Close(); err != nil {
				t.Fatal(err)
			}
			if res, err := bindOwner(w.state, run, owner, pinned); err != nil || !res.Bound {
				t.Fatalf("the first process's line: %+v %v", res, err)
			}
			if _, err := closeOwner(run, owner); err != nil {
				t.Fatal(err)
			}
			storeTable, _, err := engine.LoadPolicy()
			if err != nil {
				t.Fatal(err)
			}

			// the resumed process's journal is observed, by the run process's world and by the engine's resume environment alike
			var obs *observedJournal
			td.World = func(ctx context.Context, req WorldRequest) (World, error) {
				oj, session, closeFn, err := openObserved(ctx, req.State, req.Spec.RunID, req.Spec.Thread)
				if err != nil {
					return World{}, err
				}
				obs = oj
				return World{Journal: oj, Backend: sessionTestPlacer{engine.NewFake()}, Session: session, Close: closeFn}, nil
			}
			prev := engine.ResumeEnv
			t.Cleanup(func() { engine.ResumeEnv = prev })
			engine.ResumeEnv = func(ctx context.Context, req engine.ResumeEnvRequest) (engine.ResumeSetup, error) {
				oj, session, closeFn, err := openObserved(ctx, req.State, req.RunID, owner)
				if err != nil {
					return engine.ResumeSetup{}, err
				}
				obs = oj
				return engine.ResumeSetup{Journal: oj, Backend: sessionTestPlacer{engine.NewFake()}, Session: session, Close: closeFn}, nil
			}
			args := []string{"resume", run}
			for _, a := range c.extra {
				if a == "SCRIPT" {
					a = script
				}
				args = append(args, a)
			}
			code, out, errOut := td.do(append(args, "--state", w.state)...)
			if code != 0 {
				t.Fatalf("the resume: exit %d\n%s\n%s", code, out, errOut)
			}
			if obs == nil {
				t.Fatal("the resume opened no journal")
			}

			// the value and the figure: the earlier 70 counted once
			var value json.RawMessage
			var spent int64
			if len(c.extra) > 0 {
				var res engine.Result
				if err := json.Unmarshal([]byte(out), &res); err != nil {
					t.Fatalf("%v: %s", err, out)
				}
				value, spent = res.Value, res.Spent
			} else {
				res, ok := ReadResult(w.state, run)
				if !ok {
					t.Fatal("the run process wrote no result")
				}
				value, spent = res.Value, res.Tokens
			}
			var got []any
			if err := json.Unmarshal(value, &got); err != nil || len(got) != 3 {
				t.Fatalf("the script's value %s: %v", value, err)
			}
			if got[0] != float64(70) || got[1] != float64(c.ceiling) || got[2] != float64(100) {
				t.Errorf("[budget.spent(), budget.total, budget.run] = %v, want [70 %d 100]: the earlier spending counted once under this process's ceiling, budget.run the first process's", got, c.ceiling)
			}
			if spent != 70 {
				t.Errorf("the result's tokensSpent %d, want the run's 70", spent)
			}
			events, _, err := loadRun(w, run)
			if err != nil {
				t.Fatal(err)
			}
			if total := engine.RunSpent(events, run).Total; total != 70 {
				t.Errorf("RunSpent %d, want 70", total)
			}

			// the resumed process's own rows: its started row, then the owner's line, written by it from that row
			seen := obs.observed()
			startedAt, finishedAt := -1, -1
			for k, r := range seen {
				switch {
				case r.event.Kind == engine.KindRun && r.event.Event == "started":
					startedAt = k
				case r.event.Kind == engine.KindRun && r.event.Event == "finished":
					finishedAt = k
				}
			}
			if startedAt < 0 || finishedAt < startedAt {
				t.Fatalf("the resumed process's rows: started at %d, finished at %d", startedAt, finishedAt)
			}
			started := seen[startedAt]
			if !validProcessToken(started.processTok) || started.processTok == "0123456789abcdef" || !started.lockHeld {
				t.Errorf("the resumed started row names the process %q (held %v): want the token of the process lock the resume holds", started.processTok, started.lockHeld)
			}
			if b := int64(num(started.event.Data, "token_budget")); b != c.ceiling {
				t.Errorf("the resumed started row records the ceiling %d, want %d (%v)", b, c.ceiling, started.event.Data)
			}
			if _, ok := started.event.Data["role_pairs"].(map[string]any); !ok {
				t.Errorf("the resumed started row pins no role pairs: %v", started.event.Data)
			}
			// up to the rows the started row is followed by (its policy_repinned row), the owner's line is not yet the resumed process's
			for k := startedAt; k < len(seen) && (k == startedAt || seen[k].event.Event == engine.EvPolicyRepinned); k++ {
				if lineOfTheStore(seen[k].line, run, storeTable) == "" {
					t.Errorf("the owner's line was the resumed process's when its %s row was written: the line came before the row", seen[k].event.Event)
				}
			}
			if why := lineOfTheStore(seen[finishedAt].line, run, storeTable); why != "" {
				t.Errorf("while the resumed run ran, %s: its process did not bind the owner after its started row", why)
			}
			// the first started row keeps the first process's ceiling, and nothing but the process wrote the run's rows
			firsts := 0
			for _, e := range events {
				if e.Kind == engine.KindRun && e.Event == "started" {
					if firsts == 0 && num(e.Data, "token_budget") != 100 {
						t.Errorf("the first started row's ceiling %v", e.Data["token_budget"])
					}
					firsts++
				}
			}
			if firsts != 2 {
				t.Errorf("%d started rows, want 2", firsts)
			}
			if n := roleStoreChangedRows(t, w, run); n != 0 {
				t.Errorf("%d role_store_changed rows", n)
			}
			if got := bound.Lookup(owner); got.State == bound.Orchestrator && got.Line.RunID == run {
				t.Errorf("the owner's line stays open after the run ended: %+v", got)
			}
		})
	}
}
