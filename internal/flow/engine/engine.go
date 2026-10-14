// Package engine runs CRW workflow scripts: it gives a loaded script (internal/flow/script) its globals, assigns every
// call its (path, position) identity, journals what the script asked and what it got, and drives agent calls through a
// Backend. The journal is in memory unless the caller gives one (internal/flow/journal persists it in the relay store).
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// Run statuses.
const (
	RunCompleted   = "completed"
	RunScriptError = "script_error"
)

// Error kinds of a script_error run.
const (
	ErrKindJS              = "js_error"
	ErrKindPathConcurrency = "path_concurrency"
	ErrKindStepLimit       = "step_limit"
	ErrKindHeapLimit       = "heap_limit"
	ErrKindUnsettled       = "unsettled"
	ErrKindEngine          = "engine"
	ErrKindCancelled       = "cancelled"
)

// DefaultPool is the number of agent calls a run keeps in flight (the agent pool).
const DefaultPool = 6

// Config is what a run needs.
type Config struct {
	// RunID names the run and is part of every handle. Empty derives it from the script and argument digests.
	RunID string
	// Workflow names the run in its record; empty uses the script's meta.name.
	Workflow string
	Backend  Backend
	Policy   *Policy
	// Journal receives every row; nil is a fresh in-memory journal. A journal that can fail to store a row implements
	// FailingJournal: the engine then sends nothing whose intent row could not be stored.
	Journal Journal
	// Args is the JSON value the script reads as args.
	Args json.RawMessage
	// Pool is the number of agent calls in flight at once (DefaultPool).
	Pool int
	// TokenBudget is the run's token ceiling; 0 sets none. 80% gives an info event and 100% pauses the run.
	TokenBudget int64
	Limits      script.Limits
	// OmitSeq leaves the journal sequence numbers (meta.startSeq, meta.endSeq) out of the CallResult a script holds and
	// of the run's result. They say in which order the host finished concurrent calls, so a run that must print the
	// same bytes every time (crw flow run) omits them; the journal rows keep theirs.
	OmitSeq bool
	// Now is the clock crw.clock() journals; nil is time.Now.
	Now func() time.Time
	// Workflows resolves workflow(name) and workflow({scriptPath}) for the one nesting level; nil refuses nesting.
	Workflows func(name, scriptPath string) (*script.Program, error)
	// Resume, when set, makes this run a resume of the journal: every call of the run whose last row is an intent is
	// reconciled against the host before the script starts (Reconcile), a call the journal already ended with the same
	// input is not run again, and a call that reconcile decided goes on from where it stopped. The full replay rule (F07)
	// is not part of this: only the calls' own journal rows and input digests are compared.
	Resume *ResumeOptions
	// Prover proves the effect of executor calls against live state when a run resumes (nil: the Backend, when it is a
	// Prover; without either every effect holds). Ops are the crw.<name> calls with effects outside the engine, by name.
	Prover Prover
	Ops    map[string]Op
	// InputSnapshot reads the plan-owned facts a call sees (the node's slice of the plan revision, the criteria digest, the
	// acceptance and integration ids of its dependencies): they are journaled with the call and compared when it resumes,
	// because only an external act changes them. Git facts are never read here: they are rebuilt from the journal. Nil
	// records and compares none.
	InputSnapshot func(ctx context.Context, req SnapshotRequest) (string, error)
	// Control, when set, is how the process that runs the script pauses it from outside (`crw flow pause`, a run deadline).
	Control *Control
	// Upward is the hook the things that go up from the run are handed to (the plan summary, the escalations, the completion digest;
	// summary_hook.go). Nil hands nothing.
	Upward UpwardHook
	// SummaryDocument is the document the run keeps the plan summary in (crw flow run --summary-document): the run records it in a
	// row of its journal before the script runs, and the hook reads it from there.
	SummaryDocument string
	// Session, when set, runs every agent call as a session call (CRW-1316): the run publishes steps for its owner thread and binds
	// the subagents it spawns, instead of starting threads through Backend. The run's started row records the backend
	// (`backend`), and a resume on the other backend is refused.
	Session *SessionConfig
}

// SnapshotRequest is a call as the input snapshot sees it.
type SnapshotRequest struct {
	RunID, CallID, Node, Role, Head string
}

