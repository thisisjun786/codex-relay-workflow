package daemon

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// The idle edge (CRW-904, section 82 of the CRW-781 decision).
//
// A delivery that finds its recipient mid-turn waits a doubling backoff, and that timer used to be
// the only thing that ended the wait: the head of a parent's receipt line held every younger
// delivery behind it for up to five minutes at a time. While a head waits, the relay keeps a
// subscription on the recipient, so the App Server reports that recipient's status changes here,
// and a report of idle (or notLoaded) makes the head due at once. The scheduler attempts it at that
// parent's next turn. The doubling backoff is not removed: it stays the safety net for a report the
// relay never saw, a host that reports no status, a recipient the relay could not subscribe, and a
// recipient that is busy again before the attempt lands.
//
// Nothing here is new authority. A report decides only when the relay looks: busy is still decided
// by the lifecycle read before any transport call (I-30), the min send interval and the hourly cap
// still pace the send (I-475), and the woken head keeps the line until it is claimed (I-478).

// The two sites the daemon's halt records a detection at (CRW-848, store.HaltSiteWrite and
// store.HaltSiteObservation). They are spelled here rather than imported because that code is newer
// than this node's baseline: the pass hands its own failure to the daemon's halt, which classifies it
// and fills the site, and the vocabulary the marker carries is these two words. The pass's own write
// is the wake; its read of the waiting heads is an observation, exactly as the observation pass's own
// census read is.
const (
	idleHaltSiteWrite       = "write"
	idleHaltSiteObservation = "observation"
)

// idleReports is the host capability the wake needs: the thread status reports the App Server
// pushed for the threads this connection subscribes to. A host without it (a scripted double, or a
// transport that is not the App Server) reports none, and the backoff stays the only trigger.
type idleReports interface {
	IdleReports() []delivery.IdleReport
}

// idleSubscriptions is the host capability the hold needs: a call that subscribes this connection
// to a recipient thread without holding the root's mutation gate, the release, and the question
// whether a subscription is held at all. A host without it never opens one, and the backoff stays
// the only trigger.
type idleSubscriptions interface {
	HoldThread(ctx context.Context, thread string) error
	ReleaseThread(thread string)
	ThreadHeld(thread string) bool
}

// idleWake is the daemon's idle-edge pass. holds is the thread this relay holds a subscription for,
// by recipient task id, so a hold is opened once and released when the backlog empties or the
// delivery is delivered. It is in memory on purpose: a subscription does not survive the process,
// so the next tick reads the store again and opens it again.
type idleWake struct {
	daemon *Daemon
	holds  map[string]string
	// refused is when a recipient's hold may be tried again, by recipient task id: a resume the host
	// refused is not retried on every tick, because the backoff is that recipient's trigger until its
	// own deadline (CRW-904 d3).
	refused map[string]float64
	// unappliedErr and unappliedSite are the first store failure this pass could not apply, kept for
	// the daemon's own halt: a store the relay has seen damaged takes no more writes (I-564), so a
	// failure of that class must end the tick rather than become a note (CRW-904 d1). The site is the
	// one the failure was seen at: the wake is this pass's own write, the head read an observation.
	unappliedErr  error
	unappliedSite string
	// halted is whether this pass has ended at the daemon's halt. It is the pass's own latch: once the
	// store is halted, the rest of the pass issues no statement at all (I-564), so the batch of reports
	// stops at the failure rather than continuing to write (CRW-904 correction, d1).
	halted bool
	// halt is the daemon's own halt (CRW-848, Daemon.halted), reached through the method the daemon
	// satisfies rather than named: that code is newer than this node's baseline, where the pass and the
	// daemon meet. On the baseline the method does not exist, so this stays nil and the pass only
	// records what it could not apply; once the halt lands, New wires it here and a corrupting failure
	// ends the tick instead of becoming a note, which is what the invariant requires.
	halt func(context.Context, *Report, string, error) bool
	// woken, opened and released are what the pass did, and notes is what it could not do: both are
	// read by the daemon's own tests and by Tick's report.
	woken, opened, released int
	notes                   []string
}

func newIdleWake(d *Daemon) *idleWake {
	w := &idleWake{daemon: d, holds: map[string]string{}, refused: map[string]float64{}}
	if halter, ok := any(d).(interface {
		halted(context.Context, *Report, string, error) bool
	}); ok {
		w.halt = halter.halted
	}
	return w
}

// begin starts one tick's pass: the counts and the notes describe this tick alone.
func (w *idleWake) begin() {
	w.woken, w.opened, w.released = 0, 0, 0
	w.notes = nil
	w.unappliedErr, w.unappliedSite = nil, ""
	w.halted = false
}

// unapplied records the store failure the pass has just failed to apply, for the daemon's halt. It
// overwrites, because the halt is consulted the moment the failure happens: keeping the first failure
// would let an earlier non-corrupting one (SQLITE_BUSY, say) mask a later corrupting one, and the
// store would then take the writes of the passes after it (CRW-904 correction, d1).
func (w *idleWake) unapplied(site string, err error) {
	w.unappliedErr, w.unappliedSite = err, site
}

// haltStore hands the failure the pass has just met to the daemon's own halt and answers whether the
// tick ended: a failure of the class that stops the store ends the tick there, so nothing after this
// pass writes anything, and every other failure leaves the pass exactly as it was, with the note it
// already wrote. It is called after every failure rather than once at the end of the pass, so no
// earlier failure can mask a later corrupting one and no statement is issued after the halt. A build
// without the halt answers false and the note stands alone.
func (w *idleWake) haltStore(ctx context.Context, r *Report) bool {
	if w.unappliedErr == nil || w.halt == nil {
		return false
	}
	if !w.halt(ctx, r, w.unappliedSite, w.unappliedErr) {
		return false
	}
	// The halt stands: nothing else in this tick writes. Clear the failure so the daemon does not
	// consult the halt a second time for the same detection.
	w.unappliedErr, w.unappliedSite = nil, ""
	w.halted = true
	return true
}

