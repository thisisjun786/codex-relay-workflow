package dagsched

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// activity is one query of a plan in its own read transaction, the way dag-activity asks it.
func (f *fixture) activity(plan string) Activity {
	f.t.Helper()
	a, err := f.sched.ReadActivity(context.Background(), plan)
	if err != nil {
		f.t.Fatalf("activity %s: %v", plan, err)
	}
	return a
}

func (a Activity) node(id string) NodeActivity {
	for _, n := range a.Nodes {
		if n.NodeID == id {
			return n
		}
	}
	panic("no node " + id + " in the activity document")
}

func (a Activity) printed() string { return pyjson.Dumps(a.Object(), pyjson.Options{Indent: 2}) }

// journalMax is the journal's newest sequence, read through the fixture's own connection.
func (f *fixture) journalMax() int64 {
	f.t.Helper()
	var seq int64
	if err := f.s.DB.QueryRowContext(context.Background(), "SELECT COALESCE(MAX(seq), 0) FROM journal").Scan(&seq); err != nil {
		f.t.Fatal(err)
	}
	return seq
}

// childActivity records the rows that show a child alive without it reporting or being ruled on: a poll of its turn, a turn admitted to its generation and the host's lifecycle
// observation of its thread. The recorded time is chosen by the test, so a "later" row is later in what the store says and not in what the clock says.
func (f *fixture) childActivity(rid, node, at string) {
	f.t.Helper()
	f.exec("INSERT OR REPLACE INTO poll_observations (relationship_id, execution_generation, turn_id, last_status, last_polled_at, last_attempt_at) VALUES (?, 1, 'turn-hb', 'inProgress', ?, ?)", rid, at, at)
	f.exec("INSERT OR REPLACE INTO generation_turns (relationship_id, execution_generation, turn_id, evidence, admitted_at) VALUES (?, 1, 'turn-hb', 'heartbeat', ?)", rid, at)
	f.exec("INSERT OR REPLACE INTO recipient_lifecycle (task_id, runtime_status, deliverable, observed_at) VALUES (?, 'active', 'yes', ?)", "child-"+node, at)
}

// Criterion 4 of the issue and the first red test of 186a: the query reads the store and changes nothing in it, the journal sequence included, and two queries of one state are one document.
func TestActivityQueryWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	forkJoinPlan(f, "p1")
	f.acceptNode("p1", "research", acceptOpts{})
	for i := 0; i < 3; i++ {
		f.exec("INSERT INTO journal (at, kind, subject, detail) VALUES ('2026-10-02T00:00:00+00:00', 'fixture', 'p1', '{}')")
	}
	seq, before, rows := f.journalMax(), dumpStore(t, f.path), f.zoneRows()
	if seq < 3 {
		t.Fatalf("the journal sequence is %d: the fixture wrote nothing to move it", seq)
	}
	first, second := f.activity("p1"), f.activity("p1")
	if first.Digest == "" || first.printed() != second.printed() || first.Digest != second.Digest {
		t.Fatalf("two queries of one store state print different documents")
	}
	if got := f.journalMax(); got != seq {
		t.Errorf("the journal sequence moved from %d to %d", seq, got)
	}
	sameStore(t, "dag-activity", before, dumpStore(t, f.path))
	if !reflect.DeepEqual(rows, f.zoneRows()) {
		t.Errorf("the query changed the zone's rows: %v -> %v", rows, f.zoneRows())
	}
}

// The two entries are two things: a ruling is evidence and is not activity, a poll is activity and is not evidence, and each names its relationship, generation, event, recorded time and source.
func TestActivityKeepsEvidenceAndActivityApart(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	forkJoinPlan(f, "p1")
	a := f.acceptNode("p1", "research", acceptOpts{})
	doc := f.activity("p1")
	n := doc.node("research")
	if n.Evidence == nil || n.Evidence.Label != LabelEvidence || n.Evidence.RelationshipID != a.Acceptance.RelationshipID || !n.Evidence.HasGeneration || n.Evidence.Generation != 1 ||
		n.Evidence.EventID != a.Event || n.Evidence.RecordedAt == "" || n.Evidence.Source != EvidenceAcceptance {
		t.Fatalf("evidence of an accepted node = %+v, want the acceptance of its relationship, generation, event and recorded time", n.Evidence)
	}
	// the child's final report is the child's recorded activity, from the events table, and is not the verification
	if n.Activity == nil || n.Activity.Label != LabelActivity || n.Activity.Source != ActivityChildEvent || n.Activity.EventID != a.Event || n.Activity.RelationshipID != a.Acceptance.RelationshipID {
		t.Fatalf("activity of an accepted node = %+v, want the child's final report", n.Activity)
	}
	// a node nobody started has neither
	if d := doc.node("design"); d.Evidence != nil || d.Activity != nil {
		t.Errorf("a node nobody owns has evidence %+v and activity %+v, want neither", d.Evidence, d.Activity)
	}
	// a poll recorded after the acceptance is the newest activity and leaves the evidence where it was
	f.childActivity(a.Acceptance.RelationshipID, "research", "2026-10-02T09:00:00.000000+00:00")
	after := f.activity("p1").node("research")
	if after.Activity.Source != ActivityPoll && after.Activity.Source != ActivityTurnAdmitted && after.Activity.Source != ActivityLifecycle || after.Activity.RecordedAt != "2026-10-02T09:00:00.000000+00:00" {
		t.Errorf("activity after a poll = %+v, want one of the rows recorded at the poll's time", after.Activity)
	}
	if !reflect.DeepEqual(after.Evidence, n.Evidence) {
		t.Errorf("a poll moved the evidence: %+v -> %+v", n.Evidence, after.Evidence)
	}
	// a ruling recorded after the poll is evidence and does not displace the activity
	f.exec("INSERT INTO verdicts (event_id, record, verdict, verdict_turn_id, decided_at) VALUES ('evt-later', '{}', 'verified', 'verdict-later', '2026-10-02T10:00:00.000000+00:00')")
	f.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
		" VALUES ('evt-later', ?, 1, ?, 'ready_for_review', 'daemon', 'child', 'turn-l', 'completed', '{}', 'final', '2026-10-02T08:00:00.000000+00:00', '2026-10-02T08:00:00.000000+00:00')", a.Acceptance.RelationshipID, dig("later"))
	ruled := f.activity("p1").node("research")
	if ruled.Evidence.Source != EvidenceVerdict || ruled.Evidence.EventID != "evt-later" || ruled.Evidence.RecordedAt != "2026-10-02T10:00:00.000000+00:00" {
		t.Errorf("evidence after a later ruling = %+v, want that ruling", ruled.Evidence)
	}
	if ruled.Activity.RecordedAt != "2026-10-02T09:00:00.000000+00:00" {
		t.Errorf("a ruling (and a daemon-produced event) moved the activity to %+v", ruled.Activity)
	}
}

