// Package engine runs CRW workflow scripts: it gives a loaded script (internal/flow/script) its globals, assigns every
// call its (path, position) identity, journals what the script asked and what it got, and drives agent calls through a
// Backend. The fake backend is the only backend F01 ships; the journal is in memory (store persistence is F06).
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	// Journal receives every row; nil is a fresh in-memory journal.
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
	pauseM sync.Mutex
	paused string

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
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	e := &Engine{cfg: cfg, j: cfg.Journal, runID: cfg.RunID, ctx: rctx, parent: ctx, cancel: cancel, pool: make(chan struct{}, cfg.Pool), loop: script.NewLoop(), byHandle: map[string]CallResult{}, undelivered: map[string]CallResult{}}
	e.j.Append(Event{RunID: e.runID, Kind: KindRun, Event: "started", Data: map[string]any{
		"workflow": workflow, "script_digest": prog.Digest, "helpers": helperDigests(prog), "args_digest": argsDigest,
		"meta": json.RawMessage(prog.MetaJSON),
	}})

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
	return res, nil
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

// callID derives a call's id from its identity, so it is the same on every run and resume.
func (e *Engine) callID(path string, position int) string {
	s := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", e.runID, path, position)))
	return "c_" + hex.EncodeToString(s[:])[:12]
}
