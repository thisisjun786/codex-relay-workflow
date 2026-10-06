package manage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	// The fixture builds a real store, so this test binary links internal/relay/store and must
	// also link internal/testsupport: that package refuses a database below a live relay state
	// directory before any TestMain runs, so a test that forgot isolation is refused rather than
	// reading the operator's live state.
	_ "github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// The fixture clock. Every row a test writes is placed relative to one base instant, so a
// threshold test reads as "inside the window" or "past it" rather than as an absolute date.
const checkpointBaseInstant = "2026-10-06T00:00:00.000000+00:00"

// checkpointNow is the instant every fixture test reads at: one day after the base, so a
// checkpoint record can be many hours old.
func checkpointNow() time.Time {
	base, err := time.Parse(time.RFC3339Nano, checkpointBaseInstant)
	if err != nil {
		panic(err)
	}
	return base.Add(24 * time.Hour)
}

// checkpointAt is the fixture instant hours after the base, in the relay's own timestamp form.
func checkpointAt(hours float64) string {
	base, err := time.Parse(time.RFC3339Nano, checkpointBaseInstant)
	if err != nil {
		panic(err)
	}
	return base.Add(time.Duration(hours * float64(time.Hour))).UTC().Format("2006-01-02T15:04:05.000000+00:00")
}

// checkpointFixture is a temporary relay store built row by row through the exported store
// schema, so the fixture has the real store shape and internal/relay is never edited to make a
// test possible.
type checkpointFixture struct {
	t     *testing.T
	dir   string
	path  string
	store *store.Store
}

func checkpointNewFixture(t *testing.T) *checkpointFixture {
	t.Helper()
	coreTempHome(t)
	dir := t.TempDir()
	path := filepath.Join(dir, checkpointStoreFile)
	st, err := store.Open(context.Background(), path, filepath.Join(dir, "app-server-control.sock"))
	if err != nil {
		t.Fatalf("create the fixture store: %v", err)
	}
	f := &checkpointFixture{t: t, dir: dir, path: path, store: st}
	t.Cleanup(f.close)
	return f
}

func (f *checkpointFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.store.DB.Exec(query, args...); err != nil {
		f.t.Fatalf("fixture insert: %v\n%s", err, query)
	}
}

// close releases the writer, so the read-only checkpoint under test sees a settled file.
func (f *checkpointFixture) close() {
	f.t.Helper()
	if f.store == nil {
		return
	}
	if err := f.store.Close(); err != nil {
		f.t.Fatalf("close the fixture store: %v", err)
	}
	f.store = nil
}

// scope records that a relationship belongs to a project, which is how every count is
// attributed to one.
func (f *checkpointFixture) scope(relationshipID, project string) {
	f.exec("INSERT INTO relationship_scope (relationship_id, project_key, recorded_at) VALUES (?,?,?)",
		relationshipID, project, checkpointAt(0))
}

// mergeTurn records one merge turn. A landed turn carries the instant it landed.
func (f *checkpointFixture) mergeTurn(turnID, project, state, at string) {
	f.exec("INSERT INTO merge_turns (turn_id, target_key, repository, base_ref, project_key, holder_task_id, holder_host_id, candidate_head, state, tenure, requested_at, closed_at, updated_at) VALUES (?,?,'owner/repo','dev',?,'parent','host','head',?,1,?,?,?)",
		turnID, "owner/repo#dev", project, state, at, checkpointNull(at), at)
}

// verdict records one ruling on an event of a relationship, which is how a project's
// needs_changes count is attributed.
func (f *checkpointFixture) verdict(eventID, relationshipID, verdict, at string) {
	f.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, first_seen_at, last_seen_at) VALUES (?,?,1,'revision','ready_for_review','child','thread','turn','completed','{}',?,?)",
		eventID, relationshipID, at, at)
	f.exec("INSERT INTO verdicts (event_id, record, verdict, verdict_turn_id, decided_at) VALUES (?,'{}',?,'turn',?)",
		eventID, verdict, at)
}

// decision records one decision reply on a relationship: the receipt is what the count reads.
func (f *checkpointFixture) decision(eventID, relationshipID, decision, at string) {
	receipt, err := json.Marshal(map[string]any{"decision": decision, "decidedAt": at})
	if err != nil {
		f.t.Fatal(err)
	}
	f.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, first_seen_at, last_seen_at) VALUES (?,?,1,'revision','decision_reply','relay','thread','turn','completed',?,?,?)",
		eventID, relationshipID, string(receipt), at, at)
}

// acceptance records one active acceptance of a plan's node, which an integration observation is
// made about. A landed merge carries the parent's merged mark on the acceptance's event,
// generation and revision, which the scheduler's integration rule requires beside the
// observation.
func (f *checkpointFixture) acceptance(planID, node, relationshipID, acceptanceID, event, head, at string) {
	f.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state) VALUES (?,?,?,'manifest',?,1,?,?,'criteria','verified',?,'verified','turn','{}','parent',0,?,'active')",
		acceptanceID, planID, node, relationshipID, event, "revision-"+acceptanceID, head, at)
}

// mark records the parent's merged mark on an acceptance, which is what makes an observation an
// integration rather than a mere ancestry reading.
func (f *checkpointFixture) mark(relationshipID, event, acceptanceID, at string) {
	f.exec("INSERT INTO assignment_marks (relationship_id, mark, event_id, execution_generation, revision_hash, evidence, actor, marked_at) VALUES (?,'merged',?,1,?,'{}','parent',?)",
		relationshipID, event, "revision-"+acceptanceID, at)
}

// observation records one ancestry observation of an acceptance in the target branch. seq orders
// the observations of one acceptance and target, which is how a later one supersedes an earlier.
func (f *checkpointFixture) observation(acceptanceID, head, repository, baseRef, at string, seq int, isAncestor bool) {
	flag := 0
	if isAncestor {
		flag = 1
	}
	f.exec("INSERT INTO dag_integration_observations (observation_id, acceptance_id, repository, base_ref, subject_sha, tip_sha, is_ancestor, method, observed_seq, observed_at) VALUES (?,?,?,?,?,'tip',?,'ancestry',?,?)",
		fmt.Sprintf("observation-%s-%s-%s-%d", acceptanceID, repository, baseRef, seq), acceptanceID, repository, baseRef, head, flag, seq, at)
}

