package dagsched

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/capacity"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// CRW-281: pause, resume, cancel and archive are typed plan revisions (the changes of the revision document), and the scheduler reads
// them. The tests drive the real store, the real plan log and the real scheduler; a revision is written exactly as an operator
// writes one (dag-plan-put's document), and the reasons are the literals the documentation promises.

// lifeOp is a lifecycle change of the revision document: one node's, or (with no node) the plan's.
func lifeOp(op, node string) doc {
	d := doc{"op": op}
	if node != "" {
		d["node_id"] = node
	}
	return d
}

func planOp(op string) doc { return lifeOp(op, "") }

// lifeLog appends lifecycle revisions to a plan: each call is the next revision, on the head the plan has.
type lifeLog struct {
	t    *testing.T
	f    *fixture
	plan string
	rev  int
}

func (l *lifeLog) put(changes ...doc) {
	l.t.Helper()
	l.rev++
	l.f.putPlan(l.plan, l.rev-1, fmt.Sprintf("%s-life-%d", l.plan, l.rev), changes...)
}

// try is a revision that may be refused: the head moves only when it was accepted.
func (l *lifeLog) try(changes ...doc) error {
	l.t.Helper()
	cs := make([]any, len(changes))
	for i, c := range changes {
		cs[i] = c
	}
	raw, err := json.Marshal(doc{"schema": dag.SchemaRevision, "plan_id": l.plan, "project_key": "P-TEST", "request_id": fmt.Sprintf("%s-try-%d", l.plan, l.rev+1),
		"expected_parent_revision": l.rev, "author_task_id": "task-test", "changes": cs})
	if err != nil {
		l.t.Fatal(err)
	}
	rev, err := dag.DecodeRevision(raw)
	if err != nil {
		return err
	}
	if _, err := l.f.repo.Put(context.Background(), rev); err != nil {
		return err
	}
	l.rev++
	return nil
}

// lifecycleOf is the "lifecycle" key of a node's reading, "" when the reading carries none (an active node).
func lifecycleOf(n NodeReading) string {
	for _, field := range n.object() {
		if field.Key == "lifecycle" {
			s, _ := field.Value.(string)
			return s
		}
	}
	return ""
}

// rejectedRules are the rule codes of a refused revision.
func rejectedRules(err error) []string {
	var rejected *dag.PlanRejected
	if !errors.As(err, &rejected) {
		return nil
	}
	var rules []string
	for _, v := range rejected.Violations {
		rules = append(rules, v.Rule)
	}
	return rules
}

// c1: a paused node, and every node of a paused plan, is not offered and says why; the resume offers it again.
func TestAPausedNodeOrPlanIsNotOfferedAndResumeOffersItAgain(t *testing.T) {
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	f.projectParent()
	l := &lifeLog{t: t, f: f, plan: "p1", rev: 1}
	offered := []string{"research"}
	if got := f.read("p1").readyIDs(); !reflect.DeepEqual(got, offered) {
		t.Fatalf("before any pause the ready set is %v, want %v", got, offered)
	}

	l.put(lifeOp("pause_node", "research"))
	paused := f.read("p1")
	n := paused.node("research")
	if len(paused.readyIDs()) != 0 || n.Disposition != DispDefer || n.Reason != "defer:node_paused" || n.State != "paused" || lifecycleOf(n) != "paused" {
		t.Fatalf("a paused node reads %+v (lifecycle %q), ready %v", n, lifecycleOf(n), paused.readyIDs())
	}
	if !ReasonsClosed(n.Reason) {
		t.Fatalf("%s is not in the closed vocabulary", n.Reason)
	}
	if d := paused.node("design"); d.Reason != WaitEdge("e0") || lifecycleOf(d) != "" {
		t.Fatalf("the pause names one node, yet design reads %+v", d)
	}

	l.put(lifeOp("resume_node", "research"))
	back := f.read("p1")
	if got := back.readyIDs(); !reflect.DeepEqual(got, offered) {
		t.Fatalf("after the resume the ready set is %v, want %v", got, offered)
	}
	if r := back.node("research"); r.Reason != "" || lifecycleOf(r) != "" || r.State != StateReady {
		t.Fatalf("a resumed node reads %+v (lifecycle %q)", r, lifecycleOf(r))
	}

	l.put(planOp("pause_plan"))
	stopped := f.read("p1")
	if len(stopped.readyIDs()) != 0 {
		t.Fatalf("a paused plan offers %v", stopped.readyIDs())
	}
	for _, node := range stopped.Nodes {
		if node.Disposition != DispDefer || node.Reason != "defer:plan_paused" || !ReasonsClosed(node.Reason) {
			t.Errorf("node %s of a paused plan reads %+v", node.NodeID, node)
		}
	}

	l.put(planOp("resume_plan"))
	if got := f.read("p1").readyIDs(); !reflect.DeepEqual(got, offered) {
		t.Fatalf("after the plan resumed the ready set is %v, want %v", got, offered)
	}
}

