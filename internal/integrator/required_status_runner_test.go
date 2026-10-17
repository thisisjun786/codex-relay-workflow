package integrator

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

type statusRunWorld struct {
	fake       *trackingfake.Server
	reusedFrom string
	targetSHA  string
	remoteBase string
	pushed     bool
}

func statusJob() json.RawMessage {
	job, _ := json.Marshal(map[string]any{
		"name": "build", "status": "completed", "conclusion": "success", "result": "passed", "databaseId": 101,
		"url":   "https://github.com/o/r/actions/runs/6/job/101",
		"steps": []map[string]any{{"name": "test", "number": 1, "status": "completed", "conclusion": "success", "result": "passed"}},
	})
	return job
}

func ghStatusRecord(t *testing.T, head, base string, keys dagsched.VerificationKeys, reusedFrom string) dagsched.VerificationRecord {
	t.Helper()
	gh := &dagsched.GitHubVerification{
		RunID: 6, URL: "https://github.com/o/r/actions/runs/6", RunHead: head,
		Branch: "ci/verify-abcdef12", Event: "workflow_dispatch", WorkflowID: 7,
		WorkflowPath: ".github/workflows/ci.yml", WorkflowDigest: "sha256:" + strings.Repeat("f", 64),
		Conclusion: "success", Attempts: 1,
		Runs: []dagsched.GitHubRunAttempt{{Attempt: 1, Status: "completed", Conclusion: "success", Jobs: []json.RawMessage{statusJob()}}},
		Scan: dagsched.GitHubScan{Base: base, Head: head, Result: "clean"},
	}
	if reusedFrom != "" {
		gh.RunHead = reusedFrom
		gh.ReusedFrom = &dagsched.GitHubReusedFrom{VerifierKind: dagsched.GitHubVerifierKind, Digest: "sha256:" + strings.Repeat("e", 64), Head: reusedFrom}
	}
	return dagsched.VerificationRecord{
		Runner: dagsched.GitHubRunner, Repository: "o/r", BaseCommit: base, HeadCommit: head,
		TreeHash: keys.Tree, CiDigest: keys.CiDigest, Tools: map[string]string{}, Pins: keys.Pins,
		PinMismatch: []string{}, Dependencies: keys.Dependencies,
		Result: "pass", Jobs: []json.RawMessage{statusJob()},
		VerifierKind: dagsched.GitHubVerifierKind, VerifierRules: dagsched.GitHubVerifierRules,
		GitHub: gh,
	}
}

func sealedStatusRecord(t *testing.T, dir, head, base, reusedFrom string) []byte {
	t.Helper()
	keys, err := dagsched.CommitVerificationKeys(context.Background(), dir, head)
	if err != nil {
		t.Fatalf("verification keys of %s in %s: %v", head, dir, err)
	}
	raw, err := dagsched.SealVerificationRecord(ghStatusRecord(t, head, base, keys, reusedFrom))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func envOf(env []string, name string) string {
	for _, e := range env {
		if strings.HasPrefix(e, name+"=") {
			return strings.TrimPrefix(e, name+"=")
		}
	}
	return ""
}

func bareHasCommit(bare, head string) bool {
	cmd := exec.Command("git", "-C", bare, "cat-file", "-e", head+"^{commit}")
	return cmd.Run() == nil
}

func (w *world) armStatus(t *testing.T, cfgBody string) *statusRunWorld {
	t.Helper()
	sw := &statusRunWorld{fake: trackingfake.New(t, "o/r")}
	sw.remoteBase = w.remoteDev()
	sw.fake.CommitExists = func(head string) bool { return bareHasCommit(w.remote, head) }
	cfgFile := filepath.Join(w.root, "status-config.json")
	w.write(cfgFile, cfgBody)
	w.r.Env = append(append([]string(nil), w.env...), "CRW_CONFIG="+cfgFile)
	w.r.StatusGitHub = func(repo string) (*tracking.GitHub, error) {
		return tracking.NewGitHub(repo, tracking.StaticToken(trackingfake.Token), tracking.WithBaseURL(sw.fake.URL))
	}
	prev := w.r.Exec
	w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
		argv := strings.Join(c.Argv, " ")
		if strings.HasSuffix(argv, " remote get-url --push --all "+w.cfg.Remote) {
			if c.Stdout != nil {
				_, _ = c.Stdout.Write([]byte("https://github.com/o/r.git\n"))
			}
			return 0, nil
		}
		if strings.Contains(argv, " push "+w.cfg.Remote+" ") && strings.HasSuffix(argv, ":refs/heads/"+w.cfg.Branch) {
			sha := strings.TrimSuffix(c.Argv[len(c.Argv)-1], ":refs/heads/"+w.cfg.Branch)
			sw.targetSHA, sw.pushed = sha, true
			got := sw.fake.Statuses(sha)
			if len(got) != 1 || got[0].Context != "dev-gate" || got[0].State != "success" {
				t.Errorf("the target push ran before the required status landed on %s: %+v", sha, got)
			}
			if got := w.remoteDev(); got != sw.remoteBase {
				t.Errorf("the remote dev moved to %s before the status was published", got)
			}
			if !bareHasCommit(w.remote, sha) {
				t.Errorf("the staged commit %s is not on the destination when the status is read", sha)
			}
		}
		rc, err := prev(ctx, c)
		if err == nil && rc == 0 {
			if head, base, recPath := envOf(c.Env, "CRW_VERIFY_HEAD"), envOf(c.Env, "CRW_VERIFY_BASE"), envOf(c.Env, "CRW_VERIFY_RECORD"); head != "" && recPath != "" {
				if err := os.WriteFile(recPath, sealedStatusRecord(t, c.Dir, head, base, sw.reusedFrom), 0o644); err != nil {
					t.Errorf("the sealed record: %v", err)
				}
			}
		}
		return rc, err
	}
	return sw
}

