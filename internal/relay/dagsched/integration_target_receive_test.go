package dagsched

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// CRW-1302: when a parent acknowledges a child's event, the relay compares the integration target recorded for the child's task with the
// baselineRevision of the child's intent, from the store and the intent file alone. These tests seed the rows managed-start would have left for
// the accepted event of node a (a generation, a managed-start request and the intent file) and call the comparison the acknowledgement calls.

// receiveKit is a target kit over the accepted event of node a, which a test gives the intent of a managed start with seed.
type receiveKit struct {
	*batchKit
	remote, event, relationship, marker, workspace string
	res                                            IntegrationBatchResult
	issue                                          string
}

func newReceiveKit(t *testing.T) *receiveKit {
	t.Helper()
	k, remote, res := targetKit(t)
	r := &receiveKit{batchKit: k, remote: remote, res: res, marker: t.TempDir(), workspace: t.TempDir()}
	row, err := k.sched.Store.One(context.Background(), "SELECT e.event_id AS event_id, e.relationship_id AS relationship_id, r.issue_key AS issue_key FROM events e JOIN relationships r ON r.relationship_id = e.relationship_id ORDER BY e.first_seen_at LIMIT 1")
	if err != nil || row == nil {
		t.Fatalf("the accepted event of node a: %v", err)
	}
	r.event, r.relationship, r.issue = row.Text("event_id"), row.Text("relationship_id"), row.Text("issue_key")
	return r
}

// seed gives the event's generation the dispatch request, the managed-start request that names the workspace and the marker root, and the intent
// file under it; a nil baseline writes no intent file.
func (r *receiveKit) seed(request string, baseline any) {
	r.t.Helper()
	generations := r.count("SELECT COUNT(*) FROM generations WHERE relationship_id = ? AND execution_generation = 1", r.relationship)
	if generations == 0 {
		r.exec("INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, opened_at) VALUES (?, 1, ?, 'bound', '2026-10-14T00:00:00+00:00')", r.relationship, request)
	} else {
		r.exec("UPDATE generations SET dispatch_request_id = ? WHERE relationship_id = ? AND execution_generation = 1", request, r.relationship)
	}
	r.exec("DELETE FROM managed_start_requests WHERE relationship_id = ?", r.relationship)
	r.exec("INSERT INTO managed_start_requests (request_id, issue_key, request_fingerprint, fingerprint_version, workspace, marker_root, socket_identity, create_request_id, dispatch_request_id, state, revision, relationship_id, execution_generation, created_at, updated_at)"+
		" VALUES ('managed-1', ?, 'fp', 'v1', ?, ?, 'sock', 'create-1', ?, 'attached', 1, ?, 1, '2026-10-14T00:00:00+00:00', '2026-10-14T00:00:00+00:00')", r.issue, r.workspace, r.marker, request, r.relationship)
	if baseline == nil {
		return
	}
	if _, err := delivery.DeclareIntent(context.Background(), r.marker, delivery.IntentDeclaration{Workspace: r.workspace, DispatchRequestID: request, IssueKey: r.issue, DeclaredAt: "2026-10-14T00:00:00+00:00", BaselineRevision: baseline}); err != nil {
		r.t.Fatalf("declare the intent: %v", err)
	}
}

func (r *receiveKit) notice(t *testing.T) delivery.Obj {
	t.Helper()
	notice, err := ReceiveTargetNotice(context.Background(), r.sched.Store, r.event)
	if err != nil {
		t.Fatalf("comparison: %v", err)
	}
	return notice
}

// observe stores the head a push or the checker would have read on the recorded target's remote branch.
func (r *receiveKit) observe(t *testing.T, head string) {
	t.Helper()
	target := r.recordTarget(t, "dev-int", "dev")
	if _, err := r.sched.ObserveTarget(context.Background(), target, head, "check", CheckActor); err != nil {
		t.Fatal(err)
	}
}

