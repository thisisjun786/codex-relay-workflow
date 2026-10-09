package manage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// The pre-merge evaluation is tested against real parts wherever a real part can be made without the
// host: a temporary git remote that carries dev and refs/pull/<N>/head, a checkout cloned from it, a
// real relay store with a plan, and a fake grader script. Only gh and the relay command line are
// replaced, through the seams the pull request audit already has.

const (
	premergeTestPR     = 7
	premergeTestIssue  = "CRW-953"
	premergeTestNode   = "n-953"
	premergeTestScrub  = "SECRETMODEL"
	premergeTestGrader = "model-x"
	premergeTestEffort = "high"
)

// premergeTestNow is the clock every test evaluation reads.
func premergeTestNow() time.Time { return time.Date(2026, 10, 9, 1, 2, 3, 0, time.UTC) }

// premergeTestBase is the file set of the shared base commit.
var premergeTestBase = map[string]string{
	"README.md":                             "readme\n",
	"a.go":                                  "package a\n\nvar A = 1\n",
	"docs/old.md":                           "old\n",
	"plugins/crw/.codex-plugin/plugin.json": "{\n  \"name\": \"crw\",\n  \"version\": \"0.4.0+aaaaaaaaaaaa\"\n}\n",
}

// premergeTestRepoSpec says what the pull request branch and dev change after the shared base.
// A value of "-" deletes the path; a value starting with "link:" makes a symbolic link.
type premergeTestRepoSpec struct {
	prFiles  map[string]string
	devFiles map[string]string
}

// premergeTestRepo is the fixture repositories: the checkout the product reads and the two commits.
type premergeTestRepo struct {
	checkout string
	remote   string
	head     string
	dev      string
}

// premergeTestGit runs one git command with its own configuration and identity, so the fixture
// neither reads nor writes the operator's git configuration.
func premergeTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
		"LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func premergeTestWrite(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		switch {
		case body == "-":
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		case strings.HasPrefix(body, "link:"):
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(strings.TrimPrefix(body, "link:"), path); err != nil {
				t.Fatal(err)
			}
		default:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// premergeTestNewRepo builds the remote with dev and refs/pull/7/head, and a checkout of it. The pull
// request branch is cut from the base; dev then moves on, so the merge is a real one.
func premergeTestNewRepo(t *testing.T, spec premergeTestRepoSpec) premergeTestRepo {
	t.Helper()
	root := t.TempDir()
	work, remote, checkout := filepath.Join(root, "work"), filepath.Join(root, "remote.git"), filepath.Join(root, "checkout")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	premergeTestGit(t, work, "init", "--quiet", "-b", "dev")
	premergeTestWrite(t, work, premergeTestBase)
	premergeTestGit(t, work, "add", "-A")
	premergeTestGit(t, work, "commit", "--quiet", "-m", "base")
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatal(err)
	}
	premergeTestGit(t, remote, "init", "--quiet", "--bare", "-b", "dev")
	premergeTestGit(t, work, "remote", "add", "origin", remote)
	premergeTestGit(t, work, "push", "--quiet", "origin", "dev")
	premergeTestGit(t, work, "checkout", "--quiet", "-b", "pr")
	premergeTestWrite(t, work, spec.prFiles)
	premergeTestGit(t, work, "add", "-A")
	premergeTestGit(t, work, "commit", "--quiet", "-m", "the change")
	head := premergeTestGit(t, work, "rev-parse", "HEAD")
	premergeTestGit(t, work, "push", "--quiet", "origin", "pr:refs/pull/"+"7"+"/head")
	premergeTestGit(t, work, "checkout", "--quiet", "dev")
	premergeTestWrite(t, work, spec.devFiles)
	premergeTestGit(t, work, "add", "-A")
	premergeTestGit(t, work, "commit", "--quiet", "--allow-empty", "-m", "dev moves on")
	dev := premergeTestGit(t, work, "rev-parse", "HEAD")
	premergeTestGit(t, work, "push", "--quiet", "origin", "dev")
	premergeTestGit(t, root, "clone", "--quiet", remote, checkout)
	return premergeTestRepo{checkout: checkout, remote: remote, head: head, dev: dev}
}

// premergeTestCleanSpec is a pull request that adds a file and edits a.go, against a dev that added
// a file of its own: the merge is clean.
func premergeTestCleanSpec() premergeTestRepoSpec {
	return premergeTestRepoSpec{
		prFiles:  map[string]string{"a.go": "package a\n\nvar A = 2\n", "feature.go": "package a\n\nvar Feature = true\n"},
		devFiles: map[string]string{"dev_only.go": "package a\n\nvar DevOnly = 1\n"},
	}
}

// premergeTestNodeSpec is one plan node.
type premergeTestNodeSpec struct{ id, issue, kind string }

// premergeTestNodeDigest is the criteria digest the plan fixes for a node.
func premergeTestNodeDigest(node string) string {
	sum := sha256.Sum256([]byte("criteria " + node))
	return hex.EncodeToString(sum[:])
}

