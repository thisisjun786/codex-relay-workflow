package delivery

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-294: the delivery path's hot queries run on every tick, against tables that only grow. The
// product never runs ANALYZE, so SQLite plans them from its defaults, in which an equality on an
// index looks like ten rows whatever the table holds. These tests hold each query to the plan its
// author meant, and to the rows and order it returned before it was rewritten.

// planStep is one line of EXPLAIN QUERY PLAN.
type planStep struct {
	parent int64
	detail string
}

// plan is the EXPLAIN QUERY PLAN of a statement. The wording is that of the SQLite the store's
// driver bundles: a driver upgrade that rewords a step changes the assertions on it and nothing else.
func (w *hotWorld) plan(query string, args ...any) []planStep {
	w.tb.Helper()
	rows, err := all(w.ctx, w.store, "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		w.tb.Fatal(err)
	}
	var steps []planStep
	for _, r := range rows {
		steps = append(steps, planStep{parent: r.I("parent"), detail: r.S("detail")})
	}
	return steps
}

// outerLoop is the first table step of the statement's own loop (the steps that are not inside a
// subquery), which is where the statement starts reading.
func outerLoop(steps []planStep) string {
	for _, s := range steps {
		if s.parent == 0 && (strings.HasPrefix(s.detail, "SEARCH ") || strings.HasPrefix(s.detail, "SCAN ")) {
			return s.detail
		}
	}
	return ""
}

func planText(steps []planStep) string {
	var lines []string
	for _, s := range steps {
		lines = append(lines, s.detail)
	}
	return strings.Join(lines, "\n")
}

// outerText is the steps of the statement's own loop, without the subqueries it materializes.
func outerText(steps []planStep) string {
	var lines []string
	for _, s := range steps {
		if s.parent == 0 {
			lines = append(lines, s.detail)
		}
	}
	return strings.Join(lines, "\n")
}

// c1: a due delivery is found by its state, then its event is read by key. Starting from the events
// of stage 'final' reads every event the store has ever held.
func TestDueDeliveriesAreFoundByStateAndNotByEventStage(t *testing.T) {
	w := newHotWorld(t, "plan", 2000, 2, hotRevisionEvery)
	check := func(name string, steps []planStep) {
		t.Helper()
		if got := outerLoop(steps); !strings.HasPrefix(got, "SEARCH d USING INDEX deliveries_state") {
			t.Errorf("%s starts from %q, not from the deliveries by state\n%s", name, got, outerText(steps))
		}
		if text := planText(steps); strings.Contains(text, "events_stage") {
			t.Errorf("%s reads events through events_stage\n%s", name, outerText(steps))
		}
	}
	query, args := w.d.eligibleParentsQuery(w.now)
	check("EligibleParents", w.plan(query, args...))
	// The per-parent reads (dueRecipients, dueRows) are built on the same predicate.
	join, where, dueArgs := w.d.eligibility(w.now)
	perParent := "SELECT d.event_id" + dueFrom + join + where + " AND r.parent_task_id = ?" + eligibleOrder
	check("a due read of one parent", w.plan(perParent, append(dueArgs, "parent-01")...))
}

var scansAttemptsOrDeliveries = regexp.MustCompile("(?m)^SCAN (a|d)( |$)")

// c1: an open attempt is found by its internal state (attempts_open), or by the state of its delivery
// (deliveries_state), and never by reading every attempt.
func TestOpenParentsAreFoundThroughTheirIndexes(t *testing.T) {
	w := newHotWorld(t, "plan", 2000, 2, hotRevisionEvery)
	text := planText(w.plan(openParentsSQL, HeldUncertain, HeldUncertain, Sending))
	if scansAttemptsOrDeliveries.MatchString(text) {
		t.Errorf("OpenParents reads every attempt or delivery\n%s", text)
	}
	for _, index := range []string{"attempts_open", "deliveries_state"} {
		if !strings.Contains(text, index) {
			t.Errorf("OpenParents does not use %s\n%s", index, text)
		}
	}
}

// c1: the anchors to bind are found from the few generations that wait for one, and not from every
// delivery that was dispatched.
func TestPendingAnchorsAreFoundFromTheGenerationsThatWait(t *testing.T) {
	w := newHotWorld(t, "plan", 2000, 2, hotRevisionEvery)
	steps := w.plan(pendingAnchorsSQL, Revision, Dispatched, Acknowledged, "anchor_pending", pendingAnchorsLimit)
	if got := outerLoop(steps); !strings.HasPrefix(got, "SCAN g") {
		t.Errorf("BindPendingAnchors starts from %q, not from the generations\n%s", got, planText(steps))
	}
	if text := planText(steps); strings.Contains(text, "deliveries_state") {
		t.Errorf("BindPendingAnchors reads the deliveries by state\n%s", text)
	}
}

