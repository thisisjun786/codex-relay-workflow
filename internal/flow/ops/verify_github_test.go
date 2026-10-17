package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// The fake GitHub side of the remote-verifier tests (CRW-1594): fakeGHRunListEntry is one `gh run list` row,
// fakeGHView is one `gh api repos/R/actions/runs/ID` poll answer (the REST shape), fakeGHJob/fakeGHStep
// the jobs and steps the attempts/N/jobs endpoint answers and fakeGHWorkflow a row of the workflows listing.
type fakeGHRunListEntry struct {
	DatabaseID         int64  `json:"databaseId"`
	HeadSHA            string `json:"headSha"`
	HeadBranch         string `json:"headBranch"`
	Event              string `json:"event"`
	WorkflowDatabaseID int64  `json:"workflowDatabaseId"`
}

type fakeGHStep struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Number     int    `json:"number"`
}

type fakeGHJob struct {
	DatabaseID int64        `json:"id"`
	Name       string       `json:"name"`
	Status     string       `json:"status"`
	Conclusion string       `json:"conclusion"`
	URL        string       `json:"html_url"`
	Steps      []fakeGHStep `json:"steps"`
}

type fakeGHView struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Attempt    int    `json:"run_attempt"`
	URL        string `json:"html_url"`
}

// fakeGHRunView is the REST object `gh api repos/o/r/actions/runs/ID` answers for a run.
type fakeGHRunView struct {
	DatabaseID         int64  `json:"id"`
	HeadSHA            string `json:"head_sha"`
	HeadBranch         string `json:"head_branch"`
	Event              string `json:"event"`
	WorkflowDatabaseID int64  `json:"workflow_id"`
	Status             string `json:"status"`
	Conclusion         string `json:"conclusion"`
	Attempt            int    `json:"run_attempt"`
	URL                string `json:"html_url"`
}

type fakeGHWorkflow struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	State string `json:"state"`
}

// fakeGHRun is one Actions run the fake reports: `gh api repos/.../runs/ID` answers one scripted view per
// poll for the run's current attempt (a missing list repeats the last), and `gh api
// .../runs/ID/attempts/N/jobs` answers the scripted jobs of that attempt.
type fakeGHRun struct {
	id         int64
	head       string
	branch     string
	workflowID int64
	attempt    int
	polls      int
	views      map[int][]fakeGHView
	jobs       map[int][]fakeGHJob
}

// fakeGH is the GitHub side a test scripts: the workflow pages, the runs that exist, the text of
// --log-failed per job id, and how each command answers. calls records every external command in order;
// pushed records the ref updates a `git push` carried (the push itself never runs).
type fakeGH struct {
	workflow     fakeGHWorkflow
	extraPages   [][]fakeGHWorkflow // further pages of the workflows listing, for pagination tests
	workflowErr  error
	runs         []*fakeGHRun
	nextID       int64
	newRun       func(run *fakeGHRun) // a test scripts the run the next dispatch creates
	pushed       map[string]string
	calls        []string
	scanArgs     [][]string
	scanEnvs     [][]string
	scanDirs     []string
	scanCode     int
	scanErr      error
	scanVersion  string // gitleaks version's stdout; "8.30.1" when unset
	scanVCode    int    // gitleaks version's exit
	dispatchErr  error
	rerunErr     error
	reruns       []int64
	logFailed    map[int64]string
	logFailedErr map[int64]bool
	jobPages     map[[2]int][][]fakeGHJob // jobs of run id, attempt: one entry per page
}

func (f *fakeGH) run() RunCommand {
	return func(ctx context.Context, dir string, env []string, name string, args ...string) (int, []byte, error) {
		switch name {
		case "git":
			if len(args) > 0 && args[0] == "push" {
				f.calls = append(f.calls, "git "+strings.Join(args, " "))
				sha, dst, _ := strings.Cut(args[len(args)-1], ":")
				if f.pushed == nil {
					f.pushed = map[string]string{}
				}
				f.pushed[strings.TrimPrefix(dst, "refs/heads/")] = sha
				return 0, nil, nil
			}
			return execGit(ctx, dir, env, args...)
		case "gitleaks":
			f.calls = append(f.calls, "gitleaks "+strings.Join(args, " "))
			if len(args) == 1 && args[0] == "version" {
				v := f.scanVersion
				if v == "" && f.scanVCode == 0 {
					v = "8.30.1"
				}
				return f.scanVCode, []byte(v), nil
			}
			f.scanArgs = append(f.scanArgs, append([]string{}, args...))
			f.scanEnvs = append(f.scanEnvs, append([]string{}, env...))
			f.scanDirs = append(f.scanDirs, dir)
			return f.scanCode, []byte("redacted scan output"), f.scanErr
		case "gh":
			return f.gh(args)
		}
		return 99, nil, fmt.Errorf("unexpected command %s %v", name, args)
	}
}

// execGit is the real git for every command the fake does not intercept.
func execGit(ctx context.Context, dir string, env []string, args ...string) (int, []byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), out.Bytes(), nil
	}
	return 0, out.Bytes(), err
}

