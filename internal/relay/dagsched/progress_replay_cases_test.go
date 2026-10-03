package dagsched

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// The fixed event records of the replay tests (CRW-287): scripts that move a real store, with the real writers' rows and the real scheduler, one step at a time. After every step the
// harness reads the live view and rebuilds it the two ways the issue names, from the beginning and from every snapshot taken after an earlier step plus the events after its cursor.

// replayStep is one move of a script: a plan revision, a result that is reported, accepted or integrated, a relationship that is paused.
type replayStep struct {
	name string
	run  func()
}

// replayScenario builds a store in its initial state and returns the steps that follow.
type replayScenario struct {
	name  string
	build func(t *testing.T) (f *fixture, plan string, steps []replayStep)
}

// replaySeen records what the scripts exercised, so a test can require that every kind of event and every kind of plan change was part of the equality it proves.
type replaySeen struct {
	kinds                                     map[string]bool
	added, retired, updated, denominatorMoved bool
	planPaused, planResumed, stale, blocked   bool
	lifecycle                                 map[string]bool
	outsideWithAcceptance, outsideHoldingSlot bool
}

func newReplaySeen() *replaySeen {
	return &replaySeen{kinds: map[string]bool{}, lifecycle: map[string]bool{}}
}

func (s *replaySeen) events(events []ProgressEvent) {
	for _, e := range events {
		s.kinds[e.Kind] = true
	}
}

func (s *replaySeen) live(snap ProgressSnapshot, p Progress) {
	for _, r := range snap.Revisions {
		s.added = s.added || len(r.Added) > 0
		s.retired = s.retired || len(r.Retired) > 0
		s.updated = s.updated || len(r.Updated) > 0
		s.denominatorMoved = s.denominatorMoved || r.DenominatorChanged
	}
	if snap.Plan.PlanState == dag.LifePaused {
		s.planPaused = true
	} else if s.planPaused {
		s.planResumed = true
	}
	for _, n := range p.Nodes {
		s.stale = s.stale || n.Stage == StageStale
		s.blocked = s.blocked || n.Disposition == DispBlocked
		if n.Lifecycle != "" {
			s.lifecycle[n.Lifecycle] = true
		}
	}
	s.outsideWithAcceptance = s.outsideWithAcceptance || p.Outside.WithAcceptance > 0
	s.outsideHoldingSlot = s.outsideHoldingSlot || p.Outside.HoldingSlot > 0
}

// catchUp brings a snapshot up to the live view the way a reader does: page after page from its own cursor, applying each page, until the store has nothing more. It returns the rebuilt snapshot
// and every event it was given.
func (f *fixture) catchUp(plan string, from ProgressSnapshot, limit int) (ProgressSnapshot, []ProgressEvent) {
	f.t.Helper()
	return catchUpWith(f.t, f.sched, plan, from, limit)
}

// catchUpWith is catchUp over any scheduler (the tests also read through a store opened read-only).
func catchUpWith(t testing.TB, s *Scheduler, plan string, from ProgressSnapshot, limit int) (ProgressSnapshot, []ProgressEvent) {
	t.Helper()
	var events []ProgressEvent
	snap := from
	for pages := 0; ; pages++ {
		if pages > 2000 {
			t.Fatalf("a read with pages of %d does not end", limit)
		}
		d, err := s.ReadProgressDelta(context.Background(), plan, snap.Cursor(), limit)
		if err != nil {
			t.Fatalf("delta (pages of %d): %v", limit, err)
		}
		if len(d.Events) > limit {
			t.Fatalf("a page of %d events for a limit of %d", len(d.Events), limit)
		}
		events = append(events, d.Events...)
		if snap, err = ApplyProgressDelta(snap, d); err != nil {
			t.Fatalf("apply (pages of %d): %v", limit, err)
		}
		if !d.More {
			return snap, events
		}
	}
}