// checkpointDigest is a stand-in for a digest: 64 lowercase hex characters derived from a name.
func checkpointDigest(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

// checkpointNodeDoc is a plan node's document: its id, the issue it carries, its kind and a
// criteria digest derived from the id.
func checkpointNodeDoc(id, kind string) map[string]any {
	return map[string]any{"node_id": id, "issue_key": "CRW-" + id, "kind": kind, "criteria_set_digest": checkpointDigest("criteria " + id)}
}

// plan registers a real DAG plan revision through dag.Repo.Put, so the reading's call to
// dagsched.ExecutionIntegrated — which re-derives the plan's slice and state digests — can read
// it. A store written by hand cannot serve that call: the scheduler verifies the plan's rows
// against its log.
//
// node is an implementation node with one integrated edge per target, so the node's targets are
// exactly the ones the caller names, which is what makes an acceptance's head required in every
// one of them.
func (f *checkpointFixture) plan(planID, project, node string, targets ...[2]string) {
	f.t.Helper()
	changes := []any{map[string]any{"op": "add_node", "node": checkpointNodeDoc(node, "implementation")}}
	for i, target := range targets {
		successor := fmt.Sprintf("%s-%d", node, i)
		changes = append(changes,
			map[string]any{"op": "add_node", "node": checkpointNodeDoc(successor, "non_pr")},
			map[string]any{"op": "add_edge", "edge": map[string]any{
				"edge_id": fmt.Sprintf("%s-%d", node, i), "from_node_id": node, "to_node_id": successor,
				"kind": "integrated", "target_repository": target[0], "target_base_ref": target[1],
			}})
	}
	document := map[string]any{
		"schema": "dag-plan-revision/1", "plan_id": planID, "project_key": project,
		"request_id": planID + "-request-1", "expected_parent_revision": 0, "author_task_id": "parent",
		"changes": changes,
	}
	raw, err := json.Marshal(document)
	if err != nil {
		f.t.Fatal(err)
	}
	revision, err := dag.DecodeRevision(raw)
	if err != nil {
		f.t.Fatalf("decode the plan revision: %v", err)
	}
	repo := &dag.Repo{Store: f.store, Now: func() string { return checkpointAt(0) }}
	if _, err := repo.Put(context.Background(), revision); err != nil {
		f.t.Fatalf("put the plan revision: %v", err)
	}
}

// execution records that a relationship executed a plan's node, which is what makes the
// scheduler's ExecutionIntegrated applicable to it.
func (f *checkpointFixture) execution(planID, node, relationshipID string) {
	f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind) VALUES (?,?,?,1,'manifest','initial')",
		planID, node, relationshipID)
}

// checkpointManageStateDir is where the manage configuration's state directory falls with
// nothing configured: the default below HOME, which the reading and the record command both use.
func checkpointManageStateDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(os.Getenv("HOME"), ".local", "state", "crw", "manage")
}

// record writes a checkpoint record line for a project, which is the baseline the next
// calculation counts after.
func (f *checkpointFixture) record(project, at, summary string) {
	f.t.Helper()
	dir := filepath.Join(checkpointManageStateDir(f.t), "checkpoint")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(summary))
	line, err := json.Marshal(map[string]any{
		"project": project, "at": at, "summary_sha256": hex.EncodeToString(sum[:]), "summary": summary,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(dir, project+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		file.Close()
		f.t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		f.t.Fatal(err)
	}
}

func checkpointNull(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// checkpointEnv is the Env a fixture test runs with: discarded streams and the fixture clock.
func checkpointEnv(t *testing.T) *Env {
	t.Helper()
	return &Env{
		Stdin:      strings.NewReader(""),
		Stdout:     io.Discard,
		Stderr:     io.Discard,
		Getenv:     os.Getenv,
		Now:        checkpointNow,
		Executable: "crw",
	}
}

// checkpointConfig points a reading at the fixture state directory, with the checkpoint
// section a test asks for.
func checkpointConfig(t *testing.T, state string, thresholds map[string]any) *Config {
	t.Helper()
	cfg := &Config{Relay: coreRelay{State: state}, raw: map[string]json.RawMessage{}}
	if len(thresholds) > 0 {
		encoded, err := json.Marshal(thresholds)
		if err != nil {
			t.Fatal(err)
		}
		cfg.raw["checkpoint"] = encoded
	}
	return cfg
}

// checkpointRead runs the reading over the fixture and fails the test on a read error.
func checkpointRead(t *testing.T, f *checkpointFixture, opts CheckpointOptions, thresholds map[string]any) []CheckpointReport {
	t.Helper()
	reports, err := Checkpoint(context.Background(), checkpointEnv(t), checkpointConfig(t, f.dir, thresholds), opts)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	return reports
}

// checkpointReport returns the one report for a project.
func checkpointReport(t *testing.T, reports []CheckpointReport, project string) CheckpointReport {
	t.Helper()
	for _, report := range reports {
		if report.Project == project {
			return report
		}
	}
	t.Fatalf("no report for %s: %+v", project, reports)
	return CheckpointReport{}
}

// checkpointHasReason reports whether a report carries one due reason.
func checkpointHasReason(report CheckpointReport, reason string) bool {
	for _, got := range report.Reasons {
		if got == reason {
			return true
		}
	}
	return false
}

// checkpointHasUnmeasured reports whether a report names one unmeasured signal.
func checkpointHasUnmeasured(report CheckpointReport, signal string) bool {
	for _, got := range report.Unmeasured {
		if got == signal {
			return true
		}
	}
	return false
}

// checkpointWriteInput writes a fixture input file and returns its path.
func checkpointWriteInput(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// C1 red test: with twenty merges landed since the last checkpoint record, the project is due
// with the reason merges_since_checkpoint. This is the issue's own first test, and it fails
// while the command does not exist.
func TestCheckpointTwentyMergesSinceTheRecordIsDue(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	for i := 0; i < 20; i++ {
		f.mergeTurn(fmt.Sprintf("turn-%02d", i), "project-1", "landed", checkpointAt(2+float64(i)/60))
	}
	f.close()
	f.record("project-1", checkpointAt(1), "the first checkpoint")

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{Project: "project-1"}, nil), "project-1")
	if !report.Due {
		t.Fatalf("twenty merges after the record are not due: %+v", report)
	}
	if !checkpointHasReason(report, checkpointSignalMerges) {
		t.Errorf("the reasons do not carry %s: %+v", checkpointSignalMerges, report.Reasons)
	}
	if report.Counts.MergesSinceCheckpoint != 20 {
		t.Errorf("merges = %d, want 20", report.Counts.MergesSinceCheckpoint)
	}
	if report.Since != checkpointAt(1) {
		t.Errorf("since = %q, want the record's instant", report.Since)
	}
}

// C1: each of the six signals makes the project due on its own, with its own reason.
func TestCheckpointEachSignalIsDueOnItsOwn(t *testing.T) {
	t.Run(checkpointSignalMerges, func(t *testing.T) {
		f := checkpointNewFixture(t)
		for i := 0; i < 20; i++ {
			f.mergeTurn(fmt.Sprintf("turn-%02d", i), "project-1", "landed", checkpointAt(2))
		}
		f.close()
		f.record("project-1", checkpointAt(1), "record")
		report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
		if !report.Due || !checkpointHasReason(report, checkpointSignalMerges) {
			t.Fatalf("merges did not fire: %+v", report)
		}
	})

	t.Run(checkpointSignalRunningHours, func(t *testing.T) {
		f := checkpointNewFixture(t)
		f.scope("relationship-1", "project-1")
		f.close()
		f.record("project-1", checkpointAt(10), "record")
		report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
		if !report.Due || !checkpointHasReason(report, checkpointSignalRunningHours) {
			t.Fatalf("running_hours did not fire: %+v", report)
		}
	})

	t.Run(checkpointSignalNeedsChanges, func(t *testing.T) {
		f := checkpointNewFixture(t)
		f.scope("relationship-1", "project-1")
		for i := 0; i < 3; i++ {
			f.verdict(fmt.Sprintf("event-%d", i), "relationship-1", "needs_changes", checkpointAt(23))
		}
		f.close()
		report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
		if !report.Due || !checkpointHasReason(report, checkpointSignalNeedsChanges) {
			t.Fatalf("needs_changes did not fire: %+v", report)
		}
	})

	t.Run("split_decisions", func(t *testing.T) {
		f := checkpointNewFixture(t)
		f.scope("relationship-1", "project-1")
		f.verdict("event-0", "relationship-1", "needs_changes", checkpointAt(23))
		for i := 1; i < 3; i++ {
			f.decision(fmt.Sprintf("decision-%d", i), "relationship-1", "split_approval", checkpointAt(23))
		}
		f.close()
		report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
		if !report.Due || !checkpointHasReason(report, checkpointSignalNeedsChanges) {
			t.Fatalf("split decisions did not fire: %+v", report)
		}
		if report.Counts.SplitDecisionsSinceCheckpoint != 2 {
			t.Errorf("split decisions = %d, want 2", report.Counts.SplitDecisionsSinceCheckpoint)
		}
	})

	t.Run(checkpointSignalPairEval, func(t *testing.T) {
		f := checkpointNewFixture(t)
		f.scope("relationship-1", "project-1")
		f.close()
		f.record("project-1", checkpointAt(1), "record")
		pairEval := checkpointWriteInput(t, t.TempDir(), "pair-eval.jsonl",
			"{\"project\":\"project-1\",\"severity\":\"P1\",\"at\":\""+checkpointAt(2)+"\"}\n"+
				"{\"project\":\"project-1\",\"severity\":\"P0\",\"at\":\""+checkpointAt(3)+"\"}\n"+
				"{\"project\":\"project-1\",\"severity\":\"P2\",\"at\":\""+checkpointAt(4)+"\"}\n")
		report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{PairEval: pairEval}, nil), "project-1")
		if !report.Due || !checkpointHasReason(report, checkpointSignalPairEval) {
			t.Fatalf("pair evaluation did not fire: %+v", report)
		}
		if report.Counts.PairEvalP0P1SinceCheckpoint == nil || *report.Counts.PairEvalP0P1SinceCheckpoint != 2 {
			t.Errorf("pair P0/P1 = %v, want 2", report.Counts.PairEvalP0P1SinceCheckpoint)
		}
	})

	t.Run(checkpointSignalBacklog, func(t *testing.T) {
		f := checkpointNewFixture(t)
		f.scope("relationship-1", "project-1")
		f.close()
		var issues []string
		for i := 0; i < 16; i++ {
			issues = append(issues, fmt.Sprintf("{\"identifier\":\"CRW-%d\",\"project\":\"project-1\",\"createdAt\":\"%s\",\"state\":\"Backlog\"}", 900+i, checkpointAt(23)))
		}
		export := checkpointWriteInput(t, t.TempDir(), "linear-export.json",
			"{\"issues\":["+strings.Join(issues, ",")+"]}")
		report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{LinearExport: export}, nil), "project-1")
		if !report.Due || !checkpointHasReason(report, checkpointSignalBacklog) {
			t.Fatalf("backlog did not fire: %+v", report)
		}
		if report.Counts.BacklogNet4h == nil || *report.Counts.BacklogNet4h != 16 {
			t.Errorf("backlog net = %v, want 16", report.Counts.BacklogNet4h)
		}
	})

	t.Run(checkpointSignalMilestone, func(t *testing.T) {
		f := checkpointNewFixture(t)
		f.scope("relationship-1", "project-1")
		f.close()
		f.record("project-1", checkpointAt(1), "record")
		export := checkpointWriteInput(t, t.TempDir(), "linear-export.json",
			"{\"issues\":["+
				"{\"identifier\":\"CRW-901\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(2)+"\",\"state\":\"Done\",\"milestone\":\"M2\",\"completedAt\":\""+checkpointAt(3)+"\"},"+
				"{\"identifier\":\"CRW-902\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(2)+"\",\"state\":\"Done\",\"milestone\":\"M2\",\"completedAt\":\""+checkpointAt(4)+"\"}]}")
		report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{LinearExport: export}, nil), "project-1")
		if !report.Due || !checkpointHasReason(report, checkpointSignalMilestone) {
			t.Fatalf("milestone did not fire: %+v", report)
		}
	})
}