// Criterion 2: activity is display only. A child that only sends heartbeats moves no stage, no count, no evidence and no digest of the progress view, however far in the future the store says
// it was recorded, and the scheduler's own reading is the same.
func TestHeartbeatOnlyFixtureChangesNoState(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	forkJoinPlan(f, "p1")
	rid := f.startNode("p1", "design")
	beforeDoc, beforeProgress, beforeReading := f.activity("p1"), f.progress("p1"), f.read("p1")
	if n := beforeDoc.node("design"); n.Stage != StageRunning || n.Activity != nil || n.Evidence != nil {
		t.Fatalf("the started node = %s, activity %+v, evidence %+v; want running with neither", n.Stage, n.Activity, n.Evidence)
	}
	for _, at := range []string{"2026-10-02T05:00:00.000000+00:00", "2099-01-01T00:00:00.000000+00:00"} {
		f.childActivity(rid, "design", at)
		doc := f.activity("p1")
		n := doc.node("design")
		if n.Activity == nil || n.Activity.RecordedAt != at {
			t.Fatalf("heartbeat at %s: activity = %+v, want it shown", at, n.Activity)
		}
		if n.Stage != StageRunning || n.State != beforeDoc.node("design").State || n.Disposition != beforeDoc.node("design").Disposition || n.Evidence != nil {
			t.Errorf("heartbeat at %s moved the node: stage %s state %s disposition %s evidence %+v", at, n.Stage, n.State, n.Disposition, n.Evidence)
		}
		progress := f.progress("p1")
		if progress.Digest != beforeProgress.Digest || progress.printed() != beforeProgress.printed() {
			t.Errorf("heartbeat at %s changed the dag-progress document", at)
		}
		if doc.Progress.Digest != beforeDoc.Progress.Digest || doc.ProgressCursor != beforeDoc.ProgressCursor {
			t.Errorf("heartbeat at %s changed the progress digest or cursor the activity document is pinned to", at)
		}
		reading := f.read("p1")
		if reading.InputDigest != beforeReading.InputDigest || !reflect.DeepEqual(reading.Pass, beforeReading.Pass) {
			t.Errorf("heartbeat at %s changed the scheduler's reading", at)
		}
	}
}

// Criterion 4: the activity document is pinned to the progress view it was read with, and that view is what a cursor-and-replay rebuild ends at. Replaying from the zero cursor, and a snapshot
// plus the events after its cursor, both end at the progress digest and the cursor the document names, before and after the store moves.
func TestActivityCursorAndReplayAreConsistent(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	forkJoinPlan(f, "p1")
	f.acceptNode("p1", "research", acceptOpts{})
	ctx := context.Background()
	replay := func(label string, doc Activity) ProgressSnapshot {
		t.Helper()
		var snap ProgressSnapshot
		cursor := ProgressCursor{}
		for {
			delta, err := f.sched.ReadProgressDelta(ctx, "p1", cursor, 3)
			if err != nil {
				t.Fatalf("%s: delta: %v", label, err)
			}
			if snap, err = ApplyProgressDelta(snap, delta); err != nil {
				t.Fatalf("%s: fold: %v", label, err)
			}
			cursor = delta.Cursor
			if !delta.More {
				break
			}
		}
		view, err := snap.Project()
		if err != nil {
			t.Fatalf("%s: project: %v", label, err)
		}
		if view.Digest != doc.Progress.Digest {
			t.Errorf("%s: the replayed view has digest %s, the activity document names %s", label, view.Digest, doc.Progress.Digest)
		}
		if snap.Cursor().String() != doc.ProgressCursor {
			t.Errorf("%s: the replayed snapshot's cursor is %s, the document names %s", label, snap.Cursor().String(), doc.ProgressCursor)
		}
		return snap
	}
	first := f.activity("p1")
	snap := replay("from the beginning", first)
	// a state that moves progress: the next node starts. The document moves with it, still consistent with a replay and with a snapshot resumed from its older cursor.
	f.startNode("p1", "design")
	second := f.activity("p1")
	if second.Progress.Digest == first.Progress.Digest || second.ProgressCursor == first.ProgressCursor {
		t.Fatal("starting a node changed neither the progress digest nor the cursor")
	}
	replay("after the store moved", second)
	cursor, err := ParseProgressCursor(snap.Cursor().String())
	if err != nil {
		t.Fatal(err)
	}
	delta, err := f.sched.ReadProgressDelta(ctx, "p1", cursor, dag.MaxPage)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := ApplyProgressDelta(snap, delta)
	if err != nil {
		t.Fatal(err)
	}
	view, err := resumed.Project()
	if err != nil || view.Digest != second.Progress.Digest || resumed.Cursor().String() != second.ProgressCursor {
		t.Errorf("a snapshot resumed from its cursor ends at digest %s cursor %s (err %v), the document names %s %s", view.Digest, resumed.Cursor().String(), err, second.Progress.Digest, second.ProgressCursor)
	}
	// every stage in the activity document is the stage of the replayed view
	for _, n := range view.Nodes {
		if got := second.node(n.NodeID).Stage; got != n.Stage {
			t.Errorf("node %s: the activity document says %s, the replayed view %s", n.NodeID, got, n.Stage)
		}
	}
}