// c1: a release of a paused node or of a node of a paused plan is refused, writes nothing and starts no child; the resume releases it.
func TestReleaseRefusesAPausedNodeAndAPausedPlan(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		pause, back  doc
	}{
		{"a paused node", "defer:node_paused", lifeOp("pause_node", "A"), lifeOp("resume_node", "A")},
		{"a paused plan", "defer:plan_paused", planOp("pause_plan"), planOp("resume_plan")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			l := &lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}
			l.put(tc.pause)
			_, err := k.release("rp", "A")
			if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("release = %v, want disposition_conflict naming %s", err, tc.reason)
			}
			if k.rows() != (rowCounts{}) {
				t.Fatalf("a refused release wrote %+v", k.rows())
			}
			if created, sent := k.host.counts(); created != 0 || sent != 0 {
				t.Fatalf("a refused release created %d and sent %d", created, sent)
			}
			l.put(tc.back)
			if res, err := k.release("rp", "A"); err != nil || !res.Bound {
				t.Fatalf("release after the resume = %v %+v", err, res)
			}
		})
	}
}

// c2: nothing downstream of a cancelled or archived node is released, and a cancel is never shown as reverted or done.
func TestDescendantsOfACancelledOrArchivedNodeAreNeverReleased(t *testing.T) {
	for _, tc := range []struct {
		name, op, life, own, descendant string
	}{
		{"cancel", "cancel_node", "cancelled", "skip:node_cancelled", "blocked:predecessor_cancelled"},
		{"archive", "archive_node", "archived", "skip:node_archived", "blocked:predecessor_archived"},
	} {
		for _, accepted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, accepted before %v", tc.name, accepted), func(t *testing.T) {
				k := newReleaseKit(t)
				releasePlan(k.fixture, "rp")
				l := &lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}
				// A -> B -> C: the grandchild waits on B, which is blocked by its cancelled or archived predecessor
				l.put(addRelNode("C", dag.NodeNonPR), addEdge("bc", "B", "C", dag.EdgeArtifactVerified, nil))
				if accepted {
					k.acceptNode("rp", "A", acceptOpts{})
				}
				l.put(lifeOp(tc.op, "A"))
				reading := k.read("rp")
				a, b, c := reading.node("A"), reading.node("B"), reading.node("C")
				if a.Reason != tc.own || lifecycleOf(a) != tc.life || strings.HasPrefix(a.Reason, "done:") || a.Disposition == DispReady {
					t.Fatalf("A reads %+v (lifecycle %q), want %s and never done", a, lifecycleOf(a), tc.own)
				}
				if b.Reason != tc.descendant || b.Disposition != DispBlocked || c.Reason != WaitEdge("bc") {
					t.Fatalf("B reads %+v and C reads %+v, want %s and %s", b, c, tc.descendant, WaitEdge("bc"))
				}
				for _, id := range reading.readyIDs() {
					if id == "A" || id == "B" || id == "C" {
						t.Fatalf("%s is offered although its ancestor is %s", id, tc.life)
					}
				}
				for _, node := range []string{"A", "B", "C"} {
					before := k.rows()
					if _, err := k.release("rp", node); refusalReason(err) != "disposition_conflict" {
						t.Fatalf("release of %s = %v, want disposition_conflict", node, err)
					}
					if k.rows() != before {
						t.Fatalf("a refused release of %s wrote rows", node)
					}
				}
				if created, _ := k.host.counts(); created != 0 {
					t.Fatalf("%d children were created below a %s node", created, tc.life)
				}
			})
		}
	}
}

// c2: a cancelled node is never resumed, reopened, cancelled again or archived: the plan has no revision that reads as undoing a cancel, and an
// archived node is as final. Starting the work again is a new node (replace_node).
func TestACancelIsNeverReverted(t *testing.T) {
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	f.projectParent()
	l := &lifeLog{t: t, f: f, plan: "p1", rev: 1}
	l.put(lifeOp("cancel_node", "research"), lifeOp("pause_node", "design"), lifeOp("archive_node", "join"))
	head := l.rev
	for _, c := range []struct{ node, op string }{
		{"research", "resume_node"}, {"research", "pause_node"}, {"research", "cancel_node"}, {"research", "archive_node"},
		{"join", "resume_node"}, {"join", "pause_node"}, {"join", "cancel_node"}, {"join", "archive_node"},
	} {
		err := l.try(lifeOp(c.op, c.node))
		if got := rejectedRules(err); !reflect.DeepEqual(got, []string{"invalid_lifecycle_transition"}) || refusalReason(err) != "malformed_receipt" {
			t.Errorf("%s of the ended node %s = %v (rules %v)", c.op, c.node, err, got)
		}
	}
	if l.rev != head {
		t.Fatal("a refused revision moved the head")
	}
	if n := f.read("p1").node("research"); n.Reason != "skip:node_cancelled" || lifecycleOf(n) != "cancelled" {
		t.Fatalf("research reads %+v after the refused resume", n)
	}
	// a node that was only paused can still be cancelled
	l.put(lifeOp("cancel_node", "design"))
	if n := f.read("p1").node("design"); lifecycleOf(n) != "cancelled" {
		t.Fatalf("design reads %+v", n)
	}
}

// c2: a landed merge is a fact the plan cannot take back: the node stays done, its integrated edge stays satisfied, and only what rested on its
// unlanded artifact edges is blocked.
func TestCancellingALandedNodeDoesNotRevertTheLanding(t *testing.T) {
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	f.projectParent()
	a := f.acceptNode("p1", "impl-a", acceptOpts{HeadSHA: head1, PR: 7, Forge: "owner/repo", Repository: "owner/repo"})
	f.integrate(a, "owner/repo", "dev", true, true)
	l := &lifeLog{t: t, f: f, plan: "p1", rev: 1}
	l.put(lifeOp("cancel_node", "impl-a"))
	reading := f.read("p1")
	if n := reading.node("impl-a"); n.Reason != DoneIntegrated || n.State != StateIntegrated || lifecycleOf(n) != "cancelled" {
		t.Fatalf("a cancelled node that had landed reads %+v (lifecycle %q)", n, lifecycleOf(n))
	}
	if st := f.status("p1", "e3"); !st.Satisfied {
		t.Fatalf("the integrated edge of a landed node = %+v", st)
	}
	if n := reading.node("stack2"); n.Reason != BlockedPredecessorCancelled {
		t.Fatalf("the artifact edge out of a cancelled node reads %+v, want %s", n, BlockedPredecessorCancelled)
	}
}