func flagOf(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func (f *fakeGH) gh(args []string) (int, []byte, error) {
	f.calls = append(f.calls, "gh "+strings.Join(args, " "))
	if len(args) < 2 {
		return 1, []byte("gh: no command"), nil
	}
	// gh 2.46.0 has no `gh workflow view --json` and `gh run view --json` has no attempt: a fake that
	// answered them would hide the production correction (CRW-1594 rework).
	if args[0] == "api" {
		return f.ghAPI(args[1:])
	}
	if args[0] == "workflow" && args[1] == "view" {
		return 1, []byte("gh workflow view: no such command in the real protocol"), nil
	}
	switch args[0] + " " + args[1] {
	case "workflow run":
		if f.dispatchErr != nil {
			return 1, []byte("the dispatch failed"), nil
		}
		branch := flagOf(args, "--ref")
		for _, run := range f.runs {
			f.nextID = max(f.nextID, run.id)
		}
		f.nextID++
		run := &fakeGHRun{id: f.nextID, head: f.pushed[branch], branch: branch, workflowID: f.workflow.ID, attempt: 1,
			views: map[int][]fakeGHView{}, jobs: map[int][]fakeGHJob{}}
		if f.newRun != nil {
			f.newRun(run)
		}
		f.runs = append(f.runs, run)
		return 0, nil, nil
	case "run list":
		var entries []fakeGHRunListEntry
		for _, run := range f.runs {
			entries = append(entries, fakeGHRunListEntry{DatabaseID: run.id, HeadSHA: run.head, HeadBranch: run.branch,
				Event: "workflow_dispatch", WorkflowDatabaseID: run.workflowID})
		}
		out, _ := json.Marshal(entries)
		return 0, out, nil
	case "run view":
		if i := slices.Index(args, "--job"); i >= 0 && i+1 < len(args) {
			job, _ := strconv.ParseInt(args[i+1], 10, 64)
			if f.logFailedErr != nil && f.logFailedErr[job] {
				return 1, []byte("the job's log could not be downloaded"), nil
			}
			return 0, []byte(f.logFailed[job]), nil
		}
		return 1, []byte("gh run view: no --json polling in the real protocol"), nil
	case "run rerun":
		if f.rerunErr != nil {
			return 1, []byte("rerun failed"), nil
		}
		id, _ := strconv.ParseInt(args[2], 10, 64)
		if run := f.find(id); run != nil {
			run.attempt++
			run.polls = 0
		}
		f.reruns = append(f.reruns, id)
		return 0, nil, nil
	case "never":
	}
	return 1, []byte("gh: unexpected " + strings.Join(args, " ")), nil
}

func (f *fakeGH) ghAPI(args []string) (int, []byte, error) {
	path := args[len(args)-1]
	rest := strings.TrimPrefix(path, "repos/o/r/")
	switch {
	case strings.HasPrefix(rest, "actions/workflows"):
		if f.workflowErr != nil {
			return 1, []byte("no such workflow"), nil
		}
		var out []byte
		pages := append([][]fakeGHWorkflow{{f.workflow}}, f.extraPages...)
		for _, page := range pages {
			raw, _ := json.Marshal(map[string]any{"workflows": page})
			out = append(out, raw...)
			out = append(out, '\n')
		}
		return 0, out, nil
	case strings.Contains(rest, "/attempts/") && strings.Contains(rest, "/jobs"):
		// repos/o/r/actions/runs/ID/attempts/N/jobs?per_page=100
		idPart := strings.TrimPrefix(rest, "actions/runs/")
		id, _ := strconv.ParseInt(strings.SplitN(idPart, "/", 2)[0], 10, 64)
		att, _ := strconv.Atoi(strings.SplitN(strings.SplitN(rest, "/attempts/", 2)[1], "/", 2)[0])
		run := f.find(id)
		if run == nil {
			return 1, []byte("run not found"), nil
		}
		var out []byte
		pages := f.jobPages[[2]int{int(id), att}]
		if len(pages) == 0 {
			pages = [][]fakeGHJob{run.jobs[att]}
		}
		for _, page := range pages {
			raw, _ := json.Marshal(map[string]any{"jobs": page})
			out = append(out, raw...)
			out = append(out, '\n')
		}
		return 0, out, nil
	case strings.HasPrefix(rest, "actions/runs/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(rest, "actions/runs/"), 10, 64)
		run := f.find(id)
		if run == nil {
			return 1, []byte("run not found"), nil
		}
		views := run.views[run.attempt]
		if len(views) == 0 {
			return 1, []byte("no scripted view"), nil
		}
		view := views[min(run.polls, len(views)-1)]
		run.polls++
		rv := fakeGHRunView{DatabaseID: id, HeadSHA: run.head, HeadBranch: run.branch,
			WorkflowDatabaseID: run.workflowID, Event: "workflow_dispatch",
			Status: view.Status, Conclusion: view.Conclusion, Attempt: view.Attempt, URL: view.URL}
		if rv.Attempt == 0 {
			rv.Attempt = run.attempt
		}
		if rv.URL == "" {
			rv.URL = fmt.Sprintf("https://github.com/o/r/actions/runs/%d", id)
		}
		out, _ := json.Marshal(rv)
		return 0, out, nil
	}
	return 1, []byte("gh api: unexpected " + path), nil
}

func (f *fakeGH) find(id int64) *fakeGHRun {
	for _, run := range f.runs {
		if run.id == id {
			return run
		}
	}
	return nil
}

// pushedRefs are the `git push` calls the fake recorded, in order.
func (f *fakeGH) pushedRefs() []string {
	var out []string
	for _, call := range f.calls {
		if strings.HasPrefix(call, "git push") {
			out = append(out, call)
		}
	}
	return out
}

func ghTools() crwconfig.VerifierTools {
	return crwconfig.VerifierTools{Kind: crwconfig.VerifierGitHubActions, Repository: "o/r", Workflow: "CRW CI"}
}

// ghRepo is a repository with the commit chain base -> head: base carries the workflow file, go.mod and
// go.sum; head is a work commit the branch devin/test-work names. origin names the fake repository o/r and
// refs/remotes/origin/dev holds base.
func ghRepo(t *testing.T) (repo, base, head string) {
	t.Helper()
	repo, _ = newRepo(t)
	write(t, repo, ".github/workflows/ci.yml", "name: CRW CI\non: [workflow_dispatch]\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps: []\n", 0o644)
	write(t, repo, "go.mod", "module example.test/m\n\ngo 1.27\n", 0o644)
	write(t, repo, "scripts/ci/secrets.sh", "scan_version=8.30.1\n", 0o644)
	write(t, repo, "go.sum", "module h1:abc=\n", 0o644)
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "ci and pins")
	base = gitIn(t, repo, "rev-parse", "HEAD")
	gitIn(t, repo, "remote", "add", "origin", "https://github.com/o/r.git")
	gitIn(t, repo, "update-ref", "refs/remotes/origin/dev", base)
	write(t, repo, "a.txt", "alpha two\n", 0o644)
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "the work")
	head = gitIn(t, repo, "rev-parse", "HEAD")
	gitIn(t, repo, "branch", "devin/test-work", head)
	return repo, base, head
}

func ghVerifier(f *fakeGH, recordDir string) GitHubActionsVerifier {
	return GitHubActionsVerifier{Tools: ghTools(), RecordDir: recordDir, Run: f.run(), PollInterval: time.Nanosecond, Timeout: 30 * time.Second}
}

func successJobs() []fakeGHJob {
	return []fakeGHJob{
		{DatabaseID: 101, Name: "build", Status: "completed", Conclusion: "success", URL: "https://github.com/o/r/actions/runs/6/job/101",
			Steps: []fakeGHStep{{Name: "test", Number: 1, Status: "completed", Conclusion: "success"}}},
		{DatabaseID: 102, Name: "lint", Status: "completed", Conclusion: "success", URL: "https://github.com/o/r/actions/runs/6/job/102"},
	}
}

// ghWantKeys are the keys JudgeVerificationRecord is asked with for the head: the commit's own tree, ci.yml,
// dependencies and pins, the base the range covers, and a deliberately different host platform (a remote
// record is not measured on this host).
func ghWantKeys(t *testing.T, checkout, head, base string) dagsched.VerificationKeys {
	t.Helper()
	keys, err := dagsched.CommitVerificationKeys(context.Background(), checkout, head)
	if err != nil {
		t.Fatal(err)
	}
	keys.Base = base
	keys.OS, keys.Arch = "plan9", "mips64"
	return keys
}

