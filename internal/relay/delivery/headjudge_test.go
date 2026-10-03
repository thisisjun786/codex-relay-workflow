package delivery

import (
	"context"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-416: the head of a generation is judged by reading every reviewable revision of it. The
// judgment now finds each lineage row by its key, walks the declared chains once, and is made once
// per claim; its answer must not change. The oracle below is the judgment as it was before, kept as
// written, and every test compares the current code with it.

// legacyHeadRevisionSQL is the statement HeadRevisionFrom ran before CRW-416: the lineage joined on
// event_id alone.
const legacyHeadRevisionSQL = "SELECT e.event_id, e.revision_hash, l.supersedes_hash FROM events e LEFT JOIN revision_lineage l ON l.event_id = e.event_id WHERE e.relationship_id = ? AND e.execution_generation = ? AND e.outcome = ? AND e.suppressed_reason IS NULL ORDER BY e.event_id"

// legacyHeadRevisionFrom is HeadRevisionFrom before CRW-416.
func legacyHeadRevisionFrom(ctx context.Context, q store.Querier, rid string, generation int64) (Obj, error) {
	rows, err := allFrom(ctx, q, legacyHeadRevisionSQL, rid, generation, "ready_for_review")
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return Obj{{Key: "eventId", Value: nil}, {Key: "revisionHash", Value: nil}, {Key: "evidence", Value: NoRevision}, {Key: "competitors", Value: []any{}}, {Key: "detail", Value: "no reviewable revision in this generation"}}, nil
	}
	anchors, err := requestedPredecessors(ctx, q, rid, generation)
	if err != nil {
		return nil, err
	}
	return legacyJudge(rows, anchors), nil
}

// legacyJudge is the judgment of HeadRevisionFrom before CRW-416, from the rows it read.
func legacyJudge(rows []Row, anchors map[string][]string) Obj {
	var nodes []string
	hashOf := map[string]string{}
	declaredOf := map[string]string{}
	byHash := map[string][]string{}
	for _, row := range rows {
		id := row.S("event_id")
		if _, seen := hashOf[id]; !seen {
			nodes = append(nodes, id)
		}
		hashOf[id] = row.S("revision_hash")
		declaredOf[id] = row.S("supersedes_hash")
	}
	for _, id := range nodes {
		byHash[hashOf[id]] = append(byHash[hashOf[id]], id)
	}
	edges := map[string]string{}
	var unresolved []string
	for _, id := range nodes {
		declared := declaredOf[id]
		if declared == "" {
			continue
		}
		targets := append(slices.Clone(byHash[declared]), anchors[declared]...)
		if len(targets) != 1 {
			unresolved = append(unresolved, id+" -> "+declared)
			continue
		}
		edges[id] = targets[0]
	}
	if len(unresolved) > 0 {
		return ambiguous(UnknownPredecessor, nodes, "a declared predecessor is neither a unique revision of this generation nor its requested correction predecessor: "+strings.Join(unresolved, ", "))
	}
	for _, start := range nodes {
		seen := map[string]bool{start: true}
		current := start
		for {
			next, ok := edges[current]
			if !ok {
				break
			}
			current = next
			if seen[current] {
				return ambiguous(Cycle, nodes, fmt.Sprintf("the declared chain from %s returns to %s", start, current))
			}
			seen[current] = true
		}
	}
	predecessors := map[string]int{}
	for _, id := range nodes {
		if target, ok := edges[id]; ok {
			predecessors[target]++
		}
	}
	var forked []string
	for target, count := range predecessors {
		if count > 1 {
			forked = append(forked, target)
		}
	}
	if len(forked) > 0 {
		slices.Sort(forked)
		return ambiguous(Fork, nodes, "more than one revision declares the same predecessor: "+strings.Join(forked, ", "))
	}
	targets := map[string]bool{}
	for _, t := range edges {
		targets[t] = true
	}
	var tips []string
	for _, id := range nodes {
		if !targets[id] {
			tips = append(tips, id)
		}
	}
	slices.Sort(tips)
	if len(tips) != 1 {
		return ambiguous(Fork, nodes, fmt.Sprintf("%d revisions in this generation are unsuperseded, so none of them is the head; a revision that replaces another says so when it is emitted", len(tips)))
	}
	tip := tips[0]
	covered := map[string]bool{tip: true}
	for current := tip; ; {
		next, ok := edges[current]
		if !ok {
			break
		}
		current = next
		covered[current] = true
	}
	for _, id := range nodes {
		if !covered[id] {
			return ambiguous(Disconnected, nodes, "the declared chain from the tip does not reach every revision in this generation")
		}
	}
	evidence := Chain
	if len(nodes) == 1 && len(edges) == 0 {
		evidence = Sole
	}
	return Obj{{Key: "eventId", Value: tip}, {Key: "revisionHash", Value: hashOf[tip]}, {Key: "evidence", Value: evidence}, {Key: "competitors", Value: []any{}}, {Key: "detail", Value: ""}}
}

