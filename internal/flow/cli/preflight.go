package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The five facts a run's start is judged on. A run waits for its owner (a decision, a hold to recover), and the wait ends only when
// the way back to the owner's thread works; the facts are the readings of that way.
const (
	FactAssignment = "assignment_routed"
	FactDelivery   = "delivery_ticking"
	FactReachable  = "thread_reachable"
	FactWaits      = "waits_deliverable"
	FactIdleWake   = "idle_wake_observed"
)

// FactProjectKey is the comparison of the plan's project key with the owner thread's live parent bindings (CRW-1317). It is judged
// by the start, not by a Prober, and it is listed after the five only when it is not a pass.
const FactProjectKey = "project_key_bound"

// Facts is the five facts in the order the preview lists them.
var Facts = []string{FactAssignment, FactDelivery, FactReachable, FactWaits, FactIdleWake}

// Fact is one preflight fact with the level it was judged at, the reading it was judged on and, for a fact that did not pass, the
// reason of its level. A fact is never a refusal: `crw flow start` prints the facts and records the ones that did not pass in the
// run's spec, and starts the run (CRW-1318). Pass is Level == pass, kept for the readers of the earlier shape.
type Fact struct {
	Name    string `json:"name"`
	Level   string `json:"level"`
	Pass    bool   `json:"pass"`
	Reading string `json:"reading"`
	Reason  string `json:"reason,omitempty"`
}

// The levels of a fact: pass, the records show the way works; warn, the records cannot tell yet (nothing is recorded for the
// thread, or the store keeps no lasting record of the thing read) and the reason says why; fail, the records show the way does not
// work, or the fact cannot be read.
const (
	LevelPass = "pass"
	LevelWarn = "warn"
	LevelFail = "fail"
)

// leveled is the fact with its level and pass agreeing: a fact given with only Pass (a Prober of the earlier shape) is pass or fail.
func (f Fact) leveled() Fact {
	if f.Level == "" {
		f.Level = LevelFail
		if f.Pass {
			f.Level = LevelPass
		}
	}
	f.Pass = f.Level == LevelPass
	return f
}

// PreflightRequest is what the facts are judged for.
type PreflightRequest struct {
	// Reader is the relay store, read-only.
	Reader *Reader
	// Thread is the thread that owns the run.
	Thread string
	// Waits are the waiting dispositions the workflow has (crw.decide, crw.escalate); empty when it never waits.
	Waits []string
	Now   time.Time
}

// Prober judges the five facts. It returns exactly five, in the order of Facts.
type Prober interface {
	Facts(ctx context.Context, req PreflightRequest) []Fact
}

// ProberFunc is a Prober.
type ProberFunc func(ctx context.Context, req PreflightRequest) []Fact

// Facts implements Prober.
func (f ProberFunc) Facts(ctx context.Context, req PreflightRequest) []Fact { return f(ctx, req) }

// tickFreshness is how recent the delivery service's last look at a thread must be for the service to count as ticking.
const tickFreshness = 15 * time.Minute

// StoreProber judges the facts from what the relay records in its store:
//
//   - assignment_routed: an active or paused scope binding of the owner thread (the relay routes assignments to bound tasks); none
//     fails;
//   - delivery_ticking: the delivery service looked at a thread lately (the newest recipient_lifecycle observation is not older
//     than 15 minutes) and a delivery has reached a turn (a dispatched or acknowledged delivery names its turn); otherwise it fails;
//   - thread_reachable: the thread's lifecycle reading says a message can be sent or queued (deliverable yes or busy, not
//     archived) and the thread has authorized settings (its approval policy). A thread the delivery service has not read yet, or
//     one with no authorized settings recorded yet, is a warning: the lifecycle row is written after the first delivery or
//     supervisor send to the thread and the settings at its managed start or registration, so a new thread's first run has
//     neither. A reading that says the thread cannot take a message (archived, not deliverable) fails;
//   - waits_deliverable: every waiting disposition of the workflow ends in a delivery to the thread; a workflow that never waits
//     passes, one that waits passes when the three facts above pass, fails when thread_reachable fails and is a warning otherwise
//     (a fact above that did not pass is reported as itself);
//   - idle_wake_observed: the relay holds a wake of an idle thread (a delivery_wakes row) in this store, which is the scope of the
//     run. The relay deletes a wake once the attempt it released is answered (delivery.Service.spendWake), and records no other
//     trace of it, so no row is no evidence either way: the fact passes on a row and is a warning without one, never a fail.
//
// A fact that cannot be read fails with the reason: an unread fact is never a pass.
type StoreProber struct{}