// C1: a count below its threshold is not due.
func TestCheckpointBelowTheThresholdIsNotDue(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	for i := 0; i < 19; i++ {
		f.mergeTurn(fmt.Sprintf("turn-%02d", i), "project-1", "landed", checkpointAt(21))
	}
	f.verdict("event-0", "relationship-1", "needs_changes", checkpointAt(23))
	f.verdict("event-1", "relationship-1", "needs_changes", checkpointAt(23))
	f.close()
	// The record is four hours old, so running_hours is below its threshold too.
	f.record("project-1", checkpointAt(20), "record")

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
	if report.Due {
		t.Fatalf("a project below every threshold is due: %+v", report)
	}
	if len(report.Reasons) != 0 {
		t.Errorf("a project that is not due carries reasons: %+v", report.Reasons)
	}
	if report.Counts.MergesSinceCheckpoint != 19 {
		t.Errorf("merges = %d, want 19", report.Counts.MergesSinceCheckpoint)
	}
}

// C2: an absent input file leaves its signal unmeasured and out of the due decision.
func TestCheckpointAbsentInputFilesAreUnmeasured(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.close()
	f.record("project-1", checkpointAt(1), "record")

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
	for _, signal := range []string{checkpointSignalMilestone, checkpointSignalPairEval, checkpointSignalBacklog} {
		if !checkpointHasUnmeasured(report, signal) {
			t.Errorf("%s is not unmeasured without its input file: %+v", signal, report.Unmeasured)
		}
		if checkpointHasReason(report, signal) {
			t.Errorf("%s decided due without its input file: %+v", signal, report.Reasons)
		}
	}
	if report.Counts.PairEvalP0P1SinceCheckpoint != nil || report.Counts.BacklogNet4h != nil || report.Counts.MilestoneIntegrated != nil {
		t.Errorf("an unmeasured signal carries a count: %+v", report.Counts)
	}
}

