package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A Lab is one scenario's world: the fake App Server host with a scripted model, a temporary git repository with an integration
// branch, a temporary relay store holding the run's journal and decision rows, the fake relay and verifier, and the backend of one
// run process on all of them. A scenario sets the plan (Nodes), scripts what the model does where it is not the default, runs the
// project lane, and reads what happened from the journal, the store, the repository and the host's request log.
type Lab struct {
	t    *testing.T
	Root string
	// Repo is the git repository the nodes' worktrees are made from; the integration branch is IntegrationRef.
	Repo  string
	State string
	Base  string

	Host    *fakehost.Server
	Threads *fakehost.Threads
	Relay   *FakeRelay
	Verify  *FakeVerifier

	StorePath string
	Store     *store.Store
	RunID     string
	Policy    *engine.Policy
	Budget    int64

	// Nodes are the issue nodes of the plan; Design is the architect's DESIGN (built from the nodes when nil).
	Nodes  []NodeSpec
	Design *ops.Design

	owner   string
	client  *appserver.Client
	owned   *agent.Threads
	alloc   *agent.Allocator
	backend *agent.Backend
	journal *hookJournal
	lane    *ops.Lane
	cancel  context.CancelFunc

	mu        sync.Mutex
	advanceMu sync.Mutex
	rules     []*rule
	// Decisions are the rows of flow_spawn_decisions and flow_step_decisions, read through the store.
	decisions ops.Decisions
	// children are the issue-lane runs of the children the lab started (child_test.go).
	children []*ChildRun
	// Upward is the hook the lab's run hands what goes up from it to, and Reporter the reporter of its milestone close (upward_test.go);
	// nil hands nothing.
	Upward   engine.UpwardHook
	Reporter ops.Reporter
}

// hookJournal wraps the run's journal: a process that dies writes no more rows, and a test can look at every row as it is written.
type hookJournal struct {
	engine.Journal
	flush func()
	mu    sync.Mutex
	dead  bool
	after func(engine.Event)
}

func (j *hookJournal) Append(e engine.Event) engine.Event {
	j.mu.Lock()
	dead, after := j.dead, j.after
	j.mu.Unlock()
	if dead {
		return e
	}
	out := j.Journal.Append(e)
	if after != nil {
		after(out)
	}
	return out
}

func (j *hookJournal) Err() error {
	if f, ok := j.Journal.(engine.FailingJournal); ok {
		return f.Err()
	}
	return nil
}