func statusConfig(context string) string {
	return `{"integration":{"required_status":"` + context + `"}}`
}

func TestRequiredStatusRunnerOrdinaryFresh(t *testing.T) {
	w := newWorld(t)
	head := w.head("feature", map[string]string{"f.txt": "x\n"}, true)
	w.pend("01", head)
	sw := w.armStatus(t, statusConfig("dev-gate"))
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	if !sw.pushed || sw.targetSHA != rep.Summary.Head {
		t.Fatalf("the target push saw sha %q, want the merged head %q", sw.targetSHA, rep.Summary.Head)
	}
	if got := w.remoteDev(); got != rep.Summary.Head {
		t.Fatalf("remote dev = %s, want the merged head %s", got, rep.Summary.Head)
	}
	got := sw.fake.Statuses(rep.Summary.Head)
	if len(got) != 1 || got[0].Context != "dev-gate" || got[0].State != "success" || got[0].TargetURL != "https://github.com/o/r/actions/runs/6" {
		t.Fatalf("statuses on the merged head = %+v", got)
	}
	if got := w.git(w.remote, "rev-parse", "refs/heads/ci/required-status/"+rep.Summary.Head); got != rep.Summary.Head {
		t.Errorf("the staging ref = %s, want %s", got, rep.Summary.Head)
	}
	w.mustWhere("01", "done")
}

func TestRequiredStatusRunnerOrdinaryReused(t *testing.T) {
	w := newWorld(t)
	head := w.head("feature", map[string]string{"f.txt": "x\n"}, true)
	w.pend("01", head)
	sw := w.armStatus(t, statusConfig("dev-gate"))
	sw.reusedFrom = head
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	if rep.Summary.Head == head {
		t.Fatal("the merge reused the candidate sha instead of a merge commit")
	}
	got := sw.fake.Statuses(rep.Summary.Head)
	if len(got) != 1 || got[0].TargetURL != "https://github.com/o/r/actions/runs/6" {
		t.Fatalf("the same-tree status = %+v, want the original run's link on the merged head", got)
	}
	if got := w.remoteDev(); got != rep.Summary.Head {
		t.Fatalf("remote dev = %s, want %s", got, rep.Summary.Head)
	}
}

func TestRequiredStatusRunnerPRFresh(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10", "11")
	w.enqueue("o/r#1")
	sw := w.armStatus(t, statusConfig("dev-gate"))
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	if rep.Summary.Head == pr.HeadSHA {
		t.Fatal("the PR landing did not merge")
	}
	got := sw.fake.Statuses(rep.Summary.Head)
	if len(got) != 1 || got[0].Context != "dev-gate" || got[0].State != "success" {
		t.Fatalf("statuses = %+v", got)
	}
	if got := w.remoteDev(); got != rep.Summary.Head {
		t.Fatalf("remote dev = %s, want %s", got, rep.Summary.Head)
	}
	w.mustWhere("pr-1", "done")
}

func TestRequiredStatusRunnerPRReused(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10", "11")
	w.enqueue("o/r#1")
	sw := w.armStatus(t, statusConfig("dev-gate"))
	sw.reusedFrom = pr.HeadSHA
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	got := sw.fake.Statuses(rep.Summary.Head)
	if len(got) != 1 || got[0].TargetURL != "https://github.com/o/r/actions/runs/6" {
		t.Fatalf("the same-tree PR status = %+v", got)
	}
}