// c3: a child that reports after its node was paused (or the plan was) has its result recorded, nothing accepts it while the pause lasts and no
// successor is released; the resume lets the parent accept it and only then does the successor become ready.
func TestAChildThatReportsAfterAPauseReleasesNoSuccessor(t *testing.T) {
	for _, tc := range []struct {
		name, reason, successor string
		pause, back             doc
	}{
		// the node pause holds the node and nothing else: its successor waits for the acceptance that cannot come; the plan pause holds every node
		{"a paused node", "defer:node_paused", WaitEdge("ab"), lifeOp("pause_node", "A"), lifeOp("resume_node", "A")},
		{"a paused plan", "defer:plan_paused", "defer:plan_paused", planOp("pause_plan"), planOp("resume_plan")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			l := &lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}
			l.put(tc.pause)
			// the child reports, verified: the relay records it as it records every report
			r := k.reportNode("rp", "A", acceptOpts{})
			if got := k.count("SELECT COUNT(*) FROM events WHERE relationship_id = ? AND stage = 'final' AND outcome = 'ready_for_review'", r.Acceptance.RelationshipID); got != 1 {
				t.Fatalf("the report is recorded %d times", got)
			}
			_, err := k.accept("rp", "A", AcceptInput{})
			if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("accept = %v, want disposition_conflict naming %s", err, tc.reason)
			}
			if k.acceptCount() != 0 {
				t.Fatal("a result reported after the pause was accepted")
			}
			reading := k.read("rp")
			if b := reading.node("B"); b.Reason != tc.successor || b.Disposition == DispReady {
				t.Fatalf("B reads %+v: the report released its successor", b)
			}
			if got := reading.readyIDs(); len(got) != 0 && tc.name == "a paused plan" {
				t.Fatalf("a paused plan offers %v", got)
			}
			if a := reading.node("A"); a.Reason != tc.reason || a.State != StateVerifying {
				t.Fatalf("A reads %+v, want its relay state and %s", a, tc.reason)
			}
			l.put(tc.back)
			if res, err := k.accept("rp", "A", AcceptInput{}); err != nil || res.AcceptanceID == "" {
				t.Fatalf("accept after the resume = %v %+v", err, res)
			}
			if got := k.read("rp").node("B"); got.Reason != "" || got.Disposition != DispReady {
				t.Fatalf("B after the acceptance reads %+v, want ready", got)
			}
		})
	}
}

// c3: an acceptance recorded before a pause is a value the pause does not touch (contract 7.4): it stays active and the node reads done, with the
// pause beside it, and it is neither revoked nor restated.
func TestAPauseDoesNotInvalidateAnAcceptance(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.reportNode("rp", "A", acceptOpts{})
	first, err := k.accept("rp", "A", AcceptInput{})
	if err != nil {
		t.Fatal(err)
	}
	l := &lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}
	l.put(lifeOp("pause_node", "A"))
	a := k.read("rp").node("A")
	if a.Reason != DoneAccepted || lifecycleOf(a) != "paused" {
		t.Fatalf("an accepted node that was paused reads %+v (lifecycle %q)", a, lifecycleOf(a))
	}
	if k.count("SELECT COUNT(*) FROM dag_acceptances WHERE acceptance_id = ? AND state = 'active'", first.AcceptanceID) != 1 {
		t.Fatal("the pause changed the acceptance")
	}
}

// c4 (contract 7.4): a pause, and a cancel, keep the execution slot until an explicit slot-release; the scheduler keeps counting it.
func TestPauseAndCancelKeepTheExecutionSlot(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.declareLimit("project", "P-TEST", "runs", 1)
	k.mustRelease("rp", "A")
	slot := func() int {
		return k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_key = 'rp/A' AND state = 'held'")
	}
	if slot() != 1 || k.read("rp").node("I").Reason != DeferNoCapacity {
		t.Fatalf("before any lifecycle change: held %d, I reads %+v", slot(), k.read("rp").node("I"))
	}
	l := &lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}
	for _, step := range []doc{lifeOp("pause_node", "A"), lifeOp("cancel_node", "A")} {
		l.put(step)
		if slot() != 1 {
			t.Fatalf("%v released the slot", step["op"])
		}
		if i := k.read("rp").node("I"); i.Reason != DeferNoCapacity {
			t.Fatalf("after %v the slot of A no longer counts: I reads %+v", step["op"], i)
		}
	}
	if _, err := (&capacity.Capacity{Store: k.s, Now: k.clock}).Release(context.Background(), capacity.Release{SubjectKind: SlotSubjectKind, SubjectKey: SlotSubjectKey("rp", "A"), ReleasedBy: "parent", Reason: "operator"}); err != nil {
		t.Fatal(err)
	}
	if i := k.read("rp").node("I"); i.Reason != "" || i.Disposition != DispReady {
		t.Fatalf("after the explicit slot-release I reads %+v, want ready", i)
	}
}