// legacySupersessionReason is SupersessionReason before CRW-416: the same decisions, reading the head with the old statement.
func (d *Service) legacySupersessionReason(ctx context.Context, eventID string) (string, error) {
	event, err := one(ctx, d.Store, "SELECT relationship_id, execution_generation, outcome, event_id, receipt FROM events WHERE event_id = ?", eventID)
	if err != nil || event == nil {
		return "", err
	}
	if event.S("outcome") == MergeTurnGrant {
		return mergeturn.GrantSupersessionFor(ctx, d.Store, event.S("receipt"))
	}
	rel, err := one(ctx, d.Store, "SELECT execution_generation FROM relationships WHERE relationship_id = ?", event.S("relationship_id"))
	if err != nil || rel == nil {
		return "", err
	}
	if event.I("execution_generation") < rel.I("execution_generation") {
		return StaleGeneration, nil
	}
	if event.S("outcome") == Revision {
		answered, err := one(ctx, d.Store, "SELECT 1 FROM events WHERE relationship_id = ? AND execution_generation = ? AND stage = 'final' AND suppressed_reason IS NULL AND event_id != ? AND outcome NOT IN ('merge_turn_grant')", event.S("relationship_id"), event.I("execution_generation"), eventID)
		if err != nil || answered == nil {
			return "", err
		}
		return SupersededRevision, nil
	}
	if slices.Contains(executionOnlyOutcomes, event.S("outcome")) {
		return "", nil
	}
	head, err := legacyHeadRevisionFrom(ctx, d.Store.Q(ctx), event.S("relationship_id"), event.I("execution_generation"))
	if err != nil {
		return "", err
	}
	headID, _ := head.Lookup("eventId")
	if headID == nil || headID == eventID {
		return "", nil
	}
	successor, err := one(ctx, d.Store, "SELECT stage FROM events WHERE event_id = ?", headID)
	if err != nil || successor == nil || successor.S("stage") != "final" {
		return "", err
	}
	return SupersededRevision, nil
}

// sameAnswer reports whether two head answers are the same bytes.
func sameAnswer(a, b Obj) bool { return dumps(a) == dumps(b) }

// graphEvent is one event of a head case. Zero values mean generation 1, outcome ready_for_review,
// stage final, not suppressed.
type graphEvent struct {
	id, hash, declared string
	generation         int64
	outcome            string
	stage              string
	suppressed         bool
}

// graphCase is a store holding the given events and the answers the current and the old code must
// both give.
type graphCase struct {
	name string
	// current is the generation the relationship stands on and judge the generation the head is
	// read for; zero means 1 and current.
	current, judge int64
	events         []graphEvent
	// requested seeds the correction of "p1" (generation 1) that opened generation 2: the request
	// event and the ruling that name it.
	requested bool
	evidence  string
	head      string
	// reasons are the SupersessionReason answers expected for some of the events.
	reasons map[string]string
}

// graphStore is one store holding many cases, each under a relationship and event ids of its own
// (opening a store is the dearest step of a test here).
type graphStore struct {
	t     *testing.T
	ctx   context.Context
	store *store.Store
	d     *Service
}

func newGraphStore(t *testing.T) *graphStore {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &graphStore{t: t, ctx: ctx, store: s, d: NewService(s, &FakeClock{T: 1_700_000_000})}
}

// seededCase is a case as stored: its relationship, the generation to judge and the prefix its
// event ids carry in the store (the case names them without it).
type seededCase struct {
	graphCase
	rid, prefix string
	judged      int64
}

