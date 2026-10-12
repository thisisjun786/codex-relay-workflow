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
