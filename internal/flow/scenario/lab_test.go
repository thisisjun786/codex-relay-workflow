package scenario

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/audit"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
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
	// FailedTests are, by node, the failed tests a failing verification of the node names (the failed_tests of its crw.verify
	// result, CRW-1469); none is a failing verification whose record names no test, as FakeVerifier answers alone.
	FailedTests map[string][]ops.FailedStep

	StorePath string
	Store     *store.Store
	RunID     string
	Policy    *engine.Policy
	Budget    int64

	// Nodes are the issue nodes of the plan; Design is the architect's DESIGN (built from the nodes when nil).
	Nodes  []NodeSpec
	Design *ops.Design
	// Cap is the executors the project lane runs at once (0: three).
	Cap int

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
	// Upward is the hook the lab's run hands what goes up from it to, and Reporter the reporter of its milestone close (upward_test.go);
	// nil hands nothing.
	Upward   engine.UpwardHook
	Reporter ops.Reporter
	// Lib, when set, is the helper library behind crw:lib/<name> the project lane loads in place of the embedded one, and Prompt the
	// role prompt the backend of the next process starts threads with in place of roles.Prompt: what a process built from other
	// packet or overlay text would run (CRW-1321).
	Lib    fs.FS
	Prompt func(role string, qa bool) (string, error)
	// Source, when set, is the text of the project lane the lab runs in place of the embedded one (reviewerLane: the project lane
	// with the old reviewer path's node lane).
	Source []byte
	// Checker is the lab's unit checker, the tool behind crw.unitCheck (CRW-1465): a scenario scripts it with UnitChecks.
	Checker *LabChecker
	// LandRoot is the run's directory of the lab (Deps.LandRoot), absolute and under Root: the close's crosscheck writes its receipts
	// file and makes its checkouts under it. NewLab sets it once, so every rebuild of the lane and every Resume keeps it.
	LandRoot string
	// Crosschecker is the lab's crosschecker, the engine of crw.crosscheck (CRW-1473): a scenario scripts it with Crosschecks.
	Crosschecker *LabCrosschecker
	crosscheck   *ops.Crosscheck
	// legacyDesign, set by UseLegacyDesign, makes the architect answer the DESIGN of designFor as a binary before S-1 wrote it (no
	// issue carries sub_tasks; CRW-1435).
	legacyDesign bool
	// ReplayHold is the engine's replay hold (engine.Config.ReplayHold, a test seam) of the processes the lab runs from now on: a
	// scenario holds the replays of a resume with it and settles them in the order it wants; nil settles every replay at once.
	ReplayHold func(row engine.Event, settle func()) bool

	settleMu sync.Mutex
	settled  func(engine.Event)
}

// OnSettled calls fn for every position of an operation the script went on from, after the script ran on from the answer to its next
// await, on the engine's loop goroutine (engine.SettleObserver): what a lane did with an answer, not when its row was written (OnRow).
// fn must not block.
func (l *Lab) OnSettled(fn func(engine.Event)) {
	l.settleMu.Lock()
	l.settled = fn
	l.settleMu.Unlock()
}

// observedOp is an operation of the lab's lane that tells the lab of its settled positions (OnSettled).
type observedOp struct {
	engine.Op
	l *Lab
}

func (o observedOp) Settled(row engine.Event) {
	o.l.settleMu.Lock()
	fn := o.l.settled
	o.l.settleMu.Unlock()
	if fn != nil {
		fn(row)
	}
}

// ops are the lane's operations, each observed (OnSettled).
func (l *Lab) ops() map[string]engine.Op {
	out := map[string]engine.Op{}
	laneOps := l.lane.Ops()
	// the lab's checker is told the round of the check it answers (its rules key on it, as they keyed on a review's label)
	laneOps[ops.NameUnitCheck] = labCheckOp{laneOps[ops.NameUnitCheck]}
	for name, op := range laneOps {
		out[name] = observedOp{Op: op, l: l}
	}
	return out
}

// hookJournal wraps the run's journal: a process that dies writes no more rows, and a test can look at every row as it is written.
type hookJournal struct {
	engine.Journal
	flush func()
	mu    sync.Mutex
	dead  bool
	after func(engine.Event)
	// rewrite changes a row before it is stored: a record that does not describe what happened.
	rewrite func(engine.Event) engine.Event
}

