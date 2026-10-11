package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// world is the outside of a run: what the registered ops did. A request is one effect, however many times the engine sends
// it, because the engine sends the same RequestID for the same call of the same run.
type world struct {
	mu sync.Mutex
	// effects counts the requests that made an effect, runs every time an op was executed.
	effects map[string]int
	runs    map[string]int
	done    map[string]bool
	// gone are the request ids whose effect was undone (an acceptance withdrawn, a commit reset away).
	gone map[string]bool
}

func newWorld() *world {
	return &world{effects: map[string]int{}, runs: map[string]int{}, done: map[string]bool{}, gone: map[string]bool{}}
}

type worldOp struct {
	w    *world
	name string
}

// Run makes the effect once per request id and returns the op's result: {id}, with the node and the commit or tree the proofs
// read in the row. The tree of a commit is the pending tree of the call its argument names.
func (o worldOp) Run(_ context.Context, req OpRequest) (OpResult, error) {
	o.w.mu.Lock()
	defer o.w.mu.Unlock()
	o.w.runs[o.name]++
	if !o.w.done[req.RequestID] {
		o.w.done[req.RequestID] = true
		o.w.effects[o.name]++
	}
	var args []any
	_ = json.Unmarshal(req.Args, &args)
	data := map[string]any{}
	if len(args) > 0 {
		if node, ok := args[0].(string); ok {
			data["node"] = node
		}
	}
	id := fmt.Sprintf("%s-%s", o.name, req.RequestID[len(req.RequestID)-6:])
	if o.name == "commit" {
		data["commit"] = id
		if len(args) > 1 {
			if call, ok := args[1].(string); ok {
				for _, ev := range req.Journal.Events() {
					if ev.Kind == KindCall && ev.CallID == call && ev.Event == EvCompleted {
						if tree := rowStr(ev.Data, "pending_tree"); tree != "" {
							data["tree"] = tree
						}
					}
				}
			}
		}
	}
	res, _ := json.Marshal(map[string]any{"id": id})
	return OpResult{Result: res, Data: data}, nil
}

// Proven holds while the effect of the row's request is there.
func (o worldOp) Proven(_ context.Context, _ []Event, row Event) (bool, string, error) {
	o.w.mu.Lock()
	defer o.w.mu.Unlock()
	req := RequestID(row.RunID, row.CallID)
	if !o.w.done[req] || o.w.gone[req] {
		return false, "the effect is gone", nil
	}
	return true, "", nil
}

func (w *world) ops(names ...string) map[string]Op {
	out := map[string]Op{}
	for _, n := range names {
		out[n] = worldOp{w: w, name: n}
	}
	return out
}

func (w *world) count(name string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.effects[name]
}

func (w *world) ran(name string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.runs[name]
}

// crashJournal is the journal of a process that dies after the row at stopAfter: every row from the next one on is lost, and
// the run goes on in memory as if nothing happened.
type crashJournal struct {
	*MemoryJournal
	mu   sync.Mutex
	dead bool
	// stop says whether the row is the last one the process wrote.
	stop func(Event) bool
}

func (c *crashJournal) Append(e Event) Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead {
		return e
	}
	out := c.MemoryJournal.Append(e)
	if c.stop != nil && c.stop(out) {
		c.dead = true
	}
	return out
}

// resumeOn runs the body as a resume of the journal and returns the result and the requests the backend got.
func (h *harness) resumeOn(body string) *Result {
	h.t.Helper()
	h.cfg.Resume = &ResumeOptions{}
	return h.run(body)
}

func (h *harness) turnCount() int {
	n := 0
	for _, r := range h.fake.Requests() {
		if r.Kind == "turn" {
			n++
		}
	}
	return n
}

// liveCalls are the agent calls the backend was asked to run since the mark.
func (h *harness) liveSince(mark int) []string {
	var out []string
	for _, r := range h.fake.Requests()[mark:] {
		if r.Kind == "turn" {
			out = append(out, strings.TrimSpace(r.Prompt))
		}
	}
	return out
}