// C3: a record leaves the baseline, and the next calculation counts only what happened after
// it.
func TestCheckpointRecordBecomesTheNextBaseline(t *testing.T) {
	f := checkpointNewFixture(t)
	state := f.dir
	f.scope("relationship-1", "project-1")
	for i := 0; i < 20; i++ {
		f.mergeTurn(fmt.Sprintf("turn-%02d", i), "project-1", "landed", checkpointAt(21))
	}
	f.close()

	before := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
	if !before.Due || before.Counts.MergesSinceCheckpoint != 20 {
		t.Fatalf("with no record the whole history was not counted: %+v", before)
	}
	if before.Since != "" {
		t.Errorf("since = %q with no record, want empty", before.Since)
	}

	// The record command stamps its own instant, so it is run with a clock two hours before the
	// reading: the baseline then lands between the merges that predate it and the one that follows.
	stdout, stderr, code := checkpointRunCommandAt(t, state, []string{
		"record", "--project", "project-1", "--summary-file",
		checkpointWriteInput(t, t.TempDir(), "summary.md", "the mid-project checkpoint"),
	}, checkpointRecordClock)
	if code != 0 {
		t.Fatalf("record exited %d: %s%s", code, stdout, stderr)
	}

	after := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
	if after.Since == "" {
		t.Fatalf("the record did not become the baseline: %+v", after)
	}
	if after.Counts.MergesSinceCheckpoint != 0 {
		t.Errorf("merges after the record = %d, want 0", after.Counts.MergesSinceCheckpoint)
	}
	if after.Due {
		t.Errorf("a project whose merges all predate the record is due: %+v", after)
	}

	// A merge that lands after the record is counted again.
	f2 := &checkpointFixture{t: t, dir: state, path: filepath.Join(state, checkpointStoreFile)}
	st, err := store.Open(context.Background(), f2.path, filepath.Join(state, "app-server-control.sock"))
	if err != nil {
		t.Fatalf("reopen the fixture store: %v", err)
	}
	f2.store = st
	f2.mergeTurn("turn-later", "project-1", "landed", checkpointAt(23))
	f2.close()

	reopened := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
	if reopened.Counts.MergesSinceCheckpoint != 1 {
		t.Errorf("merges after the record = %d, want 1", reopened.Counts.MergesSinceCheckpoint)
	}
}

// C3: the record file itself carries the summary, its digest and the project.
func TestCheckpointRecordWritesTheSummaryDigest(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.close()
	summary := strings.Repeat("가", 2500)
	if _, stderr, code := checkpointRunCommand(t, f.dir, []string{
		"record", "--project", "project-1", "--summary-file",
		checkpointWriteInput(t, t.TempDir(), "summary.md", summary),
	}); code != 0 {
		t.Fatalf("record exited %d: %s", code, stderr)
	}
	path := filepath.Join(checkpointManageStateDir(t), "checkpoint", "project-1.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the record file: %v", err)
	}
	row := map[string]any{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &row); err != nil {
		t.Fatalf("the record line is not JSON: %v\n%s", err, data)
	}
	sum := sha256.Sum256([]byte(summary))
	if row["project"] != "project-1" || row["summary_sha256"] != hex.EncodeToString(sum[:]) {
		t.Errorf("the record row is wrong: %+v", row)
	}
	got, _ := row["summary"].(string)
	if len([]rune(got)) != checkpointSummaryLimit {
		t.Errorf("the summary is %d characters, want %d", len([]rune(got)), checkpointSummaryLimit)
	}
	at, _ := row["at"].(string)
	if _, err := time.Parse("2006-01-02T15:04:05.000000+00:00", at); err != nil {
		t.Errorf("at %q is not the relay instant form: %v", at, err)
	}
}

// C4: reading the store leaves the file's bytes and mtime exactly as they were and creates no
// sidecar.
func TestCheckpointReadLeavesTheStoreFileUnchanged(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.mergeTurn("turn-1", "project-1", "landed", checkpointAt(2))
	f.close()

	before := checkpointFileState(t, f.path)
	beforeSidecars := checkpointSidecars(t, f.path)
	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
	if report.Counts.MergesSinceCheckpoint != 1 {
		t.Fatalf("the reading did not happen: %+v", report.Counts)
	}
	if after := checkpointFileState(t, f.path); after != before {
		t.Errorf("the read changed the store file:\n before %s\n after  %s", before, after)
	}
	if after := checkpointSidecars(t, f.path); len(after) != len(beforeSidecars) {
		t.Errorf("the read created a sidecar: before %v after %v", beforeSidecars, after)
	}
}

// The DAG zone is additive, so a store that predates it is still read: the integration count is
// unmeasured and the run does not fail.
func TestCheckpointStoreWithoutTheDAGZoneIsStillRead(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.mergeTurn("turn-1", "project-1", "landed", checkpointAt(2))
	for _, table := range []string{"dag_integration_observations", "dag_acceptances", "dag_plans"} {
		f.exec("DROP TABLE " + table)
	}
	f.close()

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
	if report.Counts.IntegrationsSinceCheckpoint != nil {
		t.Errorf("the integration count is measured without its tables: %+v", report.Counts)
	}
	if !checkpointHasUnmeasured(report, checkpointSignalIntegrations) {
		t.Errorf("the integration reading is not unmeasured: %+v", report.Unmeasured)
	}
	if report.Counts.MergesSinceCheckpoint != 1 {
		t.Errorf("the frozen tables were not read: %+v", report.Counts)
	}
}

// checkpointIntegrationFixture builds the one-integration world both integration tests share: a
// real plan whose node has the given targets, a relationship that executed it, one active
// acceptance of it, and the parent's merged mark on the acceptance's own event and revision. The
// caller records the observations, which are the only thing that differs between the cases.
func checkpointIntegrationFixture(t *testing.T, targets ...[2]string) *checkpointFixture {
	t.Helper()
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.plan("plan-1", "project-1", "node", targets...)
	f.execution("plan-1", "node", "relationship-1")
	f.acceptance("plan-1", "node", "relationship-1", "acceptance-1", "event-1", "head-1", checkpointAt(1))
	f.mark("relationship-1", "event-1", "acceptance-1", checkpointAt(2))
	return f
}

// An ancestry observation is not an integration until the parent's merged mark stands on the
// acceptance's event, generation and revision; one marked acceptance is counted once however many
// times it was observed in one target.
func TestCheckpointIntegrationCountRequiresTheMergedMark(t *testing.T) {
	unmarked := checkpointNewFixture(t)
	unmarked.scope("relationship-1", "project-1")
	unmarked.plan("plan-1", "project-1", "node", [2]string{"owner/repo", "dev"})
	unmarked.execution("plan-1", "node", "relationship-1")
	unmarked.acceptance("plan-1", "node", "relationship-1", "acceptance-1", "event-1", "head-1", checkpointAt(1))
	unmarked.observation("acceptance-1", "head-1", "owner/repo", "dev", checkpointAt(3), 1, true)
	unmarked.observation("acceptance-1", "head-1", "owner/repo", "dev", checkpointAt(4), 2, true)
	unmarked.close()

	report := checkpointReport(t, checkpointRead(t, unmarked, CheckpointOptions{}, nil), "project-1")
	if report.Counts.IntegrationsSinceCheckpoint == nil || *report.Counts.IntegrationsSinceCheckpoint != 0 {
		t.Fatalf("an unmarked observation counted as an integration: %+v", report.Counts)
	}

	marked := checkpointIntegrationFixture(t, [2]string{"owner/repo", "dev"})
	marked.observation("acceptance-1", "head-1", "owner/repo", "dev", checkpointAt(3), 1, true)
	marked.observation("acceptance-1", "head-1", "owner/repo", "dev", checkpointAt(4), 2, true)
	marked.close()

	report = checkpointReport(t, checkpointRead(t, marked, CheckpointOptions{}, nil), "project-1")
	if report.Counts.IntegrationsSinceCheckpoint == nil || *report.Counts.IntegrationsSinceCheckpoint != 1 {
		t.Fatalf("a marked acceptance was not counted once: %+v", report.Counts)
	}
}