// A node the plan paused or ended keeps what the relay says of its execution, except where that would hide something the operator must act on:
// a running child stays running with the pause as its reason; a creation of unknown outcome and a landed merge keep their own reasons.
func TestTheReadingOfAnOwnedNodeKeepsWhatNeedsAction(t *testing.T) {
	t.Run("a running child", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.mustRelease("rp", "A")
		l := &lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}
		l.put(lifeOp("pause_node", "A"))
		if a := k.read("rp").node("A"); a.State != StateRunning || a.Reason != "defer:node_paused" || lifecycleOf(a) != "paused" {
			t.Fatalf("A = %+v", a)
		}
		l.put(lifeOp("cancel_node", "A"))
		if a := k.read("rp").node("A"); a.State != StateRunning || a.Reason != "skip:node_cancelled" || lifecycleOf(a) != "cancelled" {
			t.Fatalf("A after the cancel = %+v: its child still runs until the relationship is cancelled", a)
		}
	})
	t.Run("a creation whose outcome is unknown", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.host.loseFirstCreation = true
		if res, err := k.release("rp", "A"); err != nil || res.Bound {
			t.Fatalf("release = %v %+v", err, res)
		}
		l := &lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}
		l.put(lifeOp("pause_node", "A"))
		if a := k.read("rp").node("A"); a.Reason != BlockedCreationUnknown || lifecycleOf(a) != "paused" {
			t.Fatalf("A = %+v", a)
		}
	})
}

// c4: a release whose intent was recorded and whose child is not bound yet is a continuation of an earlier release, and it is refused while the node is
// paused or ended: nothing is created for a node the plan stopped, and the resume lets the same request continue.
func TestAFrozenReleaseIsNotContinuedForAPausedNode(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.host.loseFirstCreation = true
	if res, err := k.release("rp", "A"); err != nil || res.Bound {
		t.Fatalf("release = %v %+v", err, res)
	}
	l := &lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}
	l.put(lifeOp("pause_node", "A"))
	_, sentBefore := k.host.counts()
	if _, err := k.release("rp", "A"); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "defer:node_paused") {
		t.Fatalf("replay while paused = %v", err)
	}
	if _, sent := k.host.counts(); sent != sentBefore || k.count("SELECT COUNT(*) FROM dag_node_executions") != 0 {
		t.Fatal("a child was bound for a paused node")
	}
	l.put(lifeOp("resume_node", "A"))
	if res, err := k.release("rp", "A"); err != nil || !res.Bound || !res.Replayed {
		t.Fatalf("replay after the resume = %v %+v", err, res)
	}
	if created, _ := k.host.counts(); created != 1 {
		t.Fatalf("%d children were created for one release", created)
	}
	// an already bound release answers as it always did: a retry of a call that succeeded is not a new release
	l.put(lifeOp("pause_node", "A"))
	if res, err := k.release("rp", "A"); err != nil || !res.Bound || !res.Replayed {
		t.Fatalf("the replay of a bound release while paused = %v %+v", err, res)
	}
}

// lifecycleCases are the four ways the plan can hold a node: it paused the node, cancelled it, archived it, or paused itself.
type lifecycleCase struct {
	name, reason string
	archived     bool // an archived node is refused the work but not the landing of a pull request it already had accepted
	change       func(node string) doc
}

func lifecycleCases() []lifecycleCase {
	return []lifecycleCase{
		{"a paused node", "defer:node_paused", false, func(n string) doc { return lifeOp("pause_node", n) }},
		{"a cancelled node", "skip:node_cancelled", false, func(n string) doc { return lifeOp("cancel_node", n) }},
		{"an archived node", "skip:node_archived", true, func(n string) doc { return lifeOp("archive_node", n) }},
		{"a paused plan", "defer:plan_paused", false, func(string) doc { return planOp("pause_plan") }},
	}
}

func refusedFor(t *testing.T, err error, reason string) {
	t.Helper()
	if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), reason) {
		t.Fatalf("got %v, want disposition_conflict naming %s", err, reason)
	}
}

// The work on a node (accepting its result, correcting it) is refused for a paused, cancelled or archived node and for a node of a paused plan, before anything is written.
func TestWorkOnAPausedOrEndedNodeIsRefusedAndWritesNothing(t *testing.T) {
	for _, c := range lifecycleCases() {
		t.Run(c.name+": accepting", func(t *testing.T) {
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			k.reportNode("rp", "A", acceptOpts{})
			(&lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}).put(c.change("A"))
			_, err := k.accept("rp", "A", AcceptInput{})
			refusedFor(t, err, c.reason)
			if k.acceptCount() != 0 {
				t.Fatal("a row was written")
			}
		})
		t.Run(c.name+": correcting", func(t *testing.T) {
			k := newReleaseKit(t)
			rid := k.correctionKit()
			k.openCorrection(rid, nil)
			(&lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}).put(c.change("A"))
			_, err := k.sched.PrepareCorrection(context.Background(), "rp", "A", "parent", ManifestInput{RuleVersion: k.request(false).RuleVersion}, VerifyOptions{ArtifactRoots: []string{k.root}})
			refusedFor(t, err, c.reason)
			_, err = k.correct("")
			refusedFor(t, err, c.reason)
			if k.count("SELECT COUNT(*) FROM dag_node_executions") != 1 {
				t.Fatal("a correction was bound")
			}
		})
	}
}

