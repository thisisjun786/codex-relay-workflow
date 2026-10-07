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
	ThreadSubscribed(thread string) bool
}

// idleWake is the daemon's idle-edge pass. holds is the thread this relay holds a subscription for,
// by recipient task id, so a hold is opened once and released when the backlog empties or the
// delivery is delivered. It is in memory on purpose: a subscription does not survive the process,
// so the next tick reads the store again and opens it again.
type idleWake struct {
	daemon *Daemon
	holds  map[string]string
	// woken, opened and released are what the last tick did, for the daemon's own tests.
	woken, opened, released int
	notes                   []string
}

func newIdleWake(d *Daemon) *idleWake { return &idleWake{daemon: d, holds: map[string]string{}} }

// idle turns the status reports the host pushed since the last tick into wakes. A report for a
// thread whose head is not waiting out a busy backoff changes nothing: WakeBusyHead writes only for
// the deferred-busy head that is still inside its backoff, so a younger delivery, a recipient with
// no backlog, and a report about a head that is already due are all no-ops.
func (w *idleWake) idle(ctx context.Context, host Host, now float64) {
	w.woken, w.opened, w.released = 0, 0, 0
	w.notes = nil
	reporter, ok := host.(idleReports)
	if !ok {
		return
	}
	for _, report := range reporter.IdleReports() {
		if report.Status != "idle" && report.Status != "notLoaded" {
			continue
		}
		wrote, err := w.daemon.Delivery.WakeBusyHead(ctx, report.ThreadID, now)
		if err != nil {
			w.notes = append(w.notes, "idle report for "+report.ThreadID+" not applied: "+err.Error())
			continue
		}
		if wrote {
			w.woken++
		}
	}
}

// hold opens a subscription for every recipient whose head waits out a busy backoff and has none,
// and releases the ones whose backlog emptied or whose delivery was delivered. A hold the relay
// cannot open is noted, and the backoff stays that recipient's only trigger, as section 82 says.
func (w *idleWake) hold(ctx context.Context, host Host, now float64) {
	subscriptions, ok := host.(idleSubscriptions)
	if !ok {
		return
	}
	heads, err := w.daemon.Delivery.BusyHeadRecipients(ctx, now)
	if err != nil {
		w.notes = append(w.notes, "busy heads not read: "+err.Error())
		return
	}
	wanted := map[string]string{}
	for _, head := range heads {
		if head.RecipientThreadID != "" {
			wanted[head.RecipientTaskID] = head.RecipientThreadID
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
	for recipient, thread := range wanted {
		if _, held := w.holds[recipient]; held {
			continue
		}
		if subscriptions.ThreadSubscribed(thread) {
			// This connection already receives the thread's reports, so the idle edge is already
			// visible and there is nothing to open.
			continue
		}
		if err := subscriptions.HoldThread(ctx, thread); err != nil {
			w.notes = append(w.notes, "subscription for "+thread+" not opened: "+err.Error())
			continue
		}
		w.holds[recipient] = thread
		w.opened++
	}
}