// take hands the notes this pass collected to the caller and clears them, so a tick that ends at the
// halt and one that runs to the end both append each note exactly once.
func (w *idleWake) take() []string {
	notes := w.notes
	w.notes = nil
	return notes
}

// idle turns the status reports the host pushed since the last tick into wakes. A report for a
// thread whose head is not waiting out a busy backoff changes nothing: WakeBusyHead writes only for
// the deferred-busy head that is still inside its backoff, so a younger delivery, a recipient with
// no backlog, and a report about a head that is already due are all no-ops.
func (w *idleWake) idle(ctx context.Context, r *Report, host Host, now float64) {
	reporter, ok := host.(idleReports)
	if !ok {
		return
	}
	for _, report := range reporter.IdleReports() {
		if w.halted {
			// The store is halted: this pass issues no further statement, so the reports after the
			// detection wait for the next daemon (I-564).
			break
		}
		if report.Status != "idle" && report.Status != "notLoaded" {
			continue
		}
		wrote, err := w.daemon.Delivery.WakeBusyHead(ctx, report.ThreadID, now)
		if err != nil {
			w.notes = append(w.notes, "idle report for "+report.ThreadID+" not applied: "+err.Error())
			// The wake is this pass's own write: a store that answers a failure of the halting class
			// ends the tick here rather than letting the delivery pass and the supervisor channel write
			// into a store the relay has seen damaged (I-564). Any other failure keeps its note. The
			// halt is consulted now, not once at the end of the batch, so no later report is written
			// after the detection and no earlier failure can mask it.
			w.unapplied(idleHaltSiteWrite, err)
			if w.haltStore(ctx, r) {
				break
			}
			continue
		}
		if wrote {
			w.woken++
		}
	}
}

// hold opens a subscription for every recipient whose head waits out a busy backoff and has none,
// and releases the ones whose backlog emptied or whose delivery was delivered. A hold the relay
// cannot open is noted once, and the backoff stays that recipient's only trigger until its own busy
// deadline, as section 82 says. The hold is this relay's own (ThreadHeld): a watch or the bridge's
// retention may already subscribe the thread, but that owner can release it mid-backlog, so the
// backlog takes a reference of its own rather than borrowing one (CRW-904 d4).
func (w *idleWake) hold(ctx context.Context, r *Report, host Host, now float64) {
	subscriptions, ok := host.(idleSubscriptions)
	if !ok {
		return
	}
	heads, err := w.daemon.Delivery.IdleWakeRecipients(ctx, now)
	if err != nil {
		w.notes = append(w.notes, "busy heads not read: "+err.Error())
		// The waiting heads are read out of the store: a corrupting read is the observation site, as
		// the observation pass's own census read is (I-564). The halt is consulted now, so the rest of
		// the pass issues no statement.
		w.unapplied(idleHaltSiteObservation, err)
		w.haltStore(ctx, r)
		return
	}
	wanted := map[string]string{}
	deadlines := map[string]float64{}
	for _, head := range heads {
		if head.RecipientThreadID != "" {
			wanted[head.RecipientTaskID] = head.RecipientThreadID
			deadlines[head.RecipientTaskID] = head.Deadline
		}
	}
	for recipient, thread := range w.holds {
		if _, waiting := wanted[recipient]; waiting {
			continue
		}
		subscriptions.ReleaseThread(thread)
		delete(w.holds, recipient)
		w.released++
	}
	for recipient := range w.refused {
		if _, waiting := wanted[recipient]; !waiting {
			delete(w.refused, recipient)
		}
	}
	for recipient, thread := range wanted {
		if held, ok := w.holds[recipient]; ok {
			// The hold is only real while this connection still carries it: a lost socket drops the
			// subscription with it, and the recipient is then held again below rather than believed.
			if held == thread && subscriptions.ThreadHeld(thread) {
				continue
			}
			if held != thread {
				// The recipient's thread changed under the hold: the old subscription is not the one its
				// reports will arrive on.
				subscriptions.ReleaseThread(held)
				w.released++
			}
			delete(w.holds, recipient)
		}
		if until, refused := w.refused[recipient]; refused && now < until {
			// A hold this relay could not take is not retried before that recipient's own busy
			// deadline: the backoff alone is its trigger until then (decision 4).
			continue
		}
		if err := subscriptions.HoldThread(ctx, thread); err != nil {
			w.notes = append(w.notes, "subscription for "+thread+" not opened: "+err.Error())
			w.refused[recipient] = w.holdRetryAt(now, deadlines[recipient])
			continue
		}
		delete(w.refused, recipient)
		w.holds[recipient] = thread
		w.opened++
	}
}

// holdRetryAt is when a hold this relay could not take may be tried again. The delivery's own busy
// deadline is the relay's next reason to look at that recipient, so a refusal waits for it; when that
// deadline has already passed, the wait is the busy curve's first step, the same pacing a deferred
// delivery gets. Either way the backoff alone is the trigger until then (decision 4).
func (w *idleWake) holdRetryAt(now, deadline float64) float64 {
	if deadline > now {
		return deadline
	}
	return now + w.daemon.Delivery.Policy.BusyBase
}
