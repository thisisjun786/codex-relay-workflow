package appserver

import (
	"context"
	"log"
	"time"

	"github.com/coder/websocket"
)

const descendantCleanupBound = 30 * time.Second
const descendantErrorLimit = 8

type cleanupConnectionKey struct{}

// ConfigureSubscriptions composes descendant release before client operations.
// The callback runs under the root gate, on its original socket, after Finish
// and terminal observations. Bridge libraries know no relay or archive policy.
func (c *Client) ConfigureSubscriptions(cleanup func(context.Context, string) (bool, error)) {
	c.subscriptions.mu.Lock()
	c.subscriptions.cleanup = cleanup
	c.subscriptions.mu.Unlock()
}

func (m *subscriptionManager) changedLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// endAdmission is called with mu held, once per successful Watch admission.
// Socket loss retires its proof but Finish still ends the operation admission.
func (m *subscriptionManager) endAdmission() {
	m.admitted--
	m.changedLocked()
}

func (m *subscriptionManager) admit(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for m.cleanupActive {
		changed := m.changed
		m.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
		case <-m.ctx.Done():
		}
		m.mu.Lock()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if m.stopping {
			return m.ctx.Err()
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if m.stopping {
		return m.ctx.Err()
	}
	m.admitted++
	return nil
}

func (m *subscriptionManager) leaveCleanup() {
	m.mu.Lock()
	m.cleanupActive = false
	m.changedLocked()
	m.mu.Unlock()
}

// The single release worker fences all Client Watch admissions as well as the
// root gate: a direct descendant send cannot race its own ancestor's cleanup.
// Existing operations drain through Finish; waits hold neither client mutex.
func (m *subscriptionManager) enterCleanup(ctx context.Context) error {
	m.mu.Lock()
	m.cleanupActive = true
	for m.admitted != 0 {
		changed := m.changed
		m.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			m.leaveCleanup()
			return ctx.Err()
		}
		m.mu.Lock()
	}
	m.mu.Unlock()
	return nil
}

func (m *subscriptionManager) cleanupRoot(thread string, r *subscriptionRoot, conn *websocket.Conn) bool {
	m.mu.Lock()
	cleanup, abandoned, bound := m.cleanup, r.cleanupAbandoned, m.cleanupBound
	m.mu.Unlock()
	if cleanup == nil || abandoned {
		return true
	}
	ctx, cancel := context.WithTimeout(m.ctx, bound)
	defer cancel()
	err := m.enterCleanup(ctx)
	complete := false
	if err == nil {
		defer m.leaveCleanup()
		complete, err = cleanup(context.WithValue(ctx, cleanupConnectionKey{}, conn), thread)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping || m.roots[thread] != r || r.connection != conn || !r.releasePending {
		return false
	}
	if complete && err == nil {
		return m.ready(r)
	}
	if err != nil {
		r.cleanupErrors++
		if r.cleanupErrors == 1 {
			log.Printf("descendant subscription release: %s: %v", thread, err)
		}
		if r.cleanupErrors >= descendantErrorLimit {
			r.cleanupAbandoned = true
			log.Printf("descendant subscription release: %s: %d errors; remaining descendants unreleased; releasing root subscription", thread, r.cleanupErrors)
			return m.ready(r)
		}
	} else {
		r.cleanupErrors = 0 // A genuine active/unknown hold is not an error retry.
	}
	r.delay = min(releaseRetryCeiling, max(m.retryFloor, r.delay*2))
	r.due = time.Now().Add(r.delay)
	return false
}