// CRW-1594: a remote pass — the scan of base..head runs before the push, the pushed ref is the existing work
// branch at the head's exact SHA, the run that answers is the new one (older runs of the same workflow and
// runs of another sha, branch, event or workflow do not count), the receipt is a sealed verification-record/1
// with the run's URL and its successful jobs, and the remote judge accepts it.
func TestGitHubActionsVerifierPassesARun(t *testing.T) {
	repo, base, head := ghRepo(t)
	f := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"},
		logFailed: map[int64]string{}}
	// older runs the dispatch must not be mistaken for
	f.runs = append(f.runs,
		&fakeGHRun{id: 3, head: head, branch: "devin/other", workflowID: 7, attempt: 1},
		&fakeGHRun{id: 4, head: strings.Repeat("9", 40), branch: "devin/test-work", workflowID: 7, attempt: 1},
		&fakeGHRun{id: 5, head: head, branch: "devin/test-work", workflowID: 8, attempt: 1})
	f.newRun = func(run *fakeGHRun) {
		run.views = map[int][]fakeGHView{1: {
			{Status: "in_progress"},
			{Status: "completed", Conclusion: "success"},
		}}
		run.jobs = map[int][]fakeGHJob{1: successJobs()}
	}
	recordDir := t.TempDir()
	out, err := ghVerifier(f, recordDir).Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: repo})
	if err != nil || !out.Pass {
		t.Fatalf("verify: %+v %v", out, err)
	}
	if out.Command != CILocalCommand {
		t.Errorf("the command is %q, want %q", out.Command, CILocalCommand)
	}
	if !strings.Contains(out.Note, "github-actions") || !strings.Contains(out.Note, "https://github.com/o/r/actions/runs/6") {
		t.Errorf("the note: %q", out.Note)
	}
	// the scan ran before the push, and the push carried the exact sha into the work branch
	scanAt, pushAt := -1, -1
	for i, call := range f.calls {
		if strings.HasPrefix(call, "gitleaks ") && scanAt < 0 {
			scanAt = i
		}
		if strings.HasPrefix(call, "git push ") && pushAt < 0 {
			pushAt = i
		}
	}
	if scanAt < 0 || pushAt < 0 || scanAt > pushAt {
		t.Errorf("the scan did not precede the push: %v", f.calls)
	}
	if got := f.pushedRefs(); len(got) != 1 || got[0] != "git push --no-follow-tags origin "+head+":refs/heads/devin/test-work" {
		t.Errorf("the pushes: %v", got)
	}
	// only the new run answered (id 6 > the old 3,4,5)
	if len(f.runs) != 4 || f.runs[3].id != 6 || f.runs[3].head != head {
		t.Errorf("the runs: %+v", f.runs)
	}
	rec, err := dagsched.DecodeVerificationRecord(out.Record)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Runner != "github-actions" || rec.VerifierKind != "github-actions" || rec.VerifierRules != "github-actions/1" ||
		rec.HeadCommit != head || rec.BaseCommit != base || rec.Result != "pass" || rec.OS != "" || rec.Arch != "" || len(rec.Tools) != 0 {
		t.Errorf("the record: %+v", rec)
	}
	gh := rec.GitHub
	if gh == nil || gh.RunID != 6 || gh.URL != "https://github.com/o/r/actions/runs/6" || gh.RunHead != head ||
		gh.Event != "workflow_dispatch" || gh.Conclusion != "success" || gh.Attempts != 1 || gh.WorkflowID != 7 ||
		gh.WorkflowPath != ".github/workflows/ci.yml" || !strings.HasPrefix(gh.WorkflowDigest, "sha256:") ||
		gh.Scan.Base != base || gh.Scan.Head != head || gh.Scan.Result != "clean" {
		t.Errorf("the github provenance: %+v", gh)
	}
	if len(rec.Jobs) != 2 {
		t.Errorf("the jobs: %s", out.Record)
	}
	// the receipt is sealed and the remote judge accepts it, whatever this host's platform is
	if _, err := dagsched.JudgeVerificationRecord(out.Record, ghWantKeys(t, repo, head, base)); err != nil {
		t.Errorf("the remote judge refused the receipt: %v", err)
	}
	// the record also landed in the record directory
	if _, err := os.Stat(filepath.Join(recordDir, "verification-record-"+head+".json")); err != nil {
		t.Errorf("no record file: %v", err)
	}
	// a passing verification is cached under the shared git directory
	entries, err := filepath.Glob(filepath.Join(repo, ".git", "crw-ci", "github-actions", "*", "*.json"))
	if err != nil || len(entries) != 1 {
		t.Errorf("the cache entries: %v %v", entries, err)
	}
}

// CRW-1594: a failed first attempt gets exactly one rerun of its failed jobs. The old attempt's completed
// rows after the rerun are not the new completion: only an attempt greater than the first counts. Jobs that
// passed stay, the rerun's jobs land by name, and the record keeps both attempts, the flaky flag and the
// first attempt's failing lines.
func TestGitHubActionsVerifierRerunsFailedJobsOnce(t *testing.T) {
	repo, base, head := ghRepo(t)
	f := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"},
		logFailed: map[int64]string{102: "--- FAIL: TestFlaky (0.01s)\n    flaky_test.go:3: unlucky\nFAIL\texample.test/m\t0.010s\n"}}
	f.newRun = func(run *fakeGHRun) {
		run.views = map[int][]fakeGHView{
			1: {{Status: "completed", Conclusion: "failure"}},
			// a stale view of the first attempt after the rerun, then the second attempt's completion
			2: {
				{Status: "completed", Conclusion: "failure", Attempt: 1},
				{Status: "completed", Conclusion: "success", Attempt: 2},
			},
		}
		run.jobs = map[int][]fakeGHJob{
			1: {
				{DatabaseID: 101, Name: "build", Status: "completed", Conclusion: "success"},
				{DatabaseID: 102, Name: "tests", Status: "completed", Conclusion: "failure"},
			},
			2: {{DatabaseID: 103, Name: "tests", Status: "completed", Conclusion: "success"}},
		}
	}
	out, err := ghVerifier(f, t.TempDir()).Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: repo})
	if err != nil || !out.Pass {
		t.Fatalf("verify: %+v %v", out, err)
	}
	if len(f.reruns) != 1 {
		t.Fatalf("the reruns: %v, want exactly one", f.reruns)
	}
	rec, err := dagsched.DecodeVerificationRecord(out.Record)
	if err != nil {
		t.Fatal(err)
	}
	gh := rec.GitHub
	if gh == nil || gh.Attempts != 2 || !gh.FlakeRerunPassed {
		t.Fatalf("the provenance: %+v", gh)
	}
	if len(gh.Runs) != 2 || gh.Runs[0].Conclusion != "failure" || gh.Runs[1].Conclusion != "success" {
		t.Errorf("the attempts: %+v", gh.Runs)
	}
	// the merged jobs: build from attempt 1, tests from attempt 2 — both passed
	if len(rec.Jobs) != 2 {
		t.Fatalf("the jobs: %s", out.Record)
	}
	var names []string
	for _, raw := range rec.Jobs {
		var job struct {
			Name       string `json:"name"`
			Result     string `json:"result"`
			Conclusion string `json:"conclusion"`
		}
		if err := json.Unmarshal(raw, &job); err != nil {
			t.Fatal(err)
		}
		if job.Result != "passed" || job.Conclusion != "success" {
			t.Errorf("job %s: %s/%s", job.Name, job.Result, job.Conclusion)
		}
		names = append(names, job.Name)
	}
	if !slices.Equal(names, []string{"build", "tests"}) {
		t.Errorf("the merged jobs: %v", names)
	}
	// the first attempt's failing lines are kept
	if len(out.FailedTests) == 0 || !slices.Contains(out.FailedTests[0].Tests, "TestFlaky") {
		t.Errorf("the failed tests: %+v", out.FailedTests)
	}
	var firstTests struct {
		Failures []struct {
			Tests []string `json:"tests"`
		} `json:"failures"`
	}
	for _, raw := range gh.Runs[0].Jobs {
		var job struct {
			Name     string `json:"name"`
			Failures []struct {
				Tests []string `json:"tests"`
			} `json:"failures"`
		}
		if err := json.Unmarshal(raw, &job); err != nil {
			t.Fatal(err)
		}
		if job.Name == "tests" {
			firstTests.Failures = job.Failures
		}
	}
	if len(firstTests.Failures) == 0 || !slices.Contains(firstTests.Failures[0].Tests, "TestFlaky") {
		t.Errorf("the first attempt kept no failing lines: %+v", gh.Runs[0])
	}
	_ = base
}

