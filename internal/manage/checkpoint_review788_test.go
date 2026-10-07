package manage

// The tests of the CRW-919 review: the checkpoint reading after the PR #788 evaluation. Every
// helper here is prefixed checkpointReview788, so a name added by a sibling issue of the same
// project cannot collide with it.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance"
)

// checkpointReview788Refresh records a base refresh of an acceptance: the dag_base_refreshes row
// whose id is the digest acceptance.RefreshDigest produces. A row that does not digest to its id is
// ignored by acceptance.StandOf, so a test that wants the acceptance to stand on the later
// generation has to record the row exactly as the relay writes it.
func (f *checkpointFixture) checkpointReview788Refresh(acceptanceID, relationshipID string, generation int64, event, revision, head, at string) {
	f.t.Helper()
	const (
		baseRepository = "owner/repo"
		baseRef        = "dev"
		baseTip        = "basetipsha"
		proofJSON      = "{}"
		resolvedJSON   = "[]"
	)
	id := acceptance.RefreshDigest(acceptanceID, relationshipID, generation, event, revision, head, baseRepository, baseRef, baseTip, proofJSON, resolvedJSON)
	f.exec("INSERT INTO dag_base_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json, recorded_by_task_id, coordinator_epoch, recorded_at) VALUES (?,?,1,?,?,?,?,?,?,?,?,?,?,'parent',0,?)",
		id, acceptanceID, relationshipID, generation, event, revision, head, baseRepository, baseRef, baseTip, proofJSON, resolvedJSON, at)
}

// checkpointReview788Mark records the parent's merged mark on an acceptance's stand: the event,
// generation and revision integration is judged against. It is checkpointFixture.mark with the
// generation and revision spelled out, because after a base refresh they are the refresh's, not
// the acceptance's own.
func (f *checkpointFixture) checkpointReview788Mark(relationshipID, event, revision string, generation int64, at string) {
	f.t.Helper()
	f.exec("INSERT INTO assignment_marks (relationship_id, mark, event_id, execution_generation, revision_hash, evidence, actor, marked_at) VALUES (?,'merged',?,?,?,'{}','parent',?)",
		relationshipID, event, generation, revision, at)
}

