package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// recordOp is an op that writes the row the signal path reads: its arguments, as the fields a real op writes.
type recordOp struct {
	fields func(args []any) map[string]any
}

func (o recordOp) Run(_ context.Context, req OpRequest) (OpResult, error) {
	var args []any
	_ = json.Unmarshal(req.Args, &args)
	return OpResult{Result: json.RawMessage(`{"ok":true}`), Data: o.fields(args)}, nil
}
func (recordOp) Proven(context.Context, []Event, Event) (bool, string, error) { return true, "", nil }

func signalOps() map[string]Op {
	return map[string]Op{
		"signalBind": recordOp{func(a []any) map[string]any {
			return map[string]any{"milestone": a[0], "k": a[1], "prev": a[2], "members": a[3]}
		}},
		"release": recordOp{func(a []any) map[string]any {
			return map[string]any{"plan": a[0], "node": a[1], "milestone": a[2], "released": true, "revision": "r1"}
		}},
		"designLedger": recordOp{func(a []any) map[string]any {
			return map[string]any{"milestone": a[0], "revision": "r1", "signal": 2}
		}},
	}
}

const signalScript = `const r = await agent.call(PROMPT, { agentType: 'executor', node: 'A', label: 'build' })
const facts = crw.callFacts(r)
await crw.signalBind('M', 1, '', [{ kind: 'receipt', call: facts.callId, node: 'A', digest: facts.digest }])
await crw.release('plan', 'A', 'M')
await crw.designLedger('M', {})
return { facts, recorded: crw.recordedSignal('M', 1), none: crw.recordedSignal('M', 2), release: crw.priorRelease('A'), nobody: crw.priorRelease('Z') }`

type signalFactsResult struct {
	Facts struct {
		CallID string `json:"callId"`
		Status string `json:"status"`
		Digest string `json:"digest"`
		Live   bool   `json:"live"`
		Seq    int    `json:"seq"`
	} `json:"facts"`
	Recorded *struct {
		K       int              `json:"k"`
		Members []map[string]any `json:"members"`
	} `json:"recorded"`
	None    any `json:"none"`
	Release *struct {
		Node     string `json:"node"`
		Revision string `json:"revision"`
		Signal   int    `json:"signal"`
	} `json:"release"`
	Nobody any `json:"nobody"`
}

func runSignalScript(h *harness, prompt string) signalFactsResult {
	h.t.Helper()
	h.cfg.Ops = signalOps()
	res := h.run(`const PROMPT = ` + prompt + "\n" + signalScript)
	if res.Status != RunCompleted {
		h.t.Fatalf("run %s: %s", res.Status, res.Error)
	}
	var out signalFactsResult
	if err := json.Unmarshal(res.Value, &out); err != nil {
		h.t.Fatalf("%s: %v", res.Value, err)
	}
	return out
}

// crw.callFacts, crw.recordedSignal and crw.priorRelease read the journal and how this process came to a result; they take no
// position and write no row. A run that does not resume has no earlier process: every result is live, and nothing was recorded.
func TestCallFactsOfARunAreLiveAndRecordNothing(t *testing.T) {
	h := newHarness(t)
	h.cfg.Ops = signalOps()
	before := len(h.journal.Events())
	got := runSignalScript(h, `'build it'`)
	if !got.Facts.Live || got.Facts.Status != StatusCompleted || got.Facts.Digest == "" || got.Facts.Seq == 0 {
		t.Errorf("facts %+v", got.Facts)
	}
	if got.Recorded != nil || got.None != nil || got.Release != nil || got.Nobody != nil {
		t.Errorf("a run that does not resume read %+v / %v / %+v", got.Recorded, got.None, got.Release)
	}
	// the three reads wrote no row and took no position: the calls of the run are the agent call and the three ops
	ops := 0
	for _, ev := range h.journal.Events()[before:] {
		if ev.Kind == KindCall && ev.Event == EvCompleted && ev.Op != "agent" {
			ops++
		}
	}
	if ops != 3 {
		t.Errorf("%d op rows, want the three ops of the script", ops)
	}
}

