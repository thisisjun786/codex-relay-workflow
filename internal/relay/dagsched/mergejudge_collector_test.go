package dagsched

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// useCollector makes the node's pull request the one the real evidence collector builds from a scripted forge, so providers, run identities and conclusions have the shapes GitHub gives
// them, not the shapes the scheduler assumes.
func (k *judgeKit) useCollector(g *ghScript) {
	k.t.Helper()
	g.pulls = []map[string]any{g.pull(k.feature)}
	k.sched.PRs = ForgePullRequestReader(func(context.Context) evidence.Runner { return g.runner })
}

func checkRun(id int, name, head, conclusion string, app int) map[string]any {
	return map[string]any{"id": id, "name": name, "head_sha": head, "status": "completed", "conclusion": conclusion, "app": map[string]any{"id": app}}
}

func collectorScript(head string, required []any, checks ...any) *ghScript {
	g := newGHScript()
	g.rules = []any{map[string]any{"type": "required_status_checks", "parameters": map[string]any{"strict_required_status_checks_policy": false, "required_status_checks": required}}}
	g.checks = checks
	return g
}

// A required check answers only for the integration its branch rule names: a check of the same name from another integration is not it, whatever it concluded, and a required name whose
// integration has not reported is pending.
func TestMergeJudgeHonoursTheProviderOfARequiredCheck(t *testing.T) {
	k := newJudgeKit(t)
	required := []any{map[string]any{"context": "dev-gate", "integration_id": 42}}
	g := collectorScript(k.feature, required, checkRun(11, "dev-gate", k.feature, "success", 42), checkRun(12, "dev-gate", k.feature, "failure", 43))
	k.useCollector(g)
	pr := g.read(t)
	if pr.Verdict != "ready" || len(pr.RequiredProviders["dev-gate"]) != 1 {
		t.Fatalf("the collector's own verdict = %s providers %v", pr.Verdict, pr.RequiredProviders)
	}
	if r := k.judge(); !r.Eligible() || len(r.FailedRequired) != 0 {
		t.Fatalf("a check of another integration decided: %+v", r)
	}
	// the required integration reports a failure: that one counts
	g = collectorScript(k.feature, required, checkRun(11, "dev-gate", k.feature, "failure", 42), checkRun(12, "dev-gate", k.feature, "success", 43))
	k.useCollector(g)
	if r := k.judge(); r.Outcome != OutcomeRetrySameSHA || len(r.FailedRequired) != 1 {
		t.Fatalf("the required integration's failure = %+v", r)
	}
	// only another integration reported: the required one has not
	g = collectorScript(k.feature, required, checkRun(12, "dev-gate", k.feature, "success", 43))
	k.useCollector(g)
	if r := k.judge(); r.Outcome != OutcomeChecksPending {
		t.Fatalf("only another integration reported = %+v", r)
	}
}

// Run identities are opaque: "workflow-run:99:..." is not older than "workflow-run:100:..." because it sorts later as text. Every run of a required name counts at its own newest attempt, the way
// merge-evidence and the lane's check read them, and a run that has not finished is pending whatever order its id sorts in.
func TestRequiredChecksCountEveryRunAtItsNewestAttempt(t *testing.T) {
	head := strings.Repeat("a", 40)
	pr := func(checks ...Check) PullRequest {
		return PullRequest{HeadSHA: head, RequiredDeclared: []string{"dev-gate"}, Checks: checks}
	}
	c := func(run string, attempt int64, conclusion string) Check {
		return Check{Name: "dev-gate", RunID: run, Attempt: attempt, Conclusion: conclusion, HeadSHA: head}
	}
	cases := []struct {
		name            string
		checks          []Check
		pending, failed int
	}{
		{"two runs that both failed", []Check{c("workflow-run:99:dev-gate#0", 1, "failure"), c("workflow-run:100:dev-gate#0", 1, "failure")}, 0, 2},
		{"the older run passed and the newer one is still running", []Check{c("workflow-run:99:dev-gate#0", 1, "success"), c("workflow-run:100:dev-gate#0", 1, "")}, 1, 0},
		{"an older attempt of a run is not its result", []Check{c("workflow-run:7:dev-gate#0", 1, "failure"), c("workflow-run:7:dev-gate#0", 2, "success")}, 0, 0},
		{"a skipped required check is not a success, as the lane reads it", []Check{c("check-run:1", 1, "skipped")}, 0, 1},
		{"neutral is not a success either", []Check{c("check-run:1", 1, "neutral")}, 0, 1},
		{"a commit status that errored has finished", []Check{c("status:dev-gate", 1, "error")}, 0, 1},
		{"a commit status that is pending has not", []Check{c("status:dev-gate", 1, "pending")}, 1, 0},
		{"nothing of the head", nil, 1, 0},
		{"a check of another head", []Check{{Name: "dev-gate", RunID: "check-run:1", Attempt: 1, Conclusion: "success", HeadSHA: strings.Repeat("b", 40)}}, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pending, failed := requiredChecks(pr(tc.checks...))
			if len(pending) != tc.pending || len(failed) != tc.failed {
				t.Fatalf("pending %v failed %v, want %d and %d", pending, failed, tc.pending, tc.failed)
			}
		})
	}
}