// checkpointReview788Numbers is every integer the text carries, in order. A test uses it to ask
// which rows a reason names without pinning the sentence around them.
func checkpointReview788Numbers(text string) []int {
	var out []int
	current := ""
	flush := func() {
		if current == "" {
			return
		}
		if value, err := strconv.Atoi(current); err == nil {
			out = append(out, value)
		}
		current = ""
	}
	for _, r := range text {
		if r >= '0' && r <= '9' {
			current += string(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// checkpointReview788HasNumber reports whether the numbers carry one value.
func checkpointReview788HasNumber(numbers []int, want int) bool {
	for _, got := range numbers {
		if got == want {
			return true
		}
	}
	return false
}

// A base refresh moves the generation an acceptance stands on, and integration is judged on that
// current stand. The acceptance was accepted at generation 1 (event-1, head-1); the parent then
// recorded a valid refresh onto generation 2 (event-2, head-2) and marked that generation merged.
// Its head is observed in every target after the checkpoint baseline, so the integration counts
// once. On dev the acceptance's own generation is passed to the scheduler, which answers
// not-integrated, and the count is 0.
func TestCheckpointReview788BaseRefreshedAcceptanceCounts(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.plan("plan-1", "project-1", "node", [2]string{"owner/repo", "dev"}, [2]string{"owner/repo", "main"})
	f.execution("plan-1", "node", "relationship-1")
	f.acceptance("plan-1", "node", "relationship-1", "acceptance-1", "event-1", "head-1", checkpointAt(1))
	// The refresh: the same acceptance now stands on generation 2, whose head is head-2.
	f.checkpointReview788Refresh("acceptance-1", "relationship-1", 2, "event-2", "revision-2", "head-2", checkpointAt(2))
	// The parent's merged mark stands on the refreshed generation, not the acceptance's own.
	f.checkpointReview788Mark("relationship-1", "event-2", "revision-2", 2, checkpointAt(2))
	// Both targets contain the head the acceptance stands on now, after the baseline.
	f.observation("acceptance-1", "head-2", "owner/repo", "dev", checkpointAt(3), 1, true)
	f.observation("acceptance-1", "head-2", "owner/repo", "main", checkpointAt(4), 1, true)
	f.close()
	f.record("project-1", checkpointAt(1), "a checkpoint before the base refresh")

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
	if report.Counts.IntegrationsSinceCheckpoint == nil {
		t.Fatalf("the integration reading is unmeasured: %+v %+v", report.Unmeasured, report.UnmeasuredReasons)
	}
	if got := *report.Counts.IntegrationsSinceCheckpoint; got != 1 {
		t.Fatalf("a base-refreshed acceptance counted %d integrations, want 1", got)
	}
}

// An unreadable observation instant of one project leaves only that project unmeasured: the other
// project's integrations are still counted, in the full reading and under --project. On dev the
// reason is returned once for the whole reading, so --project B reads A's acceptances and reports B
// unmeasured too.
func TestCheckpointReview788UnreadableInstantStaysInItsProject(t *testing.T) {
	f := checkpointNewFixture(t)
	// project-1: one integrated acceptance whose satisfying observation instant cannot be read.
	f.scope("relationship-1", "project-1")
	f.plan("plan-1", "project-1", "node", [2]string{"owner/repo", "dev"})
	f.execution("plan-1", "node", "relationship-1")
	f.acceptance("plan-1", "node", "relationship-1", "acceptance-1", "event-1", "head-1", checkpointAt(1))
	f.mark("relationship-1", "event-1", "acceptance-1", checkpointAt(2))
	f.exec("INSERT INTO dag_integration_observations (observation_id, acceptance_id, repository, base_ref, subject_sha, tip_sha, is_ancestor, method, observed_seq, observed_at) VALUES ('observation-broken','acceptance-1','owner/repo','dev','head-1','tip',1,'ancestry',1,'not an instant')")
	// project-2: the same shape with a readable instant.
	f.scope("relationship-2", "project-2")
	f.plan("plan-2", "project-2", "node", [2]string{"owner/repo", "dev"})
	f.execution("plan-2", "node", "relationship-2")
	f.acceptance("plan-2", "node", "relationship-2", "acceptance-2", "event-2", "head-2", checkpointAt(1))
	f.mark("relationship-2", "event-2", "acceptance-2", checkpointAt(2))
	f.observation("acceptance-2", "head-2", "owner/repo", "dev", checkpointAt(3), 1, true)
	f.close()
	f.record("project-1", checkpointAt(1), "a checkpoint")
	f.record("project-2", checkpointAt(1), "a checkpoint")

	all := checkpointRead(t, f, CheckpointOptions{}, nil)
	one := checkpointReport(t, all, "project-1")
	if !checkpointHasUnmeasured(one, checkpointSignalIntegrations) {
		t.Errorf("the project with the unreadable instant is not unmeasured: %+v", one.Unmeasured)
	}
	two := checkpointReport(t, all, "project-2")
	if checkpointHasUnmeasured(two, checkpointSignalIntegrations) {
		t.Errorf("another project's unreadable instant left project-2 unmeasured: %+v %+v", two.Unmeasured, two.UnmeasuredReasons)
	}
	if two.Counts.IntegrationsSinceCheckpoint == nil || *two.Counts.IntegrationsSinceCheckpoint != 1 {
		t.Errorf("project-2 integrations = %v, want 1", two.Counts.IntegrationsSinceCheckpoint)
	}

	// --project narrows the acceptance read itself, so the other project's acceptance is not read.
	narrowed := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{Project: "project-2"}, nil), "project-2")
	if checkpointHasUnmeasured(narrowed, checkpointSignalIntegrations) {
		t.Errorf("--project project-2 is unmeasured: %+v %+v", narrowed.Unmeasured, narrowed.UnmeasuredReasons)
	}
	if narrowed.Counts.IntegrationsSinceCheckpoint == nil || *narrowed.Counts.IntegrationsSinceCheckpoint != 1 {
		t.Errorf("--project project-2 integrations = %v, want 1", narrowed.Counts.IntegrationsSinceCheckpoint)
	}
}

// One non-empty pair-eval row that cannot be read leaves the whole pair-eval signal unmeasured, and
// the reason names the file and the row. On dev the broken row is dropped and the readable one is
// counted as a complete measurement.
func TestCheckpointReview788BrokenPairEvalLineIsUnmeasured(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.close()
	f.record("project-1", checkpointAt(1), "a checkpoint")
	// Row 1 is a readable P1; row 2 is missing its closing brace.
	pairEval := checkpointWriteInput(t, t.TempDir(), "pair-eval.jsonl",
		"{\"project\":\"project-1\",\"severity\":\"P1\",\"at\":\""+checkpointAt(2)+"\"}\n"+
			"{\"project\":\"project-1\",\"severity\":\"P0\",\"at\":\""+checkpointAt(3)+"\"\n")

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{PairEval: pairEval}, nil), "project-1")
	if !checkpointHasUnmeasured(report, checkpointSignalPairEval) {
		t.Fatalf("a broken pair-eval row left the signal measured: %+v %+v", report.Unmeasured, report.Counts)
	}
	if report.Counts.PairEvalP0P1SinceCheckpoint != nil {
		t.Errorf("an unmeasured pair-eval signal carries a count: %+v", report.Counts.PairEvalP0P1SinceCheckpoint)
	}
	reason := report.UnmeasuredReasons[checkpointSignalPairEval]
	if !strings.Contains(reason, filepath.Base(pairEval)) {
		t.Errorf("the reason does not name the file: %q", reason)
	}
	// Only the part after the file name is asked for row numbers: the temporary directory's own
	// digits are not row numbers.
	_, tail, _ := strings.Cut(reason, filepath.Base(pairEval))
	numbers := checkpointReview788Numbers(tail)
	if !checkpointReview788HasNumber(numbers, 2) {
		t.Errorf("the reason does not name row 2: %q -> %v", reason, numbers)
	}
	if checkpointReview788HasNumber(numbers, 1) {
		t.Errorf("the reason names row 1, which was readable: %q -> %v", reason, numbers)
	}
}

