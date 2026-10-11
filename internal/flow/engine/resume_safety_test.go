package engine

import (
	"fmt"
	"testing"
)

// A thread whose pair read back wrong is refused, and the handle it carries cannot resume it.
func TestResumeOfPairRefusedThreadStaysRefused(t *testing.T) {
	h := newHarness(t)
	h.fake.StartHook = func(s ThreadSpec) (ThreadInfo, error) {
		return ThreadInfo{ID: "thr_wrong", Pair: Pair{s.Pair.Model, "low"}}, nil
	}
	got := execRun(h, `
const a = await agent.call('first', { agentType: 'explorer' })
const b = await agent.call('second', { resume: a })
const c = await agent.call('third', { resume: a.handle })
return [a, b, c].map((r) => ({ status: r.status, reason: r.reason }))`)
	for i, g := range got {
		if g["status"] != "refused" || g["reason"] != "pair_refused" {
			t.Errorf("call %d: %v", i, g)
		}
	}
	if n := countTurns(h.fake); n != 0 {
		t.Errorf("%d turns reached the host for a pair-refused thread", n)
	}
	for _, r := range h.fake.Requests() {
		if r.Kind == "turn" {
			t.Errorf("turn request %+v", r)
		}
	}
}

// A continuation of a continuation keeps the thread's configured pair, so the check holds along the whole chain.
func TestConfiguredPairIsKeptAlongAChain(t *testing.T) {
	h := newHarness(t)
	got := execRun(h, `
const a = await agent.call('one', { agentType: 'executor-t1', stepDecision: 'sd', node: 'n' })
const b = await agent.call('two', { resume: a })
const c = await agent.call('three', { resume: b })
return [a, b, c].map((r) => ({ status: r.status, reason: r.reason, pair: r.meta.configuredPair }))`)
	for i, g := range got {
		if g["status"] != "completed" || fmt.Sprint(g["pair"]) != "map[effort:xhigh model:claude-fake-sonnet]" {
			t.Errorf("call %d: %v", i, g)
		}
	}
}

// The handle's run is judged by the record the journal holds, not by the text of the handle: a call of another run
// that sits in a shared journal cannot be continued by writing this run's id in front of its call id.
func TestForgedHandlePrefixOnAnotherRunsRecordRefused(t *testing.T) {
	a := newHarness(t)
	a.cfg.RunID = "run-a"
	ra := a.run(`const r = await agent.call('p', { agentType: 'executor', node: 'n1' }); return r.callId`)
	id := value[string](t, ra)
	b := newHarness(t)
	b.cfg.RunID = "run-b"
	b.journal, b.cfg.Journal = a.journal, a.journal
	b.fake, b.cfg.Backend = a.fake, a.fake
	before := len(a.fake.Requests())
	got := execRun(b, fmt.Sprintf(`const r = await agent.call('c', { resume: %q }); return [{ status: r.status, reason: r.reason }]`, Handle("run-b", id)))
	if got[0]["status"] != "refused" || got[0]["reason"] != "foreign_handle" {
		t.Errorf("forged prefix: %v", got)
	}
	if n := len(a.fake.Requests()) - before; n != 0 {
		t.Errorf("a refused resume sent %d requests", n)
	}
}