// ResumeOptions are the timings of the reconcile pass of a resumed run; a zero value takes the default.
type ResumeOptions struct {
	// Wait waits for d or until ctx ends (the creation grace, the poll for a connection that is still open); nil sleeps.
	Wait                        func(ctx context.Context, d time.Duration) error
	Grace, Skew, OpenWait, Poll time.Duration
}

// CallMeta is the meta field of a CallResult.
type CallMeta struct {
	Role           string   `json:"role"`
	RequestedPair  Pair     `json:"requestedPair"`
	ConfiguredPair Pair     `json:"configuredPair"`
	ThreadID       string   `json:"threadId"`
	TurnIDs        []string `json:"turnIds"`
	RepairUsed     bool     `json:"repairUsed"`
	Tokens         Tokens   `json:"tokens"`
	StartSeq       int      `json:"startSeq,omitempty"`
	EndSeq         int      `json:"endSeq,omitempty"`
}

// CallResult is what an agent call came to: a frozen object with separate status, reason and output in the script.
type CallResult struct {
	CallID   string          `json:"callId"`
	Path     string          `json:"path"`
	Position int             `json:"position"`
	Label    string          `json:"label"`
	Handle   string          `json:"handle"`
	Status   string          `json:"status"`
	Reason   string          `json:"reason"`
	Output   json.RawMessage `json:"output"`
	Meta     CallMeta        `json:"meta"`
}

// Result is what a run came to.
type Result struct {
	RunID     string          `json:"runId"`
	Workflow  string          `json:"workflow"`
	Status    string          `json:"status"`
	ErrorKind string          `json:"errorKind,omitempty"`
	Error     string          `json:"error,omitempty"`
	Value     json.RawMessage `json:"value"`
	Calls     []CallResult    `json:"calls"`
	Phases    []string        `json:"phases"`
	Logs      []string        `json:"logs"`
	Spent     int64           `json:"tokensSpent"`
}

// Engine is one run.
type Engine struct {
	cfg   Config
	j     Journal
	runID string
	// epoch is which process of the run this is: its started rows, this one's included (the wake id carries it, CRW-1372).
	epoch int
	ctx   context.Context
	// parent is the context the caller gave Run: when it is done, the run ends.
	parent context.Context
	// cancel ends every call in flight (run abort).
	cancel context.CancelFunc
	pool   chan struct{}
	loop   *script.Loop
	// inflight counts the goroutines of agent calls that have not written their terminal row; a run reports its end
	// only after all of them have.
	inflight sync.WaitGroup
	// undelivered holds, by call id, the results a call ended on that the loop has not handed to the script yet; a run
	// that is aborted reports them with the delivered ones.
	undeliveredM sync.Mutex
	undelivered  map[string]CallResult

	spent  atomic.Int64
	warned atomic.Bool
	// hookSem holds the upward hook to one call at a time (summary_hook.go).
	hookSem chan struct{}
	pauseM  sync.Mutex
	paused  string
	// turns are the calls with a turn in flight; quotaEscalated is set once the run's quota escalation was written.
	turnsM         sync.Mutex
	turns          map[*callRun]struct{}
	quotaEscalated atomic.Bool
	// stopped are the nodes that take no more work, by the reason: a restore_snapshot_mismatch, or a recovery_hold (a call of
	// the node has an outcome nobody can prove).
	stoppedM sync.Mutex
	stopped  map[string]string
	// sessR routes the dispatch reports of a session run (Config.Session) to the calls that wait for them; sessW is what the run
	// knows of its waiting steps, to wake the owner thread (sessionwake.go).
	sessR *sessionRouter
	sessW *sessionWaker
	// recovery is what reconcile decided for the calls of a resumed run; startSeq is the first row this run's calls write,
	// and every row before it belongs to a process that no longer exists (or to the reconcile pass that read them).
	recovery map[string]*Recovery
	startSeq int
	// prior is what the previous processes journaled (built after the reconcile pass of a resumed run); lineage numbers this
	// process among the runs of the journal: 0 for the first, one more for each resume.
	prior   *priorIndex
	lineage int
	// proofs are the effect proofs of the nodes' executor calls (read once per resume); boundaries are the files of the
	// nodes after the positions that held or ran (the last commit and the pending tree after it); restored are the nodes whose
	// boundary this resume rebuilt.
	proofM     sync.Mutex
	proofs     map[string]NodeProof
	boundaryM  sync.Mutex
	boundaries map[string]*boundary
	restoreM   sync.Mutex
	restored   map[string]bool
	// claimed are the calls of the previous process that a position of the resumed script took over (resumePrior): they go
	// on in this process, so a new attempt of their node does not supersede them. superseding are the calls of the previous
	// process that a new attempt ends (or ended) superseded. Both are decided under claimedM, so a call is either claimed or
	// superseded and never both: whichever decision comes first stands, and a later claim of a call that was superseded
	// runs its position as a call of its own.
	claimedM   sync.Mutex
	claimed    map[string]bool
	superseded map[string]*supersedeOp
	// facts are what crw.callFacts answers beyond the journal: the calls this process ran live (signal.go).
	facts signalFacts
	// escalated are the calls whose dispatch_unknown escalation was written by this run.
	escalatedM sync.Mutex
	escalated  map[string]bool

	// Everything below is touched only by the loop goroutine.
	fatalKind string
	fatalErr  error
	// mainPromise is the promise of the main script's completion.
	mainPromise *goja.Promise
	results     []CallResult
	byHandle    map[string]CallResult // the delivered results that hold a handle, by handle
	phases      []string
	logs        []string
	phase       string
}

