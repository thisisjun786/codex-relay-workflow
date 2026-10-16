package daemon

import (
	"context"
	"fmt"
	"math"

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
	ThreadHeld(thread string) bool
}

// idleWake is the daemon's idle-edge pass. holds is the thread this relay holds a subscription for,
// by recipient task id, so a hold is opened once and released when the backlog empties or the
// delivery is delivered. It is in memory on purpose: a subscription does not survive the process,
// so the next tick reads the store again and opens it again.
type idleWake struct {
	daemon *Daemon
	// beforeIdle and beforeHold are the tests' seams: a test damages the store at the point the pass starts,
	// so the pass's own statement meets a real corrupting failure. Nil in the daemon.
	beforeIdle, beforeHold func()
	holds                  map[string]string
	// attempted is every recipient this pass has called HoldThread for since its last release, by
	// recipient task id, whether or not the resume was answered. It is what the release sweep walks:
	// a resume whose answer never arrived may still have subscribed the recipient, and the relay owns
	// that subscription until ReleaseThread drops it, so the backlog emptying must release it too
	// (CRW-904 correction, d1). ReleaseThread is harmless when nothing is held.
	attempted map[string]string
	// refused is when a recipient's hold may be tried again, by recipient task id: a resume the host
	// refused is not retried on every tick, because the backoff is that recipient's trigger until its
	// own deadline (CRW-904 d3).
	refused map[string]float64
	// holdBackoff is the interval a repeatedly refused hold waits, by recipient task id: the same
	// doubling curve the delivery path uses for a busy answer, so a host that keeps refusing resumes is
	// not retried every tick once the delivery's own deadline has passed and there is no deadline left
	// to wait for. It is reset when a hold is taken or the recipient leaves the backlog (CRW-904
	// correction, d1).
	holdBackoff map[string]float64
	// carried are the reports a tick took off the host's channel and did not apply because a store
	// write of the idle pass failed: the failing report and every one after it. The host hands a report
	// over once, and a busy_cap or attempt_cap hold has no timer, so a report dropped here would leave
	// that hold standing until the recipient's next edge or a readback. The next pass applies them
	// before the host's new reports (CRW-1349). Applying a report again is harmless: a head already
	// woken and a message already due write nothing. In memory, like the holds.
	carried []delivery.IdleReport
	// headFailures is how many passes in a row the first carried report failed on. It bounds the carry: a
	// report whose wake fails on every pass is dropped after idleReportPasses tries, so it cannot end every
	// later tick before the delivery pass and the supervisor channel run (CRW-1349 evaluation d1).
	headFailures int
	// woken, opened and released are what the pass did, and notes is what it could not do: both are
	// read by the daemon's own tests and by Tick's report.
	woken, opened, released int
	notes                   []string
}

// idleReportPasses is how many passes one report is tried on when its wake keeps failing: the pass that
// fails it the last time drops it with a note and carries only the reports after it. A store-wide fault
// that is not corruption then costs at most this many ticks per report, and a per-report failure no
// longer wedges every tick behind it; the recipient's busy backoff, its next edge and the readback stay
// the triggers for a dropped report, as they were for every report before the carry.
const idleReportPasses = 3

func newIdleWake(d *Daemon) *idleWake {
	return &idleWake{daemon: d, holds: map[string]string{}, attempted: map[string]string{}, refused: map[string]float64{}, holdBackoff: map[string]float64{}}
}