// seed stores case number n.
func (g *graphStore) seed(n int, c graphCase) seededCase {
	t := g.t
	t.Helper()
	rid, prefix := fmt.Sprintf("rel-graph-%02d", n), fmt.Sprintf("c%02d-", n)
	current := max(c.current, 1)
	g.exec("INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES (?, ?, 'active', 'parent-graph', 'host-1', 'child-graph', 'host-1', ?, '[]', '[]', 'x', 'x')", rid, "GRAPH-"+prefix, current)
	request, ruling := "", "vt"
	if c.requested {
		var err error
		if request, err = store.RevisionRequestEventID(rid, prefix+"p1", ruling); err != nil {
			t.Fatal(err)
		}
	}
	for generation := int64(1); generation <= current; generation++ {
		reason, dispatch := "initial_assignment", fmt.Sprintf("dispatch-%d", generation)
		if generation > 1 {
			reason = "needs_changes_revision"
		}
		if generation == 2 && c.requested {
			dispatch = "revision-" + request
		}
		g.exec("INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at, bound_at) VALUES (?, ?, ?, 'bound', 'anchor', ?, 'x', 'x')", rid, generation, dispatch, reason)
	}
	events := slices.Clone(c.events)
	if c.requested {
		events = append(events,
			graphEvent{id: "p1", hash: "hp", generation: 1},
			graphEvent{id: "request", hash: "-", generation: 2, outcome: "revision_request"})
		g.exec("INSERT INTO verdicts (event_id, record, verdict, next_generation, verdict_turn_id, decided_at) VALUES (?, '{}', 'needs_changes', 2, ?, 'x')", prefix+"p1", ruling)
	}
	for _, e := range events {
		id := prefix + e.id
		if e.id == "request" {
			id = request
		}
		generation, outcome, stage, producer := max(e.generation, 1), e.outcome, e.stage, "child"
		if outcome == "" {
			outcome = "ready_for_review"
		}
		if outcome == "revision_request" {
			producer = "relay"
		}
		if stage == "" {
			stage = "final"
		}
		var suppressed any
		if e.suppressed {
			suppressed = "superseded"
		}
		g.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, suppressed_reason, first_seen_at, last_seen_at) VALUES (?, ?, ?, ?, ?, ?, 'child-graph', ?, 'completed', '{}', ?, ?, 'x', 'x')", id, rid, generation, e.hash, outcome, producer, "turn-"+id, stage, suppressed)
		if outcome == "ready_for_review" {
			var declared any
			declaredBy := "undeclared"
			if e.declared != "" {
				declared, declaredBy = e.declared, "child_declared"
			}
			g.exec("INSERT INTO revision_lineage (relationship_id, execution_generation, event_id, revision_hash, supersedes_hash, declared_by, recorded_at) VALUES (?, ?, ?, ?, ?, ?, 'x')", rid, generation, id, e.hash, declared, declaredBy)
		}
	}
	judged := c.judge
	if judged == 0 {
		judged = current
	}
	return seededCase{graphCase: c, rid: rid, prefix: prefix, judged: judged}
}

func (g *graphStore) exec(query string, args ...any) {
	g.t.Helper()
	if _, err := execSQL(g.ctx, g.store, query, args...); err != nil {
		g.t.Fatalf("%v\n%s", err, query)
	}
}

func rv(id, hash, declared string) graphEvent {
	return graphEvent{id: id, hash: hash, declared: declared}
}