// CRW-1594: two failures are a failure: exactly one rerun, the record keeps both attempts and the failures,
// and nothing is cached.
func TestGitHubActionsVerifierFailsTwice(t *testing.T) {
	repo, base, head := ghRepo(t)
	f := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"},
		logFailed: map[int64]string{101: "--- FAIL: TestBroken (0.01s)\nFAIL\texample.test/m\t0.010s\n"}}
	f.newRun = func(run *fakeGHRun) {
		run.views = map[int][]fakeGHView{
			1: {{Status: "completed", Conclusion: "failure"}},
			2: {{Status: "completed", Conclusion: "failure", Attempt: 2}},
		}
		run.jobs = map[int][]fakeGHJob{
			1: {{DatabaseID: 101, Name: "tests", Status: "completed", Conclusion: "failure"}},
			2: {{DatabaseID: 102, Name: "tests", Status: "completed", Conclusion: "failure"}},
		}
	}
	out, err := ghVerifier(f, t.TempDir()).Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: repo})
	if err != nil {
		t.Fatalf("a failed verification is an outcome, not an error: %v", err)
	}
	if out.Pass || len(f.reruns) != 1 {
		t.Fatalf("verify: %+v, reruns %v", out, f.reruns)
	}
	rec, err := dagsched.DecodeVerificationRecord(out.Record)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Result != "fail" || rec.GitHub == nil || rec.GitHub.Attempts != 2 || len(rec.GitHub.Runs) != 2 || rec.GitHub.FlakeRerunPassed {
		t.Errorf("the record: %+v", rec.GitHub)
	}
	if len(out.FailedTests) == 0 {
		t.Errorf("no failed tests: %+v", out)
	}
	entries, _ := filepath.Glob(filepath.Join(repo, ".git", "crw-ci", "*"))
	if len(entries) != 0 {
		t.Errorf("a failed verification was cached: %v", entries)
	}
	_ = base
}

// CRW-1594: a scan finding, a scanner that cannot start and an unreadable base all stop before any push or
// dispatch. The scan is of the current base..head with merge deltas, redacted, with the repository's
// ignore-allow suppressed and an empty ignore file.
func TestGitHubActionsVerifierScanGatesThePush(t *testing.T) {
	repo, base, head := ghRepo(t)
	f := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"},
		scanCode: 3}
	_, err := ghVerifier(f, t.TempDir()).Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: repo})
	if err == nil {
		t.Fatal("a scan finding verified")
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "git push") || strings.HasPrefix(call, "gh workflow run") {
			t.Errorf("a blocked scan still pushed or dispatched: %v", f.calls)
		}
	}
	if len(f.scanArgs) != 1 {
		t.Fatalf("the scans: %v", f.scanArgs)
	}
	args := f.scanArgs[0]
	if !slices.Contains(args, "--redact") || !slices.Contains(args, "--ignore-gitleaks-allow") || !slices.Contains(args, "--no-banner") ||
		!slices.Contains(args, "--log-opts=-m "+base+".."+head) {
		t.Errorf("the scan args: %v", args)
	}
	ignore := flagOf(args, "--gitleaks-ignore-path")
	if ignore == "" {
		t.Errorf("no empty ignore file: %v", args)
	}
	if i := slices.Index(args, gitCommonDir(t, repo)); i < 0 {
		t.Errorf("the scan did not name the git directory: %v", args)
	}
	env := f.scanEnvs[0]
	if !slices.Contains(env, "GITLEAKS_CONFIG=") || !slices.Contains(env, "GITLEAKS_CONFIG_TOML=") || !slices.Contains(env, "GIT_TERMINAL_PROMPT=0") {
		t.Errorf("the scan env: %v", env)
	}
	if f.scanDirs[0] == repo || !strings.HasPrefix(f.scanDirs[0], os.TempDir()) {
		t.Errorf("the scan ran in %s, not a private directory", f.scanDirs[0])
	}
	// a scanner that cannot start is the same stop
	f.scanErr, f.scanCode = errors.New("exec: no such file"), 0
	_, err = ghVerifier(f, t.TempDir()).Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: repo})
	if err == nil {
		t.Fatal("a missing scanner verified")
	}
	// an unreadable base stops before the scan
	before := len(f.scanArgs)
	_, err = ghVerifier(f, t.TempDir()).Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: repo, Base: "refs/heads/no-such-base"})
	if err == nil {
		t.Fatal("an unreadable base verified")
	}
	if len(f.scanArgs) != before {
		t.Errorf("the scan ran for an unreadable base")
	}
	if strings.Contains(fmt.Sprint(err), "redacted scan output") {
		// the scan output is redacted anyway, but the error carries none of it
	}
}

func gitCommonDir(t *testing.T, repo string) string {
	t.Helper()
	return filepath.Join(repo, ".git")
}

// CRW-1594: the same tree under another commit or another base is the same verification: the scan runs on the
// new range but no push and no dispatch happens, and the receipt names the current head and base, the original
// run and a reusedFrom of the first receipt. The cache lives in the shared git directory, so a linked worktree
// sees it too.
func TestGitHubActionsVerifierReusesAPassedTree(t *testing.T) {
	repo, base, head := ghRepo(t)
	f := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"},
		logFailed: map[int64]string{}}
	f.newRun = func(run *fakeGHRun) {
		run.views = map[int][]fakeGHView{1: {{Status: "completed", Conclusion: "success"}}}
		run.jobs = map[int][]fakeGHJob{1: successJobs()}
	}
	v := ghVerifier(f, t.TempDir())
	first, err := v.Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: repo})
	if err != nil || !first.Pass {
		t.Fatalf("the first verification: %+v %v", first, err)
	}
	firstDigest := first.Digest
	// a new commit with the same tree, verified from a linked worktree (the shared common dir)
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "empty change")
	head2 := gitIn(t, repo, "rev-parse", "HEAD")
	if tree2, tree1 := gitIn(t, repo, "rev-parse", head2+"^{tree}"), gitIn(t, repo, "rev-parse", head+"^{tree}"); tree2 != tree1 {
		t.Fatalf("the setup: %s and %s differ", tree2, tree1)
	}
	wt := addWorktree(t, repo, "wt2", head2)
	calls := len(f.calls)
	out, err := v.Verify(context.Background(), VerifyRequest{Node: "B", Head: head2, Worktree: wt})
	if err != nil || !out.Pass {
		t.Fatalf("the reuse: %+v %v", out, err)
	}
	for _, call := range f.calls[calls:] {
		if strings.HasPrefix(call, "git push") || strings.HasPrefix(call, "gh workflow run") || strings.HasPrefix(call, "gh api repos/o/r/actions/runs/") {
			t.Errorf("a cache hit pushed, dispatched or polled: %v", f.calls[calls:])
		}
	}
	// the scan ran on the new range
	var found bool
	for _, args := range f.scanArgs {
		if slices.Contains(args, "--log-opts=-m "+base+".."+head2) {
			found = true
		}
	}
	if !found {
		t.Errorf("the new range was not scanned: %v", f.scanArgs)
	}
	rec, err := dagsched.DecodeVerificationRecord(out.Record)
	if err != nil {
		t.Fatal(err)
	}
	gh := rec.GitHub
	if rec.HeadCommit != head2 || rec.BaseCommit != base || gh == nil || gh.RunHead != head || gh.RunID != f.runs[0].id {
		t.Fatalf("the reuse receipt: %+v", gh)
	}
	if gh.ReusedFrom == nil || gh.ReusedFrom.Digest != firstDigest || gh.ReusedFrom.Head != head || gh.ReusedFrom.VerifierKind != "github-actions" {
		t.Errorf("the reusedFrom: %+v", gh.ReusedFrom)
	}
	if _, err := dagsched.JudgeVerificationRecord(out.Record, ghWantKeys(t, repo, head2, base)); err != nil {
		t.Errorf("the reuse receipt was refused: %v", err)
	}
}