func TestRequiredStatusRunnerVerifierFailure(t *testing.T) {
	w := newWorld(t)
	head := w.head("feature", map[string]string{"f.txt": "x\n"}, true)
	w.pend("01", head)
	w.ctlWrite("verify-rc", "1\n")
	sw := w.armStatus(t, statusConfig("dev-gate"))
	base := w.remoteDev()
	rep := w.pass()
	requireExit(t, rep, ExitVerifyFailed, "verification-failed(rc=1)")
	if sw.pushed || len(sw.fake.Statuses(rep.Summary.Head)) != 0 || w.remoteDev() != base {
		t.Errorf("a failed verification still wrote: pushed=%v statuses=%v remote=%s", sw.pushed, sw.fake.Statuses(rep.Summary.Head), w.remoteDev())
	}
}

func TestRequiredStatusRunnerReviewRefusal(t *testing.T) {
	w := newPRWorld(t, true)
	w.milestone("10", "11")
	w.enqueue("o/r#1")
	w.ctlWrite("review-verdict", "needs_changes")
	sw := w.armStatus(t, statusConfig("dev-gate"))
	base := w.remoteDev()
	rep := w.pass()
	requireExit(t, rep, ExitReviewRefused, "review-needs-changes")
	if sw.pushed || len(sw.fake.Statuses(rep.Summary.Head)) != 0 || w.remoteDev() != base {
		t.Errorf("a refused review still wrote: pushed=%v remote=%s", sw.pushed, w.remoteDev())
	}
	w.mustWhere("pr-1", "blocked")
}

func TestRequiredStatusRunnerDisabled(t *testing.T) {
	w := newWorld(t)
	head := w.head("feature", map[string]string{"f.txt": "x\n"}, true)
	w.pend("01", head)
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	if code := exec.Command("git", "-C", w.remote, "rev-parse", "--verify", "-q", "refs/heads/ci/required-status/"+rep.Summary.Head).Run(); code == nil {
		t.Error("the disabled setting still staged the head")
	}
}

func TestRequiredStatusRunnerStatusFailure(t *testing.T) {
	w := newWorld(t)
	head := w.head("feature", map[string]string{"f.txt": "x\n"}, true)
	w.pend("01", head)
	sw := w.armStatus(t, statusConfig("dev-gate"))
	sw.fake.StatusCode = 403
	base := w.remoteDev()
	rep := w.pass()
	if rep.Exit != ExitMoved {
		t.Fatalf("pass exit = %d (result %q), want ExitMoved", rep.Exit, rep.Result)
	}
	if !strings.HasPrefix(rep.Summary.Result, "required-status-failed:") {
		t.Fatalf("result = %q, want required-status-failed", rep.Summary.Result)
	}
	if sw.pushed || w.remoteDev() != base {
		t.Errorf("a 403 status still pushed the target: pushed=%v remote=%s", sw.pushed, w.remoteDev())
	}
	w.mustWhere("01", "pending")
}

func TestRequiredStatusRunnerBadRecord(t *testing.T) {
	w := newWorld(t)
	head := w.head("feature", map[string]string{"f.txt": "x\n"}, true)
	w.pend("01", head)
	sw := w.armStatus(t, statusConfig("dev-gate"))
	sw.reusedFrom = "\x00"
	base := w.remoteDev()
	prev := w.r.Exec
	w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
		rc, err := prev(ctx, c)
		if p := envOf(c.Env, "CRW_VERIFY_RECORD"); p != "" && err == nil && rc == 0 {
			_ = os.WriteFile(p, []byte(`{"ok":true}`), 0o644)
		}
		return rc, err
	}
	rep := w.pass()
	if rep.Exit != ExitMoved {
		t.Fatalf("pass exit = %d (result %q), want ExitMoved", rep.Exit, rep.Result)
	}
	if !strings.HasPrefix(rep.Summary.Result, "required-status-failed:") {
		t.Fatalf("result = %q", rep.Summary.Result)
	}
	if sw.pushed || len(sw.fake.Statuses(rep.Summary.Head)) != 0 || w.remoteDev() != base {
		t.Errorf("a malformed record still wrote: pushed=%v remote=%s", sw.pushed, w.remoteDev())
	}
	w.mustWhere("01", "pending")
}

func TestRequiredStatusRunnerBadConfig(t *testing.T) {
	w := newWorld(t)
	head := w.head("feature", map[string]string{"f.txt": "x\n"}, true)
	w.pend("01", head)
	sw := w.armStatus(t, `{"integration":{"required_status":null}}`)
	base := w.remoteDev()
	rep := w.pass()
	if rep.Exit != ExitMoved {
		t.Fatalf("pass exit = %d (result %q), want ExitMoved", rep.Exit, rep.Result)
	}
	if !strings.HasPrefix(rep.Summary.Result, "required-status-failed:") {
		t.Fatalf("result = %q", rep.Summary.Result)
	}
	if sw.pushed || w.remoteDev() != base {
		t.Errorf("a null setting still wrote: pushed=%v remote=%s", sw.pushed, w.remoteDev())
	}
}