func headCases() []graphCase {
	staged := rv("e2", "h2", "h1")
	staged.stage = "staged"
	suppressed := rv("e2", "h2", "h1")
	suppressed.suppressed = true
	execOnly := graphEvent{id: "f1", hash: "h9", outcome: "failed"}
	otherGeneration := rv("x1", "hx", "h1")
	otherGeneration.generation = 2
	return []graphCase{
		{name: "plain chain", events: []graphEvent{rv("e1", "h1", ""), rv("e2", "h2", "h1"), rv("e3", "h3", "h2")}, evidence: Chain, head: "e3",
			reasons: map[string]string{"e1": SupersededRevision, "e2": SupersededRevision, "e3": ""}},
		{name: "sole revision", events: []graphEvent{rv("e1", "h1", "")}, evidence: Sole, head: "e1", reasons: map[string]string{"e1": ""}},
		{name: "no revision", events: []graphEvent{execOnly}, evidence: NoRevision, reasons: map[string]string{"f1": ""}},
		{name: "fork", events: []graphEvent{rv("e1", "h1", ""), rv("e2", "h2", "h1"), rv("e3", "h3", "h1")}, evidence: Fork,
			reasons: map[string]string{"e1": "", "e2": "", "e3": ""}},
		{name: "two tips", events: []graphEvent{rv("e1", "h1", ""), rv("e2", "h2", "")}, evidence: Fork, reasons: map[string]string{"e1": "", "e2": ""}},
		{name: "cycle", events: []graphEvent{rv("e1", "h1", "h2"), rv("e2", "h2", "h1")}, evidence: Cycle, reasons: map[string]string{"e1": "", "e2": ""}},
		{name: "self loop", events: []graphEvent{rv("e1", "h1", "h1")}, evidence: Cycle, reasons: map[string]string{"e1": ""}},
		{name: "cycle behind a tail", events: []graphEvent{rv("e1", "h1", "h2"), rv("e2", "h2", "h3"), rv("e3", "h3", "h2")}, evidence: Cycle},
		{name: "cycle after chains that share a tail", events: []graphEvent{rv("e1", "h1", "h4"), rv("e2", "h2", "h4"), rv("e3", "h3", "h6"), rv("e4", "h4", "h5"), rv("e5", "h5", ""), rv("e6", "h6", "h3")}, evidence: Cycle},
		{name: "unknown predecessor", events: []graphEvent{rv("e1", "h1", ""), rv("e2", "h2", "nope")}, evidence: UnknownPredecessor, reasons: map[string]string{"e1": "", "e2": ""}},
		{name: "predecessor hash held by two revisions", events: []graphEvent{rv("e1", "h1", ""), rv("e2", "h1", ""), rv("e3", "h3", "h1")}, evidence: UnknownPredecessor},
		{name: "requested correction predecessor", current: 2, requested: true, events: []graphEvent{{id: "r1", hash: "hr", declared: "hp", generation: 2}}, evidence: Chain, head: "r1",
			reasons: map[string]string{"r1": "", "p1": StaleGeneration}},
		{name: "requested correction predecessor declared twice", current: 2, requested: true, events: []graphEvent{{id: "r1", hash: "hr1", declared: "hp", generation: 2}, {id: "r2", hash: "hr2", declared: "hp", generation: 2}}, evidence: Fork},
		{name: "requested correction predecessor also held by a revision", current: 2, requested: true, events: []graphEvent{{id: "r1", hash: "hp", generation: 2}, {id: "r2", hash: "hr2", declared: "hp", generation: 2}}, evidence: UnknownPredecessor},
		{name: "correction predecessor not requested", current: 2, events: []graphEvent{{id: "p1", hash: "hp"}, {id: "r1", hash: "hr", declared: "hp", generation: 2}}, evidence: UnknownPredecessor},
		{name: "final-stage successor", events: []graphEvent{rv("e1", "h1", ""), rv("e2", "h2", "h1")}, evidence: Chain, head: "e2", reasons: map[string]string{"e1": SupersededRevision, "e2": ""}},
		{name: "staged successor", events: []graphEvent{rv("e1", "h1", ""), staged}, evidence: Chain, head: "e2", reasons: map[string]string{"e1": "", "e2": ""}},
		{name: "stale generation", current: 2, judge: 1, events: []graphEvent{rv("e1", "h1", ""), rv("e2", "h2", "h1")}, evidence: Chain, head: "e2",
			reasons: map[string]string{"e1": StaleGeneration, "e2": StaleGeneration}},
		{name: "suppressed, execution-only and other-generation rows are not revisions", events: []graphEvent{rv("e1", "h1", ""), suppressed, execOnly, otherGeneration}, evidence: Sole, head: "e1",
			reasons: map[string]string{"e1": StaleGeneration, "e2": StaleGeneration, "f1": StaleGeneration, "x1": ""}, current: 2, judge: 1},
	}
}

func seedCases(g *graphStore) []seededCase {
	var out []seededCase
	for n, c := range headCases() {
		out = append(out, g.seed(n, c))
	}
	return out
}

