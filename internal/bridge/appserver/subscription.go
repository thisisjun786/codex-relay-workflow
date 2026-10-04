package appserver

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const releaseRetryFloor = 5 * time.Second
const releaseRetryCeiling = 5 * time.Minute

type subscriptionRoot struct {
	gate             chan struct{}
	refs             int // Watches plus admissions waiting for the gate.
	connection       *websocket.Conn
	watches          []*TurnWatch
	retainedOn       *websocket.Conn
	due              time.Time
	delay            time.Duration
	releasing        bool
	releasePending   bool
	cleanupErrors    int
	cleanupAbandoned bool
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
	if r.refs == 0 && r.retainedOn == nil && !r.releasing && !r.releasePending && m.roots[thread] == r {
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
	if r.retainedOn != nil || r.refs != len(r.watches) || time.Now().Before(r.due) {
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