// 186 remaining work from CRW-801: the stagnation reading dag-ready computes is projected into the activity document node by node, with the count, cause, rung, next action, finding identity and
// the event it rests on, and the plan-wide count by rung. It appears at the threshold, climbs the ladder, resets when the finding changes and goes away when the node is repaired (landed).
func TestActivityProjectsTheStagnationReading(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	stagnatingPlan(f, "sp")
	rid := f.startNode("sp", "I")
	f.addCorrection("sp", "I", rid, 2, rulingFinding("fix the same thing", 2), false)
	if n := f.activity("sp").node("I"); n.Stagnation != nil {
		t.Fatalf("one correction: stagnation %+v, want none below the threshold of 2", n.Stagnation)
	}
	f.addCorrection("sp", "I", rid, 3, rulingFinding("fix the same thing", 3), false)
	doc := f.activity("sp")
	want := f.read("sp").node("I").Stagnation
	got := doc.node("I").Stagnation
	if got == nil || want == nil || !reflect.DeepEqual(*got, *want) {
		t.Fatalf("the activity document carries %+v, dag-ready reads %+v: they must be one reading", got, want)
	}
	if got.Count != 2 || got.Cause != CauseRepeatedFinding || got.Rung != RungEditPacket || got.Next == "" || got.FindingDigest == "" || got.LastEventID == "" {
		t.Errorf("two corrections of one finding = %+v, want count 2, repeated_finding, edit_packet, a next action, a finding identity and an event", got)
	}
	if doc.Stagnation.EditPacket != 1 || doc.Stagnation.SplitNode != 0 {
		t.Errorf("the plan-wide counts = %+v, want one node at edit_packet", doc.Stagnation)
	}
	printed := doc.decoded(t)
	node := nodeOfDecoded(t, printed, "I")
	st, _ := node["stagnation"].(map[string]any)
	if fmt.Sprint(st["count"]) != "2" || st["cause"] != CauseRepeatedFinding || st["rung"] != RungEditPacket || st["finding_digest"] != got.FindingDigest || st["last_event_id"] != got.LastEventID || st["next"] == nil {
		t.Errorf("the printed stagnation = %v", st)
	}
	if j := nodeOfDecoded(t, printed, "J"); j["stagnation"] != nil {
		t.Errorf("a node nobody corrected prints stagnation %v, want null", j["stagnation"])
	}
	f.addCorrection("sp", "I", rid, 4, rulingFinding("fix the same thing", 4), false)
	if n := f.activity("sp").node("I"); n.Stagnation == nil || n.Stagnation.Count != 3 || n.Stagnation.Rung != RungSplitNode {
		t.Errorf("three corrections = %+v, want count 3 and split_node", n.Stagnation)
	}
	// a different finding is a different problem: the count resets and the object goes away
	f.addCorrection("sp", "I", rid, 5, rulingFinding("another thing", 5), false)
	reset := f.activity("sp")
	if reset.node("I").Stagnation != nil || reset.Stagnation != (StagnationCounts{}) {
		t.Errorf("a different finding: stagnation %+v, counts %+v; want none", reset.node("I").Stagnation, reset.Stagnation)
	}
	// the same finding twice again, then the node is repaired and lands: the object clears by itself
	f.addCorrection("sp", "I", rid, 6, rulingFinding("another thing", 6), false)
	if n := f.activity("sp").node("I"); n.Stagnation == nil || n.Stagnation.Count != 2 {
		t.Fatalf("the same finding twice again = %+v, want count 2", n.Stagnation)
	}
	repaired := pinnedOpts
	repaired.Suffix = "-repair"
	a := f.acceptNode("sp", "I", repaired)
	f.integrate(a, "owner/repo", "dev", true, true)
	if n := f.activity("sp").node("I"); n.Stagnation != nil {
		t.Errorf("a node that landed carries %+v, want none", n.Stagnation)
	}
}

// The stagnation projection is a replay of rows and writes no ledger: two queries over one state print one document, the zone's rows are the same afterwards, and the dag-progress document
// of that state carries no stagnation (dag-progress/1 is unchanged).
func TestActivityStagnationIsAReplayAndWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	stagnatingPlan(f, "sp")
	rid := f.startNode("sp", "I")
	f.addCorrection("sp", "I", rid, 2, rulingFinding("fix the same thing", 2), false)
	f.addCorrection("sp", "I", rid, 3, rulingFinding("fix the same thing", 3), false)
	rows, seq, before := f.zoneRows(), f.journalMax(), dumpStore(t, f.path)
	first, second := f.activity("sp"), f.activity("sp")
	if first.printed() != second.printed() || first.node("I").Stagnation == nil {
		t.Fatal("two queries of one state differ, or the stagnation is missing")
	}
	if !reflect.DeepEqual(rows, f.zoneRows()) || f.journalMax() != seq {
		t.Error("the stagnation reading wrote a row")
	}
	sameStore(t, "dag-activity", before, dumpStore(t, f.path))
	if strings.Contains(f.progress("sp").printed(), "stagnation") {
		t.Error("dag-progress/1 prints a stagnation key: the document must stay as it was")
	}
}

func nodeOfDecoded(t *testing.T, doc map[string]any, id string) map[string]any {
	t.Helper()
	nodes, _ := doc["nodes"].([]any)
	for _, item := range nodes {
		if n, _ := item.(map[string]any); n["node_id"] == id {
			return n
		}
	}
	t.Fatalf("no node %s in the document", id)
	return nil
}

func (a Activity) decoded(t testing.TB) map[string]any {
	t.Helper()
	v, err := pyjson.Loads(a.printed(), pyjson.LoadOptions{})
	if err != nil {
		t.Fatalf("the printed document is not JSON: %v", err)
	}
	return toGeneric(v)
}