func digestJSON(v []byte) string {
	s := sha256.Sum256(v)
	return hex.EncodeToString(s[:])
}

// Run executes a loaded script to the end and reports how it ended. An error is returned only when the run could not
// start (a bad configuration); a script that fails ends with Status script_error.
func Run(ctx context.Context, prog *script.Program, cfg Config) (*Result, error) {
	if cfg.Backend == nil || cfg.Policy == nil {
		return nil, errors.New("engine: a backend and a policy are required")
	}
	if cfg.Journal == nil {
		cfg.Journal = NewMemoryJournal()
	}
	if cfg.Pool <= 0 {
		cfg.Pool = DefaultPool
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	args := cfg.Args
	if len(args) == 0 {
		args = json.RawMessage("null")
	}
	if !json.Valid(args) {
		return nil, errors.New("engine: args is not valid JSON")
	}
	argsDigest := digestJSON(args)
	if cfg.RunID == "" {
		cfg.RunID = "run-" + digestJSON([]byte(prog.Digest + "\x00" + argsDigest))[:12]
	}
	workflow := cfg.Workflow
	if workflow == "" {
		workflow, _ = prog.Meta["name"].(string)
	}
	if workflow == "" {
		workflow = prog.Name
	}
	backend := BackendAppServer
	if s := cfg.Session; s != nil {
		backend = BackendSession
		if s.Host == nil || s.Bus == nil || s.Owner == "" {
			return nil, errors.New("engine: a session run needs a session host, a step bus and its owner thread")
		}
	}
	// The run's backend is recorded in its started row: a resume on the other one is an input error, refused before anything
	// is written (the open calls of the run are threads or subagents of the backend it was started on).
	if cfg.Resume != nil {
		if recorded := RecordedBackend(cfg.Journal.Events(), cfg.RunID); recorded != backend && hasStarted(cfg.Journal.Events(), cfg.RunID) {
			return nil, fmt.Errorf("engine: run %s was started on the %s backend and cannot be resumed on the %s backend: resume it with the backend its started row records", cfg.RunID, recorded, backend)
		}
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	e := &Engine{cfg: cfg, hookSem: make(chan struct{}, 1), j: cfg.Journal, runID: cfg.RunID, ctx: rctx, parent: ctx, cancel: cancel, pool: make(chan struct{}, cfg.Pool), loop: script.NewLoop(), byHandle: map[string]CallResult{}, undelivered: map[string]CallResult{}, turns: map[*callRun]struct{}{}, proofs: map[string]NodeProof{}, boundaries: map[string]*boundary{}, restored: map[string]bool{}}
	// A run that resumes a journal read the policy under another digest before: the change is journaled before any live
	// call, and only calls that start a thread afterwards take the new pairs.
	prior, repinned := priorPolicy(e.j, e.runID, cfg.Policy)
	e.lineage = lineageOf(e.j, e.runID)
	startedData := map[string]any{
		"workflow": workflow, "script_digest": prog.Digest, "helpers": helperDigests(prog), "args_digest": argsDigest,
		"meta": json.RawMessage(prog.MetaJSON), "policy_source": cfg.Policy.Source, "policy_digest": cfg.Policy.Digest, "backend": backend,
	}
	if len(cfg.Policy.Warnings) > 0 {
		// what the table's reading warned about (CRW-1330): a run starts on it and keeps the warnings in its record
		startedData["policy_warnings"] = slices.Clone(cfg.Policy.Warnings)
	}
	if cfg.Session != nil {
		// the pairs a session run pins: its process binds the owner's line with them (Started), and `crw flow next` reads them as the
		// pairs the process runs on, never the role store read again (CRW-1372)
		pairs := map[string]any{}
		for name, spec := range cfg.Policy.Roles {
			pairs[name] = map[string]any{"model": spec.Pair.Model, "effort": spec.Pair.Effort}
		}
		startedData["role_pairs"] = pairs
		if cfg.Session.Process != "" {
			startedData["process"] = cfg.Session.Process
		}
	}
	started := e.j.Append(Event{RunID: e.runID, Kind: KindRun, Event: "started", Data: startedData})
	e.epoch = startedRows(e.j.Events(), e.runID)
	if repinned {
		e.j.Append(Event{RunID: e.runID, Kind: KindRun, Event: EvPolicyRepinned, Data: map[string]any{
			"old": prior.Digest, "new": cfg.Policy.Digest, "old_source": prior.Source, "new_source": cfg.Policy.Source,
		}})
	}
	if cfg.Session != nil && cfg.Session.Started != nil {
		// the run's own record first, then what is made from it: the owner's line with the pairs the started row pinned (CRW-1372)
		cfg.Session.Started(cfg.Policy)
	}

	e.startSeq = started.Seq
	if cfg.Control != nil {
		cfg.Control.bind(e)
		defer cfg.Control.end()
	}
	if cfg.Resume != nil {
		e.resume(ctx)
		// what reconcile appended is the journal's state too: the calls are compared with every row written so far
		if seq := lastRunSeq(e.j.Events()); seq > 0 {
			e.startSeq = seq + 1
		}
		e.prior = newPriorIndex(e.j, e.startSeq)
		e.sweepOwnDirectories()
	}
	var routed, woken chan struct{}
	if cfg.Session != nil {
		e.sessR = newSessionRouter()
		routed = make(chan struct{})
		go e.routeReports(routed)
		if cfg.Session.Wake != nil {
			e.sessW = newSessionWaker()
			woken = make(chan struct{})
			go e.wakeLoop(woken)
		}
	}
	e.noteSummaryDocument()
	if cfg.Upward != nil {
		if n, ok := e.j.(CommitNotifier); ok {
			n.SetOnCommit(e.batchCommitted)
			defer n.SetOnCommit(nil)
		}
		e.hookRun("started", func(h UpwardHook) error { return h.RunStarted(ctx, e.runID) })
	}
	root := &pathState{id: RootPath}
	main, err := e.newScriptVM(prog, root, 0, args)
	if err != nil {
		return nil, err
	}
	promise, err := main.start()
	if err != nil {
		e.fail(limitKind(err), err)
	}
	e.mainPromise = promise
	e.noteMain()
	// A cancelled parent context wakes a loop that waits for completions, so the run ends instead of waiting for work
	// that the cancellation will not deliver.
	watching := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			e.loop.Post(nil, e.noteMain)
		case <-watching:
		}
	}()
	e.loop.RunObserved(func(err error) { e.fail(limitKind(err), err) }, e.noteMain)
	close(watching)
	e.noteMain()

	res := &Result{RunID: e.runID, Workflow: workflow, Status: RunCompleted, Value: json.RawMessage("null")}
	// What the script came to is read inside the VM under the step and heap limits: rendering a thrown value or
	// serialising the returned one runs the script's own toString, toJSON and getters.
	switch {
	case e.fatalErr == errMainRejected:
		var text string
		if err := main.rt.Enter(func() error {
			var err error
			text, err = main.textOf(promise.Result())
			return err
		}); err != nil {
			e.fatalKind, e.fatalErr = valueErrKind(err), fmt.Errorf("the script failed and its error cannot be rendered: %w", err)
		} else {
			e.fatalKind, e.fatalErr = ErrKindJS, errors.New(text)
		}
	case e.fatalErr != nil:
	case promise == nil:
		e.fail(ErrKindEngine, errors.New("the script did not start"))
	case promise.State() == goja.PromiseStatePending:
		e.fail(ErrKindUnsettled, errors.New("the script awaits a promise that nothing will settle"))
	default:
		var v json.RawMessage
		if err := main.rt.Enter(func() error {
			var err error
			v, err = main.jsonOfErr(promise.Result())
			return err
		}); err != nil {
			e.fail(valueErrKind(err), fmt.Errorf("the script's value cannot be serialised: %w", err))
		} else {
			res.Value = v
		}
	}
	if e.fatalErr != nil {
		res.Status, res.ErrorKind, res.Error = RunScriptError, e.fatalKind, e.fatalErr.Error()
	}
	// Every call still in flight was cancelled by the abort (or has finished); its terminal row is written before the
	// run's last one.
	e.cancel()
	e.inflight.Wait()
	if routed != nil {
		<-routed
	}
	if woken != nil {
		<-woken
	}
	if err := journalErr(e.j); err != nil && res.Status == RunCompleted {
		res.Status, res.ErrorKind, res.Error = RunScriptError, ErrKindEngine, journalError(err).Error()
	}
	e.undeliveredM.Lock()
	for _, r := range e.undelivered {
		e.results = append(e.results, r)
	}
	e.undeliveredM.Unlock()
	res.Spent = e.spent.Load()
	sort.SliceStable(e.results, func(i, k int) bool {
		if e.results[i].Path != e.results[k].Path {
			return e.results[i].Path < e.results[k].Path
		}
		return e.results[i].Position < e.results[k].Position
	})
	res.Calls = e.results
	res.Phases = e.phases
	res.Logs = e.logs
	e.j.Append(Event{RunID: e.runID, Kind: KindRun, Event: "finished", Data: map[string]any{"status": res.Status, "error_kind": res.ErrorKind}})
	if cfg.Upward != nil {
		// the journal no longer notifies: the finish is the last point, and the hook reads the whole journal
		if n, ok := e.j.(CommitNotifier); ok {
			n.SetOnCommit(nil)
		}
		e.hookRun("finished", func(h UpwardHook) error { return h.RunFinished(ctx, e.runID, res.Status) })
	}
	// the finished row is written with the rows still buffered: when that write fails the run did not end on disk, and the
	// caller is told so instead of getting a completed run
	if err := journalErr(e.j); err != nil && res.Status == RunCompleted {
		res.Status, res.ErrorKind, res.Error = RunScriptError, ErrKindEngine, journalError(err).Error()
	}
	return res, nil
}