// treeFake is a fake backend that places executor calls in worktrees whose files it models as one tree id per worktree: a
// turn sets the tree to P:<operation>, which the pending tree of the call that ran it records.
type treeFake struct {
	*Fake
	mu   sync.Mutex
	tree string
}

func newTreeFake() *treeFake {
	t := &treeFake{Fake: NewFake(), tree: "T0"}
	t.Handler = func(r FakeRequest) FakeReply {
		t.mu.Lock()
		t.tree = "P:" + r.Op
		t.mu.Unlock()
		return FakeReply{}
	}
	return t
}

// StartThread reports the directory the thread was started in, as the host reads it back.
func (t *treeFake) StartThread(ctx context.Context, spec ThreadSpec) (ThreadInfo, error) {
	info, err := t.Fake.StartThread(ctx, spec)
	info.Cwd = spec.Cwd
	return info, err
}

func (t *treeFake) Place(req PlaceRequest) (Placement, error) {
	if req.Family != "executor" {
		return Placement{Kind: PlaceCall, Path: "/w/call/" + req.CallID}, nil
	}
	if req.Continues != nil {
		return Placement{Kind: PlaceContinued}, nil
	}
	return Placement{Kind: PlaceFresh, Attempt: 1, Path: "/w/" + req.Node + "/a1", Start: "base"}, nil
}
func (t *treeFake) Establish(context.Context, PlaceRequest, Placement) error { return nil }
func (t *treeFake) Recheck(context.Context, Placement) error                 { return nil }
func (t *treeFake) Snapshot(context.Context, string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tree, nil
}

// ProveNode proves a call whose recorded state is the tree the worktree holds now.
func (t *treeFake) ProveNode(_ context.Context, events []Event, node string) (NodeProof, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	proven := map[string]bool{}
	for _, ev := range events {
		if ev.Kind == KindCall && ev.Op == "agent" && (ev.Event == EvCompleted || ev.Event == EvHeld) {
			key := "pending_tree"
			if ev.Event == EvHeld {
				key = "p_held"
			}
			if st := rowStr(ev.Data, key); st != "" {
				proven[ev.CallID] = st == t.tree
			}
		}
	}
	return mapProof(proven), nil
}

type mapProof map[string]bool

func (m mapProof) Proven(id string) bool { return m[id] }

func (t *treeFake) setTree(tree string) {
	t.mu.Lock()
	t.tree = tree
	t.mu.Unlock()
}

// withTreeFake runs the engine on a treeFake and the world's ops.
func withTreeFake(t *testing.T) (*harness, *treeFake, *world) {
	t.Helper()
	h := newHarness(t)
	tf := newTreeFake()
	w := newWorld()
	h.fake, h.cfg.Backend = tf.Fake, tf
	h.cfg.Ops = w.ops("commit", "accept", "integrate", "hold")
	return h, tf, w
}

// With an unchanged script a resume replays every call: it asks the backend for nothing, runs no op, and returns the value
// the first run returned.
func TestResumeReplaysACompleteRunWithoutLiveCalls(t *testing.T) {
	h, _, w := withTreeFake(t)
	body := `
const b = await agent.call('build', { agentType: 'executor', node: 'n1', label: 'build' })
const c = await crw.commit('n1', b.callId)
const a = await crw.accept('n1', c.id)
return [b.status, c.id, a.id]`
	first := value[[]string](t, h.run(body))
	mark, ran := len(h.fake.Requests()), w.ran("commit")+w.ran("accept")
	again := value[[]string](t, h.resumeOn(body))
	if !slices.Equal(first, again) {
		t.Errorf("resume returned %v, the first run %v", again, first)
	}
	if n := len(h.fake.Requests()) - mark; n != 0 {
		t.Errorf("the resume sent %d requests", n)
	}
	if n := w.ran("commit") + w.ran("accept") - ran; n != 0 {
		t.Errorf("the resume ran %d ops", n)
	}
}