// Facts implements Prober.
func (StoreProber) Facts(ctx context.Context, req PreflightRequest) []Fact {
	db := req.Reader.St.DB
	var out []Fact
	add := func(name, level, reason, format string, a ...any) {
		out = append(out, Fact{Name: name, Level: level, Reading: fmt.Sprintf(format, a...), Reason: reason}.leveled())
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM scope_bindings WHERE task_id = ? AND status IN ('active','paused')`, req.Thread).Scan(&n); err != nil {
		add(FactAssignment, LevelFail, "the store cannot be read", "the scope bindings cannot be read: %v", err)
	} else if n > 0 {
		add(FactAssignment, LevelPass, "", "%d live scope binding(s) of thread %s", n, req.Thread)
	} else {
		add(FactAssignment, LevelFail, "the relay routes no assignment to a thread without a scope binding: bind the thread to its project first", "%d live scope binding(s) of thread %s", n, req.Thread)
	}

	var seen sql.NullString
	var delivered int
	errSeen := db.QueryRowContext(ctx, `SELECT MAX(observed_at) FROM recipient_lifecycle`).Scan(&seen)
	errDel := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE state IN ('dispatched','acknowledged') AND COALESCE(dispatch_turn_id,'') <> ''`).Scan(&delivered)
	switch {
	case errSeen != nil || errDel != nil:
		add(FactDelivery, LevelFail, "the store cannot be read", "the delivery tables cannot be read: %v", errors.Join(errSeen, errDel))
	default:
		at, perr := time.Parse(time.RFC3339Nano, seen.String)
		fresh := seen.Valid && perr == nil && req.Now.Sub(at) <= tickFreshness
		last := "no thread has been looked at"
		if seen.Valid {
			last = "the newest lifecycle reading is " + seen.String
		}
		if fresh && delivered > 0 {
			add(FactDelivery, LevelPass, "", "%s; %d delivery(ies) reached a turn", last, delivered)
		} else {
			add(FactDelivery, LevelFail, "the delivery service has not looked at a thread in the last 15 minutes or has delivered nothing to a turn: is it running?", "%s; %d delivery(ies) reached a turn", last, delivered)
		}
	}

	var deliverable, archived, goal sql.NullString
	errLife := db.QueryRowContext(ctx, `SELECT deliverable, CAST(archived AS TEXT), goal_status FROM recipient_lifecycle WHERE task_id = ?`, req.Thread).Scan(&deliverable, &archived, &goal)
	var settings int
	errSet := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM authorized_settings WHERE task_id = ?`, req.Thread).Scan(&settings)
	switch {
	case errors.Is(errLife, sql.ErrNoRows) && errSet == nil:
		add(FactReachable, LevelWarn, "no record yet: the delivery service reads a thread after the first delivery or 부모 send (--role supervisor) to it, so a new thread's first run has no lifecycle reading", "the delivery service has not read thread %s; %d authorized setting row(s)", req.Thread, settings)
	case (errLife != nil && !errors.Is(errLife, sql.ErrNoRows)) || errSet != nil:
		add(FactReachable, LevelFail, "the store cannot be read", "the thread cannot be read: %v", errors.Join(errLife, errSet))
	default:
		reading := fmt.Sprintf("deliverable %s, archived %s, goal %s, %d authorized setting row(s)", cell(deliverable.String), cell(archived.String), cell(goal.String), settings)
		switch {
		case !(deliverable.String == "yes" || deliverable.String == "busy") || archived.String == "1":
			add(FactReachable, LevelFail, "the lifecycle reading says the thread cannot take a message", "%s", reading)
		case settings == 0:
			add(FactReachable, LevelWarn, "no record yet: authorized settings are recorded at the thread's managed start or registration, and none is recorded for this thread", "%s", reading)
		default:
			add(FactReachable, LevelPass, "", "%s", reading)
		}
	}

	switch {
	case len(req.Waits) == 0:
		add(FactWaits, LevelPass, "", "the workflow never waits for its owner")
	default:
		// the thread's own reading decides a fail; a fact above that did not pass makes the waits a warning, since it is reported as
		// a fact of its own
		level, reason := LevelPass, ""
		var short []string
		for _, f := range out[:3] {
			if f.Level != LevelPass {
				short = append(short, f.Name+" "+f.Level)
			}
		}
		switch {
		case out[2].Level == LevelFail:
			level, reason = LevelFail, "the waits deliver to the owner thread, and thread_reachable failed"
		case len(short) > 0:
			level, reason = LevelWarn, "the waits deliver through the three facts above, and not all of them passed: "+strings.Join(short, ", ")
		}
		add(FactWaits, level, reason, "%s wait for the owner; they deliver when the three facts above pass", strings.Join(req.Waits, ", "))
	}

	var wakes, spent int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(spent_at) FROM delivery_wakes`).Scan(&wakes, &spent); err != nil {
		add(FactIdleWake, LevelFail, "the store cannot be read", "the wakes cannot be read: %v", err)
	} else if wakes > 0 {
		add(FactIdleWake, LevelPass, "", "%d wake(s) of an idle thread held in this store (%d spent by an attempt)", wakes, spent)
	} else {
		add(FactIdleWake, LevelWarn, "cannot be judged from the store: the relay deletes a wake once its attempt is answered and keeps no other record of it, so no wake row is no evidence either way", "no wake of an idle thread held in this store")
	}
	return out
}