// Criterion 3: the exact links. A non_pr node links the artifact its acceptance stands on, an implementation node accepted on the commit path links its commits and the integration ref its batch
// moved, and one accepted with a forge identity links its pull request and no commit path. Every identity is the one the store holds.
func TestActivityLinksArtifactAndCommitPath(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	forkJoinPlan(f, "p1")
	research := f.acceptNode("p1", "research", acceptOpts{})
	f.exec("UPDATE dag_acceptances SET output_manifest_ref = 'manifest://research', evidence_digest = ? WHERE acceptance_id = ?", dig("artifact evidence"), research.Acceptance.AcceptanceID)
	pr := f.acceptNode("p1", "impl-a", acceptOpts{HeadSHA: head1, PR: 7, Forge: "owner/repo", Repository: "owner/repo"})
	commit := f.acceptNode("p1", "impl-b", acceptOpts{HeadSHA: progHead2, Repository: "owner/repo"})
	acc := commit.Acceptance.AcceptanceID
	f.exec("INSERT INTO dag_acceptance_verifications (acceptance_id, record_digest, record_json, head_commit, tree_sha, base_commit, recorded_by, coordinator_epoch, recorded_at) VALUES (?, ?, '{}', ?, ?, ?, 'parent', 0, '2026-10-02T07:00:00.000000+00:00')",
		acc, dig("record"), progHead2, strings.Repeat("3", 40), strings.Repeat("4", 40))
	f.exec("INSERT INTO dag_integration_batches (batch_id, plan_id, repository, integration_ref, base_ref, old_head, new_head, merged_json, split_json, verification_json, recorded_by, coordinator_epoch, recorded_at) VALUES ('b1', 'p1', 'owner/repo', 'refs/heads/it', 'dev', '', ?, '[]', '[]', '{}', 'parent', 0, 't')", progHead2)
	f.exec("INSERT INTO dag_integration_stages (stage_id, batch_id, plan_id, stage, node_id, acceptance_id, event_id, revision_hash, generation, head_sha, detail, recorded_by, recorded_at) VALUES ('s0', 'b1', 'p1', 'intent', '', '', '', '', 0, '', '{\"integration_ref\":\"refs/heads/it\"}', 'parent', 't')")
	f.exec("INSERT INTO dag_integration_stages (stage_id, batch_id, plan_id, stage, node_id, acceptance_id, event_id, revision_hash, generation, head_sha, detail, recorded_by, recorded_at) VALUES ('s1', 'b1', 'p1', 'intent', 'impl-b', ?, ?, ?, 1, ?, '{}', 'parent', 't')", acc, commit.Event, commit.Acceptance.RevisionHash, progHead2)
	f.exec("INSERT INTO dag_integration_stages (stage_id, batch_id, plan_id, stage, node_id, acceptance_id, event_id, revision_hash, generation, head_sha, detail, recorded_by, recorded_at) VALUES ('s2', 'b1', 'p1', 'ref_moved', '', '', '', '', 0, ?, '{}', 'parent', 't')", progHead2)

	doc := f.activity("p1")
	a := doc.node("research").Artifact
	if a == nil || a.AcceptanceID != research.Acceptance.AcceptanceID || a.RelationshipID != research.Acceptance.RelationshipID || a.EventID != research.Event || a.RevisionHash != research.Acceptance.RevisionHash ||
		a.OutputManifestRef != "manifest://research" || a.EvidenceDigest != dig("artifact evidence") || a.Generation != 1 {
		t.Errorf("the non_pr artifact link = %+v, want the identities its acceptance stored", a)
	}
	if doc.node("research").Commit != nil || doc.node("research").PullRequest != nil {
		t.Errorf("a non_pr node links a commit path %+v or a pull request %+v", doc.node("research").Commit, doc.node("research").PullRequest)
	}
	c := doc.node("impl-b").Commit
	if c == nil || c.AcceptanceID != acc || c.HeadCommit != progHead2 || c.TreeSHA != strings.Repeat("3", 40) || c.BaseCommit != strings.Repeat("4", 40) || c.RecordDigest != dig("record") ||
		c.RecordedAt != "2026-10-02T07:00:00.000000+00:00" || !reflect.DeepEqual(c.IntegrationRefs, []string{"refs/heads/it"}) {
		t.Errorf("the commit-path link = %+v", c)
	}
	if doc.node("impl-b").Artifact != nil || doc.node("impl-b").PullRequest != nil {
		t.Errorf("a commit-path node links an artifact %+v or a pull request %+v", doc.node("impl-b").Artifact, doc.node("impl-b").PullRequest)
	}
	// the commit verification is also the newest evidence of that node, with its record digest as identity
	if e := doc.node("impl-b").Evidence; e == nil || e.Source != EvidenceCommitVerification || e.Ref != dig("record") || e.RecordedAt != "2026-10-02T07:00:00.000000+00:00" {
		t.Errorf("the commit path's evidence = %+v", e)
	}
	p := doc.node("impl-a")
	if p.PullRequest == nil || p.PullRequest.Number != 7 || p.PullRequest.Repository != "owner/repo" || p.Commit != nil || p.Artifact != nil {
		t.Errorf("the pull-request node = pr %+v commit %+v artifact %+v", p.PullRequest, p.Commit, p.Artifact)
	}
	if want := pr.Acceptance.RelationshipID; p.Relationship == nil || p.Relationship.ID != want || p.Thread == nil || p.Thread.ChildTaskID != "child-impl-a" {
		t.Errorf("the relationship and thread links = %+v %+v", p.Relationship, p.Thread)
	}
	// the printed links are those of dag-progress where they overlap
	prog := f.progress("p1").decoded(t)
	for _, id := range []string{"impl-a", "research"} {
		got, want := nodeOfDecoded(t, doc.decoded(t), id)["links"].(map[string]any), nodeOfDecoded(t, prog, id)["links"].(map[string]any)
		for _, key := range []string{"relationship", "thread", "pull_request"} {
			if !reflect.DeepEqual(got[key], want[key]) {
				t.Errorf("node %s: link %s is %v here and %v in dag-progress", id, key, got[key], want[key])
			}
		}
	}
}

// A store that predates the commit-path tables answers without them: no query fails, and the node has the evidence and links the older tables give.
func TestActivityToleratesAStoreWithoutTheNewerTables(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	forkJoinPlan(f, "p1")
	f.acceptNode("p1", "research", acceptOpts{})
	var owed []string
	for _, table := range []string{"dag_acceptance_premerge", "dag_verified_heads"} {
		if f.count("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table) == 1 {
			owed = append(owed, table)
		}
	}
	if len(owed) != 2 {
		t.Fatalf("the fixture store lacks %v", owed)
	}
	f.exec("DROP TABLE dag_acceptance_premerge")
	f.exec("DROP TABLE dag_verified_heads")
	n := f.activity("p1").node("research")
	if n.Evidence == nil || n.Evidence.Source != EvidenceAcceptance {
		t.Errorf("evidence without the newer tables = %+v, want the acceptance", n.Evidence)
	}
}

// The command: the built binary, with PATH emptied, prints the document, reads the store only, and the journal and every row are the same afterwards. The refusals are the relay's.
func TestCLIActivityReadsOnlyTheStore(t *testing.T) {
	t.Parallel()
	state, f := cliState(t)
	db := filepath.Join(state, "relay.sqlite3")
	raw, err := sql.Open("sqlite", "file:"+db)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := raw.Exec("INSERT INTO journal (at, kind, subject, detail) VALUES ('2026-10-02T00:00:00+00:00', 'fixture', 'p1', '{}')"); err != nil {
			t.Fatal(err)
		}
	}
	_ = raw.Close()
	_ = f
	before, seq := dumpStore(t, db), journalSeq(t, db)
	out, errs, code := crwRun(t, state, envWithPath(t.TempDir()), "dag-activity", "--plan", "p1")
	if code != 0 {
		t.Fatalf("dag-activity exit %d\n%s\n%s", code, out, errs)
	}
	doc := parseOut(t, out)
	if doc["ok"] != true || doc["schema"] != SchemaActivity || doc["plan_id"] != "p1" || doc["digest"] == nil {
		t.Fatalf("answer = %v", doc)
	}
	progress, _, _ := crwRun(t, state, nil, "dag-progress", "--plan", "p1")
	pinned, _ := doc["progress"].(map[string]any)
	if pinned["schema"] != SchemaProgress || pinned["digest"] != parseOut(t, progress)["digest"] {
		t.Errorf("the document is pinned to %v, dag-progress prints digest %v", pinned, parseOut(t, progress)["digest"])
	}
	research := nodeOfDecoded(t, doc, "research")
	if ev, _ := research["evidence"].(map[string]any); ev["source"] != EvidenceAcceptance || research["stage"] != StageAccepted {
		t.Errorf("research = %v", research)
	}
	again, _, _ := crwRun(t, state, nil, "dag-activity", "--plan", "p1")
	if again != out {
		t.Error("two runs against one store print different documents")
	}
	sameStore(t, "dag-activity", before, dumpStore(t, db))
	if journalSeq(t, db) != seq {
		t.Errorf("the journal sequence moved from %d to %d", seq, journalSeq(t, db))
	}
	if _, errs, code := crwRun(t, state, nil, "dag-activity"); code != 2 || !strings.Contains(errs, "--plan") {
		t.Errorf("no --plan: exit %d, stderr %q", code, errs)
	}
	if out, _, code := crwRun(t, state, nil, "dag-activity", "--plan", "nope"); code != 2 || parseOut(t, out)["reason"] != "unregistered_scope" {
		t.Errorf("unknown plan: exit %d %s", code, out)
	}
	absent := filepath.Join(t.TempDir(), "nowhere")
	readyOut, _, readyCode := crwRun(t, absent, nil, "dag-ready", "--plan", "p1")
	activityOut, _, activityCode := crwRun(t, absent, nil, "dag-activity", "--plan", "p1")
	if activityCode != readyCode || parseOut(t, activityOut)["reason"] != parseOut(t, readyOut)["reason"] {
		t.Errorf("no store: dag-ready exit %d %s, dag-activity exit %d %s", readyCode, readyOut, activityCode, activityOut)
	}
}

