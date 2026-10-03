package dagsched

import (
	"context"
	"database/sql"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The progress view is rebuildable (CRW-287). The tests of the rebuild run the fixed event records of progress_replay_cases_test.go; the rest pin the cursor, the pages, the refusals and the
// absence of writes.

func isInvariant(err error) bool {
	var broken *InvariantError
	return errors.As(err, &broken)
}

// assertSeen requires that the scripts exercised what the equality is about: every kind of event the reader gets in this mode, and every kind of change the plan can make to the view.
func assertSeen(t *testing.T, seen *replaySeen, kinds ...string) {
	t.Helper()
	for _, kind := range kinds {
		if !seen.kinds[kind] {
			t.Errorf("no script produced a %q event: the equality says nothing about it", kind)
		}
	}
	flags := map[string]bool{
		"a node added": seen.added, "a node retired": seen.retired, "a node updated": seen.updated, "the denominator moved": seen.denominatorMoved,
		"the plan paused": seen.planPaused, "the plan resumed": seen.planResumed, "a stale node": seen.stale, "a blocked node": seen.blocked,
		"a node paused by the plan": seen.lifecycle[dag.LifePaused], "a node cancelled by the plan": seen.lifecycle[dag.LifeCancelled], "a node archived by the plan": seen.lifecycle[dag.LifeArchived],
		"an outside node with an acceptance": seen.outsideWithAcceptance, "an outside node holding a slot": seen.outsideHoldingSlot,
	}
	var missing []string
	for name, ok := range flags {
		if !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the scripts never produced %v", missing)
	}
}

// Criterion c1, replaying the events from the beginning: after the initial state and after every step of every fixed record (fork/join, amend, pause, stale, denominators), the events from the empty
// cursor, read in pages of 1, 2, 3 and all, fold into a snapshot that projects the live document byte for byte with the live digest, and that is the live snapshot part by part.
func TestProgressReplayFromTheBeginningEqualsLive(t *testing.T) {
	seen := newReplaySeen()
	for _, sc := range replayScenarios {
		t.Run(sc.name, func(t *testing.T) { runReplayScenario(t, sc, seen, true, false) })
	}
	assertSeen(t, seen, ProgressEventPlan, ProgressEventRevision, ProgressEventOutside, ProgressEventNode)
}

// Criterion c1, a snapshot plus the events after its cursor: a snapshot taken after every step is brought, by the events after its own cursor, to the live view of every later step.
func TestProgressSnapshotPlusTailEqualsLive(t *testing.T) {
	seen := newReplaySeen()
	for _, sc := range replayScenarios {
		t.Run(sc.name, func(t *testing.T) { runReplayScenario(t, sc, seen, false, true) })
	}
	assertSeen(t, seen, ProgressEventRevision, ProgressEventRemoved, ProgressEventOutside, ProgressEventNode)
}

// Criterion c2: reading on from a cursor yields every change once. From the empty cursor and from a snapshot's cursor, a store that is not moving gives one list of events (the whole read in one
// page); pages of any size are that list cut in order, with no event twice; and from a cursor in the middle of the list, the cursor a page returns, as a value and as text, the pages are the rest
// of the list: nothing is repeated and nothing is left out.
func TestProgressDeltaPagesExactlyOnce(t *testing.T) {
	for _, sc := range []replayScenario{replayScenarios[0], replayScenarios[1]} {
		t.Run(sc.name, func(t *testing.T) {
			f, plan, steps := sc.build(t)
			steps[0].run()
			held, _, err := f.sched.ReadProgressSnapshot(context.Background(), plan)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range steps[1:] {
				step.run()
			}
			for name, start := range map[string]ProgressSnapshot{"from the beginning": {}, "from a snapshot": held} {
				full := idsOf(f.pagesFrom(plan, start.Cursor(), dag.MaxPage))
				if len(full) < 4 {
					t.Fatalf("%s: only %d events: the record is too short to cut", name, len(full))
				}
				seen := map[string]bool{}
				for _, id := range full {
					if seen[id] {
						t.Errorf("%s: event %s is in the read twice", name, id)
					}
					seen[id] = true
				}
				for _, limit := range []int{1, 2, 3, 4, 7} {
					if got := idsOf(f.pagesFrom(plan, start.Cursor(), limit)); !equalStrings(got, full) {
						t.Errorf("%s: pages of %d give %v, the whole read gives %v", name, limit, got, full)
					}
				}
				for k := 1; k < len(full); k++ {
					first, err := f.sched.ReadProgressDelta(context.Background(), plan, start.Cursor(), k)
					if err != nil || len(first.Events) != k || !first.More {
						t.Fatalf("%s: a page of %d = %d events, more %v, %v", name, k, len(first.Events), first.More, err)
					}
					if folded, err := ApplyProgressDelta(start, first); err != nil || !folded.Cursor().Equal(first.Cursor) {
						t.Fatalf("%s: the cursor after %d events is not the folded snapshot's: %v", name, k, err)
					}
					parsed, err := ParseProgressCursor(first.Cursor.String())
					if err != nil || !parsed.Equal(first.Cursor) {
						t.Fatalf("%s: the cursor after %d events does not survive its text: %v", name, k, err)
					}
					want := full[k:]
					for label, cursor := range map[string]ProgressCursor{"value": first.Cursor, "text": parsed} {
						if got := idsOf(f.pagesFrom(plan, cursor, dag.MaxPage)); !equalStrings(got, want) {
							t.Errorf("%s: from the cursor after %d events (%s) the rest is %v, want %v", name, k, label, got, want)
						}
						if got := idsOf(f.pagesFrom(plan, cursor, 3)); !equalStrings(got, want) {
							t.Errorf("%s: pages of 3 from the cursor after %d events (%s) are %v, want %v", name, k, label, got, want)
						}
					}
					if got := append(idsOf(first.Events), idsOf(f.pagesFrom(plan, first.Cursor, 2))...); !equalStrings(got, full) {
						t.Errorf("%s: the first %d events and the rest are %v, the whole read is %v", name, k, got, full)
					}
				}
			}
		})
	}
}

// When the store moves between two pages the fold still ends at the live view: the cursor is what the reader holds, so a record that changed after it was read is read again (a new event, not a
// repeat) and one that did not change is never sent twice. No (event, digest) pair is delivered twice.
func TestProgressDeltaConvergesWhenTheStoreMoves(t *testing.T) {
	f, plan, steps := scenarioForkJoin(t)
	steps[0].run()
	snap := ProgressSnapshot{}
	var events []ProgressEvent
	first, err := f.sched.ReadProgressDelta(context.Background(), plan, snap.Cursor(), 3)
	if err != nil || !first.More {
		t.Fatalf("first page: more %v, %v", first.More, err)
	}
	events = append(events, first.Events...)
	if snap, err = ApplyProgressDelta(snap, first); err != nil {
		t.Fatal(err)
	}
	if _, err := snap.Project(); err == nil {
		t.Fatal("a snapshot that has only the first page projects")
	}
	before, _, err := f.sched.ReadProgressSnapshot(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps[1:] {
		step.run()
	}
	for i := 0; ; i++ {
		if i > 100 {
			t.Fatal("the read does not end")
		}
		d, err := f.sched.ReadProgressDelta(context.Background(), plan, snap.Cursor(), 3)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, d.Events...)
		if snap, err = ApplyProgressDelta(snap, d); err != nil {
			t.Fatal(err)
		}
		if !d.More {
			break
		}
	}
	live, liveProgress, err := f.sched.ReadProgressSnapshot(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	assertRebuilt(t, "after the store moved between pages", snap, live, liveProgress)
	seen := map[string]int{}
	for _, id := range idsOf(events) {
		seen[id]++
		if seen[id] > 1 {
			t.Errorf("event %s was delivered twice", id)
		}
	}
	for id, r := range before.Records {
		if after, ok := live.Records[id]; ok && after.Digest == r.Digest && seen["p1/node/"+id+"@"+r.Digest] != 1 {
			t.Errorf("record %s did not change and was delivered %d times", id, seen["p1/node/"+id+"@"+r.Digest])
		}
	}
}

// Criterion c3: a query, a snapshot, every page of every size, the fold and the projection write nothing: every row of every table of the store (the journal included) and every object of its
// schema are the same afterwards, and the same read works, with the same answer, through a store opened read-only.
func TestProgressReplayWritesNothing(t *testing.T) {
	f, plan, steps := scenarioForkJoin(t)
	for _, step := range steps {
		step.run()
	}
	ctx := context.Background()
	before, seq := dumpStore(t, f.path), journalSeq(t, f.path)
	live, liveProgress, err := f.sched.ReadProgressSnapshot(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{1, 2, 5, dag.MaxPage} {
		got, _ := f.catchUp(plan, ProgressSnapshot{}, limit)
		assertRebuilt(t, "pages of "+strconv.Itoa(limit), got, live, liveProgress)
	}
	if _, err := f.sched.ReadProgress(ctx, plan); err != nil {
		t.Fatal(err)
	}
	sameStore(t, "reading, rebuilding and querying", before, dumpStore(t, f.path))
	if after := journalSeq(t, f.path); after != seq {
		t.Errorf("the journal moved from %d to %d", seq, after)
	}

	readOnly, err := store.OpenReadOnlyStore(ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = readOnly.Close() }()
	sched := &Scheduler{Store: readOnly}
	got, _ := catchUpWith(t, sched, plan, ProgressSnapshot{}, 3)
	assertRebuilt(t, "through a read-only store", got, live, liveProgress)
	if _, _, err := sched.ReadProgressSnapshot(ctx, plan); err != nil {
		t.Fatalf("a snapshot through a read-only store: %v", err)
	}
	sameStore(t, "reading through a read-only store", before, dumpStore(t, f.path))
}

// A page cut anywhere cannot be folded into a view that looks complete: the events of a page are a prefix of the read until the page with more false, and only then does the fold carry the
// proof. The case: one revision and one node count, two relationships paused with no revision; a snapshot that took the first page only would pass the checks ProjectProgress has (the revision
// and the node count) and print a hybrid, so the fold refuses to project until it has caught up.
func TestProgressSnapshotIsNotProjectedBeforeItHasCaughtUp(t *testing.T) {
	f := newFixture(t)
	runningPlan(f, "pq", 3)
	ctx := context.Background()
	held, _, err := f.sched.ReadProgressSnapshot(ctx, "pq")
	if err != nil {
		t.Fatal(err)
	}
	f.exec("UPDATE relationships SET status = 'paused' WHERE relationship_id IN ('rel-pq-n00', 'rel-pq-n01')")
	first, err := f.sched.ReadProgressDelta(ctx, "pq", held.Cursor(), 1)
	if err != nil || len(first.Events) != 1 || !first.More {
		t.Fatalf("first page = %d events, more %v, %v", len(first.Events), first.More, err)
	}
	part, err := ApplyProgressDelta(held, first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Project(); !isInvariant(err) {
		t.Errorf("a fold that has one of two changes projects: %v", err)
	}
	hybrid, err := part.project()
	if err != nil {
		t.Fatalf("the hybrid should pass the checks ProjectProgress has, or this case proves nothing: %v", err)
	}
	live, liveProgress, err := f.sched.ReadProgressSnapshot(ctx, "pq")
	if err != nil {
		t.Fatal(err)
	}
	if hybrid.Digest == liveProgress.Digest {
		t.Fatal("the hybrid is the live view: the case did not change two records")
	}
	second, err := f.sched.ReadProgressDelta(ctx, "pq", part.Cursor(), 1)
	if err != nil || len(second.Events) != 1 || second.More {
		t.Fatalf("second page = %d events, more %v, %v", len(second.Events), second.More, err)
	}
	done, err := ApplyProgressDelta(part, second)
	if err != nil {
		t.Fatal(err)
	}
	assertRebuilt(t, "after the second page", done, live, liveProgress)
}

// A reader cut off after the plan header alone must not read the header again for ever: the cursor holds the plan, so the next page goes on to the revisions.
func TestProgressDeltaHeaderAloneDoesNotRepeat(t *testing.T) {
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	ctx := context.Background()
	first, err := f.sched.ReadProgressDelta(ctx, "p1", ProgressCursor{}, 1)
	if err != nil || len(first.Events) != 1 || first.Events[0].Kind != ProgressEventPlan || !first.More {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	if first.Cursor.PlanID != "p1" || first.Cursor.Revision != 0 {
		t.Errorf("cursor after the header = %+v", first.Cursor)
	}
	second, err := f.sched.ReadProgressDelta(ctx, "p1", first.Cursor, 1)
	if err != nil || len(second.Events) != 1 || second.Events[0].Kind != ProgressEventRevision || second.Events[0].Revision.RevisionNo != 1 {
		t.Fatalf("second page = %+v, %v", second, err)
	}
	snap, err := ApplyProgressDelta(ProgressSnapshot{}, first)
	if err != nil || !snap.Cursor().Equal(first.Cursor) {
		t.Fatalf("a snapshot with the header alone has cursor %+v: %v", snap.Cursor(), err)
	}
	got, _ := f.catchUp("p1", snap, 1)
	live, liveProgress, _ := f.sched.ReadProgressSnapshot(ctx, "p1")
	assertRebuilt(t, "after a header-only page", got, live, liveProgress)
}

// The plan's limit is 64 nodes. Replacing one keeps 64 live nodes, and a reader that holds 64 records and receives the new one before the old one is gone would hold 65: the removal comes
// first, so it never holds more than the plan's limit, and it is delivered exactly once whatever the page size; every cursor on the way survives its text and is the folded snapshot's.
func TestProgressDeltaReplacementAtTheNodeLimit(t *testing.T) {
	f := newFixture(t)
	var changes []doc
	for i := 0; i < 64; i++ {
		changes = append(changes, addNode("n"+strconv.Itoa(100+i), dag.NodeNonPR))
	}
	f.putPlan("big", 0, "big-r1", changes...)
	ctx := context.Background()
	held, _, err := f.sched.ReadProgressSnapshot(ctx, "big")
	if err != nil {
		t.Fatal(err)
	}
	if len(held.Records) != 64 {
		t.Fatalf("the plan holds %d records", len(held.Records))
	}
	f.putPlan("big", 1, "big-r2", doc{"op": dag.OpReplaceNode, "node": nodeDoc("n200", dag.NodeNonPR), "supersedes_node_id": "n163"})
	snap, removals, pages := held, 0, 0
	for {
		pages++
		if pages > 20 {
			t.Fatal("the read does not end")
		}
		d, err := f.sched.ReadProgressDelta(ctx, "big", snap.Cursor(), 1)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseProgressCursor(d.Cursor.String())
		if err != nil || !parsed.Equal(d.Cursor) {
			t.Fatalf("page %d: the cursor does not survive its text: %v", pages, err)
		}
		for _, e := range d.Events {
			if e.Kind == ProgressEventRemoved && e.NodeID == "n163" {
				removals++
			}
		}
		if snap, err = ApplyProgressDelta(snap, d); err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if len(snap.Records) > 64 || len(parsed.Records) > 64 {
			t.Errorf("page %d: the reader holds %d records", pages, len(snap.Records))
		}
		if !d.More {
			break
		}
	}
	if removals != 1 {
		t.Errorf("the removal of the replaced node was delivered %d times", removals)
	}
	live, liveProgress, err := f.sched.ReadProgressSnapshot(ctx, "big")
	if err != nil {
		t.Fatal(err)
	}
	assertRebuilt(t, "after the replacement", snap, live, liveProgress)
}

// A cursor of a reader whose log is not this one is refused: the revision it holds is not in the log (unregistered_scope), or the log has that revision with other content (revision_mismatch,
// whether the state digest or only the metadata of a revision differs). A cursor of another plan holds nothing of this one.
func TestProgressDeltaCursorGuards(t *testing.T) {
	f, plan, steps := scenarioForkJoin(t)
	for _, step := range steps {
		step.run()
	}
	ctx := context.Background()
	live, _, err := f.sched.ReadProgressSnapshot(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	head := live.Plan.Revision
	if head < 2 {
		t.Fatalf("head %d: the record is too short", head)
	}
	read := func(c ProgressCursor) error {
		_, err := f.sched.ReadProgressDelta(ctx, plan, c, 5)
		return err
	}
	if reason := refusalReason(read(live.Cursor())); reason != "" {
		t.Errorf("the live cursor is refused: %s", reason)
	}
	beyond := live.Cursor()
	beyond.Revision = head + 1
	if reason := refusalReason(read(beyond)); reason != "unregistered_scope" {
		t.Errorf("a cursor beyond the head = %q, want unregistered_scope", reason)
	}
	if _, err := f.sched.ReadProgressDelta(ctx, "no-such-plan", ProgressCursor{}, 5); refusalReason(err) != "unregistered_scope" {
		t.Errorf("an unknown plan = %v, want unregistered_scope", err)
	}
	mutations := map[string]func(s *ProgressSnapshot){
		"the state digest of a revision": func(s *ProgressSnapshot) { s.Revisions[0].StateDigest = dig("another log") },
		"the time of a revision":         func(s *ProgressSnapshot) { s.Revisions[0].RecordedAt = "2000-01-01T00:00:00.000000+00:00" },
		"the request of a revision":      func(s *ProgressSnapshot) { s.Revisions[0].RequestID = "another-request" },
		"the author of a revision":       func(s *ProgressSnapshot) { s.Revisions[0].AuthorTaskID = "another-author" },
		"the epoch of a revision":        func(s *ProgressSnapshot) { s.Revisions[0].CoordinatorEpoch = 9 },
		"the ops of a revision":          func(s *ProgressSnapshot) { s.Revisions[0].Ops = []string{"cancel_node"} },
		"an earlier revision of two":     func(s *ProgressSnapshot) { s.Revisions[0].RequestID += "-x" },
	}
	for name, mutate := range mutations {
		held := live
		held.Revisions = append([]RevisionCount(nil), live.Revisions...)
		mutate(&held)
		if reason := refusalReason(read(held.Cursor())); reason != "revision_mismatch" {
			t.Errorf("a log that differs in %s = %q, want revision_mismatch", name, reason)
		}
	}
	noDigest := live.Cursor()
	noDigest.RevisionDigest = ""
	if reason := refusalReason(read(noDigest)); reason != "revision_mismatch" {
		t.Errorf("a revision with no digest = %q, want revision_mismatch", reason)
	}
	// the derived parts of a revision are not metadata: they differ only when the fold and the rows do, which the fingerprint reports
	g := live
	g.Revisions = append([]RevisionCount(nil), live.Revisions...)
	g.Revisions[0].Nodes++
	if reason := refusalReason(read(g.Cursor())); reason != "" {
		t.Errorf("a derived count of a revision is not part of the log's identity: %q", reason)
	}
}

// Plans in one store: a snapshot of one plan given the delta of another from the empty cursor (it starts with the plan header) is reset and converges on the other plan; the delta of the other plan
// read from its own cursor, which has no header, is refused.
func TestProgressDeltaOfAnotherPlan(t *testing.T) {
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	forkJoinPlan(f, "p2")
	f.projectParent()
	f.acceptNode("p2", "research", acceptOpts{})
	ctx := context.Background()
	one, _, err := f.sched.ReadProgressSnapshot(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	two, twoProgress, err := f.sched.ReadProgressSnapshot(ctx, "p2")
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.sched.ReadProgressDelta(ctx, "p2", one.Cursor(), dag.MaxPage)
	if err != nil {
		t.Fatalf("a cursor of another plan: %v", err)
	}
	if len(d.Events) == 0 || d.Events[0].Kind != ProgressEventPlan || d.Events[0].PlanID != "p2" {
		t.Fatalf("a cursor of another plan holds nothing of this one: the read must start with the plan header, got %v", idsOf(d.Events))
	}
	got, err := ApplyProgressDelta(one, d)
	if err != nil {
		t.Fatalf("a snapshot of another plan given a delta that starts with the header: %v", err)
	}
	assertRebuilt(t, "p1 snapshot given the delta of p2", got, two, twoProgress)
	own, err := f.sched.ReadProgressDelta(ctx, "p2", two.Cursor(), dag.MaxPage)
	if err != nil {
		t.Fatal(err)
	}
	stale := own
	stale.Events = []ProgressEvent{{Kind: ProgressEventOutside, Outside: two.Outside}}
	stale.Cursor = two.Cursor()
	if _, err := ApplyProgressDelta(one, stale); !isInvariant(err) {
		t.Errorf("the delta of another plan with no header folded into a snapshot of this one: %v", err)
	}
}

// The cursor is text a reader can keep: it parses back to itself, the empty text is the beginning, and everything that is not a cursor is refused as a malformed document.
func TestProgressCursorText(t *testing.T) {
	f, plan, steps := scenarioForkJoin(t)
	steps[0].run()
	live, _, err := f.sched.ReadProgressSnapshot(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	c := live.Cursor()
	text := c.String()
	back, err := ParseProgressCursor(text)
	if err != nil || !back.Equal(c) || back.String() != text {
		t.Fatalf("the cursor does not survive its text: %v\n%s", err, text)
	}
	for _, empty := range []string{"", "  ", "{}", ProgressCursor{}.String()} {
		got, err := ParseProgressCursor(empty)
		if err != nil || !got.Equal(ProgressCursor{}) {
			t.Errorf("ParseProgressCursor(%q) = %+v, %v, want the empty cursor", empty, got, err)
		}
	}
	good := dig("x")
	bad := map[string]string{
		"not json":                          "not json",
		"null":                              "null",
		"an array":                          "[]",
		"an unknown field":                  `{"plan_id":"p","extra":1}`,
		"trailing data":                     `{"plan_id":"p"}{"plan_id":"q"}`,
		"a negative revision":               `{"plan_id":"p","revision":-1}`,
		"a revision without a digest":       `{"plan_id":"p","revision":2,"revision_digest":""}`,
		"a digest without a revision":       `{"plan_id":"p","revision":0,"revision_digest":"` + good + `"}`,
		"a revision without a plan":         `{"plan_id":"","revision":1,"revision_digest":"` + good + `"}`,
		"a digest that is not hex":          `{"plan_id":"p","revision":1,"revision_digest":"zz"}`,
		"an outside digest that is not hex": `{"plan_id":"p","outside":"zz"}`,
		"a record digest that is not hex":   `{"plan_id":"p","records":{"n":"zz"}}`,
		"a record with no id":               `{"plan_id":"p","records":{"":"` + good + `"}}`,
		"records without a plan":            `{"records":{"n":"` + good + `"}}`,
	}
	for name, text := range bad {
		if _, err := ParseProgressCursor(text); refusalReason(err) != "malformed_receipt" {
			t.Errorf("%s: %v, want malformed_receipt", name, err)
		}
	}
}

// What the fold refuses, each with its trigger: an event that does not follow the one before, a revision twice, a record whose digest is not its content, an event of another plan, a delta whose
// cursor or fingerprint is not what the fold reached; and what it leaves alone: the snapshot it was given, even when it refuses.
func TestProgressDeltaApplyGuards(t *testing.T) {
	f, plan, steps := scenarioForkJoin(t)
	for _, step := range steps {
		step.run()
	}
	ctx := context.Background()
	full, err := f.sched.ReadProgressDelta(ctx, plan, ProgressCursor{}, dag.MaxPage)
	if err != nil || full.More {
		t.Fatalf("whole read: more %v, %v", full.More, err)
	}
	var revisions []int
	for i, e := range full.Events {
		if e.Kind == ProgressEventRevision {
			revisions = append(revisions, i)
		}
	}
	if len(revisions) < 2 {
		t.Fatalf("%d revisions: the record is too short", len(revisions))
	}
	with := func(mutate func(events []ProgressEvent) []ProgressEvent) ProgressDelta {
		d := full
		d.Events = mutate(append([]ProgressEvent(nil), full.Events...))
		return d
	}
	nodeAt := -1
	for i, e := range full.Events {
		if e.Kind == ProgressEventNode {
			nodeAt = i
			break
		}
	}
	cases := map[string]ProgressDelta{
		"an event that does not follow the one before": with(func(ev []ProgressEvent) []ProgressEvent { return append(ev[:revisions[0]], ev[revisions[0]+1:]...) }),
		"a revision twice": with(func(ev []ProgressEvent) []ProgressEvent {
			return append(ev[:revisions[1]+1], append([]ProgressEvent{ev[revisions[0]]}, ev[revisions[1]+1:]...)...)
		}),
		"a record whose digest is not its content": with(func(ev []ProgressEvent) []ProgressEvent {
			r := *ev[nodeAt].Record
			r.Reading.Detail += " (changed)"
			ev[nodeAt].Record = &r
			return ev
		}),
		"an event of another plan": with(func(ev []ProgressEvent) []ProgressEvent {
			e := *ev[revisions[0]].Revision
			e.PlanID = "other"
			ev[revisions[0]].Revision = &e
			return ev
		}),
	}
	wrongCursor := full
	wrongCursor.Cursor.Revision--
	cases["a delta whose cursor is not what the fold reached"] = wrongCursor
	wrongFingerprint := full
	wrongFingerprint.Fingerprint = dig("another view")
	cases["a delta whose fingerprint is not the folded view's"] = wrongFingerprint
	held, _, err := f.sched.ReadProgressSnapshot(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	heldCursor, heldRecords := held.Cursor(), len(held.Records)
	for name, d := range cases {
		got, err := ApplyProgressDelta(ProgressSnapshot{}, d)
		if !isInvariant(err) {
			t.Errorf("%s: %v, want an InvariantError", name, err)
		}
		if got.Plan.PlanID != "" || len(got.Records) != 0 || len(got.Revisions) != 0 {
			t.Errorf("%s: a refused fold returned %+v, want the zero snapshot", name, got)
		}
		if _, err := ApplyProgressDelta(held, d); err == nil {
			t.Errorf("%s: folded into a snapshot that already holds the plan", name)
		}
	}
	if !held.complete {
		t.Fatal("the live snapshot is not complete")
	}
	if !held.Cursor().Equal(heldCursor) || len(held.Records) != heldRecords {
		t.Error("a fold changed the snapshot it was given")
	}
	// a snapshot that was never folded or never took the live view is not complete, and a refused projection says so
	for name, s := range map[string]ProgressSnapshot{"the zero snapshot": {}, "a hand-built snapshot": {Plan: held.Plan, Revisions: held.Revisions, Records: held.Records, Outside: held.Outside}} {
		if _, err := s.Project(); !isInvariant(err) {
			t.Errorf("%s projects: %v", name, err)
		}
	}
	if p, err := held.Project(); err != nil || p.Digest == "" {
		t.Errorf("the live snapshot does not project: %v", err)
	}
	// a fold into a snapshot does not alias it: the maps of the input are not written
	d, err := f.sched.ReadProgressDelta(ctx, plan, ProgressCursor{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := ApplyProgressDelta(ProgressSnapshot{}, d)
	if err != nil {
		t.Fatal(err)
	}
	before := seed.Cursor()
	d2, err := f.sched.ReadProgressDelta(ctx, plan, seed.Cursor(), dag.MaxPage)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyProgressDelta(seed, d2); err != nil {
		t.Fatal(err)
	}
	if !seed.Cursor().Equal(before) {
		t.Error("folding a delta changed the snapshot it started from")
	}
}

// Dropping the events of any one kind from a read that has them is refused by the fold (its cursor is not the delta's), so a fold that is missing a kind of event cannot report that it caught
// up. The read is taken from a snapshot after the first step of the amend script, which has all four kinds a snapshot's tail has.
func TestProgressDeltaWithAMissingKindIsRefused(t *testing.T) {
	f, plan, steps := scenarioAmend(t)
	steps[0].run()
	ctx := context.Background()
	held, _, err := f.sched.ReadProgressSnapshot(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps[1:] {
		step.run()
	}
	d, err := f.sched.ReadProgressDelta(ctx, plan, held.Cursor(), dag.MaxPage)
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, e := range d.Events {
		present[e.Kind] = true
	}
	for _, kind := range []string{ProgressEventRevision, ProgressEventRemoved, ProgressEventOutside, ProgressEventNode} {
		if !present[kind] {
			t.Fatalf("the tail has no %q event: the case proves nothing about it", kind)
		}
		cut := d
		cut.Events = nil
		for _, e := range d.Events {
			if e.Kind != kind {
				cut.Events = append(cut.Events, e)
			}
		}
		if _, err := ApplyProgressDelta(held, cut); !isInvariant(err) {
			t.Errorf("a read without its %q events folded: %v", kind, err)
		}
	}
	if _, err := ApplyProgressDelta(held, d); err != nil {
		t.Fatalf("the whole read does not fold: %v", err)
	}
}

// The plan's own state comes back with the replay: a plan paused and then resumed is an active plan in the rebuilt view (no plan_state), and a plan that is paused is paused in it.
func TestProgressReplayCarriesThePlanState(t *testing.T) {
	f, plan, steps := scenarioPause(t)
	ctx := context.Background()
	for i, step := range steps {
		step.run()
		got, _ := f.catchUp(plan, ProgressSnapshot{}, 5)
		p, err := got.Project()
		if err != nil {
			t.Fatal(err)
		}
		want := ""
		if i == 2 {
			want = dag.LifePaused
		}
		if p.PlanState != want || got.Plan.PlanState != want {
			t.Errorf("after %q the rebuilt plan state is %q (snapshot %q), want %q", step.name, p.PlanState, got.Plan.PlanState, want)
		}
		if live, _, err := f.sched.ReadProgressSnapshot(ctx, plan); err != nil || live.Plan.PlanState != want {
			t.Errorf("after %q the live plan state is %q, %v", step.name, live.Plan.PlanState, err)
		}
	}
}

// A record is keyed: a change to one relationship changes the digest of exactly its node's record, and nothing else the cursor holds.
func TestProgressRecordDigestIsKeyed(t *testing.T) {
	f := newFixture(t)
	runningPlan(f, "pq", 3)
	ctx := context.Background()
	before, _, err := f.sched.ReadProgressSnapshot(ctx, "pq")
	if err != nil {
		t.Fatal(err)
	}
	f.exec("UPDATE relationships SET status = 'paused' WHERE relationship_id = 'rel-pq-n01'")
	after, _, err := f.sched.ReadProgressSnapshot(ctx, "pq")
	if err != nil {
		t.Fatal(err)
	}
	var changed []string
	for id, r := range after.Records {
		if before.Records[id].Digest != r.Digest {
			changed = append(changed, id)
		}
	}
	if len(changed) != 1 || changed[0] != "n01" {
		t.Errorf("records that changed = %v, want n01 alone", changed)
	}
	if before.Outside.Digest != after.Outside.Digest || !before.Cursor().Equal(before.Cursor()) || before.Cursor().Equal(after.Cursor()) {
		t.Error("the cursor must move with the one record and with nothing else")
	}
}

// A change to one printed field of a record alone is exactly one node event, and the fold that takes it is the live view. The cases move one field each, in a plan whose nodes do not otherwise
// change: a link, the slot a node holds, the title. A digest that left one of them out would send no event and the fold would end at a stale view. (The acceptance id is in the digest too, but
// no case moves it alone: the detail of an accepted node names it, so a new acceptance moves two fields at once.)
func TestProgressDeltaSeesEveryPrintedFieldOfARecord(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("pf", 0, "pf-r1", addNode("a", dag.NodeNonPR), addNode("b", dag.NodeNonPR), addNode("im", dag.NodeImplementation))
	f.startNode("pf", "a")
	f.startNode("pf", "b")
	rid, event := f.seedReceived("pf", "im")
	retitled := nodeDoc("a", dag.NodeNonPR)
	retitled["title"] = "a, retitled"
	cases := []struct {
		name   string
		change func()
		want   []string // the kinds and subjects of the events, in order
	}{
		{"a link alone (a work report names a pull request)", func() { f.workReport(rid, event, 1, dig("revision "+rid), 11, progHead2) }, []string{"pf/node/im"}},
		{"the slot alone", func() { f.holdSlotsFor("pf", "b") }, []string{"pf/node/b"}},
		{"the title alone", func() { f.putPlan("pf", 1, "pf-r2", doc{"op": dag.OpUpdateNode, "node": retitled}) }, []string{"pf/revision/2", "pf/node/a"}},
	}
	ctx := context.Background()
	for _, c := range cases {
		held, _, err := f.sched.ReadProgressSnapshot(ctx, "pf")
		if err != nil {
			t.Fatal(err)
		}
		c.change()
		d, err := f.sched.ReadProgressDelta(ctx, "pf", held.Cursor(), dag.MaxPage)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range d.Events {
			id := e.ID()
			got = append(got, id[:strings.Index(id, "@")])
		}
		if !equalStrings(got, c.want) {
			t.Errorf("%s: the events are %v, want %v", c.name, got, c.want)
		}
		live, liveProgress, err := f.sched.ReadProgressSnapshot(ctx, "pf")
		if err != nil {
			t.Fatal(err)
		}
		folded, err := ApplyProgressDelta(held, d)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		assertRebuilt(t, c.name, folded, live, liveProgress)
	}
}

// A record that returns to an earlier state is delivered again and folded: the ID of the event that brings it back is the earlier state's ID, which is why an ID counts deliveries inside one
// read and never deduplicates across reads, and the cursor is what says what a reader holds. The case: a relationship paused and resumed between reads. Node ids are plan-local, so the IDs
// of two plans with the same node names are different.
func TestProgressDeltaStateCycleIsDeliveredEveryTime(t *testing.T) {
	f := newFixture(t)
	runningPlan(f, "pq", 2)
	ctx := context.Background()
	first, _ := f.catchUp("pq", ProgressSnapshot{}, dag.MaxPage)
	initial := first.Records["n00"].Digest
	idOf := func(snap ProgressSnapshot, id string) string {
		return ProgressEvent{Kind: ProgressEventNode, PlanID: "pq", NodeID: id, Record: &ProgressRecord{Digest: snap.Records[id].Digest}}.ID()
	}
	snap := first
	for _, status := range []string{"paused", "active"} {
		f.exec("UPDATE relationships SET status = ? WHERE relationship_id = 'rel-pq-n00'", status)
		d, err := f.sched.ReadProgressDelta(ctx, "pq", snap.Cursor(), dag.MaxPage)
		if err != nil || len(d.Events) != 1 || d.Events[0].Kind != ProgressEventNode || d.Events[0].NodeID != "n00" {
			t.Fatalf("after %s: events %v, %v", status, idsOf(d.Events), err)
		}
		if snap, err = ApplyProgressDelta(snap, d); err != nil {
			t.Fatalf("after %s: %v", status, err)
		}
		live, liveProgress, err := f.sched.ReadProgressSnapshot(ctx, "pq")
		if err != nil {
			t.Fatal(err)
		}
		assertRebuilt(t, "after "+status, snap, live, liveProgress)
	}
	if snap.Records["n00"].Digest != initial {
		t.Fatal("the record did not return to its first state: the case does not cycle")
	}
	if got, want := idOf(snap, "n00"), idOf(first, "n00"); got != want {
		t.Errorf("the returning event is %s, the first state's is %s: they are expected to be the same ID", got, want)
	}

	forkJoinPlan(f, "p1")
	forkJoinPlan(f, "p2")
	ids := map[string]string{}
	for _, plan := range []string{"p1", "p2"} {
		for _, e := range f.pagesFrom(plan, ProgressCursor{}, dag.MaxPage) {
			if e.Kind == ProgressEventNode && e.NodeID == "design" {
				ids[plan] = e.ID()
			}
		}
	}
	if ids["p1"] == "" || ids["p1"] == ids["p2"] {
		t.Errorf("the same node of two plans has the IDs %q and %q: node ids are plan-local", ids["p1"], ids["p2"])
	}
}

// The readers' entry points join a caller's transaction the way Progress does: inside Compose with the caller's querier, and in a plain transaction ReadProgressDelta is refused as a nested one.
func TestProgressDeltaInsideCallersTransaction(t *testing.T) {
	f, plan, steps := scenarioForkJoin(t)
	steps[0].run()
	ctx := context.Background()
	want, wantProgress, err := f.sched.ReadProgressSnapshot(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	err = f.s.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		d, err := f.sched.ProgressDelta(txCtx, f.s.Q(txCtx), plan, ProgressCursor{}, dag.MaxPage)
		if err != nil {
			return err
		}
		got, err := ApplyProgressDelta(ProgressSnapshot{}, d)
		if err != nil {
			return err
		}
		assertRebuilt(t, "inside the caller's transaction", got, want, wantProgress)
		snap, _, err := f.sched.ProgressSnapshotOf(txCtx, f.s.Q(txCtx), plan)
		if err != nil {
			return err
		}
		if !snap.Cursor().Equal(want.Cursor()) {
			t.Error("a snapshot inside the caller's transaction has another cursor")
		}
		joined, err := f.sched.ReadProgressDelta(txCtx, plan, ProgressCursor{}, dag.MaxPage)
		if err != nil {
			return err
		}
		if joined.Fingerprint != d.Fingerprint {
			t.Error("ReadProgressDelta inside Compose reads another view")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = f.s.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		_, err := f.sched.ReadProgressDelta(txCtx, plan, ProgressCursor{}, 5)
		return err
	})
	if !errors.Is(err, store.ErrNestedTransaction) {
		t.Errorf("ReadProgressDelta inside a plain transaction = %v, want store.ErrNestedTransaction", err)
	}
}

// A guard, not the proof of criterion c3 (that is the behaviour above): the library file imports nothing that reaches outside the process or the command layer.
func TestProgressReplaySourceImportsGuard(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "progress_replay.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{"os": true, "os/exec": true, "net": true, "net/http": true, "io/ioutil": true, "time": true,
		modulePrefix + "internal/relay/dispatch": true, modulePrefix + "internal/relay/daemon": true, modulePrefix + "internal/relay/service": true}
	for _, imp := range file.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if banned[path] {
			t.Errorf("progress_replay.go imports %s", path)
		}
	}
}

// The page names every entry point and every kind of event of the rebuild, so it cannot drift from the code.
func TestProgressReplayPageNamesTheAPI(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/relay/dag-progress.md")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, want := range []string{"ProgressDelta", "ReadProgressDelta", "ProgressSnapshotOf", "ReadProgressSnapshot", "ApplyProgressDelta", "ProgressSnapshot", "ProgressCursor", "ParseProgressCursor", "Project",
		ProgressEventPlan, ProgressEventRevision, ProgressEventRemoved, ProgressEventOutside, ProgressEventNode, "revision_mismatch", "unregistered_scope", "malformed_receipt"} {
		if !strings.Contains(page, "`"+want) {
			t.Errorf("docs/relay/dag-progress.md does not name %s", want)
		}
	}
	plans, err := os.ReadFile("../../../docs/relay/dag-plans.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plans), "dag-progress.md") {
		t.Error("docs/relay/dag-plans.md does not point to the progress page from its section on events, cursors and snapshots")
	}
}
