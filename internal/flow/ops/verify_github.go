package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
)

// GitHubActionsVerifier verifies a head on GitHub-hosted runners (CRW-1594): the head is pushed as a branch
// of the configured repository, the configured workflow is dispatched on it, its run is waited for and its
// jobs make a verification-record/1 with runner github-actions. Before anything is pushed, gitleaks scans the
// commits of the current base..head. A passed tree is reused from the repository-local cache when the new
// commits carry no objects but commits (a blob added and removed between the run's head and the verified one
// is a new verification, never a reuse). Every external command runs through Run (ExecCommand by default)
// with argument arrays, never a shell, and the caller's environment as it is — gh reads its own login from
// HOME and GH_CONFIG_DIR.
//
// The remote protocol is the one gh 2.46.0 supports: `gh workflow view --json` and `gh run view --json
// attempt` do not exist, so the workflow is selected from `gh api repos/R/actions/workflows` (paginated), the
// run is polled with `gh api repos/R/actions/runs/ID` and each attempt's jobs come from
// `gh api --paginate repos/R/actions/runs/ID/attempts/N/jobs`. Dispatch and run list use the resolved numeric
// workflow ID; `gh run rerun --failed` and `gh run view --job J --log-failed` keep their CLI forms.
type GitHubActionsVerifier struct {
	// Coordinator is the controller's authority store. External candidates must
	// supply it; workers never open replicas or dispatch their own verification.
	Coordinator *store.Store
	CILimit     int
	// Tools are the verifier's tools of the configuration: kind github-actions, repository, workflow, branch prefix.
	Tools crwconfig.VerifierTools
	// RecordDir is where the records are written; empty is ciRecordDir of the worktree.
	RecordDir string
	// Run runs the external commands; nil is ExecCommand.
	Run RunCommand
	// PollInterval is the wait between polls; 0 is the default ghPollInterval.
	PollInterval time.Duration
	// Timeout bounds the whole remote operation; 0 is the default ghVerifyTimeout.
	Timeout time.Duration
}

const (
	ghVerifyTimeout = 45 * time.Minute
	ghPollInterval  = 5 * time.Second
)

// NewVerifier is the verifier the configuration selects (CRW-1594): github-actions for a remote section, the
// local `crw-dev ci local` for everything else.
func NewVerifier(tools crwconfig.VerifierTools) Verifier {
	if tools.VerifierKind() == crwconfig.VerifierGitHubActions {
		return GitHubActionsVerifier{Tools: tools}
	}
	return CILocalVerifier{Tools: tools}
}

// The remote environment entries every external command of the verifier gets: gh and git never prompt, and
// gitleaks reads no ambient configuration.
var ghCommandEnv = []string{"GH_PROMPT_DISABLED=1", "GIT_TERMINAL_PROMPT=0"}

// ghHex is a full commit or tree ID, 40 (SHA-1) or 64 (SHA-256) hex digits.
var ghHex = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// ghRunView is what `gh api repos/R/actions/runs/ID` answers — the REST shape, not a CLI --json shape.
type ghRunView struct {
	DatabaseID         int64   `json:"id"`
	HeadSHA            string  `json:"head_sha"`
	HeadBranch         string  `json:"head_branch"`
	Event              string  `json:"event"`
	WorkflowDatabaseID int64   `json:"workflow_id"`
	Status             string  `json:"status"`
	Conclusion         string  `json:"conclusion"`
	Attempt            int     `json:"run_attempt"`
	URL                string  `json:"html_url"`
	Jobs               []ghJob `json:"-"`
}

type ghStep struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Number     int    `json:"number"`
}

type ghJob struct {
	DatabaseID int64    `json:"id"`
	RunID      int64    `json:"run_id"`
	RunAttempt int      `json:"run_attempt"`
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	Conclusion string   `json:"conclusion"`
	URL        string   `json:"html_url"`
	Steps      []ghStep `json:"steps"`
}

// ghJobsPage is one page of `gh api --paginate repos/R/actions/runs/ID/attempts/N/jobs?per_page=100`.
type ghJobsPage struct {
	Jobs []ghJob `json:"jobs"`
}