// H1 committed, fix t2 completed and the process died before the H2 commit: the prefix - the build, the H1 commit and the fix -
// is replayed, and the one call that runs live is the commit that was not recorded.
func TestCrashAfterFixBeforeCommitKeepsPrefix(t *testing.T) {
	h, _, w := withTreeFake(t)
	cj := &crashJournal{MemoryJournal: h.journal}
	h.cfg.Journal = cj
	// the process dies when it is about to record the second commit: the commit may have been made, the row is lost
	cj.stop = func(e Event) bool { return e.Kind == KindCall && e.Label == "fix2" && e.Event == EvCompleted }
	body := `
const b = await agent.call('build', { agentType: 'executor', node: 'n1', label: 'build' })
const c1 = await crw.commit('n1', b.callId)
const f2 = await agent.call('fix', { resume: b, label: 'fix2' })
const c2 = await crw.commit('n1', f2.callId)
return [b.status, c1.id, f2.status, c2.id]`
	value[[]string](t, h.run(body))
	h.cfg.Journal = h.journal
	mark := len(h.fake.Requests())
	commits := w.ran("commit")
	got := value[[]string](t, h.resumeOn(body))
	if len(got) != 4 || got[0] != "completed" || got[2] != "completed" {
		t.Fatalf("%v", got)
	}
	if live := h.liveSince(mark); len(live) != 0 {
		t.Errorf("the resume ran agent calls live: %v", live)
	}
	if n := w.ran("commit") - commits; n != 1 {
		t.Errorf("%d commits ran live, want the one that was not recorded", n)
	}
	if n := w.count("commit"); n != 2 {
		t.Errorf("%d commit effects, want 2: one per commit call", n)
	}
	// the row of the live commit exists once: the first commit was replayed, not recorded again
	rows := 0
	for _, ev := range h.journal.Events() {
		if ev.Kind == KindCall && ev.Op == "crw.commit" && ev.Event == EvCompleted {
			rows++
		}
	}
	if rows != 2 {
		t.Errorf("%d commit rows, want one per commit call", rows)
	}
}

// The identity of a call is its path and position: the order the items of a parallel() finish in, and the order the array
// lists them in, change nothing when the keys are the same.
func TestParallelReorderSamePaths(t *testing.T) {
	h := newHarness(t)
	h.fake.Handler = func(r FakeRequest) FakeReply {
		// the item that was listed first finishes last
		if strings.Contains(r.Prompt, "slow") {
			time.Sleep(30 * time.Millisecond)
		}
		return FakeReply{}
	}
	first := h.run(`
return await parallel([
  async () => (await agent.call('slow a', { agentType: 'explorer', label: 'a' })).callId,
  async () => (await agent.call('fast b', { agentType: 'explorer', label: 'b' })).callId,
], { keys: ['a', 'b'] })`)
	ids := value[[]string](t, first)
	mark := len(h.fake.Requests())
	// the resumed script lists the items the other way round and the fast one now finishes first
	h.fake.Handler = nil
	second := value[[]string](t, h.resumeOn(`
return await parallel([
  async () => (await agent.call('fast b', { agentType: 'explorer', label: 'b' })).callId,
  async () => (await agent.call('slow a', { agentType: 'explorer', label: 'a' })).callId,
], { keys: ['b', 'a'] })`))
	if len(second) != 2 || second[0] != ids[1] || second[1] != ids[0] {
		t.Errorf("the call ids %v, the first run's %v: an item keeps its path whatever its place", second, ids)
	}
	if n := len(h.fake.Requests()) - mark; n != 0 {
		t.Errorf("the resume sent %d requests: the items are the same paths", n)
	}
}

