package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fakes of the session call tests (CRW-1331): a SessionHost that holds the subagents the owner thread spawned, an orchestrator
// that takes the steps the run publishes on a real FileStepBus and reports what it spawned, and a placer that gives every call its
// directory P and models the files and the HEAD of each.

const sessionOwner = "thr-owner"

// orchCwd is the directory the owner thread runs in: every subagent inherits it, and it is never a call's directory.
const orchCwd = "/orch"

type fakeAgent struct {
	SessionAgent
	turns  []*SessionTurn // oldest first
	closed bool
	gone   bool
	// back is the number of reads a notLoaded thread still shows notLoaded before it shows idle again (a host that lost the
	// thread for a moment).
	back int
}

type fakeSessionHost struct {
	mu         sync.Mutex
	agents     map[string]*fakeAgent
	order      []string
	tokens     map[string]Tokens
	interrupts []string
	// down makes every read fail as a host that cannot be reached would.
	down bool
}

func newFakeSessionHost() *fakeSessionHost {
	return &fakeSessionHost{agents: map[string]*fakeAgent{}, tokens: map[string]Tokens{}}
}

func (h *fakeSessionHost) ReadAgent(_ context.Context, id string) (SessionAgent, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.down {
		return SessionAgent{}, fmt.Errorf("the control socket is not answering")
	}
	a, ok := h.agents[id]
	if !ok || a.gone {
		return SessionAgent{}, fmt.Errorf("thread %s: %w", id, ErrSessionAgentNotFound)
	}
	out := a.SessionAgent
	if a.back > 0 && a.Status == "notLoaded" {
		if a.back--; a.back == 0 {
			a.Status = "idle"
		}
	}
	return out, nil
}

func (h *fakeSessionHost) Turns(_ context.Context, id string, limit int, full bool) ([]SessionTurn, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.down {
		return nil, fmt.Errorf("the control socket is not answering")
	}
	a, ok := h.agents[id]
	if !ok || a.gone {
		return nil, fmt.Errorf("thread %s: %w", id, ErrSessionAgentNotFound)
	}
	var out []SessionTurn
	for i := len(a.turns) - 1; i >= 0; i-- {
		t := *a.turns[i]
		if !full {
			t.FinalText, t.FinalPhase = "", ""
		}
		out = append(out, t)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func (h *fakeSessionHost) FindByMarker(_ context.Context, owner, op string) (string, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.down {
		return "", false, fmt.Errorf("the control socket is not answering")
	}
	for _, id := range h.order {
		a := h.agents[id]
		if a.ParentThreadID != owner || len(a.turns) == 0 {
			continue
		}
		if first, _, _ := strings.Cut(a.turns[0].FirstUserText, "\n"); first == "crw-flow-op: "+op {
			return id, true, nil
		}
	}
	return "", false, nil
}

func (h *fakeSessionHost) Interrupt(_ context.Context, id, turn string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.interrupts = append(h.interrupts, id+"/"+turn)
	if a, ok := h.agents[id]; ok {
		for _, t := range a.turns {
			if t.ID == turn && t.Status == "inProgress" {
				t.Status = "interrupted"
			}
		}
		a.Status = "idle"
	}
	return nil
}

func (h *fakeSessionHost) Tokens(_ context.Context, rollout string) (Tokens, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.tokens[rollout]
	return t, ok
}

func (h *fakeSessionHost) agent(id string) fakeAgent {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.agents[id]
	if a == nil {
		return fakeAgent{}
	}
	out := *a
	out.turns = nil
	for _, t := range a.turns {
		c := *t
		out.turns = append(out.turns, &c)
	}
	return out
}

func (h *fakeSessionHost) interrupted() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.interrupts...)
}

// set changes an agent and its newest turn under the lock.
func (h *fakeSessionHost) set(id string, f func(a *fakeAgent, t *SessionTurn)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.agents[id]
	if a == nil {
		return
	}
	var t *SessionTurn
	if len(a.turns) > 0 {
		t = a.turns[len(a.turns)-1]
	}
	f(a, t)
}

