package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// This file is the reconcile pass of dispatch intents (engine.dispatch.reconcile, F05). A request that was sent and got no
// answer - a lost connection, a run process that died after an intent row or after a send - leaves the call's last row at
// an intent. Before a resume goes on, and before a node starts a new attempt, every such call is looked up on the host by
// the fields the 0.154 host returns (the call's private cwd, the thread's parent, source and creation time, the fork's
// source thread, the turn's clientId marker), and what is found decides the call: an adopted thread, fork or turn, a
// request that is proven absent and may be sent again, or an outcome nobody can prove (outcome_unknown, which holds the
// node). The rule is the bridge's own: a mutation of which a byte was sent is never sent again unless it is proven absent
// (internal/bridge/bridge.go outcome_unknown, retrySafe false).

// Reconcile timings (the creation grace is the one internal/relay/managed/reconcile.go uses).
const (
	DefaultCreationGrace = 2 * time.Minute
	DefaultClockSkew     = 5 * time.Minute
	DefaultOpenWait      = 30 * time.Second
	defaultReconPoll     = time.Second
)

// Rows reconcile writes. EvRecoveryCleared is a call row (the hold of an unknown outcome ended); the others are flow rows
// of the call.
const (
	EvRecoveryCleared    = "recovery_cleared"
	EvOrphanThread       = "orphan_thread"
	EvForeignThread      = "foreign_thread"
	EvCreationClosedSeen = "creation_closed_seen"
	EvCreationResendable = "creation_resendable"
	EvEscalation         = "escalation"
)

// Recovery actions: what a resumed call does with the outcome of its reconcile.
const (
	// RecoverThreadReady is a thread (or fork) that reconcile adopted: the call goes on to its first turn on it.
	RecoverThreadReady = "thread_ready"
	// RecoverResendCreate is a creation proven absent: the call sends thread/start (thread/fork) again, under the same
	// call id and cwd, with the send ordinal Send.
	RecoverResendCreate = "resend_create"
	// RecoverTurn is a turn that is the call's: running (Running) or ended (Outcome).
	RecoverTurn = "turn"
	// RecoverResendTurn is a turn proven absent: it is sent again with the same identity.
	RecoverResendTurn = "resend_turn"
	// RecoverUnknown is an outcome that cannot be established: outcome_unknown was appended and the node is held.
	RecoverUnknown = "unknown"
	// RecoverPending is a creation whose connection is still alive: the answer may yet come, nothing is sent.
	RecoverPending = "pending"
)

// HostThread is a thread as thread/list and thread/read describe it, reduced to what an identity needs.
type HostThread struct {
	ID, Cwd, Source              string
	ParentThreadID, ForkedFromID string
	CreatedAt                    time.Time
	Pair                         Pair
	// Status is the thread status type: idle, notLoaded, active or systemError.
	Status    string
	Ephemeral bool
}

// HostTurn is a turn of a thread's history as thread/turns/list gives it, its items read in full when the host could.
type HostTurn struct {
	ID     string
	Status string
	// Marker is the operation the turn belongs to (the userMessage's clientId, else its first input line), "" when it has
	// none or the items are not readable.
	Marker string
	// Readable is whether the turn's items were read in full (from the list, or from thread/items/list for a turn the list
	// did not return in full). A turn that is not readable is neither without a marker nor absent.
	Readable  bool
	ReadError error
}

// Creation is what a backend can say about a creation request (thread/start or thread/fork) it sent.
type Creation int

const (
	// CreationUnsent: no byte of the request was written; it certainly did not reach the host.
	CreationUnsent Creation = iota + 1
	// CreationOpen: the request was written and the connection that carried it is alive; its answer may still come.
	CreationOpen
	// CreationClosed: the request was written and its connection is closed (or the run process that held it is dead).
	CreationClosed
	// CreationUnknown: nothing is known about the connection that carried it.
	CreationUnknown
)

