package journal

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// world is the outside of a run: what the ops did. A request made once is one effect, however often the engine sends it.
type world struct {
	mu      sync.Mutex
	effects map[string]int
	runs    int
	done    map[string]bool
}

func newWorld() *world { return &world{effects: map[string]int{}, done: map[string]bool{}} }

type worldOp struct {
	w    *world
	name string
}

func (o worldOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	o.w.mu.Lock()
	defer o.w.mu.Unlock()
	o.w.runs++
	if !o.w.done[req.RequestID] {
		o.w.done[req.RequestID] = true
		o.w.effects[o.name]++
	}
	var args []any
	_ = json.Unmarshal(req.Args, &args)
	data := map[string]any{}
	if len(args) > 0 {
		data["node"] = args[0]
	}
	id := fmt.Sprintf("%s-%s", o.name, req.RequestID[len(req.RequestID)-6:])
	if o.name == "commit" {
		data["commit"] = id
	}
	res, _ := json.Marshal(map[string]any{"id": id})
	return engine.OpResult{Result: res, Data: data}, nil
}

func (o worldOp) Proven(_ context.Context, _ []engine.Event, row engine.Event) (bool, string, error) {
	o.w.mu.Lock()
	defer o.w.mu.Unlock()
	return o.w.done[engine.RequestID(row.RunID, row.CallID)], "", nil
}

func (w *world) ops() map[string]engine.Op {
	out := map[string]engine.Op{}
	for _, n := range []string{"commit", "accept", "integrate"} {
		out[n] = worldOp{w: w, name: n}
	}
	return out
}

// diedAfter is the journal of a process that dies after a row: it writes nothing from the next row on, and the run goes on in
// memory (the rows that were buffered at that moment are committed first, as a process that dies between two writes leaves
// them).
type diedAfter struct {
	*Journal
	mu   sync.Mutex
	dead bool
	stop func(engine.Event) bool
}

func (d *diedAfter) Append(e engine.Event) engine.Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dead {
		return e
	}
	out := d.Journal.Append(e)
	if d.stop != nil && d.stop(out) {
		_ = d.Journal.Flush()
		d.dead = true
	}
	return out
}