// orchAnswer is what a subagent's turn comes to.
type orchAnswer struct {
	// text and phase are the answer of a completed turn (phase final_answer when empty and not raw).
	text, phase string
	rawPhase    bool
	// fail is the codexErrorInfo of a failed turn.
	fail string
	// hang keeps the turn running on an active thread; lose leaves it running on a thread the host no longer has loaded.
	hang, lose bool
	// after completes the turn after the delay (the thread stays active until then).
	after time.Duration
	// noDispatch spawns the subagent and reports nothing.
	noDispatch bool
	// skip leaves the step untaken: the owner never got to that file (a file a process publishes again is a new step it takes).
	skip bool
	// pair spawns with this pair instead of the step's.
	pair *Pair
	// edit sets the tree (and with head the HEAD) of the call's directory, as the subagent's edits would.
	edit, head string
}

// fakeOrch is the owner thread: it takes each step once, spawns or sends input, and reports.
type fakeOrch struct {
	bus    *FileStepBus
	host   *fakeSessionHost
	placer *sessPlacer
	answer func(st Step, packet string, n int) orchAnswer
	// onSpawn is called after a spawn, before the report; onInput after a send_input, before the report.
	onSpawn func(st Step, agent string)
	onInput func(st Step, agent string)

	mu      sync.Mutex
	seen    map[string]bool
	spawns  []Step
	inputs  []Step
	closed  []string
	reports []DispatchReport
	n       int
	stop    chan struct{}
	done    chan struct{}
}

func newFakeOrch(bus *FileStepBus, host *fakeSessionHost, placer *sessPlacer) *fakeOrch {
	return &fakeOrch{bus: bus, host: host, placer: placer, seen: map[string]bool{}, stop: make(chan struct{}), done: make(chan struct{})}
}

func (o *fakeOrch) start(t *testing.T) {
	go o.loop()
	t.Cleanup(func() {
		close(o.stop)
		<-o.done
	})
}

func (o *fakeOrch) loop() {
	defer close(o.done)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-o.stop:
			return
		case <-tick.C:
		}
		steps, _ := o.bus.Steps()
		for _, st := range steps {
			// a step file is taken once: the same file seen again is the step already taken, and a step a process publishes again
			// (a new file) is a new one, as the owner thread sees it
			key := fmt.Sprintf("%s/%s/t%d-a%d/%s", st.Call, st.Action, st.K, st.Attempt, st.AgentID)
			if st.Action != StepClose {
				fi, err := os.Stat(filepath.Join(o.bus.StepsDir(st.Call), StepName(st.K, st.Attempt)))
				if err != nil {
					// retracted since it was read: no step to take
					continue
				}
				key += "@" + fi.ModTime().Format(time.RFC3339Nano)
			}
			o.mu.Lock()
			seen := o.seen[key]
			o.seen[key] = true
			o.mu.Unlock()
			if seen {
				continue
			}
			o.take(st)
		}
	}
}

// forget lets the orchestrator take a step again (a step a resumed run shows again after the process that published it died).
func (o *fakeOrch) forget() {
	o.mu.Lock()
	o.seen = map[string]bool{}
	o.mu.Unlock()
}