// ReconHost is the host as reconcile sees it. The App Server backend implements it; the host is only read, apart from the
// archive of an orphan and the interrupt of a turn a new attempt supersedes.
type ReconHost interface {
	// ListThreads returns every thread at cwd on the source the backend connects as, following the cursor to its end. An
	// error means the listing is not complete.
	ListThreads(ctx context.Context, cwd string) ([]HostThread, error)
	// ReadThread reads one thread (thread/read, no turns).
	ReadThread(ctx context.Context, id string) (HostThread, error)
	// Turns returns every turn of a thread, every page, each in full when it can be read. An error means the history is
	// not complete.
	Turns(ctx context.Context, thread string) ([]HostTurn, error)
	// ArchiveThread archives a thread.
	ArchiveThread(ctx context.Context, id string) error
	// InterruptTurn sends turn/interrupt for a turn.
	InterruptTurn(ctx context.Context, thread, turn string) error
	// HarvestTurn reads one turn by its id as the turn loop would have ended it; running is set while it has not ended. A
	// turn whose items cannot be read, or that carries another operation, is an error.
	HarvestTurn(ctx context.Context, thread, turn, op string) (out TurnOutcome, running bool, err error)
	// AwaitTurn waits until a turn has ended and returns its outcome.
	AwaitTurn(ctx context.Context, thread, turn, op string) (TurnOutcome, error)
	// Source is the Thread.source of the threads the backend creates and lists by: the one source a thread of this host can
	// have and be a call's thread.
	Source() string
	// CreationState says what is known about the creation request of call `callID` with send ordinal send that the run
	// process `owner` wrote.
	CreationState(owner, callID string, send int) Creation
}

// Owned is implemented by a backend that names the run process it lives in; the name is recorded on a creation intent so
// that a later process can tell whether the connection that carried the request can still be open.
type Owned interface {
	Owner() string
}

// ReconcileConfig is what a reconcile pass needs.
type ReconcileConfig struct {
	RunID string
	Host  ReconHost
	// Now is the clock (nil is time.Now).
	Now func() time.Time
	// Wait waits for d or until ctx ends; nil makes the pass wait for nothing: an outcome that needs a wait is then
	// reported as not established (the way `crw flow recover --absent` takes it).
	Wait func(ctx context.Context, d time.Duration) error
	// Grace, Skew, OpenWait and Poll override the defaults.
	Grace, Skew, OpenWait, Poll time.Duration
}

func (c ReconcileConfig) withDefaults() ReconcileConfig {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Grace <= 0 {
		c.Grace = DefaultCreationGrace
	}
	if c.Skew <= 0 {
		c.Skew = DefaultClockSkew
	}
	if c.OpenWait <= 0 {
		c.OpenWait = DefaultOpenWait
	}
	if c.Poll <= 0 {
		c.Poll = defaultReconPoll
	}
	return c
}

// SleepWait is the Wait of a real run.
func SleepWait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// Recovery is what reconcile decided for one call.
type Recovery struct {
	CallID string
	Node   string
	Action string
	// Kind is "thread" or "fork" for a creation.
	Kind     string
	ThreadID string
	Pair     Pair
	Cwd      string
	// Send is the send ordinal of the creation that is sent again.
	Send int
	// K, Op and Input are the turn operation of RecoverTurn and RecoverResendTurn; Input is the text the turn was
	// requested with (without the marker line).
	K     int
	Op    string
	Input string
	// TurnID is the turn of RecoverTurn; Running is set while it has not ended, Outcome is its end otherwise.
	TurnID  string
	Running bool
	Outcome *TurnOutcome
	// InterruptReason is the reason of an interrupt the engine requested for the turn before it was lost.
	InterruptReason string
	// Detail says why an outcome is unknown or pending.
	Detail string
}

// Reconciliation is the result of a reconcile pass.
type Reconciliation struct {
	// ByCall holds the recovery of every call the pass decided.
	ByCall map[string]*Recovery
}

// Unknown returns the calls the pass left with an outcome that cannot be established, in call order.
func (r *Reconciliation) Unknown() []*Recovery {
	var out []*Recovery
	for _, rec := range r.ByCall {
		if rec.Action == RecoverUnknown {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].CallID < out[k].CallID })
	return out
}