func TestHeadRevisionFromMatchesTheOldJudgmentOnNamedCases(t *testing.T) {
	t.Parallel()
	g := newGraphStore(t)
	for _, c := range seedCases(g) {
		t.Run(c.name, func(t *testing.T) {
			q := g.store.Q(g.ctx)
			got, err := HeadRevisionFrom(g.ctx, q, c.rid, c.judged)
			if err != nil {
				t.Fatal(err)
			}
			want, err := legacyHeadRevisionFrom(g.ctx, q, c.rid, c.judged)
			if err != nil {
				t.Fatal(err)
			}
			if !sameAnswer(got, want) {
				t.Fatalf("the head answer changed:\nnow %s\nwas %s", dumps(got), dumps(want))
			}
			if evidence := fmt.Sprint(got.Get("evidence")); evidence != c.evidence {
				t.Fatalf("evidence %q, the case expects %q: %s", evidence, c.evidence, dumps(got))
			}
			if head, _ := got.Lookup("eventId"); c.head != "" && head != c.prefix+c.head {
				t.Fatalf("head %v, the case expects %q", head, c.prefix+c.head)
			}
		})
	}
}

func TestSupersessionReasonMatchesTheOldJudgmentOnNamedCases(t *testing.T) {
	t.Parallel()
	g := newGraphStore(t)
	for _, c := range seedCases(g) {
		t.Run(c.name, func(t *testing.T) {
			rows, err := all(g.ctx, g.store, "SELECT event_id FROM events WHERE relationship_id = ? ORDER BY event_id", c.rid)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				id := row.S("event_id")
				got, err := g.d.SupersessionReason(g.ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				want, err := g.d.legacySupersessionReason(g.ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Errorf("%s: reason %q, was %q", id, got, want)
				}
				if expected, ok := c.reasons[strings.TrimPrefix(id, c.prefix)]; ok && got != expected {
					t.Errorf("%s: reason %q, the case expects %q", id, got, expected)
				}
			}
		})
	}
}

// A grant notice is judged by the merge-turn port, not by the head; the claim path still asks
// SupersessionReason, so its answer is held to the old one before and after the grant is answered.
func TestSupersessionReasonOfAMergeTurnGrantMatchesTheOldJudgment(t *testing.T) {
	t.Parallel()
	f, m, turn, event := promotedGrant(t)
	for _, when := range []string{"promoted", "acknowledged"} {
		got, err := f.delivery.SupersessionReason(f.ctx, event)
		mustDo(t, err)
		want, err := f.delivery.legacySupersessionReason(f.ctx, event)
		mustDo(t, err)
		if got != want {
			t.Fatalf("%s grant: reason %q, was %q", when, got, want)
		}
		if when == "promoted" {
			if got != "" {
				t.Fatalf("a promoted grant is current: %q", got)
			}
			record, err := m.Turn(f.ctx, turn)
			mustDo(t, err)
			_, err = m.Acknowledge(f.ctx, turn, parent, record["grant"].(map[string]any)["grantId"].(string), "read the grant")
			mustDo(t, err)
		}
	}
}