// isolate points every git command of the test at its own home and no global or system configuration.
func isolate(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// NewLab makes a lab with the plan's nodes. The plan is the lab's own; the scenario sets Nodes before the first run.
func NewLab(t *testing.T, nodes ...NodeSpec) *Lab {
	t.Helper()
	isolate(t)
	l := &Lab{t: t, RunID: "run-f08", Policy: engine.FakePolicy(), owner: "proc-1", Nodes: nodes}
	l.Root = t.TempDir()
	l.Repo = filepath.Join(l.Root, "repo")
	l.State = filepath.Join(l.Root, "state")
	for _, n := range nodes {
		for _, r := range n.Regions {
			if !strings.HasSuffix(r, "/") {
				writeFile(t, filepath.Join(l.Repo, r), "base of "+r+"\n")
			}
		}
	}
	writeFile(t, filepath.Join(l.Repo, "README.md"), "base\n")
	l.git(l.Repo, "init", "-q", "-b", "main")
	l.git(l.Repo, "add", "-A")
	l.git(l.Repo, "commit", "-q", "-m", "base")
	l.Base = l.git(l.Repo, "rev-parse", "HEAD")
	l.git(l.Repo, "update-ref", IntegrationRef, l.Base)

	l.StorePath = filepath.Join(l.Root, "relay.sqlite3")
	l.Store = l.openStore()
	l.Relay = newRelay(l)
	l.Relay.nodes = nodes
	l.Verify = &FakeVerifier{}
	l.Host = fakehost.Start(t)
	l.Threads = l.Host.Threads()
	l.Threads.OnTurn(l.onTurn)
	l.spawn(l.owner)
	return l
}

func (l *Lab) openStore() *store.Store {
	l.t.Helper()
	st, err := store.Open(context.Background(), l.StorePath, "")
	if err != nil {
		l.t.Fatal(err)
	}
	l.t.Cleanup(func() { _ = st.Close() })
	return st
}

// spawn starts a run process: a journal over the store, a client on the host, a backend and the lane's operations.
func (l *Lab) spawn(owner string) {
	l.t.Helper()
	if l.client != nil {
		_ = l.client.Close()
	}
	prev := l.owner
	l.owner = owner
	alive := func(o string) bool { return o == owner && o != prev }
	l.owned = agent.NewThreads()
	l.client = appserver.New(l.Host.SocketPath, appserver.DefaultBounds, agent.ClientOption(l.owned))
	l.t.Cleanup(func() { _ = l.client.Close() })
	var err error
	l.alloc, err = agent.NewAllocator(agent.AllocatorConfig{Repo: l.Repo, StateDir: l.State, RunID: l.RunID,
		Base: func(string) (string, error) { return l.Relay.IntegrationTip(context.Background(), "") }})
	if err != nil {
		l.t.Fatal(err)
	}
	l.backend, err = agent.New(agent.Config{Client: l.client, Allocator: l.alloc, Threads: l.owned, PollEvery: 25 * time.Millisecond, InterruptWait: 5 * time.Second, Owner: owner, OwnerAlive: alive})
	if err != nil {
		l.t.Fatal(err)
	}
	j, err := journal.Open(context.Background(), l.Store, l.RunID, journal.Identity{PlanID: "plan-1", PlanRevision: 1, ActorTaskID: "parent", EpochNonce: "nonce"}, journal.Options{})
	if err != nil {
		l.t.Fatal(err)
	}
	l.journal = &hookJournal{Journal: j}
	l.journal.flush = func() { _ = j.Flush() }
	l.decisions = ops.StoreDecisions{DB: l.Store.DB}
	l.newLane()
}

// newLane makes the lane's operations over the lab's journal, relay and verifier (and the reporter, when the lab has one).
func (l *Lab) newLane() {
	l.lane = ops.NewLane(ops.Deps{RunID: l.RunID, Repo: l.Repo, Journal: l.journal, Relay: l.Relay, Verifier: l.Verify, Decisions: l.decisions, Actor: "parent", Reporter: l.Reporter,
		Now: func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }})
}

// NewProcess is the resume after a process ended: the journal is read again from the store, and a new client and backend serve
// it. What the old process left on the host keeps running.
func (l *Lab) NewProcess(owner string) {
	l.t.Helper()
	l.journal.flush()
	if c, ok := l.journal.Journal.(interface{ Close() error }); ok {
		_ = c.Close()
	}
	l.spawn(owner)
}

// Kill ends the process the way a crash does: it writes no more rows and its connection to the host is gone.
func (l *Lab) Kill() {
	l.journal.flush()
	l.journal.mu.Lock()
	l.journal.dead = true
	l.journal.mu.Unlock()
	_ = l.client.Close()
	if l.cancel != nil {
		l.cancel()
	}
}

// OnRow calls fn for every row the run journals, after it is written.
func (l *Lab) OnRow(fn func(engine.Event)) {
	l.journal.mu.Lock()
	l.journal.after = fn
	l.journal.mu.Unlock()
}

// Config is the engine configuration of the lab's run.
func (l *Lab) Config(resume bool) engine.Config {
	cfg := engine.Config{Backend: l.backend, Policy: l.Policy, Journal: l.journal, RunID: l.RunID, TokenBudget: l.Budget, Ops: l.lane.Ops(), InputSnapshot: l.lane.InputSnapshot,
		Prover: ops.NewProver(), Upward: l.Upward, Now: func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }}
	if resume {
		cfg.Resume = &engine.ResumeOptions{Wait: func(context.Context, time.Duration) error { return nil }, Grace: time.Millisecond, Skew: time.Hour, OpenWait: 10 * time.Millisecond, Poll: time.Millisecond}
	}
	return cfg
}

// Args are the arguments of the project lane the lab runs.
func (l *Lab) Args() json.RawMessage {
	return json.RawMessage(`{"plan":"plan-1","milestone":"M1","cap":3}`)
}