// Verify implements Verifier.
func (v GitHubActionsVerifier) Verify(ctx context.Context, req VerifyRequest) (VerifyOutcome, error) {
	if req.Worktree == "" || req.Head == "" {
		return VerifyOutcome{}, errors.New("ops: a verification needs the head and the worktree it is committed in")
	}
	if req.Command != "" && req.Command != CILocalCommand {
		return VerifyOutcome{}, fmt.Errorf("ops: this verifier runs %q only, not %q", CILocalCommand, req.Command)
	}
	if err := v.Tools.Validate(); err != nil {
		return VerifyOutcome{}, fmt.Errorf("ops: the verifier's configuration: %w", err)
	}
	if v.Tools.VerifierKind() != crwconfig.VerifierGitHubActions {
		return VerifyOutcome{}, fmt.Errorf("ops: the verifier kind %q is not github-actions", v.Tools.VerifierKind())
	}
	run := v.Run
	if run == nil {
		run = ExecCommand
	}
	timeout := v.Timeout
	if timeout <= 0 {
		timeout = ghVerifyTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	poll := v.PollInterval
	if poll <= 0 {
		poll = ghPollInterval
	}
	g := &ghVerify{v: v, run: run, poll: poll, repo: v.Tools.Repository, workflow: v.Tools.GitHubWorkflow()}
	return g.verify(ctx, req)
}

// ghVerify is one remote verification.
type ghVerify struct {
	v        GitHubActionsVerifier
	run      RunCommand
	poll     time.Duration
	repo     string
	workflow string
	flight   *store.VerificationFlight
}

func (g *ghVerify) git(ctx context.Context, dir string, args ...string) (string, error) {
	code, out, err := g.run(ctx, dir, ghCommandEnv, "git", args...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	if code != 0 {
		return "", fmt.Errorf("git %s: exit %d", strings.Join(args, " "), code)
	}
	return strings.TrimSpace(string(out)), nil
}

func (g *ghVerify) gh(ctx context.Context, dir string, args ...string) ([]byte, error) {
	code, out, err := g.run(ctx, dir, ghCommandEnv, "gh", args...)
	if err != nil {
		return nil, fmt.Errorf("gh %s: %w", strings.Join(args, " "), err)
	}
	if code != 0 {
		return nil, fmt.Errorf("gh %s: exit %d", strings.Join(args, " "), code)
	}
	return out, nil
}

func (g *ghVerify) sleep(ctx context.Context) error {
	timer := time.NewTimer(g.poll)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// resolve is `git rev-parse --verify --end-of-options <ref>^{commit}` answered as a full hex SHA.
func (g *ghVerify) resolve(ctx context.Context, dir, ref string) (string, error) {
	sha, err := g.git(ctx, dir, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil || !ghHex.MatchString(sha) {
		return "", fmt.Errorf("%s does not resolve to a commit", ref)
	}
	return sha, nil
}

// commitFile is a file as the commit has it, "" when the commit has none.
func (g *ghVerify) commitFile(ctx context.Context, dir, head, path string) (string, bool, error) {
	code, out, err := g.run(ctx, dir, ghCommandEnv, "git", "show", head+":"+path)
	if err != nil {
		return "", false, fmt.Errorf("git show %s:%s: %w", head, path, err)
	}
	if code != 0 {
		return "", false, nil
	}
	return string(out), true, nil
}

// commitDigest is sha256:<hex> of a file as the commit has it, "" when absent.
func (g *ghVerify) commitDigest(ctx context.Context, dir, head, path string) (string, error) {
	body, found, err := g.commitFile(ctx, dir, head, path)
	if err != nil || !found {
		return "", err
	}
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ghDependencyFiles are the dependency files the record digests — the same set a local record covers.
var ghDependencyFiles = []string{"go.sum", "web/package-lock.json"}

const ghCIFile = ".github/workflows/ci.yml"

func (g *ghVerify) verify(ctx context.Context, req VerifyRequest) (VerifyOutcome, error) {
	wt := req.Worktree
	// step 1: the head, the base, the history, the commit's declarations and the record's home
	head, err := g.resolve(ctx, wt, req.Head)
	if err != nil {
		return VerifyOutcome{}, err
	}
	baseRef := req.Base
	if baseRef == "" {
		baseRef = "origin/dev"
	}
	base, err := g.resolve(ctx, wt, baseRef)
	if err != nil {
		return VerifyOutcome{}, fmt.Errorf("the base %s could not be resolved: %w", baseRef, err)
	}
	shallow, err := g.git(ctx, wt, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return VerifyOutcome{}, err
	}
	if shallow != "false" {
		return VerifyOutcome{}, errors.New("ops: the worktree's history is shallow: a remote verification needs the full history")
	}
	tree, err := g.git(ctx, wt, "rev-parse", head+"^{tree}")
	if err != nil {
		return VerifyOutcome{}, err
	}
	if !ghHex.MatchString(tree) {
		return VerifyOutcome{}, fmt.Errorf("the head's tree %q is not a full object ID", tree)
	}
	commonDir, err := g.git(ctx, wt, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return VerifyOutcome{}, err
	}
	if !filepath.IsAbs(commonDir) {
		return VerifyOutcome{}, fmt.Errorf("the git common directory %q is not an absolute path", commonDir)
	}
	// the record's directory is established before anything remote is pushed: a verification that cannot
	// write its receipt never begins (CRW-1594 rework: not after a push and a 45-minute wait).
	recordDir := g.v.RecordDir
	if recordDir == "" {
		if recordDir, err = ciRecordDir(wt); err != nil {
			return VerifyOutcome{}, fmt.Errorf("ops: ci_record_dir: %w", err)
		}
	}
	ciDigest, err := g.commitDigest(ctx, wt, head, ghCIFile)
	if err != nil {
		return VerifyOutcome{}, err
	}
	deps := map[string]string{}
	for _, file := range ghDependencyFiles {
		digest, err := g.commitDigest(ctx, wt, head, file)
		if err != nil {
			return VerifyOutcome{}, err
		}
		deps[file] = digest
	}
	pins, err := dagsched.DeclaredToolPins(func(path string) ([]byte, bool, error) {
		body, found, err := g.commitFile(ctx, wt, head, path)
		return []byte(body), found, err
	})
	if err != nil {
		return VerifyOutcome{}, err
	}
	// the selected workflow: active, a single file below .github/workflows/, read from HEAD. gh 2.46.0 has no
	// `gh workflow view --json`; the listing comes from the REST endpoint through gh api.
	out, err := g.gh(ctx, wt, "api", "--paginate", "repos/"+g.repo+"/actions/workflows?per_page=100")
	if err != nil {
		return VerifyOutcome{}, err
	}
	workflow, err := crwconfig.DecodeGitHubWorkflow(out, g.workflow)
	if err != nil {
		return VerifyOutcome{}, err
	}
	workflowBody, found, err := g.commitFile(ctx, wt, head, workflow.Path)
	if err != nil {
		return VerifyOutcome{}, err
	}
	if !found {
		return VerifyOutcome{}, fmt.Errorf("the workflow %s is not in %s", workflow.Path, head)
	}
	wfSum := sha256.Sum256([]byte(workflowBody))
	workflowDigest := "sha256:" + hex.EncodeToString(wfSum[:])

	// step 2: the secret scan of the current range, before push and before any cache reuse
	if err := g.scanSecrets(ctx, wt, commonDir, base, head, pins["gitleaks"]); err != nil {
		return VerifyOutcome{}, err
	}
	if g.v.Coordinator != nil {
		if err := g.scanBlobs(ctx, wt, base, head); err != nil {
			return VerifyOutcome{}, err
		}
	}

	// steps 3/4: the repository-local cache of a passed tree, judged by the real receipt judge
	key := g.cacheKey(workflow, workflowDigest, tree, ciDigest, deps, pins)
	want := dagsched.VerificationKeys{Base: base, Tree: tree, CiDigest: ciDigest, Dependencies: deps, Pins: pins}
	reused, err := g.cachedRecord(ctx, wt, commonDir, tree, key, head, want)
	if err != nil {
		return VerifyOutcome{}, err
	}
	// A controller flight shares terminal failure as well as success and owns
	// remote-ref checks. A local cache must not bypass those facts.
	if g.v.Coordinator != nil {
		reused = nil
	}
	var raw []byte
	var note string
	if reused != nil {
		receipt := *reused
		receipt.HeadCommit, receipt.BaseCommit = head, base
		receipt.GitHub.Scan = dagsched.GitHubScan{Base: base, Head: head, Result: "clean"}
		receipt.GitHub.ReusedFrom = &dagsched.GitHubReusedFrom{Digest: reused.Digest, Head: reused.HeadCommit, VerifierKind: dagsched.GitHubVerifierKind}
		if raw, err = dagsched.SealVerificationRecord(receipt); err != nil {
			return VerifyOutcome{}, err
		}
	} else {
		// steps 5-8: branch, push, dispatch, wait, rerun once on failure
		var derr *dispatchError
		raw, note, derr = g.dispatch(ctx, wt, req, head, base, tree, ciDigest, deps, pins, workflow, workflowDigest, key, commonDir)
		if derr != nil {
			// a failed remote operation can still carry the receipt it built: write it like any record.
			if len(derr.outcome.Record) > 0 {
				_ = g.writeRecordFile(recordDir, head, derr.outcome.Record)
			}
			return derr.outcome, derr.err
		}
	}
	// the outcome and the record file
	rec, derr := dagsched.DecodeVerificationRecord(raw)
	if derr != nil {
		return VerifyOutcome{}, fmt.Errorf("ops: the remote record could not be read: %v", derr)
	}
	if err := g.writeRecordFile(recordDir, head, raw); err != nil {
		return VerifyOutcome{}, err
	}
	outcome := VerifyOutcome{Command: CILocalCommand, Digest: rec.Digest, Record: raw, Pass: rec.Result == "pass",
		Failures: []string{}, FailedTests: recordFailedTests(rec)}
	if rec.GitHub != nil {
		outcome.Note = strings.TrimSpace(fmt.Sprintf("verified by github-actions: %s %s", rec.GitHub.URL, note))
		outcome.Failures = append(outcome.Failures, recordFailures(rec)...)
	}
	switch {
	case outcome.Pass:
		outcome.Summary = "pass"
	case len(outcome.Failures) > 0:
		outcome.Summary = strings.Join(outcome.Failures, "; ")
	default:
		outcome.Summary = "the remote verification failed"
	}
	return outcome, nil
}

// writeRecordFile writes the sealed record below the record directory, creating it.
func (g *ghVerify) writeRecordFile(dir, head string, raw []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return writeRecordAtomic(filepath.Join(dir, "verification-record-"+head+".json"), raw)
}

// ghJobRecord is a job as the record writes it: the run's answer, the mapped result and, for a failed job,
// the bounded failing lines its --log-failed gave.
type ghJobRecord struct {
	Name       string         `json:"name"`
	Status     string         `json:"status"`
	Conclusion string         `json:"conclusion"`
	Result     string         `json:"result"`
	DatabaseID int64          `json:"databaseId"`
	URL        string         `json:"url,omitempty"`
	Steps      []ghStepRecord `json:"steps,omitempty"`
	Failures   []FailedStep   `json:"failures,omitempty"`
}

type ghStepRecord struct {
	Name       string `json:"name"`
	Number     int    `json:"number"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Result     string `json:"result"`
}

func ghJobRecordOf(job ghJob, failures []FailedStep) ghJobRecord {
	rec := ghJobRecord{Name: job.Name, Status: job.Status, Conclusion: job.Conclusion, DatabaseID: job.DatabaseID,
		URL: job.URL, Failures: failures}
	rec.Result = "failed"
	if job.Conclusion == "success" && job.Status == "completed" {
		rec.Result = "passed"
	}
	for _, step := range job.Steps {
		sr := ghStepRecord{Name: step.Name, Number: step.Number, Status: step.Status, Conclusion: step.Conclusion, Result: "failed"}
		if step.Conclusion == "success" {
			sr.Result = "passed"
		}
		rec.Steps = append(rec.Steps, sr)
	}
	return rec
}

// scanSecrets is step 2: gitleaks over the current base..head with merge deltas, in a private directory,
// against the absolute git directory, with the repository's .gitleaks.toml when the head carries one and an
// empty ignore file plus --ignore-gitleaks-allow so nothing is suppressed locally.
func (g *ghVerify) scanSecrets(ctx context.Context, wt, commonDir, base, head, version string) error {
	if version == "" {
		return errors.New("the verified commit declares no Gitleaks version in scripts/ci/secrets.sh: nothing was pushed")
	}
	tmp, err := tmploc.MkdirTemp("crw-gh-scan-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	ignore := filepath.Join(tmp, "gitleaks.ignore")
	if err := os.WriteFile(ignore, nil, 0o600); err != nil {
		return err
	}
	args := []string{"git", "--ignore-gitleaks-allow", "--redact", "--no-banner", "--gitleaks-ignore-path", ignore}
	if body, found, err := g.commitFile(ctx, wt, head, ".gitleaks.toml"); err != nil {
		return err
	} else if found {
		config := filepath.Join(tmp, ".gitleaks.toml")
		if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
			return err
		}
		args = append(args, "--config", config)
	}
	args = append(args, "--log-opts=-m "+base+".."+head, commonDir)
	env := append(append([]string{}, ghCommandEnv...), "GITLEAKS_CONFIG=", "GITLEAKS_CONFIG_TOML=")
	// the scanner is the version the verified commit declares: anything else is refused, its own
	// version output stays out of the error — it could carry bytes that are not a version
	code, versionOut, err := g.run(ctx, tmp, env, "gitleaks", "version")
	if err != nil {
		return fmt.Errorf("the pinned Gitleaks version could not be checked: %w", err)
	}
	if code != 0 || strings.TrimPrefix(strings.TrimSpace(string(versionOut)), "v") != version {
		return fmt.Errorf("the scanner must be Gitleaks version %s as the verified commit declares: nothing was pushed", version)
	}
	code, _, err = g.run(ctx, tmp, env, "gitleaks", args...)
	if err != nil {
		return fmt.Errorf("the secret scan of %s..%s could not run: %w", base, head, err)
	}
	if code != 0 {
		return fmt.Errorf("the secret scan of %s..%s found a secret or failed (gitleaks exit %d): nothing was pushed", base, head, code)
	}
	return nil
}

// cacheKey is the canonical-JSON hash of the keys a cached record must agree with (CRW-1594, CRW-1590's
// separation of immutable tree, rules and platform from the current range).
func (g *ghVerify) cacheKey(workflow crwconfig.GitHubWorkflowInfo, workflowDigest, tree, ciDigest string, deps, pins map[string]string) string {
	raw, _ := json.Marshal(map[string]any{
		"verifierKind": dagsched.GitHubVerifierKind, "verifierRules": dagsched.GitHubVerifierRules,
		"repository": g.repo, "workflowID": workflow.ID, "workflowPath": workflow.Path, "workflowDigest": workflowDigest,
		"treeHash": tree, "ciDigest": ciDigest, "dependencies": deps, "pins": pins})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// cachedRecord reads <commonDir>/crw-ci/github-actions/<tree>/<key>.json through an os.Root on the existing
// absolute git common directory: every component must exist as a real directory or regular file, a symlink
// is never followed, and any missing, unreadable, malformed or unsafe entry is a miss, never an error and
// never a pass. A candidate must be an original passing run receipt (no reusedFrom) whose repository is this
// one, must be accepted by JudgeVerificationRecord against the current keys with its own base, and must have
// brought in only commit objects since its run's head — a blob added and deleted between them is a new
// verification.
func (g *ghVerify) cachedRecord(ctx context.Context, wt, commonDir, tree, key, head string, want dagsched.VerificationKeys) (*dagsched.VerificationRecord, error) {
	root, err := os.OpenRoot(commonDir)
	if err != nil {
		return nil, nil
	}
	defer root.Close()
	rel := "crw-ci/github-actions/" + tree
	for _, dir := range []string{"crw-ci", "crw-ci/github-actions", rel} {
		info, err := root.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, nil
		}
	}
	entry := rel + "/" + key + ".json"
	info, err := root.Lstat(entry)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, nil
	}
	file, err := root.Open(entry)
	if err != nil {
		return nil, nil
	}
	raw, err := io.ReadAll(file)
	file.Close()
	if err != nil {
		return nil, nil
	}
	return g.reusableRecord(ctx, wt, tree, key, head, want, raw)
}

// reusableRecord is shared by the local cache and controller flight reuse.
func (g *ghVerify) reusableRecord(ctx context.Context, wt, tree, key, head string, want dagsched.VerificationKeys, raw []byte) (*dagsched.VerificationRecord, error) {
	record, err := dagsched.DecodeVerificationRecord(raw)
	if err != nil {
		return nil, nil
	}
	digest, err := dagsched.VerificationRecordDigest(raw)
	if err != nil || record.Digest != digest {
		return nil, nil
	}
	if record.Runner != dagsched.GitHubRunner || record.VerifierKind != dagsched.GitHubVerifierKind ||
		record.VerifierRules != dagsched.GitHubVerifierRules || record.Result != "pass" || len(record.Jobs) == 0 ||
		record.Repository != g.repo {
		return nil, nil
	}
	gh := record.GitHub
	if gh == nil || gh.RunID <= 0 || !ghHex.MatchString(gh.RunHead) || gh.Conclusion != "success" ||
		(gh.Attempts != 1 && gh.Attempts != 2) || gh.Event != "workflow_dispatch" || gh.ReusedFrom != nil {
		return nil, nil
	}
	// the cached keys must agree: tree, ci.yml, dependencies, pins and the workflow identity
	want2 := g.cacheKey(crwconfig.GitHubWorkflowInfo{ID: gh.WorkflowID, Path: gh.WorkflowPath}, gh.WorkflowDigest, record.TreeHash,
		record.CiDigest, record.Dependencies, record.Pins)
	if want2 != key || record.TreeHash != tree {
		return nil, nil
	}
	// the record must pass the real receipt judge against the current keys with its own base: a forged or
	// inconsistent cache entry is a miss, never a pass.
	cachedKeys := want
	cachedKeys.Base = record.BaseCommit
	if _, err := dagsched.JudgeVerificationRecord(raw, cachedKeys); err != nil {
		return nil, nil
	}
	// reuse only when every object the verified head adds over the run's head is a commit: a blob added and
	// deleted between them leaves the tree identical but is a different verification.
	objects, err := g.git(ctx, wt, "rev-list", "--objects", "--no-object-names", head, "^"+gh.RunHead)
	if err != nil {
		return nil, nil
	}
	for _, oid := range strings.Fields(objects) {
		if !ghHex.MatchString(oid) {
			return nil, nil
		}
		kind, err := g.git(ctx, wt, "cat-file", "-t", oid)
		if err != nil || kind != "commit" {
			return nil, nil
		}
	}
	return &record, nil
}

// dispatchError is a failed remote operation with the outcome it still wrote (a failure receipt), if any.
type dispatchError struct {
	outcome VerifyOutcome
	err     error
}

// dispatch is steps 5-8: pick the branch, check the remote, push the head, dispatch the workflow, wait, and
// rerun the failed jobs once. On completion it returns the sealed record and a note; on an infrastructure
// failure after the first attempt completed, it still returns the nonpassing receipt it built.
func (g *ghVerify) dispatch(ctx context.Context, wt string, req VerifyRequest, head, base, tree, ciDigest string,
	deps, pins map[string]string, workflow crwconfig.GitHubWorkflowInfo, workflowDigest, key, commonDir string) ([]byte, string, *dispatchError) {
	if g.v.Coordinator != nil {
		return g.coordinatedDispatch(ctx, wt, req, head, base, tree, ciDigest, deps, pins, workflow, workflowDigest)
	}
	fail := func(err error) ([]byte, string, *dispatchError) { return nil, "", &dispatchError{err: err} }
	branch, err := g.branchFor(ctx, wt, req, head)
	if err != nil {
		return fail(err)
	}
	if err := g.checkRemote(ctx, wt); err != nil {
		return fail(err)
	}
	if _, err := g.git(ctx, wt, "push", "--no-follow-tags", "origin", head+":refs/heads/"+branch); err != nil {
		return fail(fmt.Errorf("the push of the head to %s failed: %w", branch, err))
	}
	// dispatch and find the new run: the resolved numeric workflow ID, never the ambiguous name
	maxID := int64(0)
	workflowID := fmt.Sprint(workflow.ID)
	if entries, err := g.listRuns(ctx, wt, workflowID, branch); err != nil {
		return fail(err)
	} else {
		for _, e := range entries {
			maxID = max(maxID, e.DatabaseID)
		}
	}
	if _, err := g.gh(ctx, wt, "workflow", "run", workflowID, "--repo", g.repo, "--ref", branch); err != nil {
		return fail(fmt.Errorf("the dispatch of workflow %d on %s failed: %w", workflow.ID, branch, err))
	}
	var runID int64
	for runID == 0 {
		entries, err := g.listRuns(ctx, wt, workflowID, branch)
		if err != nil {
			return fail(err)
		}
		for _, e := range entries {
			if e.DatabaseID > maxID && e.HeadSHA == head && e.HeadBranch == branch &&
				e.Event == "workflow_dispatch" && e.WorkflowDatabaseID == workflow.ID {
				runID = e.DatabaseID
			}
		}
		if runID == 0 {
			if err := g.sleep(ctx); err != nil {
				return fail(fmt.Errorf("waiting for the run of %s on %s: %w", head, branch, err))
			}
		}
	}
	// wait for the run's completion and rerun its failed jobs once
	attempts, passed, failures, ferr := g.awaitRun(ctx, wt, runID, head, branch, workflow)
	if ferr != nil {
		if len(attempts.runs) == 0 {
			return fail(ferr)
		}
		// the attempt-1 record was built before the retry: a rerun that cannot start or never completes
		// still leaves the nonpassing receipt it reached.
		record := g.buildRecord(base, head, tree, ciDigest, deps, pins, workflow, workflowDigest, branch, runID, attempts)
		raw, serr := dagsched.SealVerificationRecord(record)
		if serr != nil {
			return fail(serr)
		}
		return nil, "", &dispatchError{outcome: ghOutcomeOf(raw, record, failures), err: ferr}
	}
	// step 9: the sealed record
	record := g.buildRecord(base, head, tree, ciDigest, deps, pins, workflow, workflowDigest, branch, runID, attempts)
	record.Result = "fail"
	record.GitHub.Conclusion = attempts.runs[len(attempts.runs)-1].Conclusion
	if passed {
		record.Result = "pass"
		record.GitHub.Conclusion = "success"
		record.GitHub.FlakeRerunPassed = len(attempts.runs) == 2
	}
	raw, err := dagsched.SealVerificationRecord(record)
	if err != nil {
		return fail(err)
	}
	note := ""
	if passed {
		// the produced pass must satisfy the same acceptance contract the judge applies: a receipt the
		// judge refuses is never a pass and never cached — it is an infrastructure error carrying the
		// observed failure receipt (CRW-1594 final step)
		if _, jerr := dagsched.JudgeVerificationRecord(raw, dagsched.VerificationKeys{Base: base, Tree: tree, CiDigest: ciDigest, Dependencies: deps, Pins: pins}); jerr != nil {
			record.Result = "fail"
			record.GitHub.FlakeRerunPassed = false
			observed, _ := dagsched.SealVerificationRecord(record)
			return nil, "", &dispatchError{outcome: ghOutcomeOf(observed, record, failures),
				err: fmt.Errorf("the produced verification receipt fails the acceptance contract: %w", jerr)}
		}
		if werr := g.writeCache(commonDir, tree, key, raw); werr != nil {
			note = "the cache could not be written: " + werr.Error()
		}
	}
	return raw, note, nil
}

// buildRecord is the verification-record/1 of the run's attempts, before its pass/fail fields.
func (g *ghVerify) buildRecord(base, head, tree, ciDigest string, deps, pins map[string]string,
	workflow crwconfig.GitHubWorkflowInfo, workflowDigest, branch string, runID int64, attempts ghAttempts) dagsched.VerificationRecord {
	record := dagsched.VerificationRecord{
		Runner: dagsched.GitHubRunner, Repository: g.repo, BaseCommit: base, HeadCommit: head, TreeHash: tree,
		CiDigest: ciDigest, Tools: map[string]string{}, Pins: pins, GoFlags: "", GoEnv: "", PinMismatch: []string{},
		Dependencies: deps, OS: "", Arch: "", Result: "fail",
		VerifierKind: dagsched.GitHubVerifierKind, VerifierRules: dagsched.GitHubVerifierRules,
	}
	for _, name := range attempts.jobOrder {
		raw, _ := json.Marshal(attempts.merged[name])
		record.Jobs = append(record.Jobs, raw)
	}
	record.GitHub = &dagsched.GitHubVerification{
		RunID: runID, URL: fmt.Sprintf("https://github.com/%s/actions/runs/%d", g.repo, runID),
		RunHead: head, Branch: branch, Event: "workflow_dispatch",
		WorkflowID: workflow.ID, WorkflowPath: workflow.Path, WorkflowDigest: workflowDigest,
		Conclusion: attempts.runs[len(attempts.runs)-1].Conclusion,
		Attempts:   len(attempts.runs), Runs: attempts.runs,
		Scan: dagsched.GitHubScan{Base: base, Head: head, Result: "clean"},
	}
	return record
}

// ghOutcomeOf is the outcome of a record the verify reached: digest, summary and failures.
func ghOutcomeOf(raw []byte, record dagsched.VerificationRecord, failures []FailedStep) VerifyOutcome {
	digest, _ := dagsched.VerificationRecordDigest(raw)
	outcome := VerifyOutcome{Command: CILocalCommand, Digest: digest, Record: raw, Pass: record.Result == "pass",
		FailedTests: failures, Failures: recordFailures(record)}
	if record.GitHub != nil {
		outcome.Note = "verified by github-actions: " + record.GitHub.URL
	}
	switch {
	case outcome.Pass:
		outcome.Summary = "pass"
	case len(outcome.Failures) > 0:
		outcome.Summary = strings.Join(outcome.Failures, "; ")
	default:
		outcome.Summary = "the remote verification failed"
	}
	return outcome
}

// ghAttempts is what the run's polls answered: the jobs merged by name over the attempts, their order, the
// attempt rows the record keeps and the failing lines collected.
type ghAttempts struct {
	merged   map[string]ghJobRecord
	jobOrder []string
	runs     []dagsched.GitHubRunAttempt
}

// awaitRun polls the run until it completes, rerunning the failed jobs once when the first completion fails.
// Its verdict is truthful: a retry only passes when its own completion is a success and every merged job
// passed. The failing lines of every failed job of every completed attempt are collected; a skipped or
// cancelled job keeps its conclusion as the explanation and a log that cannot be read becomes a diagnostic
// failure line, never a discarded receipt.
func (g *ghVerify) awaitRun(ctx context.Context, wt string, runID int64, head, branch string, workflow crwconfig.GitHubWorkflowInfo) (ghAttempts, bool, []FailedStep, error) {
	out := ghAttempts{merged: map[string]ghJobRecord{}}
	var failedTests []FailedStep
	saved, err := g.retryFirst(ctx)
	if err != nil {
		return out, false, nil, err
	}
	var view ghRunView
	if saved != nil {
		view = *saved
	} else {
		view, err = g.waitRun(ctx, wt, runID, head, branch, workflow, 1)
		if err != nil && g.flight != nil {
			// A concurrent attachment may already have reserved the rerun.
			if first, e := g.retryFirst(ctx); e == nil && first != nil {
				view, err = *first, nil
			}
		}
	}
	if err != nil {
		return out, false, nil, err
	}
	logs := func(jobs []ghJob) (map[string][]FailedStep, []FailedStep) {
		jobFailures := map[string][]FailedStep{}
		var all []FailedStep
		for _, job := range jobs {
			switch job.Conclusion {
			case "success":
				continue
			case "failure":
				raw, err := g.gh(ctx, wt, "run", "view", fmt.Sprint(runID), "--repo", g.repo,
					"--job", fmt.Sprint(job.DatabaseID), "--log-failed")
				if err != nil {
					step := FailedStep{Job: job.Name, Step: job.Name, Tail: "the job's failed logs could not be read: " + err.Error()}
					jobFailures[job.Name] = []FailedStep{step}
					all = append(all, step)
					continue
				}
				step := failedSteps(job.Name, job.Name, ghLogNormalize(string(raw)))
				jobFailures[job.Name] = []FailedStep{step}
				all = append(all, step)
			default:
				step := FailedStep{Job: job.Name, Step: job.Name, Tail: "the job ended " + job.Conclusion}
				jobFailures[job.Name] = []FailedStep{step}
				all = append(all, step)
			}
		}
		return jobFailures, all
	}
	if view.Conclusion != "failure" {
		// a completion that is not a retryable failure: success, cancellation, timed out, skipped
		jobFailures, all := logs(view.Jobs)
		failedTests = all
		out.runs = append(out.runs, runAttempt(view, jobFailures))
		for _, job := range view.Jobs {
			out.merged[job.Name] = ghJobRecordOf(job, jobFailures[job.Name])
			out.jobOrder = append(out.jobOrder, job.Name)
		}
		return out, runPassed(view), failedTests, nil
	}
	// attempt 1 failed: collect its failed-job evidence, build its record row and its merged jobs before
	// the rerun is even issued — a rerun that cannot start still leaves the complete attempt-1 receipt.
	jobFailures1, all1 := logs(view.Jobs)
	failedTests = all1
	first := view
	out.runs = append(out.runs, runAttempt(first, jobFailures1))
	for _, job := range first.Jobs {
		out.merged[job.Name] = ghJobRecordOf(job, jobFailures1[job.Name])
		out.jobOrder = append(out.jobOrder, job.Name)
	}
	sendRetry := true
	if g.flight != nil {
		raw, _ := json.Marshal(struct {
			ghRunView
			SavedJobs []ghJob `json:"saved_jobs"`
		}{first, first.Jobs})
		sendRetry, err = g.v.Coordinator.ClaimVerificationRetry(ctx, g.flight.Key, runID, raw)
		if err != nil {
			return out, false, failedTests, err
		}
	}
	if sendRetry {
		if _, err := g.gh(ctx, wt, "run", "rerun", fmt.Sprint(runID), "--repo", g.repo, "--failed"); err != nil {
			return out, false, failedTests, fmt.Errorf("the rerun of the failed jobs of run %d is uncertain: %w", runID, err)
		}
	}
	view2, err := g.waitRun(ctx, wt, runID, head, branch, workflow, first.Attempt+1)
	if err != nil {
		return out, false, failedTests, err
	}
	jobFailures2, all2 := logs(view2.Jobs)
	failedTests = append(failedTests, all2...)
	out.runs = append(out.runs, runAttempt(view2, jobFailures2))
	for _, job := range view2.Jobs {
		if !slicesContains(out.jobOrder, job.Name) {
			out.jobOrder = append(out.jobOrder, job.Name)
		}
		out.merged[job.Name] = ghJobRecordOf(job, jobFailures2[job.Name])
	}
	passed := view2.Status == "completed" && view2.Conclusion == "success" && len(view2.Jobs) > 0 && runMergedPassed(out.merged)
	return out, passed, failedTests, nil
}

// runAttempt is the record row of one attempt. A run that answered two jobs of one name is an inconsistent
// answer, not a verdict — the record keeps the row but the attempt's verdict cannot hide a same-named
// failure behind a same-named pass, so the name is marked duplicate by keeping both jobs in the row while
// the merge keeps the last.
func runAttempt(view ghRunView, jobFailures map[string][]FailedStep) dagsched.GitHubRunAttempt {
	var jobs []json.RawMessage
	seen := map[string]bool{}
	for _, job := range view.Jobs {
		rec := ghJobRecordOf(job, jobFailures[job.Name])
		if seen[job.Name] {
			// a duplicated name is never silently overwritten: it stays a failure in the record
			rec.Result = "failed"
		}
		seen[job.Name] = true
		raw, _ := json.Marshal(rec)
		jobs = append(jobs, raw)
	}
	return dagsched.GitHubRunAttempt{Attempt: view.Attempt, Status: view.Status, Conclusion: view.Conclusion, Jobs: jobs}
}

func runPassed(view ghRunView) bool {
	if view.Status != "completed" || view.Conclusion != "success" || len(view.Jobs) == 0 {
		return false
	}
	for _, job := range view.Jobs {
		if job.Name == "" || job.Status != "completed" || job.Conclusion != "success" {
			return false
		}
	}
	return true
}

// runMergedPassed is the verdict of the merged jobs: every one completed, succeeded and passed.
func runMergedPassed(merged map[string]ghJobRecord) bool {
	if len(merged) == 0 {
		return false
	}
	for _, job := range merged {
		if job.Name == "" || job.Status != "completed" || job.Conclusion != "success" || job.Result != "passed" {
			return false
		}
	}
	return true
}

// waitRun polls `gh api repos/R/actions/runs/ID` until the run completes at an attempt of at least
// minAttempt — a completed answer of an older attempt is not the rerun's, and an attempt beyond the one
// expected is an infrastructure error (the verifier did not authorize or observe that retry). On completion
// the attempt's jobs come from `gh api --paginate .../attempts/N/jobs`.
func (g *ghVerify) waitRun(ctx context.Context, wt string, runID int64, head, branch string, workflow crwconfig.GitHubWorkflowInfo, minAttempt int) (ghRunView, error) {
	runPath := "repos/" + g.repo + "/actions/runs/" + fmt.Sprint(runID)
	for {
		raw, err := g.gh(ctx, wt, "api", runPath)
		if err != nil {
			return ghRunView{}, err
		}
		var view ghRunView
		if err := json.Unmarshal(raw, &view); err != nil {
			return ghRunView{}, fmt.Errorf("the run view of %d could not be read: %w", runID, err)
		}
		if view.DatabaseID != runID || view.HeadSHA != head || view.HeadBranch != branch ||
			view.Event != "workflow_dispatch" || view.WorkflowDatabaseID != workflow.ID {
			return ghRunView{}, fmt.Errorf("the run view of %d names another run (sha %s, branch %s, event %s, workflow %d)",
				runID, view.HeadSHA, view.HeadBranch, view.Event, view.WorkflowDatabaseID)
		}
		if view.Attempt > minAttempt {
			return ghRunView{}, fmt.Errorf("run %d is at attempt %d: the verifier authorized attempt %d only", runID, view.Attempt, minAttempt)
		}
		if view.Status == "completed" && view.Attempt == minAttempt {
			jobsRaw, err := g.gh(ctx, wt, "api", "--paginate", runPath+"/attempts/"+fmt.Sprint(view.Attempt)+"/jobs?per_page=100")
			if err != nil {
				return ghRunView{}, err
			}
			decoder := json.NewDecoder(strings.NewReader(string(jobsRaw)))
			for {
				var page ghJobsPage
				if err := decoder.Decode(&page); err == io.EOF {
					break
				} else if err != nil {
					return ghRunView{}, fmt.Errorf("the jobs of run %d attempt %d could not be read: %w", runID, view.Attempt, err)
				}
				view.Jobs = append(view.Jobs, page.Jobs...)
			}
			// an unnamed, duplicate or unidentified job is an inconsistent answer: it is never evidence,
			// so the completed view carrying one is an infrastructure error, not a verdict
			seen := map[string]bool{}
			for _, job := range view.Jobs {
				if g.flight != nil && (job.RunID != runID || job.RunAttempt != view.Attempt) {
					return ghRunView{}, fmt.Errorf("job %d belongs to another run or attempt", job.DatabaseID)
				}
				if job.Name == "" || job.DatabaseID <= 0 || seen[job.Name] {
					return ghRunView{}, fmt.Errorf("run %d attempt %d has an unnamed, duplicate or unidentified job", runID, view.Attempt)
				}
				seen[job.Name] = true
			}
			return view, nil
		}
		if err := g.sleep(ctx); err != nil {
			return ghRunView{}, fmt.Errorf("waiting for run %d: %w", runID, err)
		}
	}
}

// ghRunEntry is one `gh run list` row.
type ghRunEntry struct {
	DatabaseID         int64  `json:"databaseId"`
	HeadSHA            string `json:"headSha"`
	HeadBranch         string `json:"headBranch"`
	Event              string `json:"event"`
	WorkflowDatabaseID int64  `json:"workflowDatabaseId"`
}

// listRuns is `gh run list` for the numeric workflow ID and branch of workflow_dispatch runs.
func (g *ghVerify) listRuns(ctx context.Context, wt, workflowID, branch string) ([]ghRunEntry, error) {
	raw, err := g.gh(ctx, wt, "run", "list", "--repo", g.repo, "--workflow", workflowID, "--branch", branch,
		"--event", "workflow_dispatch", "--limit", "100",
		"--json", "databaseId,headSha,headBranch,event,workflowDatabaseId")
	if err != nil {
		return nil, err
	}
	var entries []ghRunEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("the run list of workflow %s on %s could not be read: %w", workflowID, branch, err)
	}
	return entries, nil
}

// ghLogTimestamp is an RFC3339-like timestamp at the start of a `gh run view --job --log-failed` log column.
var ghLogTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})`)

// ghLogNormalize strips the "<job>\t<step>\t<timestamp> " prefix the real gh --log-failed puts on every
// line: when a line has three tab-delimited columns and its third begins with a timestamp, the first two
// columns and the timestamp through its first space go. Everything else — indentation, FAIL\t<package>
// lines, plain unprefixed output — is kept as it is.
func ghLogNormalize(raw string) string {
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	for i, line := range lines {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 || !ghLogTimestamp.MatchString(parts[2]) {
			continue
		}
		if j := strings.IndexByte(parts[2], ' '); j >= 0 {
			lines[i] = parts[2][j+1:]
		} else {
			lines[i] = parts[2]
		}
	}
	return strings.Join(lines, "\n")
}

// branchFor picks the remote branch: the current symbolic branch when its tip is the head, a work branch
// pointing at the head (devin/, codex/ or the configured prefix first), or a generated ci/<name>-<sha8>.
// Protected names and the request's base are never picked, and whatever is picked must pass
// `git check-ref-format` before it is pushed.
func (g *ghVerify) branchFor(ctx context.Context, wt string, req VerifyRequest, head string) (string, error) {
	if g.v.Coordinator != nil {
		branch := g.v.Tools.GitHubBranchPrefix() + "verify-" + head
		if _, err := g.git(ctx, wt, "check-ref-format", "refs/heads/"+branch); err != nil {
			return "", err
		}
		return branch, nil
	}
	protected := map[string]bool{"dev": true, "main": true, "master": true}
	if base := req.Base; base != "" {
		base = strings.TrimPrefix(strings.TrimPrefix(base, "refs/heads/"), "origin/")
		protected[base] = true
	}
	check := func(branch string) bool {
		_, err := g.git(ctx, wt, "check-ref-format", "refs/heads/"+branch)
		return err == nil
	}
	// the current symbolic branch when its tip is the head
	if branch, err := g.git(ctx, wt, "symbolic-ref", "-q", "--short", "HEAD"); err == nil && !protected[branch] && check(branch) {
		if tip, err := g.git(ctx, wt, "rev-parse", "refs/heads/"+branch); err == nil && tip == head {
			return branch, nil
		}
	}
	// a local work branch pointing at the head
	out, err := g.git(ctx, wt, "for-each-ref", "--points-at", head, "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		return "", err
	}
	var task, other []string
	for _, branch := range strings.Fields(out) {
		if protected[branch] || !check(branch) {
			continue
		}
		if strings.HasPrefix(branch, "devin/") || strings.HasPrefix(branch, "codex/") || strings.HasPrefix(branch, g.v.Tools.GitHubBranchPrefix()) {
			task = append(task, branch)
		} else {
			other = append(other, branch)
		}
	}
	for _, branch := range append(task, other...) {
		return branch, nil
	}
	// none: a generated branch of the configured prefix
	name := req.Node
	if name == "" {
		name = "verify"
	}
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	branch := g.v.Tools.GitHubBranchPrefix() + strings.Trim(b.String(), "-") + "-" + head[:8]
	if protected[strings.TrimSpace(branch)] {
		return "", fmt.Errorf("the generated branch %s is protected", branch)
	}
	if !check(branch) {
		return "", fmt.Errorf("the generated branch %s is not a ref", branch)
	}
	return branch, nil
}

// checkRemote verifies every push URL of origin is the configured GitHub repository — a push goes to all
// of them, so the first get-url answer is not enough. The error names origin and the configured repository
// only — a remote URL can carry credentials, so it never appears in one.
func (g *ghVerify) checkRemote(ctx context.Context, wt string) error {
	origin, err := g.git(ctx, wt, "remote", "get-url", "--push", "--all", "origin")
	if err != nil {
		return fmt.Errorf("origin's push URLs could not be read: %w", err)
	}
	urls := strings.Fields(string(origin))
	if len(urls) == 0 {
		return fmt.Errorf("origin has no push URL for the configured repository %s: nothing was pushed", g.repo)
	}
	for _, url := range urls {
		if slug := githubSlug(url); slug == "" || !strings.EqualFold(slug, g.repo) {
			return fmt.Errorf("origin's push URLs are not all the configured repository %s: nothing was pushed", g.repo)
		}
	}
	return nil
}

// githubRepositoryPattern is OWNER/REPO of GitHub identifiers.
var githubRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// githubSlug is owner/repo of a GitHub remote URL: https://github.com/o/r(.git), git@github.com:o/r(.git),
// ssh://git@github.com/o/r(.git). Anything with userinfo, a password, a query or a fragment, and any other
// spelling, is no GitHub remote of the configured repository.
func githubSlug(origin string) string {
	origin = strings.TrimSpace(origin)
	var repoPath string
	switch {
	case strings.HasPrefix(origin, "git@github.com:"):
		repoPath = strings.TrimPrefix(origin, "git@github.com:")
		if strings.ContainsAny(repoPath, "?#@") {
			return ""
		}
	case strings.Contains(origin, "://"):
		u, err := url.Parse(origin)
		if err != nil || u.RawQuery != "" || u.Fragment != "" ||
			(u.Scheme != "https" && u.Scheme != "ssh") || !strings.EqualFold(u.Hostname(), "github.com") {
			return ""
		}
		// https carries no userinfo at all; ssh carries the required user "git", never a password
		if u.Scheme == "https" && u.User != nil {
			return ""
		}
		if u.Scheme == "ssh" && u.User != nil && (u.User.Username() != "git" || u.User.String() != "git") {
			return ""
		}
		repoPath = strings.TrimPrefix(u.Path, "/")
	default:
		return ""
	}
	repoPath = strings.TrimSuffix(strings.Trim(repoPath, "/"), ".git")
	if !githubRepositoryPattern.MatchString(repoPath) {
		return ""
	}
	return repoPath
}

// writeCache writes the record into the repository-local cache through the os.Root of the common directory,
// owner-only, through a temporary file renamed over the target. A component that is unsafe to write through
// is a refusal — the caller turns it into a note, never a lost receipt.
func (g *ghVerify) writeCache(commonDir, tree, key string, raw []byte) error {
	root, err := os.OpenRoot(commonDir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, dir := range []string{"crw-ci", "crw-ci/github-actions", "crw-ci/github-actions/" + tree} {
		if info, err := root.Lstat(dir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return fmt.Errorf("the verification cache component %s is not a directory", dir)
		}
		if err := root.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	entry := "crw-ci/github-actions/" + tree + "/" + key + ".json"
	tmp := entry + ".tmp"
	file, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		file.Close()
		root.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		root.Remove(tmp)
		return err
	}
	return root.Rename(tmp, entry)
}

// writeRecordAtomic writes raw to path through a temporary file in its directory, renamed over it.
func writeRecordAtomic(path string, raw []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".record-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// recordFailures are the names of the record's jobs that did not pass.
func recordFailures(rec dagsched.VerificationRecord) []string {
	var out []string
	for _, raw := range rec.Jobs {
		var job struct {
			Name   string `json:"name"`
			Result string `json:"result"`
		}
		if json.Unmarshal(raw, &job) == nil && job.Result != "passed" {
			out = append(out, job.Name+": "+job.Result)
		}
	}
	return out
}

// recordFailedTests are the failing lines the record's jobs carry.
func recordFailedTests(rec dagsched.VerificationRecord) []FailedStep {
	var out []FailedStep
	if rec.GitHub != nil {
		for _, attempt := range rec.GitHub.Runs {
			for _, raw := range attempt.Jobs {
				var job ghJobRecord
				if json.Unmarshal(raw, &job) == nil {
					out = append(out, job.Failures...)
				}
			}
		}
	}
	return out
}

// slicesContains is strings.Contains's list form, local so the file needs no slices import for one call.
func slicesContains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