// premergeTestPlan registers a real plan revision through dag.Repo.Put.
func premergeTestPlan(t *testing.T, f *checkpointFixture, planID, project string, revision int, changes ...map[string]any) {
	t.Helper()
	list := make([]any, len(changes))
	for i, change := range changes {
		list[i] = change
	}
	document := map[string]any{
		"schema": "dag-plan-revision/1", "plan_id": planID, "project_key": project,
		"request_id": planID + "-request-" + string(rune('0'+revision)), "expected_parent_revision": revision - 1,
		"author_task_id": "parent", "changes": list,
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := dag.DecodeRevision(raw)
	if err != nil {
		t.Fatalf("decode the plan revision: %v", err)
	}
	repo := &dag.Repo{Store: f.store, Now: func() string { return checkpointAt(0) }}
	if _, err := repo.Put(context.Background(), decoded); err != nil {
		t.Fatalf("put the plan revision: %v", err)
	}
}

func premergeTestAddNode(spec premergeTestNodeSpec) map[string]any {
	return map[string]any{"op": "add_node", "node": map[string]any{
		"node_id": spec.id, "issue_key": spec.issue, "kind": spec.kind, "criteria_set_digest": premergeTestNodeDigest(spec.id),
	}}
}

// premergeTestGraderScript is the fake grader. It runs in the bundle and writes what the test says.
const premergeTestGraderScript = "#!/bin/sh\n" +
	"if [ -n \"$PREMERGE_RAN\" ]; then echo ran >> \"$PREMERGE_RAN\"; fi\n" +
	"case \"$PREMERGE_FAKE\" in\n" +
	"json) printf '%s' \"$PREMERGE_JSON\" > grade.json ;;\n" +
	"timeout) sleep 30 ;;\n" +
	"nofile) ;;\n" +
	"esac\n" +
	"exit 0\n"

// premergeTestGrade is a grade.json with a partial criterion and two defects.
const premergeTestGrade = `{"criteria":{"c1":{"verdict":"PASS","evidence":"a.go:3"},"c2":{"verdict":"PARTIAL","evidence":"feature.go:3"}},` +
	`"paths":["the stop path"],"defects":[` +
	`{"severity":"P1","impact":"criterion_unmet","introduced":true,"in_promise":true,"what":"first","trigger":"run it","where":"a.go:3"},` +
	`{"severity":"P3","impact":"minor_separable","introduced":false,"in_promise":false,"what":"second"}],` +
	`"score":6,"summary":"needs one more round"}`

// premergeTestCleanGrade is a grade.json with nothing to dispose of.
const premergeTestCleanGrade = `{"criteria":{"c1":{"verdict":"PASS","evidence":"a.go:3"},"c2":{"verdict":"PASS","evidence":"feature.go:3"}},` +
	`"paths":[],"defects":[],"score":9,"summary":"clean"}`

// premergeTestFixture is everything one evaluation test runs against.
type premergeTestFixture struct {
	t       *testing.T
	repo    premergeTestRepo
	e       *Env
	out     *strings.Builder
	errOut  *strings.Builder
	cfg     *Config
	state   string
	bundles string
	records string
	ran     string
	gh      *[][]string
	relay   *[][]string
}

// premergeTestOptions changes what the fixture builds.
type premergeTestOptions struct {
	spec         *premergeTestRepoSpec
	nodes        []premergeTestNodeSpec
	title        string
	body         string
	prState      string
	criteria     string
	audit        map[string]any
	premerge     map[string]any
	noAssignment bool // the issue has no assignment, so no relationship
	changes      [][]map[string]any
}

// premergeTestCriteriaJSON is a criteria-show answer with two criteria.
const premergeTestCriteriaJSON = `{"relationshipId":"rel-1","criteria":[{"id":"c1","title":"the first thing","required":true},` +
	`{"id":"c2","title":"the second thing","required":false}],"setDigest":"d"}`

func premergeTestNew(t *testing.T, opts premergeTestOptions) *premergeTestFixture {
	t.Helper()
	spec := premergeTestCleanSpec()
	if opts.spec != nil {
		spec = *opts.spec
	}
	repo := premergeTestNewRepo(t, spec)
	nodes := opts.nodes
	if nodes == nil {
		nodes = []premergeTestNodeSpec{{premergeTestNode, premergeTestIssue, "implementation"}}
	}
	store := checkpointNewFixture(t)
	adds := make([]map[string]any, len(nodes))
	for i, node := range nodes {
		adds[i] = premergeTestAddNode(node)
	}
	premergeTestPlan(t, store, "plan-1", "project-1", 1, adds...)
	for i, change := range opts.changes {
		premergeTestPlan(t, store, "plan-1", "project-1", i+2, change...)
	}
	store.close()

	graderPath := filepath.Join(t.TempDir(), "grader.sh")
	syscall.ForkLock.RLock()
	writeErr := os.WriteFile(graderPath, []byte(premergeTestGraderScript), 0o700)
	syscall.ForkLock.RUnlock()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	f := &premergeTestFixture{t: t, repo: repo, state: t.TempDir(), bundles: filepath.Join(t.TempDir(), "bundles"), records: filepath.Join(t.TempDir(), "records")}
	f.ran = filepath.Join(t.TempDir(), "ran")
	t.Setenv("PREMERGE_RAN", f.ran)
	f.setGrade("json", premergeTestGrade)

	audit := map[string]any{"grader": []string{graderPath, "{prompt_file}", "{bundle}"}, "grader_timeout_seconds": 20, "scrub": []string{premergeTestScrub}}
	for k, v := range opts.audit {
		audit[k] = v
	}
	premerge := map[string]any{"bundle_dir": f.bundles, "record_dir": f.records, "grader_model": premergeTestGrader, "grader_effort": premergeTestEffort}
	for k, v := range opts.premerge {
		if v == nil {
			delete(premerge, k)
			continue
		}
		premerge[k] = v
	}
	f.cfg = &Config{StateDir: f.state, Repository: "example/repository", Parents: map[string]string{"thread-1": "parent-one"},
		Relay: coreRelay{State: store.dir}, raw: map[string]json.RawMessage{}}
	for name, section := range map[string]any{
		"audit": audit, "premerge": premerge,
		"checkout": map[string]any{"repository": repo.checkout, "base_ref": "origin/dev"},
	} {
		data, err := json.Marshal(section)
		if err != nil {
			t.Fatal(err)
		}
		f.cfg.raw[name] = data
	}
	f.e, f.out, f.errOut = auditTestEnv(t)
	f.e.Now = premergeTestNow

	title, body, state := opts.title, opts.body, opts.prState
	if title == "" {
		title = premergeTestIssue + ": the pre-merge evaluation"
	}
	if body == "" {
		body = "the body"
	}
	if state == "" {
		state = "OPEN"
	}
	f.gh = premergeTestFakeGh(t, map[int]premergeTestPull{premergeTestPR: {title: title, body: body, head: repo.head, state: state}})
	criteria := opts.criteria
	if criteria == "" {
		criteria = premergeTestCriteriaJSON
	}
	assignments := map[string]string{premergeTestIssue: auditPRAssignmentJSON(t, "rel-1", "child-1")}
	if opts.noAssignment {
		assignments = map[string]string{premergeTestIssue: `{"assignments":[]}`}
	}
	f.relay = auditPRFakeRelay(t, assignments, map[string]string{"child-1": auditPRSettingsJSON(t, "model-z")}, map[string]string{"rel-1": criteria})
	return f
}

func (f *premergeTestFixture) setGrade(mode, grade string) {
	f.t.Helper()
	f.t.Setenv("PREMERGE_FAKE", mode)
	f.t.Setenv("PREMERGE_JSON", grade)
}

func (f *premergeTestFixture) graderRuns() int {
	f.t.Helper()
	data, err := os.ReadFile(f.ran)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.Count(string(data), "ran\n")
}

func (f *premergeTestFixture) eval(opts PremergeEvalOptions) (PremergeEvalResult, error) {
	f.t.Helper()
	if opts.PR == 0 {
		opts.PR = premergeTestPR
	}
	return PremergeEval(context.Background(), f.e, f.cfg, opts)
}

func (f *premergeTestFixture) bundle() string {
	return filepath.Join(f.bundles, "pr-7-"+f.repo.head[:8])
}

func (f *premergeTestFixture) recordPath() string {
	return filepath.Join(f.records, "pr7-"+f.repo.head[:8]+".json")
}

// premergeTestPull is what the fake gh answers for one pull request.
type premergeTestPull struct{ title, body, head, state string }

// premergeTestFakeGh answers `gh pr view`, the one gh call the evaluation makes.
func premergeTestFakeGh(t *testing.T, prs map[int]premergeTestPull) *[][]string {
	t.Helper()
	previous := auditPRGh
	calls := &[][]string{}
	auditPRGh = func(_ context.Context, args ...string) ([]byte, error) {
		*calls = append(*calls, args)
		if len(args) >= 3 && args[0] == "pr" && args[1] == "view" {
			for number, pr := range prs {
				if args[2] == itoaForTest(number) {
					return json.Marshal(map[string]any{"number": number, "title": pr.title, "body": pr.body, "headRefOid": pr.head, "state": pr.state})
				}
			}
		}
		return nil, errors.New("unexpected gh arguments: " + strings.Join(args, " "))
	}
	t.Cleanup(func() { auditPRGh = previous })
	return calls
}

func itoaForTest(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func premergeTestReadRecord(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return doc
}

// premergeTestErr asserts err is a *PremergeError with the exit and reason.
func premergeTestErr(t *testing.T, err error, exit int, reason string) *PremergeError {
	t.Helper()
	var pe *PremergeError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a PremergeError %d %s", err, exit, reason)
	}
	if pe.Exit != exit || pe.Reason != reason {
		t.Fatalf("error = %d %s (%s), want %d %s", pe.Exit, pe.Reason, pe.Detail, exit, reason)
	}
	return pe
}

func premergeTestNoRecords(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, entry := range entries {
			names[i] = entry.Name()
		}
		t.Errorf("%s holds %v, want nothing", dir, names)
	}
}