// c4: an observed head that is not the baseline and not a head the relay recorded for the integration ref is a move outside the path. The
// comparison records the fault with the source receive and names it in the notice; repeating it adds nothing. A head equal to the baseline, and
// the target advancing along the integration path (a recorded head), record nothing.
func TestReceiveComparesObservationWithBaseline(t *testing.T) {
	r := newReceiveKit(t)
	r.seed("dispatch-request-1", r.base) // the child was started from the base of dev
	if got := r.notice(t); got != nil {
		t.Fatalf("a notice before any target is recorded: %v", got)
	}
	r.observe(t, r.base)
	if got := r.notice(t); got != nil {
		t.Fatalf("an observation equal to the baseline: %v; want none", got)
	}
	r.observe(t, r.res.NewHead)
	if got := r.notice(t); got != nil {
		t.Fatalf("the target advanced to a head the relay recorded: %v; want none (the integration path moved it)", got)
	}
	if faults := targetFaults(t, r.batchKit); len(faults) != 0 {
		t.Fatalf("%d faults before any move outside the path", len(faults))
	}

	foreign := r.heads["a"]
	r.observe(t, foreign)
	notice := r.notice(t)
	if notice == nil {
		t.Fatalf("a head outside the recorded set recorded no notice")
	}
	id := pyjson.Text(notice.Get("faultId"))
	if pyjson.Text(notice.Get("faultClass")) != ClassIntegrationTargetMoved || id == "" || notice.Get("faultError") != nil || pyjson.Text(notice.Get("observedHead")) != foreign ||
		pyjson.Text(notice.Get("baselineRevision")) != r.base || pyjson.Text(notice.Get("integrationRef")) != "dev-int" || pyjson.Text(notice.Get("remoteRef")) != "dev" {
		t.Fatalf("notice %v; want the fault of dev-int's moved target, observed %s against baseline %s", notice, foreign, r.base)
	}
	faults := targetFaults(t, r.batchKit)
	if len(faults) != 1 || faults[0].Text("fault_id") != id || faults[0].Text("severity") != "broken" || faults[0].Get("occurrence_count") != int64(1) || faults[0].Get("blocking") != int64(1) {
		t.Fatalf("faults %v; want one broken fault with one occurrence and one blocking notice", faults)
	}
	if evidence := r.faultEvidence(t, id); !strings.Contains(evidence, `"source":"receive"`) {
		t.Fatalf("fault evidence %s; want the source receive", evidence)
	}
	again := r.notice(t)
	if again == nil || pyjson.Text(again.Get("faultId")) != id {
		t.Fatalf("a replayed acknowledgement: %v; want the same fault", again)
	}
	if faults = targetFaults(t, r.batchKit); len(faults) != 1 || faults[0].Get("occurrence_count") != int64(1) || faults[0].Get("blocking") != int64(1) {
		t.Fatalf("faults after a replay: %v; want the same occurrence", faults)
	}
}

// c4: no record, a baseline that is not a full commit id, an event with no managed-start intent, an unreadable intent and an ambiguous target compare nothing.
func TestReceiveComparesNothingWithoutBothSides(t *testing.T) {
	foreign := func(r *receiveKit) string { return r.heads["a"] }
	cases := []struct {
		name  string
		setup func(t *testing.T, r *receiveKit)
	}{
		{"a baseline that is an opaque string", func(t *testing.T, r *receiveKit) {
			r.seed("dispatch-request-2", "dag-input-manifest:"+dig("manifest"))
			r.observe(t, foreign(r))
		}},
		{"an abbreviated commit id", func(t *testing.T, r *receiveKit) {
			r.seed("dispatch-request-3", r.base[:12])
			r.observe(t, foreign(r))
		}},
		{"no intent file", func(t *testing.T, r *receiveKit) {
			r.seed("dispatch-request-4", nil)
			r.observe(t, foreign(r))
		}},
		{"an intent file that cannot be read", func(t *testing.T, r *receiveKit) {
			r.seed("dispatch-request-5", r.base)
			r.exec("UPDATE managed_start_requests SET marker_root = ?", filepath.Join(t.TempDir(), "no-such-root"))
			r.observe(t, foreign(r))
		}},
		{"an event no managed-start request started", func(t *testing.T, r *receiveKit) {
			r.seed("dispatch-request-6", r.base)
			r.exec("DELETE FROM managed_start_requests")
			r.observe(t, foreign(r))
		}},
		{"no observation of the recorded target", func(t *testing.T, r *receiveKit) {
			r.seed("dispatch-request-7", r.base)
			r.recordTarget(t, "dev-int", "dev")
		}},
		{"an issue no live node of a plan carries", func(t *testing.T, r *receiveKit) {
			r.seed("dispatch-request-8", r.base)
			r.observe(t, foreign(r))
			r.exec("UPDATE relationships SET issue_key = 'CRW-elsewhere'")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newReceiveKit(t)
			c.setup(t, r)
			if got := r.notice(t); got != nil {
				t.Fatalf("notice %v; want a comparison that compares nothing", got)
			}
			if faults := targetFaults(t, r.batchKit); len(faults) != 0 {
				t.Fatalf("%d faults; want none", len(faults))
			}
		})
	}
}

// c4: the comparison reads the store and a file. It makes no git call and no network call: with no git on PATH it still answers, and the
// acknowledgement's wiring is in place whenever this package is linked.
func TestReceiveComparisonMakesNoGitCall(t *testing.T) {
	r := newReceiveKit(t)
	r.seed("dispatch-request-1", r.base)
	r.observe(t, r.heads["a"])
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	if _, err := os.Stat(filepath.Join(empty, "git")); err == nil {
		t.Fatal("git is on the emptied PATH")
	}
	if got := r.notice(t); got == nil {
		t.Fatal("the comparison needed git: no notice with an empty PATH")
	}
	ack := delivery.NewAck(delivery.NewService(r.sched.Store, delivery.SystemClock{}))
	if ack.TargetNotice == nil {
		t.Fatal("an acknowledgement built in a process that links the scheduler carries no target comparison")
	}
}

