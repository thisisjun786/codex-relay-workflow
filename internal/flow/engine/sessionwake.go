package engine

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file is how a session run wakes its owner thread (CRW-1316 §10). The owner, the project orchestrator, takes the steps the
// run publishes with `crw flow next`; between its turns nobody reads them. When the run publishes a step and the owner thread is
// idle or notLoaded, the run asks for a turn on it (SessionConfig.Wake, which stages the run: supervisor message of CRW-1333 in
// production: the relay opens a turn on an idle thread, resumes a notLoaded one and waits for a busy one's turn to end, CRW-1334).
// While steps wait and no subagent is dispatched for WakeAfter (15 minutes), the run notifies again, each interval twice the one
// before up to RenotifyMax (an hour). After UnresponsiveAfter (2 hours) without a dispatch it writes an escalation row
// (session_unresponsive) and goes on waiting: nothing is stopped. The logical id of a wake is (run, batch): the batch is the set of
// steps that wait, so a wake of the same steps staged again is the same message, and a re-notification is its own.

// Session wake defaults.
const (
	DefaultSessionWakeAfter         = 15 * time.Minute
	DefaultSessionRenotifyMax       = time.Hour
	DefaultSessionUnresponsiveAfter = 2 * time.Hour
)

// Row and reason names of the wake.
const (
	EvSessionWake             = "session_wake"
	ReasonSessionUnresponsive = "session_unresponsive"
)

// SessionWake is one wake of the owner thread.
type SessionWake struct {
	Run, Owner string
	// Batch names the steps that wait (a digest of their ops); ID is the wake's logical id: the batch, and /r<n> for the n-th
	// re-notification of it.
	Batch, ID string
	Renotify  int
	// Ops are the steps that wait (their markers), oldest first.
	Ops []string
	// Waiting is how long no subagent has been dispatched while steps waited.
	Waiting time.Duration
	// OwnerStatus is the owner thread's status as the host read it ("" when it could not be read).
	OwnerStatus string
}

func (s *SessionConfig) wakeAfter() time.Duration {
	if s.WakeAfter > 0 {
		return s.WakeAfter
	}
	return DefaultSessionWakeAfter
}

func (s *SessionConfig) renotifyMax() time.Duration {
	if s.RenotifyMax > 0 {
		return s.RenotifyMax
	}
	return DefaultSessionRenotifyMax
}

func (s *SessionConfig) unresponsiveAfter() time.Duration {
	if s.UnresponsiveAfter > 0 {
		return s.UnresponsiveAfter
	}
	return DefaultSessionUnresponsiveAfter
}

// renotifyDue is how long after the last dispatch the n-th re-notification (1, 2, ...) is due: WakeAfter, then intervals that
// double up to RenotifyMax.
func (s *SessionConfig) renotifyDue(n int) time.Duration {
	var due time.Duration
	step := s.wakeAfter()
	for i := 1; i <= n; i++ {
		due += step
		if step = 2 * step; step > s.renotifyMax() {
			step = s.renotifyMax()
		}
	}
	return due
}

// sessionWaker is what the run knows of its waiting steps.
type sessionWaker struct {
	mu sync.Mutex
	// waiting are the markers of the published spawn and send_input steps no subagent is bound to yet, with when they were
	// published; fresh says a step was published since the last look.
	waiting map[string]time.Time
	fresh   bool
	// quietSince is the last dispatch, or when steps began to wait after none did.
	quietSince time.Time
	batch      string
	renotified int
	escalated  bool
}

func newSessionWaker() *sessionWaker { return &sessionWaker{waiting: map[string]time.Time{}} }

// published notes a step the run published.
func (w *sessionWaker) published(marker string, now time.Time) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.waiting) == 0 {
		w.quietSince = now
	}
	if _, ok := w.waiting[marker]; !ok {
		w.waiting[marker] = now
		w.fresh = true
	}
}

// taken notes a step that no longer waits: bound says a subagent was dispatched for it.
func (w *sessionWaker) taken(marker string, bound bool, now time.Time) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.waiting, marker)
	if bound {
		w.quietSince, w.renotified, w.escalated = now, 0, false
	}
}