// A node that was accepted is not rebuilt silently when its packet changes: the call is refused with the route named, and the
// host is asked for nothing.
func TestAcceptedNodePacketChangeRefusedWithRoute(t *testing.T) {
	h, _, w := withTreeFake(t)
	value[[]string](t, h.run(`
const b = await agent.call('build the parser', { agentType: 'executor', node: 'n1', label: 'build' })
const a = await crw.accept('n1', b.callId)
return [b.status, a.id]`))
	mark, accepted := len(h.fake.Requests()), w.count("accept")
	res := h.resumeOn(`
const b = await agent.call('build the parser differently', { agentType: 'executor', node: 'n1', label: 'build' })
return [b.status, b.reason]`)
	got := value[[]string](t, res)
	if got[0] != "refused" || got[1] != ReasonAcceptedNodeChanged {
		t.Fatalf("%v", got)
	}
	if n := len(h.fake.Requests()) - mark; n != 0 {
		t.Errorf("a refused call sent %d requests", n)
	}
	var detail string
	for _, ev := range h.journal.Events() {
		if ev.Kind == KindCall && ev.Event == EvRefused && ev.Label == "build" && ev.Seq > 0 {
			detail = rowStr(ev.Data, "detail")
		}
	}
	for _, route := range []string{"update_node", "dag-correct", "n1"} {
		if !strings.Contains(detail, route) {
			t.Errorf("the refusal %q does not name %q", detail, route)
		}
	}
	if w.count("accept") != accepted {
		t.Errorf("the acceptance was made again")
	}
}

// Nothing is reused across (path, position): the same call recorded at another position, or by an earlier lineage, is not
// a result for this one.
func TestNoGlobalCacheAcrossLineages(t *testing.T) {
	h := newHarness(t)
	h.run(`
const x = await agent.call('call x', { agentType: 'explorer', label: 'x' })
const y = await agent.call('call y', { agentType: 'explorer', label: 'y' })
return [x.status, y.status]`)
	mark := len(h.fake.Requests())
	// the same two calls in the other order: each is at a position that holds the other, so both run
	h.resumeOn(`
const y = await agent.call('call y', { agentType: 'explorer', label: 'y' })
const x = await agent.call('call x', { agentType: 'explorer', label: 'x' })
return [x.status, y.status]`)
	if live := h.liveSince(mark); !slices.Equal(live, []string{"call y", "call x"}) {
		t.Errorf("lineage 1 ran %v: a result is reused only at its own position", live)
	}
	mark = len(h.fake.Requests())
	// the first script again: the journal's newest attempt of each position is the other order's, so both run again
	h.resumeOn(`
const x = await agent.call('call x', { agentType: 'explorer', label: 'x' })
const y = await agent.call('call y', { agentType: 'explorer', label: 'y' })
return [x.status, y.status]`)
	if live := h.liveSince(mark); !slices.Equal(live, []string{"call x", "call y"}) {
		t.Errorf("lineage 2 ran %v: an earlier lineage's result is not a cache", live)
	}
	// and the third lineage that changes nothing replays both
	mark = len(h.fake.Requests())
	h.resumeOn(`
const x = await agent.call('call x', { agentType: 'explorer', label: 'x' })
const y = await agent.call('call y', { agentType: 'explorer', label: 'y' })
return [x.status, y.status]`)
	if n := len(h.fake.Requests()) - mark; n != 0 {
		t.Errorf("an unchanged resume sent %d requests", n)
	}
}