func (o *fakeOrch) take(st Step) {
	if st.Action == StepClose {
		o.mu.Lock()
		o.closed = append(o.closed, st.AgentID)
		o.mu.Unlock()
		o.host.set(st.AgentID, func(a *fakeAgent, _ *SessionTurn) { a.closed = true })
		_ = os.Remove(filepath.Join(o.bus.StepsDir(st.Call), st.File))
		return
	}
	packet, _ := os.ReadFile(st.Packet)
	// the hooks are read under the lock: a test that swaps them while the orchestrator runs (runDeath, between its two processes) swaps
	// them under it too (CRW-1392)
	o.mu.Lock()
	o.n++
	n := o.n
	answer, onSpawn, onInput := o.answer, o.onSpawn, o.onInput
	o.mu.Unlock()
	var ans orchAnswer
	turns := 0
	if st.Action == StepSendInput {
		turns = len(o.host.agent(st.AgentID).turns)
	}
	if answer != nil {
		ans = answer(st, string(packet), turns)
	}
	if ans.skip {
		return
	}
	if ans.text == "" && ans.fail == "" && !ans.hang && !ans.lose {
		ans.text = defaultMessage(st.Message, packetSchema(string(packet)))
	}
	agent := st.AgentID
	turnID := ""
	o.host.mu.Lock()
	if st.Action == StepSpawn {
		agent = fmt.Sprintf("agent-%d", n)
		pair := Pair{Model: st.Model, Effort: st.ReasoningEffort}
		if ans.pair != nil {
			pair = *ans.pair
		}
		o.host.agents[agent] = &fakeAgent{SessionAgent: SessionAgent{ThreadID: agent, ParentThreadID: sessionOwner, Model: pair.Model, Effort: pair.Effort,
			Cwd: orchCwd, AgentRole: st.AgentType, Status: "active", RolloutPath: "/rollout/" + agent}}
		o.host.order = append(o.host.order, agent)
	}
	a := o.host.agents[agent]
	turnID = fmt.Sprintf("%s-turn%d", agent, len(a.turns)+1)
	a.turns = append(a.turns, &SessionTurn{ID: turnID, Status: "inProgress", FirstUserText: st.Message, UpdatedAt: time.Now()})
	a.Status = "active"
	o.host.mu.Unlock()
	o.mu.Lock()
	if st.Action == StepSpawn {
		o.spawns = append(o.spawns, st)
	} else {
		o.inputs = append(o.inputs, st)
	}
	o.mu.Unlock()
	finish := func() {
		if ans.edit != "" && o.placer != nil {
			o.placer.setTree(st.Workspace, ans.edit)
			if ans.head != "" {
				o.placer.setHead(st.Workspace, ans.head)
			}
		}
		o.host.set(agent, func(a *fakeAgent, t *SessionTurn) {
			t.UpdatedAt = time.Now()
			switch {
			case ans.lose:
				a.Status = "notLoaded"
			case ans.hang:
			case ans.fail != "":
				t.Status, t.ErrorInfo, t.ErrorMessage = "failed", ans.fail, "the turn failed: "+ans.fail
				a.Status = "idle"
			default:
				t.Status, t.FinalText, t.FinalPhase = "completed", ans.text, ans.phase
				if t.FinalPhase == "" && !ans.rawPhase {
					t.FinalPhase = "final_answer"
				}
				a.Status = "idle"
			}
		})
	}
	if ans.after > 0 {
		go func() {
			time.Sleep(ans.after)
			finish()
		}()
	} else {
		finish()
	}
	if onSpawn != nil && st.Action == StepSpawn {
		onSpawn(st, agent)
	}
	if onInput != nil && st.Action == StepSendInput {
		onInput(st, agent)
	}
	if ans.noDispatch {
		return
	}
	rep := DispatchReport{ID: fmt.Sprintf("%s-t%d-a%d-%s", st.Call, st.K, st.Attempt, agent), Run: st.Run, Call: st.Call, K: st.K, Attempt: st.Attempt, Agent: agent}
	o.mu.Lock()
	o.reports = append(o.reports, rep)
	o.mu.Unlock()
	_ = o.bus.Report(rep)
}

func (o *fakeOrch) spawned() []Step {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]Step(nil), o.spawns...)
}

func (o *fakeOrch) sentInput() []Step {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]Step(nil), o.inputs...)
}

func (o *fakeOrch) closedAgents() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.closed...)
}