// batchOf is the batch of a set of waiting steps.
func batchOf(ops []string) string {
	return "b" + digestJSON([]byte(strings.Join(ops, "\n")))[:12]
}

// wakeLoop looks at the waiting steps every poll until the run ends.
func (e *Engine) wakeLoop(done chan struct{}) {
	defer close(done)
	s := e.cfg.Session
	for {
		if err := s.wait(e.ctx, s.poll()); err != nil {
			return
		}
		e.wakeLook(e.ctx)
	}
}

// wakeLook is one look: a new batch wakes an owner that is not busy, a batch that waited long enough is re-notified, and one that
// waited UnresponsiveAfter is escalated once.
func (e *Engine) wakeLook(ctx context.Context) {
	s, w := e.cfg.Session, e.sessW
	now := e.cfg.Now()
	w.mu.Lock()
	if len(w.waiting) == 0 {
		w.batch, w.fresh = "", false
		w.mu.Unlock()
		return
	}
	ops := make([]string, 0, len(w.waiting))
	for op := range w.waiting {
		ops = append(ops, op)
	}
	sort.Slice(ops, func(i, k int) bool {
		if a, b := w.waiting[ops[i]], w.waiting[ops[k]]; !a.Equal(b) {
			return a.Before(b)
		}
		return ops[i] < ops[k]
	})
	fresh := w.fresh
	if fresh {
		sorted := append([]string(nil), ops...)
		sort.Strings(sorted)
		w.fresh, w.batch = false, batchOf(sorted)
	}
	quiet := now.Sub(w.quietSince)
	batch, renotify := w.batch, 0
	if !fresh && quiet >= s.renotifyDue(w.renotified+1) {
		w.renotified++
		renotify = w.renotified
	}
	escalate := !w.escalated && quiet >= s.unresponsiveAfter()
	if escalate {
		w.escalated = true
	}
	since := w.quietSince.UTC().Format(time.RFC3339)
	w.mu.Unlock()

	if fresh || renotify > 0 {
		status := ""
		if ag, err := s.Host.ReadAgent(ctx, s.Owner); err == nil {
			status = ag.Status
		} else if errors.Is(err, ErrSessionAgentNotFound) {
			status = "notFound"
		}
		// a busy owner is working through `crw flow next` and sees the new steps there; a re-notification goes to it anyway, and the
		// relay delivers it after the turn
		if renotify > 0 || status != "active" {
			e.wake(ctx, SessionWake{Run: e.runID, Owner: s.Owner, Batch: batch, ID: wakeID(batch, renotify), Renotify: renotify, Ops: ops, Waiting: quiet, OwnerStatus: status})
		}
	}
	if escalate {
		e.j.Append(Event{RunID: e.runID, Kind: KindFlow, Event: EvEscalation, Data: map[string]any{"reason": ReasonSessionUnresponsive, "owner": s.Owner,
			"ops": ops, "waiting_seconds": int64(quiet / time.Second), "batch": batch,
			"detail": "the run's steps have waited without a dispatched report since " + since + "; the run goes on waiting"}})
	}
}

func wakeID(batch string, renotify int) string {
	if renotify == 0 {
		return batch
	}
	return batch + "/r" + strconv.Itoa(renotify)
}

// wake stages one wake and records it (a wake that could not be staged is recorded with its error: the run goes on).
func (e *Engine) wake(ctx context.Context, wk SessionWake) {
	s := e.cfg.Session
	data := map[string]any{"batch": wk.Batch, "wake_id": wk.ID, "renotify": wk.Renotify, "ops": wk.Ops, "owner": wk.Owner, "owner_status": wk.OwnerStatus,
		"waiting_seconds": int64(wk.Waiting / time.Second)}
	if err := s.Wake(ctx, wk); err != nil {
		data["staged"], data["error"] = false, err.Error()
	} else {
		data["staged"] = true
	}
	e.j.Append(Event{RunID: e.runID, Kind: KindFlow, Event: EvSessionWake, Data: data})
}
