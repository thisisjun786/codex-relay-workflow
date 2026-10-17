package integrate

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

func (e *targetEnv) statusRecord(t *testing.T, digestOnly bool) string {
	t.Helper()
	ctx := context.Background()
	keys, err := dagsched.CommitVerificationKeys(ctx, e.repo, e.moved)
	if err != nil {
		t.Fatal(err)
	}
	job, _ := json.Marshal(map[string]any{
		"name": "build", "status": "completed", "conclusion": "success", "result": "passed", "databaseId": 101,
		"url":   "https://github.com/o/r/actions/runs/6/job/101",
		"steps": []map[string]any{{"name": "test", "number": 1, "status": "completed", "conclusion": "success", "result": "passed"}},
	})
	record, err := dagsched.SealVerificationRecord(dagsched.VerificationRecord{
		Runner: dagsched.GitHubRunner, Repository: "o/r", BaseCommit: e.base, HeadCommit: e.moved,
		TreeHash: keys.Tree, CiDigest: keys.CiDigest, Tools: map[string]string{}, Pins: keys.Pins,
		PinMismatch: []string{}, Dependencies: keys.Dependencies, Result: "pass",
		Jobs:          []json.RawMessage{job},
		VerifierKind:  dagsched.GitHubVerifierKind,
		VerifierRules: dagsched.GitHubVerifierRules,
		GitHub: &dagsched.GitHubVerification{
			RunID: 6, URL: "https://github.com/o/r/actions/runs/6", RunHead: e.moved,
			Branch: "ci/verify-abcdef12", Event: "workflow_dispatch", WorkflowID: 7,
			WorkflowPath: ".github/workflows/ci.yml", WorkflowDigest: "sha256:" + strings.Repeat("f", 64),
			Conclusion: "success", Attempts: 1,
			Runs: []dagsched.GitHubRunAttempt{{Attempt: 1, Status: "completed", Conclusion: "success", Jobs: []json.RawMessage{job}}},
			Scan: dagsched.GitHubScan{Base: e.base, Head: e.moved, Result: "clean"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	intent := map[string]string{
		"integration_ref":     "dev-int",
		"base":                e.base,
		"verified_head":       e.moved,
		"verification_digest": shaDigest(record),
		"verification_base":   e.base,
	}
	if !digestOnly {
		intent["verification_record"] = string(record)
	}
	detail, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	return string(detail)
}

func shaDigest(raw []byte) string {
	d, err := dagsched.VerificationRecordDigest(raw)
	if err != nil {
		panic(err)
	}
	return d
}

func (e *targetEnv) rewriteIntent(t *testing.T, digestOnly bool) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(e.state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	detail := e.statusRecord(t, digestOnly)
	for _, r := range []store.IntegrationStageRow{
		{StageID: "s-record-intent", BatchID: "b2", PlanID: "g", Stage: "intent", Detail: detail, RecordedBy: "parent", RecordedAt: "2026-10-14T00:00:02Z"},
		{StageID: "s-record-moved", BatchID: "b2", PlanID: "g", Stage: "ref_moved", Detail: e.moved, HeadSHA: e.moved, RecordedBy: "parent", RecordedAt: "2026-10-14T00:00:03Z"},
	} {
		if err := store.RecordIntegrationStage(ctx, s, r); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *targetEnv) armStatusSeams(t *testing.T, cfgBody string) *trackingfake.Server {
	t.Helper()
	fake := trackingfake.New(t, "o/r")
	fake.CommitExists = func(head string) bool {
		if got := gitIn(t, e.remote, "rev-parse", "--verify", "-q", "refs/heads/dev"); got != e.base {
			t.Errorf("the target branch moved to %s before the status was posted", got)
		}
		return exec.Command("git", "-C", e.remote, "cat-file", "-e", head+"^{commit}").Run() == nil
	}
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", cfg)
	prevGit, prevGH := integrationStatusGit, integrationStatusGitHub
	t.Cleanup(func() { integrationStatusGit, integrationStatusGitHub = prevGit, prevGH })
	integrationStatusGit = func(ctx context.Context, checkout string, args ...string) (string, error) {
		if strings.Join(args, " ") == "remote get-url --push --all origin" {
			return "https://github.com/o/r.git", nil
		}
		return gitIn(t, checkout, args...), nil
	}
	integrationStatusGitHub = func(repo string) (*tracking.GitHub, error) {
		return tracking.NewGitHub(repo, tracking.StaticToken(trackingfake.Token), tracking.WithBaseURL(fake.URL))
	}
	return fake
}

func TestRequiredStatusRelayPush(t *testing.T) {
	e := newTargetEnv(t)
	e.rewriteIntent(t, false)
	fake := e.armStatusSeams(t, `{"integration":{"required_status":"dev-gate"}}`)
	doc, err := e.run("dag-integrate-push", e.pushArgs()...)
	if err != nil || doc["outcome"] != "pushed" {
		t.Fatalf("push = %v, %v", doc, err)
	}
	got := fake.Statuses(e.moved)
	if len(got) != 1 || got[0].Context != "dev-gate" || got[0].State != "success" || got[0].TargetURL != "https://github.com/o/r/actions/runs/6" {
		t.Fatalf("statuses = %+v", got)
	}
	if got := gitIn(t, e.remote, "rev-parse", "dev"); got != e.moved {
		t.Errorf("remote dev = %s, want %s", got, e.moved)
	}
	if got := gitIn(t, e.remote, "rev-parse", "refs/heads/ci/required-status/"+e.moved); got != e.moved {
		t.Errorf("the staging ref = %s, want %s", got, e.moved)
	}
}

func TestRequiredStatusRelayPushDisabled(t *testing.T) {
	e := newTargetEnv(t)
	fake := e.armStatusSeams(t, `{"integration":{}}`)
	doc, err := e.run("dag-integrate-push", e.pushArgs()...)
	if err != nil || doc["outcome"] != "pushed" {
		t.Fatalf("push = %v, %v", doc, err)
	}
	if got := fake.Statuses(e.moved); len(got) != 0 {
		t.Errorf("a disabled setting still posted: %+v", got)
	}
	if len(fake.Requests()) != 0 {
		t.Errorf("a disabled setting still made API calls: %v", fake.Requests())
	}
}

func TestRequiredStatusRelayPushMissingProof(t *testing.T) {
	e := newTargetEnv(t)
	e.armStatusSeams(t, `{"integration":{"required_status":"dev-gate"}}`)
	if _, err := e.run("dag-integrate-push", e.pushArgs()...); err == nil {
		t.Fatal("a digest-only move pushed")
	}
	if got := gitIn(t, e.remote, "rev-parse", "dev"); got != e.base {
		t.Errorf("remote dev = %s, want the base %s", got, e.base)
	}
}

func TestRequiredStatusRelayPushStatusFailure(t *testing.T) {
	e := newTargetEnv(t)
	e.rewriteIntent(t, false)
	fake := e.armStatusSeams(t, `{"integration":{"required_status":"dev-gate"}}`)
	fake.StatusCode = 403
	if _, err := e.run("dag-integrate-push", e.pushArgs()...); err == nil {
		t.Fatal("a failed status pushed the target")
	}
	if got := gitIn(t, e.remote, "rev-parse", "dev"); got != e.base {
		t.Errorf("remote dev = %s, want the base %s", got, e.base)
	}
}