// stampedRefresh records a base refresh, the way dag-base-refresh does, with a recorded time the test chooses and an event of its own; the row digests to its id, so a reader that checks the digest
// reads it as valid. generation is the generation the refresh stands on.
func (f *fixture) stampedRefresh(a accepted, seq int, generation int64, event, head, at string) string {
	f.t.Helper()
	revision := dig("revision refresh " + head)
	id := refreshDigest(a.Acceptance.AcceptanceID, a.Acceptance.RelationshipID, generation, event, revision, head, "owner/repo", "dev", "tip", "{}", "[]")
	f.exec("INSERT INTO dag_base_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json, recorded_by_task_id, coordinator_epoch, recorded_at)"+
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'owner/repo', 'dev', 'tip', '{}', '[]', 'parent', 0, ?)",
		id, a.Acceptance.AcceptanceID, seq, a.Acceptance.RelationshipID, generation, event, revision, head, at)
	return id
}

// stampedCheck records a merge check of an accepted pull request at a head, with a recorded time the test chooses.
func (f *fixture) stampedCheck(a accepted, id string, seq int, head, at string) {
	f.t.Helper()
	body := EvidenceBody{Required: []string{"dev-gate"}}.JSON()
	f.exec("INSERT INTO dag_merge_checks (check_id, acceptance_id, check_seq, head_sha, observed_head_sha, base_tip_sha, checks_digest, evidence_json, failed_required_json, round_no, outcome, reason, recorded_at)"+
		" VALUES (?, ?, ?, ?, ?, 'tip', ?, ?, '[]', 1, ?, 'test', ?)", id, a.Acceptance.AcceptanceID, seq, head, head, dig("evidence "+id), body, OutcomeEligible, at)
}

// Verification finding 1: the host's lifecycle observation of a child's thread is keyed by the task alone. It was recorded before a later generation was opened, and opening one records no activity, so
// the entry must not name the generation the relationship stands on now: the generation is absent, and the same row prints the same before and after.
func TestActivityLifecycleIdentityDoesNotMoveWithGeneration(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	forkJoinPlan(f, "p1")
	a := f.acceptNode("p1", "research", acceptOpts{})
	f.exec("INSERT OR REPLACE INTO recipient_lifecycle (task_id, runtime_status, deliverable, observed_at) VALUES ('child-research', 'active', 'yes', '2099-01-01T00:01:00.000000+00:00')")
	before := f.activity("p1").node("research").Activity
	if before == nil || before.Source != ActivityLifecycle || before.RecordedAt != "2099-01-01T00:01:00.000000+00:00" {
		t.Fatalf("activity = %+v, want the lifecycle observation as the newest row", before)
	}
	if before.HasGeneration {
		t.Errorf("the lifecycle observation names generation %d, but the store keys it by the task and cannot say which generation it was observed in", before.Generation)
	}
	f.exec("UPDATE relationships SET execution_generation = execution_generation + 1 WHERE relationship_id = ?", a.Acceptance.RelationshipID)
	after := f.activity("p1").node("research").Activity
	if !reflect.DeepEqual(before, after) {
		t.Errorf("opening a generation moved a past observation: %+v -> %+v", before, after)
	}
	if got := nodeOfDecoded(t, f.activity("p1").decoded(t), "research")["activity"].(map[string]any)["execution_generation"]; got != nil {
		t.Errorf("the printed generation of the lifecycle observation is %v, want null", got)
	}
}

// Verification finding 2: a base refresh the acceptance stands on is the newest durable verification change when it is recorded after the acceptance, on the acceptance's own generation or a later one.
// A row that does not digest to its id is not a refresh, and is not read.
func TestActivityBaseRefreshIsTheNewestVerificationChange(t *testing.T) {
	for _, tc := range []struct {
		name       string
		generation int64
	}{{"a later generation", 2}, {"the acceptance's own generation", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.projectParent()
			forkJoinPlan(f, "p1")
			a := f.acceptNode("p1", "research", acceptOpts{})
			if got := f.activity("p1").node("research").Evidence; got == nil || got.Source != EvidenceAcceptance {
				t.Fatalf("evidence before the refresh = %+v, want the acceptance", got)
			}
			id := f.stampedRefresh(a, 1, tc.generation, "evt-refresh", "headrefresh1", "2099-01-01T00:02:00.000000+00:00")
			e := f.activity("p1").node("research").Evidence
			if e == nil || e.Label != LabelEvidence || e.Source != EvidenceBaseRefresh || e.Ref != id || e.RelationshipID != a.Acceptance.RelationshipID || !e.HasGeneration || e.Generation != tc.generation ||
				e.EventID != "evt-refresh" || e.Change != "headrefresh1" || e.RecordedAt != "2099-01-01T00:02:00.000000+00:00" {
				t.Fatalf("evidence after a base refresh = %+v, want the refresh %s: its relationship, generation %d, event, head and recorded time", e, id, tc.generation)
			}
			// a row written by hand under another content is not a refresh
			f.exec("INSERT INTO dag_base_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json, recorded_by_task_id, coordinator_epoch, recorded_at)"+
				" VALUES ('dbr-forged', ?, 2, ?, ?, 'evt-forged', ?, 'headforged', 'owner/repo', 'dev', 'tip', '{}', '[]', 'parent', 0, '2099-02-01T00:00:00.000000+00:00')",
				a.Acceptance.AcceptanceID, a.Acceptance.RelationshipID, tc.generation, dig("forged"))
			if got := f.activity("p1").node("research").Evidence; !reflect.DeepEqual(got, e) {
				t.Errorf("a refresh row that does not digest to its id changed the evidence: %+v -> %+v", e, got)
			}
		})
	}
}

