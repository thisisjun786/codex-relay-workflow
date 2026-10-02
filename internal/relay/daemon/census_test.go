package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The cost and the order of the census (CRW-262): which store rows it reads, and how it ranks a turn that has
// more than one reason to be pending.

// planOf is the EXPLAIN QUERY PLAN of a statement, one line per step.
func planOf(t *testing.T, ctx context.Context, s *store.Store, query string, args ...any) []string {
	t.Helper()
	rows, err := s.All(ctx, "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	steps := []string{}
	for _, row := range rows {
		steps = append(steps, row.Get("detail").(string))
	}
	return steps
}

// stageClaimAt stores a staged claim with its own event id, first seen at the given stamp.
func stageClaimAt(t *testing.T, s *store.Store, i int, event, turn, at string) {
	t.Helper()
	exec(t, s, "INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,stage,staged_at,first_seen_at,last_seen_at) VALUES(?,?,1,?,'ready_for_review','child',?,?,'inProgress','{}','staged',?,?,?)",
		event, name("rel", i), "rev-"+event, name("child", i), turn, at, at, at)
}

// pollRow stores a read of a turn under a generation of relationship i.
func pollRow(t *testing.T, s *store.Store, i, generation int, turn, at string) {
	t.Helper()
	exec(t, s, "INSERT INTO poll_observations(relationship_id,execution_generation,turn_id,last_status,last_polled_at,last_attempt_at) VALUES(?,?,?,'inProgress',?,?)", name("rel", i), generation, turn, at, at)
}

// c1: the poll rows of a relationship are never deleted, so the census must ask for the rows of its pending
// turns by key and not for the relationship's whole history. The plan is checked on the statement the census
// runs: a search by relationship, generation and turn, never by relationship alone.
func TestThePollRowsOfAPendingTurnAreReadByKey(t *testing.T) {
	ctx, s := throughputStore(t)
	steps := planOf(t, ctx, s, attemptsOfPendingTurns, "rel-00", `["turn"]`)
	found := false
	for _, step := range steps {
		if !strings.Contains(step, "poll_observations") {
			continue
		}
		found = true
		if !strings.HasPrefix(step, "SEARCH") || !strings.Contains(step, "(relationship_id=? AND execution_generation=? AND turn_id=?)") {
			t.Errorf("the poll rows are not read by the pending turn's key: %q (plan %q)", step, steps)
		}
	}
	if !found {
		t.Fatalf("plan %q never touches poll_observations", steps)
	}
}

// c1: with a long settled history, the statement returns the rows of the pending turns and nothing else, and the
// census puts each stamp on its own turn, whatever characters a turn id has (the ids travel as one JSON array).
func TestThePollHistoryOfPendingTurnsIsReadWhateverTheirIds(t *testing.T) {
	ctx, s := throughputStore(t)
	seedIndexed(t, s, 0)
	for k := range 300 {
		turn := fmt.Sprintf("settled-%03d", k)
		admitTurn(t, s, 0, 1, "anchor-00", turn)
		settleTurn(t, s, 0, turn)
		pollRow(t, s, 0, 1, turn, "2023-11-14T22:13:20.000000+00:00")
	}
	pending := map[string]string{
		"plain":           "2023-11-15T01:00:00.000000+00:00",
		`with"quote`:      "2023-11-15T01:01:00.000000+00:00",
		"with\\backslash": "2023-11-15T01:02:00.000000+00:00",
		"with\nnewline":   "2023-11-15T01:03:00.000000+00:00",
		"ünï-コード-🙂":       "2023-11-15T01:04:00.000000+00:00",
	}
	ids := []string{}
	for turn, at := range pending {
		admitTurnAt(t, s, 0, 1, "anchor-00", turn, "2023-11-14T22:13:20.000000+00:00")
		pollRow(t, s, 0, 1, turn, at)
		ids = append(ids, turn)
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.All(ctx, attemptsOfPendingTurns, "rel-00", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, row := range rows {
		got = append(got, row.Get("turn").(string))
	}
	slices.Sort(got)
	slices.Sort(ids)
	if !slices.Equal(got, ids) {
		t.Fatalf("the statement returned %d rows (first %q), want exactly the %d pending turns %q", len(got), got[:min(len(got), 6)], len(ids), ids)
	}
	d := New(s, &observationHost{status: "inProgress"}, movingClock(), nil)
	work, err := d.census(ctx)
	if err != nil || len(work) != 1 {
		t.Fatalf("census: %v, %v", work, err)
	}
	for turn, at := range pending {
		if p := work[0].turns[turn]; p == nil || p.attempt != at {
			t.Errorf("turn %q has attempt %+v, want %s", turn, p, at)
		}
	}
	if n := len(work[0].turns); n != len(pending)+1 {
		t.Errorf("%d pending turns, want the %d admitted ones and the anchor", n, len(pending))
	}
}

// A read of a turn counts whichever generation of its relationship it was stored under: a turn last read under
// generation 1 of a relationship now on generation 2 ranks behind a turn nobody has read, because the read is later
// than the other turn's admission. A guard for the poll statement's generation filter; it holds on the old
// statement too.
func TestAPendingTurnKeepsItsPollHistoryFromEveryGenerationOfItsRelationship(t *testing.T) {
	ctx, s := throughputStore(t)
	seedIndexed(t, s, 0)
	exec(t, s, "INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES('rel-00',2,'dispatch-2','bound','anchor-02','revision','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')")
	exec(t, s, "UPDATE relationships SET execution_generation=2 WHERE relationship_id='rel-00'")
	admitTurnAt(t, s, 0, 2, "anchor-02", "p", "2023-11-14T01:00:00.000000+00:00")
	admitTurnAt(t, s, 0, 2, "anchor-02", "q", "2023-11-14T02:00:00.000000+00:00")
	pollRow(t, s, 0, 1, "p", "2023-11-14T03:00:00.000000+00:00")
	host := &observationHost{status: "inProgress"}
	d := New(s, host, movingClock(), nil)
	d.Policy.MaxSends = -1
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readsOf(host, "p", "q"); !slices.Equal(got, []string{"q", "p"}) {
		t.Fatalf("read %v, want q before p: p was read after q was admitted", got)
	}
}

// readsOf is the order in which the host was asked about the named turns.
func readsOf(host *observationHost, turns ...string) []string {
	out := []string{}
	for _, turn := range host.reads {
		if slices.Contains(turns, turn) {
			out = append(out, turn)
		}
	}
	return out
}

// c2: a stamp later than the clock now was written before the clock was set back, so it counts as the oldest stamp
// there is, and a turn is ranked by the earliest of its reasons judged one by one. Turn x is pending for two reasons,
// one stamped from before a set-back (2100) and one validly, later than the stamp of turn y's only reason; it must
// come first. Both turns hold a staged claim, so they are ranked against each other. Judging the earliest stamp by
// its text first would rank x by its valid stamp and read y first.
func TestATurnWithARegressedStampAndANormalOneRanksByTheRegressedOne(t *testing.T) {
	const (
		regressed = "2100-01-01T00:00:00.000000+00:00"
		older     = "2023-11-14T01:00:00.000000+00:00"
		newer     = "2023-11-14T02:00:00.000000+00:00"
	)
	for _, c := range []struct {
		name string
		x    func(t *testing.T, s *store.Store)
	}{
		{"two staged claims", func(t *testing.T, s *store.Store) {
			stageClaimAt(t, s, 0, "x-regressed", "x", regressed)
			stageClaimAt(t, s, 0, "x-valid", "x", newer)
		}},
		{"a staged claim and an admission", func(t *testing.T, s *store.Store) {
			stageClaimAt(t, s, 0, "x-staged", "x", regressed)
			admitTurnAt(t, s, 0, 1, "anchor-00", "x", newer)
		}},
		{"an admission and a staged claim, the admission regressed", func(t *testing.T, s *store.Store) {
			admitTurnAt(t, s, 0, 1, "anchor-00", "x", regressed)
			stageClaimAt(t, s, 0, "x-staged", "x", newer)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, s := throughputStore(t)
			seedIndexed(t, s, 0)
			stageClaimAt(t, s, 0, "y-staged", "y", older)
			c.x(t, s)
			host := &observationHost{status: "inProgress"}
			d := New(s, host, movingClock(), nil)
			d.Policy.MaxSends = -1
			if _, err := d.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if got := readsOf(host, "x", "y"); !slices.Equal(got, []string{"x", "y"}) {
				t.Fatalf("read %v, want x (a regressed stamp counts as the oldest) before y", got)
			}
		})
	}
}

// c3: the admissions scan starts from the active relationships and reaches generation_turns through the key of
// each of their generations. Planned the other way round it scans generation_turns whole, the admissions of every
// relationship there ever was, archived ones included.
func TestTheAdmissionsScanStartsFromActiveRelationships(t *testing.T) {
	ctx, s := throughputStore(t)
	steps := planOf(t, ctx, s, unsettledAdmissions)
	reached := false
	for _, step := range steps {
		if strings.HasPrefix(step, "SCAN") && !strings.HasPrefix(step, "SCAN r ") {
			t.Errorf("the admissions scan reads a table whole: %q (plan %q)", step, steps)
		}
		if strings.HasPrefix(step, "SEARCH t ") {
			reached = true
			if !strings.Contains(step, "(relationship_id=? AND execution_generation=?)") {
				t.Errorf("generation_turns is not reached by relationship and generation: %q", step)
			}
		}
	}
	if !reached {
		t.Errorf("plan %q never searches generation_turns by key", steps)
	}
}

// The admissions scan lists the unsettled admissions of active, current relationships whose evidence names the
// generation's own anchor, in relationship and admission order, and nothing else. A guard for the rewritten
// statement; it holds on the old one.
func TestTheAdmissionsScanListsExactlyTheUnsettledAdmissionsOfActiveRelationships(t *testing.T) {
	ctx, s := throughputStore(t)
	for i := range 5 {
		seedIndexed(t, s, i)
	}
	bound := func(anchor string) string { return "explicit_admission_bound:" + anchor }
	admit := func(i, generation int, evidence, turn string) {
		exec(t, s, "INSERT INTO generation_turns(relationship_id,execution_generation,turn_id,evidence,actor,detail,admitted_at) VALUES(?,?,?,?,'child','admitted','2023-11-14T22:13:20Z')", name("rel", i), generation, turn, evidence)
	}
	// rel-00: two unsettled admissions (stored p2 first), a settled one, one admitted on other evidence.
	admit(0, 1, bound("anchor-00"), "p2")
	admit(0, 1, bound("anchor-00"), "p1")
	admit(0, 1, bound("anchor-00"), "settled")
	settleTurn(t, s, 0, "settled")
	admit(0, 1, "not_the_anchor", "odd")
	// rel-01 is archived and rel-02 superseded: neither is active and current.
	admit(1, 1, bound("anchor-01"), "z1")
	exec(t, s, "UPDATE relationships SET status='archived' WHERE relationship_id='rel-01'")
	admit(2, 1, bound("anchor-02"), "z2")
	exec(t, s, "UPDATE relationships SET superseded_by='rel-04' WHERE relationship_id='rel-02'")
	// rel-03 has a second generation with its own anchor and an unsettled admission in it, and an admission that
	// names an anchor the generation does not have.
	exec(t, s, "INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES('rel-03',2,'dispatch-2','bound','anchor-03b','revision','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')")
	admit(3, 2, bound("anchor-03b"), "g2")
	admit(3, 2, bound("anchor-03"), "stale")
	// rel-04 has a generation whose anchor is not bound yet: nothing is admitted against it.
	exec(t, s, "INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,opened_at) VALUES('rel-04',2,'dispatch-2','anchor_pending','2023-11-14T22:13:20Z')")
	admit(4, 2, bound("anchor-04"), "nobody")
	rows, err := s.All(ctx, unsettledAdmissions)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, row := range rows {
		got = append(got, row.Get("rid").(string)+"/"+row.Get("turn").(string))
	}
	if want := []string{"rel-00/p2", "rel-00/p1", "rel-03/g2"}; !slices.Equal(got, want) {
		t.Fatalf("admissions %v, want %v", got, want)
	}
}