// hotMix is a deterministic pseudo-random integer in [0, n) of the integer SQL expression expr, for
// one salt: a multiplicative hash of the two. A world built from it is the same on every run.
func hotMix(expr string, salt, n int) string {
	return fmt.Sprintf("(((((%s) + %d) * 2654435761) %% 4294967296) / 4096 %% %d)", expr, salt, n)
}

// scramble gives every row of the world a state drawn from salt, so that each condition of the
// hot queries is met by some rows and missed by others: relationships paused, archived or superseded;
// events staged or suppressed; deliveries in every state, held, backing off or due; attempts in
// flight, held or without a state (few of them, so that the parents of the open ones are some of the
// parents and each of the two ways an attempt is open decides some of them); relationships over their
// hourly cap; anchors pending or bound.
func (w *hotWorld) scramble(salt int) {
	w.tb.Helper()
	rel := "CAST(substr(relationship_id, 5) AS INTEGER)"
	event := "CAST(substr(event_id, 4) AS INTEGER)"
	window := store.SendStamp(float64(int64(w.now)/3600*3600) + 60)
	w.d.Policy.MaxSendsPerRelationshipPerHour = 3
	for _, q := range []string{
		"UPDATE relationships SET status = CASE " + hotMix(rel, salt+1, 12) + " WHEN 0 THEN 'paused' WHEN 1 THEN 'archived' ELSE 'active' END," +
			" superseded_by = CASE WHEN " + hotMix(rel, salt+2, 15) + " = 0 THEN 'rel-9999' END," +
			" execution_generation = 1 + (" + hotMix(rel, salt+3, 6) + " = 0)",
		"UPDATE generations SET anchor_state = CASE WHEN " + hotMix(rel, salt+11, 2) + " = 0 THEN 'anchor_pending' ELSE 'bound' END",
		"UPDATE events SET stage = CASE " + hotMix(event, salt+4, 10) + " WHEN 0 THEN 'staged' WHEN 1 THEN 'suppressed' ELSE 'final' END," +
			" outcome = CASE WHEN " + hotMix(event, salt+5, 8) + " = 0 THEN 'merge_turn_grant' ELSE 'ready_for_review' END",
		"UPDATE deliveries SET state = CASE " + hotMix(event, salt+6, 12) + " WHEN 0 THEN 'queued' WHEN 1 THEN 'queued' WHEN 2 THEN 'queued' WHEN 3 THEN 'deferred_busy' WHEN 4 THEN 'deferred_busy'" +
			" WHEN 5 THEN 'withheld_pre_send' WHEN 6 THEN 'held_uncertain' WHEN 7 THEN 'sending' WHEN 8 THEN 'superseded' ELSE state END," +
			" hold_reason = CASE WHEN " + hotMix(event, salt+7, 10) + " = 0 THEN 'held' END," +
			fmt.Sprintf(" next_eligible_at = CASE %s WHEN 0 THEN %v - 100 WHEN 1 THEN %v + 100 WHEN 2 THEN %v END,", hotMix(event, salt+8, 5), w.now, w.now, w.now) +
			" dispatch_turn_id = CASE WHEN " + hotMix(event, salt+12, 6) + " = 0 THEN NULL ELSE dispatch_turn_id END",
		"UPDATE attempts SET internal_state = CASE WHEN " + hotMix(event, salt+9, 300) + " = 0 THEN 'in_flight' ELSE 'settled' END," +
			" state = CASE " + hotMix(event, salt+10, 40) + " WHEN 0 THEN 'held_uncertain' WHEN 1 THEN NULL ELSE state END," +
			" sent_at = CASE WHEN " + hotMix(event, salt+13, 4) + " = 0 THEN '" + window + "' ELSE sent_at END",
	} {
		w.exec(q)
	}
	// A send just begun is open both ways: its attempt is in flight and held, and its delivery is sending.
	w.exec("UPDATE attempts SET internal_state = 'in_flight', state = ? WHERE request_id IN ('req-000401-2', 'req-000802-2')", HeldUncertain)
	w.exec("UPDATE deliveries SET state = ? WHERE event_id IN ('ev-000401', 'ev-000802')", Sending)
}