// Verification finding 3: a merge check judges the head the acceptance stood on when it was made, and the entry names the identity of that head: the acceptance's own before a base refresh, the
// refresh's after, and no generation or event for a head the store cannot tie to either.
func TestActivityMergeCheckNamesTheIdentityOfTheHeadItJudged(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	stagnatingPlan(f, "sp")
	a := f.acceptNode("sp", "I", pinnedOpts)
	own := a.Acceptance.HeadSHA
	if own == "" {
		t.Fatal("the fixture's acceptance holds no head")
	}
	evidence := func() *ActivityEntry { return f.activity("sp").node("I").Evidence }

	f.stampedCheck(a, "mc-1", 1, own, "2099-01-01T00:01:00.000000+00:00")
	e := evidence()
	if e == nil || e.Source != EvidenceMergeCheck || e.Ref != "mc-1" || !e.HasGeneration || e.Generation != a.Acceptance.ExecutionGeneration || e.EventID != a.Event {
		t.Fatalf("a check of the accepted head = %+v, want the acceptance's generation %d and event %s", e, a.Acceptance.ExecutionGeneration, a.Event)
	}
	f.stampedRefresh(a, 1, 2, "evt-refresh", "headrefresh1", "2099-01-01T00:02:00.000000+00:00")
	f.stampedCheck(a, "mc-2", 2, "headrefresh1", "2099-01-01T00:03:00.000000+00:00")
	e = evidence()
	if e == nil || e.Source != EvidenceMergeCheck || e.Ref != "mc-2" || !e.HasGeneration || e.Generation != 2 || e.EventID != "evt-refresh" {
		t.Fatalf("a check of the refreshed head = %+v, want the refresh's generation 2 and event evt-refresh", e)
	}
	// the earlier check keeps its own identity when it is the newest row again
	f.exec("UPDATE dag_merge_checks SET recorded_at = '2099-01-01T00:04:00.000000+00:00' WHERE check_id = 'mc-1'")
	e = evidence()
	if e == nil || e.Ref != "mc-1" || !e.HasGeneration || e.Generation != a.Acceptance.ExecutionGeneration || e.EventID != a.Event {
		t.Fatalf("a check made before the refresh = %+v, want the acceptance's identity, not the refresh's", e)
	}
	f.stampedCheck(a, "mc-3", 3, "headunknown", "2099-01-01T00:05:00.000000+00:00")
	e = evidence()
	if e == nil || e.Ref != "mc-3" || e.HasGeneration || e.EventID != "" || e.RelationshipID != a.Acceptance.RelationshipID {
		t.Fatalf("a check of a head neither the acceptance nor a valid refresh names = %+v, want no generation and no event", e)
	}
}

// Verification round 2, finding 1: a revalidation after a base refresh records the event of the refreshed generation, so its entry names that event's relationship and generation, not the generation
// the acceptance was first recorded under.
func TestActivityRevalidationNamesTheGenerationOfItsEvent(t *testing.T) {
	s := newRefreshScenario(t)
	s.openGeneration()
	s.refreshBase()
	if _, err := s.record("shared.json"); err != nil {
		t.Fatal(err)
	}
	other := dig("the second edition of I's criteria after the refresh")
	s.rvReregister("g", "I", "g-r3", s.rid, other, nil)
	s.rvRule(s.rid, "verified", other, rvVerified())
	res, err := s.accept("g", "I", AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 7}, Premerge: premergeAt(s.sched, context.Background(), "g", "I", s.head)})
	if err != nil || !res.Revalidated {
		t.Fatalf("revalidation = %+v %v", res, err)
	}
	// The revalidation is the newest durable verification change of the node.
	s.exec("UPDATE dag_acceptance_revalidations SET revalidated_at = '2099-01-01T00:00:00Z' WHERE acceptance_id = ?", res.AcceptanceID)
	e := s.activity("g").node("I").Evidence
	if e == nil || e.Source != EvidenceRevalidation {
		t.Fatalf("evidence = %+v, want the revalidation", e)
	}
	var rid string
	var generation, accepted int64
	if err := s.s.DB.QueryRow("SELECT relationship_id, execution_generation FROM events WHERE event_id = ?", e.EventID).Scan(&rid, &generation); err != nil {
		t.Fatal(err)
	}
	if err := s.s.DB.QueryRow("SELECT execution_generation FROM dag_acceptances WHERE acceptance_id = ?", res.AcceptanceID).Scan(&accepted); err != nil {
		t.Fatal(err)
	}
	if generation == accepted {
		t.Fatalf("the fixture revalidated on the acceptance's own generation %d; it must revalidate on the refreshed one", accepted)
	}
	if !e.HasGeneration || e.Generation != generation || e.RelationshipID != rid {
		t.Errorf("the revalidation names event %s (relationship %s, generation %d) but prints %+v", e.EventID, rid, generation, e)
	}
	// A revalidation whose event the store does not hold cannot be tied to a generation.
	s.exec("UPDATE dag_acceptance_revalidations SET event_id = 'evt-not-stored' WHERE acceptance_id = ?", res.AcceptanceID)
	if e := s.activity("g").node("I").Evidence; e == nil || e.Source != EvidenceRevalidation || e.HasGeneration {
		t.Errorf("a revalidation of an unstored event = %+v, want no generation", e)
	}
}

// Verification round 2, finding 2: a turn may be admitted under more than one generation of a relationship; an observation of that turn names a generation only when its event or a single admission
// ties it to one.
func TestActivityTurnObservationNamesAGenerationOnlyWhenOneTiesIt(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	forkJoinPlan(f, "p1")
	rid := f.startNode("p1", "design")
	f.addCorrection("p1", "design", rid, 2, nil, true)
	reg := &registry.Registry{Store: f.s}
	if err := reg.AdmitExplicitly(context.Background(), rid, 1, "turn-shared", "parent", "explicit shared turn"); err != nil {
		t.Fatalf("admit generation 1: %v", err)
	}
	f.exec("INSERT INTO observations (thread_id, turn_id, terminal_status, relationship_id, classification, observed_at) VALUES ('child-design', 'turn-shared', 'completed', ?, 'unreported', '2099-01-01T00:00:00Z')", rid)
	e := f.activity("p1").node("design").Activity
	if e == nil || e.Source != ActivityTurnObservation {
		t.Fatalf("activity = %+v, want the turn observation", e)
	}
	if !e.HasGeneration || e.Generation != 1 {
		t.Errorf("a turn admitted under generation 1 alone prints %+v, want generation 1", e)
	}
	if err := reg.AdmitExplicitly(context.Background(), rid, 2, "turn-shared", "parent", "explicit shared turn"); err != nil {
		t.Fatalf("admit generation 2: %v", err)
	}
	if e := f.activity("p1").node("design").Activity; e == nil || e.Source != ActivityTurnObservation || e.HasGeneration {
		t.Errorf("a turn admitted under generations 1 and 2 prints %+v, want no generation", e)
	}
	if got := nodeOfDecoded(t, f.activity("p1").decoded(t), "design")["activity"].(map[string]any)["execution_generation"]; got != nil {
		t.Errorf("the printed generation of the ambiguous turn observation is %v, want null", got)
	}
}