// The landing of an accepted pull request (judging it for merge, asking the merge lane for a turn, observing where it landed) is refused for a paused or cancelled node and for a node of a
// paused plan, as it is for a paused or cancelled relationship.
func TestLandingOfAPausedOrCancelledNodeIsRefused(t *testing.T) {
	for _, c := range lifecycleCases() {
		if c.archived {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			k := newJudgeKit(t)
			(&lifeLog{t: t, f: k.fixture, plan: "g", rev: int(k.snapshot("g").Revision)}).put(c.change("I"))
			ctx := context.Background()
			_, err := k.sched.Judge(ctx, "g", "I", "parent", JudgeInput{})
			refusedFor(t, err, c.reason)
			_, _, err = k.sched.RequestMergeTurn(ctx, "g", "I", "parent", MergeRequestInput{Host: "host"})
			refusedFor(t, err, c.reason)
			_, err = k.sched.ObserveIntegration(ctx, "g", "I", "parent", []Target{{Repository: k.repo.path, BaseRef: "dev"}})
			refusedFor(t, err, c.reason)
			if got := k.count("SELECT COUNT(*) FROM dag_merge_checks") + k.count("SELECT COUNT(*) FROM dag_integration_observations") + k.count("SELECT COUNT(*) FROM merge_turns"); got != 0 {
				t.Fatalf("%d rows were written", got)
			}
		})
	}
}

// An archived node has put its work down, but a pull request it already had accepted still has to land, as it does for an archived relationship: the judgement, the merge turn and the
// observation go through, and the work (accepting, correcting, releasing) does not.
func TestAnArchivedNodeStillLands(t *testing.T) {
	k := newJudgeKit(t)
	(&lifeLog{t: t, f: k.fixture, plan: "g", rev: int(k.snapshot("g").Revision)}).put(lifeOp("archive_node", "I"))
	ctx := context.Background()
	res, err := k.sched.Judge(ctx, "g", "I", "parent", JudgeInput{})
	if err != nil || !res.Eligible() {
		t.Fatalf("judge = %v %+v", err, res)
	}
	if _, turn, err := k.sched.RequestMergeTurn(ctx, "g", "I", "parent", MergeRequestInput{Host: "host"}); err != nil || turn == nil {
		t.Fatalf("request = %v %v", err, turn)
	}
	observed, err := k.sched.ObserveIntegration(ctx, "g", "I", "parent", []Target{{Repository: k.repo.path, BaseRef: "dev"}})
	if err != nil || len(observed.Observations) != 1 {
		t.Fatalf("observe = %v %+v", err, observed)
	}
	if n := k.read("g").node("I"); n.Reason != "skip:node_archived" || lifecycleOf(n) != "archived" {
		t.Fatalf("I reads %+v", n)
	}
	k.sched.Tips = &tips{sha: head1} // a release reads the tip its child would start from before it reads the plan
	if _, err := k.release("g", "I"); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("a release of an archived node = %v", err)
	}
}

// A pause that lands after a command has read the plan and before its transaction commits is caught inside the transaction: the slice and criteria digests that transaction already compares
// do not move for a pause, so each transaction asks the lifecycle again. Each case pauses from the seam between the unlocked read and the transaction and checks that the seam fired.
func TestAPauseThatLandsBetweenTheReadAndTheTransactionIsCaught(t *testing.T) {
	const reason = "defer:node_paused"
	t.Run("accepting", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.reportNode("rp", "A", acceptOpts{})
		l := &lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}
		fired := false
		k.sched.testBeforeAcceptTx = func() { fired = true; l.put(lifeOp("pause_node", "A")) }
		_, err := k.accept("rp", "A", AcceptInput{})
		if !fired {
			t.Fatal("the seam did not fire")
		}
		refusedFor(t, err, reason)
		if k.acceptCount() != 0 {
			t.Fatal("a row was written")
		}
	})
	t.Run("judging", func(t *testing.T) {
		k := newJudgeKit(t)
		l := &lifeLog{t: t, f: k.fixture, plan: "g", rev: int(k.snapshot("g").Revision)}
		fired := false
		k.sched.testBeforeJudgeTx = func() { fired = true; l.put(lifeOp("pause_node", "I")) }
		_, err := k.sched.Judge(context.Background(), "g", "I", "parent", JudgeInput{})
		if !fired {
			t.Fatal("the seam did not fire")
		}
		refusedFor(t, err, reason)
		if k.count("SELECT COUNT(*) FROM dag_merge_checks") != 0 {
			t.Fatal("a judgement was recorded for a paused node")
		}
	})
	t.Run("requesting a merge turn", func(t *testing.T) {
		k := newJudgeKit(t)
		l := &lifeLog{t: t, f: k.fixture, plan: "g", rev: int(k.snapshot("g").Revision)}
		fired := false
		k.sched.testBetweenJudgeAndAsk = func() { fired = true; l.put(lifeOp("pause_node", "I")) }
		_, turn, err := k.sched.RequestMergeTurn(context.Background(), "g", "I", "parent", MergeRequestInput{Host: "host"})
		if !fired {
			t.Fatal("the seam did not fire")
		}
		refusedFor(t, err, reason)
		if turn != nil || k.count("SELECT COUNT(*) FROM merge_turns") != 0 {
			t.Fatal("a merge turn was created for a paused node")
		}
	})
	t.Run("observing the integration", func(t *testing.T) {
		k := newJudgeKit(t)
		l := &lifeLog{t: t, f: k.fixture, plan: "g", rev: int(k.snapshot("g").Revision)}
		fired := false
		k.sched.testBeforeObserveTx = func() { fired = true; l.put(lifeOp("pause_node", "I")) }
		_, err := k.sched.ObserveIntegration(context.Background(), "g", "I", "parent", []Target{{Repository: k.repo.path, BaseRef: "dev"}})
		if !fired {
			t.Fatal("the seam did not fire")
		}
		refusedFor(t, err, reason)
		if k.count("SELECT COUNT(*) FROM dag_integration_observations") != 0 {
			t.Fatal("an observation was recorded for a paused node")
		}
	})
	t.Run("continuing a frozen release", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.host.loseFirstCreation = true
		if res, err := k.release("rp", "A"); err != nil || res.Bound {
			t.Fatalf("release = %v %+v", err, res)
		}
		l := &lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}
		fired := false
		k.sched.testBeforeReplayTx = func() { fired = true; l.put(lifeOp("pause_node", "A")) }
		_, sentBefore := k.host.counts()
		_, err := k.release("rp", "A")
		if !fired {
			t.Fatal("the seam did not fire")
		}
		refusedFor(t, err, reason)
		if _, sent := k.host.counts(); sent != sentBefore || k.count("SELECT COUNT(*) FROM dag_node_executions") != 0 {
			t.Fatal("a child was bound for a paused node")
		}
	})
}

