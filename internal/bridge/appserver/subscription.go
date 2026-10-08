package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const releaseRetryFloor = 5 * time.Second
const releaseRetryCeiling = 5 * time.Minute

type subscriptionRoot struct {
	gate       chan struct{}
	refs       int // Watches plus admissions waiting for the gate.
	connection *websocket.Conn
	watches    []*TurnWatch
	retainedOn *websocket.Conn
	// busyHeld is the relay delivery path's own hold on this root (CRW-904): the subscription is kept
	// while a delivery to this thread waits out a busy backoff, so the App Server reports that
	// recipient's status changes to this connection. It is deliberately not retainedOn: that field is
	// the bridge's retention of a never-run root, and releasing one must never drop the other. It
	// holds no gate and owns no watch, so the delivery's own WatchTurn is admitted while it stands.
	busyHeld bool
	busyOn   *websocket.Conn
	// busyUnconfirmed is a resume this relay sent but whose answer never arrived: the host may have
	// applied it, so the connection may carry a real subscription, but nobody knows. It is kept exactly
	// as busyHeld is kept - ready and prune treat it as not releasable, and the release worker
	// unsubscribes it once ReleaseThread drops it - so an applied resume is never left unowned. It is
	// deliberately not busyHeld: receivesOn and ThreadHeld must not read it as an established
	// subscription, or the retry the caller needs would be suppressed and the daemon would believe it
	// holds a subscription the recipient's reports never reach (CRW-904 correction, d1 and d2).
	busyUnconfirmed   bool
	busyUnconfirmedOn *websocket.Conn
	due               time.Time
	delay             time.Duration
	releasing         bool
	releasePending    bool
	cleanupErrors     int
	cleanupAbandoned  bool
}

// TurnWatch holds the root's mutation gate until Finish. Its context fences all
// operation calls to this socket; neither dispatch nor release reconnects.
type TurnWatch struct {
	manager                                 *subscriptionManager
	root                                    *subscriptionRoot
	thread                                  string
	connection                              *websocket.Conn
	turn                                    string
	seen                                    map[string]bool
	transmitted, refused, finished, retired bool
	admission                               bool
}
type watchContextKey struct{}

func (w *TurnWatch) Context(ctx context.Context) context.Context {
	return context.WithValue(ctx, watchContextKey{}, w)
}

// Transmitted reports whether this watch's turn/start frame was written to the connection. The
// flag is set immediately before the write (client.request), so a call the client withheld before
// writing -- a connection that already ended, or a retired watch -- reports false and no turn can
// have started. Read under the subscription lock the writer holds, so a caller on another
// goroutine reads the write's own verdict rather than a torn one.
func (w *TurnWatch) Transmitted() bool {
	if w.manager == nil {
		return false
	}
	w.manager.mu.Lock()
	defer w.manager.mu.Unlock()
	return w.transmitted
}

type subscriptionManager struct {
	client            *Client
	mu                sync.Mutex
	roots             map[string]*subscriptionRoot
	ctx               context.Context
	cancel            context.CancelFunc
	wake, done        chan struct{}
	started, stopping bool
	retryFloor        time.Duration
	cleanup           func(context.Context, string) (bool, error)
	cleanupActive     bool
	admitted          int
	changed           chan struct{}
	cleanupBound      time.Duration
}