// rowStr, rowInt and rowMap read a row's data whether the journal kept it as written (Go values) or read it back from
// JSON (float64, []any, map[string]any).
func rowStr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func rowInt(m map[string]any, key string) int {
	switch n := m[key].(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func rowInt64(m map[string]any, key string) int64 {
	switch n := m[key].(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// prefixTurn is one turn of a fork's expected history.
type prefixTurn struct {
	TurnID string `json:"turn_id"`
	Op     string `json:"op"`
}

func prefixOf(m map[string]any) []prefixTurn {
	raw, err := json.Marshal(m["prefix"])
	if err != nil {
		return nil
	}
	var out []prefixTurn
	_ = json.Unmarshal(raw, &out)
	return out
}

// callState is what a call's rows say it was doing when it stopped.
type callState struct {
	id, node string
	rows     []Event
	// last is the last row that says what the call was doing: prepared, thread_requested, thread_started, fork_requested,
	// fork_started, turn_requested or turn_started.
	last string
	// create is the creation request the call is in the middle of; firstAt is when its first send was intended (ms).
	create  Event
	firstAt int64
	// thread, pair and cwd are the call's thread as the rows record it.
	thread string
	pair   Pair
	cwd    string
	// turnReq and turnStarted are the last turn_requested and turn_started rows.
	turnReq, turnStarted Event
	interrupt            string
	// adopted is set while the last row is a thread_started or fork_started that reconcile or an operator recorded
	// (recovered): the call has not sent a turn on that thread yet and goes on with it.
	adopted bool
	// terminal is the call's last terminal event, "" while it is open.
	terminal string
	// unknownDetail is the detail of a trailing outcome_unknown.
	unknownDetail string
	// rejected is set on the state of a held call whose creation or turn request the host answered with an error: the
	// answer proves the request finished (no grace is waited for), and a turn that carries its marker is not an adoption but
	// an outcome nobody can prove.
	rejected bool
}

func isTerminal(ev string) bool {
	_, ok := terminalEvents[ev]
	return ok
}

// stateOf reads one call's state from its rows (call rows of agent calls only).
func stateOf(id string, rows []Event) *callState {
	st := &callState{id: id, rows: rows}
	for _, ev := range rows {
		d := ev.Data
		switch {
		case ev.Event == EvPrepared:
			*st = callState{id: id, rows: rows, node: rowStr(d, "node"), last: EvPrepared, thread: rowStr(d, "thread_id"), cwd: rowStr(d, "thread_cwd")}
			st.pair = Pair{Model: rowStr(d, "configured_model"), Effort: rowStr(d, "configured_effort")}
		case ev.Event == EvThreadRequested || ev.Event == EvForkRequested:
			st.create, st.last, st.terminal = ev, ev.Event, ""
			if st.firstAt == 0 {
				st.firstAt = rowInt64(d, "at")
			}
		case ev.Event == EvThreadStarted || ev.Event == EvForkStarted:
			st.last, st.terminal, st.firstAt = ev.Event, "", 0
			st.adopted, _ = d["recovered"].(bool)
			st.thread, st.cwd = rowStr(d, "thread_id"), rowStr(d, "cwd")
			st.pair = Pair{Model: rowStr(d, "model"), Effort: rowStr(d, "effort")}
		case ev.Event == EvTurnRequested:
			st.turnReq, st.turnStarted, st.last, st.terminal, st.interrupt, st.adopted = ev, Event{}, ev.Event, "", "", false
		case ev.Event == EvTurnStarted:
			st.turnStarted, st.last, st.terminal, st.interrupt, st.adopted = ev, ev.Event, "", "", false
		case ev.Event == EvInterruptRequested:
			st.interrupt = rowStr(d, "reason")
		case ev.Event == EvRecoveryCleared:
			st.terminal = ""
		case isTerminal(ev.Event):
			st.terminal = ev.Event
			st.unknownDetail = ""
			if ev.Event == EvOutcomeUnknown {
				st.unknownDetail = rowStr(d, "detail")
			}
		}
	}
	return st
}

// intentState says whether the last row is an intent a send may have followed.
func (s *callState) intentState() bool {
	switch s.last {
	case EvThreadRequested, EvForkRequested, EvTurnRequested, EvTurnStarted:
		return true
	}
	return false
}

// open is a call that stopped between an intent and its outcome: its last row is not terminal.
func (s *callState) open() bool { return s.terminal == "" && s.intentState() }

// held is a call whose outcome was left unknown and has not been cleared since.
func (s *callState) held() bool {
	if s.terminal != EvOutcomeUnknown || !s.intentState() {
		return false
	}
	for i := len(s.rows) - 1; i >= 0; i-- {
		if s.rows[i].Event == EvOutcomeUnknown {
			return rowStr(s.rows[i].Data, "reason") == ReasonDispatchUnknown
		}
	}
	return false
}

// callStates groups the agent-call rows of a run by call id, in order of first appearance.
func callStates(j Journal, runID string) []*callState { return callStatesBefore(j, runID, 0) }

// callStatesBefore is callStates over the rows written before seq (all of them when seq is 0): the state a previous
// process left.
func callStatesBefore(j Journal, runID string, seq int) []*callState {
	rows := map[string][]Event{}
	var order []string
	for _, ev := range j.Events() {
		if ev.Kind != KindCall || ev.RunID != runID || ev.Op != "agent" || (seq > 0 && ev.Seq >= seq) {
			continue
		}
		if _, ok := rows[ev.CallID]; !ok {
			order = append(order, ev.CallID)
		}
		rows[ev.CallID] = append(rows[ev.CallID], ev)
	}
	out := make([]*callState, 0, len(order))
	for _, id := range order {
		out = append(out, stateOf(id, rows[id]))
	}
	return out
}

// RecoveryHolds returns the nodes that hold a call whose outcome is unknown (recovery_hold) and the calls that hold them.
func RecoveryHolds(j Journal, runID string) map[string][]string {
	out := map[string][]string{}
	for _, st := range callStates(j, runID) {
		if st.node != "" && st.held() {
			out[st.node] = append(out[st.node], st.id)
		}
	}
	return out
}

// plan is what the pass decided for one call, before any row or host change is made.
type plan struct {
	rec      *Recovery
	unknown  string
	pending  bool
	rows     []Event
	orphans  []orphan
	evidence map[string]any
}

type orphan struct {
	id     string
	reason string
}

type reconciler struct {
	cfg     ReconcileConfig
	j       Journal
	claimed map[string]bool
}

// Reconcile decides every call of the run whose last row is an intent, and every call whose outcome is unknown and has not
// been cleared, against the host. It appends what it established (an adopted thread, fork or turn, the orphans it
// archived, outcome_unknown with the escalation that names the node) and returns what each call does next. A call whose last
// row is terminal - a held call included - is not looked up at all.
func Reconcile(ctx context.Context, j Journal, cfg ReconcileConfig) (*Reconciliation, error) {
	if cfg.Host == nil {
		return nil, errors.New("engine: reconcile needs a host")
	}
	cfg = cfg.withDefaults()
	r := &reconciler{cfg: cfg, j: j, claimed: claimedThreads(j)}
	var todo []*callState
	res := &Reconciliation{ByCall: map[string]*Recovery{}}
	for _, st := range callStates(j, cfg.RunID) {
		switch {
		case st.open() || st.held():
			todo = append(todo, st)
		case st.terminal == "" && st.adopted && (st.last == EvThreadStarted || st.last == EvForkStarted):
			// A thread adopted by an earlier pass or by an operator, on which the call sent nothing yet: nothing to look up.
			rec := r.recoveryOf(st, RecoverThreadReady)
			rec.Kind = "thread"
			if st.last == EvForkStarted {
				rec.Kind = "fork"
			}
			res.ByCall[st.id] = rec
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, st := range todo {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := r.observe(ctx, st)
			if ctx.Err() != nil {
				// a pass that was cancelled decides nothing: an error it read is the cancellation, not the host
				return
			}
			rec := r.apply(ctx, st, p)
			mu.Lock()
			res.ByCall[st.id] = rec
			mu.Unlock()
		}()
	}
	wg.Wait()
	r.sweepOrphans(ctx)
	return res, ctx.Err()
}

// claimedThreads are the threads a journal already gives to a call (started, forked or archived as an orphan).
func claimedThreads(j Journal) map[string]bool {
	out := map[string]bool{}
	for _, ev := range j.Events() {
		switch {
		case ev.Kind == KindCall && (ev.Event == EvThreadStarted || ev.Event == EvForkStarted):
			out[rowStr(ev.Data, "thread_id")] = true
		case ev.Kind == KindFlow && ev.Event == EvOrphanThread:
			out[rowStr(ev.Data, "thread_id")] = true
		}
	}
	return out
}

func (r *reconciler) recoveryOf(st *callState, action string) *Recovery {
	return &Recovery{CallID: st.id, Node: st.node, Action: action, ThreadID: st.thread, Pair: st.pair, Cwd: st.cwd}
}

func (r *reconciler) unknown(st *callState, format string, args ...any) plan {
	return plan{unknown: fmt.Sprintf(format, args...)}
}

// observe looks the call up on the host and decides it; it changes nothing but the record of when a closed connection was
// first seen.
func (r *reconciler) observe(ctx context.Context, st *callState) plan {
	switch st.last {
	case EvThreadRequested, EvForkRequested:
		return r.observeCreate(ctx, st)
	case EvTurnRequested:
		return r.observeTurnRequested(ctx, st)
	case EvTurnStarted:
		return r.observeTurnStarted(ctx, st, nil)
	}
	return r.unknown(st, "the call stopped at %s, which is not an intent", st.last)
}

// scanResult is one complete look at the threads a creation may have made.
type scanResult struct {
	adoptable []HostThread
	foreign   []string
	err       error
	at        time.Time
}

// identity is the test of a listed thread against the fields the host returns.
func (r *reconciler) identity(th HostThread, cwd string, since time.Time, fork bool, source string) bool {
	if th.Cwd != cwd || th.ParentThreadID != "" {
		return false
	}
	if !since.IsZero() && th.CreatedAt.Before(since.Add(-r.cfg.Skew)) {
		return false
	}
	if fork {
		return th.ForkedFromID == source
	}
	return th.ForkedFromID == ""
}

// scan lists the threads at cwd, keeps those that fit the identity and reads the history of each: a thread/start has to
// hold no turn, a fork exactly the recorded prefix.
func (r *reconciler) scan(ctx context.Context, cwd string, since time.Time, fork bool, source string, prefix []prefixTurn) scanResult {
	list, err := r.cfg.Host.ListThreads(ctx, cwd)
	if err != nil {
		return scanResult{err: fmt.Errorf("the listing of %s is not complete: %w", cwd, err)}
	}
	res := scanResult{at: r.cfg.Now()}
	for _, th := range list {
		if r.claimed[th.ID] || !r.identity(th, cwd, since, fork, source) {
			continue
		}
		if th.Ephemeral {
			res.foreign = append(res.foreign, fmt.Sprintf("thread %s is ephemeral", th.ID))
			continue
		}
		turns, err := r.cfg.Host.Turns(ctx, th.ID)
		if err != nil {
			return scanResult{err: fmt.Errorf("the history of candidate %s cannot be read: %w", th.ID, err)}
		}
		if !fork {
			if len(turns) == 0 {
				res.adoptable = append(res.adoptable, th)
			} else {
				res.foreign = append(res.foreign, fmt.Sprintf("thread %s holds %d turn(s) but none of this call was sent", th.ID, len(turns)))
			}
			continue
		}
		why, unreadable := exactPrefix(turns, prefix)
		switch {
		case unreadable != nil:
			return scanResult{err: fmt.Errorf("candidate fork %s: %w", th.ID, unreadable)}
		case why != "":
			res.foreign = append(res.foreign, fmt.Sprintf("fork %s: %s", th.ID, why))
		default:
			res.adoptable = append(res.adoptable, th)
		}
	}
	sort.Slice(res.adoptable, func(i, k int) bool {
		a, b := res.adoptable[i], res.adoptable[k]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	return res
}

// exactPrefix says why a history is not the recorded prefix ("" when it is), or the error of a prefix turn that cannot be
// read in full.
func exactPrefix(turns []HostTurn, prefix []prefixTurn) (why string, unreadable error) {
	if len(turns) != len(prefix) {
		return fmt.Sprintf("holds %d turn(s), the recorded prefix has %d", len(turns), len(prefix)), nil
	}
	for i, t := range turns {
		if !t.Readable {
			return "", fmt.Errorf("prefix turn %d (%s) cannot be read in full: %v", i+1, t.ID, t.ReadError)
		}
	}
	for i, t := range turns {
		p := prefix[i]
		byID := p.TurnID != "" && t.ID == p.TurnID
		byMarker := p.Op != "" && t.Marker == p.Op
		switch {
		case !byID && !byMarker:
			return fmt.Sprintf("turn %d is %s (%s), the recorded prefix has %s (%s)", i+1, t.ID, t.Marker, p.TurnID, p.Op), nil
		case byID && p.Op != "" && t.Marker != "" && t.Marker != p.Op:
			return fmt.Sprintf("turn %d carries the operation %s, the recorded prefix has %s", i+1, t.Marker, p.Op), nil
		case t.Status == "inProgress":
			return fmt.Sprintf("prefix turn %d (%s) has not ended", i+1, t.ID), nil
		}
	}
	return "", nil
}

func (r *reconciler) closedSeen(callID string, send int) (time.Time, bool) {
	for _, ev := range r.j.Events() {
		if ev.Kind == KindFlow && ev.Event == EvCreationClosedSeen && ev.CallID == callID && rowInt(ev.Data, "send") == send {
			return time.UnixMilli(rowInt64(ev.Data, "at")), true
		}
	}
	return time.Time{}, false
}

// observeCreate decides a creation request that has no thread_started or fork_started after it.
func (r *reconciler) observeCreate(ctx context.Context, st *callState) plan {
	req := st.create
	fork := req.Event == EvForkRequested
	cwd := rowStr(req.Data, "cwd")
	send := max(1, rowInt(req.Data, "send"))
	owner := rowStr(req.Data, "owner")
	source := rowStr(req.Data, "thread_id")
	var prefix []prefixTurn
	if fork {
		prefix = prefixOf(req.Data)
	}
	var since time.Time
	if st.firstAt > 0 {
		since = time.UnixMilli(st.firstAt)
	}
	kind := "thread"
	if fork {
		kind = "fork"
	}
	scan := func() scanResult { return r.scan(ctx, cwd, since, fork, source, prefix) }
	res := scan()
	deadline := r.cfg.Now().Add(r.cfg.OpenWait)
	for {
		switch {
		case res.err != nil:
			return r.unknown(st, "%v", res.err)
		case len(res.foreign) > 0:
			return r.unknown(st, "a %s at %s is not this call's: %s", kind, cwd, strings.Join(res.foreign, "; "))
		case len(res.adoptable) > 0:
			return r.adopt(ctx, st, kind, cwd, res)
		}
		// No candidate: the request is absent only if it is proven finished. A request the host answered with an error is.
		if st.rejected {
			return r.resendCreate(st, kind, cwd, send, map[string]any{"proof": "rejected"})
		}
		switch r.cfg.Host.CreationState(owner, st.id, send) {
		case CreationUnsent:
			return r.resendCreate(st, kind, cwd, send, map[string]any{"proof": "unsent"})
		case CreationClosed:
			seenAt, seen := r.closedSeen(st.id, send)
			if !seen {
				seenAt = r.cfg.Now()
				r.j.Append(Event{RunID: r.cfg.RunID, Kind: KindFlow, CallID: st.id, Path: st.rows[0].Path, Position: st.rows[0].Position, Op: "agent", Label: st.rows[0].Label,
					Event: EvCreationClosedSeen, Data: map[string]any{"call_id": st.id, "send": send, "at": seenAt.UnixMilli()}})
			}
			due := seenAt.Add(r.cfg.Grace)
			if res.at.Before(due) {
				if r.cfg.Wait == nil {
					return r.unknown(st, "the connection of the %s request closed at %s; a second complete listing is due at %s", kind, seenAt.UTC().Format(time.RFC3339), due.UTC().Format(time.RFC3339))
				}
				if err := r.cfg.Wait(ctx, due.Sub(r.cfg.Now())); err != nil {
					return plan{pending: true} // the pass was cancelled: the grace has not passed, nothing is decided
				}
				res = scan()
				continue
			}
			return r.resendCreate(st, kind, cwd, send, map[string]any{"proof": "connection_closed", "closed_seen_at": seenAt.UnixMilli(), "listing_at": res.at.UnixMilli()})
		case CreationOpen:
			if r.cfg.Wait == nil || !r.cfg.Now().Before(deadline) {
				p := plan{pending: true}
				return p
			}
			if err := r.cfg.Wait(ctx, r.cfg.Poll); err != nil {
				return plan{pending: true}
			}
			res = scan()
			continue
		}
		return r.unknown(st, "no candidate for the %s request at %s and nothing proves the request finished", kind, cwd)
	}
}

func (r *reconciler) resendCreate(st *callState, kind, cwd string, send int, evidence map[string]any) plan {
	rec := r.recoveryOf(st, RecoverResendCreate)
	rec.Kind, rec.Cwd, rec.Send = kind, cwd, send+1
	evidence["send"], evidence["kind"] = send, kind
	return plan{rec: rec, evidence: evidence, rows: []Event{r.flowRow(st, EvCreationResendable, evidence)}}
}

func (r *reconciler) flowRow(st *callState, event string, data map[string]any) Event {
	data["call_id"] = st.id
	first := st.rows[0]
	return Event{RunID: r.cfg.RunID, Kind: KindFlow, CallID: st.id, Path: first.Path, Position: first.Position, Op: "agent", Label: first.Label, Event: event, Data: data}
}

func (r *reconciler) callRow(st *callState, event string, data map[string]any) Event {
	first := st.rows[0]
	return Event{RunID: r.cfg.RunID, Kind: KindCall, CallID: st.id, Path: first.Path, Position: first.Position, Op: "agent", Label: first.Label, Event: event, Data: data}
}

// adopt takes the earliest candidate (then the lowest id) after reading its pair and cwd, and archives the others.
func (r *reconciler) adopt(ctx context.Context, st *callState, kind, cwd string, res scanResult) plan {
	chosen := res.adoptable[0]
	read, err := r.cfg.Host.ReadThread(ctx, chosen.ID)
	if err != nil {
		return r.unknown(st, "the candidate %s cannot be read: %v", chosen.ID, err)
	}
	if read.Cwd != cwd {
		return r.unknown(st, "the candidate %s reports the cwd %s, not %s", chosen.ID, read.Cwd, cwd)
	}
	ev := EvThreadStarted
	if kind == "fork" {
		ev = EvForkStarted
	}
	evidence := map[string]any{"listing": "complete", "candidates": len(res.adoptable), "created_at": chosen.CreatedAt.Unix()}
	rec := r.recoveryOf(st, RecoverThreadReady)
	rec.Kind, rec.ThreadID, rec.Pair, rec.Cwd = kind, chosen.ID, read.Pair, read.Cwd
	p := plan{rec: rec, evidence: evidence}
	p.rows = append(p.rows, r.callRow(st, ev, map[string]any{"thread_id": chosen.ID, "model": read.Pair.Model, "effort": read.Pair.Effort, "cwd": read.Cwd, "recovered": true, "evidence": evidence}))
	for _, other := range res.adoptable[1:] {
		p.orphans = append(p.orphans, orphan{id: other.ID, reason: "duplicate_creation"})
	}
	return p
}

// observeTurnRequested decides a turn_requested(k) that has no turn_started(k): the turn carrying the marker is adopted;
// only when every examined turn is read in full, none carries the marker and the thread is idle is the turn sent again.
func (r *reconciler) observeTurnRequested(ctx context.Context, st *callState) plan {
	if st.thread == "" {
		return r.unknown(st, "turn_requested without a thread")
	}
	op := rowStr(st.turnReq.Data, "op")
	turns, err := r.cfg.Host.Turns(ctx, st.thread)
	if err != nil {
		return r.unknown(st, "the history of thread %s is not complete: %v", st.thread, err)
	}
	for _, t := range turns {
		if t.Readable && t.Marker == op && st.rejected {
			return r.unknown(st, "turn %s of thread %s carries %s, the marker of a request the host refused", t.ID, st.thread, op)
		}
		if t.Readable && t.Marker == op {
			adopted := r.callRow(st, EvTurnStarted, map[string]any{"k": rowInt(st.turnReq.Data, "k"), "op": op, "turn_id": t.ID, "recovered": true,
				"evidence": map[string]any{"listing": "complete", "marker": op, "status": t.Status}})
			return r.observeTurnStarted(ctx, st, &turnAdoption{turn: t, row: adopted})
		}
	}
	for _, t := range turns {
		if !t.Readable {
			return r.unknown(st, "turn %s of thread %s exists but its items cannot be read in full (%v): it is not an absence", t.ID, st.thread, t.ReadError)
		}
	}
	th, err := r.cfg.Host.ReadThread(ctx, st.thread)
	if err != nil {
		return r.unknown(st, "thread %s cannot be read: %v", st.thread, err)
	}
	if th.Status != "idle" && th.Status != "notLoaded" {
		return r.unknown(st, "no turn carries %s and thread %s is %s", op, st.thread, th.Status)
	}
	rec := r.recoveryOf(st, RecoverResendTurn)
	rec.K, rec.Op, rec.Input = rowInt(st.turnReq.Data, "k"), op, rowStr(st.turnReq.Data, "input")
	evidence := map[string]any{"listing": "complete", "turns": len(turns), "thread_status": th.Status, "marker": op}
	return plan{rec: rec, evidence: evidence, rows: []Event{r.flowRow(st, EvCreationResendable, map[string]any{"proof": "turn_absent", "op": op, "evidence": evidence})}}
}

// turnAdoption is a turn found by its marker that the turn_started row has not recorded yet.
type turnAdoption struct {
	turn HostTurn
	row  Event
}

// observeTurnStarted decides a call whose turn the host named: a running turn is attached to again, an ended turn is
// harvested, a turn the host does not list or cannot be read is an unknown outcome.
func (r *reconciler) observeTurnStarted(ctx context.Context, st *callState, adopted *turnAdoption) plan {
	op, turnID := rowStr(st.turnReq.Data, "op"), ""
	k := rowInt(st.turnReq.Data, "k")
	if adopted != nil {
		turnID = adopted.turn.ID
	} else {
		turnID = rowStr(st.turnStarted.Data, "turn_id")
		if o := rowStr(st.turnStarted.Data, "op"); o != "" {
			op = o
		}
		k = rowInt(st.turnStarted.Data, "k")
	}
	var rows []Event
	if adopted != nil {
		rows = append(rows, adopted.row)
	}
	turns, err := r.cfg.Host.Turns(ctx, st.thread)
	if err != nil {
		return r.unknown(st, "the history of thread %s is not complete: %v", st.thread, err)
	}
	var found *HostTurn
	for i := range turns {
		if turns[i].ID == turnID {
			found = &turns[i]
		}
	}
	if found == nil {
		return r.unknown(st, "the host lists no turn %s on thread %s", turnID, st.thread)
	}
	rec := r.recoveryOf(st, RecoverTurn)
	rec.K, rec.Op, rec.TurnID, rec.Input, rec.InterruptReason = k, op, turnID, rowStr(st.turnReq.Data, "input"), st.interrupt
	if found.Status == "inProgress" || found.Status == "" {
		rec.Running = true
		return plan{rec: rec, rows: rows}
	}
	out, running, err := r.cfg.Host.HarvestTurn(ctx, st.thread, turnID, op)
	switch {
	case err != nil:
		return r.unknown(st, "turn %s of thread %s: %v", turnID, st.thread, err)
	case running:
		rec.Running = true
	default:
		rec.Outcome = &out
	}
	return plan{rec: rec, rows: rows}
}

// apply makes the plan real: the orphans are archived and recorded, the rows are appended in order, and an unknown outcome
// is appended with the escalation that names the node.
func (r *reconciler) apply(ctx context.Context, st *callState, p plan) *Recovery {
	if st.held() && p.unknown == "" && !p.pending {
		// A later pass established what an earlier one could not: the call is open again, whatever rows follow, so the hold
		// ends and a resumed call goes on from this outcome instead of replaying the unknown one. (Recover opens the call in
		// memory and writes this row itself, so it never reaches here with a held call.)
		r.j.Append(r.callRow(st, EvRecoveryCleared, map[string]any{"mode": "reconcile", "action": p.rec.Action, "evidence": p.evidence}))
	}
	for _, o := range p.orphans {
		data := map[string]any{"thread_id": o.id, "reason": o.reason}
		if err := r.cfg.Host.ArchiveThread(ctx, o.id); err != nil {
			data["archived"], data["archive_error"] = false, err.Error()
		} else {
			data["archived"] = true
		}
		r.j.Append(r.flowRow(st, EvOrphanThread, data))
	}
	for _, row := range p.rows {
		r.j.Append(row)
	}
	switch {
	case p.unknown != "":
		if !(st.held() && st.unknownDetail == p.unknown) {
			r.j.Append(r.callRow(st, EvOutcomeUnknown, map[string]any{"reason": ReasonDispatchUnknown, "detail": p.unknown, "recovered": true}))
			r.j.Append(r.flowRow(st, EvEscalation, map[string]any{"reason": ReasonDispatchUnknown, "node": st.node, "detail": p.unknown, "last_event": st.last}))
		}
		rec := r.recoveryOf(st, RecoverUnknown)
		rec.Detail = p.unknown
		return rec
	case p.pending:
		rec := r.recoveryOf(st, RecoverPending)
		rec.Detail = "the connection that carried the creation request is alive: its answer may still come"
		return rec
	}
	return p.rec
}

// sweepOrphans archives the threads a creation that was sent again may have made after all: a call that was resent has
// one thread of its own, and a thread of the first request that shows up later at the call's cwd, still without a turn of
// the call, is an orphan. A thread there that holds turns is not archived; it is recorded once as foreign.
func (r *reconciler) sweepOrphans(ctx context.Context) {
	seenForeign := map[string]bool{}
	for _, ev := range r.j.Events() {
		if ev.Kind == KindFlow && ev.Event == EvForeignThread {
			seenForeign[rowStr(ev.Data, "thread_id")] = true
		}
	}
	for _, st := range callStates(r.j, r.cfg.RunID) {
		if st.thread == "" {
			continue
		}
		var req Event
		resent := false
		for _, ev := range st.rows {
			if (ev.Event == EvThreadRequested || ev.Event == EvForkRequested) && rowInt(ev.Data, "send") > 1 {
				resent = true
			}
			if ev.Event == EvThreadRequested || ev.Event == EvForkRequested {
				req = ev
			}
		}
		if !resent {
			continue
		}
		fork := req.Event == EvForkRequested
		var since time.Time
		var firstAt int64
		for _, ev := range st.rows {
			if (ev.Event == EvThreadRequested || ev.Event == EvForkRequested) && (firstAt == 0 || rowInt64(ev.Data, "at") < firstAt) {
				firstAt = rowInt64(ev.Data, "at")
			}
		}
		if firstAt > 0 {
			since = time.UnixMilli(firstAt)
		}
		cwd := rowStr(req.Data, "cwd")
		list, err := r.cfg.Host.ListThreads(ctx, cwd)
		if err != nil {
			continue
		}
		for _, th := range list {
			if th.ID == st.thread || r.claimed[th.ID] || !r.identity(th, cwd, since, fork, rowStr(req.Data, "thread_id")) {
				continue
			}
			turns, err := r.cfg.Host.Turns(ctx, th.ID)
			if err != nil {
				continue
			}
			expect := 0
			var prefix []prefixTurn
			if fork {
				prefix = prefixOf(req.Data)
				expect = len(prefix)
			}
			isOrphan := len(turns) == expect
			if isOrphan && fork {
				why, unreadable := exactPrefix(turns, prefix)
				isOrphan = why == "" && unreadable == nil
			}
			if !isOrphan {
				if !seenForeign[th.ID] {
					seenForeign[th.ID] = true
					r.j.Append(r.flowRow(st, EvForeignThread, map[string]any{"thread_id": th.ID, "turns": len(turns), "cwd": cwd}))
				}
				continue
			}
			data := map[string]any{"thread_id": th.ID, "reason": "late_first_request"}
			if err := r.cfg.Host.ArchiveThread(ctx, th.ID); err != nil {
				data["archived"], data["archive_error"] = false, err.Error()
			} else {
				data["archived"] = true
			}
			r.claimed[th.ID] = true
			r.j.Append(r.flowRow(st, EvOrphanThread, data))
		}
	}
}
