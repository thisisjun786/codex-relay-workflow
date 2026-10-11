package engine

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// The rows that announce a request are in the journal before the backend is asked to send it: when the fake backend is asked to
// create the thread, the call's prepared and thread_requested rows are there; when it is asked to run the turn, the
// turn_requested row is too (and the engine, which has no durable journal to fail here, sends both).
func TestIntentRowCommittedBeforeSend(t *testing.T) {
	h := newHarness(t)
	var atThread, atTurn []string
	h.fake.StartHook = func(spec ThreadSpec) (ThreadInfo, error) {
		atThread = kindsOf(CallEvents(h.journal, spec.CallID))
		return ThreadInfo{ID: "thr_" + spec.CallID, Pair: spec.Pair}, nil
	}
	h.fake.Handler = func(r FakeRequest) FakeReply {
		atTurn = kindsOf(CallEvents(h.journal, r.CallID))
		return FakeReply{}
	}
	h.run(`const a = await agent.call('build', { agentType: 'executor', node: 'n1', label: 'a' }); return a.status`)
	if !slices.Equal(atThread, []string{EvPrepared, EvThreadRequested}) {
		t.Errorf("at the thread request the journal holds %v", atThread)
	}
	if !slices.Equal(atTurn, []string{EvPrepared, EvThreadRequested, EvThreadStarted, EvTurnRequested}) {
		t.Errorf("at the turn request the journal holds %v", atTurn)
	}
}

// failingJournal stores nothing from its failure on and reports it, as a persistent journal whose store went away.
type failingJournal struct {
	*MemoryJournal
	failAt string
	failed bool
}

func (f *failingJournal) Append(e Event) Event {
	if e.Kind == KindCall && e.Event == f.failAt {
		f.failed = true
	}
	return f.MemoryJournal.Append(e)
}

func (f *failingJournal) Err() error {
	if f.failed {
		return context.DeadlineExceeded
	}
	return nil
}

// A journal that could not store an intent row stops the call before the request: the fake backend is never asked, the run ends
// with the journal's error, and the call writes no row after the one that failed.
func TestIntentRowThatCannotBeStoredIsNeverSent(t *testing.T) {
	for _, intent := range []string{EvThreadRequested, EvTurnRequested} {
		t.Run(intent, func(t *testing.T) {
			h := newHarness(t)
			j := &failingJournal{MemoryJournal: h.journal, failAt: intent}
			h.cfg.Journal = j
			res := h.run(`const a = await agent.call('build', { agentType: 'executor', node: 'n1', label: 'a' }); return a.status`)
			if res.Status != RunScriptError || res.ErrorKind != ErrKindEngine || !strings.Contains(res.Error, "journal cannot be written") {
				t.Errorf("%s %s %q", res.Status, res.ErrorKind, res.Error)
			}
			for _, r := range h.fake.Requests() {
				if intent == EvThreadRequested || r.Kind == "turn" {
					t.Errorf("a request was sent whose intent could not be stored: %+v", r)
				}
			}
			if rows := kindsOf(CallEvents(h.journal, res.Calls[0].CallID)); rows[len(rows)-1] != intent {
				t.Errorf("rows %v: nothing is written after the intent that could not be stored", rows)
			}
		})
	}
}

func kindsOf(rows []Event) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Event
	}
	return out
}

// The CallResult of a call is restored from the call's rows alone, for every way a call ends.
func TestReplayRestoresCallResult(t *testing.T) {
	h := newHarness(t)
	garbage := ""
	h.fake.Handler = func(r FakeRequest) FakeReply {
		if strings.Contains(r.Prompt, "garbage") {
			garbage = r.CallID
		}
		switch {
		case strings.Contains(r.Prompt, "overloaded"):
			return FakeReply{Fail: &TurnFailure{CodexErrorInfo: "serverOverloaded"}}
		case strings.Contains(r.Prompt, "refuse"):
			return FakeReply{Fail: &TurnFailure{CodexErrorInfo: "unauthorized"}}
		case r.CallID == garbage:
			return FakeReply{Text: "not json"}
		}
		return FakeReply{}
	}
	res := h.run(`const S = { type: 'object', properties: { verdict: { type: 'string' } }, required: ['verdict'], additionalProperties: false }
const a = await agent.call('build', { agentType: 'executor', node: 'n1', schema: S, label: 'a' })
const b = await agent.call('garbage', { agentType: 'executor', node: 'n2', schema: S, label: 'b' })
const c = await agent.call('overloaded', { agentType: 'executor', node: 'n3', label: 'c' })
const d = await agent.call('refuse', { agentType: 'reviewer', label: 'd' })
const e = await agent.call('continue', { resume: a, label: 'e' })
return [a.status, b.status, c.status, d.status, e.status]`)
	var statuses []string
	if err := json.Unmarshal(res.Value, &statuses); err != nil || !slices.Equal(statuses, []string{"completed", "null_model", "held", "refused", "completed"}) {
		t.Fatalf("%s %v (%s)", res.Value, err, res.Error)
	}
	for _, want := range res.Calls {
		got, ok := RestoreCallResult(res.RunID, want.CallID, CallEvents(h.journal, want.CallID), false)
		if !ok {
			t.Fatalf("%s: not restored", want.Label)
		}
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		if string(g) != string(w) {
			t.Errorf("%s restored differently:\n got  %s\n want %s", want.Label, g, w)
		}
	}
	// rows that hold no terminal event restore nothing
	if _, ok := RestoreCallResult(res.RunID, "c_none", nil, false); ok {
		t.Error("a call without rows has no result")
	}
	rows := CallEvents(h.journal, res.Calls[0].CallID)
	if _, ok := RestoreCallResult(res.RunID, res.Calls[0].CallID, rows[:len(rows)-1], false); ok {
		t.Error("a call without a terminal row has no result")
	}
}