// packetSchema is the schema the packet's output contract names, nil when it names none.
func packetSchema(packet string) []byte {
	_, rest, ok := strings.Cut(packet, "```json\n")
	if !ok {
		return nil
	}
	body, _, ok := strings.Cut(rest, "\n```")
	if !ok {
		return nil
	}
	return []byte(body)
}

// sessPlacer is the placer of the session tests: an executor gets an attempt worktree, every other role a directory of its own;
// each directory has a tree and a HEAD.
type sessPlacer struct {
	*Fake
	mu          sync.Mutex
	trees       map[string]string
	heads       map[string]string
	kept        map[string]string
	established []string
}

func newSessPlacer() *sessPlacer {
	return &sessPlacer{Fake: NewFake(), trees: map[string]string{}, heads: map[string]string{}, kept: map[string]string{}}
}

func (p *sessPlacer) StartThread(context.Context, ThreadSpec) (ThreadInfo, error) {
	return ThreadInfo{}, fmt.Errorf("a session run starts no thread")
}

func (p *sessPlacer) RunTurn(context.Context, TurnSpec) (TurnOutcome, error) {
	return TurnOutcome{}, fmt.Errorf("a session run starts no turn")
}

func (p *sessPlacer) Place(req PlaceRequest) (Placement, error) {
	if req.Family != "executor" || req.QA {
		start := req.Head
		if start == "" {
			start = "base"
		}
		return Placement{Kind: PlaceCall, Path: "/w/call/" + req.CallID, Start: start, Fork: req.Fork && req.Continues != nil}, nil
	}
	if req.Continues != nil {
		return Placement{Kind: PlaceContinued, Path: "/w/" + req.Node + "/a1", Start: "base", Fork: req.Fork}, nil
	}
	return Placement{Kind: PlaceFresh, Attempt: 1, Path: "/w/" + req.Node + "/a1", Start: "base"}, nil
}

func (p *sessPlacer) Establish(_ context.Context, _ PlaceRequest, pl Placement) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.established = append(p.established, pl.Path)
	if _, ok := p.heads[pl.Path]; !ok {
		p.heads[pl.Path] = pl.Start
	}
	if _, ok := p.trees[pl.Path]; !ok {
		p.trees[pl.Path] = "T0"
	}
	return nil
}

func (p *sessPlacer) Recheck(context.Context, Placement) error { return nil }

func (p *sessPlacer) Snapshot(_ context.Context, path string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := p.trees[path]; ok {
		return t, nil
	}
	return "T0", nil
}

func (p *sessPlacer) WorktreeExists(path string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.heads[path]
	return ok
}

func (p *sessPlacer) HeadOf(_ context.Context, path string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.heads[path], nil
}

func (p *sessPlacer) KeepRef(_ context.Context, ref, commit string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.kept[ref] = commit
	return nil
}

func (p *sessPlacer) setTree(path, tree string) {
	p.mu.Lock()
	p.trees[path] = tree
	p.mu.Unlock()
}

func (p *sessPlacer) setHead(path, head string) {
	p.mu.Lock()
	p.heads[path] = head
	p.mu.Unlock()
}

func (p *sessPlacer) establishedPaths() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.established...)
}

// deadSwitch is the death of a run process: once it is set, nothing the process does reaches the journal or the bus, and its
// context ends.
type deadSwitch struct {
	mu     sync.Mutex
	dead   bool
	cancel context.CancelFunc
}

func (d *deadSwitch) kill() {
	d.mu.Lock()
	d.dead = true
	cancel := d.cancel
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (d *deadSwitch) isDead() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dead
}

// dyingJournal stores rows until the process dies; stop names the last row it stores. A journal that batches its rows (the
// SQLite journal) loses the rows of its open batch with the process: nothing is flushed after the death.
type dyingJournal struct {
	Journal
	sw   *deadSwitch
	stop func(Event) bool
}

func (j *dyingJournal) Append(e Event) Event {
	if j.sw.isDead() {
		return e
	}
	out := j.Journal.Append(e)
	if j.stop != nil && j.stop(out) {
		j.sw.kill()
	}
	return out
}

