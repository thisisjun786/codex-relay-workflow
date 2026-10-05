package mergeturn

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-586 test helpers: a relationship, the work reports that name its heads, and a claim that records a pull request, a
// relationship or both. Nothing in the product writes a work report (docs/port/decisions.md, section 53), so the tests
// record the rows the readers read.

const (
	headCompareLocal = "/srv/local/R.git"
	headCompareRel   = "rel-hc"
)

// headCompareFixture is a fixture with a fake forge that answers for pull request 500 (head-500), a readable local
// repository and a pull request reader wired in.
func headCompareFixture(t *testing.T) (*fx, *livePullHeads) {
	t.Helper()
	w := newFx(t)
	pulls := newLivePullHeads()
	pulls.set(fxRepo, 500, "head-500")
	w.m.Pulls = pulls
	w.target.set(headCompareLocal, fxBase, "base-0")
	return w, pulls
}

func headCompareRelate(w *fx, relationship string) {
	w.t.Helper()
	w.exec(`INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		relationship, "ISS-HC", "active", alpha.TaskID, alpha.HostID, "task-child", "host-child", 3, "[]", `["task-alpha"]`, fxISO, fxISO)
}

// headCompareReport records one submission of a work report; a nil head is a report that names none.
func headCompareReport(w *fx, relationship, event string, submission, generation int, head any) {
	w.t.Helper()
	w.exec(`INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,base_ref,head_sha,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		event, submission, relationship, generation, "rev", fxRepo, fxBase, head, "done", "r", "1", "s", "n", fxISO)
}

func headCompareClaim(w *fx, repository, head string, pr int64, relationship string, ready bool) map[string]any {
	w.t.Helper()
	options := ClaimOptions{}
	if pr != 0 {
		options.PR = nullInt(pr)
	}
	if relationship != "" {
		options.Relationship = sql.NullString{String: relationship, Valid: true}
	}
	return w.must(w.m.Request(w.ctx, repository, fxBase, fxA, alpha.TaskID, alpha.HostID, head, ready, options))
}

// headCompareHeld is a holding turn whose grant its holder answered.
func headCompareHeld(w *fx, repository, head string, pr int64, relationship string, ready bool) string {
	w.t.Helper()
	turn := headCompareClaim(w, repository, head, pr, relationship, ready)["turnId"].(string)
	w.answer(turn, alpha.TaskID)
	return turn
}

func headCompareCheck(w *fx, turn, head string, reader Reader) (map[string]any, error) {
	return w.m.Check(w.ctx, turn, alpha.TaskID, head, "base-0", runChecks(head, "success", 1, "dev-gate", "run-1"), green(), []string{"dev-gate"}, reader)
}

func headCompareDetail(err error) string {
	var refused *store.RefusedError
	if errors.As(err, &refused) {
		return refused.Detail
	}
	return fmt.Sprint(err)
}

func headCompareCount(w *fx, query string, args ...any) int64 {
	w.t.Helper()
	var n int64
	if err := w.s.DB.QueryRowContext(w.ctx, query, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// headCompareRefused fails unless err is a refusal with this reason whose detail holds every wanted text.
func headCompareRefused(t *testing.T, err error, reason string, wants ...string) {
	t.Helper()
	if reasonOf(err) != reason {
		t.Fatalf("want a %s refusal, got %v", reason, err)
	}
	for _, want := range wants {
		if !strings.Contains(headCompareDetail(err), want) {
			t.Errorf("the refusal does not say %q: %s", want, headCompareDetail(err))
		}
	}
}

// The way out a refusal for want of anything to compare with says, in both of its forms.
const (
	headCompareWayOneText = "with --pr on a forge repository"
	headCompareWayTwoText = "a work report records the head"
)

// headCompareDecision fails unless the answer says who decided the head, which head and, when it names one, the pull request.
func headCompareDecision(t *testing.T, answer map[string]any, by, head string, pr int64, relationship string) {
	t.Helper()
	decided, _ := answer["pullRequestHead"].(map[string]any)
	if decided == nil || decided["decidedBy"] != by || decided["head"] != head {
		t.Fatalf("the answer does not say %s decided %s: %v", by, head, answer["pullRequestHead"])
	}
	if pr == 0 && decided["pullRequest"] != nil || pr != 0 && decided["pullRequest"] != pr {
		t.Errorf("the decision names pull request %v, want %d", decided["pullRequest"], pr)
	}
	if relationship != "" && decided["relationship"] != relationship {
		t.Errorf("the decision names relationship %v, want %s", decided["relationship"], relationship)
	}
}