// A resume replays what it can: a call replayed from the journal answers the digest and the place it had, and is not live; a call
// whose input changed runs again and is live, with the digest of its new answer. The signal and the release the earlier process
// recorded are read while the journal stands on them, and not once a resume ended their rows.
func TestCallFactsAndRecordedRowsOfAResume(t *testing.T) {
	h := newHarness(t)
	first := runSignalScript(h, `'build it'`)

	h.cfg.Resume = &ResumeOptions{}
	same := runSignalScript(h, `'build it'`)
	if same.Facts.Live || same.Facts.Digest != first.Facts.Digest || same.Facts.Seq != first.Facts.Seq {
		t.Errorf("a replayed call answers %+v, the run's was %+v", same.Facts, first.Facts)
	}
	if same.Recorded == nil || same.Recorded.K != 1 || len(same.Recorded.Members) != 1 || same.Recorded.Members[0]["call"] != first.Facts.CallID || same.Recorded.Members[0]["digest"] != first.Facts.Digest {
		t.Errorf("the recorded signal %+v", same.Recorded)
	}
	if same.None != nil {
		t.Errorf("signal 2 was never recorded: %v", same.None)
	}
	if same.Release == nil || same.Release.Node != "A" || same.Release.Revision != "r1" || same.Release.Signal != 2 || same.Nobody != nil {
		t.Errorf("the prior release %+v (of Z: %v)", same.Release, same.Nobody)
	}

	// another prompt is another input: the call runs again, is live, and the rows of the positions after it were ended by the resume
	h.fake.Handler = func(FakeRequest) FakeReply {
		return FakeReply{Text: `{"outcome":"ready_for_review","files":[],"verification":[],"summary":"s","gap":null,"research_question":null,"plan_mismatch":"changed"}`}
	}
	changed := runSignalScript(h, `'build it another way'`)
	if !changed.Facts.Live || changed.Facts.Digest == first.Facts.Digest || changed.Facts.CallID != first.Facts.CallID {
		t.Errorf("a call that ran again answers %+v, the first run's was %+v", changed.Facts, first.Facts)
	}
}

// laneScript is two lanes on paths of their own: A's RECEIPT makes a signal and the revision that signal made, B is released under that
// revision (the release names it), as the dag does for a signal path and the nodes after it.
const laneScript = `const out = await parallel([
  async () => {
    const r = await agent.call(PROMPT_A, { agentType: 'executor', node: 'A', label: 'build-A' })
    const f = crw.callFacts(r)
    await crw.signalBind('M', 1, '', [{ kind: 'receipt', call: f.callId, node: 'A', digest: f.digest }])
    await crw.designLedger('M', {})
    return f.digest
  },
  async () => {
    await agent.call('build B', { agentType: 'executor', node: 'B', label: 'build-B' })
    await crw.release('plan', 'B', 'M')
    return 'B'
  },
], { keys: ['A', 'B'] })
return { out, recorded: crw.recordedSignal('M', 1), release: crw.priorRelease('B') }`

type laneRun struct {
	Out      []string `json:"out"`
	Recorded *struct {
		K       int              `json:"k"`
		Members []map[string]any `json:"members"`
	} `json:"recorded"`
	Release *struct {
		Revision string `json:"revision"`
		Signal   int    `json:"signal"`
	} `json:"release"`
}

func runLanes(h *harness, promptA string) (laneRun, *Result) {
	h.t.Helper()
	h.cfg.Ops = signalOps()
	res := h.run(`const PROMPT_A = ` + promptA + "\n" + laneScript)
	if res.Status != RunCompleted {
		h.t.Fatalf("run %s: %s", res.Status, res.Error)
	}
	var out laneRun
	if err := json.Unmarshal(res.Value, &out); err != nil {
		h.t.Fatalf("%s: %v", res.Value, err)
	}
	return out, res
}

func turnLabels(h *harness, mark int) []string {
	var out []string
	for _, r := range h.fake.Requests()[mark:] {
		if r.Kind == "turn" {
			out = append(out, r.Prompt)
		}
	}
	return out
}

// A run that completed is resumed unchanged: the signal and the revision it made replay, no turn is sent, and what the engine reads for the
// signal path is what the run recorded.
func TestResumeAfterCompletedRunWithInFlightReplanZeroLive(t *testing.T) {
	h := newHarness(t)
	first, _ := runLanes(h, `'build A'`)
	mark := len(h.fake.Requests())
	h.cfg.Resume = &ResumeOptions{}
	again, _ := runLanes(h, `'build A'`)
	if live := turnLabels(h, mark); len(live) != 0 {
		t.Errorf("the resume sent turns %v", live)
	}
	if again.Recorded == nil || again.Recorded.K != 1 || again.Out[0] != first.Out[0] || again.Release == nil || again.Release.Signal != 2 {
		t.Errorf("after the resume: %+v (first %+v)", again, first)
	}
	for _, ev := range h.journal.Events() {
		if ev.Kind == KindCall && ev.Event == EvSuperseded {
			t.Errorf("a call was superseded: %s", ev.Op)
		}
	}
}