// pagesFrom reads page after page from a cursor, following only the cursors the pages return, and gives every event of every page; nothing is folded.
func (f *fixture) pagesFrom(plan string, c ProgressCursor, limit int) []ProgressEvent {
	f.t.Helper()
	var out []ProgressEvent
	for i := 0; ; i++ {
		if i > 2000 {
			f.t.Fatalf("a read with pages of %d does not end", limit)
		}
		d, err := f.sched.ReadProgressDelta(context.Background(), plan, c, limit)
		if err != nil {
			f.t.Fatalf("delta: %v", err)
		}
		out = append(out, d.Events...)
		c = d.Cursor
		if !d.More {
			return out
		}
	}
}

// assertRebuilt is the equality of the issue: the document the rebuilt snapshot projects is the live document byte for byte and carries its digest, and the rebuilt snapshot is the live one
// part by part (the revisions replayed from the log are the revisions read from the rows, the plan replayed is the plan stored, every record and the outside record are the live ones).
func assertRebuilt(t *testing.T, label string, got, live ProgressSnapshot, liveProgress Progress) {
	t.Helper()
	p, err := got.Project()
	if err != nil {
		t.Fatalf("%s: the rebuilt snapshot does not project: %v", label, err)
	}
	if p.printed() != liveProgress.printed() {
		t.Errorf("%s: the rebuilt document differs from the live one\n--- rebuilt\n%s\n--- live\n%s", label, p.printed(), liveProgress.printed())
	}
	if p.Digest != liveProgress.Digest {
		t.Errorf("%s: digest %s, live %s", label, p.Digest, liveProgress.Digest)
	}
	if !reflect.DeepEqual(got.Revisions, live.Revisions) {
		t.Errorf("%s: the revisions replayed from the log are not the revisions read from the rows\n got  %+v\n live %+v", label, got.Revisions, live.Revisions)
	}
	if !reflect.DeepEqual(got.Plan, live.Plan) {
		t.Errorf("%s: the plan replayed is not the plan stored\n got  %+v\n live %+v", label, got.Plan, live.Plan)
	}
	if len(got.Records) != len(live.Records) {
		t.Errorf("%s: %d records, live %d", label, len(got.Records), len(live.Records))
	}
	for id, r := range live.Records {
		if g, ok := got.Records[id]; !ok || !reflect.DeepEqual(g, r) {
			t.Errorf("%s: record %s = %+v, live %+v", label, id, g, r)
		}
	}
	if !reflect.DeepEqual(got.Outside, live.Outside) {
		t.Errorf("%s: outside %+v, live %+v", label, got.Outside, live.Outside)
	}
	if !got.Cursor().Equal(live.Cursor()) {
		t.Errorf("%s: cursor %s, live %s", label, got.Cursor(), live.Cursor())
	}
}

// runReplayScenario plays a script and checks, after the initial state and after every step: the rebuild from the beginning (pages of several sizes) and, from every snapshot taken after
// an earlier step, the rebuild from that snapshot and the events after its cursor.
func runReplayScenario(t *testing.T, sc replayScenario, seen *replaySeen, fromBeginning, fromSnapshots bool) {
	t.Helper()
	f, plan, steps := sc.build(t)
	type held struct {
		label string
		snap  ProgressSnapshot
	}
	var helds []held
	check := func(label string, last bool) {
		t.Helper()
		live, liveProgress, err := f.sched.ReadProgressSnapshot(context.Background(), plan)
		if err != nil {
			t.Fatalf("%s: live snapshot: %v", label, err)
		}
		seen.live(live, liveProgress)
		beginning, tails := []int{1, dag.MaxPage}, []int{2}
		if last {
			beginning, tails = []int{1, 2, 3, dag.MaxPage}, []int{1, 3, dag.MaxPage}
		}
		if !fromBeginning {
			beginning = nil
		}
		if !fromSnapshots {
			tails = nil
		}
		for _, limit := range beginning {
			got, events := f.catchUp(plan, ProgressSnapshot{}, limit)
			seen.events(events)
			assertRebuilt(t, fmt.Sprintf("%s: from the beginning, pages of %d", label, limit), got, live, liveProgress)
		}
		for _, h := range helds {
			for _, limit := range tails {
				got, events := f.catchUp(plan, h.snap, limit)
				seen.events(events)
				assertRebuilt(t, fmt.Sprintf("%s: from the snapshot after %q, pages of %d", label, h.label, limit), got, live, liveProgress)
			}
		}
		helds = append(helds, held{label, live})
	}
	check("the initial state", len(steps) == 0)
	for i, step := range steps {
		step.run()
		check(step.name, i == len(steps)-1)
	}
}