// legacyDueRecipients is dueRecipients with eligibleBase as it was.
func (w *hotWorld) legacyDueRecipients(parent, after string, limit int) []string {
	w.tb.Helper()
	join, where, args := w.d.eligibility(w.now)
	where = strings.Replace(where, eligibleBase, legacyEligibleBase, 1)
	rows, err := all(w.ctx, w.store, "SELECT d.recipient_task_id AS recipient_task_id"+dueFrom+join+where+" AND r.parent_task_id = ? GROUP BY d.recipient_task_id ORDER BY d.recipient_task_id <= ?, d.recipient_task_id LIMIT ?", append(args, parent, after, limit)...)
	if err != nil {
		w.tb.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.S("recipient_task_id"))
	}
	return out
}

// legacyDueRows is dueRows with eligibleBase as it was.
func (w *hotWorld) legacyDueRows(parent, recipient, marker string, limit int) []Row {
	w.tb.Helper()
	join, where, args := w.d.eligibility(w.now)
	where = strings.Replace(where, eligibleBase, legacyEligibleBase, 1)
	query := "SELECT d.*, r.parent_task_id AS parent_task_id, e.first_seen_at AS first_seen_at" + dueFrom + join + where + " AND r.parent_task_id = ?"
	args = append(args, parent)
	if recipient != "" {
		query += " AND d.recipient_task_id = ?"
		args = append(args, recipient)
	}
	order := eligibleOrder
	if firstSeen, created, event, ok := splitKey(marker); ok {
		order = " ORDER BY CASE WHEN (e.first_seen_at, d.created_at, d.event_id) > (?,?,?) THEN 0 ELSE 1 END, e.first_seen_at, d.created_at, d.event_id"
		args = append(args, firstSeen, created, event)
	}
	rows, err := all(w.ctx, w.store, query+order+" LIMIT ?", append(args, limit)...)
	if err != nil {
		w.tb.Fatal(err)
	}
	return rows
}