// A later observation saying the head is not contained supersedes the earlier positive one, so
// the acceptance is not integrated.
func TestCheckpointIntegrationCountIsClearedByALaterNegativeObservation(t *testing.T) {
	f := checkpointIntegrationFixture(t, [2]string{"owner/repo", "dev"})
	f.observation("acceptance-1", "head-1", "owner/repo", "dev", checkpointAt(3), 1, true)
	f.observation("acceptance-1", "head-1", "owner/repo", "dev", checkpointAt(4), 2, false)
	f.close()

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
	if report.Counts.IntegrationsSinceCheckpoint == nil || *report.Counts.IntegrationsSinceCheckpoint != 0 {
		t.Fatalf("a superseded observation counted as an integration: %+v", report.Counts)
	}
}

// C5: the relay scheduler requires every target of a node. An acceptance observed in only one of
// two targets is not integrated, and one observed in both is counted once. The instant is the
// latest of the per-target first satisfying observations.
func TestCheckpointIntegrationNeedsEveryTarget(t *testing.T) {
	f := checkpointIntegrationFixture(t, [2]string{"owner/repo", "dev"}, [2]string{"owner/repo", "main"})
	// Observed in dev only: the node has not landed everywhere it has to.
	f.observation("acceptance-1", "head-1", "owner/repo", "dev", checkpointAt(3), 1, true)
	f.close()

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
	if report.Counts.IntegrationsSinceCheckpoint == nil || *report.Counts.IntegrationsSinceCheckpoint != 0 {
		t.Fatalf("an acceptance observed in one of two targets counted as an integration: %+v", report.Counts)
	}

	// The second target lands later: now every target contains the head, and the acceptance is
	// counted once.
	f2 := &checkpointFixture{t: t, dir: f.dir, path: f.path}
	st, err := store.Open(context.Background(), f2.path, filepath.Join(f.dir, "app-server-control.sock"))
	if err != nil {
		t.Fatalf("reopen the fixture store: %v", err)
	}
	f2.store = st
	f2.observation("acceptance-1", "head-1", "owner/repo", "main", checkpointAt(4), 1, true)
	f2.close()

	report = checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
	if report.Counts.IntegrationsSinceCheckpoint == nil || *report.Counts.IntegrationsSinceCheckpoint != 1 {
		t.Fatalf("an acceptance integrated in every target was not counted once: %+v", report.Counts)
	}
}

// An issue created and closed inside the backlog window changes the backlog by nothing, so it
// neither adds nor subtracts.
func TestCheckpointBacklogNetCountsCreationAndCompletionOnce(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.close()
	export := checkpointWriteInput(t, t.TempDir(), "linear-export.json",
		"{\"issues\":["+
			"{\"identifier\":\"CRW-1\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(21)+"\",\"state\":\"Backlog\"},"+
			"{\"identifier\":\"CRW-2\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(21)+"\",\"state\":\"Done\",\"completedAt\":\""+checkpointAt(22)+"\"}]}")

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{LinearExport: export}, nil), "project-1")
	if report.Counts.BacklogNet4h == nil || *report.Counts.BacklogNet4h != 1 {
		t.Fatalf("the backlog net is %v, want 1 (one entered, one entered and left)", report.Counts.BacklogNet4h)
	}
}

// C1: whether an issue left the backlog is its state, matched against the configured closed
// list. An issue that is closed counts as having left even when the export carries no
// completedAt: created inside the window it nets to zero, and created outside it leaves the
// signal unmeasured, because when it left is then unknown.
func TestCheckpointBacklogClosedByState(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.close()
	inside := checkpointWriteInput(t, t.TempDir(), "inside.json",
		"{\"issues\":[{\"identifier\":\"CRW-1\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(21)+"\",\"state\":\"Done\"}]}")

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{LinearExport: inside}, nil), "project-1")
	if report.Counts.BacklogNet4h == nil || *report.Counts.BacklogNet4h != 0 {
		t.Fatalf("a closed issue created inside the window did not net to zero: %v", report.Counts.BacklogNet4h)
	}
	if checkpointHasUnmeasured(report, checkpointSignalBacklog) {
		t.Errorf("a closed issue created inside the window left the signal unmeasured: %+v", report.Unmeasured)
	}

	outside := checkpointWriteInput(t, t.TempDir(), "outside.json",
		"{\"issues\":[{\"identifier\":\"CRW-2\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(1)+"\",\"state\":\"Canceled\"}]}")
	report = checkpointReport(t, checkpointRead(t, f, CheckpointOptions{LinearExport: outside}, nil), "project-1")
	if report.Counts.BacklogNet4h != nil {
		t.Fatalf("a closed issue with no completion instant created outside the window was measured: %v", report.Counts.BacklogNet4h)
	}
	if !checkpointHasUnmeasured(report, checkpointSignalBacklog) {
		t.Errorf("the backlog reading is not unmeasured: %+v", report.Unmeasured)
	}
	if report.UnmeasuredReasons[checkpointSignalBacklog] == "" {
		t.Errorf("the unmeasured backlog carries no reason: %+v", report.UnmeasuredReasons)
	}
}

// C1: an open issue's leftover completion instant is ignored, because the issue is still in the
// backlog; and a closed state matched case-insensitively is closed, while a custom name merely
// containing a closed word is not.
func TestCheckpointBacklogStateRules(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.close()
	export := checkpointWriteInput(t, t.TempDir(), "linear-export.json",
		"{\"issues\":["+
			// Open, but the export still carries a completion instant: it entered and stayed.
			"{\"identifier\":\"CRW-1\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(21)+"\",\"state\":\"In Progress\",\"completedAt\":\""+checkpointAt(22)+"\"},"+
			// Closed, matched case-insensitively.
			"{\"identifier\":\"CRW-2\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(21)+"\",\"state\":\"done\",\"completedAt\":\""+checkpointAt(22)+"\"},"+
			// Closed by the default list through a cancellation word.
			"{\"identifier\":\"CRW-3\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(21)+"\",\"state\":\"Duplicate\",\"completedAt\":\""+checkpointAt(22)+"\"}]}")

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{LinearExport: export}, nil), "project-1")
	// CRW-1 entered (+1) and its leftover instant is ignored; CRW-2 and CRW-3 each entered and
	// left inside the window (0 each).
	if report.Counts.BacklogNet4h == nil || *report.Counts.BacklogNet4h != 1 {
		t.Fatalf("the backlog net is %v, want 1 (the open issue entered and stayed)", report.Counts.BacklogNet4h)
	}
}