// Evaluation d1: a poll row keeps the status of its newest attempt and the time of its newest successful one. When the newest attempt failed, the status is not the one observed at that time, so the
// entry keeps the time of the successful poll (the child answered then) and states no status.
func TestActivityPollNeverPairsAStatusWithAnotherAttemptsTime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  any
		pollErr any
		want    string
	}{
		{"a successful poll", "inProgress", nil, "inProgress"},
		{"the turn absent at a later attempt", "absent", "str: the host reports this turn absent", ""},
		{"a transport error at a later attempt", nil, "Exception: connection reset", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.projectParent()
			forkJoinPlan(f, "p1")
			a := f.acceptNode("p1", "research", acceptOpts{})
			f.exec("INSERT OR REPLACE INTO poll_observations (relationship_id, execution_generation, turn_id, last_status, last_polled_at, last_attempt_at, last_error) VALUES (?, 1, 'turn-p', ?, ?, ?, ?)",
				a.Acceptance.RelationshipID, tc.status, "2099-01-01T00:00:00.000000+00:00", "2099-01-01T00:05:00.000000+00:00", tc.pollErr)
			e := f.activity("p1").node("research").Activity
			if e == nil || e.Source != ActivityPoll || e.RecordedAt != "2099-01-01T00:00:00.000000+00:00" || e.Ref != "turn-p" {
				t.Fatalf("activity = %+v, want the poll recorded at the time of the successful attempt", e)
			}
			if e.Change != tc.want {
				t.Errorf("the poll entry prints status %q, want %q: the stored status belongs to the attempt at 2099-01-01T00:05:00, not to the poll at 2099-01-01T00:00:00", e.Change, tc.want)
			}
		})
	}
}

// premergeEvidenceScenario is the refresh scenario with its base refreshed to the head the pull request shows now (generation 2, its own event) and a pre-merge record stored for accepted at a late time.
func premergeEvidenceScenario(t *testing.T, accepted func(s *refreshScenario) string) *refreshScenario {
	t.Helper()
	s := newRefreshScenario(t)
	s.openGeneration()
	s.refreshBase()
	if _, err := s.record("shared.json"); err != nil {
		t.Fatal(err)
	}
	head := accepted(s)
	raw := premergeAt(s.sched, context.Background(), "g", "I", head)
	digest, err := premerge.Digest(raw)
	if err != nil {
		t.Fatal(err)
	}
	s.exec("INSERT INTO dag_acceptance_premerge (acceptance_id, record_digest, record_json, evaluated_head, accepted_head, recorded_by, coordinator_epoch, recorded_at) VALUES (?,?,?,?,?,?,?,?)",
		s.accepted.Acceptance.AcceptanceID, digest, string(raw), head, head, "parent", 0, "2099-01-01T00:00:00.000000+00:00")
	return s
}

// Evaluation d2: the first pre-merge record of an acceptance, attached after a base refresh, is stored for the refreshed head. The entry takes the identity of the head the record was stored for: the
// refresh's generation and event for the refreshed head, the acceptance's own for its own head, and none for a head neither names.
func TestActivityPremergeRecordNamesTheIdentityOfItsHead(t *testing.T) {
	t.Run("the refreshed head", func(t *testing.T) {
		s := premergeEvidenceScenario(t, func(s *refreshScenario) string { return s.head })
		e := s.activity("g").node("I").Evidence
		if e == nil || e.Source != EvidencePremerge || e.Change != s.head {
			t.Fatalf("evidence = %+v, want the pre-merge record of the refreshed head", e)
		}
		if !e.HasGeneration || e.Generation != 2 || e.EventID != s.event2 || e.RelationshipID != s.rid {
			t.Errorf("the record of the refreshed head names relationship %s generation %d (%v) event %s, want generation 2 and the refresh's event %s", e.RelationshipID, e.Generation, e.HasGeneration, e.EventID, s.event2)
		}
	})
	t.Run("the head of the acceptance", func(t *testing.T) {
		s := premergeEvidenceScenario(t, func(s *refreshScenario) string { return s.h1 })
		e := s.activity("g").node("I").Evidence
		if e == nil || e.Source != EvidencePremerge || !e.HasGeneration || e.Generation != 1 || e.EventID != s.accepted.Event {
			t.Errorf("the record of the accepted head = %+v, want the acceptance's generation 1 and event %s", e, s.accepted.Event)
		}
	})
	t.Run("a head neither names", func(t *testing.T) {
		s := premergeEvidenceScenario(t, func(*refreshScenario) string { return "headunknown" })
		e := s.activity("g").node("I").Evidence
		if e == nil || e.Source != EvidencePremerge || e.HasGeneration || e.EventID != "" || e.RelationshipID != s.rid {
			t.Errorf("the record of an unnamed head = %+v, want no generation and no event", e)
		}
	})
}

// Evaluation d3: the pre-merge record a re-validation was judged on (dag_revalidation_premerge) is the record integration reads once the criteria were re-registered, and is stored after the
// re-validation row itself. It is evidence, the newest here, and names the identity of the head it was stored for.
func TestActivityRevalidationPremergeRecordIsEvidence(t *testing.T) {
	s := newRefreshScenario(t)
	s.openGeneration()
	s.refreshBase()
	if _, err := s.record("shared.json"); err != nil {
		t.Fatal(err)
	}
	other := dig("the second edition of I's criteria after the refresh")
	s.rvReregister("g", "I", "g-r3", s.rid, other, nil)
	s.rvRule(s.rid, "verified", other, rvVerified())
	res, err := s.accept("g", "I", AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 7}, Premerge: premergeAt(s.sched, context.Background(), "g", "I", s.head)})
	if err != nil || !res.Revalidated {
		t.Fatalf("revalidation = %+v %v", res, err)
	}
	s.exec("UPDATE dag_acceptance_revalidations SET revalidated_at = '2099-01-01T00:00:00.000000+00:00' WHERE acceptance_id = ?", res.AcceptanceID)
	s.exec("DROP TRIGGER dag_revalidation_premerge_no_update")
	s.exec("UPDATE dag_revalidation_premerge SET recorded_at = '2099-01-01T00:00:01.000000+00:00' WHERE acceptance_id = ?", res.AcceptanceID)
	var digest, evaluated string
	if err := s.s.DB.QueryRow("SELECT record_digest, evaluated_head FROM dag_revalidation_premerge WHERE acceptance_id = ?", res.AcceptanceID).Scan(&digest, &evaluated); err != nil {
		t.Fatal(err)
	}
	e := s.activity("g").node("I").Evidence
	if e == nil || e.Source != EvidenceRevalidationPremerge || e.RecordedAt != "2099-01-01T00:00:01.000000+00:00" || e.Ref != digest || e.Change != evaluated {
		t.Fatalf("evidence = %+v, want the re-validation's pre-merge record %s of head %s recorded after the re-validation", e, digest, evaluated)
	}
	if !e.HasGeneration || e.Generation != 2 || e.EventID != s.event2 {
		t.Errorf("the record of the refreshed head names generation %d (%v) and event %s, want the refresh's generation 2 and event %s", e.Generation, e.HasGeneration, e.EventID, s.event2)
	}
}