// lineageOf numbers a process among the runs of a journal: the number of started rows the journal holds already.
func lineageOf(j Journal, runID string) int {
	n := 0
	for _, ev := range j.Events() {
		if ev.Kind == KindRun && ev.RunID == runID && ev.Event == "started" {
			n++
		}
	}
	return n
}

// EvPolicyRepinned is the run event a resumed run writes when the policy file it reads has another digest than the one
// the run last recorded.
const EvPolicyRepinned = "policy_repinned"

// priorPolicy is the policy the journal last recorded for the run (its source and digest) and whether the policy the
// run reads now has another digest.
func priorPolicy(j Journal, runID string, now *Policy) (prior Policy, repinned bool) {
	found := false
	for _, ev := range j.Events() {
		if ev.Kind != KindRun || ev.RunID != runID {
			continue
		}
		digest, ok := ev.Data["policy_digest"].(string)
		if !ok {
			continue
		}
		prior.Digest, found = digest, true
		prior.Source, _ = ev.Data["policy_source"].(string)
	}
	return prior, found && prior.Digest != now.Digest
}

func helperDigests(p *script.Program) []map[string]any {
	var out []map[string]any
	for _, h := range p.Helpers {
		out = append(out, map[string]any{"spec": h.Spec, "digest": h.Digest})
	}
	return out
}