// C1: the closed-state list comes from the checkpoint section, so a project that closes issues in
// its own state name measures the backlog with it.
func TestCheckpointBacklogClosedStatesComeFromTheSection(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.close()
	export := checkpointWriteInput(t, t.TempDir(), "linear-export.json",
		"{\"issues\":[{\"identifier\":\"CRW-1\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(21)+"\",\"state\":\"Shipped\",\"completedAt\":\""+checkpointAt(22)+"\"}]}")

	// The default list does not know "Shipped", so the issue is still in the backlog.
	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{LinearExport: export}, nil), "project-1")
	if report.Counts.BacklogNet4h == nil || *report.Counts.BacklogNet4h != 1 {
		t.Fatalf("a state outside the default list was read as closed: %v", report.Counts.BacklogNet4h)
	}
	// The section's list makes it closed, so the issue entered and left inside the window.
	report = checkpointReport(t, checkpointRead(t, f, CheckpointOptions{LinearExport: export}, map[string]any{"closed_states": []string{"Shipped"}}), "project-1")
	if report.Counts.BacklogNet4h == nil || *report.Counts.BacklogNet4h != 0 {
		t.Fatalf("the configured closed list was not honoured: %v", report.Counts.BacklogNet4h)
	}
}

// A milestone whose every closed issue carries no completion instant cannot be compared with the
// baseline, so the reading is unmeasured rather than a measured zero — and the milestone is named.
func TestCheckpointMilestoneWithoutCompletionTimeIsUnmeasured(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.close()
	f.record("project-1", checkpointAt(1), "record")
	export := checkpointWriteInput(t, t.TempDir(), "linear-export.json",
		"{\"issues\":[{\"identifier\":\"CRW-1\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(2)+"\",\"state\":\"Done\",\"milestone\":\"M1\"}]}")

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{LinearExport: export}, nil), "project-1")
	if report.Counts.MilestoneIntegrated != nil {
		t.Fatalf("a milestone without completion timing was measured: %+v", report.Counts)
	}
	if !checkpointHasUnmeasured(report, checkpointSignalMilestone) {
		t.Errorf("the milestone reading is not unmeasured: %+v", report.Unmeasured)
	}
	if report.UnmeasuredReasons["M1"] == "" {
		t.Errorf("the skipped milestone is not named in the reasons: %+v", report.UnmeasuredReasons)
	}
}

// C4: one milestone whose completion instant is unknown no longer hides another milestone's
// confirmed integration. The confirmed milestone is counted and the unknown one is named.
func TestCheckpointUnknownMilestoneTimingDoesNotHideAnother(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.close()
	f.record("project-1", checkpointAt(1), "record")
	export := checkpointWriteInput(t, t.TempDir(), "linear-export.json",
		"{\"issues\":["+
			"{\"identifier\":\"CRW-1\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(2)+"\",\"state\":\"Done\",\"milestone\":\"M1\"},"+
			"{\"identifier\":\"CRW-2\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(2)+"\",\"state\":\"Done\",\"milestone\":\"M2\",\"completedAt\":\""+checkpointAt(3)+"\"}]}")

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{LinearExport: export}, nil), "project-1")
	if report.Counts.MilestoneIntegrated == nil || *report.Counts.MilestoneIntegrated != 1 {
		t.Fatalf("the confirmed milestone was not counted: %+v", report.Counts.MilestoneIntegrated)
	}
	if checkpointHasUnmeasured(report, checkpointSignalMilestone) {
		t.Errorf("the signal is unmeasured although a confirmed milestone was counted: %+v", report.Unmeasured)
	}
	if report.UnmeasuredReasons["M1"] == "" {
		t.Errorf("the skipped milestone is not named in the reasons: %+v", report.UnmeasuredReasons)
	}
}

// C4: the signal is unmeasured only when every fully closed milestone has unknown timing. A
// confirmed milestone whose completion predates the baseline keeps the signal measured at zero,
// even beside a milestone with unknown timing.
func TestCheckpointMilestoneWithKnownTimingBeforeTheBaselineStaysMeasured(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.close()
	f.record("project-1", checkpointAt(10), "record")
	export := checkpointWriteInput(t, t.TempDir(), "linear-export.json",
		"{\"issues\":["+
			"{\"identifier\":\"CRW-1\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(2)+"\",\"state\":\"Done\",\"milestone\":\"M1\"},"+
			"{\"identifier\":\"CRW-2\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(2)+"\",\"state\":\"Done\",\"milestone\":\"M2\",\"completedAt\":\""+checkpointAt(3)+"\"}]}")

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{LinearExport: export}, nil), "project-1")
	if report.Counts.MilestoneIntegrated == nil || *report.Counts.MilestoneIntegrated != 0 {
		t.Fatalf("a milestone whose completion predates the baseline was not measured as zero: %+v", report.Counts.MilestoneIntegrated)
	}
	if report.UnmeasuredReasons["M1"] == "" {
		t.Errorf("the skipped milestone is not named in the reasons: %+v", report.UnmeasuredReasons)
	}
}

// A later checkpoint record clears running_hours and the rolling 2h window: both are anchored at
// the baseline, so a project that was just checked is not due again for the same events.
func TestCheckpointRecordClearsTheRunningHoursAndWindowSignals(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	for i := 0; i < 3; i++ {
		f.verdict(fmt.Sprintf("event-%d", i), "relationship-1", "needs_changes", checkpointAt(23))
	}
	f.close()

	// An old record: running_hours is past its threshold and the verdicts are inside the window.
	f.record("project-1", checkpointAt(10), "an old checkpoint")
	before := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
	if !checkpointHasReason(before, checkpointSignalRunningHours) {
		t.Fatalf("running_hours did not fire on an old record: %+v", before)
	}
	if !checkpointHasReason(before, checkpointSignalNeedsChanges) {
		t.Fatalf("the 2h window did not fire: %+v", before)
	}

	// A record at the reading instant clears both: the elapsed time is zero and the window starts
	// at the baseline, which is after the verdicts.
	f.record("project-1", checkpointAt(24), "a fresh checkpoint")
	after := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
	if checkpointHasReason(after, checkpointSignalRunningHours) {
		t.Errorf("running_hours stayed due after a fresh record: %+v", after)
	}
	if checkpointHasReason(after, checkpointSignalNeedsChanges) {
		t.Errorf("the 2h window kept events the checkpoint already saw: %+v", after)
	}
	if after.Counts.NeedsChangesOrSplit2h != 0 {
		t.Errorf("needs_changes_or_split_2h = %d, want 0", after.Counts.NeedsChangesOrSplit2h)
	}
}