var (
	waitCall   = regexp.MustCompile(`crw\.(decide|escalate)\(`)
	nestedCall = regexp.MustCompile(`workflow\(\s*['"]([A-Za-z0-9_-]+)['"]`)
)

var laneCall = regexp.MustCompile(`\bcrw\.([A-Za-z]+)\s*\(`)

// engineCrw are the functions the engine puts on the crw object itself (engine/scriptvm.go); they need no operation of the lane.
var engineCrw = map[string]bool{"callFacts": true, "classify": true, "clock": true, "priorRelease": true, "recordedSignal": true, "tierStep": true}

// LaneOperations reads the `crw.*` operations a workflow calls, in the workflows it nests too: the names a build has to install
// (OpsFor) for the workflow to run. The functions the engine itself provides (crw.clock, crw.classify, crw.tierStep and the reads of the signal
// path crw.callFacts, crw.recordedSignal, crw.priorRelease) are not among them.
func LaneOperations(src Source) []string {
	seen := map[string]bool{}
	var out []string
	var walk func(body string, depth int)
	walk = func(body string, depth int) {
		for _, m := range laneCall.FindAllStringSubmatch(body, -1) {
			if !seen[m[1]] && !engineCrw[m[1]] {
				seen[m[1]] = true
				out = append(out, "crw."+m[1])
			}
		}
		if depth > 2 || src.Workflows == nil {
			return
		}
		for _, m := range nestedCall.FindAllStringSubmatch(body, -1) {
			if child, err := src.Workflows(m[1], ""); err == nil {
				walk(child.Body, depth+1)
			}
		}
	}
	walk(src.Program.Body, 0)
	return out
}

// Dispositions reads the waiting dispositions of a workflow: the kinds of wait it makes (crw.decide, crw.escalate), looking in the
// workflows it nests too.
func Dispositions(src Source) []string {
	seen := map[string]bool{}
	var out []string
	var walk func(body string, depth int)
	walk = func(body string, depth int) {
		for _, m := range waitCall.FindAllStringSubmatch(body, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, "crw."+m[1])
			}
		}
		if depth > 1 || src.Workflows == nil {
			return
		}
		for _, m := range nestedCall.FindAllStringSubmatch(body, -1) {
			if child, err := src.Workflows(m[1], ""); err == nil {
				walk(child.Body, depth+1)
			}
		}
	}
	walk(src.Program.Body, 0)
	return out
}
