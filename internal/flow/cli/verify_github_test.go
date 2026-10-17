package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// The fake remote toolchain of the flow-side remote-verifier tests (CRW-1594): a bin directory of git,
// gitleaks and gh scripts. git delegates to the real binary except `git push`, which is only recorded;
// gitleaks records and exits CRW_FAKE_SCAN_CODE (0); gh answers from files in CRW_FAKE_DIR: workflow.json the
// `gh api .../actions/workflows` answer, runs.jsonl the `gh run list` rows, view-<id>.json the `gh api .../runs/ID` answer,
// view-<id>-attempt2.json what the run answers after `gh run rerun`, logfailed-<job>.txt the --log-failed
// output. next-id is the databaseId the next dispatch gets.
func fakeRemoteBin(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("git", "#!/bin/sh\ncase \"$1\" in push) echo \"git $*\" >> \"$CRW_FAKE_DIR/pushes.log\"; exit 0;; esac\nexec \""+realGit+"\" \"$@\"\n")
	write("gitleaks", "#!/bin/sh\ncase \"$1\" in version) echo \"${CRW_FAKE_GITLEAKS_VERSION:-8.30.1}\"; exit 0;; esac\necho \"gitleaks $* DIR=$(pwd)\" >> \"$CRW_FAKE_DIR/scans.log\"\nexit ${CRW_FAKE_SCAN_CODE:-0}\n")
	write("gh", `#!/bin/sh
D="$CRW_FAKE_DIR"
echo "gh $*" >> "$D/calls.log"
case "$1 $2" in
"api --paginate")
  for url in "$@"; do :; done
  case "$url" in
    */actions/workflows*) cat "$D/workflows.json" ;;
    */attempts/*/jobs*)
      key=$(echo "$url" | sed 's|.*/runs/\([0-9]*\)/attempts/\([0-9]*\)/jobs.*|\1-\2|')
      if [ -f "$D/jobs-$key.json" ]; then cat "$D/jobs-$key.json";
      else echo '{"jobs":[{"id":101,"name":"ci","status":"completed","conclusion":"success","steps":[{"name":"test","number":1,"status":"completed","conclusion":"success"}]}]}'; fi
      ;;
    */actions/runs/*)
      id=$(echo "$url" | sed 's|.*/runs/||;s|[^0-9].*||')
      cat "$D/view-$id.json"
      ;;
    *) exit 1 ;;
  esac
  ;;
"api repos/"*)
  for url in "$@"; do :; done
  case "$url" in
    */actions/runs/*)
      id=$(echo "$url" | sed 's|.*/runs/||;s|[^0-9].*||')
      if [ -f "$D/view-$id.json" ]; then cat "$D/view-$id.json";
      else
        # automatic successful run mode (CRW-1594 lane test): the run's own pushed sha/branch from runs.jsonl
        line=$(grep "\"databaseId\":$id," "$D/runs.jsonl" | head -1)
        sha=$(echo "$line" | sed -n 's/.*"headSha":"\([^"]*\)".*/\1/p')
        br=$(echo "$line" | sed -n 's/.*"headBranch":"\([^"]*\)".*/\1/p')
        att=$(awk -v id="$id" -F'"attempt":' '/"databaseId":'"$id"'/ {print $2}' "$D/reruns.log" 2>/dev/null | tr -d '}' )
        att=1
        echo "{\"id\":$id,\"head_sha\":\"$sha\",\"head_branch\":\"$br\",\"event\":\"workflow_dispatch\",\"workflow_id\":7,\"status\":\"completed\",\"conclusion\":\"success\",\"run_attempt\":1,\"html_url\":\"https://github.com/fake-owner/fake-repo/actions/runs/$id\"}"
      fi
      ;;
    *) exit 1 ;;
  esac
  ;;
"workflow run")
  prev=""; ref=""
  for a in "$@"; do [ "$prev" = "--ref" ] && ref="$a"; prev="$a"; done
  spec=$(tail -1 "$D/pushes.log" | awk '{print $NF}')
  sha=${spec%%:*}
  id=$(cat "$D/next-id"); echo $((id + 1)) > "$D/next-id"
  echo "{\"databaseId\":$id,\"headSha\":\"$sha\",\"headBranch\":\"$ref\",\"event\":\"workflow_dispatch\",\"workflowDatabaseId\":7}" >> "$D/runs.jsonl"
  ;;
"run list")
  if [ -s "$D/runs.jsonl" ]; then { echo "["; paste -sd, "$D/runs.jsonl"; echo "]"; } else echo "[]"; fi
  ;;
"run view")
  case "$*" in
    *--job*)
      prev=""; job=""
      for a in "$@"; do [ "$prev" = "--job" ] && job="$a"; prev="$a"; done
      cat "$D/logfailed-$job.txt"
      ;;
    *) echo "gh run view without --job is not in the real protocol" >&2; exit 1 ;;
  esac
  ;;
"run rerun")
  [ -f "$D/view-$3-attempt2.json" ] && cp "$D/view-$3-attempt2.json" "$D/view-$3.json"
  echo "gh $*" >> "$D/reruns.log"
  ;;
"auth status") exit 0 ;;
"repo view") echo '{"viewerPermission":"WRITE"}' ;;
*) echo "gh: unexpected $*" >&2; exit 1 ;;
esac
exit 0
`)
	return bin
}