// The scripts. Each has a name that says what it is the fixed record of.

// fork/join (contract E-01): the plan of forkJoinPlan; results are accepted, one is integrated, one is only reported, the child of another reports blocked, the plan grows.
func scenarioForkJoin(t *testing.T) (*fixture, string, []replayStep) {
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	f.projectParent()
	steps := []replayStep{
		{"research and design accepted", func() {
			f.acceptNode("p1", "research", acceptOpts{})
			f.acceptNode("p1", "design", acceptOpts{})
		}},
		{"impl-a accepted and integrated", func() {
			a := f.acceptNode("p1", "impl-a", pinnedAcceptance(head1, 7))
			f.integrate(a, "owner/repo", "dev", true, true)
		}},
		{"impl-b reported", func() { f.reportNode("p1", "impl-b", acceptOpts{}) }},
		{"the child of impl-b reports blocked", func() { f.childBlocked("rel-p1-impl-b") }},
		{"the plan grows by a node and an edge", func() {
			f.putPlan("p1", 1, "p1-r2", addNode("extra", dag.NodeNonPR), addEdge("e9", "research", "extra", dag.EdgeArtifactVerified, nil))
		}},
	}
	return f, "p1", steps
}

// amend (contract E-18, E-24 and the partial changes of 8.4): through the real release path, the shared-root plan R -> A, R -> B, A -> C is settled, and then the plan is revised again and again:
// a spec changes (update_node), a node and an edge are added, an edge into a node is replaced, a node is paused by the plan (a lifecycle change moves no version), a node is retired with its edge,
// a node is replaced.
func scenarioAmend(t *testing.T) (*fixture, string, []replayStep) {
	k := newReleaseKit(t)
	invSharedRoot(k)
	next := func(request string, changes ...doc) {
		snap := k.snapshot("sr")
		k.putPlan("sr", int(snap.Revision), request, changes...)
	}
	steps := []replayStep{
		{"A retitled: A and C go stale", func() { k.invRevise("sr", "A", "sr-r2", invTitle("A, retitled")) }},
		{"a node and an edge are added", func() {
			next("sr-r3", addRelNode("D", dag.NodeNonPR), addEdge("bd", "B", "D", dag.EdgeArtifactVerified, nil))
		}},
		{"the edge into C is replaced", func() {
			next("sr-r4", doc{"op": dag.OpRetireEdge, "edge_id": "ac"}, addEdge("bc", "B", "C", dag.EdgeArtifactVerified, nil))
		}},
		{"B is paused by the plan", func() { next("sr-r5", doc{"op": dag.OpPauseNode, "node_id": "B"}) }},
		{"C is retired with its edge", func() {
			next("sr-r6", doc{"op": dag.OpRetireEdge, "edge_id": "bc"}, doc{"op": dag.OpRetireNode, "node_id": "C"})
		}},
		{"D is replaced by D2", func() {
			next("sr-r7", doc{"op": dag.OpRetireEdge, "edge_id": "bd"}, doc{"op": dag.OpReplaceNode, "node": relNode("D2", dag.NodeNonPR), "supersedes_node_id": "D"})
		}},
	}
	return k.fixture, "sr", steps
}