// laterPlanChild gives the kit a second plan, h, with one node b whose issue key no node of plan g carries, and returns the kit read for the event
// of b's accepted result: the child of a later plan. The target of the integration ref dev-int stays what plan g's record made it: plan g, origin
// and dev (a second record of the same remote branch is the no-op CRW-1304 pins, so the row never names plan h).
func (r *receiveKit) laterPlanChild(t *testing.T) *receiveKit {
	t.Helper()
	r.putPlan("h", 0, "h-r1", addRelNode("b", dag.NodeImplementation))
	reported := r.reportNode("h", "b", acceptOpts{})
	row, err := r.sched.Store.One(context.Background(), "SELECT r.issue_key AS issue_key FROM relationships r WHERE r.relationship_id = ?", reported.Acceptance.RelationshipID)
	if err != nil || row == nil || row.Text("issue_key") == r.issue {
		t.Fatalf("the later plan's child needs an issue key of its own: %v %v", row, err)
	}
	later := *r
	later.event, later.relationship, later.issue = reported.Event, reported.Acceptance.RelationshipID, row.Text("issue_key")
	return &later
}

// elsewhere is a commit no branch holds and no batch recorded: a head that is outside the integration path.
func (r *receiveKit) elsewhere() string {
	return r.repo.git("commit-tree", r.treeOf(r.base), "-p", r.base, "-m", "elsewhere")
}

// CRW-1374 d1: the target is looked up by the child's integration ref, not by the plan the first record named. A child of a later plan that
// integrates onto the same ref and the same remote branch is compared at acknowledgement like the child of the plan that recorded the target.
func TestReceiveComparesAChildOfALaterPlanOnTheSameIntegrationRef(t *testing.T) {
	cases := []struct {
		name  string
		setup func(r *receiveKit) (baseline string)
	}{
		{"the later plan has a batch of its own on the ref", func(r *receiveKit) string {
			// plan h recorded an intent stage for dev-int, as its first integrate batch would
			r.exec("INSERT INTO dag_integration_stages (stage_id, batch_id, plan_id, stage, node_id, acceptance_id, event_id, revision_hash, generation, head_sha, detail, recorded_by, recorded_at)"+
				" VALUES ('stage-h-1', 'batch-h-1', 'h', 'intent', '', '', '', '', 0, '', ?, 'parent', '2026-10-14T00:00:00+00:00')", `{"integration_ref":"dev-int","base":"`+r.res.NewHead+`"}`)
			return r.heads["a"] // a baseline of no recorded head: only the plan's batch names the ref
		}},
		{"the child was started from a head the ref recorded", func(r *receiveKit) string { return r.base }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newReceiveKit(t)
			target := r.recordTarget(t, "dev-int", "dev")
			if target.PlanID != "g" {
				t.Fatalf("the recorded target names plan %q; want g", target.PlanID)
			}
			rowsBefore := r.count("SELECT COUNT(*) FROM dag_integration_targets")
			child := r.laterPlanChild(t)
			child.seed("dispatch-request-later", c.setup(r))
			foreign := r.elsewhere()
			child.observe(t, foreign)
			if again := r.recordTarget(t, "dev-int", "dev"); again.PlanID != "g" {
				t.Fatalf("the repeated record names plan %q; the stored target must stay plan g's (no schema or row change is needed)", again.PlanID)
			}
			notice := child.notice(t)
			if notice == nil {
				t.Fatal("a child of a later plan on the same integration ref and remote branch compared nothing at acknowledgement")
			}
			if pyjson.Text(notice.Get("integrationRef")) != "dev-int" || pyjson.Text(notice.Get("remoteRef")) != "dev" || pyjson.Text(notice.Get("observedHead")) != foreign || pyjson.Text(notice.Get("faultId")) == "" {
				t.Fatalf("notice %v; want the fault of dev-int's moved target, observed %s", notice, foreign)
			}
			if faults := targetFaults(t, r.batchKit); len(faults) != 1 {
				t.Fatalf("%d faults; want one", len(faults))
			}
			if rows := r.count("SELECT COUNT(*) FROM dag_integration_targets"); rows != rowsBefore+1 {
				t.Fatalf("%d target rows (%d before the observation); the comparison must add no target row", rows, rowsBefore)
			}
		})
	}
}

// CRW-1374 d1: a child of a later plan that nothing ties to the ref (no batch of its plan on it, and a baseline that is none of its recorded heads)
// is not compared: the lookup still does not guess which target the child was started from.
func TestReceiveDoesNotGuessTheTargetOfAChildNothingTiesToTheRef(t *testing.T) {
	r := newReceiveKit(t)
	r.recordTarget(t, "dev-int", "dev")
	child := r.laterPlanChild(t)
	child.seed("dispatch-request-later", r.heads["a"])
	child.observe(t, r.elsewhere())
	if got := child.notice(t); got != nil {
		t.Fatalf("notice %v; want a comparison that compares nothing", got)
	}
	if faults := targetFaults(t, r.batchKit); len(faults) != 0 {
		t.Fatalf("%d faults; want none", len(faults))
	}
}