// The exit status follows due only: a due project with an unreadable input is still exit 1, and the
// unreadable input leaves only the signals that depend on it unmeasured. This is the contrast case
// of the review: it holds on dev and after the fix.
// checkpointReview788PairEvalReason reads the pair-eval reason a file produces.
func checkpointReview788PairEvalReason(t *testing.T, f *checkpointFixture, pairEval string) string {
	t.Helper()
	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{PairEval: pairEval}, nil), "project-1")
	if !checkpointHasUnmeasured(report, checkpointSignalPairEval) {
		t.Fatalf("the pair evaluation is measured: %+v %+v", report.Unmeasured, report.Counts)
	}
	if report.Counts.PairEvalP0P1SinceCheckpoint != nil {
		t.Errorf("an unmeasured pair-eval signal carries a count: %+v", report.Counts.PairEvalP0P1SinceCheckpoint)
	}
	return report.UnmeasuredReasons[checkpointSignalPairEval]
}

// The other trigger the issue names: a row whose JSON parses but whose instant this build cannot
// read is unreadable too, so it leaves the signal unmeasured and is named by its row number.
func TestCheckpointReview788UnreadablePairEvalInstantIsUnmeasured(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.close()
	f.record("project-1", checkpointAt(1), "a checkpoint")
	// Row 1 reads; row 2 parses but carries an instant in no form this build knows.
	pairEval := checkpointWriteInput(t, t.TempDir(), "pair-eval.jsonl",
		"{\"project\":\"project-1\",\"severity\":\"P1\",\"at\":\""+checkpointAt(2)+"\"}\n"+
			"{\"project\":\"project-1\",\"severity\":\"P0\",\"at\":\"not an instant\"}\n")

	reason := checkpointReview788PairEvalReason(t, f, pairEval)
	if !strings.Contains(reason, filepath.Base(pairEval)) {
		t.Errorf("the reason does not name the file: %q", reason)
	}
	_, rows, _ := strings.Cut(reason, filepath.Base(pairEval))
	numbers := checkpointReview788Numbers(rows)
	if !checkpointReview788HasNumber(numbers, 2) {
		t.Errorf("the reason does not name row 2: %q -> %v", reason, numbers)
	}
	if checkpointReview788HasNumber(numbers, 1) {
		t.Errorf("the reason names row 1, which was readable: %q -> %v", reason, numbers)
	}
}