func newSubscriptions(c *Client) *subscriptionManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &subscriptionManager{client: c, roots: map[string]*subscriptionRoot{}, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{}), retryFloor: releaseRetryFloor, changed: make(chan struct{}), cleanupBound: descendantCleanupBound}
}
func (m *subscriptionManager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
func (m *subscriptionManager) root(thread string) *subscriptionRoot {
	r := m.roots[thread]
	if r == nil {
		r = &subscriptionRoot{gate: make(chan struct{}, 1)}
		r.gate <- struct{}{}
		m.roots[thread] = r
	}
	return r
}
func (m *subscriptionManager) start() {
	if !m.started {
		m.started = true
		go m.run()
	}
}

// WatchTurn admits on an already established socket. The preceding read/start
// establishes it; admission never silently moves an operation to another socket.
func (c *Client) WatchTurn(ctx context.Context, thread string) (*TurnWatch, error) {
	return c.watchTurn(ctx, thread, false)
}

// WatchCreatedTurn also requires the socket that acknowledged this creation.
func (c *Client) WatchCreatedTurn(ctx context.Context, thread string) (*TurnWatch, error) {
	return c.watchTurn(ctx, thread, true)
}

// HoldThread subscribes this client's connection to thread with the relay's own thread/resume and
// keeps the subscription without an admitted watch, so the App Server reports that thread's status
// changes to this connection while a delivery waits out the recipient's busy backoff (CRW-904).
//
// Only thread/start and thread/resume subscribe a connection, and an admitted watch holds the root's
// mutation gate until Finish, which would block the delivery's own send. The hold therefore takes no
// gate and no TurnWatch: it records the root as busy-held, which the release worker treats as not
// releasable, and the ordinary release path (Finish and its terminal) leaves it standing. The resume
// carries no overrides and starts no turn; an already loaded thread only reports its state, so a
// busy recipient is never interrupted. A resume the host refuses returns that error and leaves the
// doubling backoff as the only trigger, which is the behavior when the relay holds nothing.
func (c *Client) HoldThread(ctx context.Context, thread string) error {
	m := c.subscriptions
	if m == nil {
		return &TransportError{Reason: "this client has no subscription manager"}
	}
	if err := c.connect(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return &TransportError{Reason: "subscription connection ended before the hold"}
	}
	// The root's own gate serializes this against the release worker, which takes the same gate before
	// it rechecks readiness and sends thread/unsubscribe. Taking it here means the two cannot interleave:
	// either the worker unsubscribes first and this resume reopens the subscription after it, or this
	// hold is recorded first and the worker's recheck sees it and leaves the subscription alone. The
	// resume is one bounded call, so the gate is held no longer than a release holds it.
	//
	// The root is pinned for the whole hold, as an admission pins the root it waits on: a reference is
	// taken before the gate is awaited and dropped when the hold is done. Without it a cancelled
	// admission could prune this root while the resume was outstanding (refs falls to zero, and the
	// hold has not set busyHeld yet), a later admission would build a replacement root with its own
	// gate, and that root's release worker could unsubscribe the thread before this hold's successful
	// reply arrived. The hold would then be recorded on a socket that receives nothing, and the busy
	// head would fall back to its timer (CRW-904 correction, d1).
	m.mu.Lock()
	r := m.root(thread)
	r.refs++
	m.mu.Unlock()
	defer m.unreserve(thread, r)
	select {
	case <-r.gate:
	case <-ctx.Done():
		return ctx.Err()
	case <-m.ctx.Done():
		return m.ctx.Err()
	}
	defer func() { r.gate <- struct{}{} }()
	c.mu.Lock()
	m.mu.Lock()
	if m.stopping || c.conn != conn {
		m.mu.Unlock()
		c.mu.Unlock()
		return &TransportError{Reason: "subscription connection ended before the hold"}
	}
	// A resume this connection does not need: it already receives the thread's reports, from this
	// relay's own hold, the bridge's retention of a never-run root, or a live watch. The hold is
	// recorded below either way, because a watch or the retention can release its subscription while
	// the backlog waits, and this relay's own hold is what keeps the idle edge through that (CRW-904
	// d4).
	resume := !m.receivesOn(thread, conn)
	m.mu.Unlock()
	c.mu.Unlock()
	if resume {
		if _, err := c.request(ctx, conn, "thread/resume", map[string]any{"threadId": thread, "excludeTurns": true}); err != nil {
			// The reply is missing, but the resume may already have landed: a refusal the server
			// answered is an RPCError and subscribes nothing, while a timeout or a lost socket after
			// the write can leave a real subscription on this connection with nobody owning it. The
			// hold is recorded in that case so the ordinary release path unsubscribes it, and the error
			// is still returned, so the caller does not treat the subscription as established
			// (CRW-904 correction, d2). Without this the subscription survives until the socket dies.
			var refused *RPCError
			if !errors.As(err, &refused) {
				c.mu.Lock()
				m.mu.Lock()
				if !m.stopping && c.conn == conn {
					current := m.root(thread)
					current.connection = conn
					// Unconfirmed, not held: the subscription is kept alive so the ordinary release
					// worker can unsubscribe it once the backlog empties, while receivesOn and
					// ThreadHeld keep answering "not established", so the caller retries and the
					// daemon never believes it holds a subscription the reports do not reach.
					current.busyUnconfirmed = true
					current.busyUnconfirmedOn = conn
					m.start()
				}
				m.mu.Unlock()
				c.mu.Unlock()
				m.signal()
			}
			return err
		}
	}
	c.mu.Lock()
	m.mu.Lock()
	if m.stopping || c.conn != conn {
		m.mu.Unlock()
		c.mu.Unlock()
		return &TransportError{Reason: "subscription connection ended before the hold"}
	}
	// The root for this thread as it stands now: a prune can have dropped the one whose gate is held
	// while the resume was in flight, and the hold belongs on the root the manager currently keeps.
	current := m.root(thread)
	current.connection = conn
	current.busyHeld = true
	current.busyOn = conn
	// A resume that has now been acknowledged supersedes an earlier unconfirmed one on this socket:
	// the subscription is established, and the release path owns it through busyHeld alone.
	current.busyUnconfirmed = false
	current.busyUnconfirmedOn = nil
	m.start()
	m.mu.Unlock()
	c.mu.Unlock()
	m.signal()
	return nil
}

// ReleaseThread drops the hold HoldThread took on thread, so the release worker unsubscribes it on
// the original socket once nothing else keeps the root: the backlog emptied, or the delivery was
// delivered. It leaves the bridge's own retention of a never-run root and any live watch alone.
func (c *Client) ReleaseThread(thread string) {
	m := c.subscriptions
	if m == nil {
		return
	}
	m.mu.Lock()
	if r := m.roots[thread]; r != nil {
		r.busyHeld = false
		r.busyOn = nil
		r.busyUnconfirmed = false
		r.busyUnconfirmedOn = nil
	}
	m.mu.Unlock()
	m.signal()
}

// ThreadSubscribed reports whether this client currently holds a subscription on thread: a hold the
// relay took, the bridge's retention of a never-run root, or a live watch. It answers "does a busy
// deferral find a subscription", which decides whether the relay has to open one.
func (c *Client) ThreadSubscribed(thread string) bool {
	m := c.subscriptions
	if m == nil {
		return false
	}
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.subscribedOn(thread, conn)
}

// ThreadHeld reports whether this relay's own hold stands on thread: the subscription HoldThread took
// and ReleaseThread drops, which the release worker treats as not releasable. It is not
// ThreadSubscribed: a watch or the bridge's retention may be subscribed while the relay's own hold is
// gone, and the backlog has to take its own reference so the idle edge survives that owner releasing
// mid-backlog (CRW-904 d4).
func (c *Client) ThreadHeld(thread string) bool {
	m := c.subscriptions
	if m == nil {
		return false
	}
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.roots[thread]
	return r != nil && r.busyHeld && r.busyOn != nil && r.busyOn == conn
}

// subscribedOn reports whether a busy deferral finds a subscription on thread at all: this relay's own
// hold, the bridge's retention of a never-run root, or any live watch, whichever connection carries it.
// The caller holds m.mu.
func (m *subscriptionManager) subscribedOn(thread string, conn *websocket.Conn) bool {
	r := m.roots[thread]
	if r == nil {
		return false
	}
	if r.busyHeld && r.busyOn != nil && r.busyOn == conn {
		return true
	}
	if r.retainedOn != nil {
		return true
	}
	for _, w := range r.watches {
		if w.connection != nil && !w.retired {
			return true
		}
	}
	return false
}

// receivesOn reports whether conn is already known to receive thread's reports: this relay's own hold
// on it, or the bridge's retention of a never-run root on it. Those two are the subscriptions this
// client made itself and records; the resume HoldThread would send is exactly what creates them, so a
// subscription carried by another socket is not one and the resume is still owed. The caller holds
// m.mu.
//
// A live watch is deliberately not evidence. WatchTurn only admits a watch; it sends no subscribing
// RPC of its own, and this client records no acknowledgement of the resume the delivery path sends
// beside it (CRW-904 d2). A watch whose resume was refused or cancelled leaves the connection
// unsubscribed while the watch is still live, so reading it as a subscription would let HoldThread
// skip the override-free resume and record a hold on a socket the recipient's reports never reach.
// The cost of the other error is one redundant resume on a thread that is already subscribed, which
// starts no turn and changes nothing.
func (m *subscriptionManager) receivesOn(thread string, conn *websocket.Conn) bool {
	r := m.roots[thread]
	if r == nil {
		return false
	}
	return (r.busyHeld && r.busyOn == conn) || r.retainedOn == conn
}
func (c *Client) watchTurn(ctx context.Context, thread string, created bool) (*TurnWatch, error) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	m := c.subscriptions
	m.mu.Lock()
	r := m.roots[thread]
	if m.stopping || conn == nil || (created && (r == nil || r.retainedOn != conn)) {
		m.mu.Unlock()
		return nil, &TransportError{Reason: "subscription connection ended before admission"}
	}
	r = m.root(thread)
	r.refs++
	m.mu.Unlock()
	select {
	case <-r.gate:
	case <-ctx.Done():
		m.unreserve(thread, r)
		return nil, ctx.Err()
	case <-m.ctx.Done():
		m.unreserve(thread, r)
		return nil, m.ctx.Err()
	}
	if err := m.admit(ctx); err != nil {
		r.gate <- struct{}{}
		m.unreserve(thread, r)
		return nil, err
	}
	c.mu.Lock()
	m.mu.Lock()
	err := ctx.Err()
	if err == nil && (m.stopping || c.conn != conn) {
		err = &TransportError{Reason: "subscription connection ended before admission"}
	}
	if err != nil {
		m.endAdmission()
		m.mu.Unlock()
		c.mu.Unlock()
		r.gate <- struct{}{}
		m.unreserve(thread, r)
		return nil, err
	}
	w := &TurnWatch{manager: m, root: r, thread: thread, connection: conn, seen: map[string]bool{}, admission: true}
	r.connection = conn
	r.watches = append(r.watches, w)
	m.start()
	m.mu.Unlock()
	c.mu.Unlock()
	return w, nil
}
func (m *subscriptionManager) unreserve(thread string, r *subscriptionRoot) {
	m.mu.Lock()
	r.refs--
	m.prune(thread, r)
	m.mu.Unlock()
	m.signal()
}
func (m *subscriptionManager) prune(thread string, r *subscriptionRoot) {
	if r.refs == 0 && r.retainedOn == nil && !r.busyHeld && !r.busyUnconfirmed && !r.releasing && !r.releasePending && m.roots[thread] == r {
		delete(m.roots, thread)
	}
}