func limitKind(err error) string {
	var le *script.LimitError
	if errors.As(err, &le) {
		if le.Kind == "heap" {
			return ErrKindHeapLimit
		}
		return ErrKindStepLimit
	}
	return ErrKindEngine
}

// valueErrKind is the error kind of a failure to read the script's final value: a limit, or the script's own throw.
func valueErrKind(err error) string {
	var le *script.LimitError
	if errors.As(err, &le) {
		return limitKind(err)
	}
	return ErrKindJS
}

// errMainRejected stands for the error of a main script whose promise was rejected until the run renders it (under
// the step and heap limits, after the loop has stopped).
var errMainRejected = errors.New("the script failed")

// noteMain ends the run as soon as the main script has failed or the caller's context is done, not after the work
// that is still queued: an abort must stop the script's other paths before they send another request. It runs after
// every step of the loop and when a script opens a call.
func (e *Engine) noteMain() {
	if e.fatalErr != nil {
		return
	}
	if e.parent.Err() != nil {
		e.fail(ErrKindCancelled, errors.New("the run was cancelled"))
		return
	}
	if e.mainPromise != nil && e.mainPromise.State() == goja.PromiseStateRejected {
		e.fail(ErrKindJS, errMainRejected)
	}
}

// fail ends the run: the first failure is its error, every call in flight is cancelled and the loop is aborted, so no
// completion resumes the script and no pending operation keeps the run waiting.
func (e *Engine) fail(kind string, err error) {
	if e.fatalErr == nil {
		e.fatalKind, e.fatalErr = kind, err
	}
	e.cancel()
	e.loop.Abort()
}