// Whatever row the process died after, a resume does not run a call whose end was recorded, and no op makes its effect twice.
func TestKillAtEachCallNoDuplicateEffect(t *testing.T) {
	body := `
const b = await agent.call('build', { agentType: 'executor', node: 'n1', label: 'build' })
const c1 = await crw.commit('n1', b.callId)
const f2 = await agent.call('fix', { resume: b, label: 'fix2' })
const c2 = await crw.commit('n1', f2.callId)
const a = await crw.accept('n1', c2.id)
const i = await crw.integrate('n1', a.id)
return [b.status, c1.id, f2.status, c2.id, a.id, i.id]`
	h, _, w := withTreeFake(t)
	want := value[[]string](t, h.run(body))
	full := h.journal.Events()
	endedAgent := func(rows []Event) map[string]bool {
		out := map[string]bool{}
		for _, ev := range rows {
			if ev.Kind == KindCall && ev.Op == "agent" && ev.Event == EvCompleted {
				out[ev.CallID] = true
			}
		}
		return out
	}
	for cut := 1; cut <= len(full); cut++ {
		for _, effectBeforeRow := range []bool{false, true} {
			t.Run(fmt.Sprintf("after row %d effect %v", cut, effectBeforeRow), func(t *testing.T) {
				h2, tf2, w2 := withTreeFake(t)
				// the process died after `cut` rows; with effectBeforeRow the op after them had made its effect already
				for _, ev := range full[:cut] {
					h2.journal.Append(ev)
				}
				for _, ev := range full[:cut] {
					if ev.Kind == KindCall && strings.HasPrefix(ev.Op, opPrefix) && ev.Event == EvCompleted {
						w2.done[RequestID(ev.RunID, ev.CallID)] = true
						w2.effects[strings.TrimPrefix(ev.Op, opPrefix)]++
					}
				}
				if effectBeforeRow && cut < len(full) {
					next := full[cut]
					if next.Kind == KindCall && strings.HasPrefix(next.Op, opPrefix) {
						w2.done[RequestID(next.RunID, next.CallID)] = true
						w2.effects[strings.TrimPrefix(next.Op, opPrefix)]++
					}
				}
				seedBackend(h2.fake, tf2, full[:cut])
				ended := endedAgent(full[:cut])
				got := value[[]string](t, h2.resumeOn(body))
				if !slices.Equal(got, want) {
					t.Errorf("the resumed run returned %v, an uninterrupted one %v", got, want)
				}
				for _, r := range h2.fake.Requests() {
					if ended[r.CallID] {
						t.Errorf("call %s ended before the kill and was sent again: %+v", r.CallID, r)
					}
				}
				for _, name := range []string{"commit", "accept", "integrate"} {
					if n, wantN := w2.count(name), w.count(name); n != wantN {
						t.Errorf("%s made %d effects, want %d", name, n, wantN)
					}
				}
			})
		}
	}
}

// seedBackend gives a fresh backend the threads the rows of a dead process created and the files it had left: the pending tree
// of the last executor call that ended.
func seedBackend(f *Fake, tf *treeFake, rows []Event) {
	tree := "T0"
	for _, ev := range rows {
		if ev.Kind != KindCall || ev.Op != "agent" {
			continue
		}
		switch ev.Event {
		case EvThreadStarted:
			f.threads[rowStr(ev.Data, "thread_id")] = &FakeThread{ID: rowStr(ev.Data, "thread_id"), Pair: Pair{Model: rowStr(ev.Data, "model"), Effort: rowStr(ev.Data, "effort")}}
		case EvTurnStarted:
			for _, th := range f.threads {
				if strings.HasPrefix(rowStr(ev.Data, "op"), strings.TrimPrefix(th.ID, "thr_")+"/") {
					th.Turns = append(th.Turns, rowStr(ev.Data, "op"))
				}
			}
		case EvCompleted:
			if p := rowStr(ev.Data, "pending_tree"); p != "" {
				tree = p
			}
		}
	}
	tf.setTree(tree)
}

// A join whose item path had to run live does not replay, so the positions after it run live too, though their own
// arguments are unchanged.
func TestJoinAfterChildSuffixRerunsLaterPositions(t *testing.T) {
	h := newHarness(t)
	body := func(second string) string {
		return `
const items = await pipeline(['a', 'b'], async (x) => (await agent.call('stage1 ' + x + ' ' + (x === 'a' ? '` + second + `' : ''), { agentType: 'explorer', label: 's1' + x })).status)
const after = await agent.call('after the join', { agentType: 'explorer', label: 'after' })
return [items, after.status]`
	}
	h.run(body("v1"))
	mark := len(h.fake.Requests())
	h.resumeOn(body("v2"))
	live := h.liveSince(mark)
	if len(live) != 2 || !strings.HasPrefix(live[0], "stage1 a v2") || live[1] != "after the join" {
		t.Errorf("the resume ran %v: item a's stage and the position after the join", live)
	}
	// nothing changed: the join replays and so does the position after it
	mark = len(h.fake.Requests())
	h.resumeOn(body("v2"))
	if n := len(h.fake.Requests()) - mark; n != 0 {
		t.Errorf("an unchanged resume sent %d requests", n)
	}
}