// c2: the descendants of an ended node are blocked through what they consumed, not only through the edge that leaves it: a node accepted on the result of a node the plan then cancelled or
// archived is not a value to build on, so what follows it is not released. A landing is the exception: what an integrated input handed over is in the target, whatever happens to the node.
func TestTheDescendantsOfAnEndedNodeAreBlockedThroughWhatTheyConsumed(t *testing.T) {
	for _, tc := range []struct{ op, reason string }{{"cancel_node", "blocked:predecessor_cancelled"}, {"archive_node", "blocked:predecessor_archived"}} {
		t.Run(tc.op, func(t *testing.T) {
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			l := &lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}
			l.put(addRelNode("C", dag.NodeNonPR), addEdge("bc", "B", "C", dag.EdgeArtifactVerified, nil))
			a := k.acceptNode("rp", "A", acceptOpts{})
			k.acceptNode("rp", "B", acceptOpts{Inputs: []any{consumes("ab", "A", a)}})
			if c := k.read("rp").node("C"); c.Disposition != DispReady {
				t.Fatalf("before the plan ends A, C reads %+v", c)
			}
			l.put(lifeOp(tc.op, "A"))
			reading := k.read("rp")
			if b := reading.node("B"); b.Reason != DoneAccepted {
				t.Fatalf("B, accepted on its own, reads %+v", b)
			}
			c := reading.node("C")
			if c.Reason != tc.reason || c.Disposition != DispBlocked || !strings.Contains(c.Detail, "A") {
				t.Fatalf("C reads %+v, want %s naming A", c, tc.reason)
			}
			if _, err := k.release("rp", "C"); refusalReason(err) != "disposition_conflict" {
				t.Fatalf("release of C = %v", err)
			}
			if created, _ := k.host.counts(); created != 0 {
				t.Fatalf("%d children were created below an ended node", created)
			}
		})
	}
	// what a landing handed over is in the target whatever happens to the node: X -> Y -> Z with Y accepted on X's result, X then cancelled. Y consumed X as a landing (an integrated
	// input) and Z is not blocked by it; Y consumed X as an artifact (a code artifact pinned to its head) and Z is.
	for _, tc := range []struct {
		name, kind, reason string
		pin                doc
	}{
		{"an integrated input", dag.EdgeIntegrated, DeferAuthorityPending, nil},
		{"an artifact input", dag.EdgeArtifactVerified, BlockedPredecessorCancelled, doc{"pins_code_head": true, "target_repository": "owner/repo", "target_base_ref": "dev"}},
	} {
		t.Run("consumed as "+tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.putPlan("x", 0, "x-r1", addNode("X", dag.NodeImplementation), addNode("Y", dag.NodeNonPR), addNode("Z", dag.NodeNonPR),
				addEdge("xy", "X", "Y", tc.kind, tc.pin), addEdge("yz", "Y", "Z", dag.EdgeArtifactVerified, nil))
			a := f.acceptNode("x", "X", acceptOpts{HeadSHA: head1, PR: 7, Forge: "owner/repo", Repository: "owner/repo"})
			input := consumes("xy", "X", a)
			if tc.kind == dag.EdgeIntegrated {
				f.integrate(a, "owner/repo", "dev", true, true)
				input = doc{"edge_id": "xy", "kind": dag.EdgeIntegrated, "from_node_id": "X", "acceptance_id": a.Acceptance.AcceptanceID, "head_sha": head1, "landed_sha": head1}
			}
			f.acceptNode("x", "Y", acceptOpts{Inputs: []any{input}})
			if st := f.status("x", "yz"); !st.Satisfied {
				t.Fatalf("before the plan ends X, the edge out of Y reads %+v", st)
			}
			(&lifeLog{t: t, f: f, plan: "x", rev: 1}).put(lifeOp("cancel_node", "X"))
			got := f.status("x", "yz")
			if tc.kind == dag.EdgeIntegrated {
				if !got.Satisfied {
					t.Fatalf("Y consumed a landing, and the edge out of it reads %+v", got)
				}
				return
			}
			if got.Satisfied || got.Reason != tc.reason {
				t.Fatalf("Y consumed X's artifact, and the edge out of it reads %+v, want %s", got, tc.reason)
			}
		})
	}
}