// C3: when the 2h window starts at the baseline, an event exactly at that instant is not counted,
// because the checkpoint that wrote the baseline already looked at it; an event a second later is.
func TestCheckpointWindowStartExcludesTheBaseline(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	// Two verdicts: one exactly at the baseline instant, one half an hour after it and still
	// before now.
	f.verdict("event-at", "relationship-1", "needs_changes", checkpointAt(23))
	f.verdict("event-after", "relationship-1", "needs_changes", checkpointAt(23.5))
	f.close()
	// The baseline is an hour before now, so it is later than now-2h and the window starts there.
	f.record("project-1", checkpointAt(23), "a checkpoint an hour ago")

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
	if report.Counts.NeedsChangesOrSplit2h != 1 {
		t.Fatalf("needs_changes_or_split_2h = %d, want 1 (the event at the baseline excluded, the later one counted)", report.Counts.NeedsChangesOrSplit2h)
	}
	if checkpointHasReason(report, checkpointSignalNeedsChanges) {
		t.Errorf("the window fired with a single event past the baseline: %+v", report.Reasons)
	}
}

// C3: the baseline-exclusive start applies to the backlog window too, so an issue created exactly
// at the baseline instant does not re-enter the net.
func TestCheckpointBacklogWindowStartExcludesTheBaseline(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.close()
	// The baseline is two hours before now, so it is later than now-4h and the window starts there.
	f.record("project-1", checkpointAt(22), "a checkpoint two hours ago")
	export := checkpointWriteInput(t, t.TempDir(), "linear-export.json",
		"{\"issues\":[{\"identifier\":\"CRW-1\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(22)+"\",\"state\":\"Backlog\"}]}")

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{LinearExport: export}, nil), "project-1")
	if report.Counts.BacklogNet4h == nil || *report.Counts.BacklogNet4h != 0 {
		t.Fatalf("an issue created exactly at the baseline was counted: %v", report.Counts.BacklogNet4h)
	}
}

// A custom state name that merely contains "done" or "complet" is not a completed state, so it
// stays in the backlog and does not make a milestone look integrated.
func TestCheckpointStateMatchingIsNotSubstring(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.close()
	export := checkpointWriteInput(t, t.TempDir(), "linear-export.json",
		"{\"issues\":["+
			"{\"identifier\":\"CRW-1\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(21)+"\",\"state\":\"Incomplete\"},"+
			"{\"identifier\":\"CRW-2\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(21)+"\",\"state\":\"Not Done\"}]}")

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{LinearExport: export}, nil), "project-1")
	if report.Counts.BacklogNet4h == nil || *report.Counts.BacklogNet4h != 2 {
		t.Fatalf("a name merely containing done/complet was read as completed: %v", report.Counts.BacklogNet4h)
	}
}

// A record path that is a symbolic link is refused, so an existing link cannot send the append
// outside the state directory.
func TestCheckpointRecordRefusesASymlinkedRecordFile(t *testing.T) {
	coreTempHome(t)
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(checkpointManageStateDir(t), "checkpoint")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "project-1.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, _, code := checkpointRunCommand(t, t.TempDir(), []string{
		"record", "--project", "project-1", "--summary-file",
		checkpointWriteInput(t, t.TempDir(), "summary.md", "summary"),
	}); code == 0 {
		t.Fatal("record followed a symbolic link")
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Errorf("the record was written through the link: %q", data)
	}
}

// C2: an input file the reading cannot use leaves only the signals that depend on it unmeasured.
// The rest is computed and the command exits 0; the reason is recorded per signal.
func TestCheckpointUnreadableInputLeavesOnlyItsSignalsUnmeasured(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.mergeTurn("turn-1", "project-1", "landed", checkpointAt(2))
	f.close()
	missing := filepath.Join(t.TempDir(), "absent.json")

	// A missing linear export leaves the backlog and the milestone unmeasured; the merge count,
	// which does not depend on it, is still computed.
	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{LinearExport: missing}, nil), "project-1")
	for _, signal := range []string{checkpointSignalBacklog, checkpointSignalMilestone} {
		if !checkpointHasUnmeasured(report, signal) {
			t.Errorf("%s is not unmeasured without the export: %+v", signal, report.Unmeasured)
		}
		if report.UnmeasuredReasons[signal] == "" {
			t.Errorf("%s carries no reason: %+v", signal, report.UnmeasuredReasons)
		}
	}
	if report.Counts.BacklogNet4h != nil || report.Counts.MilestoneIntegrated != nil {
		t.Errorf("an unmeasured signal carries a count: %+v", report.Counts)
	}
	if report.Counts.MergesSinceCheckpoint != 1 {
		t.Errorf("a signal that does not depend on the export was not computed: %+v", report.Counts)
	}
	if checkpointHasUnmeasured(report, checkpointSignalMerges) || checkpointHasUnmeasured(report, checkpointSignalIntegrations) {
		t.Errorf("an independent signal was left unmeasured: %+v", report.Unmeasured)
	}

	// The command exits 0 (the project is not due), not the old input error exit.
	stdout, stderr, code := checkpointRunCommand(t, f.dir, []string{"--linear-export", missing})
	if code != 0 {
		t.Fatalf("an unreadable export exited %d, want 0: %s%s", code, stdout, stderr)
	}
	if code == checkpointStoreExit {
		t.Error("a broken export was reported as unreadable relay state")
	}

	// A missing pair evaluation leaves only its own signal unmeasured, while a readable export
	// beside it keeps the backlog and milestone measured.
	goodExport := checkpointWriteInput(t, t.TempDir(), "linear-export.json",
		"{\"issues\":[{\"identifier\":\"CRW-1\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(22)+"\",\"state\":\"Done\",\"milestone\":\"M1\",\"completedAt\":\""+checkpointAt(23)+"\"}]}")
	report = checkpointReport(t, checkpointRead(t, f, CheckpointOptions{LinearExport: goodExport, PairEval: missing}, nil), "project-1")
	if !checkpointHasUnmeasured(report, checkpointSignalPairEval) || report.UnmeasuredReasons[checkpointSignalPairEval] == "" {
		t.Errorf("the pair evaluation is not unmeasured with a reason: %+v %+v", report.Unmeasured, report.UnmeasuredReasons)
	}
	if report.Counts.PairEvalP0P1SinceCheckpoint != nil {
		t.Errorf("an unmeasured signal carries a count: %+v", report.Counts)
	}
	if checkpointHasUnmeasured(report, checkpointSignalBacklog) {
		t.Errorf("a signal that does not depend on the pair evaluation was left unmeasured: %+v", report.Unmeasured)
	}
	if report.Counts.BacklogNet4h == nil || *report.Counts.BacklogNet4h != 0 {
		t.Errorf("the readable export was not computed beside the broken pair evaluation: %+v", report.Counts.BacklogNet4h)
	}
}