// A call that only records a fact (crw.tierStep) is recorded once: a resumed run that reaches it again finds its row and writes
// no second one, and the step decision is the recorded one.
func TestRecordCallProofsNeverResend(t *testing.T) {
	h := newHarness(t)
	body := `const s = crw.tierStep('n1', { reason: 'null_model twice' }); return await s`
	first := h.run(body)
	var stepID string
	var step struct{ ID string }
	if err := json.Unmarshal(first.Value, &step); err != nil || step.ID == "" {
		t.Fatalf("%s %v", first.Value, err)
	}
	stepID = step.ID
	count := func() int {
		n := 0
		for _, ev := range h.journal.Events() {
			if ev.Kind == KindCall && ev.Op == "crw.tierStep" && ev.Event == EvCompleted {
				n++
			}
		}
		return n
	}
	if count() != 1 {
		t.Fatalf("%d rows", count())
	}
	for i := 0; i < 2; i++ {
		h.cfg.Resume = &ResumeOptions{}
		prog, err := script.Load(script.Source{Name: "t.js", Text: []byte(header + body)})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		res, err := Run(ctx, prog, h.cfg)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(res.Value, &step); err != nil || step.ID != stepID {
			t.Errorf("resume %d: the step decision is %q, recorded %q", i, step.ID, stepID)
		}
		if n := count(); n != 1 {
			t.Errorf("resume %d: %d tierStep rows: a recorded call is recorded again", i, n)
		}
	}
	// a run that does not resume records its calls again: nothing is replayed without Resume
	h.cfg.Resume = nil
	h.run(body)
	if n := count(); n != 2 {
		t.Errorf("a run that resumes nothing: %d rows", n)
	}
}

// The rows of a call that reopened after a terminal row restore no result: the terminal row belongs to an earlier attempt of
// the call id, and the newest prepared (or recovery_cleared, or intent) row says the call is open again.
func TestReplayRestoresNothingFromAReopenedCall(t *testing.T) {
	row := func(seq int, ev string, data map[string]any) Event {
		return Event{Seq: seq, RunID: "r", Kind: KindCall, CallID: "c_1", Path: "root", Position: 1, Op: "agent", Event: ev, Data: data}
	}
	first := []Event{
		row(1, EvPrepared, map[string]any{"role": "executor"}), row(2, EvThreadRequested, nil), row(3, EvThreadStarted, map[string]any{"thread_id": "t1"}),
		row(4, EvTurnRequested, nil), row(5, EvTurnStarted, map[string]any{"turn_id": "u1"}),
		row(6, EvCompleted, map[string]any{"result": `"old"`}),
	}
	if r, ok := RestoreCallResult("r", "c_1", first, false); !ok || r.Status != StatusCompleted || string(r.Output) != `"old"` {
		t.Fatalf("a completed call restores its result: %+v %v", r, ok)
	}
	cases := map[string][]Event{
		"a new attempt prepared":    append(slices.Clone(first), row(7, EvPrepared, map[string]any{"role": "executor"}), row(8, EvThreadRequested, nil), row(9, EvThreadStarted, map[string]any{"thread_id": "t2"})),
		"only the new prepared row": append(slices.Clone(first), row(7, EvPrepared, map[string]any{"role": "executor"})),
		"a recovered unknown outcome": {
			row(1, EvPrepared, nil), row(2, EvThreadRequested, nil), row(3, EvOutcomeUnknown, map[string]any{"reason": ReasonDispatchUnknown}),
			row(4, EvRecoveryCleared, map[string]any{"mode": "adopt", "thread_id": "t1"}), row(5, EvThreadStarted, map[string]any{"thread_id": "t1", "recovered": true}),
		},
	}
	for name, rows := range cases {
		if r, ok := RestoreCallResult("r", "c_1", rows, false); ok {
			t.Errorf("%s: restored %s with the sequence numbers %d..%d", name, r.Status, r.Meta.StartSeq, r.Meta.EndSeq)
		}
	}
	// the new attempt ends: its own terminal row is the result, with its own metadata
	again := append(slices.Clone(first), row(7, EvPrepared, map[string]any{"role": "executor"}), row(8, EvThreadStarted, map[string]any{"thread_id": "t2"}),
		row(9, EvCompleted, map[string]any{"result": `"new"`}))
	r, ok := RestoreCallResult("r", "c_1", again, false)
	if !ok || string(r.Output) != `"new"` || r.Meta.ThreadID != "t2" || r.Meta.StartSeq != 7 || r.Meta.EndSeq != 9 {
		t.Errorf("%+v %v", r, ok)
	}
}