func (j *dyingJournal) Err() error {
	if f, ok := j.Journal.(FailingJournal); ok {
		return f.Err()
	}
	return nil
}

func (j *dyingJournal) SetOnCommit(notify func()) {
	if n, ok := j.Journal.(CommitNotifier); ok {
		n.SetOnCommit(notify)
	}
}

// dyingBus is the bus as the process sees it: a dead process writes nothing and reads nothing.
type dyingBus struct {
	*FileStepBus
	sw *deadSwitch
}

func (b *dyingBus) WriteFile(call, name string, body []byte) (string, error) {
	if b.sw.isDead() {
		return filepath.Join(b.StepsDir(call), name), nil
	}
	return b.FileStepBus.WriteFile(call, name, body)
}

func (b *dyingBus) Publish(st Step) error {
	if b.sw.isDead() {
		return nil
	}
	return b.FileStepBus.Publish(st)
}

func (b *dyingBus) Retract(call string, k, attempt int) error {
	if b.sw.isDead() {
		return nil
	}
	return b.FileStepBus.Retract(call, k, attempt)
}

func (b *dyingBus) Reports() ([]DispatchReport, error) {
	if b.sw.isDead() {
		return nil, nil
	}
	return b.FileStepBus.Reports()
}

func (b *dyingBus) Reply(rep DispatchReport, r DispatchReply) error {
	if b.sw.isDead() {
		return nil
	}
	return b.FileStepBus.Reply(rep, r)
}

// sessionKit is one session run under test.
type sessionKit struct {
	t      *testing.T
	h      *harness
	host   *fakeSessionHost
	placer *sessPlacer
	bus    *FileStepBus
	orch   *fakeOrch
	cfg    *SessionConfig
	// jr is the journal the rows are read from when it is not the harness's (the SQLite journal of a resumed process).
	jr Journal
	// resumed is the result of the process that resumed the run (runDeath).
	resumed *Result
}

func newSessionKit(t *testing.T) *sessionKit {
	t.Helper()
	h := newHarness(t)
	k := &sessionKit{t: t, h: h, host: newFakeSessionHost(), placer: newSessPlacer(), bus: NewFileStepBus(t.TempDir())}
	k.cfg = &SessionConfig{Host: k.host, Bus: k.bus, Owner: sessionOwner, Poll: time.Millisecond, LostAfter: 40 * time.Millisecond}
	h.cfg.Backend = k.placer
	h.cfg.Session = k.cfg
	k.orch = newFakeOrch(k.bus, k.host, k.placer)
	k.orch.start(t)
	return k
}

// rowsOf are the call rows with the label, and eventsOf their event names.
func (k *sessionKit) rowsOf(label string) []Event {
	if k.jr == nil {
		return k.h.callRows(label)
	}
	var out []Event
	for _, e := range k.jr.Events() {
		if e.Kind == KindCall && e.Label == label {
			out = append(out, e)
		}
	}
	return out
}

func eventsOf(rows []Event) string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Event)
	}
	return strings.Join(out, ",")
}

func rowsNamed(rows []Event, event string) []Event {
	var out []Event
	for _, r := range rows {
		if r.Event == event {
			out = append(out, r)
		}
	}
	return out
}

func flowRows(j Journal, event string) []Event {
	var out []Event
	for _, e := range j.Events() {
		if e.Kind == KindFlow && e.Event == event {
			out = append(out, e)
		}
	}
	return out
}

// dyingHost is the host as the process sees it: a dead process interrupts nothing.
type dyingHost struct {
	*fakeSessionHost
	sw *deadSwitch
}

func (h *dyingHost) Interrupt(ctx context.Context, id, turn string) error {
	if h.sw.isDead() {
		return nil
	}
	return h.fakeSessionHost.Interrupt(ctx, id, turn)
}