// holdsTheSame reports each difference between what the current queries return in w and what the
// statements as they were return: the same rows, in the same order. It returns how much there was
// to compare: the due rows read, the parents with an open attempt and the anchors to bind. The
// statements as they were are slow on a large store, so the per-parent reads are compared for at most
// parents of them.
func (w *hotWorld) holdsTheSame(t *testing.T, parents int) (due, open, pending int) {
	t.Helper()
	// What is due: the parents, then each parent's recipients and rows, in the ways the scheduler reads them.
	eligible, err := w.d.EligibleParents(w.ctx, w.now)
	if err != nil {
		t.Fatal(err)
	}
	if want := w.legacyEligibleParents(); !slices.Equal(eligible, want) {
		t.Errorf("EligibleParents = %q, was %q", eligible, want)
	}
	for _, parent := range eligible[:min(parents, len(eligible))] {
		for _, after := range []string{"", parent, "child-0200", "~"} {
			got, err := w.d.dueRecipients(w.ctx, parent, w.now, after, 3)
			if err != nil {
				t.Fatal(err)
			}
			if want := w.legacyDueRecipients(parent, after, 3); !slices.Equal(got, want) {
				t.Errorf("dueRecipients(%s, after %q) = %q, was %q", parent, after, got, want)
			}
		}
		rows, err := w.d.dueRows(w.ctx, parent, w.now, "", "", 40)
		if err != nil {
			t.Fatal(err)
		}
		if want := w.legacyDueRows(parent, "", "", 40); !reflect.DeepEqual(rows, want) {
			t.Errorf("dueRows(%s) = %d rows, was %d rows, or in another order", parent, len(rows), len(want))
		}
		due += len(rows)
		for i, row := range rows {
			if i != 0 && i != len(rows)/2 && i != len(rows)-1 {
				continue
			}
			marker := row.S("first_seen_at") + "|" + row.S("created_at") + "|" + row.S("event_id")
			recipient := row.S("recipient_task_id")
			got, err := w.d.dueRows(w.ctx, parent, w.now, recipient, marker, 5)
			if err != nil {
				t.Fatal(err)
			}
			if want := w.legacyDueRows(parent, recipient, marker, 5); !reflect.DeepEqual(got, want) {
				t.Errorf("dueRows(%s, %s after %s) differ from what they were", parent, recipient, marker)
			}
		}
	}
	// What is open.
	opened, err := w.rc.OpenParents(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := w.legacyOpenParents(); !slices.Equal(opened, want) {
		t.Errorf("OpenParents = %q, was %q", opened, want)
	}
	// The other readers of the same predicate list the attempts those parents own: one definition of open.
	attempts, err := w.rc.OpenAttempts(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var owners []string
	for _, a := range attempts {
		if p := a.S("parent_task_id"); !seen[p] {
			seen[p] = true
			owners = append(owners, p)
		}
	}
	slices.Sort(owners)
	if len(opened) != 0 || len(owners) != 0 {
		if !slices.Equal(opened, owners) {
			t.Errorf("OpenParents = %q, but the open attempts belong to %q", opened, owners)
		}
	}
	open = len(opened)
	// What waits for an anchor.
	anchors, err := w.ack.pendingAnchors(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range anchors {
		got = append(got, r.S("event_id"))
	}
	if want := w.legacyPendingAnchors(); !slices.Equal(got, want) {
		t.Errorf("BindPendingAnchors reads %q, it read %q", got, want)
	}
	pending = len(got)
	return due, open, pending
}

// c3: in worlds where every condition of the three queries is met by some rows and missed by others,
// each query returns the rows it returned before and in the order it did.
func TestHotQueriesReturnTheRowsTheyReturnedInTheOrderTheyDid(t *testing.T) {
	for _, salt := range []int{1, 2, 3, 4} {
		t.Run(fmt.Sprintf("salt=%d", salt), func(t *testing.T) {
			t.Parallel()
			w := newHotWorld(t, "same", 3000, 2, 2)
			w.scramble(salt)
			due, open, pending := w.holdsTheSame(t, 6)
			// The world must not be vacuous: something is due, something is open, and more anchors
			// wait than one recovery pass binds, so the limit cuts the list.
			if due == 0 || open == 0 || pending != pendingAnchorsLimit {
				t.Fatalf("a world that checks little: %d due rows, %d open parents, %d pending anchors", due, open, pending)
			}
			// Each way an attempt is open decides some parents the other does not, and some attempts are
			// open both ways (an attempt in flight whose delivery is held), which a union must list once.
			parentsOf := func(where string, args ...any) []string {
				rows, err := all(w.ctx, w.store, "SELECT DISTINCT r.parent_task_id AS parent_task_id"+openAttemptsFrom+" WHERE "+where+" ORDER BY r.parent_task_id", args...)
				if err != nil {
					t.Fatal(err)
				}
				var out []string
				for _, r := range rows {
					out = append(out, r.S("parent_task_id"))
				}
				return out
			}
			inFlight := parentsOf("a.internal_state = 'in_flight'")
			held := parentsOf("a.state = ? AND d.state IN (?, ?)", HeldUncertain, HeldUncertain, Sending)
			onlyInFlight := slices.DeleteFunc(slices.Clone(inFlight), func(p string) bool { return slices.Contains(held, p) })
			onlyHeld := slices.DeleteFunc(slices.Clone(held), func(p string) bool { return slices.Contains(inFlight, p) })
			if len(onlyInFlight) == 0 || len(onlyHeld) == 0 {
				t.Errorf("a world where a term decides nothing: in flight %q, held %q", inFlight, held)
			}
			both, err := one(w.ctx, w.store, "SELECT COUNT(*) AS c FROM attempts a JOIN deliveries d ON d.event_id = a.event_id WHERE a.internal_state = 'in_flight' AND a.state = ? AND d.state IN (?, ?)", HeldUncertain, HeldUncertain, Sending)
			if err != nil || both.I("c") == 0 {
				t.Errorf("no attempt is open both ways (%v)", err)
			}
		})
	}
}

// c3: the same holds on the store the benchmarks measure, at the issue's scale, with something to find
// and with nothing.
func TestHotQueriesReturnTheSameRowsAtTheIssuesScale(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprintf("busy=%v", busy), func(t *testing.T) {
			t.Parallel()
			w := newHotWorld(t, "scale", hotBenchEvents, 2, hotRevisionEvery)
			if busy {
				w.makeBusy()
			}
			due, open, pending := w.holdsTheSame(t, 3)
			if (due > 0) != busy || (open > 0) != busy || (pending > 0) != busy {
				t.Errorf("busy=%v but %d due rows, %d open parents, %d pending anchors", busy, due, open, pending)
			}
		})
	}
}