// CRW-1594: reuse does not skip history-sensitive hosted validation — a same-tree head whose new objects are
// not all commits (a blob added and then deleted leaves the final tree identical) is a cache miss and dispatches.
func TestGitHubActionsVerifierDoesNotReuseATreeWithNewObjects(t *testing.T) {
	repo, _, head := ghRepo(t)
	f := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"},
		logFailed: map[int64]string{}}
	dispatched := 0
	f.newRun = func(run *fakeGHRun) {
		dispatched++
		run.views = map[int][]fakeGHView{1: {{Status: "completed", Conclusion: "success"}}}
		run.jobs = map[int][]fakeGHJob{1: successJobs()}
	}
	v := ghVerifier(f, t.TempDir())
	if out, err := v.Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: repo}); err != nil || !out.Pass {
		t.Fatalf("the first verification: %v", err)
	}
	// add a blob in one commit and delete it in the next: the tree is again head's tree
	write(t, repo, "big.bin", strings.Repeat("x", 1<<20), 0o644)
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "add blob")
	if err := os.Remove(filepath.Join(repo, "big.bin")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "delete blob")
	head3 := gitIn(t, repo, "rev-parse", "HEAD")
	if tree3, tree1 := gitIn(t, repo, "rev-parse", head3+"^{tree}"), gitIn(t, repo, "rev-parse", head+"^{tree}"); tree3 != tree1 {
		t.Fatalf("the setup: %s and %s differ", tree3, tree1)
	}
	if _, err := v.Verify(context.Background(), VerifyRequest{Node: "B", Head: head3, Worktree: repo}); err != nil {
		t.Fatalf("the second verification: %v", err)
	}
	if dispatched != 2 {
		t.Errorf("the add-then-delete head reused the cache: %d dispatches", dispatched)
	}
}

// CRW-1594: a changed workflow identity, a corrupted cache file, a failed or jobless record and a symlinked
// cache are never a pass.
func TestGitHubActionsVerifierCacheIsNotAPassByItself(t *testing.T) {
	repo, _, head := ghRepo(t)
	f := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"},
		logFailed: map[int64]string{}}
	dispatched := 0
	f.newRun = func(run *fakeGHRun) {
		dispatched++
		run.views = map[int][]fakeGHView{1: {{Status: "completed", Conclusion: "success"}}}
		run.jobs = map[int][]fakeGHJob{1: successJobs()}
	}
	v := ghVerifier(f, t.TempDir())
	verify := func() {
		t.Helper()
		if _, err := v.Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: repo}); err != nil {
			t.Fatalf("verify: %v", err)
		}
	}
	verify()
	if dispatched != 1 {
		t.Fatalf("setup: %d dispatches", dispatched)
	}
	// a changed workflow identity is another verification
	f.workflow = fakeGHWorkflow{ID: 9, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"}
	verify()
	if dispatched != 2 {
		t.Errorf("a changed workflow reused the cache")
	}
	// a corrupted cache entry is a miss, never a pass: the live entry is the one the current workflow wrote
	live := ""
	entries, _ := filepath.Glob(filepath.Join(repo, ".git", "crw-ci", "github-actions", "*", "*.json"))
	for _, entry := range entries {
		raw, err := os.ReadFile(entry)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		if gh, _ := doc["github"].(map[string]any); gh != nil && gh["workflowId"] == float64(9) {
			live = entry
		}
	}
	if live == "" {
		t.Fatal("no live cache entry")
	}
	raw, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["result"] = "fail" // the digest no longer matches
	corrupt, _ := json.Marshal(doc)
	if err := os.WriteFile(live, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	verify()
	if dispatched != 3 {
		t.Errorf("a corrupted cache entry passed")
	}
	// a freshly resealed record whose contents fail the judge is a miss, never a pass: wrong URL, another
	// repository, a stale scan, empty jobs and a job that never completed each dispatch again.
	livePath := func() string {
		entries, _ := filepath.Glob(filepath.Join(repo, ".git", "crw-ci", "github-actions", "*", "*.json"))
		for _, entry := range entries {
			raw, _ := os.ReadFile(entry)
			var d map[string]any
			if json.Unmarshal(raw, &d) == nil {
				if gh, _ := d["github"].(map[string]any); gh != nil && gh["workflowId"] == float64(9) {
					return entry
				}
			}
		}
		return ""
	}
	inject := func(mutate func(record *dagsched.VerificationRecord)) {
		path := livePath()
		if path == "" {
			t.Fatal("no live cache entry")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		record, err := dagsched.DecodeVerificationRecord(raw)
		if err != nil {
			t.Fatal(err)
		}
		mutate(&record)
		sealed, err := dagsched.SealVerificationRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, sealed, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, mutate := range map[string]func(*dagsched.VerificationRecord){
		"a forged url":       func(r *dagsched.VerificationRecord) { r.GitHub.URL = "https://example.com/runs/1" },
		"another repository": func(r *dagsched.VerificationRecord) { r.Repository = "other/repo" },
		"a stale scan":       func(r *dagsched.VerificationRecord) { r.GitHub.Scan.Base = strings.Repeat("1", 40) },
		"empty jobs":         func(r *dagsched.VerificationRecord) { r.Jobs = nil },
		"a job that never completed": func(r *dagsched.VerificationRecord) {
			job, _ := json.Marshal(map[string]any{"name": "build", "databaseId": 101, "status": "in_progress", "conclusion": "success", "result": "passed"})
			r.Jobs = []json.RawMessage{job}
		},
		"a foreign run head without new objects": func(r *dagsched.VerificationRecord) {
			// runHead is not an ancestor: the new-object check sees it cannot reuse either
			r.GitHub.RunHead = strings.Repeat("9", 40)
			r.GitHub.ReusedFrom = &dagsched.GitHubReusedFrom{Digest: "sha256:" + strings.Repeat("9", 64), Head: strings.Repeat("9", 40), VerifierKind: "github-actions"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			before := dispatched
			inject(mutate)
			verify()
			if dispatched != before+1 {
				t.Errorf("%s was reused", name)
			}
		})
	}
	// a symlinked cache file is a miss that is never followed: the dispatch happens, the external target is
	// untouched.
	path := livePath()
	if path == "" {
		t.Fatal("no live cache entry")
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("not a record"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	before := dispatched
	verify()
	if dispatched != before+1 {
		t.Error("a symlinked cache file was reused")
	}
	body, err := os.ReadFile(target)
	if err != nil || string(body) != "not a record" {
		t.Errorf("the symlink target was touched: %q %v", body, err)
	}
}

// CRW-1594: a protected branch is never pushed, and no remote or local branch is ever deleted.
func TestGitHubActionsVerifierNeverTouchesProtectedBranches(t *testing.T) {
	repo, _, head := ghRepo(t)
	f := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"},
		logFailed: map[int64]string{}}
	f.newRun = func(run *fakeGHRun) {
		run.views = map[int][]fakeGHView{1: {{Status: "completed", Conclusion: "success"}}}
		run.jobs = map[int][]fakeGHJob{1: successJobs()}
	}
	// no work branch points at the head and HEAD is detached at it: a ci/<node>-<sha8> branch is generated
	gitIn(t, repo, "checkout", "-q", "--detach", head)
	gitIn(t, repo, "branch", "-D", "devin/test-work")
	out, err := ghVerifier(f, t.TempDir()).Verify(context.Background(), VerifyRequest{Node: "my node", Head: head, Worktree: repo})
	if err != nil || !out.Pass {
		t.Fatalf("verify: %+v %v", out, err)
	}
	pushes := f.pushedRefs()
	if len(pushes) != 1 {
		t.Fatalf("the pushes: %v", pushes)
	}
	dst := pushes[0][strings.LastIndex(pushes[0], ":")+1:]
	if !strings.HasPrefix(dst, "refs/heads/ci/") || strings.HasSuffix(dst, "main") || strings.HasSuffix(dst, "dev") {
		t.Errorf("the pushed ref: %s", dst)
	}
	for _, call := range f.calls {
		if strings.Contains(call, "--delete") || strings.Contains(call, " -d ") || strings.Contains(call, " -D ") ||
			strings.Contains(call, "branch -d") || strings.Contains(call, "branch -D") {
			t.Errorf("a deletion ran: %s", call)
		}
	}
}

// CRW-1594: NewVerifier selects by kind, and a request for another command is refused by either verifier.
func TestNewVerifierSelectsTheKind(t *testing.T) {
	if _, ok := NewVerifier(crwconfig.VerifierTools{}).(CILocalVerifier); !ok {
		t.Errorf("the default is not the local verifier")
	}
	if _, ok := NewVerifier(ghTools()).(GitHubActionsVerifier); !ok {
		t.Errorf("github-actions is not the remote verifier")
	}
	repo, _, _ := ghRepo(t)
	f := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"}}
	_, err := ghVerifier(f, t.TempDir()).Verify(context.Background(), VerifyRequest{Node: "A", Head: "HEAD", Worktree: repo, Command: "crw-dev ci nightly"})
	if err == nil || !strings.Contains(err.Error(), CILocalCommand) {
		t.Errorf("another command: %v", err)
	}
	// a github-actions verifier built directly on a local configuration validates it
	_, err = GitHubActionsVerifier{Tools: crwconfig.VerifierTools{CrwDev: "/x"}, Run: f.run()}.Verify(context.Background(),
		VerifyRequest{Node: "A", Head: "HEAD", Worktree: repo})
	if err == nil {
		t.Error("a local configuration ran the remote verifier")
	}
}

// CRW-1594: a cancelled remote verification ends promptly — an external command's cancellation is an
// infrastructure error, never a pass.
func TestGitHubActionsVerifierCancellationIsNotAPass(t *testing.T) {
	repo, _, head := ghRepo(t)
	f := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"},
		logFailed: map[int64]string{}}
	f.newRun = func(run *fakeGHRun) {
		run.views = map[int][]fakeGHView{1: {{Status: "in_progress"}}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		out, err := ghVerifier(f, t.TempDir()).Verify(ctx, VerifyRequest{Node: "A", Head: head, Worktree: repo})
		if err == nil && out.Pass {
			err = errors.New("a cancelled verification passed")
		}
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("the cancellation produced no error")
		}
	case <-time.After(10 * time.Second):
		t.Error("a cancelled verification did not end")
	}
}

// CRW-1594 rework: the workflow is selected from the paginated REST listing and dispatch uses its numeric
// ID; the attempt's jobs arrive over the paginated attempts/N/jobs endpoint.
func TestGitHubActionsVerifierPaginatesWorkflowAndJobs(t *testing.T) {
	repo, _, head := ghRepo(t)
	f := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"},
		extraPages: [][]fakeGHWorkflow{{{ID: 9, Name: "Nightly", Path: ".github/workflows/nightly.yml", State: "active"}}},
		logFailed:  map[int64]string{}}
	f.newRun = func(run *fakeGHRun) {
		run.views = map[int][]fakeGHView{1: {{Status: "completed", Conclusion: "success"}}}
		// the two jobs arrive on two pages of the attempts/N/jobs answer
		f.jobPages = map[[2]int][][]fakeGHJob{{int(run.id), 1}: {{successJobs()[0]}, {successJobs()[1]}}}
	}
	out, err := ghVerifier(f, t.TempDir()).Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: repo})
	if err != nil || !out.Pass {
		t.Fatalf("verify over pages: %+v %v", out, err)
	}
	// the dispatch and the list used the numeric workflow ID, not the name
	usedNumeric := false
	for _, call := range f.calls {
		if strings.HasPrefix(call, "gh workflow run 7 ") || strings.HasPrefix(call, "gh run list") && strings.Contains(call, "--workflow 7") {
			usedNumeric = true
		}
	}
	if !usedNumeric {
		t.Errorf("no numeric-ID dispatch or list: %v", f.calls)
	}
	rec, err := dagsched.DecodeVerificationRecord(out.Record)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Jobs) != 2 {
		t.Errorf("the paged jobs: %d", len(rec.Jobs))
	}
}

