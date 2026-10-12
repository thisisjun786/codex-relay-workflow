// Package agent is the App Server backend of the flow engine: it runs agent calls as persistent Codex 0.154 threads. A
// role's thread is started with the role's prompt as its developer instructions, its model and effort and a call-private
// working directory; the pair it reads back is the configured pair; every turn repeats that pair, carries the call's
// operation marker (the first input line and clientUserMessageId) and the call's outputSchema; and a turn that gets no
// notification is read from the host by its recorded id. Executors run in attempt worktrees made by the Allocator, which
// is also what crw.worktree (F08) wraps.
//
// Every engine thread is persistent. Codex 0.154 refuses includeTurns and thread/turns/list for an ephemeral thread, which
// would make pull recovery and the audit of a run impossible, so the backend never asks for one.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/roles"
)

// Default timings of a backend.
const (
	// DefaultPollEvery is how long a turn waits for a notification before it reads the turn from the host.
	DefaultPollEvery = 10 * time.Second
	// DefaultInterruptWait is how long an interrupted turn is waited for before the call gives up on its answer.
	DefaultInterruptWait = 15 * time.Second
)

// Config is what a Backend needs.
type Config struct {
	// Client is the App Server connection. Build it with ClientOption(Threads) so the server requests of the threads
	// this backend started are answered by the flow responder.
	Client *appserver.Client
	// Allocator decides and makes the working directories of the calls.
	Allocator *Allocator
	// Threads records the threads this backend started; nil keeps a private record.
	Threads *Threads
	// Prompt returns the developer instructions of a role, and of the QA capture call (SP-QA-1) of a final gate when qa is set; nil
	// uses the role definition and its CRW overlay.
	Prompt func(role string, qa bool) (string, error)
	// PollEvery and InterruptWait override the defaults (a test shortens them).
	PollEvery, InterruptWait time.Duration
	// Owner names the process the backend lives in (ProcessOwner when empty); every creation intent records it.
	Owner string
	// OwnerAlive says whether the process an owner names can still hold a connection (ProcessAlive when nil).
	OwnerAlive func(owner string) bool
	// Source is the Thread.source of the threads the backend creates (DefaultSource when empty).
	Source string
	// Bound makes the backend tell the native spawn hook which threads are the run's own (F14); nil writes no binding.
	Bound *BoundConfig
}

// Backend is the engine.Backend that runs calls on the App Server. It also places calls (engine.Placer), forks threads
// (engine.Forker) and leaves a request that got no answer to the reconcile pass (engine.Reconciled).
type Backend struct {
	cfg     Config
	client  *appserver.Client
	threads *Threads

	mu         sync.Mutex
	tokenTotal map[string]engine.Tokens
	// orchestrator is the thread BindOrchestrator bound, until CloseOrchestrator.
	orchestrator string
}

var (
	_ engine.Backend    = (*Backend)(nil)
	_ engine.Placer     = (*Backend)(nil)
	_ engine.Forker     = (*Backend)(nil)
	_ engine.Reconciled = (*Backend)(nil)
	_ engine.Restorer   = (*Backend)(nil)
	_ engine.Prover     = (*Backend)(nil)
	_ engine.Owned      = (*Backend)(nil)
	_ engine.ReconHost  = (*Backend)(nil)
)

// New returns a backend.
func New(cfg Config) (*Backend, error) {
	if cfg.Client == nil || cfg.Allocator == nil {
		return nil, errors.New("agent: a backend needs an App Server client and an allocator")
	}
	return newBackend(cfg)
}

// NewHost returns a backend that only reads the host (engine.ReconHost): `crw flow recover` asks it about the threads and
// turns of a run that is not running. It has no allocator, so it places no call and makes no worktree.
func NewHost(cfg Config) (*Backend, error) {
	if cfg.Client == nil {
		return nil, errors.New("agent: a host reader needs an App Server client")
	}
	return newBackend(cfg)
}

var errNoAllocator = errors.New("agent: this backend only reads the host: it has no allocator")

func newBackend(cfg Config) (*Backend, error) {
	if cfg.Bound != nil {
		if err := cfg.Bound.validate(); err != nil {
			return nil, err
		}
	}
	if cfg.Threads == nil {
		cfg.Threads = NewThreads()
	}
	if cfg.Prompt == nil {
		cfg.Prompt = func(role string, qa bool) (string, error) { return roles.Prompt(role, roles.Options{QA: qa}) }
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = DefaultPollEvery
	}
	if cfg.InterruptWait <= 0 {
		cfg.InterruptWait = DefaultInterruptWait
	}
	if cfg.Owner == "" {
		cfg.Owner = ProcessOwner()
	}
	if cfg.OwnerAlive == nil {
		cfg.OwnerAlive = ProcessAlive
	}
	if cfg.Source == "" {
		cfg.Source = DefaultSource
	}
	return &Backend{cfg: cfg, client: cfg.Client, threads: cfg.Threads, tokenTotal: map[string]engine.Tokens{}}, nil
}

// hold keeps this connection subscribed to a thread for as long as the backend lives. The client drops the subscription
// of a thread it started when the first turn there ends (it only retains a never-run thread), and a host sends a thread's
// notifications only to subscribed connections, so without the hold the second turn of a call - a repair, a fix - would
// never be heard on the bus.
func (b *Backend) hold(ctx context.Context, thread string) error {
	b.threads.Add(thread)
	if b.client.ThreadHeld(thread) {
		return nil
	}
	if err := b.client.HoldThread(ctx, thread); err != nil {
		return requestError(err)
	}
	return nil
}

// Close releases the subscriptions the backend holds. It does not close the client.
func (b *Backend) Close() {
	for _, id := range b.threads.IDs() {
		b.client.ReleaseThread(id)
	}
}