// remoteCheckout is a git checkout of CRW-1594's shape: a base commit with the workflow file, a head commit a
// devin/ branch names, and origin/dev at the base. The repository is fake-owner/fake-repo.
func remoteCheckout(t *testing.T) (repo, base, head string) {
	t.Helper()
	repo = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	put := func(name, body string) {
		p := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put(".github/workflows/ci.yml", "name: CRW CI\non: [workflow_dispatch]\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps: []\n")
	put("go.mod", "module example.test/m\n\ngo 1.27\n")
	put("go.sum", "module h1:abc=\n")
	put("scripts/ci/secrets.sh", "scan_version=8.30.1\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	base = git("rev-parse", "HEAD")
	git("remote", "add", "origin", "https://github.com/fake-owner/fake-repo.git")
	git("update-ref", "refs/remotes/origin/dev", base)
	put("a.txt", "work\n")
	git("add", "-A")
	git("commit", "-q", "-m", "work")
	head = git("rev-parse", "HEAD")
	git("branch", "devin/fake-work", head)
	return repo, base, head
}

// remoteFixture prepares the fake dir of fakeRemoteBin for a dispatch whose run gets databaseId 6, then the
// view files the test hands it. It answers (dir, run id).
func remoteFixture(t *testing.T, head, branch string) (dir string, id int64) {
	t.Helper()
	dir = t.TempDir()
	id = 6
	writeAt := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeAt("workflows.json", `{"workflows":[{"id":7,"name":"CRW CI","path":".github/workflows/ci.yml","state":"active"}]}`)
	writeAt("next-id", "6\n")
	url := "https://github.com/fake-owner/fake-repo/actions/runs/6"
	writeAt("view-6.json", fmt.Sprintf(`{"id":6,"head_sha":%q,"head_branch":%q,"event":"workflow_dispatch","workflow_id":7,"status":"completed","conclusion":"success","run_attempt":1,"html_url":%q}`, head, branch, url))
	writeAt("jobs-6-1.json", fmt.Sprintf(`{"jobs":[{"id":101,"name":"build","status":"completed","conclusion":"success","html_url":%q,"steps":[{"name":"test","number":1,"status":"completed","conclusion":"success"}]}]}`, url+"/job/101"))
	return dir, id
}

func remoteConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"verifier":{"kind":"github-actions","repository":"fake-owner/fake-repo","workflow":"CRW CI"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// CRW-1594: `crw flow verify` runs the configured verifier — here github-actions — over the checkout, writes
// the sealed record where --record names, answers 0 on pass, and prints the run's URL.
func TestFlowVerifyRunsTheConfiguredRemoteVerifier(t *testing.T) {
	repo, base, head := remoteCheckout(t)
	dir, _ := remoteFixture(t, head, "devin/fake-work")
	bin := fakeRemoteBin(t, dir)
	record := filepath.Join(t.TempDir(), "record.json")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRW_CONFIG", remoteConfig(t))
	t.Setenv("CRW_FAKE_DIR", dir)
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"verify", "--checkout", repo, "--head", head, "--base", base, "--record", record}, nil, &out, &errOut)
	if code != 0 {
		t.Fatalf("verify: exit %d\n%s%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "https://github.com/fake-owner/fake-repo/actions/runs/6") {
		t.Errorf("the output names no run URL: %q", out.String())
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("no record: %v", err)
	}
	rec, err := dagsched.DecodeVerificationRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Runner != "github-actions" || rec.HeadCommit != head || rec.BaseCommit != base || rec.Result != "pass" {
		t.Errorf("the record: %+v", rec)
	}
	keys, err := dagsched.CommitVerificationKeys(context.Background(), repo, head)
	if err != nil {
		t.Fatal(err)
	}
	keys.Base = base
	if _, err := dagsched.JudgeVerificationRecord(raw, keys); err != nil {
		t.Errorf("the record is not accepted: %v", err)
	}
	push, err := os.ReadFile(filepath.Join(dir, "pushes.log"))
	if err != nil || !strings.Contains(string(push), head+":refs/heads/devin/fake-work") {
		t.Errorf("the push: %q %v", push, err)
	}
}

// CRW-1594: `crw flow verify` writes the failure receipt before it answers 1; a configuration or
// infrastructure failure is 2.
func TestFlowVerifyExits(t *testing.T) {
	repo, base, head := remoteCheckout(t)
	dir, _ := remoteFixture(t, head, "devin/fake-work")
	bin := fakeRemoteBin(t, dir)
	// the run fails and its rerun fails too
	url := "https://github.com/fake-owner/fake-repo/actions/runs/6"
	writeView := func(name, conclusion string, attempt int) {
		body := fmt.Sprintf(`{"id":6,"head_sha":%q,"head_branch":"devin/fake-work","event":"workflow_dispatch","workflow_id":7,"status":"completed","conclusion":%q,"run_attempt":%d,"html_url":%q}`, head, conclusion, attempt, url)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeView("view-6.json", "failure", 1)
	writeView("view-6-attempt2.json", "failure", 2)
	if err := os.WriteFile(filepath.Join(dir, "jobs-6-1.json"), []byte(`{"jobs":[{"id":101,"name":"tests","status":"completed","conclusion":"failure"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "jobs-6-2.json"), []byte(`{"jobs":[{"id":101,"name":"tests","status":"completed","conclusion":"failure"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logfailed-101.txt"), []byte("--- FAIL: TestIt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "record.json")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRW_CONFIG", remoteConfig(t))
	t.Setenv("CRW_FAKE_DIR", dir)
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"verify", "--checkout", repo, "--head", head, "--base", base, "--record", record}, nil, &out, &errOut)
	if code != 1 {
		t.Fatalf("a failed verification: exit %d\n%s%s", code, out.String(), errOut.String())
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the failure receipt was not written: %v", err)
	}
	rec, err := dagsched.DecodeVerificationRecord(raw)
	if err != nil || rec.Result != "fail" {
		t.Errorf("the receipt: %v %+v", err, rec)
	}
	// a configuration that is no configuration is 2
	t.Setenv("CRW_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	if err := os.WriteFile(os.Getenv("CRW_CONFIG"), []byte(`{"verifier":{"kind":"github-actions"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	code = Run(context.Background(), []string{"verify", "--checkout", repo, "--head", head, "--record", record}, nil, &out, &errOut)
	if code != 2 {
		t.Errorf("a refused configuration: exit %d\n%s%s", code, out.String(), errOut.String())
	}
}

// CRW-1594: ConfiguredIntegration keeps the local verifier for a local configuration and hands a remote one
// the batch's CRW_VERIFY_*: the record lands where CRW_VERIFY_RECORD names, a pass is nil, a failed
// verification is an ordinary error naming the failure and a host failure is IntegrationVerifierHostError.
func TestConfiguredIntegrationSelectsTheVerifier(t *testing.T) {
	repo, base, head := remoteCheckout(t)
	dir, _ := remoteFixture(t, head, "devin/fake-work")
	bin := fakeRemoteBin(t, dir)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRW_FAKE_DIR", dir)
	record := filepath.Join(t.TempDir(), "record.json")
	var notes bytes.Buffer
	tools := crwconfig.VerifierTools{Kind: crwconfig.VerifierGitHubActions, Repository: "fake-owner/fake-repo", Workflow: "CRW CI"}
	verify := ConfiguredIntegration(tools, &notes)
	env := []string{"CRW_VERIFY_HEAD=" + head, "CRW_VERIFY_BASE=" + base, "CRW_VERIFY_RECORD=" + record}
	if err := verify(context.Background(), repo, env); err != nil {
		t.Fatalf("the integration verification: %v", err)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the record was not written: %v", err)
	}
	keys, err := dagsched.CommitVerificationKeys(context.Background(), repo, head)
	if err != nil {
		t.Fatal(err)
	}
	keys.Base = base
	if _, err := dagsched.JudgeVerificationRecord(raw, keys); err != nil {
		t.Errorf("the record is not accepted: %v", err)
	}
	if !strings.Contains(notes.String(), "github-actions") {
		t.Errorf("no note: %q", notes.String())
	}
	// a host failure is a host error: with the fake gh gone, the run cannot complete
	badDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(badDir, "workflows.json"), []byte(`{"workflows":[{"id":7,"name":"CRW CI","path":".github/workflows/ci.yml","state":"active"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))
	t.Setenv("CRW_FAKE_DIR", badDir)
	err = ConfiguredIntegration(tools, nil)(context.Background(), repo, env)
	var host *dagsched.IntegrationVerifierHostError
	if err == nil || !errors.As(err, &host) {
		t.Errorf("a missing toolchain: %v", err)
	}
}

// CRW-1594: the remote preflight names the repository and workflow, needs no Go, and is a warning with the
// probe's problems when the remote cannot run.
func TestVerifierPrerequisitesRemote(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	bin := fakeRemoteBin(t, dir)
	writeAt := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeAt("workflows.json", `{"workflows":[{"id":7,"name":"CRW CI","path":".github/workflows/ci.yml","state":"active"}]}`)
	writeAt("repo.json", `{"viewerPermission":"WRITE"}`)
	config := filepath.Join(home, "config.json")
	if err := os.WriteFile(config, []byte(`{"verifier":{"kind":"github-actions","repository":"fake-owner/fake-repo","workflow":"CRW CI"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// the preflight gh needs auth and repo answers too — fakeRemoteBin's gh covers auth status and repo view
	if err := os.MkdirAll(filepath.Join(home, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home, "PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "CRW_CONFIG=" + config, "CRW_FAKE_DIR=" + dir}
	fact := VerifierPrerequisites(context.Background(), env, "")
	if !fact.Pass || !strings.Contains(fact.Reading, "fake-owner/fake-repo") || !strings.Contains(fact.Reading, "CRW CI") {
		t.Fatalf("the remote fact: %+v", fact)
	}
	if strings.Contains(fact.Reading, "no go") || strings.Contains(fact.Reading, "crw-dev") {
		t.Errorf("the remote fact speaks of local tools: %q", fact.Reading)
	}
}

// CRW-1594: the shipped lane verifies with the configured verifier — BuildLane's defaults select
// github-actions here, so a node's head and the milestone's close are verified by the fake GitHub Actions
// toolchain: one dispatch for the node's run, the same-tree integration and final verifications reusing its
// receipt, every range freshly scanned, and no crw-dev anywhere.
func TestBuildLaneSelectsGitHubForNodeIntegrationAndClose(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	// the lab's base carries the workflow and the module file the remote record digests
	writeLabFile(t, l.repo, ".github/workflows/ci.yml", "name: CRW CI\non: [workflow_dispatch]\njobs:\n  ci:\n    runs-on: ubuntu-latest\n    steps: []\n", 0o644)
	writeLabFile(t, l.repo, "go.mod", "module example.test/lab\n\ngo 1.27\n", 0o644)
	writeLabFile(t, l.repo, "scripts/ci/secrets.sh", "scan_version=8.30.1\n", 0o644)
	l.git(l.repo, "add", "-A")
	l.git(l.repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "ci setup")
	setup := l.git(l.repo, "rev-parse", "HEAD")
	l.git(l.repo, "update-ref", "refs/heads/"+l.target, setup)
	l.git(l.repo, "remote", "add", "origin", "https://github.com/fake-owner/fake-repo.git")
	l.git(l.repo, "update-ref", "refs/remotes/origin/"+l.target, setup)
	dir := t.TempDir()
	writeAt := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeAt("workflows.json", `{"workflows":[{"id":7,"name":"CRW CI","path":".github/workflows/ci.yml","state":"active"}]}`)
	writeAt("next-id", "1\n")
	bin := fakeRemoteBin(t, dir)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRW_FAKE_DIR", dir)
	t.Setenv("CRW_CONFIG", remoteConfig(t))
	// BuildLane with its shipped defaults: no Verifier, no IntegrationVerify.
	OpsFor = func(ctx context.Context, req LaneRequest) (LaneWorld, error) {
		return BuildLane(ctx, req, LaneOptions{SkillsDigest: "sha256:lab-skills", Reporter: &labReporter{l: l},
			UnitCheck: l.uc.seam(), Crosscheck: l.cc.seam(), Relay: func(r *SchedRelay) ops.Relay { return &labRelay{SchedRelay: r, l: l} }})
	}
	spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
	l.closed(l.runToEnd(spec.RunID))
	// exactly one workflow dispatch over the whole lane: the node's head verified once, every same-tree
	// verification after it reusing that run's receipt
	runs, err := os.ReadFile(filepath.Join(dir, "runs.jsonl"))
	if err != nil || len(strings.Split(strings.TrimSpace(string(runs)), "\n")) != 1 {
		t.Fatalf("the dispatches: %q %v", runs, err)
	}
	scans, err := os.ReadFile(filepath.Join(dir, "scans.log"))
	if err != nil || len(strings.Split(strings.TrimSpace(string(scans)), "\n")) < 2 {
		t.Errorf("the scans of the ranges verified: %q %v", scans, err)
	}
	if push, _ := os.ReadFile(filepath.Join(dir, "pushes.log")); !strings.Contains(string(push), "refs/heads/") {
		t.Errorf("no head was pushed: %q", push)
	}
	// no crw-dev ran anywhere: nothing on the fake PATH is called but git, gitleaks and gh
	if calls, err := os.ReadFile(filepath.Join(dir, "calls.log")); err == nil && strings.Contains(string(calls), "crw-dev") {
		t.Errorf("crw-dev ran: %s", calls)
	}
	// the evidence on disk is the sealed remote receipts
	var records []dagsched.VerificationRecord
	filepath.Walk(filepath.Dir(l.repo), func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.HasPrefix(info.Name(), "verification-record-") && strings.HasSuffix(info.Name(), ".json") {
			if raw, err := os.ReadFile(p); err == nil {
				if rec, err := dagsched.DecodeVerificationRecord(raw); err == nil {
					records = append(records, rec)
				}
			}
		}
		return nil
	})
	// at least the surviving receipt is a remote pass, and it is the one pushed head's run
	if len(records) == 0 {
		t.Fatalf("the lane left no remote receipt")
	}
	for _, rec := range records {
		if rec.Runner != "github-actions" || rec.Result != "pass" {
			t.Errorf("a record that is not a remote pass: runner %q result %q", rec.Runner, rec.Result)
		}
	}
	// the dispatched run's head is a commit of the lab's history: the pushed sha from the fake's push log
	push, _ := os.ReadFile(filepath.Join(dir, "pushes.log"))
	refspec := ""
	if f := strings.Fields(string(push)); len(f) > 0 {
		refspec = f[len(f)-1]
	}
	pushedHead := refspec[:strings.Index(refspec, ":")]
	if rec := records[0]; rec.GitHub == nil || (rec.GitHub.RunHead != pushedHead && rec.HeadCommit != pushedHead) {
		t.Errorf("the receipt names no pushed head %s: %+v", pushedHead, rec.GitHub)
	}
}

func writeLabFile(t *testing.T, dir, name, body string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}