func runWith(t *testing.T, j engine.Journal, fake *engine.Fake, w *world, resume *engine.ResumeOptions, body string) *engine.Result {
	t.Helper()
	prog, err := script.Load(script.Source{Name: "t.js", Text: []byte(header + body)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Run(ctx, prog, engine.Config{Backend: fake, Policy: engine.FakePolicy(), Journal: j, RunID: runID, Now: fixedNow, Resume: resume, Ops: w.ops()})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// zeroLive runs the script, kills the process after the row the stop function names, reopens the store in a new process and
// resumes: nothing is live - the backend is asked for nothing, no op runs, the effects stay what they were - and the value is
// the one the first run computed.
func zeroLive(t *testing.T, body string, stop func(engine.Event) bool) {
	t.Helper()
	st, path := newStore(t)
	j := newJournal(t, st)
	w := newWorld()
	first := engine.NewFake()
	cj := &diedAfter{Journal: j, stop: stop}
	want := runWith(t, cj, first, w, nil, body)
	if want.Status != engine.RunCompleted {
		t.Fatalf("the first run: %s %s", want.Status, want.Error)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	again := openStore(t, path)
	j2 := newJournal(t, again)
	second := engine.NewFake()
	before := len(j2.Events())
	effects, runs := map[string]int{}, w.runs
	for k, v := range w.effects {
		effects[k] = v
	}
	got := runWith(t, j2, second, w, &engine.ResumeOptions{}, body)
	if got.Status != engine.RunCompleted {
		t.Fatalf("the resume: %s %s", got.Status, got.Error)
	}
	if string(got.Value) != string(want.Value) {
		t.Errorf("the resume returned %s, the first run %s", got.Value, want.Value)
	}
	if n := len(second.Requests()); n != 0 {
		t.Errorf("the resume sent %d requests to the backend", n)
	}
	if n := w.runs - runs; n != 0 {
		t.Errorf("the resume ran %d ops", n)
	}
	for k, v := range w.effects {
		if v != effects[k] {
			t.Errorf("the resume made another %s effect", k)
		}
	}
	for _, e := range j2.Events()[before:] {
		if e.Kind == engine.KindCall && e.Event == engine.EvPrepared {
			t.Errorf("the resume prepared a call: %+v", e)
		}
	}
}

const buildAndCommit = `
const b = await agent.call('build', { agentType: 'executor', node: 'n1', label: 'build' })
const c = await crw.commit('n1', b.callId)
`

func opDone(name string) func(engine.Event) bool {
	return func(e engine.Event) bool {
		return e.Kind == engine.KindCall && e.Op == "crw."+name && e.Event == engine.EvCompleted
	}
}

// A complete run resumes with no live call.
func TestResumeZeroLiveAfterComplete(t *testing.T) {
	zeroLive(t, buildAndCommit+`
const a = await crw.accept('n1', c.id)
const i = await crw.integrate('n1', a.id)
return [b.status, c.id, a.id, i.id]`, nil)
}

// A crash right after the commit row: nothing is live on resume.
func TestResumeZeroLiveAfterCommit(t *testing.T) {
	zeroLive(t, buildAndCommit+`return [b.status, c.id]`, opDone("commit"))
}

// A crash right after the accept row.
func TestResumeZeroLiveAfterAccept(t *testing.T) {
	zeroLive(t, buildAndCommit+`
const a = await crw.accept('n1', c.id)
return [b.status, c.id, a.id]`, opDone("accept"))
}

// A crash right after the integrate row.
func TestResumeZeroLiveAfterIntegrate(t *testing.T) {
	zeroLive(t, buildAndCommit+`
const a = await crw.accept('n1', c.id)
const i = await crw.integrate('n1', a.id)
return [b.status, c.id, a.id, i.id]`, opDone("integrate"))
}

// The join digest and the rows of a resumed run survive the store: a parallel() and a pipeline() run to the end,
// the store is reopened by another process, and the resume replays every position - the join digests it compares are the ones
// read back from flow_events.
func TestResumeReplaysJoinsFromTheStore(t *testing.T) {
	body := `
const both = await parallel([
  async () => (await agent.call('left', { agentType: 'explorer', label: 'l' })).status,
  async () => (await agent.call('right', { agentType: 'explorer', label: 'r' })).status,
], { keys: ['l', 'r'] })
const piped = await pipeline(['a', 'b'], async (x) => (await agent.call('stage ' + x, { agentType: 'explorer', label: 's' + x })).status)
const after = await agent.call('after the joins', { agentType: 'explorer', label: 'after' })
return [both, piped, after.status]`
	st, path := newStore(t)
	j := newJournal(t, st)
	w := newWorld()
	first := engine.NewFake()
	want := runWith(t, j, first, w, nil, body)
	if want.Status != engine.RunCompleted || len(first.Requests()) == 0 {
		t.Fatalf("%s %s", want.Status, want.Error)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	again := openStore(t, path)
	j2 := newJournal(t, again)
	settled := 0
	for _, e := range j2.Events() {
		if e.Kind == engine.KindFlow && e.Event == "join_settled" {
			settled++
			if e.Data["digest"] == "" || e.Data["digest"] == nil {
				t.Errorf("join_settled without a digest: %+v", e)
			}
		}
	}
	if settled != 2 {
		t.Fatalf("%d join_settled rows in the store", settled)
	}
	second := engine.NewFake()
	got := runWith(t, j2, second, w, &engine.ResumeOptions{}, body)
	if string(got.Value) != string(want.Value) {
		t.Errorf("the resume returned %s, the run %s", got.Value, want.Value)
	}
	if n := len(second.Requests()); n != 0 {
		t.Errorf("the resume sent %d requests", n)
	}
}