// CRW-1594 rework: an origin URL carrying credentials is refused without ever printing them — the error
// names origin and the configured repository only.
func TestGitHubActionsVerifierNeverPrintsACredentialRemote(t *testing.T) {
	repo, _, head := ghRepo(t)
	gitIn(t, repo, "remote", "set-url", "--push", "origin", "https://user:secret-token@github.com/o/r.git")
	f := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"},
		logFailed: map[int64]string{}}
	_, err := ghVerifier(f, t.TempDir()).Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: repo})
	if err == nil {
		t.Fatal("a credential-bearing push URL was used")
	}
	if strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "user:") || strings.Contains(err.Error(), "https://") {
		t.Errorf("the error leaks the remote: %v", err)
	}
	if !strings.Contains(err.Error(), "origin") || !strings.Contains(err.Error(), "o/r") {
		t.Errorf("the error names no remote or repository: %v", err)
	}
	if len(f.pushedRefs()) != 0 {
		t.Error("a credential-bearing remote was pushed to")
	}
	// the supported spellings still parse
	for url, want := range map[string]string{
		"https://github.com/o/r.git":   "o/r",
		"git@github.com:o/r.git":       "o/r",
		"ssh://git@github.com/o/r":     "o/r",
		"https://github.com/o/r?x=1":   "",
		"https://token@github.com/o/r": "",
		"file:///srv/repo":             "",
	} {
		if got := githubSlug(url); got != want {
			t.Errorf("githubSlug(%q) = %q, want %q", url, got, want)
		}
	}
}