// A pass keeps what the reading said, the lifecycle of the nodes included, so the retained pass and the live reading do not differ.
func TestAPassKeepsTheLifecycleOfTheNodesItRead(t *testing.T) {
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	f.projectParent()
	(&lifeLog{t: t, f: f, plan: "p1", rev: 1}).put(lifeOp("cancel_node", "research"))
	reading, _ := f.recordPass("p1")
	if lifecycleOf(reading.node("research")) != "cancelled" {
		t.Fatalf("the reading does not carry the lifecycle: %+v", reading.node("research"))
	}
	rows := f.passRows("p1")
	if len(rows) != 1 || !strings.Contains(rows[0].dispositions, "\"lifecycle\":\"cancelled\"") || strings.Count(rows[0].dispositions, "lifecycle") != 1 {
		t.Fatalf("the pass kept %v", rows)
	}
}

// A release whose intent was recorded and whose child was not created is a continuation of the earlier release, and what it rests on is asked again: if the plan ended the node the frozen manifest consumed
// from, the retry never reaches the managed start (the edge-level and consumed-chain gates read the store as it is now; the frozen intent is the one place that did not).
func TestAFrozenSuccessorReleaseIsNotContinuedWhenItsInputWasEnded(t *testing.T) {
	for _, tc := range []struct{ op, reason string }{{"cancel_node", "blocked:predecessor_cancelled"}, {"archive_node", "blocked:predecessor_archived"}} {
		t.Run(tc.op, func(t *testing.T) {
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			k.reportNode("rp", "A", acceptOpts{})
			if _, err := k.accept("rp", "A", AcceptInput{}); err != nil {
				t.Fatal(err)
			}
			real := k.sched.Start
			calls := 0
			unreachable := errors.New("the host is not reachable")
			k.sched.Start = func(context.Context, []byte) (StartAnswer, error) { calls++; return StartAnswer{}, unreachable }
			if _, err := k.release("rp", "B"); !errors.Is(err, unreachable) || calls != 1 {
				t.Fatalf("the first release = %v after %d starts, want the host's failure after one", err, calls)
			}
			before := k.rows()
			if before.releases != 1 || before.slots != 1 || before.executions != 1 { // B has an intent and a slot and no child; the one execution is the report of A
				t.Fatalf("the failed start left %+v", before)
			}
			(&lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}).put(lifeOp(tc.op, "A"))
			k.sched.Start = func(ctx context.Context, raw []byte) (StartAnswer, error) { calls++; return real(ctx, raw) }
			_, err := k.release("rp", "B")
			if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("the retry = %v, want disposition_conflict naming %s", err, tc.reason)
			}
			if created, _ := k.host.counts(); calls != 1 || created != 0 || k.rows() != before {
				t.Fatalf("the retry reached the managed start (%d starts, %d children) or changed rows: %+v", calls, created, k.rows())
			}
		})
	}
}

// The last point at which the DAG can stop a child from being created is just before the managed start, for a new release and for a continuation alike: a pause that landed since the intent was
// written stops it, with the intent and the slot left where they were, and the resume lets the same release go on.
func TestAPauseThatLandsBeforeTheManagedStartStopsTheChild(t *testing.T) {
	t.Run("a new release", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		l := &lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}
		fired := false
		k.sched.testBeforeStart = func() { fired = true; l.put(lifeOp("pause_node", "A")) }
		_, err := k.release("rp", "A")
		if !fired {
			t.Fatal("the seam did not fire")
		}
		refusedFor(t, err, "defer:node_paused")
		if created, sent := k.host.counts(); created != 0 || sent != 0 {
			t.Fatalf("the managed start ran: %d children, %d messages", created, sent)
		}
		if got := k.rows(); got.releases != 1 || got.slots != 1 || got.executions != 0 {
			t.Fatalf("the intent and the slot were not left where they were: %+v", got)
		}
		k.sched.testBeforeStart = nil
		l.put(lifeOp("resume_node", "A"))
		if res, err := k.release("rp", "A"); err != nil || !res.Bound || !res.Replayed {
			t.Fatalf("the release after the resume = %v %+v", err, res)
		}
		if created, _ := k.host.counts(); created != 1 {
			t.Fatalf("%d children for one release", created)
		}
	})
	t.Run("a frozen release", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		real := k.sched.Start
		k.sched.Start = func(context.Context, []byte) (StartAnswer, error) {
			return StartAnswer{}, errors.New("the host is not reachable")
		}
		if _, err := k.release("rp", "A"); err == nil {
			t.Fatal("the first start was meant to fail")
		}
		k.sched.Start = real
		l := &lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}
		fired := false
		k.sched.testBeforeStart = func() { fired = true; l.put(lifeOp("pause_node", "A")) }
		_, err := k.release("rp", "A")
		if !fired {
			t.Fatal("the seam did not fire")
		}
		refusedFor(t, err, "defer:node_paused")
		if created, sent := k.host.counts(); created != 0 || sent != 0 {
			t.Fatalf("the managed start ran: %d children, %d messages", created, sent)
		}
	})
}