// The suffix of one item does not reach its siblings: they are other paths and replay with no live call.
func TestSiblingPathStaysReplayedWhenOneSuffixRuns(t *testing.T) {
	h := newHarness(t)
	body := func(a2 string) string {
		return `
return await parallel([
  async () => { await agent.call('A1', { agentType: 'explorer', label: 'a1' }); return (await agent.call('A2 ` + a2 + `', { agentType: 'explorer', label: 'a2' })).status },
  async () => { await agent.call('B1', { agentType: 'explorer', label: 'b1' }); return (await agent.call('B2', { agentType: 'explorer', label: 'b2' })).status },
], { keys: ['A', 'B'] })`
	}
	h.run(body("one"))
	mark := len(h.fake.Requests())
	h.resumeOn(body("two"))
	live := h.liveSince(mark)
	if len(live) != 1 || !strings.HasPrefix(live[0], "A2 two") {
		t.Errorf("the resume ran %v: only item A's changed position is live, item B's prefix replays", live)
	}
}

// Only a position after the failing one makes a path a suffix: the paths an earlier position opened, and the sibling lanes
// that were released after the lane, replay.
func TestSuffixPathsOnlyFromLaterPositionsOnSamePath(t *testing.T) {
	h := newHarness(t)
	body := func(a2 string) string {
		return `
return await parallel([
  async () => {
    await parallel([async () => agent.call('A-early', { agentType: 'explorer', label: 'early' })], { keys: ['x'] })
    await agent.call('A2 ` + a2 + `', { agentType: 'explorer', label: 'a2' })
    await parallel([async () => agent.call('A-late', { agentType: 'explorer', label: 'late' })], { keys: ['y'] })
    return 'A'
  },
  async () => { await agent.call('B1', { agentType: 'explorer', label: 'b1' }); await agent.call('B2', { agentType: 'explorer', label: 'b2' }); return 'B' },
], { keys: ['A', 'B'] })`
	}
	h.run(body("one"))
	mark := len(h.fake.Requests())
	h.resumeOn(body("two"))
	live := h.liveSince(mark)
	if len(live) != 2 || !strings.HasPrefix(live[0], "A2 two") || live[1] != "A-late" {
		t.Errorf("the resume ran %v: the failing position of A and the path a later position of A opened; A-early and lane B replay", live)
	}
}

// The context boundary is part of the input digest: the same prompt on a continuation of another turn is another call.
func TestContextBoundaryInInputDigest(t *testing.T) {
	digestOf := func(handle string) string {
		h := newHarness(t)
		h.run(`
const first = await agent.call('one', { agentType: 'explorer', label: 'first' })
const second = await agent.call('two', { resume: first, label: 'second' })
const third = await agent.call('same prompt', { resume: ` + handle + `, label: 'third' })
return third.status`)
		for _, ev := range h.journal.Events() {
			if ev.Kind == KindCall && ev.Event == EvPrepared && ev.Label == "third" {
				return rowStr(ev.Data, "input_digest")
			}
		}
		t.Fatal("no prepared row")
		return ""
	}
	if a, b := digestOf("first"), digestOf("second"); a == b {
		t.Errorf("the continuation of the first turn and of the second turn share the input digest %s", a)
	}
	if a, b := digestOf("second"), digestOf("second"); a != b {
		t.Errorf("the digest is not deterministic: %s %s", a, b)
	}
	// a resume whose continuation resumes another turn does not replay the recorded call
	h := newHarness(t)
	h.run(`
const first = await agent.call('one', { agentType: 'explorer', label: 'first' })
const second = await agent.call('two', { resume: first, label: 'second' })
const third = await agent.call('same prompt', { resume: second, label: 'third' })
return third.status`)
	mark := len(h.fake.Requests())
	h.resumeOn(`
const first = await agent.call('one', { agentType: 'explorer', label: 'first' })
const second = await agent.call('two', { resume: first, label: 'second' })
const third = await agent.call('same prompt', { resume: first, label: 'third' })
return third.status`)
	if live := h.liveSince(mark); !slices.Equal(live, []string{"same prompt"}) {
		t.Errorf("the resume ran %v, want only the continuation whose context changed", live)
	}
}