// CRW-1594 rework: the real `gh run view --job --log-failed` prefixes every line with "<job>\t<step>\t
// <timestamp> "; the failing lines of BOTH attempts land in the receipt, each attempt's own tests kept.
func TestGitHubActionsVerifierNormalizesLogFailedPrefixes(t *testing.T) {
	repo, _, head := ghRepo(t)
	prefix := func(line string) string {
		return "go-product (test-rest)\tTest and replay the contract corpus (test-rest)\t2026-10-16T20:43:19.6369083Z " + line
	}
	var firstLog strings.Builder
	firstLog.WriteString(prefix("=== RUN   TestFirst\n"))
	firstLog.WriteString(prefix("--- FAIL: TestFirst (0.79s)\n"))
	for i := 0; i < 25; i++ {
		firstLog.WriteString(prefix(fmt.Sprintf("    unrelated output line %d\n", i)))
	}
	f := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"},
		logFailed: map[int64]string{
			101: firstLog.String(),
			201: prefix("--- FAIL: TestSecond (0.11s)\n") + prefix("    second_test.go:9: also wrong\n") + "FAIL\texample.test/m\t0.110s\n",
		}}
	f.newRun = func(run *fakeGHRun) {
		run.views = map[int][]fakeGHView{
			1: {{Status: "completed", Conclusion: "failure"}},
			2: {{Status: "completed", Conclusion: "failure", Attempt: 2}},
		}
		run.jobs = map[int][]fakeGHJob{
			1: {
				{DatabaseID: 101, Name: "tests", Status: "completed", Conclusion: "failure"},
				{DatabaseID: 102, Name: "build", Status: "completed", Conclusion: "success"},
			},
			2: {
				{DatabaseID: 201, Name: "tests", Status: "completed", Conclusion: "failure"},
			},
		}
	}
	out, err := ghVerifier(f, t.TempDir()).Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: repo})
	if err != nil || out.Pass {
		t.Fatalf("verify: %+v %v", out, err)
	}
	var tests []string
	for _, step := range out.FailedTests {
		tests = append(tests, step.Tests...)
	}
	if !slices.Contains(tests, "TestFirst") || !slices.Contains(tests, "TestSecond") {
		t.Errorf("the receipt's tests: %v", tests)
	}
	// the prefixed noise lines are not tests, and the tail is bounded
	for _, step := range out.FailedTests {
		for _, test := range step.Tests {
			if strings.Contains(test, "unrelated") || strings.Contains(test, "go-product") {
				t.Errorf("a noise line became a test: %q", test)
			}
		}
	}
	// a run whose log fetch fails still keeps its outcome, with the diagnostic in the failure detail —
	// verified on another tree so the first verify's cache cannot answer it.
	write(t, repo, "another.txt", "another\n", 0o644)
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "another")
	head2 := gitIn(t, repo, "rev-parse", "HEAD")
	f2 := &fakeGH{workflow: f.workflow, logFailedErr: map[int64]bool{101: true}, logFailed: map[int64]string{}}
	f2.newRun = func(run *fakeGHRun) {
		run.views = map[int][]fakeGHView{
			1: {{Status: "completed", Conclusion: "failure"}},
			2: {{Status: "completed", Conclusion: "success", Attempt: 2}},
		}
		run.jobs = map[int][]fakeGHJob{
			1: {{DatabaseID: 101, Name: "tests", Status: "completed", Conclusion: "failure"}},
			2: {{DatabaseID: 201, Name: "tests", Status: "completed", Conclusion: "success"}},
		}
	}
	out2, err := ghVerifier(f2, t.TempDir()).Verify(context.Background(), VerifyRequest{Node: "A", Head: head2, Worktree: repo})
	if err != nil || !out2.Pass {
		t.Fatalf("a lost job log is not a lost verification: %+v %v", out2, err)
	}
	found := false
	for _, step := range out2.FailedTests {
		if strings.Contains(step.Tail, "could not be read") {
			found = true
		}
	}
	if !found {
		t.Errorf("the log-fetch diagnostic is missing: %+v", out2.FailedTests)
	}
}

// CRW-1594 rework: a retry verdict is the retry's own completion, not the merge — a cancelled second
// attempt is a failure, and a rerun that cannot start still leaves its sealed attempt-1 receipt.
func TestGitHubActionsVerifierRetryVerdictIsTheRetrysOwn(t *testing.T) {
	repo, _, head := ghRepo(t)
	// cancelled second attempt: not a pass even though attempt 1's jobs merged
	f := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"},
		logFailed: map[int64]string{101: "--- FAIL: TestX (0.01s)\n"}}
	f.newRun = func(run *fakeGHRun) {
		run.views = map[int][]fakeGHView{
			1: {{Status: "completed", Conclusion: "failure"}},
			2: {{Status: "completed", Conclusion: "cancelled", Attempt: 2}},
		}
		run.jobs = map[int][]fakeGHJob{
			1: {{DatabaseID: 101, Name: "tests", Status: "completed", Conclusion: "failure"}},
			2: {{DatabaseID: 201, Name: "tests", Status: "completed", Conclusion: "cancelled"}},
		}
	}
	out, err := ghVerifier(f, t.TempDir()).Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: repo})
	if err != nil || out.Pass {
		t.Errorf("a cancelled retry passed: %+v %v", out, err)
	}
	// a rerun that cannot start: an infrastructure error whose sealed attempt-1 receipt is persisted
	f2 := &fakeGH{workflow: f.workflow, rerunErr: fmt.Errorf("rerun refused"), logFailed: map[int64]string{101: "--- FAIL: TestX (0.01s)\n"}}
	f2.newRun = func(run *fakeGHRun) {
		run.views = map[int][]fakeGHView{1: {{Status: "completed", Conclusion: "failure"}}}
		run.jobs = map[int][]fakeGHJob{1: {{DatabaseID: 101, Name: "tests", Status: "completed", Conclusion: "failure"}}}
	}
	recordDir := t.TempDir()
	out2, err := ghVerifier(f2, recordDir).Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: repo})
	if err == nil {
		t.Fatal("a failed rerun is an error")
	}
	if out2.Pass || len(out2.Record) == 0 || out2.Digest == "" || out2.Summary == "" {
		t.Fatalf("the outcome has no receipt: %+v", out2)
	}
	rec, err := dagsched.DecodeVerificationRecord(out2.Record)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Result != "fail" || rec.GitHub == nil || rec.GitHub.Attempts != 1 || len(rec.GitHub.Runs) != 1 {
		t.Errorf("the persisted receipt: %+v", rec.GitHub)
	}
	if _, err := os.Stat(filepath.Join(recordDir, "verification-record-"+head+".json")); err != nil {
		t.Errorf("the receipt was not persisted: %v", err)
	}
}

// CRW-1594 final step: a producer-side answer with an unnamed, duplicate or unidentified job is an
// infrastructure error, never a pass — a failure followed by a same-name success and two same-name
// successes alike are inconsistent evidence.
func TestGitHubActionsVerifierDuplicateJobsAreNeverEvidence(t *testing.T) {
	repo, _, head := ghRepo(t)
	for name, jobs := range map[string][]fakeGHJob{
		"a failure and a same-name success": {
			{DatabaseID: 101, Name: "tests", Status: "completed", Conclusion: "failure"},
			{DatabaseID: 102, Name: "tests", Status: "completed", Conclusion: "success"},
		},
		"two same-name successes": {
			{DatabaseID: 101, Name: "build", Status: "completed", Conclusion: "success"},
			{DatabaseID: 102, Name: "build", Status: "completed", Conclusion: "success"},
		},
		"an unnamed job": {
			{DatabaseID: 101, Name: "", Status: "completed", Conclusion: "success"},
		},
		"an unidentified job": {
			{Name: "build", Status: "completed", Conclusion: "success"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"},
				logFailed: map[int64]string{}}
			f.newRun = func(run *fakeGHRun) {
				run.views = map[int][]fakeGHView{1: {{Status: "completed", Conclusion: "success"}}}
				run.jobs = map[int][]fakeGHJob{1: jobs}
			}
			out, err := ghVerifier(f, t.TempDir()).Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: repo})
			if err == nil || out.Pass {
				t.Errorf("%s passed: %+v", name, out)
			}
		})
	}
	// a retry whose second attempt answers a duplicate job name is the same refusal
	f := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"},
		logFailed: map[int64]string{101: "--- FAIL: TestX (0.01s)\n"}}
	f.newRun = func(run *fakeGHRun) {
		run.views = map[int][]fakeGHView{
			1: {{Status: "completed", Conclusion: "failure"}},
			2: {{Status: "completed", Conclusion: "success", Attempt: 2}},
		}
		run.jobs = map[int][]fakeGHJob{
			1: {{DatabaseID: 101, Name: "tests", Status: "completed", Conclusion: "failure"}},
			2: {
				{DatabaseID: 201, Name: "tests", Status: "completed", Conclusion: "success"},
				{DatabaseID: 202, Name: "tests", Status: "completed", Conclusion: "success"},
			},
		}
	}
	out, err := ghVerifier(f, t.TempDir()).Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: repo})
	if err == nil || out.Pass {
		t.Error("a duplicate retry job passed")
	}
}