// begin starts one tick's pass: the counts and the notes describe this tick alone.
func (w *idleWake) begin() {
	w.woken, w.opened, w.released = 0, 0, 0
	w.notes = nil
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
//
// The reports a failed tick did not apply come first (carried, CRW-1349), then the host's new ones.
func (w *idleWake) idle(ctx context.Context, r *Report, host Host, now float64) error {
	if w.beforeIdle != nil {
		w.beforeIdle()
	}
	reporter, ok := host.(idleReports)
	if !ok {
		return nil
	}
	// headFailed is how often reports[0] already failed: only a carried report has failed before.
	headFailed := 0
	if len(w.carried) > 0 {
		headFailed = w.headFailures
	}
	reports := append(w.carried, reporter.IdleReports()...)
	w.carried, w.headFailures = nil, 0
	for i, report := range reports {
		if report.Status != "idle" && report.Status != "notLoaded" {
			continue
		}
		wrote, err := w.daemon.Delivery.WakeBusyHead(ctx, report.ThreadID, now)
		if err != nil {
			// A store failure ends the tick here, before any later write: the wake is this pass's own write,
			// and nothing after a failed write may take the store's writes (I-564). The delivery pass and
			// the supervisor channel only run on a tick whose idle pass succeeded (CRW-904 correction, d1).
			// The reports not applied are kept for the next tick rather than dropped (CRW-1349), the failing
			// one only up to its last try.
			w.fail(reports, i, headFailed)
			return fmt.Errorf("idle report for %s not applied: %w", report.ThreadID, err)
		}
		if wrote {
			w.woken++
		}
		if w.daemon.Channel == nil {
			continue
		}
		// The supervisor channel takes the same edge (CRW-1334): its messages that wait on this recipient
		// are made due, and a busy_cap or attempt_cap hold of theirs is released. It is the same pass's own
		// write, so a store failure ends the tick the same way.
		count, err := w.daemon.Channel.WakeBusyRecipient(ctx, report.ThreadID, report.Status, now)
		if err != nil {
			// The delivery wake of this report may stand; applying it again next tick writes nothing.
			w.fail(reports, i, headFailed)
			return fmt.Errorf("idle report for %s not applied to the 부모 channel: %w", report.ThreadID, err)
		}
		w.woken += count.Woken + count.Released
	}
	return nil
}

// fail carries what a pass that failed on reports[i] did not apply. The failing report is kept at the head
// of the carried list unless this was its last try (idleReportPasses passes in a row, counting the passes it
// failed on as a carried report): then it is dropped with a note and the reports after it are carried.
// headFailed is how often reports[0] had failed before this pass.
func (w *idleWake) fail(reports []delivery.IdleReport, i, headFailed int) {
	failures := 1
	if i == 0 {
		failures = headFailed + 1
	}
	if failures >= idleReportPasses {
		report := reports[i]
		w.notes = append(w.notes, fmt.Sprintf("idle report %s for %s dropped after %d failed passes; its busy backoff, next edge or readback is its trigger", report.Status, report.ThreadID, failures))
		// A copy of the same report later in the batch is dropped with it, so it does not start its tries again.
		rest := make([]delivery.IdleReport, 0, len(reports)-i-1)
		for _, later := range reports[i+1:] {
			if later != report {
				rest = append(rest, later)
			}
		}
		w.carry(rest)
		return
	}
	w.carry(reports[i:])
	w.headFailures = failures
}

// carry keeps the reports a failed idle pass did not apply for the next pass, once each and in their
// order: a store that keeps failing tick after tick does not grow the list with the same reports.
func (w *idleWake) carry(reports []delivery.IdleReport) {
	seen := map[delivery.IdleReport]bool{}
	kept := make([]delivery.IdleReport, 0, len(reports))
	for _, report := range reports {
		if seen[report] {
			continue
		}
		seen[report] = true
		kept = append(kept, report)
	}
	w.carried = kept
}

// hold opens a subscription for every recipient whose head waits out a busy backoff and has none,
// and releases the ones whose backlog emptied or whose delivery was delivered. A hold the relay
// cannot open is noted once, and the backoff stays that recipient's only trigger until its own busy
// deadline, as section 82 says. The hold is this relay's own (ThreadHeld): a watch or the bridge's
// retention may already subscribe the thread, but that owner can release it mid-backlog, so the
// backlog takes a reference of its own rather than borrowing one (CRW-904 d4).
func (w *idleWake) hold(ctx context.Context, r *Report, host Host, now float64) error {
	if w.beforeHold != nil {
		w.beforeHold()
	}
	subscriptions, ok := host.(idleSubscriptions)
	if !ok {
		return nil
	}
	heads, err := w.daemon.Delivery.IdleWakeRecipients(ctx, now)
	if err != nil {
		// The waiting heads are read out of the store: a failed read ends the tick, as the idle pass's write
		// does: the delivery pass ran before it, and the supervisor channel does not write after it (CRW-904 d1).
		return fmt.Errorf("busy heads not read: %w", err)
	}
	if w.daemon.Channel != nil {
		// A supervisor message waiting on a busy recipient, or held at busy_cap or attempt_cap, needs the
		// same subscription for its recipient's edge to be seen (CRW-1334). The read is the same kind of
		// failure as the delivery heads' read above.
		waiting, err := w.daemon.Channel.IdleWakeRecipients(ctx, now)
		if err != nil {
			return fmt.Errorf("busy 부모 recipients not read: %w", err)
		}
		heads = append(heads, waiting...)
	}
	wanted := map[string]string{}
	deadlines := map[string]float64{}
	for _, head := range heads {
		if head.RecipientThreadID == "" {
			continue
		}
		// One recipient may wait in both channels: the subscription is one, on the thread the first
		// (the delivery path's) names, and the earliest deadline either channel has for it is the one a
		// refused hold waits for. A deadline of zero (a message only an edge releases) gives no time.
		if _, seen := wanted[head.RecipientTaskID]; !seen {
			wanted[head.RecipientTaskID] = head.RecipientThreadID
			deadlines[head.RecipientTaskID] = head.Deadline
		} else if earlier := deadlines[head.RecipientTaskID]; head.Deadline > 0 && (earlier <= 0 || head.Deadline < earlier) {
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
	// A recipient this pass tried to hold but whose resume was never answered is released the same
	// way: the host may have applied that resume, so the relay owns the subscription until
	// ReleaseThread drops it. The map is cleared here, so the next pass starts from what it holds.
	for recipient, thread := range w.attempted {
		if _, waiting := wanted[recipient]; waiting {
			continue
		}
		subscriptions.ReleaseThread(thread)
		delete(w.attempted, recipient)
		w.released++
	}
	for recipient := range w.refused {
		if _, waiting := wanted[recipient]; !waiting {
			delete(w.refused, recipient)
			delete(w.holdBackoff, recipient)
		}
	}
	for recipient, thread := range wanted {
		// The recipient's thread changed under a hold this pass owns, held or not: the old
		// subscription is not the one its reports will arrive on, so it is released rather than
		// replaced, exactly as a held thread is. Without this an unconfirmed resume on the old thread
		// would be overwritten and left with nobody owning it (CRW-904 correction, d1).
		if old, ok := w.attempted[recipient]; ok && old != thread {
			subscriptions.ReleaseThread(old)
			delete(w.attempted, recipient)
			w.released++
		}
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
			// The resume may have reached the host even though its answer did not: the recipient stays
			// in the release sweep, so the backlog emptying unsubscribes it (CRW-904 correction, d1).
			w.attempted[recipient] = thread
			w.refused[recipient] = w.holdRetryAt(recipient, now, deadlines[recipient])
			continue
		}
		delete(w.refused, recipient)
		delete(w.holdBackoff, recipient)
		delete(w.attempted, recipient)
		w.holds[recipient] = thread
		w.opened++
	}
	return nil
}

// holdRetryAt is when a hold this relay could not take may be tried again, by recipient. The delivery's
// own busy deadline is the relay's next reason to look at that recipient, so a refusal waits for it.
// When that deadline has already passed - the delivery is overdue and the scheduler has not reached it,
// or its relationship is at its hourly cap and cannot advance it - there is no deadline left to wait
// for, so the wait is the same doubling curve the delivery path gives a busy answer: the first refusal
// waits BusyBase and each further refusal doubles it up to BusyMax, rather than restarting at the first
// step on every tick (CRW-904 correction, d1). The curve is cleared when a hold is taken and when the
// recipient leaves the backlog.
func (w *idleWake) holdRetryAt(recipient string, now, deadline float64) float64 {
	if deadline > now {
		delete(w.holdBackoff, recipient)
		return deadline
	}
	step := w.holdBackoff[recipient]
	if step == 0 {
		step = w.daemon.Delivery.Policy.BusyBase
	} else {
		step = math.Min(w.daemon.Delivery.Policy.BusyMax, step*2)
	}
	w.holdBackoff[recipient] = step
	return now + step
}