func (j *hookJournal) Append(e engine.Event) engine.Event {
	j.mu.Lock()
	dead, after, rewrite := j.dead, j.after, j.rewrite
	j.mu.Unlock()
	if dead {
		return e
	}
	if rewrite != nil {
		e = rewrite(e)
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
	l := &Lab{t: t, RunID: "run-f08", Policy: engine.FakePolicy(), owner: "proc-1", Nodes: nodes, Checker: newLabChecker(), Crosschecker: newLabCrosschecker()}
	l.Root = t.TempDir()
	l.LandRoot = filepath.Join(l.Root, "land")
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
	l.backend, err = agent.New(agent.Config{Client: l.client, Allocator: l.alloc, Threads: l.owned, PollEvery: 25 * time.Millisecond, InterruptWait: 5 * time.Second, Owner: owner, OwnerAlive: alive, Prompt: l.Prompt})
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

// newLane makes the lane's operations over the lab's journal, relay and verifier (and the reporter, when the lab has one), with the
// host of the close.
func (l *Lab) newLane() {
	if l.crosscheck == nil {
		l.crosscheck = l.Crosschecker.seam(l.LandRoot)
	}
	deps := ops.Deps{RunID: l.RunID, Repo: l.Repo, Journal: l.journal, Relay: l.Relay, Verifier: labVerifier{l}, Decisions: l.decisions, Actor: "parent", Reporter: l.Reporter,
		Now: func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }, UnitCheck: l.Checker.seam(), LandRoot: l.LandRoot, Crosscheck: l.crosscheck}
	deps.Threads = labThreads{BusReader: audit.BusReader{Bus: l.client.Bus()}, backend: l.backend}
	l.lane = ops.NewLane(deps)
}

// labVerifier is the lab's verifier: FakeVerifier's answer, with the failed tests the scenario gives the node when it fails.
type labVerifier struct{ l *Lab }

// Verify implements ops.Verifier.
func (v labVerifier) Verify(ctx context.Context, req ops.VerifyRequest) (ops.VerifyOutcome, error) {
	out, err := v.l.Verify.Verify(ctx, req)
	if err == nil && !out.Pass {
		out.FailedTests = v.l.FailedTests[req.Node]
	}
	return out, err
}

// labThreads is the host of the close: the audit reads the reviewer threads through the client of the run process, and the backend
// archives them.
type labThreads struct {
	audit.BusReader
	backend *agent.Backend
}

func (t labThreads) ArchiveThread(ctx context.Context, id string) error {
	return t.backend.ArchiveThread(ctx, id)
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

// Rewrite changes every row the run journals from now on with fn before it is stored.
func (l *Lab) Rewrite(fn func(engine.Event) engine.Event) {
	l.journal.mu.Lock()
	l.journal.rewrite = fn
	l.journal.mu.Unlock()
}

// Config is the engine configuration of the lab's run.
func (l *Lab) Config(resume bool) engine.Config {
	// the crosscheck's authors are the run's role pairs, as the cli gives them from the run's role table (FakePolicy: claude)
	l.crosscheck.Authors = map[string]string{}
	for _, role := range []string{"architect", "executor", "executor-t1"} {
		l.crosscheck.Authors[role] = l.Policy.Roles[role].Pair.Model
	}
	cfg := engine.Config{Backend: l.backend, Policy: l.Policy, Journal: l.journal, RunID: l.RunID, TokenBudget: l.Budget, Ops: l.ops(), InputSnapshot: l.lane.InputSnapshot,
		Prover: ops.NewProver(), Upward: l.Upward, Now: func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }, ReplayHold: l.ReplayHold}
	if resume {
		cfg.Resume = &engine.ResumeOptions{Wait: func(context.Context, time.Duration) error { return nil }, Grace: time.Millisecond, Skew: time.Hour, OpenWait: 10 * time.Millisecond, Poll: time.Millisecond}
	}
	return cfg
}

// Args are the arguments of the project lane the lab runs.
func (l *Lab) Args() json.RawMessage {
	cap := 3
	if l.Cap > 0 {
		cap = l.Cap
	}
	return json.RawMessage(fmt.Sprintf(`{"plan":"plan-1","milestone":"M1","cap":%d}`, cap))
}

// Run runs the project lane as a new run of the lab's journal.
func (l *Lab) Run() *engine.Result { l.t.Helper(); return l.exec(false, l.Args()) }

// Resume runs the project lane again over the journal the earlier process wrote (`crw flow resume`).
func (l *Lab) Resume() *engine.Result { l.t.Helper(); return l.exec(true, l.Args()) }

func (l *Lab) exec(resume bool, args json.RawMessage) *engine.Result {
	l.t.Helper()
	prog, err := workflows.Load("project-lane")
	if l.Lib != nil || l.Source != nil {
		text := l.Source
		if text == nil {
			var serr error
			if text, serr = workflows.Source("project-lane"); serr != nil {
				l.t.Fatal(serr)
			}
		}
		prog, err = script.Load(script.Source{Name: "project-lane.js", Text: text, Dir: workflows.FS, Lib: l.Lib})
	}
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

// BackgroundLabel is the label prefix of the background explorers that run before the architect (CRW-1466): one for each node.
const BackgroundLabel = "explore-bg:"

// Calls are the agent calls of the run, in the order they were prepared (a call prepared again by a resume is listed again), without
// the background explorers (BackgroundLabel): the scenarios pin the calls of the design loop and the lanes, and one test pins those
// (TestExplorersRunBeforeTheArchitectAndTheirFindingsReachItsPacket). Labels, Roles and CallsOf read these. AllCalls has them all.
func (l *Lab) Calls() []CallInfo {
	var out []CallInfo
	for _, c := range l.AllCalls() {
		if !strings.HasPrefix(c.Label, BackgroundLabel) {
			out = append(out, c)
		}
	}
	return out
}

// AllCalls are the agent calls of the run, the background explorers among them, in the order they were prepared.
func (l *Lab) AllCalls() []CallInfo {
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

// SpawnRows reads flow_spawn_decisions in the order the rows were written, without the routes of the background questions (their
// inputs.kind is background, CRW-1466; a reused background question is left out too): BackgroundSpawnRows has those.
func (l *Lab) SpawnRows() []SpawnRow {
	l.t.Helper()
	return l.spawnRows(`json_extract(evidence_json, '$.inputs.kind') IS NOT 'background'`)
}

// BackgroundSpawnRows are the rows of flow_spawn_decisions of the background questions, in the order they were written.
func (l *Lab) BackgroundSpawnRows() []SpawnRow {
	l.t.Helper()
	return l.spawnRows(`json_extract(evidence_json, '$.inputs.kind') IS 'background'`)
}

func (l *Lab) spawnRows(where string) []SpawnRow {
	l.t.Helper()
	rows, err := l.Store.DB.Query(`SELECT rule_id, decision, node_id, kind, reason FROM flow_spawn_decisions WHERE run_id = ? AND `+where+` ORDER BY rowid`, l.RunID)
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
		all := l.Events()
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

// Reflection is the answer of the architect's reflection at the close: ALIGNED, or MISALIGNED with the decision D1 departed from.
func Reflection(aligned bool) map[string]any {
	if aligned {
		return map[string]any{"verdict": "ALIGNED", "misaligned": []any{}}
	}
	return map[string]any{"verdict": "MISALIGNED", "misaligned": []any{map[string]any{"decision_id": "D1", "observed": "the range departs from D1", "affected_nodes": []string{"A"}, "evidence": []string{"README.md"}}}}
}

// QACapture is the answer of the QA capture call: one screenshot.
func QACapture() map[string]any {
	return map[string]any{"captures": []any{map[string]any{"path": "/tmp/qa/shot-1.png", "kind": "screenshot", "shows": "the home screen at 1280px"}},
		"matrix": []string{"home at 1280px"}, "teardown": []string{"no resources spawned: checked ps and lsof"}, "summary": "drove the surface"}
}

// QAPassOf is the answer of an oracle pass.
func QAPassOf(pass, verdict string) map[string]any {
	return map[string]any{"pass": pass, "verdict": verdict, "findings": []any{}, "summary": "pass " + pass + " read the captures"}
}

// QAReceiptOf is the verdict of the QA call: its self-review or the synthesis of the passes.
func QAReceiptOf(verdict string) map[string]any {
	return map[string]any{"verdict": verdict, "findings": []any{}, "summary": "receipt " + verdict}
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
	case c.Role == "architect" && strings.HasPrefix(c.Label, "reflection:"):
		return Say(Reflection(true))
	case c.Role == "architect" && l.legacyDesign:
		return Say(legacyDesignOf(l.t, l.designFor()))
	case c.Role == "architect":
		return Say(l.designFor())
	case c.Role == "executor" && strings.HasPrefix(c.Label, "qa:"):
		return Say(QACapture())
	case c.Role == "executor" && strings.HasPrefix(c.Label, "qa-"):
		return Say(QAReceiptOf("PASS"))
	case c.Role == "explorer" && strings.HasPrefix(c.Label, "qa-pass:"):
		return Say(QAPassOf(strings.SplitN(c.Label, ":", 3)[1], "PASS"))
	case c.Role == "explorer" && strings.HasPrefix(c.Label, BackgroundLabel):
		// a background explorer costs 1 token (fakehost counts a zero as 10), so a two-node lab's background costs 2
		p := Say(map[string]any{"answer": "answer of the explorer", "evidence": []string{"README.md"}, "verified": []string{"README.md"}, "inferred": []string{}, "unknown": []string{}})
		p.Tokens = 1
		return p
	case c.Role == "critic" && strings.HasPrefix(c.Label, "design-audit"):
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

// designImport is the line of project-lane.js that imports the schemas, DESIGN among them. The legacy lane imports them less DESIGN
// (legacyDesignImport) and declares DESIGN from legacyDesignSchemaJSON after the header (legacyDesignConst): a script's header holds
// its imports and then its meta.
const (
	designImport       = "import { RECEIPT, DESIGN, DESIGN_AUDIT, REFLECTION } from 'crw:lib/schemas'\n"
	legacyDesignImport = "import { RECEIPT, DESIGN_AUDIT, REFLECTION } from 'crw:lib/schemas'\n"
	legacyDesignConst  = "const DESIGN = " + legacyDesignSchemaJSON + "\n"
	metaLine           = "\nexport const meta = "
)

// legacyDesignSchemaJSON is the DESIGN of crw:lib/schemas before S-1 (CRW-1435): a node is sized for an executor, a child or a split,
// and an issue has no sub_tasks member. TestLegacyDesignLaneSwapsOnlyTheDesignSchema pins it to the current DESIGN less S-1.
const legacyDesignSchemaJSON = `{"type":"object","properties":{` +
	`"decisions":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"evidence":{"type":"array","items":{"type":"string"}},"change":{"type":"string"},` +
	`"alternatives":{"type":"array","items":{"type":"string"}},"assumptions":{"type":"array","items":{"type":"string"}}},"required":["id","evidence","change","alternatives","assumptions"],"additionalProperties":false}},` +
	`"issues":{"type":"array","items":{"type":"object","properties":{"issue":{"type":"string"},"node":{"type":"string"},"size_fit":{"type":"string","enum":["executor","child","split"]},` +
	`"size_reason":{"type":"string"},"issue_size_decision":{"type":"string","enum":["ok","over_line","over_line_accepted"]},"edit_regions":{"type":"array","items":{"type":"string"}},` +
	`"approach":{"type":"string"},"tests_first":{"type":"array","items":{"type":"string"}},"done_condition":{"type":"string"},"decision_refs":{"type":"array","items":{"type":"string"}},` +
	`"order_after":{"type":"array","items":{"type":"string"}},"group":{"type":["string","null"]}},` +
	`"required":["issue","node","size_fit","size_reason","issue_size_decision","edit_regions","approach","tests_first","done_condition","decision_refs","order_after","group"],"additionalProperties":false}},` +
	`"open_questions":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"question":{"type":"string"},"scope":{"type":"string"},"files_needed":{"type":"array","items":{"type":"string"}}},` +
	`"required":["id","question","scope","files_needed"],"additionalProperties":false}},` +
	`"risks":{"type":"array","items":{"type":"string"}},` +
	`"dispositions":{"type":["array","null"],"items":{"type":"object","properties":{"finding":{"type":"string"},"disposition":{"type":"string","enum":["folded","rebutted"]},"reason":{"type":"string"}},` +
	`"required":["finding","disposition","reason"],"additionalProperties":false}}},` +
	`"required":["decisions","issues","open_questions","risks","dispositions"],"additionalProperties":false}`

// legacyDesignLane is the text of the project lane an old run (before S-1, CRW-1435) ran: the current project-lane.js with its line-1
// import of DESIGN replaced by the DESIGN of that time, declared after the header, so its architect answers a DESIGN that sizes a node
// for a child, or for a split without sub-tasks, as an old architect did.
func legacyDesignLane(t *testing.T) []byte {
	t.Helper()
	text, err := workflows.Source("project-lane")
	if err != nil {
		t.Fatal(err)
	}
	head, ok := strings.CutPrefix(string(text), designImport)
	at := strings.Index(head, metaLine)
	if !ok || at < 0 || strings.Contains(head[at+1:], metaLine) {
		t.Fatalf("project-lane.js no longer starts with the import of DESIGN the legacy lane replaces, followed by one meta line")
	}
	end := at + 1 + strings.IndexByte(head[at+1:], '\n') + 1
	return []byte(legacyDesignImport + head[:end] + legacyDesignConst + head[end:])
}

// legacyDesignOf is a DESIGN as an architect before S-1 answered it: d with the sub_tasks member of every issue dropped.
func legacyDesignOf(t *testing.T, d ops.Design) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	issues, _ := m["issues"].([]any)
	for _, is := range issues {
		delete(is.(map[string]any), "sub_tasks")
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// UseLegacyDesign makes the lab an old run's world (before S-1, CRW-1435): the project lane it runs is legacyDesignLane, and its
// architect answers the DESIGN of designFor with no sub_tasks member. A node sized for a child, or for a split with no sub-tasks,
// reaches a lane only so: the current DESIGN refuses both.
func (l *Lab) UseLegacyDesign() {
	l.t.Helper()
	l.Source = legacyDesignLane(l.t)
	l.legacyDesign = true
}

// The legacy lane differs from the current project lane only in where DESIGN comes from, and its DESIGN is the current one less S-1
// (CRW-1435): child back in the size_fit enum and no sub_tasks member. The lane loads.
func TestLegacyDesignLaneSwapsOnlyTheDesignSchema(t *testing.T) {
	text, err := workflows.Source("project-lane")
	if err != nil {
		t.Fatal(err)
	}
	legacy := string(legacyDesignLane(t))
	if strings.Count(legacy, legacyDesignConst) != 1 {
		t.Fatalf("the legacy lane does not declare DESIGN once")
	}
	if got := designImport + strings.Replace(strings.TrimPrefix(legacy, legacyDesignImport), legacyDesignConst, "", 1); got != string(text) {
		t.Errorf("the legacy lane differs from project-lane.js beyond the import of DESIGN")
	}
	if _, err := script.Load(script.Source{Name: "project-lane.js", Text: []byte(legacy), Dir: workflows.FS}); err != nil {
		t.Fatalf("the legacy lane does not load: %v", err)
	}
	current, _ := ops.SchemaText(ops.SchemaDesign)
	var want map[string]any
	if err := json.Unmarshal([]byte(current), &want); err != nil {
		t.Fatal(err)
	}
	issue := want["properties"].(map[string]any)["issues"].(map[string]any)["items"].(map[string]any)
	props := issue["properties"].(map[string]any)
	delete(props, "sub_tasks")
	props["size_fit"].(map[string]any)["enum"] = []any{"executor", "child", "split"}
	var required []any
	for _, r := range issue["required"].([]any) {
		if r != "sub_tasks" {
			required = append(required, r)
		}
	}
	issue["required"] = required
	var got map[string]any
	if err := json.Unmarshal([]byte(legacyDesignSchemaJSON), &got); err != nil {
		t.Fatalf("the legacy DESIGN is not JSON: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.MarshalIndent(got, "", " ")
		wb, _ := json.MarshalIndent(want, "", " ")
		t.Errorf("the legacy DESIGN is not the current DESIGN less S-1\nlegacy:\n%s\nwant:\n%s", gb, wb)
	}
	if errs := script.LintStrictSchema(got); len(errs) > 0 {
		t.Errorf("the legacy DESIGN fails the strict structured-output lint: %v", errs)
	}
}

// ---- helpers for scenarios ----

// Git runs git in the lab's repository.
func (l *Lab) Git(args ...string) string { return l.git(l.Repo, args...) }

// GitIn runs git in a directory.
func (l *Lab) GitIn(dir string, args ...string) string { return l.git(dir, args...) }

// Tip is the tip of the integration branch.
func (l *Lab) Tip() string { return l.git(l.Repo, "rev-parse", IntegrationRef) }

// Archived are the threads the host was asked to archive, in order.
func (l *Lab) Archived() []string { return l.Threads.Archived() }

// ---- the unit checker (CRW-1465) ----

// The lab's unit checker stands in for the review tool the shipped lane runs before every commit of a node (crw.unitCheck): the lane
// gives it the request the op built, and it answers a record without running a tool. Its default answer is a verified check with no
// finding by Bugbot's pair (engine family grok, model grok-4.7-medium, effort medium), of another family than the lab's executors; a
// scenario scripts the answers of a node's checks in order (UnitChecks), and a scripted answer's Do runs before it answers, where a
// scenario hooked the reviewer of the node before. The node is read from the prompt (the op's prompt names it).

// CheckModel is the model of the lab's unit checker.
const CheckModel = "grok-4.7-medium"

// UnitCheck is one answer of the lab's unit checker. An empty State is ok, with the verdict verified unless one is given; Err makes the
// call fail with that Go error; Do runs before the answer, and the answer waits for Gate to close (as a held turn of the host waits
// for its gate) or for the end of the check's context, which fails the check.
type UnitCheck struct {
	State    reviewadapter.State
	Verdict  string
	Findings []reviewadapter.Finding
	Err      error
	Do       func()
	Gate     <-chan struct{}
}

// UnitCheckCall is one request the lab's unit checker was given.
type UnitCheckCall struct {
	Node    string
	Request reviewadapter.Request
}

// LabChecker is the lab's ops.UnitChecker.
type LabChecker struct {
	mu    sync.Mutex
	queue map[string][]UnitCheck
	calls []UnitCheckCall
	rules []func(CheckInfo) (UnitCheck, bool)
}

// CheckInfo is what a rule of the lab's checker is given: the node, the round of the node lane the check is of (its crw.unitCheck
// opts.round, the round a review's label named before CRW-1465) and the ordinal of the check of the node (from 1, counted over the
// lab's life, every process's checks included).
type CheckInfo struct {
	Node     string
	Round, N int
	// Prompt and Worktree are the request's: the prompt the op built and the worktree the check judges.
	Prompt, Worktree string
}

type labRoundKey struct{}

// labCheckOp is the lane's crw.unitCheck with the round of the call handed to the lab's checker through the context.
type labCheckOp struct{ engine.Op }

func (o labCheckOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var args []json.RawMessage
	var opts struct {
		Round int `json:"round"`
	}
	if json.Unmarshal(req.Args, &args) == nil && len(args) > 2 {
		_ = json.Unmarshal(args[2], &opts)
	}
	return o.Op.Run(context.WithValue(ctx, labRoundKey{}, opts.Round), req)
}

var labCheckNode = regexp.MustCompile(`unit check of node (\S+) `)

func newLabChecker() *LabChecker { return &LabChecker{queue: map[string][]UnitCheck{}} }

func (u *LabChecker) seam() *ops.UnitCheck {
	return &ops.UnitCheck{Checker: u, Limit: time.Minute, Profile: func(mode string) (reviewadapter.Profile, error) {
		return reviewadapter.JSONProfile("lab-check-"+mode, []string{"lab-unit-check"}, CheckModel, "medium"), nil
	}}
}

// Calls are the requests the checker was given for a node (every node when it is empty), in order.
func (u *LabChecker) Calls(node string) []UnitCheckCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []UnitCheckCall
	for _, c := range u.calls {
		if node == "" || c.Node == node {
			out = append(out, c)
		}
	}
	return out
}

func (u *LabChecker) Check(ctx context.Context, req reviewadapter.Request) (reviewadapter.Result, error) {
	node := ""
	if m := labCheckNode.FindStringSubmatch(req.Prompt); m != nil {
		node = m[1]
	}
	u.mu.Lock()
	u.calls = append(u.calls, UnitCheckCall{Node: node, Request: req})
	n := 0
	for _, c := range u.calls {
		if c.Node == node {
			n++
		}
	}
	rules := slices.Clone(u.rules)
	u.mu.Unlock()
	round, _ := ctx.Value(labRoundKey{}).(int)
	c, ruled := UnitCheck{}, false
	for _, rl := range rules {
		if c, ruled = rl(CheckInfo{Node: node, Round: round, N: n, Prompt: req.Prompt, Worktree: req.Worktree}); ruled {
			break
		}
	}
	if !ruled {
		u.mu.Lock()
		if q := u.queue[node]; len(q) > 0 {
			c, u.queue[node] = q[0], q[1:]
		}
		u.mu.Unlock()
	}
	if c.Do != nil {
		c.Do()
	}
	if c.Gate != nil {
		select {
		case <-c.Gate:
		case <-ctx.Done():
			return reviewadapter.Result{}, ctx.Err()
		}
	}
	if c.Err != nil {
		return reviewadapter.Result{}, c.Err
	}
	if c.State == "" {
		c.State = reviewadapter.StateOK
	}
	verdict := ""
	findings := []reviewadapter.Finding{}
	if c.State == reviewadapter.StateOK {
		verdict = c.Verdict
		if verdict == "" {
			verdict = reviewadapter.VerdictVerified
		}
		for _, f := range c.Findings {
			if f.ID == "" {
				f.ID = reviewadapter.FindingID(f.File, f.Text)
			}
			findings = append(findings, f)
		}
	}
	fp, err := ops.Snapshot(ctx, ops.ExecGit("git"), req.Worktree)
	if err != nil {
		return reviewadapter.Result{}, fmt.Errorf("the lab's unit checker: fingerprint: %w", err)
	}
	sum := sha256.Sum256([]byte(req.Prompt))
	return reviewadapter.Result{State: c.State, Verdict: verdict, Findings: findings, EngineFamily: "grok", Model: CheckModel, Effort: "medium", GuideRev: req.GuideRev,
		PromptDigest: hex.EncodeToString(sum[:]), FingerprintBefore: fp, FingerprintAfter: fp, ElapsedMS: 1}, nil
}

// UnitChecks scripts the answers of the next unit checks of a node, in order; a check no script answers is verified with no finding.
func (l *Lab) UnitChecks(node string, checks ...UnitCheck) {
	l.Checker.mu.Lock()
	defer l.Checker.mu.Unlock()
	l.Checker.queue[node] = append(l.Checker.queue[node], checks...)
}

// OnUnitCheck scripts the checker with a rule asked before the scripted answers: an answer fn gives with true is the check's. Rules
// are asked in the order they were added.
func (l *Lab) OnUnitCheck(fn func(CheckInfo) (UnitCheck, bool)) {
	l.Checker.mu.Lock()
	defer l.Checker.mu.Unlock()
	l.Checker.rules = append(l.Checker.rules, fn)
}

// Finding is a finding of a unit check on the node's file. Its impact follows the severity: a P0-P2 finding is
// core_function and blocks, a P3 finding is minor_separable and does not.
func Finding(severity, text string) reviewadapter.Finding {
	impact := "core_function"
	if severity == "P3" {
		impact = "minor_separable"
	}
	return reviewadapter.Finding{Severity: severity, Impact: impact, File: "node.txt", Line: 1, Text: text}
}

// NeedsChanges is a unit check that rules needs_changes with the findings.
func NeedsChanges(findings ...reviewadapter.Finding) UnitCheck {
	return UnitCheck{Verdict: reviewadapter.VerdictNeedsChanges, Findings: findings}
}

// TestFixCallNamesTheFailedTestsOfTheVerification (CRW-1469, CRW-1488 item 16): the failed tests a failing verification names reach
// the executor: the crw.verify row records them and the fix call of the node's thread names each failed step's tests, packages and
// the last lines of its output.
func TestFixCallNamesTheFailedTestsOfTheVerification(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Verify.Fail = map[string][]int{"A": {1}}
	l.FailedTests = map[string][]ops.FailedStep{"A": {{Job: "go-product", Step: "tests", Tests: []string{"TestGuard"}, Packages: []string{"example.com/m/a"},
		Tail: "    guard_test.go:9: the guard is open\n--- FAIL: TestGuard (0.01s)"}}}
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	verifies := l.Rows("crw.verify")
	if len(verifies) < 2 || verifies[0].Data["failed_tests"] == nil || verifies[1].Data["failed_tests"] != nil {
		t.Fatalf("the verify rows %+v", verifies)
	}
	fix := textOfCall(l, callNamed(t, l, "fix:A:executor:r1").ID)
	for _, want := range []string{"VERIFICATION FAILED (" + ops.CILocalCommand + "):\n- validate/tests: failed\n\nFAILED TESTS (",
		"\n- go-product/tests: TestGuard (packages example.com/m/a)\n        guard_test.go:9: the guard is open\n    --- FAIL: TestGuard (0.01s)"} {
		if !strings.Contains(fix, want) {
			t.Errorf("the fix call lacks %q:\n%s", want, fix)
		}
	}
}

// ---- the crosscheck of the close (CRW-1473) ----

// Crosscheck is one answer of the lab's crosschecker to one pass. An empty State is ok, with the verdict verified unless one is given;
// Do runs before the answer.
type Crosscheck struct {
	State    reviewadapter.State
	Verdict  string
	Findings []reviewadapter.Finding
	Do       func()
}

// CrosscheckCall is one pass the lab's crosschecker was asked: its key (<ms>/r<round>/e<evidence round>/<pass><attempt>), the request
// and whether its checkout existed during the call.
type CrosscheckCall struct {
	Key     string
	Request reviewadapter.Request
	Existed bool
}

// LabCrosschecker is the lab's ops.UnitChecker of crw.crosscheck, with the fake checkout of its passes: a fresh directory
// <LandRoot>/crosscheck-wt/<ms>-r<round>-e<evidence round>-<pass><attempt> per pass, removed after it.
type LabCrosschecker struct {
	mu        sync.Mutex
	queue     map[string][]Crosscheck
	calls     []CrosscheckCall
	checkouts []string
}

func newLabCrosschecker() *LabCrosschecker { return &LabCrosschecker{queue: map[string][]Crosscheck{}} }

var labCheckoutName = regexp.MustCompile(`^(.+)-r(\d+)-e(\d+)-([AB])(\d+)$`)

// seam is the crosscheck of the lab's lane: this checker, the shipped profiles (astra, then grok) and the fake checkout under the lab's
// run directory root. Its authors are set from the lab's policy when a run starts (Config).
func (c *LabCrosschecker) seam(root string) *ops.Crosscheck {
	checkout := func(_ context.Context, ms string, round, evidenceRound int, pass string, attempt int, _ string) (string, func() error, error) {
		dir := filepath.Join(root, "crosscheck-wt", fmt.Sprintf("%s-r%d-e%d-%s%d", ms, round, evidenceRound, pass, attempt))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", nil, err
		}
		c.mu.Lock()
		c.checkouts = append(c.checkouts, dir)
		c.mu.Unlock()
		return dir, func() error { return os.RemoveAll(dir) }, nil
	}
	return &ops.Crosscheck{Checker: c, Limit: time.Minute, Checkout: checkout,
		Profiles: func(runDir string) ([]reviewadapter.Profile, error) {
			return []reviewadapter.Profile{reviewadapter.AstraProfile(filepath.Join(runDir, "crosscheck", "answer.strict.json")), reviewadapter.GrokProfile(reviewadapter.AnswerSchema)}, nil
		}}
}

// Calls are the passes the crosschecker was asked, in order.
func (c *LabCrosschecker) Calls() []CrosscheckCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

// Checkouts are the directories the fake checkout made, in order.
func (c *LabCrosschecker) Checkouts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.checkouts)
}

func (c *LabCrosschecker) Check(ctx context.Context, req reviewadapter.Request) (reviewadapter.Result, error) {
	key := filepath.Base(req.Worktree)
	if m := labCheckoutName.FindStringSubmatch(key); m != nil {
		key = m[1] + "/r" + m[2] + "/e" + m[3] + "/" + m[4] + m[5]
	}
	_, statErr := os.Stat(req.Worktree)
	c.mu.Lock()
	c.calls = append(c.calls, CrosscheckCall{Key: key, Request: req, Existed: statErr == nil})
	a := Crosscheck{}
	if q := c.queue[key]; len(q) > 0 {
		a, c.queue[key] = q[0], q[1:]
	}
	c.mu.Unlock()
	if a.Do != nil {
		a.Do()
	}
	if a.State == "" {
		a.State = reviewadapter.StateOK
	}
	verdict := ""
	findings := []reviewadapter.Finding{}
	if a.State == reviewadapter.StateOK {
		verdict = a.Verdict
		if verdict == "" {
			verdict = reviewadapter.VerdictVerified
		}
		for _, f := range a.Findings {
			if f.ID == "" {
				f.ID = reviewadapter.FindingID(f.File, f.Text)
			}
			findings = append(findings, f)
		}
	}
	sum := sha256.Sum256([]byte(req.Prompt))
	return reviewadapter.Result{State: a.State, Verdict: verdict, Findings: findings, EngineFamily: req.Profile.Family, Model: req.Profile.Model, Effort: req.Profile.Effort,
		GuideRev: req.GuideRev, PromptDigest: hex.EncodeToString(sum[:]), ElapsedMS: 1}, nil
}

// Crosschecks scripts the next answers of one pass of a milestone's crosscheck (round, evidence round, pass, attempt), in order; a
// pass no script answers is verified with no finding.
func (l *Lab) Crosschecks(ms string, round, evidenceRound int, pass string, attempt int, answers ...Crosscheck) {
	l.Crosschecker.mu.Lock()
	defer l.Crosschecker.mu.Unlock()
	key := fmt.Sprintf("%s/r%d/e%d/%s%d", ms, round, evidenceRound, pass, attempt)
	l.Crosschecker.queue[key] = append(l.Crosschecker.queue[key], answers...)
}