// A call refused before it opened ends the same way on every run, so it is a position that replays: the calls after it replay
// too.
func TestRefusedCallReplaysAndKeepsThePathReplayed(t *testing.T) {
	h := newHarness(t)
	body := `
const refused = await agent.call('no role named', { label: 'refused' })
const ok = await agent.call('real work', { agentType: 'explorer', label: 'ok' })
return [refused.status, refused.reason, ok.status]`
	first := value[[]string](t, h.run(body))
	if first[0] != "refused" {
		t.Fatalf("%v", first)
	}
	mark := len(h.fake.Requests())
	if again := value[[]string](t, h.resumeOn(body)); !slices.Equal(first, again) {
		t.Errorf("resume returned %v, the first run %v", again, first)
	}
	if n := len(h.fake.Requests()) - mark; n != 0 {
		t.Errorf("the resume sent %d requests", n)
	}
}

// The time crw.clock() returned is journaled: a resumed script reads the time it read before, not the time of the resume.
func TestClockReplaysItsJournaledTime(t *testing.T) {
	h := newHarness(t)
	first := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	h.cfg.Now = func() time.Time { return first }
	body := `const t = await crw.clock(); const a = await agent.call('after the clock', { agentType: 'explorer', label: 'a' }); return [t, a.status]`
	want := value[[]string](t, h.run(body))
	h.cfg.Now = func() time.Time { return first.Add(time.Hour) }
	mark := len(h.fake.Requests())
	got := value[[]string](t, h.resumeOn(body))
	if !slices.Equal(got, want) {
		t.Errorf("the resumed script read %v, the first run %v", got, want)
	}
	if n := len(h.fake.Requests()) - mark; n != 0 {
		t.Errorf("the resume sent %d requests", n)
	}
}

// The input snapshot is part of what a position must match: the plan-owned facts a call saw are compared with the relay's now,
// and a call whose facts changed is another call.
func TestInputSnapshotChangeIsAnotherCall(t *testing.T) {
	h := newHarness(t)
	snapshot := "plan revision 1"
	h.cfg.InputSnapshot = func(_ context.Context, r SnapshotRequest) (string, error) { return r.Node + " at " + snapshot, nil }
	body := `
const a = await agent.call('build', { agentType: 'executor', node: 'n1', label: 'a' })
const b = await agent.call('review', { agentType: 'explorer', label: 'b' })
return [a.status, b.status]`
	h.run(body)
	mark := len(h.fake.Requests())
	h.resumeOn(body)
	if n := len(h.fake.Requests()) - mark; n != 0 {
		t.Errorf("an unchanged snapshot sent %d requests", n)
	}
	snapshot = "plan revision 2"
	mark = len(h.fake.Requests())
	h.resumeOn(body)
	if live := h.liveSince(mark); !slices.Equal(live, []string{"build", "review"}) {
		t.Errorf("a changed snapshot ran %v: the call and the rest of its path", live)
	}
	var recorded string
	for _, ev := range h.journal.Events() {
		if ev.Kind == KindCall && ev.Event == EvPrepared && ev.Label == "a" {
			recorded = rowStr(ev.Data, "input_snapshot")
		}
	}
	if recorded != "n1 at plan revision 2" {
		t.Errorf("the prepared row's input_snapshot is %q", recorded)
	}
}

