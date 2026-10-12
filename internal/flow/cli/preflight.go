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

// Facts is the five facts in the order the preview lists them.
var Facts = []string{FactAssignment, FactDelivery, FactReachable, FactWaits, FactIdleWake}

// Fact is one preflight fact with the reading it was judged on.
type Fact struct {
	Name    string `json:"name"`
	Pass    bool   `json:"pass"`
	Reading string `json:"reading"`
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
//   - assignment_routed: an active or paused scope binding of the owner thread (the relay routes assignments to bound tasks);
//   - delivery_ticking: the delivery service looked at a thread lately (the newest recipient_lifecycle observation is not older
//     than 15 minutes) and a delivery has reached a turn (a dispatched or acknowledged delivery names its turn);
//   - thread_reachable: the thread's lifecycle reading says a message can be sent or queued (deliverable yes or busy, not
//     archived) and the thread has authorized settings (its approval policy);
//   - waits_deliverable: every waiting disposition of the workflow ends in a delivery to the thread; a workflow that never waits
//     passes, one that waits passes when the three facts above do;
//   - idle_wake_observed: the relay has woken an idle thread into a turn without anybody there (a spent delivery wake) in this
//     store, which is the scope of the run.
//
// A fact that cannot be read fails with the reason: an unread fact is never a pass.
type StoreProber struct{}

// Facts implements Prober.
func (StoreProber) Facts(ctx context.Context, req PreflightRequest) []Fact {
	db := req.Reader.St.DB
	var out []Fact
	add := func(name string, pass bool, format string, a ...any) {
		out = append(out, Fact{Name: name, Pass: pass, Reading: fmt.Sprintf(format, a...)})
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM scope_bindings WHERE task_id = ? AND status IN ('active','paused')`, req.Thread).Scan(&n); err != nil {
		add(FactAssignment, false, "the scope bindings cannot be read: %v", err)
	} else {
		add(FactAssignment, n > 0, "%d live scope binding(s) of thread %s", n, req.Thread)
	}

	var seen sql.NullString
	var delivered int
	errSeen := db.QueryRowContext(ctx, `SELECT MAX(observed_at) FROM recipient_lifecycle`).Scan(&seen)
	errDel := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE state IN ('dispatched','acknowledged') AND COALESCE(dispatch_turn_id,'') <> ''`).Scan(&delivered)
	switch {
	case errSeen != nil || errDel != nil:
		add(FactDelivery, false, "the delivery tables cannot be read: %v", errors.Join(errSeen, errDel))
	default:
		at, perr := time.Parse(time.RFC3339Nano, seen.String)
		fresh := seen.Valid && perr == nil && req.Now.Sub(at) <= tickFreshness
		last := "no thread has been looked at"
		if seen.Valid {
			last = "the newest lifecycle reading is " + seen.String
		}
		add(FactDelivery, fresh && delivered > 0, "%s; %d delivery(ies) reached a turn", last, delivered)
	}

	reachable := false
	var deliverable, archived, goal sql.NullString
	errLife := db.QueryRowContext(ctx, `SELECT deliverable, CAST(archived AS TEXT), goal_status FROM recipient_lifecycle WHERE task_id = ?`, req.Thread).Scan(&deliverable, &archived, &goal)
	var settings int
	errSet := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM authorized_settings WHERE task_id = ?`, req.Thread).Scan(&settings)
	switch {
	case errors.Is(errLife, sql.ErrNoRows):
		add(FactReachable, false, "the delivery service has not read thread %s", req.Thread)
	case errLife != nil || errSet != nil:
		add(FactReachable, false, "the thread cannot be read: %v", errors.Join(errLife, errSet))
	default:
		reachable = (deliverable.String == "yes" || deliverable.String == "busy") && archived.String != "1" && settings > 0
		add(FactReachable, reachable, "deliverable %s, archived %s, goal %s, %d authorized setting row(s)", cell(deliverable.String), cell(archived.String), cell(goal.String), settings)
	}

	switch {
	case len(req.Waits) == 0:
		add(FactWaits, true, "the workflow never waits for its owner")
	default:
		pass := out[0].Pass && out[1].Pass && reachable
		add(FactWaits, pass, "%s wait for the owner; they deliver when the three facts above pass", strings.Join(req.Waits, ", "))
	}

	var wakes int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delivery_wakes WHERE spent_at IS NOT NULL`).Scan(&wakes); err != nil {
		add(FactIdleWake, false, "the wakes cannot be read: %v", err)
	} else {
		add(FactIdleWake, wakes > 0, "%d spent wake(s) of an idle thread in this store", wakes)
	}
	return out
}

var (
	waitCall   = regexp.MustCompile(`crw\.(decide|escalate)\(`)
	nestedCall = regexp.MustCompile(`workflow\(\s*['"]([A-Za-z0-9_-]+)['"]`)
)

var laneCall = regexp.MustCompile(`\bcrw\.([A-Za-z]+)\s*\(`)

// engineCrw are the functions the engine puts on the crw object itself (engine/scriptvm.go); they need no operation of the lane.
var engineCrw = map[string]bool{"classify": true, "clock": true, "tierStep": true}

// LaneOperations reads the `crw.*` operations a workflow calls, in the workflows it nests too: the names a build has to install
// (OpsFor) for the workflow to run. The functions the engine itself provides (crw.clock, crw.classify, crw.tierStep) are not among them.
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