// failResolve reports an error from resolving a script promise. An interrupt is the step or heap limit, which the
// runtime reports itself when the step ends.
func (e *Engine) failResolve(err error) {
	var ie *goja.InterruptedError
	if errors.As(err, &ie) {
		return
	}
	e.fail(ErrKindEngine, err)
}

// Pause stops the run from sending any further request: a call that would send one ends held with the reason.
func (e *Engine) pause(reason string) {
	e.pauseM.Lock()
	if e.paused == "" {
		e.paused = reason
	}
	e.pauseM.Unlock()
}

func (e *Engine) pauseReason() string {
	e.pauseM.Lock()
	defer e.pauseM.Unlock()
	return e.paused
}

// addTokens accounts a turn's tokens against the run's ceiling: 80% gives an info event, 100% pauses the run.
func (e *Engine) addTokens(n int64) {
	spent := e.spent.Add(n)
	total := e.cfg.TokenBudget
	if total <= 0 {
		return
	}
	if spent*100 >= total*80 && e.warned.CompareAndSwap(false, true) {
		e.j.Append(Event{RunID: e.runID, Kind: KindFlow, Event: "budget_warning", Data: map[string]any{"spent": spent, "total": total}})
	}
	if spent >= total {
		e.pause(ReasonBudgetExhausted)
	}
}

// tokensUnmeasured journals a completed turn whose usage was never reported. Under a token ceiling the run cannot tell
// what it has spent any more, so it stops sending requests the way an exhausted budget does.
//
// A session call's turn whose rollout could not be read is recorded the same way and pauses nothing (§3.3): the run's ceiling
// counts the calls it measured.
func (e *Engine) tokensUnmeasured(c *callRun, turnID string) {
	ceiling := e.cfg.TokenBudget > 0
	pauses := ceiling && c.sess == nil
	e.j.Append(Event{RunID: e.runID, Kind: KindFlow, CallID: c.id, Path: c.path.id, Position: c.position, Op: "agent", Label: c.label, Event: "tokens_unmeasured",
		Data: map[string]any{"call_id": c.id, "node": c.plan.node, "turn_id": turnID, "ceiling": ceiling, "pauses": pauses}})
	if pauses {
		e.pause(ReasonBudgetExhausted)
	}
}

// stopNode ends the node's work for the rest of the run for a reason; nodeStopped asks whether it was. A call without a
// node is never stopped.
func (e *Engine) stopNode(node, reason string) {
	if node == "" {
		return
	}
	e.stoppedM.Lock()
	if e.stopped == nil {
		e.stopped = map[string]string{}
	}
	if e.stopped[node] == "" {
		e.stopped[node] = reason
	}
	e.stoppedM.Unlock()
}

func (e *Engine) nodeStopped(node string) bool { return e.nodeStopReason(node) != "" }

// nodeStopReason is the reason a node was stopped for, "" when it was not.
func (e *Engine) nodeStopReason(node string) string {
	if node == "" {
		return ""
	}
	e.stoppedM.Lock()
	defer e.stoppedM.Unlock()
	return e.stopped[node]
}

// claim records that the resumed script took over a call of the previous process, unless a new attempt has superseded it
// or is superseding it: it reports whether the call is the position's to go on with.
func (e *Engine) claim(id string) bool {
	e.claimedM.Lock()
	defer e.claimedM.Unlock()
	if e.superseded[id] != nil {
		return false
	}
	if e.claimed == nil {
		e.claimed = map[string]bool{}
	}
	e.claimed[id] = true
	return true
}