// TestHeadIdentity verifies that headIdentity resolves unique identities, detects ambiguity when multiple
// generations or events point at the same head, and deduplicates identical identities.
func TestHeadIdentity(t *testing.T) {
	tests := []struct {
		name           string
		head           string
		ownHead        string
		own            refreshRow
		refreshes      []refreshRow
		wantGeneration int64
		wantEvent      string
		wantOK         bool
	}{
		{
			name:           "the head of the acceptance",
			head:           "head-own",
			ownHead:        "head-own",
			own:            refreshRow{acceptanceID: "acc-1", generation: 1, eventID: "evt-own"},
			wantGeneration: 1,
			wantEvent:      "evt-own",
			wantOK:         true,
		},
		{
			name:    "the refreshed head",
			head:    "head-ref",
			ownHead: "head-own",
			own:     refreshRow{acceptanceID: "acc-1", generation: 1, eventID: "evt-own"},
			refreshes: []refreshRow{
				{acceptanceID: "acc-1", head: "head-ref", generation: 2, eventID: "evt-ref"},
			},
			wantGeneration: 2,
			wantEvent:      "evt-ref",
			wantOK:         true,
		},
		{
			name:    "a head neither names",
			head:    "head-unknown",
			ownHead: "head-own",
			own:     refreshRow{acceptanceID: "acc-1", generation: 1, eventID: "evt-own"},
			refreshes: []refreshRow{
				{acceptanceID: "acc-1", head: "head-ref", generation: 2, eventID: "evt-ref"},
			},
			wantGeneration: 0,
			wantEvent:      "",
			wantOK:         false,
		},
		{
			name:    "same head pointed at by two different generations",
			head:    "head-1",
			ownHead: "head-1",
			own:     refreshRow{acceptanceID: "acc-1", generation: 1, eventID: "evt-1"},
			refreshes: []refreshRow{
				{acceptanceID: "acc-1", head: "head-1", generation: 2, eventID: "evt-1"},
			},
			wantGeneration: 0,
			wantEvent:      "",
			wantOK:         false,
		},
		{
			name:    "same head pointed at by two different generations across refreshes",
			head:    "head-2",
			ownHead: "head-1",
			own:     refreshRow{acceptanceID: "acc-1", generation: 1, eventID: "evt-1"},
			refreshes: []refreshRow{
				{acceptanceID: "acc-1", head: "head-2", generation: 2, eventID: "evt-2"},
				{acceptanceID: "acc-1", head: "head-2", generation: 3, eventID: "evt-2"},
			},
			wantGeneration: 0,
			wantEvent:      "",
			wantOK:         false,
		},
		{
			name:    "same head pointed at by two different events",
			head:    "head-1",
			ownHead: "head-1",
			own:     refreshRow{acceptanceID: "acc-1", generation: 1, eventID: "evt-1"},
			refreshes: []refreshRow{
				{acceptanceID: "acc-1", head: "head-1", generation: 1, eventID: "evt-2"},
			},
			wantGeneration: 0,
			wantEvent:      "",
			wantOK:         false,
		},
		{
			name:    "same head pointed at by two different events across refreshes",
			head:    "head-2",
			ownHead: "head-1",
			own:     refreshRow{acceptanceID: "acc-1", generation: 1, eventID: "evt-1"},
			refreshes: []refreshRow{
				{acceptanceID: "acc-1", head: "head-2", generation: 2, eventID: "evt-2a"},
				{acceptanceID: "acc-1", head: "head-2", generation: 2, eventID: "evt-2b"},
			},
			wantGeneration: 0,
			wantEvent:      "",
			wantOK:         false,
		},
		{
			name:    "the same identity repeated (counted once)",
			head:    "head-1",
			ownHead: "head-1",
			own:     refreshRow{acceptanceID: "acc-1", generation: 1, eventID: "evt-1"},
			refreshes: []refreshRow{
				{acceptanceID: "acc-1", head: "head-1", generation: 1, eventID: "evt-1"},
			},
			wantGeneration: 1,
			wantEvent:      "evt-1",
			wantOK:         true,
		},
		{
			name:    "the same identity repeated across refreshes (counted once)",
			head:    "head-2",
			ownHead: "head-1",
			own:     refreshRow{acceptanceID: "acc-1", generation: 1, eventID: "evt-1"},
			refreshes: []refreshRow{
				{acceptanceID: "acc-1", head: "head-2", generation: 2, eventID: "evt-2"},
				{acceptanceID: "acc-1", head: "head-2", generation: 2, eventID: "evt-2"},
			},
			wantGeneration: 2,
			wantEvent:      "evt-2",
			wantOK:         true,
		},
		{
			name:    "refresh from a different acceptance is ignored",
			head:    "head-ref",
			ownHead: "head-own",
			own:     refreshRow{acceptanceID: "acc-1", generation: 1, eventID: "evt-own"},
			refreshes: []refreshRow{
				{acceptanceID: "acc-other", head: "head-ref", generation: 2, eventID: "evt-ref"},
			},
			wantGeneration: 0,
			wantEvent:      "",
			wantOK:         false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotGeneration, gotEvent, gotOK := headIdentity(tc.head, tc.ownHead, tc.own, tc.refreshes)
			if gotGeneration != tc.wantGeneration || gotEvent != tc.wantEvent || gotOK != tc.wantOK {
				t.Fatalf("headIdentity(%q, %q, %+v, %+v) = (%d, %q, %v), want (%d, %q, %v)",
					tc.head, tc.ownHead, tc.own, tc.refreshes,
					gotGeneration, gotEvent, gotOK, tc.wantGeneration, tc.wantEvent, tc.wantOK)
			}
		})
	}
}