// A skipped or neutral required check is judged the way the merge lane reads it, so a head the judgement lets into the lane is not refused there for the same check.
func TestMergeJudgeAgreesWithTheLanesReadingOfNeutralChecks(t *testing.T) {
	k := newJudgeKit(t)
	required := []any{map[string]any{"context": "dev-gate", "integration_id": 42}}
	g := collectorScript(k.feature, required, checkRun(11, "dev-gate", k.feature, "neutral", 42))
	k.useCollector(g)
	pr := g.read(t)
	if len(pr.CheckProblems) == 0 {
		t.Fatalf("the evidence helper accepts a neutral required check: %+v", pr)
	}
	if r := k.judge(); r.Eligible() {
		t.Fatalf("a neutral required check made the head eligible: %+v", r)
	}
}

func statusOf(id int, context, state, at string) map[string]any {
	return map[string]any{"id": id, "context": context, "state": state, "updated_at": at, "target_url": "u"}
}

// A name that two integrations are required to answer needs both of them: one integration's failure, seen before the other has reported, is not a finished set of checks and uses no retry.
func TestEveryRequiredIntegrationMustReport(t *testing.T) {
	k := newJudgeKit(t)
	required := []any{map[string]any{"context": "dev-gate", "integration_id": 42}, map[string]any{"context": "dev-gate", "integration_id": 43}}
	k.useCollector(collectorScript(k.feature, required, checkRun(11, "dev-gate", k.feature, "failure", 42)))
	if r := k.judge(); r.Outcome != OutcomeChecksPending || len(r.FailedRequired) != 0 {
		t.Fatalf("one of two integrations reported a failure = %+v", r)
	}
	k.useCollector(collectorScript(k.feature, required, checkRun(11, "dev-gate", k.feature, "failure", 42), checkRun(12, "dev-gate", k.feature, "success", 43)))
	if r := k.judge(); r.Outcome != OutcomeRetrySameSHA || len(r.FailedRequired) != 1 {
		t.Fatalf("both reported = %+v", r)
	}
}

// A commit status has no attempt and no run: the forge changes it in place. The time it last changed is what tells a status that failed again from the same status read twice, so the second
// failure of a status evicts and a duplicate wake does not.
func TestAStatusThatFailsAgainIsASecondFailure(t *testing.T) {
	k := newJudgeKit(t)
	gate := func(state, at string) {
		g := collectorScript(k.feature, []any{map[string]any{"context": "dev-gate"}})
		g.statuses = []any{statusOf(len(at), "dev-gate", state, at)}
		k.useCollector(g)
	}
	gate("error", "2026-10-02T00:00:01Z")
	first := k.judge()
	if first.Outcome != OutcomeRetrySameSHA || len(first.FailedRequired) != 1 {
		t.Fatalf("first = %+v", first)
	}
	if again := k.judge(); !again.Replayed || again.CheckSeq != first.CheckSeq {
		t.Fatalf("the same status read again = %+v", again)
	}
	gate("failure", "2026-10-02T00:00:09Z")
	if second := k.judge(); second.Outcome != OutcomeEvicted {
		t.Fatalf("the status failed again = %+v", second)
	}
	gate("success", "2026-10-02T00:00:20Z")
	if later := k.judge(); later.Outcome != OutcomeEvicted {
		t.Fatalf("success after the eviction = %+v", later)
	}
}