// C2: a file that is present but is not the document the reading expects is an unreadable input
// too, so it leaves the same signals unmeasured rather than failing the reading.
func TestCheckpointUnparseableInputLeavesItsSignalsUnmeasured(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.close()
	broken := checkpointWriteInput(t, t.TempDir(), "linear-export.json", "not json at all")

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{LinearExport: broken}, nil), "project-1")
	for _, signal := range []string{checkpointSignalBacklog, checkpointSignalMilestone} {
		if !checkpointHasUnmeasured(report, signal) || report.UnmeasuredReasons[signal] == "" {
			t.Errorf("%s is not unmeasured with a reason: %+v %+v", signal, report.Unmeasured, report.UnmeasuredReasons)
		}
	}
	if _, _, code := checkpointRunCommand(t, f.dir, []string{"--linear-export", broken}); code != 0 {
		t.Errorf("an unparseable export exited %d, want 0", code)
	}
}

// The thresholds come from the checkpoint settings section.
func TestCheckpointThresholdsComeFromTheSection(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	for i := 0; i < 5; i++ {
		f.mergeTurn(fmt.Sprintf("turn-%02d", i), "project-1", "landed", checkpointAt(2))
	}
	f.close()

	if report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1"); report.Due {
		t.Errorf("five merges are due at the default threshold: %+v", report)
	}
	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, map[string]any{"merges_since_checkpoint": 5}), "project-1")
	if !report.Due || !checkpointHasReason(report, checkpointSignalMerges) {
		t.Errorf("the configured threshold was not honoured: %+v", report)
	}
}

// --project narrows the report to one project, and without it every recorded project is
// reported.
func TestCheckpointProjectFilter(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.scope("relationship-2", "project-2")
	f.mergeTurn("turn-1", "project-1", "landed", checkpointAt(2))
	f.mergeTurn("turn-2", "project-2", "landed", checkpointAt(2))
	f.close()

	all := checkpointRead(t, f, CheckpointOptions{}, nil)
	if len(all) != 2 || all[0].Project != "project-1" || all[1].Project != "project-2" {
		t.Fatalf("every project was not reported, sorted: %+v", all)
	}
	one := checkpointRead(t, f, CheckpointOptions{Project: "project-2"}, nil)
	if len(one) != 1 || one[0].Project != "project-2" {
		t.Fatalf("--project did not narrow the report: %+v", one)
	}
}

// A project key that would escape the checkpoint directory is refused rather than written.
func TestCheckpointRecordRefusesAPathEscape(t *testing.T) {
	coreTempHome(t)
	dir := t.TempDir()
	for _, project := range []string{"", ".", "..", "a/b", "a\\b"} {
		if _, _, code := checkpointRunCommand(t, dir, []string{
			"record", "--project", project, "--summary-file",
			checkpointWriteInput(t, t.TempDir(), "summary.md", "summary"),
		}); code == 0 {
			t.Errorf("record accepted the project key %q", project)
		}
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) != 0 {
		t.Errorf("a refused record wrote into the state directory: %v", entries)
	}
}

// A missing store is a read failure (exit 3), a due project is exit 1 and a clean one is 0.
func TestCheckpointExitStatuses(t *testing.T) {
	coreTempHome(t)
	if _, _, code := checkpointRunCommand(t, t.TempDir(), nil); code != checkpointStoreExit {
		t.Errorf("a state directory with no store exited %d, want %d", code, checkpointStoreExit)
	}

	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.mergeTurn("turn-1", "project-1", "landed", checkpointAt(2))
	f.close()
	if _, _, code := checkpointRunCommand(t, f.dir, nil); code != 0 {
		t.Errorf("a clean project exited %d, want 0", code)
	}

	f2 := checkpointNewFixture(t)
	f2.scope("relationship-1", "project-1")
	for i := 0; i < 20; i++ {
		f2.mergeTurn(fmt.Sprintf("turn-%02d", i), "project-1", "landed", checkpointAt(2))
	}
	f2.close()
	stdout, stderr, code := checkpointRunCommand(t, f2.dir, nil)
	if code != checkpointDueExit {
		t.Fatalf("a due project exited %d, want %d: %s%s", code, checkpointDueExit, stdout, stderr)
	}
	var reports []CheckpointReport
	if err := json.Unmarshal([]byte(stdout), &reports); err != nil {
		t.Fatalf("the output is not a JSON array: %v\n%s", err, stdout)
	}
	if len(reports) != 1 || !reports[0].Due {
		t.Errorf("the output does not carry the due project: %+v", reports)
	}
}

// The reading is refused when the store file is absent, which is what the exit status 3
// reports.
func TestCheckpointMissingStoreIsAnError(t *testing.T) {
	coreTempHome(t)
	dir := t.TempDir()
	if _, err := Checkpoint(context.Background(), checkpointEnv(t), checkpointConfig(t, dir, nil), CheckpointOptions{}); err == nil {
		t.Fatal("a state directory with no relay.sqlite3 read as a checkpoint")
	}
}

// An unknown argument and an unknown subcommand are usage errors, and --help prints the usage.
func TestCheckpointCommandUsage(t *testing.T) {
	f := checkpointNewFixture(t)
	f.close()
	if _, _, code := checkpointRunCommand(t, f.dir, []string{"--nope"}); code != usageExit {
		t.Errorf("an unknown argument exited %d, want %d", code, usageExit)
	}
	if _, _, code := checkpointRunCommand(t, f.dir, []string{"nonsense"}); code != usageExit {
		t.Errorf("an unknown subcommand exited %d, want %d", code, usageExit)
	}
	stdout, _, code := checkpointRunCommand(t, f.dir, []string{"--help"})
	if code != 0 || !strings.Contains(stdout, "usage: crw manage checkpoint") {
		t.Errorf("--help: exit %d output %q", code, stdout)
	}
}

// checkpointRunCommand runs the command against a fixture state directory with the clock fixed,
// and returns its stdout, stderr and status.
func checkpointRunCommand(t *testing.T, state string, args []string) (string, string, int) {
	t.Helper()
	return checkpointRunCommandAt(t, state, args, checkpointNow)
}

// checkpointRecordClock is the clock a record is made with when a test needs the baseline to fall
// before the reading: two hours earlier, so a later event is counted and an earlier one is not.
func checkpointRecordClock() time.Time { return checkpointNow().Add(-2 * time.Hour) }

// checkpointRunCommandAt is checkpointRunCommand with the clock a test chooses.
func checkpointRunCommandAt(t *testing.T, state string, args []string, now func() time.Time) (string, string, int) {
	t.Helper()
	exe, _ := coreFakeCRW(t, state, 0)
	var stdout, stderr strings.Builder
	e := &Env{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr, Getenv: os.Getenv, Now: now, Executable: exe}
	code := checkpointCommand.Run(context.Background(), e, args)
	return stdout.String(), stderr.String(), code
}

// checkpointFileState is what a read must leave alone: the file's bytes and its mtime.
func checkpointFileState(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%s %d %s", hex.EncodeToString(sum[:]), info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano))
}

// checkpointSidecars lists the SQLite coordination files beside a store, which a read-only
// checkpoint must never create.
func checkpointSidecars(t *testing.T, path string) []string {
	t.Helper()
	var found []string
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			found = append(found, filepath.Base(path+suffix))
		}
	}
	return found
}
