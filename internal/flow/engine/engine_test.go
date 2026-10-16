package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

const header = "import { RECEIPT, VERDICT, FINDINGS } from 'crw:lib/schemas'\nexport const meta = { name: 't' }\n"

// harness is one run under test.
type harness struct {
	t       *testing.T
	fake    *Fake
	journal *MemoryJournal
	cfg     Config
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, fake: NewFake(), journal: NewMemoryJournal()}
	h.cfg = Config{Backend: h.fake, Policy: FakePolicy(), Journal: h.journal, RunID: "run-test"}
	return h
}

func (h *harness) load(body string) *script.Program {
	h.t.Helper()
	p, err := script.Load(script.Source{Name: "t.js", Text: []byte(header + body)})
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

// run executes the body and returns the result, failing the test if the run cannot finish in time.
func (h *harness) run(body string) *Result {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := Run(ctx, h.load(body), h.cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

// value decodes the script's return value. A run the operator paused (RunPaused) has its value as a completed run has; a script_error
// fails the test.
func value[T any](t *testing.T, res *Result) T {
	t.Helper()
	if res.Status != RunCompleted && res.Status != RunPaused {
		t.Fatalf("run %s: %s: %s", res.Status, res.ErrorKind, res.Error)
	}
	var v T
	if err := json.Unmarshal(res.Value, &v); err != nil {
		t.Fatalf("value %s: %v", res.Value, err)
	}
	return v
}

func (h *harness) callRows(label string) []Event {
	var out []Event
	for _, e := range h.journal.Events() {
		if e.Kind == KindCall && e.Label == label {
			out = append(out, e)
		}
	}
	return out
}

func terminalOf(rows []Event) string {
	for i := len(rows) - 1; i >= 0; i-- {
		if _, ok := terminalEvents[rows[i].Event]; ok {
			return rows[i].Event
		}
	}
	return ""
}

func TestHelloRuns(t *testing.T) {
	h := newHarness(t)
	res := h.run(`
phase('one')
log('start')
const out = await agent('say hi', { agentType: 'explorer', label: 'hi' })
return { out, args, kinds: [typeof agent, typeof agent.call, typeof parallel, typeof pipeline, typeof workflow, typeof crw.classify, typeof budget.spent] }
`)
	v := value[map[string]any](t, res)
	if v["out"] != "fake answer to: say hi" {
		t.Errorf("out %v", v["out"])
	}
	if len(res.Calls) != 1 || res.Calls[0].Path != RootPath || res.Calls[0].Position != 1 || res.Calls[0].Status != StatusCompleted {
		t.Errorf("calls %+v", res.Calls)
	}
	if len(res.Phases) != 1 || len(res.Logs) != 1 {
		t.Errorf("phases %v logs %v", res.Phases, res.Logs)
	}
	rows := CallEvents(h.journal, res.Calls[0].CallID)
	var names []string
	for _, r := range rows {
		names = append(names, r.Event)
	}
	if got := strings.Join(names, ","); got != "prepared,thread_requested,thread_started,turn_requested,turn_started,completed" {
		t.Errorf("events %s", got)
	}
}

// parallel() items are keyed by their input index; completion order never decides a path or a position.
func TestParallelCompletionOrderDoesNotChangePaths(t *testing.T) {
	const n = 4
	body := `
const outs = await parallel([0, 1, 2, 3].map((i) => async () => {
  const r = await agent.call('item-' + i, { agentType: 'explorer', label: 'item-' + i })
  return r.callId
}))
return outs
`
	type ident struct {
		Path     string
		Position int
		Label    string
		CallID   string
	}
	runOrder := func(release []int) (map[string]ident, []string) {
		h := newHarness(t)
		gates := make([]chan struct{}, n)
		for i := range gates {
			gates[i] = make(chan struct{})
		}
		h.fake.Handler = func(r FakeRequest) FakeReply {
			var i int
			fmt.Sscanf(r.Prompt, "item-%d", &i)
			<-gates[i]
			return FakeReply{}
		}
		go func() {
			for _, i := range release {
				close(gates[i])
				// let the completion land before the next gate opens, so the order is the requested one
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					done := 0
					for _, e := range h.journal.Events() {
						if e.Event == EvCompleted {
							done++
						}
					}
					if done > slices.Index(release, i) {
						break
					}
					time.Sleep(time.Millisecond)
				}
			}
		}()
		res := h.run(body)
		_ = value[[]string](t, res)
		ids := map[string]ident{}
		for _, c := range res.Calls {
			ids[c.Label] = ident{c.Path, c.Position, c.Label, c.CallID}
		}
		var order []string
		for _, e := range h.journal.Events() {
			if e.Event == EvCompleted {
				order = append(order, e.Label)
			}
		}
		return ids, order
	}
	a, orderA := runOrder([]int{0, 1, 2, 3})
	b, orderB := runOrder([]int{3, 1, 0, 2})
	if slices.Equal(orderA, orderB) {
		t.Fatalf("the two runs completed in the same order %v; the test does not exercise reordering", orderA)
	}
	if len(a) != n {
		t.Fatalf("calls %v", a)
	}
	for label, ia := range a {
		if ib := b[label]; ia != ib {
			t.Errorf("%s: %+v in the first run, %+v in the second", label, ia, ib)
		}
	}
	if a["item-2"].Path != "root/1:2" || a["item-2"].Position != 1 {
		t.Errorf("item-2 is %+v, want path root/1:2 position 1", a["item-2"])
	}
}

// pipeline() has no barrier: an item that is past stage 1 moves on while another item is still in stage 1.
//
// Item 0 stays in stage 1 until the journal holds the completed rows of stage 2 of items 1 and 2 (CRW-1389): releasing it when their
// turns were asked for raced with those rows under load, and its own completed row came first. A pipeline with a barrier never
// runs stage 2 while item 0 is in stage 1, so item 0 is let go after ten seconds and the order check fails.
func TestPipelineNoBarrier(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	h.cfg.Journal = &completedRows{Journal: h.journal, labels: []string{"stage2:1", "stage2:2"}, all: release}
	h.fake.Handler = func(r FakeRequest) FakeReply {
		if r.Prompt == "s1-0" {
			select {
			case <-release:
			case <-time.After(10 * time.Second):
			}
		}
		return FakeReply{}
	}
	res := h.run(`
const outs = await pipeline([0, 1, 2],
  async (item, _o, i) => { await agent('s1-' + i, { agentType: 'explorer', label: 'stage1:' + i }); return item * 10 },
  async (prev, item, i) => { await agent('s2-' + i, { agentType: 'explorer', label: 'stage2:' + i }); return prev + 1 })
return outs
`)
	if got := value[[]int](t, res); !slices.Equal(got, []int{1, 11, 21}) {
		t.Errorf("results %v", got)
	}
	idx := map[string]int{}
	for i, e := range h.journal.Events() {
		if e.Event == EvCompleted {
			idx[e.Label] = i
		}
	}
	if !(idx["stage2:1"] < idx["stage1:0"] && idx["stage2:2"] < idx["stage1:0"]) {
		t.Errorf("stage 2 of items 1 and 2 must complete before stage 1 of item 0: %v", idx)
	}
}

// completedRows is a journal that closes all once it stored a completed call row for every one of labels.
type completedRows struct {
	Journal
	labels []string
	all    chan struct{}
	mu     sync.Mutex
	seen   map[string]bool
	closed bool
}

func (j *completedRows) Append(e Event) Event {
	out := j.Journal.Append(e)
	if e.Kind != KindCall || e.Event != EvCompleted {
		return out
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.seen == nil {
		j.seen = map[string]bool{}
	}
	j.seen[e.Label] = true
	for _, l := range j.labels {
		if !j.seen[l] {
			return out
		}
	}
	if !j.closed {
		j.closed = true
		close(j.all)
	}
	return out
}

func TestParallelThunkThrowIsNullAndJoinsSettle(t *testing.T) {
	h := newHarness(t)
	res := h.run(`
const outs = await parallel([async () => 1, async () => { throw new Error('boom') }, async () => 3])
const after = await agent('after', { agentType: 'explorer' })
return { outs, after: typeof after }
`)
	v := value[map[string]any](t, res)
	if fmt.Sprint(v["outs"]) != "[1 <nil> 3]" || v["after"] != "string" {
		t.Errorf("%v", v)
	}
	var item *Event
	for _, e := range h.journal.Events() {
		if e.Event == "item_error" {
			e := e
			item = &e
		}
	}
	if item == nil || !strings.Contains(fmt.Sprint(item.Data["error"]), "boom") {
		t.Errorf("item error row %+v", item)
	}
}

const schemaCall = `const r = await agent.call('give', { agentType: 'reviewer', schema: VERDICT, label: 'v' })
return { status: r.status, reason: r.reason, repairUsed: r.meta.repairUsed, turns: r.meta.turnIds.length, hasOutput: r.output !== null }`

func TestSchemaRepairThenNull(t *testing.T) {
	// valid after the repair turn
	h := newHarness(t)
	h.fake.Handler = func(r FakeRequest) FakeReply {
		if !r.Repair {
			return FakeReply{Text: `{"verdict": "maybe"}`}
		}
		return FakeReply{}
	}
	v := value[map[string]any](t, h.run(schemaCall))
	if v["status"] != "completed" || v["repairUsed"] != true || v["turns"] != float64(2) || v["hasOutput"] != true {
		t.Errorf("repair success: %v", v)
	}

	// still invalid after the repair turn: null_model, exactly one repair turn, no third turn
	h = newHarness(t)
	h.fake.Handler = func(r FakeRequest) FakeReply { return FakeReply{Text: "not json at all"} }
	v = value[map[string]any](t, h.run(schemaCall))
	if v["status"] != "null_model" || v["reason"] != "schema_invalid_after_repair" || v["turns"] != float64(2) || v["hasOutput"] != false {
		t.Errorf("repair failure: %v", v)
	}
	var turns, repairs int
	for _, r := range h.fake.Requests() {
		if r.Kind == "turn" {
			turns++
			if r.Repair {
				repairs++
			}
		}
	}
	if turns != 2 || repairs != 1 {
		t.Errorf("turns %d repairs %d, want 2 and 1", turns, repairs)
	}
	rows := h.callRows("v")
	if terminalOf(rows) != EvNullModel {
		t.Errorf("terminal %s", terminalOf(rows))
	}
}

// Success, schema failure, policy refusal, unknown and held each carry their own status, reason and output.
func TestCallResultFieldsSeparate(t *testing.T) {
	h := newHarness(t)
	h.fake.Handler = func(r FakeRequest) FakeReply {
		if r.Repair {
			return FakeReply{Text: "{}"}
		}
		switch r.Prompt {
		case "schema-fail":
			return FakeReply{Text: "{}"}
		case "unknown":
			return FakeReply{Err: &TransportError{Err: errors.New("connection reset")}}
		case "held":
			return FakeReply{Fail: &TurnFailure{CodexErrorInfo: "serverOverloaded", Message: "busy"}}
		}
		return FakeReply{}
	}
	res := h.run(`
const sum = (r) => ({ status: r.status, reason: r.reason, hasOutput: r.output !== null, hasHandle: r.handle !== null, callId: typeof r.callId, frozen: Object.isFrozen(r), pair: r.meta.configuredPair.model })
const out = await parallel([
  async () => sum(await agent.call('ok', { agentType: 'reviewer', schema: VERDICT })),
  async () => sum(await agent.call('schema-fail', { agentType: 'reviewer', schema: VERDICT })),
  async () => sum(await agent.call('x', { agentType: 'no-such-role' })),
  async () => sum(await agent.call('unknown', { agentType: 'explorer' })),
  async () => sum(await agent.call('held', { agentType: 'explorer' })),
])
return out
`)
	got := value[[]map[string]any](t, res)
	want := []map[string]any{
		{"status": "completed", "reason": nil, "hasOutput": true, "hasHandle": true, "callId": "string", "frozen": true, "pair": "gpt-fake-sol"},
		{"status": "null_model", "reason": "schema_invalid_after_repair", "hasOutput": false, "hasHandle": true, "callId": "string", "frozen": true, "pair": "gpt-fake-sol"},
		{"status": "refused", "reason": "policy_refused", "hasOutput": false, "hasHandle": false, "callId": "string", "frozen": true, "pair": ""},
		{"status": "unknown", "reason": "dispatch_unknown", "hasOutput": false, "hasHandle": true, "callId": "string", "frozen": true, "pair": "claude-fake-haiku"},
		{"status": "held", "reason": "host_infra_serverOverloaded", "hasOutput": false, "hasHandle": true, "callId": "string", "frozen": true, "pair": "claude-fake-haiku"},
	}
	for i := range want {
		if fmt.Sprint(got[i]) != fmt.Sprint(want[i]) {
			t.Errorf("result %d: %v, want %v", i, got[i], want[i])
		}
	}
	// the journal ends each call on its own terminal event
	byLabel := map[string]string{}
	for _, c := range res.Calls {
		byLabel[c.Status] = terminalOf(CallEvents(h.journal, c.CallID))
	}
	for st, ev := range map[string]string{"completed": EvCompleted, "null_model": EvNullModel, "refused": EvRefused, "unknown": EvOutcomeUnknown, "held": EvHeld} {
		if byLabel[st] != ev {
			t.Errorf("status %s ends on %q, want %q", st, byLabel[st], ev)
		}
	}
	// a held call records whether its packet was delivered
	for _, c := range res.Calls {
		if c.Status == StatusHeld {
			rows := CallEvents(h.journal, c.CallID)
			last := rows[len(rows)-1]
			if last.Data["input_state"] != "delivered" || last.Data["codex_error_info"] != "serverOverloaded" {
				t.Errorf("held row %+v", last.Data)
			}
		}
	}
}

func TestHostFailureMap(t *testing.T) {
	for info, want := range map[string]Disposition{
		"usageLimitExceeded":           {StatusHeld, "host_infra_usageLimitExceeded", ""},
		"rateLimitExceeded":            {StatusHeld, "host_infra_rateLimitExceeded", ""},
		"serverOverloaded":             {StatusHeld, "host_infra_serverOverloaded", ""},
		"activeTurnNotSteerable":       {StatusHeld, "host_infra_activeTurnNotSteerable", ""},
		"other":                        {StatusHeld, "host_infra_other", ""},
		"sessionBudgetExceeded":        {StatusHeld, "budget_exhausted", ""},
		"badRequest":                   {StatusRefused, "host_config_badRequest", ""},
		"unauthorized":                 {StatusRefused, "host_config_unauthorized", ""},
		"sandboxError":                 {StatusRefused, "host_config_sandboxError", ""},
		"cyberPolicy":                  {StatusRefused, "host_policy_cyberPolicy", ""},
		"misalignmentPolicyViolation":  {StatusRefused, "host_policy_misalignmentPolicyViolation", ""},
		"contextWindowExceeded":        {StatusNullModel, "context_window_exceeded", ""},
		CodexErrorFlowBinding:          {StatusHeld, "host_config_flow_binding", ""},
		"":                             {StatusHeld, "host_infra_unknown", ""},
		"aVariantThePinnedSchemaLacks": {StatusHeld, "host_infra_unknown", ""},
	} {
		got := ClassifyHostFailure(info)
		if got.Status != want.Status || got.Reason != want.Reason {
			t.Errorf("%q: %+v, want %+v", info, got, want)
		}
	}
	// a quota or rate limit pauses the run; the next call is held engine_paused before any request
	h := newHarness(t)
	h.fake.Handler = func(r FakeRequest) FakeReply {
		return FakeReply{Fail: &TurnFailure{CodexErrorInfo: "usageLimitExceeded"}}
	}
	res := h.run(`
const a = await agent.call('one', { agentType: 'explorer' })
const b = await agent.call('two', { agentType: 'explorer' })
return [a.reason, b.reason, b.status]`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"host_infra_usageLimitExceeded", "engine_paused", "held"}) {
		t.Errorf("%v", got)
	}
	if n := countTurns(h.fake); n != 1 {
		t.Errorf("%d turns were sent, want 1: the paused run sends no request", n)
	}
	// an interrupt the engine did not send is held host_interrupted_external, never a tier signal
	h = newHarness(t)
	h.fake.Handler = func(r FakeRequest) FakeReply { return FakeReply{Fail: &TurnFailure{Interrupted: true}} }
	res = h.run(`const a = await agent.call('one', { agentType: 'explorer' }); return [a.status, a.reason]`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"held", "host_interrupted_external"}) {
		t.Errorf("%v", got)
	}
	// an RPC error to thread/start with a codexErrorInfo is classified, and nothing was delivered
	h = newHarness(t)
	h.fake.StartHook = func(ThreadSpec) (ThreadInfo, error) {
		return ThreadInfo{}, &RPCError{Message: "bad schema", CodexErrorInfo: "badRequest"}
	}
	res = h.run(`const a = await agent.call('one', { agentType: 'explorer' }); return [a.status, a.reason, a.handle]`)
	if got := value[[]any](t, res); fmt.Sprint(got) != "[refused host_config_badRequest <nil>]" {
		t.Errorf("%v", got)
	}
}

func countTurns(f *Fake) (n int) {
	for _, r := range f.Requests() {
		if r.Kind == "turn" {
			n++
		}
	}
	return
}

func TestPlainAgentReturnsOutputOrNull(t *testing.T) {
	h := newHarness(t)
	h.fake.Handler = func(r FakeRequest) FakeReply {
		if r.Prompt == "held" {
			return FakeReply{Fail: &TurnFailure{CodexErrorInfo: "serverOverloaded"}}
		}
		return FakeReply{}
	}
	res := h.run(`
const obj = await agent('structured', { agentType: 'explorer', schema: FINDINGS })
const text = await agent('plain text', { agentType: 'explorer' })
const none = await agent('held', { agentType: 'explorer' })
const refused = await agent('x', { agentType: 'nope' })
const viaCall = (await agent.call('plain text', { agentType: 'explorer' })).output
return { objKeys: Object.keys(obj).sort(), text, none, refused, same: viaCall === text, wrapper: typeof agent.call === 'function' }
`)
	v := value[map[string]any](t, res)
	if fmt.Sprint(v["objKeys"]) != "[answer evidence inferred unknown verified]" || v["text"] != "fake answer to: plain text" ||
		v["none"] != nil || v["refused"] != nil || v["same"] != true {
		t.Errorf("%v", v)
	}
}

func TestClassifyRejectsBareValue(t *testing.T) {
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
const real = await agent.call('p', { agentType: 'executor', schema: RECEIPT })
attempt('empty object', () => crw.classify({}))
attempt('null', () => crw.classify(null))
attempt('undefined', () => crw.classify())
attempt('string', () => crw.classify('crwh1:run-test:x'))
attempt('output', () => crw.classify(real.output))
attempt('copy', () => crw.classify({ ...real }))
attempt('json copy', () => crw.classify(JSON.parse(JSON.stringify(real))))
attempt('resume null', () => agent.call('c', { agentType: 'executor', resume: null }))
attempt('resume bare', () => agent.call('c', { agentType: 'executor', resume: {} }))
attempt('resume garbage string', () => agent.call('c', { agentType: 'executor', resume: 'not-a-handle' }))
attempt('resume number', () => agent.call('c', { agentType: 'executor', resume: 7 }))
const states = []
for (const o of ['ready_for_review', 'needs_research', 'blocked_scope', 'failed', 'budget_exhausted']) {
  states.push(crw.classify(await agent.call('outcome:' + o, { agentType: 'executor', schema: RECEIPT })))
}
return { errs, ok: crw.classify(real), states }
`)
	v := value[map[string]any](t, res)
	for _, e := range v["errs"].([]any) {
		if !strings.HasSuffix(e.(string), ": TypeError") {
			t.Errorf("%v, want a TypeError", e)
		}
	}
	if len(v["errs"].([]any)) != 11 || v["ok"] != "ready" || fmt.Sprint(v["states"]) != "[ready research scope self_failed self_failed]" {
		t.Errorf("%v", v)
	}
	// the rejected resume calls never opened a position: nothing but the real calls is journaled
	for _, e := range h.journal.Events() {
		if e.Event == EvPrepared && e.Data["input_digest"] == digestJSON([]byte("c")) {
			t.Errorf("a TypeError resume was journaled: %+v", e)
		}
	}
	// each failure state classifies from its own result
	h = newHarness(t)
	h.fake.Handler = func(r FakeRequest) FakeReply {
		if r.Repair {
			return FakeReply{Text: "still nope"}
		}
		switch r.Prompt {
		case "null":
			return FakeReply{Text: "nope"}
		case "held":
			return FakeReply{Fail: &TurnFailure{CodexErrorInfo: "serverOverloaded"}}
		case "unknown":
			return FakeReply{Err: &TransportError{Err: errors.New("eof")}}
		}
		return FakeReply{}
	}
	res = h.run(`
const c = async (p, o) => crw.classify(await agent.call(p, { agentType: 'executor', schema: RECEIPT, ...o }))
return [await c('null'), await c('held'), await c('unknown'), await c('x', { agentType: 'nope' })]`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"null_model", "host_held", "null_unknown", "refused"}) {
		t.Errorf("%v", got)
	}
}

func execRun(h *harness, body string) []map[string]any {
	res := h.run(body)
	return value[[]map[string]any](h.t, res)
}

func TestForeignHandleRefused(t *testing.T) {
	// another run lineage: a handle from run A used in run B
	a := newHarness(t)
	a.cfg.RunID = "run-a"
	ra := a.run(`const r = await agent.call('p', { agentType: 'executor', node: 'n1' }); return r.handle`)
	handleA := value[string](t, ra)
	b := newHarness(t)
	b.cfg.RunID = "run-b"
	// run B reads the journal run A wrote, so the call the handle names is found: only the lineage check refuses it
	b.journal, b.cfg.Journal = a.journal, a.journal
	got := execRun(b, fmt.Sprintf(`const r = await agent.call('c', { resume: %q }); return [{ status: r.status, reason: r.reason }]`, handleA))
	if got[0]["status"] != "refused" || got[0]["reason"] != "foreign_handle" {
		t.Errorf("other lineage: %v", got)
	}
	if countTurns(b.fake) != 0 || len(b.fake.Requests()) != 0 {
		t.Errorf("a refused resume sent a request: %v", b.fake.Requests())
	}

	// other role family and other node, in one run
	h := newHarness(t)
	got = execRun(h, `
const ex = await agent.call('build', { agentType: 'executor', node: 'n1', label: 'ex' })
const rev = await agent.call('r', { agentType: 'reviewer', resume: ex.handle })
const alias = await agent.call('r', { agentType: 'ocx-gpt-6-1-sol', resume: ex.handle })
const node = await agent.call('r', { resume: ex.handle, node: 'n2' })
const forged = await agent.call('r', { resume: 'crwh1:run-test:c_000000000000' })
const same = await agent.call('r', { resume: ex.handle, node: 'n1', agentType: 'executor' })
return [rev, alias, node, forged, same].map((r) => ({ status: r.status, reason: r.reason }))`)
	want := []string{"refused/foreign_handle", "refused/foreign_handle", "refused/foreign_handle", "refused/foreign_handle", "completed/<nil>"}
	for i, w := range want {
		if g := fmt.Sprintf("%v/%v", got[i]["status"], got[i]["reason"]); g != w {
			t.Errorf("case %d: %s, want %s", i, g, w)
		}
	}
	if n := countTurns(h.fake); n != 2 {
		t.Errorf("%d turns: the build and the one valid continuation only", n)
	}
}

func TestResumeInheritsRolePairAndStepDecision(t *testing.T) {
	h := newHarness(t)
	res := h.run(`
const first = await agent.call('build', { agentType: 'executor-t1', stepDecision: 'sd-7', node: 'node-3', label: 'first' })
const next = await agent.call('fix it', { resume: first.handle, label: 'next' })
const viaResult = await agent.call('again', { resume: next, label: 'again' })
return [first, next, viaResult].map((r) => ({ status: r.status, handle: r.handle, role: r.meta.role, pair: r.meta.configuredPair, thread: r.meta.threadId, turns: r.meta.turnIds }))`)
	got := value[[]struct {
		Status, Handle, Role, Thread string
		Pair                         Pair
		Turns                        []string
	}](t, res)
	for i, g := range got {
		if g.Status != "completed" || g.Role != "executor-t1" || g.Pair != (Pair{"claude-fake-sonnet", "xhigh"}) || g.Thread != got[0].Thread {
			t.Errorf("call %d: %+v", i, g)
		}
	}
	if got[0].Handle == got[1].Handle || got[1].Handle == got[2].Handle {
		t.Errorf("each call has its own handle: %v", got)
	}
	// the host saw one thread and three turns on it
	var threads int
	for _, r := range h.fake.Requests() {
		if r.Kind == "thread" {
			threads++
		}
	}
	th, ok := h.fake.Thread(got[0].Thread)
	if threads != 1 || !ok || len(th.Turns) != 3 || th.Pair != (Pair{"claude-fake-sonnet", "xhigh"}) {
		t.Errorf("threads %d thread %+v", threads, th)
	}
	// the journal row of the continuation carries the inherited identity and what it inherited
	var prep Event
	for _, e := range h.callRows("next") {
		if e.Event == EvPrepared {
			prep = e
		}
	}
	d := prep.Data
	if d["role"] != "executor-t1" || d["step_decision"] != "sd-7" || d["node"] != "node-3" || d["model"] != "claude-fake-sonnet" || d["thread_id"] != got[0].Thread {
		t.Errorf("prepared row %+v", d)
	}
	if inh := fmt.Sprint(d["inherited"]); inh != "[role pair node stepDecision]" {
		t.Errorf("inherited %s", inh)
	}
	// the handle resolves through the journal to the thread and the call's last turn
	info, ok := LookupCall(h.journal, got[1].Handle[strings.LastIndex(got[1].Handle, ":")+1:])
	if !ok || info.ThreadID != got[0].Thread || info.ThroughTurnID != got[1].Turns[len(got[1].Turns)-1] || info.StepDecision != "sd-7" {
		t.Errorf("call info %+v", info)
	}
}

func TestResumeConflictingPairRefused(t *testing.T) {
	h := newHarness(t)
	got := execRun(h, `
const t1 = await agent.call('build', { agentType: 'executor-t1', stepDecision: 'sd-1', node: 'n' })
const r = (o) => agent.call('c', { resume: t1.handle, ...o }).then((x) => ({ status: x.status, reason: x.reason }))
return [
  await r({ agentType: 'executor' }),
  await r({ agentType: 'haiku', isolation: 'worktree' }),
  await r({ model: 'haiku' }),
  await r({ stepDecision: 'sd-2' }),
  await r({ effort: 'low' }),
  await r({ agentType: 'executor-t1', stepDecision: 'sd-1', model: 'sonnet', effort: 'low' }),
  await r({ agentType: 'executor-t1', stepDecision: 'sd-1', model: 'sonnet', effort: 'xhigh' }),
]`)
	want := []string{"refused/pair_refused", "refused/pair_refused", "refused/pair_refused", "refused/pair_refused", "refused/pair_refused", "refused/pair_refused", "completed/<nil>"}
	for i, w := range want {
		if g := fmt.Sprintf("%v/%v", got[i]["status"], got[i]["reason"]); g != w {
			t.Errorf("case %d: %s, want %s", i, g, w)
		}
	}
	if n := countTurns(h.fake); n != 2 {
		t.Errorf("%d turns: a refused continuation sends nothing", n)
	}
}

// A fresh call's requested effort is recorded and not applied; a continuation that names another effort than the
// thread's pair is refused (TestResumeConflictingPairRefused).
func TestRequestedEffortIsKeptNotApplied(t *testing.T) {
	h := newHarness(t)
	res := h.run(`
const r = await agent.call('build', { agentType: 'executor-t1', stepDecision: 'sd-1', effort: 'low' })
return { status: r.status, configured: r.meta.configuredPair, requested: r.meta.requestedPair }`)
	v := value[map[string]any](t, res)
	if v["status"] != "completed" || fmt.Sprint(v["configured"]) != "map[effort:xhigh model:claude-fake-sonnet]" || fmt.Sprint(v["requested"]) != "map[effort:low model:claude-fake-sonnet]" {
		t.Errorf("%v", v)
	}
}

func TestFreshExecutorT1NeedsStepDecision(t *testing.T) {
	h := newHarness(t)
	got := execRun(h, `
const a = await agent.call('x', { agentType: 'executor-t1' })
const b = await agent.call('x', { agentType: 'sonnet' })
const c = await agent.call('x', { agentType: 'executor-t1', stepDecision: 'sd' })
return [a, b, c].map((r) => ({ status: r.status, reason: r.reason }))`)
	want := []string{"refused/missing_step_decision", "refused/missing_step_decision", "completed/<nil>"}
	for i, w := range want {
		if g := fmt.Sprintf("%v/%v", got[i]["status"], got[i]["reason"]); g != w {
			t.Errorf("case %d: %s, want %s", i, g, w)
		}
	}
}

func TestAliasTable(t *testing.T) {
	h := newHarness(t)
	got := execRun(h, `
const role = async (o) => { const r = await agent.call('x', { schema: FINDINGS, ...o }); return { status: r.status, reason: r.reason, role: r.meta.role, req: r.meta.requestedPair.effort } }
return [
  await role({ model: 'opus' }),
  await role({ model: 'haiku', isolation: 'worktree' }),
  await role({ model: 'haiku' }),
  await role({ agentType: 'ocx-gpt-6-1-sol', effort: 'xhigh' }),
  await role({ agentType: 'ocx-gpt-6-1-sol', effort: 'high' }),
  await role({ agentType: 'executor', model: 'haiku', effort: 'low' }),
  await role({ agentType: 'executor', model: 'sonnet' }),
  await role({ model: 'gpt-9' }),
  await role({}),
]`)
	want := []string{
		"completed/<nil>/architect/high", "completed/<nil>/executor/xhigh", "refused/policy_refused//",
		"completed/<nil>/reviewer/xhigh", "completed/<nil>/reviewer/high", "completed/<nil>/executor/low",
		"refused/pair_refused//", "refused/policy_refused//", "refused/policy_refused//",
	}
	for i, w := range want {
		g := fmt.Sprintf("%v/%v/%v/%v", got[i]["status"], got[i]["reason"], got[i]["role"], got[i]["req"])
		if g != w {
			t.Errorf("case %d: %s, want %s", i, g, w)
		}
	}
}

func TestAdhocSchemaNotStrictRefused(t *testing.T) {
	h := newHarness(t)
	got := execRun(h, `
const bad = { type: 'object', properties: { a: { type: 'string' } }, required: [] , additionalProperties: false }
const r = await agent.call('x', { agentType: 'explorer', schema: bad })
return [{ status: r.status, reason: r.reason }]`)
	if got[0]["status"] != "refused" || got[0]["reason"] != "schema_not_strict" {
		t.Errorf("%v", got)
	}
	if len(h.fake.Requests()) != 0 {
		t.Errorf("a schema that is not strict must not start a thread: %v", h.fake.Requests())
	}
}

func TestPairReadbackMismatchRefused(t *testing.T) {
	h := newHarness(t)
	h.fake.StartHook = func(spec ThreadSpec) (ThreadInfo, error) {
		return ThreadInfo{ID: "thr_x", Pair: Pair{spec.Pair.Model, "low"}}, nil
	}
	got := execRun(h, `const r = await agent.call('x', { agentType: 'explorer' }); return [{ status: r.status, reason: r.reason }]`)
	if got[0]["status"] != "refused" || got[0]["reason"] != "pair_refused" || countTurns(h.fake) != 0 {
		t.Errorf("%v turns %d", got, countTurns(h.fake))
	}
}

// Two positions open on one path are refused before any intent row or request, and the run ends script_error.
func TestConcurrentCallOnPathRefused(t *testing.T) {
	for name, body := range map[string]string{
		"Promise.all": `return await Promise.all([agent('A', { agentType: 'explorer', label: 'A' }), agent('B', { agentType: 'explorer', label: 'B' })])`,
		"unawaited call": `const p = agent.call('A', { agentType: 'explorer', label: 'A' })
const q = await agent('B', { agentType: 'explorer', label: 'B' })
return [p, q]`,
		"call while a join is pending": `const j = parallel([async () => agent('A', { agentType: 'explorer', label: 'A' })], { label: 'J' })
return await agent('B', { agentType: 'explorer', label: 'B' })`,
		"crw call while a call is pending": `const p = agent('A', { agentType: 'explorer', label: 'A' })
await crw.clock()
return p`,
		"caught and swallowed": `try { await Promise.all([agent('A', { agentType: 'explorer', label: 'A' }), agent('B', { agentType: 'explorer', label: 'B' })]) } catch (e) { }
return 'swallowed'`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			res := h.run(body)
			if res.Status != RunScriptError || res.ErrorKind != ErrKindPathConcurrency {
				t.Fatalf("run %s %s: %s", res.Status, res.ErrorKind, res.Error)
			}
			if !strings.Contains(res.Error, `"B"`) && !strings.Contains(res.Error, "crw.clock") || !strings.Contains(res.Error, `"A"`) && !strings.Contains(res.Error, `"J"`) {
				t.Errorf("the error must name both labels: %s", res.Error)
			}
			if rows := h.callRows("B"); len(rows) != 0 {
				t.Errorf("B has %d journal rows, want 0: %+v", len(rows), rows)
			}
			for _, r := range h.fake.Requests() {
				if r.Prompt == "B" {
					t.Errorf("B reached the host: %+v", r)
				}
			}
		})
	}
	// the script sees a CrwPathConcurrencyError with both labels
	h := newHarness(t)
	res := h.run(`try { await Promise.all([agent('A', { agentType: 'explorer', label: 'A' }), agent('B', { agentType: 'explorer', label: 'B' })]) } catch (e) { log(JSON.stringify({ name: e.name, open: e.openLabel, req: e.requestedLabel, path: e.path, inst: e instanceof CrwPathConcurrencyError, err: e instanceof Error })) }`)
	if res.Status != RunScriptError || len(res.Logs) != 1 {
		t.Fatalf("status %s logs %v", res.Status, res.Logs)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(res.Logs[0]), &v); err != nil || v["name"] != "CrwPathConcurrencyError" || v["open"] != "A" || v["req"] != "B" || v["path"] != RootPath || v["inst"] != true || v["err"] != true {
		t.Errorf("%s %v", res.Logs[0], err)
	}
	// A was started and is ended by the abort: superseded, never completed
	if st := terminalOf(h.callRows("A")); st != EvSuperseded && st != EvCompleted {
		t.Errorf("A ends on %q", st)
	}
}

// Every call of a parallel item sits at the same (path, position) whatever order the host completes them in.
func TestParallelItemNestedCallsStablePositions(t *testing.T) {
	body := `
await parallel([0, 1, 2].map((i) => async () => {
  const a = await agent('first-' + i, { agentType: 'explorer', label: 'first-' + i })
  return agent('second-' + i + ' after ' + a, { agentType: 'explorer', label: 'second-' + i })
}))
`
	type slot struct {
		Path     string
		Position int
		CallID   string
	}
	run := func(delay func(prompt string) time.Duration) (map[string]slot, []string) {
		h := newHarness(t)
		h.fake.Handler = func(r FakeRequest) FakeReply {
			time.Sleep(delay(r.Prompt))
			return FakeReply{}
		}
		res := h.run(body)
		if res.Status != RunCompleted {
			t.Fatalf("%s %s", res.Status, res.Error)
		}
		out := map[string]slot{}
		for _, c := range res.Calls {
			out[c.Label] = slot{c.Path, c.Position, c.CallID}
		}
		var order []string
		for _, e := range h.journal.Events() {
			if e.Event == EvCompleted {
				order = append(order, e.Label)
			}
		}
		return out, order
	}
	forward, orderF := run(func(p string) time.Duration {
		var i int
		if strings.HasPrefix(p, "first-") {
			fmt.Sscanf(p, "first-%d", &i)
		} else {
			fmt.Sscanf(p, "second-%d", &i)
		}
		return time.Duration(i*25) * time.Millisecond
	})
	reversed, orderR := run(func(p string) time.Duration {
		var i int
		if strings.HasPrefix(p, "first-") {
			fmt.Sscanf(p, "first-%d", &i)
		} else {
			fmt.Sscanf(p, "second-%d", &i)
		}
		return time.Duration((2-i)*25) * time.Millisecond
	})
	if slices.Equal(orderF, orderR) {
		t.Fatalf("both runs completed in order %v", orderF)
	}
	if len(forward) != 6 {
		t.Fatalf("calls %v", forward)
	}
	for label, s := range forward {
		if reversed[label] != s {
			t.Errorf("%s: %+v then %+v", label, s, reversed[label])
		}
	}
	for i := 0; i < 3; i++ {
		f, s := forward[fmt.Sprintf("first-%d", i)], forward[fmt.Sprintf("second-%d", i)]
		if f.Path != s.Path || f.Position != 1 || s.Position != 2 || f.Path != fmt.Sprintf("root/1:%d", i) {
			t.Errorf("item %d: first %+v second %+v", i, f, s)
		}
	}
}

func TestNestedJoinsAndKeys(t *testing.T) {
	h := newHarness(t)
	res := h.run(`
await parallel([async () => {
  await pipeline(['x', 'y'], async (v) => agent('in-' + v, { agentType: 'explorer', label: 'in-' + v }), { keys: ['kx', 'ky'] })
}, async () => agent('solo', { agentType: 'explorer', label: 'solo' })], { keys: ['left', 'right'] })
const t = await agent('tail', { agentType: 'explorer', label: 'tail' })
return 'ok'`)
	if res.Status != RunCompleted {
		t.Fatal(res.Error)
	}
	paths := map[string]string{}
	for _, c := range res.Calls {
		paths[c.Label] = fmt.Sprintf("%s#%d", c.Path, c.Position)
	}
	want := map[string]string{"in-x": "root/1:left/1:kx#1", "in-y": "root/1:left/1:ky#1", "solo": "root/1:right#1", "tail": "root#2"}
	for k, v := range want {
		if paths[k] != v {
			t.Errorf("%s at %s, want %s", k, paths[k], v)
		}
	}
	// duplicate keys are a TypeError
	h = newHarness(t)
	res = h.run(`return await parallel([async () => 1, async () => 2], { keys: ['a', 'a'] })`)
	if res.Status != RunScriptError || !strings.Contains(res.Error, "duplicate item key") {
		t.Errorf("%s %s", res.Status, res.Error)
	}
}

func TestStepLimitStopsRunawayScript(t *testing.T) {
	h := newHarness(t)
	h.cfg.Limits = script.Limits{Step: 100 * time.Millisecond}
	res := h.run(`let n = 0; while (true) { n++ }`)
	if res.Status != RunScriptError || res.ErrorKind != ErrKindStepLimit {
		t.Errorf("%s %s %s", res.Status, res.ErrorKind, res.Error)
	}
	// a runaway step after a call returns is stopped too, and the call is not left pending
	h = newHarness(t)
	h.cfg.Limits = script.Limits{Step: 100 * time.Millisecond}
	res = h.run(`await agent('x', { agentType: 'explorer' }); while (true) {}`)
	if res.Status != RunScriptError || res.ErrorKind != ErrKindStepLimit {
		t.Errorf("%s %s %s", res.Status, res.ErrorKind, res.Error)
	}
}

func TestDeterminismStubsThroughEngine(t *testing.T) {
	h := newHarness(t)
	res := h.run(`
const names = []
for (const f of [() => Date.now(), () => Math.random(), () => new Date()]) { try { f() } catch (e) { names.push(e.name) } }
return names`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"DeterminismError", "DeterminismError", "DeterminismError"}) {
		t.Errorf("%v", got)
	}
}

func TestClockIsJournaledAndTakesAPosition(t *testing.T) {
	h := newHarness(t)
	h.cfg.Now = func() time.Time { return time.Date(2026, 10, 11, 1, 2, 3, 0, time.UTC) }
	res := h.run(`const a = await crw.clock(); const b = await agent('x', { agentType: 'explorer', label: 'x' }); return a`)
	if got := value[string](t, res); got != "2026-10-11T01:02:03Z" {
		t.Errorf("clock %q", got)
	}
	if c := res.Calls[0]; c.Label != "x" || c.Position != 2 {
		t.Errorf("the clock took position 1, so the call sits at 2: %+v", c)
	}
	var clock *Event
	for _, e := range h.journal.Events() {
		if e.Op == "crw.clock" {
			e := e
			clock = &e
		}
	}
	if clock == nil || clock.Data["result"] != `"2026-10-11T01:02:03Z"` || clock.Position != 1 {
		t.Errorf("clock row %+v", clock)
	}
}

func TestBudgetPausesTheRun(t *testing.T) {
	h := newHarness(t)
	h.cfg.TokenBudget = 100
	h.fake.Handler = func(r FakeRequest) FakeReply { return FakeReply{Tokens: &Tokens{Input: 40, Output: 45}} }
	res := h.run(`
const first = await agent.call('one', { agentType: 'explorer' })
const mid = budget.remaining()
const second = await agent.call('two', { agentType: 'explorer' })
const third = await agent.call('three', { agentType: 'explorer' })
return { total: budget.total, spent: budget.spent(), mid, st: [first.status, second.status, third.status], reasons: [second.reason, third.reason] }`)
	v := value[map[string]any](t, res)
	if v["total"] != float64(100) || v["spent"] != float64(170) || v["mid"] != float64(15) || fmt.Sprint(v["st"]) != "[completed completed held]" || fmt.Sprint(v["reasons"]) != "[<nil> budget_exhausted]" {
		t.Errorf("%v", v)
	}
	var warned bool
	for _, e := range h.journal.Events() {
		if e.Event == "budget_warning" {
			warned = true
		}
	}
	if !warned || countTurns(h.fake) != 2 {
		t.Errorf("warned %v turns %d", warned, countTurns(h.fake))
	}
	// no budget: total is null and remaining is infinite
	h = newHarness(t)
	res = h.run(`return [budget.total, budget.remaining() === Infinity, budget.spent()]`)
	if got := value[[]any](t, res); fmt.Sprint(got) != "[<nil> true 0]" {
		t.Errorf("%v", got)
	}
}

func TestPoolLimitsConcurrentCalls(t *testing.T) {
	h := newHarness(t)
	h.cfg.Pool = 2
	var mu sync.Mutex
	cur, peak := 0, 0
	h.fake.Handler = func(r FakeRequest) FakeReply {
		mu.Lock()
		cur++
		peak = max(peak, cur)
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		cur--
		mu.Unlock()
		return FakeReply{}
	}
	res := h.run(`await parallel([0, 1, 2, 3, 4, 5].map((i) => async () => agent('p' + i, { agentType: 'explorer' }))); return 1`)
	if res.Status != RunCompleted || peak != 2 {
		t.Errorf("%s peak %d, want 2", res.Status, peak)
	}
}

func TestWorkflowRunsChildOnItsOwnPath(t *testing.T) {
	h := newHarness(t)
	child, err := script.Load(script.Source{Name: "child.js", Text: []byte("export const meta = { name: 'child' }\nconst r = await agent('child work ' + args.n, { agentType: 'explorer', label: 'child-call' })\nreturn { r, n: args.n }\n")})
	if err != nil {
		t.Fatal(err)
	}
	h.cfg.Workflows = func(name, path string) (*script.Program, error) {
		if name == "kid" || path == "/x/kid.js" {
			return child, nil
		}
		return nil, errors.New("unknown workflow " + name)
	}
	res := h.run(`
const a = await workflow('kid', { n: 1 })
const b = await workflow({ scriptPath: '/x/kid.js' }, { n: 2 })
let nested = 'none'
try { await workflow('missing') } catch (e) { nested = e.message }
return { a, b, nested }`)
	v := value[map[string]any](t, res)
	if fmt.Sprint(v["a"]) != "map[n:1 r:fake answer to: child work 1]" || fmt.Sprint(v["b"]) != "map[n:2 r:fake answer to: child work 2]" || !strings.Contains(fmt.Sprint(v["nested"]), "unknown workflow missing") {
		t.Errorf("%v", v)
	}
	var paths []string
	for _, c := range res.Calls {
		paths = append(paths, fmt.Sprintf("%s#%d", c.Path, c.Position))
	}
	slices.Sort(paths)
	if fmt.Sprint(paths) != "[root/1:workflow#1 root/2:workflow#1]" {
		t.Errorf("paths %v", paths)
	}
	// one nesting level only
	h = newHarness(t)
	grand, _ := script.Load(script.Source{Name: "g.js", Text: []byte("export const meta = { name: 'g' }\nreturn await workflow('kid')\n")})
	h.cfg.Workflows = func(name, path string) (*script.Program, error) { return grand, nil }
	res = h.run(`return await workflow('g')`)
	if res.Status != RunScriptError || !strings.Contains(res.Error, "one level only") {
		t.Errorf("%s: %s", res.Status, res.Error)
	}
	// a child that opens a position on the parent's path is not possible: the parent is busy while it runs
	h = newHarness(t)
	h.cfg.Workflows = func(name, path string) (*script.Program, error) { return child, nil }
	res = h.run(`const w = workflow('kid', { n: 3 }); return await agent('during', { agentType: 'explorer' })`)
	if res.Status != RunScriptError || res.ErrorKind != ErrKindPathConcurrency {
		t.Errorf("%s %s", res.Status, res.ErrorKind)
	}
}

func TestRunRecordHoldsDigests(t *testing.T) {
	h := newHarness(t)
	h.cfg.Args = json.RawMessage(`{"k":1}`)
	res := h.run(`return args.k`)
	if value[int](t, res) != 1 {
		t.Fatal("args")
	}
	var rec Event
	for _, e := range h.journal.Events() {
		if e.Kind == KindRun && e.Event == "started" {
			rec = e
		}
	}
	if rec.Data["script_digest"] == "" || rec.Data["args_digest"] == "" || rec.Data["workflow"] != "t" {
		t.Errorf("run record %+v", rec.Data)
	}
	helpers, _ := rec.Data["helpers"].([]map[string]any)
	if len(helpers) != 1 || helpers[0]["spec"] != "crw:lib/schemas" || helpers[0]["digest"] == "" {
		t.Errorf("helper digests %+v", rec.Data["helpers"])
	}
}

func TestSettledAndUnsettledRuns(t *testing.T) {
	h := newHarness(t)
	res := h.run(`await new Promise(() => {})`)
	if res.Status != RunScriptError || res.ErrorKind != ErrKindUnsettled {
		t.Errorf("%s %s", res.Status, res.ErrorKind)
	}
	h = newHarness(t)
	res = h.run(`throw new TypeError('plain failure')`)
	if res.Status != RunScriptError || res.ErrorKind != ErrKindJS || !strings.Contains(res.Error, "plain failure") {
		t.Errorf("%s %s %s", res.Status, res.ErrorKind, res.Error)
	}
}
