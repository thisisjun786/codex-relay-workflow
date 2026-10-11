package engine

import (
	"fmt"
	"strings"
	"testing"
)

// crw.classify takes a handle as well as a CallResult, and reads the exact result the handle names.
func TestClassifyAcceptsHandleOfItsOwnResult(t *testing.T) {
	h := newHarness(t)
	h.fake.Handler = func(r FakeRequest) FakeReply {
		var outcome string
		fmt.Sscanf(r.Prompt, "outcome:%s", &outcome)
		if outcome == "" {
			return FakeReply{}
		}
		return FakeReply{Text: fmt.Sprintf(`{"outcome":%q,"files":[],"verification":[],"summary":"s","gap":null,"research_question":null,"plan_mismatch":null}`, outcome)}
	}
	res := h.run(`
const errs = []
const attempt = (name, f) => { try { f(); errs.push(name + ': no error') } catch (e) { errs.push(name + ': ' + e.name) } }
const a = await agent.call('outcome:needs_research', { agentType: 'executor', schema: RECEIPT })
const b = await agent.call('outcome:blocked_scope', { agentType: 'executor', schema: RECEIPT })
const viaHandle = [crw.classify(a.handle), crw.classify(b.handle), crw.classify(a), crw.classify(b)]
attempt('forged handle', () => crw.classify('crwh1:run-test:c_000000000000'))
attempt('other run handle', () => crw.classify(a.handle.replace('run-test', 'run-x')))
attempt('garbage string', () => crw.classify('nope'))
attempt('bare object', () => crw.classify({}))
attempt('null', () => crw.classify(null))
attempt('undefined', () => crw.classify())
const nohandle = await agent.call('x', { agentType: 'nope' })
attempt('no handle result', () => crw.classify(nohandle.handle))
return { viaHandle, errs }`)
	v := value[map[string]any](t, res)
	if fmt.Sprint(v["viaHandle"]) != "[research scope research scope]" {
		t.Errorf("viaHandle %v", v["viaHandle"])
	}
	for _, e := range v["errs"].([]any) {
		if !strings.HasSuffix(e.(string), ": TypeError") {
			t.Errorf("%v, want a TypeError", e)
		}
	}
	if len(v["errs"].([]any)) != 7 {
		t.Errorf("%v", v["errs"])
	}
}

// crw.* calls take a position of their path, so classify while another position is outstanding is refused.
func TestClassifyObeysPathConcurrency(t *testing.T) {
	for name, body := range map[string]string{
		"pending call": `const r = await agent.call('first', { agentType: 'executor', schema: RECEIPT })
const p = agent.call('pending', { agentType: 'explorer', label: 'pending' })
const x = crw.classify(r)
await p
return x`,
		"pending join": `const r = await agent.call('first', { agentType: 'executor', schema: RECEIPT })
const j = parallel([async () => agent('in', { agentType: 'explorer', label: 'in' })], { label: 'J' })
const x = crw.classify(r)
await j
return x`,
		"pending handle": `const r = await agent.call('first', { agentType: 'executor', schema: RECEIPT })
const p = agent.call('pending', { agentType: 'explorer', label: 'pending' })
const x = crw.classify(r.handle)
await p
return x`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			res := h.run(body)
			if res.Status != RunScriptError || res.ErrorKind != ErrKindPathConcurrency {
				t.Fatalf("run %s %s: %s %s", res.Status, res.ErrorKind, res.Error, res.Value)
			}
			if !strings.Contains(res.Error, "crw.classify") {
				t.Errorf("the error must name crw.classify: %s", res.Error)
			}
		})
	}
	// a classify between settled calls takes a position and gives no trouble
	h := newHarness(t)
	res := h.run(`const r = await agent.call('first', { agentType: 'executor', schema: RECEIPT })
const s = crw.classify(r)
const t = await agent.call('second', { agentType: 'explorer', label: 'second' })
return [s, t.callId !== r.callId, t.position, t.meta.role]`)
	if res.Status != RunCompleted {
		t.Fatalf("%s %s", res.Status, res.Error)
	}
	if len(res.Calls) != 2 || res.Calls[1].Position != 3 {
		t.Errorf("calls %+v: classify takes position 2", res.Calls)
	}
}
