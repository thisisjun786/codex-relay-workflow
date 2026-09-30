package contracttest

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The release workflow's step grammar: jobs are two-space headers, steps with an id start
// `      - id: <id>`, and a step's shell is one literal `run: |` block (release_steps.py read it
// the same way, deliberately without a YAML parser).
var (
	releaseJob      = regexp.MustCompile(`(?m)^  ([a-z][a-z0-9-]*):\n`)
	releaseStep     = regexp.MustCompile(`(?m)^      - id: ([a-z][a-z0-9-]+)\n`)
	releaseRun      = regexp.MustCompile(`(?m)^        run: \|\n((?:          .*\n|\n)*)`)
	releaseNextItem = regexp.MustCompile(`(?m)^      - `)
	releaseIndent   = regexp.MustCompile(`(?m)^          `)
)

// releaseJobBody is the text of the one job named job, up to the next job.
func releaseJobBody(workflow, job string) (string, error) {
	jobs := releaseJob.FindAllStringSubmatchIndex(workflow, -1)
	var body string
	found := 0
	for i, match := range jobs {
		if workflow[match[2]:match[3]] != job {
			continue
		}
		found++
		end := len(workflow)
		if i+1 < len(jobs) {
			end = jobs[i+1][0]
		}
		body = workflow[match[1]:end]
	}
	if found != 1 {
		return "", fmt.Errorf("expected one %s job, found %d", job, found)
	}
	return body, nil
}

// releaseStepIDs is the ids of a job's steps in workflow order.
func releaseStepIDs(workflow, job string) ([]string, error) {
	body, err := releaseJobBody(workflow, job)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, match := range releaseStep.FindAllStringSubmatch(body, -1) {
		ids = append(ids, match[1])
	}
	return ids, nil
}

// releaseStepBlock is the named step of the named job: its metadata (the keys before `run:`)
// and its one literal shell block, dedented.
func releaseStepBlock(workflow, job, step string) (metadata, script string, err error) {
	body, err := releaseJobBody(workflow, job)
	if err != nil {
		return "", "", err
	}
	steps := releaseStep.FindAllStringSubmatchIndex(body, -1)
	start, end, found := 0, len(body), 0
	for i, match := range steps {
		if body[match[2]:match[3]] != step {
			continue
		}
		found++
		start = match[1]
		end = len(body)
		if i+1 < len(steps) {
			end = steps[i+1][0]
		}
	}
	if found != 1 {
		return "", "", fmt.Errorf("expected one %s step, found %d", step, found)
	}
	// A following step may omit an id: stop at the next step list item too.
	if next := releaseNextItem.FindStringIndex(body[start:]); next != nil && start+next[0] < end {
		end = start + next[0]
	}
	section := body[start:end]
	run := releaseRun.FindStringSubmatchIndex(section)
	if run == nil || strings.TrimSpace(section[run[1]:]) != "" {
		return "", "", fmt.Errorf("expected one literal shell block on %s", step)
	}
	return section[:run[0]], releaseIndent.ReplaceAllString(section[run[2]:run[3]], ""), nil
}

// releaseRepo is the world a release step runs in: a real bare remote whose main is base and
// whose dev is candidate one commit later, a clone of it on dev, and testdata/release's fake gh
// and git first on PATH. The fakes read their scripted state one file per key,
// state/<fake>/<key>, with cat, so no JSON parser (and no Python) is needed; gh.log collects
// every gh invocation and state/created.txt the one release the fake created.
type releaseRepo struct {
	dir, remote, checkout, state, log string
	workflow                          string
	base, candidate                   string
	env                               map[string]string
}

// releaseDefaults is the fakes' state before a case changes it: the latest dev-push CI run
// succeeded, and no tag or release exists yet.
var releaseDefaults = map[string]map[string]any{"ci": {"case": "success"}, "tag": {"case": "missing"}, "release": {"case": "missing"}, "push": {"fail_main": false, "lie_main": false}}

func newReleaseRepo(t *testing.T) (*releaseRepo, error) {
	root, err := Root()
	if err != nil {
		return nil, err
	}
	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"bash", "git", "jq"} {
		if _, err := exec.LookPath(name); err != nil {
			return nil, fmt.Errorf("the release workflow's steps need %s: %w", name, err)
		}
	}
	gitReal, _ := exec.LookPath("git")
	dir := t.TempDir()
	r := &releaseRepo{dir: dir, remote: filepath.Join(dir, "remote.git"), checkout: filepath.Join(dir, "checkout"),
		state: filepath.Join(dir, "state"), log: filepath.Join(dir, "gh.log"), workflow: string(workflow)}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return nil, err
	}
	for _, name := range []string{"gh", "git"} {
		raw, err := os.ReadFile(filepath.Join(root, "internal", "contracttest", "testdata", "release", "fake_"+name+".sh"))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(bin, name), raw, 0o755); err != nil {
			return nil, err
		}
	}
	for _, name := range []string{"gh.log", "summary"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			return nil, err
		}
	}
	for name, values := range releaseDefaults {
		if err := r.set(name, values); err != nil {
			return nil, err
		}
	}
	r.env = map[string]string{"HOME": dir, "XDG_CONFIG_HOME": dir,
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null",
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.hooksPath", "GIT_CONFIG_VALUE_0": "/dev/null",
		"GIT_AUTHOR_NAME": "Release Fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
		"GIT_COMMITTER_NAME": "Release Fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid",
		"PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH"), "GH_LOG": r.log, "GH_STATE": r.state,
		"GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary"), "GITHUB_REPOSITORY": "fixture/repository", "GITHUB_REF": "refs/heads/dev",
		"ACTOR": "owner", "TRIGGERING_ACTOR": "owner", "OWNER": "owner",
		"RELEASE_SHA": "", "RELEASE_TAG": "v0.1.0", "RELEASE_NOTES": "Fixture notes", "RELEASE_TOKEN": "fixture-only", "GH_TOKEN": "fixture-read",
		"GIT_REAL": gitReal}
	if _, err := r.command(dir, "git", "init", "--bare", "--initial-branch=main", r.remote); err != nil {
		return nil, err
	}
	if _, err := r.command(dir, "git", "clone", r.remote, r.checkout); err != nil {
		return nil, err
	}
	for _, args := range [][]string{{"commit", "--allow-empty", "-m", "base"}, {"push", "origin", "main"}, {"switch", "-c", "dev"}, {"commit", "--allow-empty", "-m", "candidate"}} {
		if _, err := r.git(args...); err != nil {
			return nil, err
		}
		if args[0] == "commit" && r.base == "" {
			if r.base, err = r.git("rev-parse", "HEAD"); err != nil {
				return nil, err
			}
		}
	}
	if r.candidate, err = r.git("rev-parse", "HEAD"); err != nil {
		return nil, err
	}
	if _, err := r.git("push", "origin", "dev"); err != nil {
		return nil, err
	}
	r.env["RELEASE_SHA"] = r.candidate
	return r, nil
}