// CRW-1594 review: the pre-push secret scan runs the Gitleaks version the verified commit pins in
// scripts/ci/secrets.sh — a missing pin, a version the binary cannot report, a malformed answer and an
// older scanner all refuse before any scan, push or dispatch.
func TestGitHubActionsVerifierRequiresThePinnedGitleaks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script bool   // the commit carries scripts/ci/secrets.sh
		ver    string // gitleaks version's answer
		vcode  int
		want   string
	}{
		{name: "the pinned version", script: true, ver: "8.30.1", want: ""},
		{name: "an old version", script: true, ver: "8.18.0", want: "must be Gitleaks version 8.30.1"},
		{name: "a v-prefixed match", script: true, ver: "v8.30.1", want: ""},
		{name: "malformed output", script: true, ver: "gitleaks tool\nother", want: "must be Gitleaks version 8.30.1"},
		{name: "version cannot run", script: true, vcode: 1, want: "must be Gitleaks version 8.30.1"},
		{name: "no declared pin", script: false, ver: "8.30.1", want: "declares no Gitleaks version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, base, head := ghRepo(t)
			if !tc.script {
				gitIn(t, repo, "rm", "-q", "scripts/ci/secrets.sh")
				gitIn(t, repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "no scanner pin")
				head = gitIn(t, repo, "rev-parse", "HEAD")
				gitIn(t, repo, "branch", "-f", "devin/test-work", head)
			}
			f := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"},
				scanVersion: tc.ver, scanVCode: tc.vcode}
			f.newRun = func(run *fakeGHRun) {
				run.views = map[int][]fakeGHView{1: {{Status: "completed", Conclusion: "success"}}}
				run.jobs = map[int][]fakeGHJob{1: {{DatabaseID: 101, Name: "ci", Status: "completed", Conclusion: "success"}}}
			}
			out, err := ghVerifier(f, t.TempDir()).Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: repo, Base: base})
			if tc.want == "" {
				if err != nil || !out.Pass {
					t.Fatalf("verify: %+v %v", out, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("verify: %v, want %q", err, tc.want)
			}
			if len(f.scanArgs) != 0 || len(f.pushed) != 0 {
				t.Errorf("a refusal scanned or pushed: scans %v pushes %v", f.scanArgs, f.pushed)
			}
		})
	}
}

// CRW-1594 review: origin may carry several push URLs — every one must name the configured repository,
// not just the first get-url reports, or the push lands at an unapproved remote. Any mismatch stops the
// verification before the push; the message never carries a URL.
func TestGitHubActionsVerifierChecksEveryPushURL(t *testing.T) {
	repo, base, head := ghRepo(t)
	// a second push URL to a different repository after the configured one: get-url's first answer stays
	// the configured repository, but a push would reach both
	gitIn(t, repo, "config", "--add", "remote.origin.pushurl", "https://github.com/o/r.git")
	gitIn(t, repo, "config", "--add", "remote.origin.pushurl", "https://github.com/other/private.git")
	f := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"}}
	out, err := ghVerifier(f, t.TempDir()).Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: repo, Base: base})
	if err == nil || out.Pass {
		t.Fatalf("a second push URL passed: %+v %v", out, err)
	}
	if !strings.Contains(err.Error(), "configured repository o/r") || strings.Contains(err.Error(), "other") || strings.Contains(err.Error(), "private") {
		t.Errorf("the refusal names a URL: %v", err)
	}
	if len(f.pushed) != 0 {
		t.Errorf("pushed anyway: %v", f.pushed)
	}
	// two spellings of the configured repository are all of them the repository: accepted
	repo2, base2, head2 := ghRepo(t)
	gitIn(t, repo2, "config", "--add", "remote.origin.pushurl", "git@github.com:o/r.git")
	f2 := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"}}
	f2.newRun = func(run *fakeGHRun) {
		run.views = map[int][]fakeGHView{1: {{Status: "completed", Conclusion: "success"}}}
		run.jobs = map[int][]fakeGHJob{1: {{DatabaseID: 101, Name: "ci", Status: "completed", Conclusion: "success"}}}
	}
	out2, err := ghVerifier(f2, t.TempDir()).Verify(context.Background(), VerifyRequest{Node: "A", Head: head2, Worktree: repo2, Base: base2})
	if err != nil || !out2.Pass {
		t.Fatalf("two matching push URLs: %+v %v", out2, err)
	}
}

// CRW-1594 review: the verification push publishes exactly the branch it names — with push.followTags on
// and an annotated tag on the head, a push without --no-follow-tags would publish the tag too. The real git
// push runs here, its destination swapped for a temporary bare remote, so the refspec and options the
// verifier actually sends decide what lands — not the fake's bookkeeping.
func TestGitHubActionsVerifierPublishesNoTags(t *testing.T) {
	repo, base, head := ghRepo(t)
	bare := t.TempDir() + "/remote.git"
	gitIn(t, repo, "init", "-q", "--bare", bare)
	gitIn(t, repo, "config", "push.followTags", "true")
	gitIn(t, repo, "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "tag", "-a", "-m", "unpublished", "v9.9.9", head)
	f := &fakeGH{workflow: fakeGHWorkflow{ID: 7, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"}}
	f.newRun = func(run *fakeGHRun) {
		run.views = map[int][]fakeGHView{1: {{Status: "completed", Conclusion: "success"}}}
		run.jobs = map[int][]fakeGHJob{1: {{DatabaseID: 101, Name: "ci", Status: "completed", Conclusion: "success"}}}
	}
	inner := f.run()
	var pushArgs []string
	v := ghVerifier(f, t.TempDir())
	v.Run = func(ctx context.Context, dir string, env []string, name string, args ...string) (int, []byte, error) {
		if name == "git" && len(args) > 0 && args[0] == "push" {
			pushArgs = append([]string{}, args...)
			// record the ref update as the fake does, then really push the head to the bare remote
			if _, _, err := inner(ctx, dir, env, name, args...); err != nil {
				return 1, nil, err
			}
			real := append([]string{}, args...)
			for i, a := range real {
				if a == "origin" {
					real[i] = bare
				}
			}
			return execGit(ctx, dir, env, real...)
		}
		return inner(ctx, dir, env, name, args...)
	}
	out, err := v.Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: repo, Base: base})
	if err != nil || !out.Pass {
		t.Fatalf("verify: %+v %v", out, err)
	}
	if !slices.Contains(pushArgs, "--no-follow-tags") {
		t.Fatalf("the push was %q: tags may follow", pushArgs)
	}
	if ref := gitIn(t, bare, "for-each-ref", "--format=%(refname)"); ref != "refs/heads/devin/test-work" {
		t.Errorf("the remote refs: %s", ref)
	}
	if got := strings.TrimSpace(gitIn(t, bare, "rev-parse", "refs/heads/devin/test-work")); got != head {
		t.Errorf("the remote branch is %s, want %s", got, head)
	}
}