// LeavesLostRequestsOpen implements engine.Reconciled: a request that got no answer is decided by reconcile (F05), so
// the call keeps the intent row it wrote and is never held.
func (b *Backend) LeavesLostRequestsOpen() bool { return true }

// Place implements engine.Placer.
func (b *Backend) Place(req engine.PlaceRequest) (engine.Placement, error) {
	if b.cfg.Allocator == nil {
		return engine.Placement{}, errNoAllocator
	}
	return b.cfg.Allocator.Place(req)
}

// Establish implements engine.Placer.
func (b *Backend) Establish(ctx context.Context, req engine.PlaceRequest, p engine.Placement) error {
	if b.cfg.Allocator == nil {
		return errNoAllocator
	}
	return b.cfg.Allocator.Establish(ctx, req, p)
}

// Recheck implements engine.Placer.
func (b *Backend) Recheck(ctx context.Context, p engine.Placement) error {
	if b.cfg.Allocator == nil {
		return errNoAllocator
	}
	return b.cfg.Allocator.Recheck(ctx, p)
}

// Snapshot implements engine.Placer.
func (b *Backend) Snapshot(ctx context.Context, path string) (string, error) {
	if b.cfg.Allocator == nil {
		return "", errNoAllocator
	}
	return b.cfg.Allocator.Snapshot(ctx, path)
}

// Restore implements engine.Restorer.
func (b *Backend) Restore(ctx context.Context, req engine.RestoreRequest) (engine.Restored, error) {
	if b.cfg.Allocator == nil {
		return engine.Restored{}, errNoAllocator
	}
	return b.cfg.Allocator.Restore(ctx, req)
}

// Preserve implements engine.Restorer.
func (b *Backend) Preserve(ctx context.Context, req engine.PreserveRequest) (engine.Preserved, error) {
	if b.cfg.Allocator == nil {
		return engine.Preserved{}, errNoAllocator
	}
	return b.cfg.Allocator.Preserve(ctx, req)
}

// WorktreeExists reports whether the directory of an old worktree is still there.
func (b *Backend) WorktreeExists(path string) bool {
	return b.cfg.Allocator != nil && b.cfg.Allocator.WorktreeExists(path)
}

// ProveNode implements engine.Prover: the effects of a node's executor calls against its current worktree (internal/flow/ops).
func (b *Backend) ProveNode(ctx context.Context, events []engine.Event, node string) (engine.NodeProof, error) {
	if b.cfg.Allocator == nil {
		return nil, errNoAllocator
	}
	return ops.Prover{Git: ops.GitFunc(b.cfg.Allocator.git)}.ProveNode(ctx, events, node)
}

// Threads is the set of threads a backend started: the server requests about them are the flow's to refuse.
type Threads struct {
	mu  sync.Mutex
	ids map[string]bool
}

// NewThreads returns an empty set.
func NewThreads() *Threads { return &Threads{ids: map[string]bool{}} }

// Add records a thread.
func (t *Threads) Add(id string) {
	t.mu.Lock()
	t.ids[id] = true
	t.mu.Unlock()
}

// IDs returns the threads recorded, in no order.
func (t *Threads) IDs() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.ids))
	for id := range t.ids {
		out = append(out, id)
	}
	return out
}

// Owns reports whether the backend started the thread.
func (t *Threads) Owns(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ids[id]
}

// ClientOption installs the flow's server-request responder for the threads in t on a client: an approval or a question
// on such a thread is refused (a flow run has nobody to answer it), and every other thread keeps the client's default.
func ClientOption(t *Threads) appserver.Option {
	return appserver.WithServerRequestResponder(t.Owns, appserver.DenyAnswer)
}

// infoName is the name of a CodexErrorInfo value: the string of a plain variant, or the only member name of a variant
// that carries data ({"httpConnectionFailed": {...}}). Anything else (null, a number, an object with other than one member)
// has no name.
func infoName(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case map[string]any:
		if len(x) == 1 {
			for name := range x {
				return name
			}
		}
	}
	return ""
}

// rpcInfo is the CodexErrorInfo variant the error data of an RPC error carries.
func rpcInfo(object map[string]any) string {
	if data, ok := object["data"].(map[string]any); ok {
		if name := infoName(data["codexErrorInfo"]); name != "" {
			return name
		}
	}
	return infoName(object["codexErrorInfo"])
}

// requestError turns what the client returned for a request into what the engine classifies: an answer that is an error
// is an *engine.RPCError (with the variant its data names), everything else - a lost connection, a phase timeout, a
// withheld write, a cancelled context - is an *engine.TransportError, which is never held.
func requestError(err error) error {
	var rpc *appserver.RPCError
	if errors.As(err, &rpc) {
		return &engine.RPCError{Message: rpc.Message, CodexErrorInfo: rpcInfo(rpc.Object)}
	}
	var already *engine.RPCError
	if errors.As(err, &already) {
		return already
	}
	return &engine.TransportError{Err: err}
}

// threadObject is the Thread member of a thread/start, thread/fork or thread/read answer.
type threadObject struct {
	ID              string  `json:"id"`
	Cwd             string  `json:"cwd"`
	Ephemeral       bool    `json:"ephemeral"`
	Model           *string `json:"model"`
	ReasoningEffort *string `json:"reasoningEffort"`
}

func decodeThread(raw json.RawMessage, method string) (threadObject, error) {
	var answer struct {
		Thread threadObject `json:"thread"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil || answer.Thread.ID == "" {
		return threadObject{}, &engine.TransportError{Err: fmt.Errorf("%s: the answer names no thread", method)}
	}
	return answer.Thread, nil
}