// A correction is prepared from inputs read while the plan may move: the plan's hold is asked again in the transaction that stores the manifest, so a pause that lands while the inputs are
// verified leaves no manifest, no frozen file and no instruction.
func TestAPauseThatLandsWhileACorrectionIsPreparedLeavesNothing(t *testing.T) {
	k := newReleaseKit(t)
	rid := k.correctionKit()
	k.openCorrection(rid, nil)
	l := &lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}
	fired := false
	k.sched.testBeforePrepareTx = func() { fired = true; l.put(lifeOp("pause_node", "A")) }
	manifests := k.count("SELECT COUNT(*) FROM dag_input_manifests")
	snapshot := writeFile(t, k.root, "corrections.md", "the notes of the correction")
	prepared, err := k.sched.PrepareCorrection(context.Background(), "rp", "A", "parent", ManifestInput{RuleVersion: k.request(false).RuleVersion,
		Volatile: []Volatile{{Source: "linear:comment", SnapshotURI: snapshot, SHA256: shaOf([]byte("the notes of the correction")), CapturedAt: "2026-10-02T00:00:00Z"}}}, VerifyOptions{ArtifactRoots: []string{k.root}})
	if !fired {
		t.Fatal("the seam did not fire")
	}
	refusedFor(t, err, "defer:node_paused")
	if prepared.Instruction != "" || prepared.FrozenPath != "" {
		t.Fatalf("an instruction was handed out: %+v", prepared)
	}
	if got := k.count("SELECT COUNT(*) FROM dag_input_manifests"); got != manifests {
		t.Fatalf("%d manifests were stored", got-manifests)
	}
	if _, err := os.Stat(filepath.Join(k.root, "dag-input-manifests")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a manifest file was frozen (%v)", err)
	}
}

// A node the plan retired or replaced since a command read it is not one to advance: the gates that ask the plan again inside a transaction refuse an absent node (unregistered_scope) as they
// refuse a held one, so a retry of a frozen release, or a correction in the middle of its preparation, never starts a child or stores a manifest for a node that left the plan.
func retireNodeA() []doc {
	return []doc{{"op": dag.OpRetireEdge, "edge_id": "ab"}, {"op": dag.OpRetireNode, "node_id": "A"}}
}

func TestANodeThatLeftThePlanIsNotStartedOrCorrected(t *testing.T) {
	t.Run("a frozen release", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		real := k.sched.Start
		calls := 0
		k.sched.Start = func(context.Context, []byte) (StartAnswer, error) {
			calls++
			return StartAnswer{}, errors.New("the host is not reachable")
		}
		if _, err := k.release("rp", "A"); err == nil || calls != 1 {
			t.Fatalf("the first start was meant to fail once: %v after %d starts", err, calls)
		}
		(&lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}).put(retireNodeA()...)
		k.sched.Start = func(ctx context.Context, raw []byte) (StartAnswer, error) { calls++; return real(ctx, raw) }
		_, err := k.release("rp", "A")
		if refusalReason(err) != "unregistered_scope" {
			t.Fatalf("the retry of a retired node's release = %v, want unregistered_scope", err)
		}
		if created, _ := k.host.counts(); calls != 1 || created != 0 {
			t.Fatalf("the managed start ran for a node that left the plan (%d starts, %d children)", calls, created)
		}
	})
	t.Run("a correction in the middle of its preparation", func(t *testing.T) {
		k := newReleaseKit(t)
		rid := k.correctionKit()
		k.openCorrection(rid, nil)
		l := &lifeLog{t: t, f: k.fixture, plan: "rp", rev: 1}
		fired := false
		k.sched.testBeforePrepareTx = func() { fired = true; l.put(retireNodeA()...) }
		manifests := k.count("SELECT COUNT(*) FROM dag_input_manifests")
		prepared, err := k.sched.PrepareCorrection(context.Background(), "rp", "A", "parent", ManifestInput{RuleVersion: k.request(false).RuleVersion}, VerifyOptions{ArtifactRoots: []string{k.root}})
		if !fired {
			t.Fatal("the seam did not fire")
		}
		if refusalReason(err) != "unregistered_scope" || prepared.Instruction != "" {
			t.Fatalf("prepare = %v %+v, want unregistered_scope and no instruction", err, prepared)
		}
		if got := k.count("SELECT COUNT(*) FROM dag_input_manifests"); got != manifests {
			t.Fatalf("%d manifests were stored", got-manifests)
		}
	})
}

// The reading keeps one rule the stale reading set down: a node that is not stale carries no stale object. A paused node keeps its stale reading (a pause overrides only that the node is owned); a node the plan
// cancelled or archived reads as ended and drops the object with the reason it explained.
func TestAStaleNodeThePlanHoldsKeepsTheStaleReadingsInvariant(t *testing.T) {
	for _, tc := range []struct{ op, reason string }{
		{"pause_node", invSliceChanged},
		{"cancel_node", "skip:node_cancelled"},
		{"archive_node", "skip:node_archived"},
	} {
		t.Run(tc.op, func(t *testing.T) {
			k := newReleaseKit(t)
			invSharedRoot(k)
			k.invRevise("sr", "A", "sr-r2", invTitle("a changed title"))
			before := k.read("sr")
			if got := invStaleIDs(before); !reflect.DeepEqual(got, []string{"A", "C"}) {
				t.Fatalf("before the plan holds A, the stale nodes are %v", got)
			}
			(&lifeLog{t: t, f: k.fixture, plan: "sr", rev: 2}).put(lifeOp(tc.op, "A"))
			after := k.read("sr")
			invAssertReasons(t, after)
			if a := after.node("A"); a.Reason != tc.reason {
				t.Fatalf("A reads %+v, want %s", a, tc.reason)
			}
			wantStale := []string{"A", "C"}
			if tc.op != "pause_node" {
				wantStale = []string{"C"}
			}
			if got := invStaleIDs(after); !reflect.DeepEqual(got, wantStale) {
				t.Fatalf("the stale nodes after %s are %v, want %v", tc.op, got, wantStale)
			}
		})
	}
}