// environ is this process's environment with the fixture's variables and extra over it.
func (r *releaseRepo) environ(extra map[string]string) []string {
	all := map[string]string{}
	for _, kv := range os.Environ() {
		if key, value, ok := strings.Cut(kv, "="); ok {
			all[key] = value
		}
	}
	maps.Copy(all, r.env)
	maps.Copy(all, extra)
	out := make([]string, 0, len(all))
	for key, value := range all {
		out = append(out, key+"="+value)
	}
	return out
}

// command runs args in cwd under the fixture's environment and returns its trimmed stdout; a
// non-zero exit is an error naming its stderr.
func (r *releaseRepo) command(cwd string, args ...string) (string, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir, cmd.Env = cwd, r.environ(nil)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%v: %w: %s", args, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// git runs git in the checkout.
func (r *releaseRepo) git(args ...string) (string, error) {
	return r.command(r.checkout, append([]string{"git"}, args...)...)
}

// remoteRef is what the bare remote's ref resolves to, and whether it resolves at all.
func (r *releaseRepo) remoteRef(ref string) (string, bool) {
	sha, err := r.command(r.dir, "git", "--git-dir", r.remote, "rev-parse", "--verify", "--quiet", ref)
	return sha, err == nil
}

// set merges values into one fake's state, one file per key holding what the fake prints
// for it: true/false for booleans, integers without a fraction.
func (r *releaseRepo) set(fake string, values map[string]any) error {
	dir := filepath.Join(r.state, fake)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for key, value := range values {
		var text string
		switch v := value.(type) {
		case nil:
			text = "None"
		case bool:
			text = strconv.FormatBool(v)
		case int:
			text = strconv.Itoa(v)
		case float64:
			text = strconv.FormatFloat(v, 'f', -1, 64)
		case string:
			text = v
		default:
			return fmt.Errorf("%w: release state %s is %T", ErrFixture, key, value)
		}
		if err := os.WriteFile(filepath.Join(dir, key), []byte(text), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// releaseOutcome is one step's run: its exit status and output.
type releaseOutcome struct {
	exit           int
	stdout, stderr string
}

// run is the named step's shell block run by bash in the checkout, with extra over the
// fixture's environment.
func (r *releaseRepo) run(job, step string, extra map[string]string) (releaseOutcome, error) {
	_, script, err := releaseStepBlock(r.workflow, job, step)
	if err != nil {
		return releaseOutcome{}, fmt.Errorf("%w: %v", ErrFixture, err)
	}
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir, cmd.Env = r.checkout, r.environ(extra)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	outcome := releaseOutcome{}
	if err := cmd.Run(); err != nil {
		var exited *exec.ExitError
		if !errors.As(err, &exited) {
			return outcome, err
		}
		outcome.exit = exited.ExitCode()
	}
	outcome.stdout, outcome.stderr = stdout.String(), stderr.String()
	return outcome, nil
}

// runRelease is the corpus's `release` run kind: the named workflow step's shell block in a
// fresh releaseRepo, given.{ci,tag,release,push} merged over releaseDefaults and run.env over
// the fixture's environment. calls are the gh invocations and remote_main the remote's main after.
func runRelease(t *testing.T, s Scenario) (map[string]any, error) {
	r, err := newReleaseRepo(t)
	if err != nil {
		return nil, err
	}
	for name := range releaseDefaults {
		if given, ok := s.Given[name].(map[string]any); ok {
			if err := r.set(name, given); err != nil {
				return nil, err
			}
		}
	}
	job, _ := s.Run["job"].(string)
	step, _ := s.Run["step"].(string)
	extra := map[string]string{}
	if env, ok := s.Run["env"].(map[string]any); ok {
		for key, value := range env {
			extra[key] = fmt.Sprint(value)
		}
	}
	outcome, err := r.run(job, step, extra)
	if err != nil {
		return nil, err
	}
	log, err := os.ReadFile(r.log)
	if err != nil {
		return nil, err
	}
	calls := []any{}
	for _, line := range strings.Split(strings.TrimSuffix(string(log), "\n"), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	main, _ := r.remoteRef("main")
	return map[string]any{"exit": float64(outcome.exit), "stdout": outcome.stdout, "stderr": outcome.stderr, "calls": calls, "remote_main": main}, nil
}