// pause (contract 3.2, 7.4 and CRW-281): the plan holds and ends nodes, a relationship is paused with no revision at all, the plan itself pauses and resumes.
func scenarioPause(t *testing.T) (*fixture, string, []replayStep) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("lc", 0, "lc-r1", addNode("un", dag.NodeNonPR), addNode("ar", dag.NodeNonPR), addNode("ca", dag.NodeNonPR), addNode("pa", dag.NodeNonPR),
		addNode("ru", dag.NodeNonPR), addNode("ac", dag.NodeNonPR), addNode("ad", dag.NodeNonPR))
	f.startNode("lc", "ru")
	f.holdSlotsFor("lc", "ru")
	f.acceptNode("lc", "ac", acceptOpts{})
	f.acceptNode("lc", "ad", acceptOpts{})
	steps := []replayStep{
		{"the plan cancels, archives and pauses nodes", func() {
			f.putPlan("lc", 1, "lc-r2", doc{"op": dag.OpCancelNode, "node_id": "ca"}, doc{"op": dag.OpArchiveNode, "node_id": "ar"}, doc{"op": dag.OpPauseNode, "node_id": "pa"},
				doc{"op": dag.OpPauseNode, "node_id": "ru"}, doc{"op": dag.OpCancelNode, "node_id": "ac"}, doc{"op": dag.OpPauseNode, "node_id": "ad"})
		}},
		{"the relationship of ru is paused, with no revision", func() { f.exec("UPDATE relationships SET status = 'paused' WHERE relationship_id = 'rel-lc-ru'") }},
		{"the plan pauses", func() { f.putPlan("lc", 2, "lc-r3", doc{"op": dag.OpPausePlan}) }},
		{"the plan resumes", func() { f.putPlan("lc", 3, "lc-r4", doc{"op": dag.OpResumePlan}) }},
	}
	return f, "lc", steps
}

// stale (contract E-11, E-18, E-24): a node's criteria change, then the spec of the root changes under results that were accepted on it.
func scenarioStale(t *testing.T) (*fixture, string, []replayStep) {
	k := newReleaseKit(t)
	invSharedRoot(k)
	steps := []replayStep{
		{"the criteria of B change", func() {
			k.invRevise("sr", "B", "sr-r2", func(n doc) { n["criteria_set_digest"] = dig("changed criteria of B") })
		}},
		{"the spec of R changes: everything below it is stale", func() { k.invRevise("sr", "R", "sr-r3", invTitle("R, retitled")) }},
	}
	return k.fixture, "sr", steps
}

// denominators (criterion c3 of CRW-286): nodes come and go, an accepted node and a node holding a slot leave the denominator, one is updated, one is replaced.
func scenarioDenominators(t *testing.T) (*fixture, string, []replayStep) {
	f := newFixture(t)
	f.putPlan("p", 0, "p-r1", addNode("a", dag.NodeNonPR), addNode("b", dag.NodeNonPR), addNode("c", dag.NodeNonPR),
		addEdge("ab", "a", "b", dag.EdgeArtifactVerified, nil), addEdge("bc", "b", "c", dag.EdgeArtifactVerified, nil))
	retitled := nodeDoc("a", dag.NodeNonPR)
	retitled["title"] = "a, retitled"
	steps := []replayStep{
		{"d is added", func() { f.putPlan("p", 1, "p-r2", addNode("d", dag.NodeNonPR)) }},
		{"c is accepted and d holds a slot", func() {
			f.acceptNode("p", "c", acceptOpts{})
			f.holdSlotsFor("p", "d")
		}},
		{"c is retired", func() {
			f.putPlan("p", 2, "p-r3", doc{"op": dag.OpRetireEdge, "edge_id": "bc"}, doc{"op": dag.OpRetireNode, "node_id": "c"})
		}},
		{"a is retitled", func() { f.putPlan("p", 3, "p-r4", doc{"op": dag.OpUpdateNode, "node": retitled}) }},
		{"d is replaced by d2", func() {
			f.putPlan("p", 4, "p-r5", doc{"op": dag.OpReplaceNode, "node": nodeDoc("d2", dag.NodeNonPR), "supersedes_node_id": "d"})
		}},
	}
	return f, "p", steps
}

var replayScenarios = []replayScenario{
	{"fork/join", scenarioForkJoin},
	{"amend", scenarioAmend},
	{"pause", scenarioPause},
	{"stale", scenarioStale},
	{"denominators", scenarioDenominators},
}

// runningPlan is a plan of n independent nodes whose children all run.
func runningPlan(f *fixture, plan string, n int) []string {
	f.t.Helper()
	f.projectParent()
	var changes []doc
	var ids []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("n%02d", i)
		ids = append(ids, id)
		changes = append(changes, addNode(id, dag.NodeNonPR))
	}
	f.putPlan(plan, 0, plan+"-r1", changes...)
	for _, id := range ids {
		f.startNode(plan, id)
	}
	return ids
}

// idsOf is the identity of every event of a page, in order.
func idsOf(events []ProgressEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.ID()
	}
	return out
}