// Run runs the project lane as a new run of the lab's journal.
func (l *Lab) Run() *engine.Result { l.t.Helper(); return l.exec(false, l.Args()) }

// Resume runs the project lane again over the journal the earlier process wrote (`crw flow resume`).
func (l *Lab) Resume() *engine.Result { l.t.Helper(); return l.exec(true, l.Args()) }

func (l *Lab) exec(resume bool, args json.RawMessage) *engine.Result {
	l.t.Helper()
	prog, err := workflows.Load("project-lane")
	if err != nil {
		l.t.Fatal(err)
	}
	cfg := l.Config(resume)
	cfg.Args = args
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	l.cancel = cancel
	defer cancel()
	res, err := engine.Run(ctx, prog, cfg)
	if err != nil {
		l.t.Fatal(err)
	}
	l.journal.flush()
	return res
}

// Script runs a script of the lab's (not the project lane) over the same world: the operations, the backend and the journal.
func (l *Lab) Script(body string, resume bool) *engine.Result {
	l.t.Helper()
	prog, err := loadScript("export const meta = { name: 't' }\n" + body)
	if err != nil {
		l.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res, err := engine.Run(ctx, prog, l.Config(resume))
	if err != nil {
		l.t.Fatal(err)
	}
	l.journal.flush()
	return res
}

// Value decodes the value a run came to.
func Value[T any](t *testing.T, res *engine.Result) T {
	t.Helper()
	if res.Status != engine.RunCompleted {
		t.Fatalf("run %s: %s: %s", res.Status, res.ErrorKind, res.Error)
	}
	var v T
	if err := json.Unmarshal(res.Value, &v); err != nil {
		t.Fatalf("value %s: %v", res.Value, err)
	}
	return v
}

// ---- the journal ----

// Events are the rows of the run's journal.
func (l *Lab) Events() []engine.Event { return l.journal.Events() }

// CallInfo is one agent call as the journal's prepared row names it.
type CallInfo struct {
	ID, Label, Role, Node, Step string
	Resume                      string
	Seq                         int
}

// Calls are the agent calls of the run, in the order they were prepared (a call prepared again by a resume is listed again).
func (l *Lab) Calls() []CallInfo {
	var out []CallInfo
	for _, ev := range l.Events() {
		if ev.Kind == engine.KindCall && ev.Op == "agent" && ev.Event == engine.EvPrepared {
			out = append(out, CallInfo{ID: ev.CallID, Label: ev.Label, Role: str(ev.Data, "role"), Node: str(ev.Data, "node"), Step: str(ev.Data, "step_decision"), Resume: str(ev.Data, "resume_of"), Seq: ev.Seq})
		}
	}
	return out
}

// Labels are the labels of the calls, in order.
func (l *Lab) Labels() []string {
	var out []string
	for _, c := range l.Calls() {
		out = append(out, c.Label)
	}
	return out
}

// Roles are the roles of the calls, in order.
func (l *Lab) Roles() []string {
	var out []string
	for _, c := range l.Calls() {
		out = append(out, c.Role)
	}
	return out
}

// CallsOf are the calls of a role.
func (l *Lab) CallsOf(role string) []CallInfo {
	var out []CallInfo
	for _, c := range l.Calls() {
		if c.Role == role {
			out = append(out, c)
		}
	}
	return out
}

// Rows are the completed rows of an op.
func (l *Lab) Rows(op string) []engine.Event {
	var out []engine.Event
	for _, ev := range l.Events() {
		if ev.Kind == engine.KindCall && ev.Op == op && ev.Event == engine.EvCompleted {
			out = append(out, ev)
		}
	}
	return out
}

// FlowRows are the flow rows of an event name.
func (l *Lab) FlowRows(event string) []engine.Event {
	var out []engine.Event
	for _, ev := range l.Events() {
		if ev.Kind == engine.KindFlow && ev.Event == event {
			out = append(out, ev)
		}
	}
	return out
}

// Prepared is the prepared row of a call.
func (l *Lab) Prepared(callID string) engine.Event {
	l.t.Helper()
	for i := len(l.Events()) - 1; i >= 0; i-- {
		ev := l.Events()[i]
		if ev.Kind == engine.KindCall && ev.Event == engine.EvPrepared && ev.CallID == callID {
			return ev
		}
	}
	l.t.Fatalf("no prepared row for call %s", callID)
	return engine.Event{}
}

// Terminal is the last terminal event of the calls of a label.
func (l *Lab) Terminal(label string) string {
	last := ""
	for _, ev := range l.Events() {
		if ev.Kind == engine.KindCall && ev.Op == "agent" && ev.Label == label {
			switch ev.Event {
			case engine.EvCompleted, engine.EvNullModel, engine.EvHeld, engine.EvRefused, engine.EvOutcomeUnknown, engine.EvSuperseded:
				last = ev.Event
			}
		}
	}
	return last
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// SpawnRow is a row of flow_spawn_decisions.
type SpawnRow struct{ Rule, Decision, Node, Kind, Reason string }

// SpawnRows reads flow_spawn_decisions in the order the rows were written.
func (l *Lab) SpawnRows() []SpawnRow {
	l.t.Helper()
	rows, err := l.Store.DB.Query(`SELECT rule_id, decision, node_id, kind, reason FROM flow_spawn_decisions WHERE run_id = ? ORDER BY rowid`, l.RunID)
	if err != nil {
		l.t.Fatal(err)
	}
	defer rows.Close()
	var out []SpawnRow
	for rows.Next() {
		var r SpawnRow
		if err := rows.Scan(&r.Rule, &r.Decision, &r.Node, &r.Kind, &r.Reason); err != nil {
			l.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// StepRow is a row of flow_step_decisions.
type StepRow struct {
	ID, Node, From, To, Rule, Evidence, Head, Tree string
}

// StepRows reads flow_step_decisions.
func (l *Lab) StepRows() []StepRow {
	l.t.Helper()
	rows, err := l.Store.DB.Query(`SELECT step_id, node_id, from_tier, to_tier, rule_id, evidence_json, checkpoint_head, checkpoint_tree FROM flow_step_decisions WHERE run_id = ? ORDER BY rowid`, l.RunID)
	if err != nil {
		l.t.Fatal(err)
	}
	defer rows.Close()
	var out []StepRow
	for rows.Next() {
		var r StepRow
		if err := rows.Scan(&r.ID, &r.Node, &r.From, &r.To, &r.Rule, &r.Evidence, &r.Head, &r.Tree); err != nil {
			l.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// ---- the host ----

// Requests are the requests the host received, of the methods named (all when none).
func (l *Lab) Requests(methods ...string) []fakehost.Request {
	var out []fakehost.Request
	for _, r := range l.Host.Requests() {
		if len(methods) == 0 || slices.Contains(methods, r.Method) {
			out = append(out, r)
		}
	}
	return out
}

// TurnTexts are the texts of the turns started on a thread, in order.
func (l *Lab) TurnTexts() []string {
	var out []string
	for _, r := range l.Requests("turn/start") {
		var p map[string]any
		_ = json.Unmarshal(r.Params, &p)
		out = append(out, inputText(p))
	}
	return out
}

func inputText(params map[string]any) string {
	input, _ := params["input"].([]any)
	text := ""
	for _, el := range input {
		if m, ok := el.(map[string]any); ok && str(m, "type") == "text" {
			text += str(m, "text")
		}
	}
	return text
}

// LiveCalls are the agent calls the run started a thread or a turn for since the journal had n rows: the work a resume did.
func (l *Lab) LiveCalls(sinceSeq int) []CallInfo {
	var out []CallInfo
	seen := map[string]bool{}
	for _, ev := range l.Events() {
		if ev.Seq <= sinceSeq || ev.Kind != engine.KindCall || ev.Op != "agent" {
			continue
		}
		if ev.Event == engine.EvTurnRequested || ev.Event == engine.EvThreadRequested || ev.Event == engine.EvForkRequested {
			if !seen[ev.CallID] {
				seen[ev.CallID] = true
				out = append(out, CallInfo{ID: ev.CallID, Label: ev.Label, Seq: ev.Seq})
			}
		}
	}
	return out
}

// LastSeq is the sequence number of the newest row.
func (l *Lab) LastSeq() int {
	ev := l.Events()
	if len(ev) == 0 {
		return 0
	}
	return ev[len(ev)-1].Seq
}

// ---- the model ----

// Call is the call a turn belongs to, as the scripted model sees it.
type Call struct {
	ID, Label, Role, Node string
	ThreadID, Text        string
	// N is the turn's ordinal on its thread.
	N int
	// Cwd is the directory the call's thread works in.
	Cwd string
	lab *Lab
}

type rule struct {
	re *regexp.Regexp
	fn func(*Call) *fakehost.TurnPlan
}

// On scripts the model: the turns of the calls whose label matches the expression are answered by fn; a turn fn answers with nil
// falls through to the next rule and then to the default of the role. Rules are asked in the order they were added.
func (l *Lab) On(label string, fn func(*Call) *fakehost.TurnPlan) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rules = append(l.rules, &rule{re: regexp.MustCompile(label), fn: fn})
}

// Sequence scripts the nth turn-start of the calls that match (counting calls, not turns: the repair turn of a call is the same
// call) with the nth plan, the last plan repeating.
func (l *Lab) Sequence(label string, plans ...func(*Call) fakehost.TurnPlan) {
	var mu sync.Mutex
	seen := map[string]int{}
	order := []string{}
	l.On(label, func(c *Call) *fakehost.TurnPlan {
		mu.Lock()
		defer mu.Unlock()
		idx, ok := seen[c.ID]
		if !ok {
			idx = len(order)
			seen[c.ID] = idx
			order = append(order, c.ID)
		}
		p := plans[min(idx, len(plans)-1)](c)
		return &p
	})
}

var opLine = regexp.MustCompile(`^crw-flow-op: (c_[0-9a-f]+)/t(\d+)`)

func (l *Lab) onTurn(r fakehost.TurnRequest) fakehost.TurnPlan {
	c := &Call{ThreadID: r.ThreadID, Text: r.Text, N: r.N, lab: l}
	if m := opLine.FindStringSubmatch(strings.SplitN(r.Text, "\n", 2)[0]); m != nil {
		c.ID = m[1]
		all := l.allEvents()
		for i := len(all) - 1; i >= 0; i-- {
			ev := all[i]
			if ev.Kind == engine.KindCall && ev.Event == engine.EvPrepared && ev.CallID == c.ID {
				c.Label, c.Role, c.Node = ev.Label, str(ev.Data, "role"), str(ev.Data, "node")
				if m, ok := ev.Data["allocation"].(map[string]any); ok && str(m, "path") != "" {
					c.Cwd = str(m, "path")
				} else {
					c.Cwd = str(ev.Data, "thread_cwd")
				}
				break
			}
		}
	}
	l.mu.Lock()
	rules := slices.Clone(l.rules)
	l.mu.Unlock()
	for _, rl := range rules {
		if !rl.re.MatchString(c.Label) {
			continue
		}
		if p := rl.fn(c); p != nil {
			return *p
		}
	}
	return l.defaultPlan(c)
}

// Say is a plan that answers with the JSON of v.
func Say(v any) fakehost.TurnPlan {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return fakehost.TurnPlan{Message: string(b)}
}

// Fail is a plan whose turn fails with the codexErrorInfo.
func Fail(info any) fakehost.TurnPlan {
	return fakehost.TurnPlan{Fail: &fakehost.Failure{Message: "the host failed the turn", CodexErrorInfo: info}}
}

// Garbage is a plan whose answer is no JSON: the repair turn gets the same, and the call is null_model.
func Garbage() fakehost.TurnPlan {
	return fakehost.TurnPlan{Message: "I cannot answer in the format asked."}
}

// EditFile writes the call's file in its worktree: one line for each turn the thread has had.
func (c *Call) Edit() {
	c.lab.t.Helper()
	if c.Cwd == "" || c.Node == "" {
		return
	}
	path := filepath.Join(c.Cwd, "node-"+c.Node+".txt")
	prior, _ := os.ReadFile(path)
	writeFile(c.lab.t, path, string(prior)+fmt.Sprintf("%s turn %d\n", c.Label, c.N))
}

// EditPath writes a file of the node's worktree.
func (c *Call) EditPath(rel, content string) {
	c.lab.t.Helper()
	writeFile(c.lab.t, filepath.Join(c.Cwd, rel), content)
}

// Receipt is an executor's report.
func Receipt(outcome string) map[string]any {
	return map[string]any{"outcome": outcome, "files": []string{}, "verification": []any{}, "summary": "done: " + outcome, "gap": nil, "research_question": nil, "plan_mismatch": nil}
}

// ReceiptWith is a report with fields set.
func ReceiptWith(outcome string, set map[string]any) map[string]any {
	r := Receipt(outcome)
	for k, v := range set {
		r[k] = v
	}
	return r
}

// Verdict is a reviewer's answer: verified with one criterion met, or needs_changes with the defects.
func Verdict(verified bool, defects ...map[string]any) map[string]any {
	v := "verified"
	if !verified {
		v = "needs_changes"
	}
	if defects == nil {
		defects = []map[string]any{}
	}
	return map[string]any{"verdict": v, "criteria": []any{map[string]any{"id": "c1", "met": verified, "evidence": "read the head"}}, "defects": defects, "summary": "reviewed"}
}

// Defect is a defect of a verdict.
func Defect(ref, kind, severity, finding string) map[string]any {
	return map[string]any{"ref": ref, "kind": kind, "severity": severity, "introduced": false, "impact": "", "inPromise": false, "file": "node.txt", "line": 1, "finding": finding, "evidence_question": nil}
}

// EvidenceDefect is a defect of kind evidence_missing with its question.
func EvidenceDefect(ref, question string, files ...string) map[string]any {
	d := Defect(ref, "evidence_missing", "P2", "cannot tell from the head: "+question)
	if files == nil {
		files = []string{}
	}
	d["evidence_question"] = map[string]any{"question": question, "files_needed": files}
	return d
}

func (l *Lab) designFor() ops.Design {
	if l.Design != nil {
		return *l.Design
	}
	d := ops.Design{Decisions: []ops.Decision{{ID: "D1", Evidence: []string{"README.md"}, Change: "the nodes edit their own files", Alternatives: []string{}, Assumptions: []string{}}},
		OpenQuestions: []ops.OpenQuestion{}, Risks: []string{}}
	for _, n := range l.Nodes {
		fit := n.SizeFit
		if fit == "" {
			fit = "executor"
		}
		var group *string
		if n.Group != "" {
			g := n.Group
			group = &g
		}
		d.Issues = append(d.Issues, ops.DesignIssue{Issue: n.Issue, Node: n.ID, SizeFit: fit, SizeReason: n.SizeReason, IssueSizeDecision: "ok", EditRegions: nz(n.Regions),
			Approach: "approach of " + n.ID, TestsFirst: []string{"test of " + n.ID}, DoneCondition: "done condition of " + n.ID, DecisionRefs: []string{"D1"}, OrderAfter: nz(n.After), Group: group})
	}
	return d
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// defaultPlan is what a role does when no rule answers.
func (l *Lab) defaultPlan(c *Call) fakehost.TurnPlan {
	switch {
	case c.Role == "architect":
		return Say(l.designFor())
	case c.Role == "reviewer" && strings.HasPrefix(c.Label, "design-audit"):
		return Say(map[string]any{"verdict": "approved", "findings": []any{}, "summary": "the DESIGN holds"})
	case c.Role == "reviewer":
		return Say(Verdict(true))
	case c.Role == "explorer":
		return Say(map[string]any{"answer": "answer of the explorer", "evidence": []string{"README.md"}, "verified": []string{"README.md"}, "inferred": []string{}, "unknown": []string{}})
	case c.Role == "executor" || c.Role == "executor-t1":
		c.Edit()
		return Say(Receipt("ready_for_review"))
	}
	return fakehost.TurnPlan{Message: "ok"}
}

// ---- helpers for scenarios ----

// Git runs git in the lab's repository.
func (l *Lab) Git(args ...string) string { return l.git(l.Repo, args...) }

// GitIn runs git in a directory.
func (l *Lab) GitIn(dir string, args ...string) string { return l.git(dir, args...) }

// Tip is the tip of the integration branch.
func (l *Lab) Tip() string { return l.git(l.Repo, "rev-parse", IntegrationRef) }