// A file broken on more rows than the reason names still produces a short reason: at most five row
// numbers, and the rest elided rather than listed.
func TestCheckpointReview788ManyBrokenPairEvalRowsAreCapped(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.close()
	f.record("project-1", checkpointAt(1), "a checkpoint")
	var lines []string
	for i := 0; i < 8; i++ {
		lines = append(lines, "{\"project\":\"project-1\",\"severity\":\"P1\"")
	}
	pairEval := checkpointWriteInput(t, t.TempDir(), "pair-eval.jsonl", strings.Join(lines, "\n")+"\n")

	// The decided answer's own number: at most five row numbers in the reason. Pinned here as a
	// literal rather than read from the implementation, so the test states the contract instead of
	// mirroring the code it checks.
	const decidedRowLimit = 5
	reason := checkpointReview788PairEvalReason(t, f, pairEval)
	_, rows, _ := strings.Cut(reason, filepath.Base(pairEval))
	numbers := checkpointReview788Numbers(rows)
	if len(numbers) != decidedRowLimit {
		t.Errorf("the reason names %d rows, want %d: %q -> %v", len(numbers), decidedRowLimit, reason, numbers)
	}
	if !strings.Contains(rows, "...") {
		t.Errorf("the reason does not mark the elided rows: %q", reason)
	}
}

// An all-readable pair-eval file behaves exactly as before: the readable findings are counted.
func TestCheckpointReview788ReadablePairEvalStillCounts(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	f.close()
	f.record("project-1", checkpointAt(1), "a checkpoint")
	// A blank line between the rows is skipped and does not make the file unreadable.
	pairEval := checkpointWriteInput(t, t.TempDir(), "pair-eval.jsonl",
		"{\"project\":\"project-1\",\"severity\":\"P1\",\"at\":\""+checkpointAt(2)+"\"}\n"+
			"\n"+
			"{\"project\":\"project-1\",\"severity\":\"P0\",\"at\":\""+checkpointAt(3)+"\"}\n"+
			"{\"project\":\"project-1\",\"severity\":\"P2\",\"at\":\""+checkpointAt(4)+"\"}\n")

	report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{PairEval: pairEval}, nil), "project-1")
	if checkpointHasUnmeasured(report, checkpointSignalPairEval) {
		t.Fatalf("a readable pair evaluation is unmeasured: %+v %+v", report.Unmeasured, report.UnmeasuredReasons)
	}
	if report.Counts.PairEvalP0P1SinceCheckpoint == nil || *report.Counts.PairEvalP0P1SinceCheckpoint != 2 {
		t.Errorf("pair P0/P1 = %v, want 2", report.Counts.PairEvalP0P1SinceCheckpoint)
	}
}

func TestCheckpointReview788DueWithUnreadableInputExitsOne(t *testing.T) {
	f := checkpointNewFixture(t)
	f.scope("relationship-1", "project-1")
	for i := 0; i < 20; i++ {
		f.mergeTurn(fmt.Sprintf("turn-%02d", i), "project-1", "landed", checkpointAt(2+float64(i)/60))
	}
	f.close()
	f.record("project-1", checkpointAt(1), "the first checkpoint")
	missing := filepath.Join(t.TempDir(), "absent-linear-export.json")

	stdout, stderr, code := checkpointRunCommand(t, f.dir, []string{"--linear-export", missing})
	if code != checkpointDueExit {
		t.Fatalf("a due project with an unreadable export exited %d, want %d: %s%s", code, checkpointDueExit, stdout, stderr)
	}
	var reports []CheckpointReport
	if err := json.Unmarshal([]byte(stdout), &reports); err != nil {
		t.Fatalf("the output is not a JSON array: %v\n%s", err, stdout)
	}
	report := checkpointReport(t, reports, "project-1")
	if !report.Due {
		t.Errorf("the report is not due: %+v", report)
	}
	if report.Counts.MergesSinceCheckpoint != 20 {
		t.Errorf("merges = %d, want 20", report.Counts.MergesSinceCheckpoint)
	}
	for _, signal := range []string{checkpointSignalBacklog, checkpointSignalMilestone} {
		if !checkpointHasUnmeasured(report, signal) {
			t.Errorf("%s is not unmeasured without the export: %+v", signal, report.Unmeasured)
		}
	}
}