// A workflow() position is a join of its one child path: it replays when nothing under it ran live and its value is the
// recorded one, and a change in the child makes the positions after it live.
func TestWorkflowPositionReplaysWithItsChild(t *testing.T) {
	h := newHarness(t)
	child := func(prompt string) *script.Program {
		p, err := script.Load(script.Source{Name: "kid.js", Text: []byte(header + "const r = await agent.call('" + prompt + "', { agentType: 'explorer', label: 'kid' }); return r.status\n")})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	prompt := "child work"
	h.cfg.Workflows = func(name, path string) (*script.Program, error) { return child(prompt), nil }
	body := `
const k = await workflow('kid')
const after = await agent.call('after the workflow', { agentType: 'explorer', label: 'after' })
return [k, after.status]`
	h.run(body)
	mark := len(h.fake.Requests())
	h.resumeOn(body)
	if n := len(h.fake.Requests()) - mark; n != 0 {
		t.Errorf("an unchanged resume sent %d requests", n)
	}
	prompt = "child work, changed"
	mark = len(h.fake.Requests())
	h.resumeOn(body)
	if live := h.liveSince(mark); !slices.Equal(live, []string{"child work, changed", "after the workflow"}) {
		t.Errorf("a changed child ran %v: its call and the position after the workflow", live)
	}
}

// A call that failed after its packet was delivered goes on in place on any backend: the next turn of the same call, on the
// same thread, with the continuation instruction, and the call keeps its attempt.
func TestHeldDeliveredCallGoesOnInPlaceOnTheFakeBackend(t *testing.T) {
	h := newHarness(t)
	h.fake.Handler = func(r FakeRequest) FakeReply {
		if strings.HasSuffix(r.Op, "/t1") {
			return FakeReply{Fail: &TurnFailure{CodexErrorInfo: "serverOverloaded", Message: "busy"}}
		}
		return FakeReply{}
	}
	body := `const r = await agent.call('review it', { agentType: 'reviewer', label: 'r' }); return [r.status, r.reason]`
	if got := value[[]string](t, h.run(body)); got[0] != "held" {
		t.Fatalf("%v", got)
	}
	thread := ""
	for _, r := range h.fake.Requests() {
		if r.Kind == "turn" {
			thread = r.ThreadID
		}
	}
	mark := len(h.fake.Requests())
	if got := value[[]string](t, h.resumeOn(body)); got[0] != "completed" {
		t.Fatalf("%v", got)
	}
	sent := h.fake.Requests()[mark:]
	if len(sent) != 1 || sent[0].Kind != "turn" || sent[0].ThreadID != thread || !strings.HasSuffix(sent[0].Op, "/t2") {
		t.Fatalf("the resume sent %+v, want one t2 turn on %s", sent, thread)
	}
	if !strings.Contains(sent[0].Prompt, "Continue the same task") || strings.Contains(sent[0].Prompt, "review it") {
		t.Errorf("the t2 input %q", sent[0].Prompt)
	}
	prepared := 0
	for _, ev := range h.journal.Events() {
		if ev.Kind == KindCall && ev.Event == EvPrepared {
			prepared++
		}
	}
	if prepared != 1 {
		t.Errorf("%d prepared rows", prepared)
	}
	// a held call whose resume is held again appends a second held row and goes on the next time
	h2 := newHarness(t)
	h2.fake.Handler = func(r FakeRequest) FakeReply {
		return FakeReply{Fail: &TurnFailure{CodexErrorInfo: "serverOverloaded", Message: "busy"}}
	}
	h2.run(body)
	if got := value[[]string](t, h2.resumeOn(body)); got[0] != "held" {
		t.Fatalf("%v", got)
	}
	held := 0
	for _, ev := range h2.journal.Events() {
		if ev.Kind == KindCall && ev.Event == EvHeld {
			held++
		}
	}
	if held != 2 {
		t.Errorf("%d held rows, want one for each try", held)
	}
}

// A node that was accepted is refused, with the route, when its packet changed in a path that is itself in a suffix: the earlier
// position's change does not hide it.
func TestAcceptedNodeIsRefusedInASuffixToo(t *testing.T) {
	h, _, _ := withTreeFake(t)
	value[[]string](t, h.run(`
const e = await agent.call('earlier', { agentType: 'explorer', label: 'e' })
const b = await agent.call('build the parser', { agentType: 'executor', node: 'n1', label: 'build' })
const a = await crw.accept('n1', b.callId)
return [b.status, a.id]`))
	res := h.resumeOn(`
const e = await agent.call('earlier, edited', { agentType: 'explorer', label: 'e' })
const b = await agent.call('build the parser differently', { agentType: 'executor', node: 'n1', label: 'build' })
return [b.status, b.reason]`)
	if got := value[[]string](t, res); got[1] != ReasonAcceptedNodeChanged {
		t.Fatalf("%v", got)
	}
}