// The RECEIPT that made a signal runs again with another prompt: the signal and the revision it made are the suffix of A's path, their rows
// are ended superseded and are not read as recorded any more, and the lane of B - another path - replays; the release it made still names
// the revision, and the signal that made it.
func TestSupersededTriggerSupersedesSignalAndRevision(t *testing.T) {
	h := newHarness(t)
	runLanes(h, `'build A'`)
	mark := len(h.fake.Requests())
	h.cfg.Resume = &ResumeOptions{}
	got, _ := runLanes(h, `'build A another way'`)
	if live := turnLabels(h, mark); len(live) != 1 || live[0] != "build A another way" {
		t.Errorf("live turns %v: only the lane of A, with the new prompt, runs again", live)
	}
	superseded := map[string]bool{}
	for _, ev := range h.journal.Events() {
		if ev.Kind == KindCall && ev.Event == EvSuperseded {
			superseded[ev.Op] = true
		}
	}
	// the new signal and revision were made by the resume itself (the same positions), the old rows were ended
	if !superseded["crw.signalBind"] || !superseded["crw.designLedger"] {
		t.Errorf("superseded rows of %v, want the signal and the revision of the earlier process", superseded)
	}
	if got.Recorded == nil || got.Release == nil || got.Release.Signal != 2 {
		t.Errorf("after the resume: %+v", got)
	}
	// the earlier signal is not read as recorded once only its superseded row is there: a script that never ran the position again sees none
	h2 := newHarness(t)
	runLanes(h2, `'build A'`)
	h2.cfg.Resume = &ResumeOptions{}
	h2.cfg.Ops = signalOps()
	res := h2.run(`const out = await parallel([async () => agent.call('build A another way', { agentType: 'executor', node: 'A', label: 'build-A' })], { keys: ['A'] })
return { recorded: crw.recordedSignal('M', 1), release: crw.priorRelease('B') }`)
	var seen laneRun
	_ = json.Unmarshal(res.Value, &seen)
	if seen.Recorded != nil {
		t.Errorf("a signal whose row a resume ended is read as recorded: %+v", seen.Recorded)
	}
	if seen.Release == nil || seen.Release.Signal != 2 {
		t.Errorf("the release of B is a record of the revision whether or not it stands: %+v", seen.Release)
	}
}

// The signals a resume replays are the ones the rows name, in the order the rows name them, whatever order the calls finish in now.
func TestSignalMembershipFromJournal(t *testing.T) {
	h := newHarness(t)
	h.cfg.Ops = signalOps()
	script := `const out = await parallel([
  async () => { const r = await agent.call('A', { agentType: 'executor', node: 'A', label: 'a' }); return crw.callFacts(r) },
  async () => { const r = await agent.call('B', { agentType: 'executor', node: 'B', label: 'b' }); return crw.callFacts(r) },
], { keys: ['A', 'B'] })
const bySeq = out.slice().sort((x, y) => x.seq - y.seq)
await crw.signalBind('M', 1, '', [{ kind: 'receipt', call: bySeq[0].callId, node: bySeq[0].callId === out[0].callId ? 'A' : 'B', digest: bySeq[0].digest }])
await crw.signalBind('M', 2, 'x', [{ kind: 'receipt', call: bySeq[1].callId, node: bySeq[1].callId === out[0].callId ? 'A' : 'B', digest: bySeq[1].digest }])
return { order: bySeq.map((f) => f.callId), s1: crw.recordedSignal('M', 1), s2: crw.recordedSignal('M', 2) }`
	res := h.run(script)
	var first struct {
		Order []string `json:"order"`
	}
	if res.Status != RunCompleted {
		t.Fatalf("run %s: %s", res.Status, res.Error)
	}
	_ = json.Unmarshal(res.Value, &first)
	h.cfg.Resume = &ResumeOptions{}
	// the resume delivers the lanes' results in an order of its own; the recorded signals do not depend on it
	h.cfg.Ops = signalOps()
	res2 := h.run(script)
	var second struct {
		S1 *struct {
			Members []map[string]any `json:"members"`
		} `json:"s1"`
		S2 *struct {
			Members []map[string]any `json:"members"`
		} `json:"s2"`
	}
	_ = json.Unmarshal(res2.Value, &second)
	if second.S1 == nil || second.S2 == nil || len(second.S1.Members) != 1 || len(second.S2.Members) != 1 {
		t.Fatalf("recorded signals %+v / %+v", second.S1, second.S2)
	}
	if second.S1.Members[0]["call"] != first.Order[0] || second.S2.Members[0]["call"] != first.Order[1] || first.Order[0] == first.Order[1] {
		t.Errorf("signal 1 was made of %v and signal 2 of %v, the first run's order of the journal was %v", second.S1.Members, second.S2.Members, first.Order)
	}
}
