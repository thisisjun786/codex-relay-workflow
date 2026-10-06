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
func (f *checkpointFixture) acceptance(planID, project, acceptanceID, head, at string) {
	f.exec("INSERT OR IGNORE INTO dag_plans (plan_id, project_key, created_by_task_id, created_at) VALUES (?,?,'parent',?)",
		planID, project, checkpointAt(0))
	f.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state) VALUES (?,?,'node','manifest',?,1,'event','revision','criteria','verified',?,'verified','turn','{}','parent',0,?,'active')",
		acceptanceID, planID, "relationship-"+acceptanceID, head, at)
}

// mark records the parent's merged mark on an acceptance, which is what makes an observation an
// integration rather than a mere ancestry reading.
func (f *checkpointFixture) mark(acceptanceID, at string) {
	f.exec("INSERT INTO assignment_marks (relationship_id, mark, event_id, execution_generation, revision_hash, evidence, actor, marked_at) VALUES (?,'merged','event',1,'revision','{}','parent',?)",
		"relationship-"+acceptanceID, at)
}

// observation records one ancestry observation of an acceptance in the target branch. seq orders
// the observations of one acceptance and target, which is how a later one supersedes an earlier.
func (f *checkpointFixture) observation(acceptanceID, at string, seq int, isAncestor bool) {
	flag := 0
	if isAncestor {
		flag = 1
	}
	f.exec("INSERT INTO dag_integration_observations (observation_id, acceptance_id, repository, base_ref, subject_sha, tip_sha, is_ancestor, method, observed_seq, observed_at) VALUES (?,?,'owner/repo','dev','subject','tip',?,'ancestry',?,?)",
		fmt.Sprintf("observation-%s-%d", acceptanceID, seq), acceptanceID, flag, seq, at)
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
				"{\"identifier\":\"CRW-901\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(2)+"\",\"state\":\"Completed\",\"milestone\":\"M2\",\"completedAt\":\""+checkpointAt(3)+"\"},"+
				"{\"identifier\":\"CRW-902\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(2)+"\",\"state\":\"Completed\",\"milestone\":\"M2\",\"completedAt\":\""+checkpointAt(4)+"\"}]}")
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

// An ancestry observation is not an integration until the parent's merged mark stands on the
// acceptance's event, generation and revision; one marked acceptance is counted once however many
// times it was observed in one target.
func TestCheckpointIntegrationCountRequiresTheMergedMark(t *testing.T) {
	unmarked := checkpointNewFixture(t)
	unmarked.scope("relationship-1", "project-1")
	unmarked.acceptance("plan-1", "project-1", "acceptance-1", "head-1", checkpointAt(1))
	unmarked.observation("acceptance-1", checkpointAt(2), 1, true)
	unmarked.observation("acceptance-1", checkpointAt(3), 2, true)
	unmarked.close()

	report := checkpointReport(t, checkpointRead(t, unmarked, CheckpointOptions{}, nil), "project-1")
	if report.Counts.IntegrationsSinceCheckpoint == nil || *report.Counts.IntegrationsSinceCheckpoint != 0 {
		t.Fatalf("an unmarked observation counted as an integration: %+v", report.Counts)
	}

	marked := checkpointNewFixture(t)
	marked.scope("relationship-1", "project-1")
	marked.acceptance("plan-1", "project-1", "acceptance-1", "head-1", checkpointAt(1))
	marked.mark("acceptance-1", checkpointAt(2))
	marked.observation("acceptance-1", checkpointAt(3), 1, true)
	marked.observation("acceptance-1", checkpointAt(4), 2, true)
	marked.close()

	report = checkpointReport(t, checkpointRead(t, marked, CheckpointOptions{}, nil), "project-1")
	if report.Counts.IntegrationsSinceCheckpoint == nil || *report.Counts.IntegrationsSinceCheckpoint != 1 {
		t.Fatalf("a marked acceptance was not counted once: %+v", report.Counts)
	}
}

// A later observation saying the head is not contained supersedes the earlier positive one, so
// the acceptance is not integrated.
func TestCheckpointIntegrationCountIsClearedByALaterNegativeObservation(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.acceptance("plan-1", "project-1", "acceptance-1", "head-1", checkpointAt(1))
	f.mark("acceptance-1", checkpointAt(2))
	f.observation("acceptance-1", checkpointAt(3), 1, true)
	f.observation("acceptance-1", checkpointAt(4), 2, false)
	f.close()

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
	if report.Counts.IntegrationsSinceCheckpoint == nil || *report.Counts.IntegrationsSinceCheckpoint != 0 {
		t.Fatalf("a superseded observation counted as an integration: %+v", report.Counts)
	}
}

// An issue created and completed inside the backlog window changes the backlog by nothing, so it
// neither adds nor subtracts.
func TestCheckpointBacklogNetCountsCreationAndCompletionOnce(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.close()
	export := checkpointWriteInput(t, t.TempDir(), "linear-export.json",
		"{\"issues\":["+
			"{\"identifier\":\"CRW-1\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(21)+"\",\"state\":\"Backlog\"},"+
			"{\"identifier\":\"CRW-2\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(21)+"\",\"state\":\"Completed\",\"completedAt\":\""+checkpointAt(22)+"\"}]}")

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{LinearExport: export}, nil), "project-1")
	if report.Counts.BacklogNet4h == nil || *report.Counts.BacklogNet4h != 1 {
		t.Fatalf("the backlog net is %v, want 1 (one entered, one entered and left)", report.Counts.BacklogNet4h)
	}
}

// A completed milestone whose completion instant is unknown cannot be compared with the baseline,
// so the reading is unmeasured rather than a measured zero.
func TestCheckpointMilestoneWithoutCompletionTimeIsUnmeasured(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.close()
	f.record("project-1", checkpointAt(1), "record")
	export := checkpointWriteInput(t, t.TempDir(), "linear-export.json",
		"{\"issues\":[{\"identifier\":\"CRW-1\",\"project\":\"project-1\",\"createdAt\":\""+checkpointAt(2)+"\",\"state\":\"Completed\",\"milestone\":\"M1\"}]}")

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{LinearExport: export}, nil), "project-1")
	if report.Counts.MilestoneIntegrated != nil {
		t.Fatalf("a milestone without completion timing was measured: %+v", report.Counts)
	}
	if !checkpointHasUnmeasured(report, checkpointSignalMilestone) {
		t.Errorf("the milestone reading is not unmeasured: %+v", report.Unmeasured)
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

// A broken input file is not unreadable relay state: it exits 2, while a missing store exits 3.
func TestCheckpointInputErrorExitsTwo(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.close()
	missing := filepath.Join(t.TempDir(), "absent.jsonl")
	stdout, stderr, code := checkpointRunCommand(t, f.dir, []string{"--pair-eval", missing})
	if code != checkpointInputExit {
		t.Fatalf("a missing input file exited %d, want %d: %s%s", code, checkpointInputExit, stdout, stderr)
	}
	if code == checkpointStoreExit {
		t.Error("a broken export was reported as unreadable relay state")
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