// Finish follows the receipt and final observation. An ACK observed by the
// reader wins over a missing receipt turn id after cancellation/checkpoint loss.
func (w *TurnWatch) Finish(turn string, retain bool) {
	m := w.manager
	m.mu.Lock()
	if turn != "" {
		w.turn = turn
	}
	w.finished = true
	if w.admission {
		w.admission = false
		m.endAdmission()
	}
	if !w.retired && retain && w.turn == "" && !w.transmitted && len(w.seen) == 0 {
		w.root.retainedOn = w.connection
	}
	m.mu.Unlock()
	w.root.gate <- struct{}{}
	m.signal()
}

// reply runs on the reader before delivering an ACK, so cancellation cannot hide
// acknowledged creation or turn ownership from the subscription manager.
func (m *subscriptionManager) reply(p pending, result, rpcError json.RawMessage) {
	if p.method != "thread/start" && p.method != "turn/start" {
		return
	}
	var ids struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(result, &ids)
	if p.method == "thread/start" && rpcError == nil && ids.Thread.ID != "" {
		m.client.mu.Lock()
		m.mu.Lock()
		if !m.stopping && m.client.conn == p.conn {
			r := m.root(ids.Thread.ID)
			r.connection = p.conn
			r.retainedOn = p.conn
			m.start()
		}
		m.mu.Unlock()
		m.client.mu.Unlock()
	}
	if p.method == "turn/start" && p.watch != nil {
		m.mu.Lock()
		if !p.watch.retired {
			p.watch.refused = rpcError != nil
			if ids.Turn.ID != "" {
				p.watch.turn = ids.Turn.ID
				if rpcError == nil {
					p.watch.root.cleanupErrors = 0
					p.watch.root.cleanupAbandoned = false
				}
			}
		}
		m.mu.Unlock()
	}
	m.signal()
}
func (m *subscriptionManager) terminal(ws *websocket.Conn, raw json.RawMessage) {
	var n struct {
		ThreadID string `json:"threadId"`
		Turn     struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(raw, &n) != nil || n.ThreadID == "" || n.Turn.ID == "" {
		return
	}
	m.mu.Lock()
	if r := m.roots[n.ThreadID]; r != nil {
		if r.retainedOn == ws {
			r.retainedOn = nil
			r.releasePending = true
		}
		known := false
		for _, w := range r.watches {
			if w.connection == ws && w.turn == n.Turn.ID {
				known = true
				w.seen[n.Turn.ID] = true
			}
		}
		for _, w := range r.watches {
			if w.connection == ws && w.turn == "" && w.transmitted && !w.refused && !known {
				w.seen[n.Turn.ID] = true
			}
		}
		r.due = time.Time{}
	}
	m.mu.Unlock()
	m.signal()
}
func (m *subscriptionManager) lost(ws *websocket.Conn) {
	m.mu.Lock()
	for thread, r := range m.roots {
		if r.connection == ws {
			r.releasePending = false
		}
		if r.retainedOn == ws {
			r.retainedOn = nil
		}
		// The hold lives on the connection: when that socket is lost, the subscription is gone with it.
		if r.busyOn == ws {
			r.busyHeld = false
			r.busyOn = nil
		}
		// An unconfirmed resume lives on the connection in the same way: the socket that may have
		// subscribed it is gone, so nothing is owed an unsubscribe for it.
		if r.busyUnconfirmedOn == ws {
			r.busyUnconfirmed = false
			r.busyUnconfirmedOn = nil
		}
		kept := r.watches[:0]
		for _, w := range r.watches {
			if w.connection == ws {
				w.retired = true
				r.refs--
			} else {
				kept = append(kept, w)
			}
		}
		r.watches = kept
		m.prune(thread, r)
	}
	m.mu.Unlock()
	m.signal()
}
func (m *subscriptionManager) ready(r *subscriptionRoot) bool {
	if r.retainedOn != nil || r.busyHeld || r.busyUnconfirmed || r.refs != len(r.watches) || time.Now().Before(r.due) {
		return false
	}
	for _, w := range r.watches {
		if !w.finished || (w.turn != "" && !w.seen[w.turn]) || (w.turn == "" && w.transmitted && !w.refused && len(w.seen) == 0) {
			return false
		}
	}
	return true
}
func (m *subscriptionManager) next() (string, *subscriptionRoot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for thread, r := range m.roots {
		if !r.releasing && m.ready(r) {
			r.releasing = true
			r.releasePending = true
			return thread, r
		}
	}
	return "", nil
}
func (m *subscriptionManager) run() {
	defer close(m.done)
	tick := time.NewTicker(m.retryFloor)
	defer tick.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.wake:
		case <-tick.C:
		}
		for thread, r := m.next(); r != nil; thread, r = m.next() {
			m.release(thread, r)
			if m.ctx.Err() != nil {
				return
			}
		}
	}
}
func (m *subscriptionManager) release(thread string, r *subscriptionRoot) {
	defer func() {
		m.mu.Lock()
		r.releasing = false
		m.prune(thread, r)
		m.mu.Unlock()
	}()
	select {
	case <-r.gate:
	case <-m.ctx.Done():
		return
	}
	defer func() { r.gate <- struct{}{} }()
	m.mu.Lock()
	ready := m.ready(r)
	conn := r.connection
	m.mu.Unlock()
	if !ready {
		return
	}
	m.client.mu.Lock()
	live := conn != nil && m.client.conn == conn
	m.client.mu.Unlock()
	if !live {
		m.lost(conn)
		return
	}
	if !m.cleanupRoot(thread, r, conn) {
		return
	}
	_, err := m.client.request(m.ctx, conn, "thread/unsubscribe", map[string]any{"threadId": thread})
	if err != nil {
		if m.ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		if m.roots[thread] == r {
			if r.delay == 0 {
				log.Printf("thread subscription release: %s: %v", thread, err)
			}
			r.delay = min(releaseRetryCeiling, max(m.retryFloor, r.delay*2))
			r.due = time.Now().Add(r.delay)
		}
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	for _, w := range r.watches {
		w.retired = true
		r.refs--
	}
	r.watches = nil
	r.releasePending = false
	m.prune(thread, r)
	m.mu.Unlock()
}
func (m *subscriptionManager) close() {
	m.mu.Lock()
	m.stopping = true
	m.cancel()
	started := m.started
	m.mu.Unlock()
	if started {
		<-m.done
	}
}