func TestJudgeHeadMatchesTheOldJudgmentOnRandomGraphs(t *testing.T) {
	rng := rand.New(rand.NewPCG(416, 2026))
	seen := map[string]int{}
	for i := 0; i < 40000; i++ {
		n := 1 + rng.IntN(8)
		hashes := make([]string, n)
		for k := range hashes {
			hashes[k] = fmt.Sprintf("h%d", k)
			if k > 0 && rng.IntN(7) == 0 {
				hashes[k] = hashes[rng.IntN(k)]
			}
		}
		anchors := map[string][]string{}
		switch rng.IntN(4) {
		case 1:
			anchors["a1"] = []string{"p1"}
		case 2:
			anchors["a1"] = []string{"p1"}
			anchors["a2"] = []string{"p2", "p3"}
		case 3:
			anchors[hashes[rng.IntN(n)]] = []string{"p4"}
		}
		declared := make([]string, n)
		if rng.IntN(4) == 0 {
			// a clean chain through a random order, now and then starting at a requested predecessor
			order := rng.Perm(n)
			for j := 1; j < n; j++ {
				declared[order[j]] = hashes[order[j-1]]
			}
			if rng.IntN(3) == 0 {
				declared[order[0]] = "a1"
				anchors["a1"] = []string{"p1"}
			}
		} else {
			for k := range declared {
				switch r := rng.IntN(100); {
				case r < 25:
				case r < 70:
					declared[k] = hashes[rng.IntN(n)]
				case r < 80:
					declared[k] = "zz"
				case r < 90:
					declared[k] = "a1"
				default:
					declared[k] = "a2"
				}
			}
		}
		var revisions []revision
		var rows []Row
		for k := range n {
			id := fmt.Sprintf("e%02d", k)
			revisions = append(revisions, revision{id: id, hash: hashes[k], declared: declared[k]})
			var supersedes any
			if declared[k] != "" {
				supersedes = declared[k]
			}
			rows = append(rows, Row{"event_id": id, "revision_hash": hashes[k], "supersedes_hash": supersedes})
			if rng.IntN(25) == 0 {
				// the same event listed again with other values keeps its place and takes the last
				again := revision{id: id, hash: hashes[rng.IntN(n)], declared: declared[rng.IntN(n)]}
				revisions = append(revisions, again)
				var again2 any
				if again.declared != "" {
					again2 = again.declared
				}
				rows = append(rows, Row{"event_id": id, "revision_hash": again.hash, "supersedes_hash": again2})
			}
		}
		got, want := judgeHead(revisions, anchors), legacyJudge(rows, anchors)
		if !sameAnswer(got, want) {
			t.Fatalf("graph %d: revisions %+v anchors %v\nnow %s\nwas %s", i, revisions, anchors, dumps(got), dumps(want))
		}
		seen[fmt.Sprint(got.Get("evidence"))]++
	}
	for _, evidence := range []string{Sole, Chain, Fork, Cycle, UnknownPredecessor} {
		if seen[evidence] < 100 {
			t.Errorf("the generator reached %q only %d times: %v", evidence, seen[evidence], seen)
		}
	}
}

// The one difference: a lineage row filed under another relationship or generation for the same
// event id was read before (and listed the event twice, in no defined order); it is ignored now, so
// the event reads as it was stored, undeclared. The product never writes such a row.
func TestALineageRowOfAnotherGenerationIsNotTheEventsDeclaration(t *testing.T) {
	t.Parallel()
	g := newGraphStore(t)
	c := g.seed(0, graphCase{events: []graphEvent{rv("e1", "h1", ""), rv("e2", "h2", "")}})
	g.exec("INSERT INTO revision_lineage (relationship_id, execution_generation, event_id, revision_hash, supersedes_hash, declared_by, recorded_at) VALUES ('rel-elsewhere', 1, ?, 'h2', 'h1', 'child_declared', 'x')", c.prefix+"e2")
	now, err := HeadRevisionFrom(g.ctx, g.store.Q(g.ctx), c.rid, 1)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(now.Get("evidence")) != Fork {
		t.Fatalf("both revisions are undeclared tips: %s", dumps(now))
	}
	was, err := legacyHeadRevisionFrom(g.ctx, g.store.Q(g.ctx), c.rid, 1)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(was.Get("evidence")) != Chain {
		t.Fatalf("the old statement read the foreign row: %s", dumps(was))
	}
}

// plan is the EXPLAIN QUERY PLAN of a statement over the world's store (the wording is that of the
// SQLite the driver bundles, as in hotquery_test.go).
func (w *headWorld) plan(query string, args ...any) []planStep {
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

// The product never runs ANALYZE, so SQLite plans from its defaults. The statement that reads a
// generation's revisions has to find each lineage row by the whole key: the old statement scanned
// revision_lineage once per event, and the index on (relationship, generation) alone would still
// walk the generation's lineage rows once per event.
func TestHeadRevisionStatementFindsLineageByItsKey(t *testing.T) {
	t.Parallel()
	w := newHeadWorld(t, shapeChain, 200)
	args := []any{headRelationship, 1, "ready_for_review"}
	now := planText(w.plan(headRevisionSQL, args...))
	if !strings.Contains(now, "SEARCH l USING INDEX sqlite_autoindex_revision_lineage_1 (relationship_id=? AND execution_generation=? AND event_id=?)") || strings.Contains(now, "SCAN l") {
		t.Errorf("the head statement reads lineage as:\n%s", now)
	}
	was := planText(w.plan(legacyHeadRevisionSQL, args...))
	if !strings.Contains(was, "SCAN l") {
		t.Errorf("the old statement no longer scans lineage, so this test no longer shows why the join changed:\n%s", was)
	}
}
