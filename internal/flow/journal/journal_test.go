package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

var ctx = context.Background()

const (
	runID  = "run-j1"
	header = "export const meta = { name: 't' }\n"
	schema = `{ type: 'object', properties: { verdict: { type: 'string' } }, required: ['verdict'], additionalProperties: false }`
)

var identity = Identity{PlanID: "plan-1", PlanRevision: 3, ActorTaskID: "task-orchestrator", EpochNonce: "nonce-1"}

var fixedNow = func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }

// newStore opens a relay store in a temporary directory.
func newStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	return openStore(t, path), path
}

func openStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newJournal(t *testing.T, st *store.Store) *Journal {
	t.Helper()
	j, err := Open(ctx, st, runID, identity, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// run executes the script body as run runID on j.
func run(t *testing.T, j engine.Journal, backend engine.Backend, resume *engine.ResumeOptions, body string) *engine.Result {
	t.Helper()
	prog, err := script.Load(script.Source{Name: "t.js", Text: []byte(header + body)})
	if err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	res, err := engine.Run(rctx, prog, engine.Config{Backend: backend, Policy: engine.FakePolicy(), Journal: j, RunID: runID, Now: fixedNow, Resume: resume})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// dbEvents are the events of a call's flow_calls rows, in order, read straight from the database.
func dbEvents(t *testing.T, db *sql.DB, call string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT event FROM flow_calls WHERE call_id = ? ORDER BY event_seq`, call)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ev string
		if err := rows.Scan(&ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// An intent row is committed before the request it announces is sent: the store is closed just as the fake backend is asked
// to create the thread, and the prepared and thread_requested rows are in the store, committed by transactions of their own.
// The rows that were still waiting for a batch are lost with the process, and the next intent, which cannot be committed, is
// never sent: the run ends with the journal's error and the fake sees no turn.
func TestIntentRowCommittedBeforeSend(t *testing.T) {
	st, path := newStore(t)
	j := newJournal(t, st)
	fake := engine.NewFake()
	var callID string
	var atSend []string
	fake.StartHook = func(spec engine.ThreadSpec) (engine.ThreadInfo, error) {
		callID = spec.CallID
		atSend = dbEvents(t, st.DB, spec.CallID) // what a second reader sees at the moment of the send
		_ = st.Close()                           // the process dies here
		return engine.ThreadInfo{ID: "thr_" + spec.CallID, Pair: spec.Pair}, nil
	}
	res := run(t, j, fake, nil, `const a = await agent.call('build', { agentType: 'executor', node: 'n1', label: 'a' }); return a.status`)
	if !slices.Equal(atSend, []string{"prepared", "thread_requested"}) {
		t.Errorf("at the send the store holds %v, want the two intents", atSend)
	}
	if res.Status != engine.RunScriptError || res.ErrorKind != engine.ErrKindEngine || !strings.Contains(res.Error, "journal") {
		t.Errorf("the run: %s %s %q", res.Status, res.ErrorKind, res.Error)
	}
	if j.Err() == nil {
		t.Error("the journal reports no failure")
	}
	for _, r := range fake.Requests() {
		if r.Kind == "turn" {
			t.Errorf("a turn was sent although its intent could not be stored: %+v", r)
		}
	}
	// the restarted process finds the intents, and only the intents
	again := openStore(t, path)
	rows, hasRun, err := Load(ctx, again, runID)
	if err != nil || !hasRun {
		t.Fatalf("%v %v", err, hasRun)
	}
	var got []string
	for _, e := range rows {
		if e.CallID == callID {
			got = append(got, e.Event)
		}
	}
	if !slices.Equal(got, []string{engine.EvPrepared, engine.EvThreadRequested}) {
		t.Errorf("the restarted process reads %v", got)
	}
}

// The rows that are not intents wait for the next intent or the end of the call and are committed then, in order: at the moment
// a turn is sent, the thread_started row before its intent is stored and nothing after it is; at the end of the call all are.
func TestResultRowsAreBatchedBehindTheNextIntent(t *testing.T) {
	st, _ := newStore(t)
	j := newJournal(t, st)
	fake := engine.NewFake()
	var atTurn []string
	fake.Handler = func(r engine.FakeRequest) engine.FakeReply {
		atTurn = dbEvents(t, st.DB, r.CallID)
		return engine.FakeReply{}
	}
	res := run(t, j, fake, nil, `const a = await agent.call('build', { agentType: 'executor', node: 'n1', label: 'a' }); return a.callId`)
	var callID string
	if err := json.Unmarshal(res.Value, &callID); err != nil {
		t.Fatal(err)
	}
	if want := []string{"prepared", "thread_requested", "thread_started", "turn_requested"}; !slices.Equal(atTurn, want) {
		t.Errorf("at the turn the store holds %v, want %v", atTurn, want)
	}
	if got, want := dbEvents(t, st.DB, callID), []string{"prepared", "thread_requested", "thread_started", "turn_requested", "turn_started", "completed"}; !slices.Equal(got, want) {
		t.Errorf("at the end %v, want %v", got, want)
	}
	if err := j.Err(); err != nil {
		t.Error(err)
	}
}

// The persisted journal writes the events the in-memory journal writes, in the same order and with the same sequence numbers,
// and a journal opened again on the store reads exactly what the first one held.
func TestPersistedJournalMatchesMemoryJournal(t *testing.T) {
	body := `const S = ` + schema + `
phase('build')
log('building')
const a = await agent.call('build', { agentType: 'executor', node: 'n1', schema: S, label: 'a' })
const b = await agent.call('review', { agentType: 'reviewer', schema: S, head: 'abc1234', label: 'b' })
const c = await agent.call('flaky', { agentType: 'executor', node: 'n2', label: 'c' })
const t = crw.clock()
return [a.status, b.status, c.status, typeof t]`
	backend := func() *engine.Fake {
		f := engine.NewFake()
		f.Handler = func(r engine.FakeRequest) engine.FakeReply {
			if strings.Contains(r.Prompt, "flaky") {
				return engine.FakeReply{Fail: &engine.TurnFailure{CodexErrorInfo: "serverOverloaded", Message: "busy"}}
			}
			return engine.FakeReply{}
		}
		return f
	}
	mem := engine.NewMemoryJournal()
	memRes := run(t, mem, backend(), nil, body)
	st, path := newStore(t)
	j := newJournal(t, st)
	res := run(t, j, backend(), nil, body)
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if asJSON(t, res) != asJSON(t, memRes) {
		t.Errorf("the results differ:\n%s\n%s", asJSON(t, res), asJSON(t, memRes))
	}
	if got, want := asJSON(t, j.Events()), asJSON(t, mem.Events()); got != want {
		t.Errorf("the events differ:\n%s\n%s", got, want)
	}
	if n := len(j.Events()); n < 20 {
		t.Errorf("only %d rows: the script did not run", n)
	}
	reopened := openStore(t, path)
	loaded, err := Open(ctx, reopened, runID, identity, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := asJSON(t, loaded.Events()), asJSON(t, j.Events()); got != want {
		t.Errorf("the journal read back differs from the one written:\n%s\n%s", got, want)
	}
	// the sequence continues where the stored one ended
	next := loaded.Append(engine.Event{RunID: runID, Kind: engine.KindFlow, Event: "log", Data: map[string]any{"message": "again"}})
	if want := j.Events()[len(j.Events())-1].Seq + 1; next.Seq != want {
		t.Errorf("the next row has sequence %d, want %d", next.Seq, want)
	}
}

// A row the relay reads for the acceptance of an agent node is readable without opening detail_json: the kind, the role, the
// plan, the node, the head a reviewer read, the thread and turn a call ended on, and the canonical result bytes.
func TestStoredRowsCarryTheColumnsTheRelayReads(t *testing.T) {
	st, _ := newStore(t)
	j := newJournal(t, st)
	res := run(t, j, engine.NewFake(), nil, `const S = `+schema+`
const r = await agent.call('review', { agentType: 'reviewer', node: 'n1', schema: S, head: 'abc1234', label: 'r' }); return r.callId`)
	var callID string
	_ = json.Unmarshal(res.Value, &callID)
	var kind, role, plan, node, commit, thread, turn, resultText, detail string
	err := st.DB.QueryRowContext(ctx, `SELECT kind, role, plan_id, node_id, commit_sha, thread_id, turn_id, COALESCE(CAST(result_bytes AS TEXT), ''), detail_json
 FROM flow_calls WHERE kind = 'agent' AND event = 'completed' AND role IN ('reviewer','reviewer-final') AND plan_id = ? AND node_id = ? AND call_id = ?`, "plan-1", "n1", callID).
		Scan(&kind, &role, &plan, &node, &commit, &thread, &turn, &resultText, &detail)
	if err != nil {
		t.Fatal(err)
	}
	if commit != "abc1234" || !strings.HasPrefix(thread, "thr_") || !strings.Contains(turn, "/turn-") || !strings.Contains(resultText, "verdict") {
		t.Errorf("commit %q thread %q turn %q result %q", commit, thread, turn, resultText)
	}
	if strings.Contains(detail, "verdict") {
		t.Errorf("the result is stored once, as bytes: %s", detail)
	}
	var pair struct{ Model, Effort string }
	var cfg string
	if err := st.DB.QueryRowContext(ctx, `SELECT detail_json FROM flow_calls WHERE call_id = ? AND event = 'thread_started'`, callID).Scan(&cfg); err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	_ = json.Unmarshal([]byte(cfg), &d)
	pair.Model, _ = d["model"].(string)
	if pair.Model != "gpt-fake-sol" {
		t.Errorf("the configured pair is read from the thread_started row's detail: %s", cfg)
	}
	var workflow, actor, planID string
	var rev int64
	if err := st.DB.QueryRowContext(ctx, `SELECT workflow, actor_task_id, plan_id, plan_revision FROM flow_runs WHERE run_id = ?`, runID).Scan(&workflow, &actor, &planID, &rev); err != nil {
		t.Fatal(err)
	}
	if workflow != "t" || actor != "task-orchestrator" || planID != "plan-1" || rev != 3 {
		t.Errorf("run record %s %s %s %d", workflow, actor, planID, rev)
	}
}

// The restored CallResult of every kind of call is the one the run handed its script, read from the call's flow_calls rows
// alone, by a process that has no host.
func TestReplayRestoresCallResult(t *testing.T) {
	st, path := newStore(t)
	j := newJournal(t, st)
	fake := engine.NewFake()
	garbage := ""
	fake.Handler = func(r engine.FakeRequest) engine.FakeReply {
		if strings.Contains(r.Prompt, "garbage") {
			garbage = r.CallID // its repair turn answers garbage too
		}
		switch {
		case strings.Contains(r.Prompt, "refuse"):
			return engine.FakeReply{Fail: &engine.TurnFailure{CodexErrorInfo: "unauthorized", Message: "no"}}
		case r.CallID == garbage:
			return engine.FakeReply{Text: "not json"}
		}
		return engine.FakeReply{}
	}
	res := run(t, j, fake, nil, `const S = `+schema+`
const a = await agent.call('build', { agentType: 'executor', node: 'n1', schema: S, label: 'a' })
const b = await agent.call('review it', { agentType: 'reviewer', schema: S, label: 'b' })
const c = await agent.call('garbage', { agentType: 'executor', node: 'n2', schema: S, label: 'c' })
const d = await agent.call('refuse', { agentType: 'executor', node: 'n3', label: 'd' })
return [a.status, b.status, c.status, d.status]`)
	if got := asJSON(t, res.Value); got != `["completed","completed","null_model","refused"]` {
		t.Fatalf("%s", got)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	// a restarted process: a new store handle, no backend
	again := openStore(t, path)
	for _, want := range res.Calls {
		got, ok, err := ReadCallResult(ctx, again, runID, want.CallID, false)
		if err != nil || !ok {
			t.Fatalf("%s: %v %v", want.Label, ok, err)
		}
		if asJSON(t, got) != asJSON(t, want) {
			t.Errorf("%s restored differently:\n got  %s\n want %s", want.Label, asJSON(t, got), asJSON(t, want))
		}
	}
	if len(res.Calls) != 4 || string(res.Calls[0].Output) == "" || res.Calls[3].Status != engine.StatusRefused {
		t.Fatalf("the script did not run as planned: %+v", res.Calls)
	}
	// a resumed run replays the calls the journal ended, from the same rows, without sending anything
	var calls atomic.Int32
	silent := engine.NewFake()
	silent.StartHook = func(engine.ThreadSpec) (engine.ThreadInfo, error) { calls.Add(1); return engine.ThreadInfo{}, nil }
	jj, err := Open(ctx, again, runID, identity, Options{})
	if err != nil {
		t.Fatal(err)
	}
	resumed := run(t, jj, silent, &engine.ResumeOptions{}, `const S = `+schema+`
const a = await agent.call('build', { agentType: 'executor', node: 'n1', schema: S, label: 'a' })
const b = await agent.call('review it', { agentType: 'reviewer', schema: S, label: 'b' })
return [a.status, b.status]`)
	if got := asJSON(t, resumed.Value); got != `["completed","completed"]` {
		t.Fatalf("%s (%s)", got, resumed.Error)
	}
	if n := calls.Load(); n != 0 || len(silent.Requests()) != 0 {
		t.Errorf("a resumed run sent %d thread and %d requests for calls the journal had ended", n, len(silent.Requests()))
	}
	for i, c := range resumed.Calls {
		if string(c.Output) != string(res.Calls[i].Output) || c.Meta.ThreadID != res.Calls[i].Meta.ThreadID {
			t.Errorf("replayed call %d: %+v, was %+v", i, c, res.Calls[i])
		}
	}
}

// A call with only a held event ends as held, with the reason of that event and no output, and is restored without the host.
func TestReplayRestoresHeldCallResult(t *testing.T) {
	t.Run("held by the host's failure", func(t *testing.T) {
		st, path := newStore(t)
		j := newJournal(t, st)
		fake := engine.NewFake()
		fake.Handler = func(engine.FakeRequest) engine.FakeReply {
			return engine.FakeReply{Fail: &engine.TurnFailure{CodexErrorInfo: "serverOverloaded", Message: "overloaded"}}
		}
		res := run(t, j, fake, nil, `const a = await agent.call('build', { agentType: 'executor', node: 'n1', label: 'a' }); return a.status`)
		_ = j.Close()
		want := res.Calls[0]
		if want.Status != engine.StatusHeld || want.Reason != "host_infra_serverOverloaded" {
			t.Fatalf("%+v", want)
		}
		got, ok, err := ReadCallResult(ctx, openStore(t, path), runID, want.CallID, false)
		if err != nil || !ok {
			t.Fatalf("%v %v", ok, err)
		}
		if got.Status != engine.StatusHeld || got.Reason != "host_infra_serverOverloaded" || string(got.Output) != "" || asJSON(t, got) != asJSON(t, want) {
			t.Errorf("restored %s, was %s", asJSON(t, got), asJSON(t, want))
		}
		if !strings.Contains(asJSON(t, got), `"output":null`) {
			t.Errorf("a held call has no output: %s", asJSON(t, got))
		}
	})
	t.Run("only prepared and held rows", func(t *testing.T) {
		st, path := newStore(t)
		j := newJournal(t, st)
		j.Append(engine.Event{RunID: runID, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "t", "script_digest": "d", "args_digest": "a"}})
		row := func(event string, data map[string]any) {
			j.Append(engine.Event{RunID: runID, Kind: engine.KindCall, CallID: "c_held", Path: "root", Position: 1, Op: "agent", Label: "h", Event: event, Data: data})
		}
		row(engine.EvPrepared, map[string]any{"role": "executor", "requested_model": "claude-fake-haiku", "requested_effort": "xhigh", "node": "n1"})
		row(engine.EvHeld, map[string]any{"reason": "host_infra_serverOverloaded", "input_state": "not_delivered", "codex_error_info": "serverOverloaded"})
		if err := j.Close(); err != nil {
			t.Fatal(err)
		}
		got, ok, err := ReadCallResult(ctx, openStore(t, path), runID, "c_held", false)
		if err != nil || !ok {
			t.Fatalf("%v %v", ok, err)
		}
		if got.Status != engine.StatusHeld || got.Reason != "host_infra_serverOverloaded" || got.Output != nil || got.Handle != "" || got.Label != "h" || got.Meta.Role != "executor" {
			t.Errorf("%s", asJSON(t, got))
		}
		if got.Meta.RequestedPair != (engine.Pair{Model: "claude-fake-haiku", Effort: "xhigh"}) {
			t.Errorf("pair %+v", got.Meta.RequestedPair)
		}
		if _, ok, _ := ReadCallResult(ctx, openStore(t, path), runID, "c_none", false); ok {
			t.Error("a call the store does not hold has no result")
		}
	})
}

// A journal that cannot store a row stops the run, and the rows of another run are refused.
func TestJournalRefusesWhatItCannotStore(t *testing.T) {
	st, _ := newStore(t)
	j := newJournal(t, st)
	j.Append(engine.Event{RunID: "run-other", Kind: engine.KindFlow, Event: "log"})
	if j.Err() == nil || !strings.Contains(j.Err().Error(), "run-other") {
		t.Errorf("a row of another run: %v", j.Err())
	}
	st2, _ := newStore(t)
	j2, err := Open(ctx, st2, runID, Identity{}, Options{}) // no actor: the run record cannot be made
	if err != nil {
		t.Fatal(err)
	}
	j2.Append(engine.Event{RunID: runID, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "t", "script_digest": "d"}})
	if j2.Err() == nil || !strings.Contains(j2.Err().Error(), "actor") {
		t.Errorf("a run without an actor: %v", j2.Err())
	}
}

// A call id that began a new attempt after a terminal row has no stored result until the new attempt ends: the stored reader
// does not return the earlier attempt's output with the new attempt's thread.
func TestStoredReaderIgnoresATerminalRowOfAnEarlierAttempt(t *testing.T) {
	st, path := newStore(t)
	j := newJournal(t, st)
	j.Append(engine.Event{RunID: runID, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "t", "script_digest": "d", "args_digest": "a"}})
	row := func(event string, data map[string]any) {
		j.Append(engine.Event{RunID: runID, Kind: engine.KindCall, CallID: "c_again", Path: "root", Position: 1, Op: "agent", Label: "x", Event: event, Data: data})
	}
	prepared := map[string]any{"role": "executor", "node": "n1"}
	row(engine.EvPrepared, prepared)
	row(engine.EvThreadRequested, nil)
	row(engine.EvThreadStarted, map[string]any{"thread_id": "thr_1"})
	row(engine.EvCompleted, map[string]any{"result": `"old"`})
	row(engine.EvPrepared, prepared)
	row(engine.EvThreadRequested, nil)
	row(engine.EvThreadStarted, map[string]any{"thread_id": "thr_2"})
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := ReadCallResult(ctx, openStore(t, path), runID, "c_again", false); err != nil || ok {
		t.Fatalf("a call that is open again has no result: %v %v %s", ok, err, asJSON(t, got))
	}
	row(engine.EvCompleted, map[string]any{"result": `"new"`})
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadCallResult(ctx, openStore(t, path), runID, "c_again", false)
	if err != nil || !ok || string(got.Output) != `"new"` || got.Meta.ThreadID != "thr_2" || got.Meta.StartSeq >= got.Meta.EndSeq {
		t.Errorf("%v %v %s", ok, err, asJSON(t, got))
	}
}

// EvidencePlacedIsAnIntent (CRW-1496): the evidence_placed row of a session call names a directory before the engine writes a flow
// evidence record there, so it is an intent: it is committed in a transaction of its own before Append returns, with the rows that
// waited before it, and a second reader of the store sees it at once.
func TestEvidencePlacedIsAnIntent(t *testing.T) {
	if !IsIntent(engine.KindCall, engine.EvEvidencePlaced) {
		t.Fatalf("%s is not an intent", engine.EvEvidencePlaced)
	}
	st, _ := newStore(t)
	j := newJournal(t, st)
	call := func(ev string, data map[string]any) engine.Event {
		return engine.Event{RunID: runID, Kind: engine.KindCall, CallID: "c-placed", Path: "root", Position: 1, Op: "agent", Label: "exe", Event: ev, Data: data}
	}
	j.Append(engine.Event{RunID: runID, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "t", "script_digest": "d", "args_digest": "a"}})
	j.Append(call(engine.EvPrepared, map[string]any{"backend": engine.BackendSession}))
	j.Append(call(engine.EvStepPublished, map[string]any{"k": 1, "attempt": 1, "marker": "c-placed/t1-a1", "evidence_assignment": "A"}))
	j.Append(call(engine.EvThreadStarted, map[string]any{"thread_id": "agent-1", "attempt": 1, "via": "session"}))
	if got := dbEvents(t, st.DB, "c-placed"); !slices.Equal(got, []string{"prepared", "step_published"}) {
		t.Fatalf("before the evidence_placed row the store holds %v", got)
	}
	j.Append(call(engine.EvEvidencePlaced, map[string]any{"evidence_assignment": "A", "evidence_cwd": "/host", "attempt": 1, "marker": "c-placed/t1-a1"}))
	if got, want := dbEvents(t, st.DB, "c-placed"), []string{"prepared", "step_published", "thread_started", "evidence_placed"}; !slices.Equal(got, want) {
		t.Errorf("when Append returned the store holds %v, want %v", got, want)
	}
	if err := j.Err(); err != nil {
		t.Error(err)
	}
}