// A completed agent call at a position does not stand for a tierStep a resumed script records at the same position: the
// decision of the tierStep is journaled.
func TestTierStepIsRecordedWhereAnAgentCallCompleted(t *testing.T) {
	h := newHarness(t)
	h.run(`const a = await agent.call('build', { agentType: 'executor', node: 'n1', label: 'a' }); return a.status`)
	h.cfg.Resume = &ResumeOptions{}
	res := h.run(`const s = crw.tierStep('n1', { reason: 'null_model twice' }); return await s`)
	var step struct{ ID string }
	if err := json.Unmarshal(res.Value, &step); err != nil || step.ID == "" {
		t.Fatalf("%s %v (%s)", res.Value, err, res.Error)
	}
	var rows []Event
	for _, ev := range h.journal.Events() {
		if ev.Kind == KindCall && ev.Op == "crw.tierStep" && ev.Event == EvCompleted {
			rows = append(rows, ev)
		}
	}
	if len(rows) != 1 || rows[0].Data["step_decision"] != step.ID {
		t.Errorf("the step %q has no journal row: %+v", step.ID, rows)
	}
}

// finishFailJournal stores everything but the run's finished row, and reports that failure as a persistent journal does.
type finishFailJournal struct {
	*MemoryJournal
	failed bool
}

func (f *finishFailJournal) Append(e Event) Event {
	if e.Kind == KindRun && e.Event == "finished" {
		f.failed = true
	}
	return f.MemoryJournal.Append(e)
}

func (f *finishFailJournal) Err() error {
	if f.failed {
		return context.DeadlineExceeded
	}
	return nil
}

// A journal that fails on the run's last row ends the run with its error: the caller is not told the run completed.
func TestRunEndsWithTheErrorOfItsFinalWrite(t *testing.T) {
	h := newHarness(t)
	h.cfg.Journal = &finishFailJournal{MemoryJournal: h.journal}
	res := h.run(`const a = await agent.call('build', { agentType: 'executor', node: 'n1', label: 'a' }); return a.status`)
	if res.Status != RunScriptError || res.ErrorKind != ErrKindEngine || !strings.Contains(res.Error, "journal cannot be written") {
		t.Errorf("%s %s %q", res.Status, res.ErrorKind, res.Error)
	}
}

// snapFailBackend is the fake backend with a placer whose snapshot always fails (the object store of the worktree is full).
type snapFailBackend struct {
	*Fake
	dir string
}

func (p *snapFailBackend) Place(req PlaceRequest) (Placement, error) {
	if req.Family != "executor" || req.Continues != nil {
		return Placement{Kind: PlaceContinued}, nil
	}
	return Placement{Kind: PlaceFresh, Attempt: 1, Path: p.dir, Start: "0123456789abcdef0123456789abcdef01234567"}, nil
}
func (p *snapFailBackend) Establish(context.Context, PlaceRequest, Placement) error { return nil }
func (p *snapFailBackend) Recheck(context.Context, Placement) error                 { return nil }
func (p *snapFailBackend) Snapshot(context.Context, string) (string, error) {
	return "", errors.New("write-tree: no space left on device")
}

// An executor whose pending tree cannot be captured does not end completed: the script sees a held call that names the
// worktree failure, and the row carries the error, instead of a success no effect proof can stand on.
func TestExecutorWhosePendingTreeCannotBeCapturedIsHeld(t *testing.T) {
	h := newHarness(t)
	h.cfg.Backend = &snapFailBackend{Fake: h.fake, dir: t.TempDir()}
	res := h.run(`const b = await agent.call('build', { agentType: 'executor', node: 'n1', label: 'build' }); return [b.status, b.reason]`)
	var got []string
	if err := json.Unmarshal(res.Value, &got); err != nil || !slices.Equal(got, []string{"held", "worktree_failed"}) {
		t.Fatalf("%s %v (%s)", res.Value, err, res.Error)
	}
	rows := h.callRows("build")
	last := rows[len(rows)-1]
	if last.Event != EvHeld || !strings.Contains(rowStr(last.Data, "pending_tree_error"), "no space left") {
		t.Errorf("%s %v", last.Event, last.Data)
	}
	if _, has := last.Data["pending_tree"]; has {
		t.Errorf("a held row records no pending tree: %v", last.Data)
	}
}