// supersedeOp is the end of one call of the previous process by a new attempt. The call that decided it does the work (the
// interruption of a turn it may still run, the superseded row); a call that wants the same end waits for it.
type supersedeOp struct {
	done chan struct{}
	err  error
}

// beginSupersede decides, under the same lock as claim, whether the new attempt ends the call: it may not when the resumed
// script claimed it (unless it is the call's own position, which never claims itself) and it need not when another attempt
// already does. owner is set for the one caller that does the work; the others wait on op.done.
func (e *Engine) beginSupersede(id string, own bool) (op *supersedeOp, owner, skip bool) {
	e.claimedM.Lock()
	defer e.claimedM.Unlock()
	if !own && e.claimed[id] {
		return nil, false, true
	}
	if prior := e.superseded[id]; prior != nil {
		return prior, false, false
	}
	if e.superseded == nil {
		e.superseded = map[string]*supersedeOp{}
	}
	op = &supersedeOp{done: make(chan struct{})}
	e.superseded[id] = op
	return op, true, false
}

// endSupersede finishes the work of the owner. A failed end (a turn that cannot be confirmed interrupted) leaves the call
// open, so it is forgotten: nothing was decided for it.
func (e *Engine) endSupersede(id string, op *supersedeOp, err error) {
	op.err = err
	if err != nil {
		e.claimedM.Lock()
		delete(e.superseded, id)
		e.claimedM.Unlock()
	}
	close(op.done)
}

// resume reconciles the journal's open calls against the host before anything else runs, and holds every node that has a
// call with an outcome nobody can prove.
func (e *Engine) resume(ctx context.Context) {
	e.recovery = map[string]*Recovery{}
	// a session run's calls are decided from their own rows when the script reaches them (session.go): reconcile is the App
	// Server's
	if host, ok := e.cfg.Backend.(ReconHost); ok && e.cfg.Session == nil {
		o := e.cfg.Resume
		wait := o.Wait
		if wait == nil {
			wait = SleepWait
		}
		// a context that ends mid-pass ends the run's calls too; what the pass decided before that is kept
		rep, _ := Reconcile(ctx, e.j, ReconcileConfig{RunID: e.runID, Host: host, Now: e.cfg.Now, Wait: wait, Grace: o.Grace, Skew: o.Skew, OpenWait: o.OpenWait, Poll: o.Poll})
		if rep != nil {
			e.recovery = rep.ByCall
		}
		for _, rec := range e.recovery {
			if rec.Action == RecoverPending {
				e.stopNode(rec.Node, ReasonRecoveryHold)
			}
		}
	}
	for node := range RecoveryHolds(e.j, e.runID) {
		e.stopNode(node, ReasonRecoveryHold)
	}
}

// journalFailed ends the run because a row could not be stored. It may be called from any goroutine: the calls in flight are
// cancelled at once, and the run's failure is set on the loop's goroutine (the run reports the journal's error at its end in
// any case).
func (e *Engine) journalFailed(err error) {
	e.cancel()
	e.loop.Post(nil, func() { e.fail(ErrKindEngine, journalError(err)) })
}

func journalError(err error) error { return fmt.Errorf("the journal cannot be written: %w", err) }

// escalateUnknown writes the dispatch_unknown escalation of a call once.
func (e *Engine) escalateUnknown(c *callRun, detail string) {
	e.escalatedM.Lock()
	if e.escalated == nil {
		e.escalated = map[string]bool{}
	}
	done := e.escalated[c.id]
	e.escalated[c.id] = true
	e.escalatedM.Unlock()
	if done {
		return
	}
	c.mu.Lock()
	last := c.lastEvent
	c.mu.Unlock()
	e.j.Append(Event{RunID: e.runID, Kind: KindFlow, CallID: c.id, Path: c.path.id, Position: c.position, Op: "agent", Label: c.label, Event: EvEscalation,
		Data: map[string]any{"reason": ReasonDispatchUnknown, "node": c.plan.node, "call_id": c.id, "detail": detail, "last_event": last}})
}

// callID derives a call's id from its identity, so it is the same on every run and resume.
func (e *Engine) callID(path string, position int) string {
	s := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", e.runID, path, position)))
	return "c_" + hex.EncodeToString(s[:])[:12]
}
