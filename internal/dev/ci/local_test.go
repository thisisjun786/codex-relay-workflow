//go:build dev

package ci

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-964: the engine, the verification record, reuse and the failure rule. Every test runs in a
// temporary repository with fake tools, never against real host state.

// localFixture is a scratch repository with one commit.
type localFixture struct {
	*fixtureRepo
	// work is the work root this fixture's runs make their clean worktrees in: outside the temporary
	// directories, and removed when the test ends.
	work string
}

// localTestWorkRoot is a work root for a test: a directory of its own, which the test owns and removes.
// The engine tests pass allowTempRoot, so the root may sit under TMPDIR; the refusal itself is tested apart.
func localTestWorkRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// newLocalFixture makes a repository whose HEAD carries the files the run reads: a workflow, a
// go.mod, a scripts/ci/secrets.sh, a go.sum and a web/package-lock.json.
func newLocalFixture(t *testing.T) *localFixture {
	t.Helper()
	repo := &localFixture{fixtureRepo: newRepo(t), work: localTestWorkRoot(t)}
	repo.write(".github/workflows/ci.yml", localFixtureWorkflow)
	repo.write("go.mod", "module fixture\n\ngo 1.27\n\ntoolchain go1.27.1\n\nrequire (\n\thonx.dev/x v1.0.0\n\thonnef.co/go/tools v0.8.1\n)\n")
	repo.write("go.sum", "example.com/x v1.0.0 h1:fixture\n")
	repo.write("web/package-lock.json", "{\"lockfileVersion\": 3}\n")
	repo.write("scripts/ci/secrets.sh", "#!/usr/bin/env bash\nscan_version=8.30.1\n")

	repo.write("file.txt", "one\n")
	repo.commit()
	return repo
}

// localFixtureWorkflow is a small ci.yml with one job and two steps, so the engine can be driven
// without the real workflow's cost.
const localFixtureWorkflow = `name: fixture

on:
  push:

jobs:
  validate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@0000000000000000000000000000000000000000 # v4
      - uses: actions/setup-node@0000000000000000000000000000000000000000 # v4
        with:
          node-version: '24.20.0'
      - name: Say hello
        run: echo hello
      - name: Node pinned
        run: echo node
`

// localFixturePlan is the table for localFixtureWorkflow: a checkout and one run step.
func localFixturePlan(command string) []localJob {
	return []localJob{{
		name: "validate",
		steps: []localStep{
			{kind: localAction, action: localCheckout, scope: "full"},
			{name: "Say hello", kind: localRun, command: command, scope: "full", heavy: true},
		},
	}}
}

// localRunOptions is a run over the fixture, with the given table.
func localRunOptions(repo *localFixture, plan []localJob, record string) localOptions {
	return localOptions{
		Root:          repo.root,
		Commit:        "HEAD",
		Record:        record,
		Runner:        "local",
		Plan:          plan,
		WorkRoot:      repo.work,
		allowTempRoot: true,
	}
}

// C2: a step that fails fails the whole run, and the record names the step and its reason.
func TestLocal_a_failed_step_fails_the_whole_run_and_is_recorded(t *testing.T) {
	repo := newLocalFixture(t)
	record := filepath.Join(t.TempDir(), "record.json")
	opts := localRunOptions(repo, localFixturePlan("echo hello && exit 3"), record)
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localFail {
		t.Errorf("result = %q, want %q", made.Result, localFail)
	}
	step := made.Jobs[0].Steps[1]
	if step.Result != localFailed {
		t.Errorf("the failing step is %q, want %q", step.Result, localFailed)
	}
	if !strings.Contains(step.Reason, "exit status 3") {
		t.Errorf("the failure's reason is %q, want the exit status", step.Reason)
	}
}

// C2: a step that does not run is a failure, not a pass. GitHub stops a job's later steps after a
// failure; the local run records them as skipped and the run fails.
func TestLocal_a_skipped_step_fails_the_whole_run(t *testing.T) {
	repo := newLocalFixture(t)
	plan := []localJob{{
		name: "validate",
		steps: []localStep{
			{kind: localAction, action: localCheckout, scope: "full"},
			{name: "First", kind: localRun, command: "exit 1", scope: "full"},
			{name: "Second", kind: localRun, command: "echo second", scope: "full"},
		},
	}}
	made, _, err := localVerify(localRunOptions(repo, plan, filepath.Join(t.TempDir(), "r.json")), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localFail {
		t.Errorf("result = %q, want %q", made.Result, localFail)
	}
	if got := made.Jobs[0].Steps[2].Result; got != localSkipped {
		t.Errorf("the step after a failure is %q, want %q", got, localSkipped)
	}
}

// C2: a step whose tool is not on PATH fails the run and says so.
func TestLocal_a_missing_tool_fails_the_whole_run(t *testing.T) {
	repo := newLocalFixture(t)
	plan := []localJob{{
		name: "validate",
		steps: []localStep{
			{kind: localAction, action: localCheckout, scope: "full"},
			{name: "Needs a tool that does not exist", kind: localRun, command: "no-such-tool --version",
				tool: "crw-964-no-such-tool", scope: "full"},
		},
	}}
	made, _, err := localVerify(localRunOptions(repo, plan, filepath.Join(t.TempDir(), "r.json")), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localFail {
		t.Errorf("result = %q, want %q", made.Result, localFail)
	}
	step := made.Jobs[0].Steps[1]
	if step.Result != localMissingTool {
		t.Errorf("the step is %q, want %q", step.Result, localMissingTool)
	}
	if !strings.Contains(step.Reason, "not on PATH") {
		t.Errorf("the reason is %q, want it to name PATH", step.Reason)
	}
}

// C6: the digest is the sha256 of the canonical serialization, and it is stable.
func TestLocalRecord_digest_is_the_canonical_serialization(t *testing.T) {
	record := verificationRecord{
		Schema: recordSchema, Runner: "local", Repository: "r", HeadCommit: "abc", TreeHash: "def",
		CiDigest: "sha256:x", Tools: map[string]string{"go": "1.27.1"},
		Pins: map[string]string{"go": "1.27.1"}, Dependencies: map[string]string{"go.sum": "sha256:y"},
		OS: "linux", Arch: "amd64", Result: localPass,
	}
	sealed, err := sealRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sealed.Digest, "sha256:") {
		t.Fatalf("digest = %q, want a sha256", sealed.Digest)
	}
	canonical, err := canonicalRecord(sealed)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(canonical, &fields); err != nil {
		t.Fatalf("the canonical serialization is not JSON: %v", err)
	}
	if _, ok := fields["digest"]; ok {
		t.Error("the canonical object still has a digest key")
	}
	again, err := recordDigest(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if again != sealed.Digest {
		t.Errorf("the digest is not stable: %s then %s", sealed.Digest, again)
	}
}

// C6: the record carries every field the decided answers require.
func TestLocalRecord_carries_every_required_field(t *testing.T) {
	repo := newLocalFixture(t)
	record := filepath.Join(t.TempDir(), "record.json")
	opts := localRunOptions(repo, localFixturePlan("echo hello"), record)
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := writeRecord(record, made)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema", "runner", "repository", "baseCommit", "headCommit", "treeHash",
		"ciDigest", "tools", "pins", "pinMismatch", "dependencies", "os", "arch", "result", "jobs", "digest"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("the record has no %q", key)
		}
	}
	if sealed.Schema != recordSchema {
		t.Errorf("schema = %q, want %q", sealed.Schema, recordSchema)
	}
	for _, tool := range localToolNames {
		if _, ok := sealed.Tools[tool]; !ok {
			t.Errorf("the record does not name the %s version", tool)
		}
		if _, ok := sealed.Pins[tool]; !ok {
			t.Errorf("the record does not name the %s pin", tool)
		}
	}
	if len(sealed.Jobs) != 1 || len(sealed.Jobs[0].Steps) != 2 {
		t.Fatalf("jobs = %#v, want one job of two steps", sealed.Jobs)
	}
	if sealed.Jobs[0].Steps[1].Seconds < 0 {
		t.Error("a step carries a negative duration")
	}
	if sealed.Jobs[0].Steps[1].Scope != "full" {
		t.Errorf("the step's scope = %q, want full", sealed.Jobs[0].Steps[1].Scope)
	}
	// Reading it back checks the digest it carries.
	if _, err := readRecord(record); err != nil {
		t.Errorf("the written record does not read back: %v", err)
	}
	// A tampered record is refused.
	tampered := strings.Replace(string(data), "\"result\": \"pass\"", "\"result\": \"fail\"", 1)
	if tampered == string(data) {
		t.Fatal("the fixture record does not carry a result to tamper with")
	}
	path := filepath.Join(t.TempDir(), "tampered.json")
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readRecord(path); err == nil {
		t.Error("a record whose contents no longer match its digest is accepted")
	}
}

// C3: a record answers a second run when every key matches.
func TestLocalReuse_every_key_matches_answers_the_record(t *testing.T) {
	base := verificationRecord{
		Schema: recordSchema, Result: localPass, TreeHash: "tree", CiDigest: "ci", OS: "linux", Arch: "amd64", BaseCommit: "base",
		Tools: map[string]string{"go": "1.27.1", "node": "24.20.0"}, Dependencies: map[string]string{"go.sum": "sum"},
	}
	same := verificationRecord{
		TreeHash: "tree", CiDigest: "ci", OS: "linux", Arch: "amd64", BaseCommit: "base",
		Tools: map[string]string{"go": "1.27.1", "node": "24.20.0"}, Dependencies: map[string]string{"go.sum": "sum"},
	}
	if ok, why := localReuse(base, same); !ok {
		t.Errorf("the same keys are refused: %s", why)
	}
}

// C3: changing one key at a time re-runs. The head commit alone is deliberately not a key, so two
// records with different commits and the same tree still match.
func TestLocalReuse_one_changed_key_reruns(t *testing.T) {
	fresh := func() verificationRecord {
		return verificationRecord{
			Schema: recordSchema, Result: localPass, TreeHash: "tree", CiDigest: "ci", OS: "linux", Arch: "amd64",
			BaseCommit:   "base",
			Tools:        map[string]string{"go": "1.27.1", "node": "24.20.0"},
			Dependencies: map[string]string{"go.sum": "sum", "web/package-lock.json": "lock"},
		}
	}
	for _, row := range []struct {
		name  string
		apply func(*verificationRecord)
	}{
		{"tree", func(r *verificationRecord) { r.TreeHash = "other" }},
		{"ci.yml digest", func(r *verificationRecord) { r.CiDigest = "other" }},
		{"go version", func(r *verificationRecord) { r.Tools["go"] = "1.26.0" }},
		{"node version", func(r *verificationRecord) { r.Tools["node"] = "22.0.0" }},
		{"go.sum digest", func(r *verificationRecord) { r.Dependencies["go.sum"] = "other" }},
		{"lockfile digest", func(r *verificationRecord) { r.Dependencies["web/package-lock.json"] = "other" }},
		{"os", func(r *verificationRecord) { r.OS = "darwin" }},
		{"arch", func(r *verificationRecord) { r.Arch = "arm64" }},
		{"base commit", func(r *verificationRecord) { r.BaseCommit = "other-base" }},
		{"GOFLAGS", func(r *verificationRecord) { r.GoFlags = "-tags=other" }},
		{"GOENV", func(r *verificationRecord) { r.GoEnv = "/tmp/goenv" }},
	} {
		current := fresh()
		row.apply(&current)
		if ok, why := localReuse(fresh(), current); ok {
			t.Errorf("%s: reused anyway (%s)", row.name, why)
		}
	}
	// The commit is not a key: the same tree with another head still matches.
	current := fresh()
	current.HeadCommit = "a-different-commit"
	if ok, why := localReuse(fresh(), current); !ok {
		t.Errorf("a different commit with the same tree is refused: %s", why)
	}
	// A record that did not pass is never reused.
	for _, result := range []string{localFail, ""} {
		stored := fresh()
		stored.Result = result
		if ok, _ := localReuse(stored, fresh()); ok {
			t.Errorf("a record whose result is %q is reused", result)
		}
	}
}

// C6/answer 3: a pin mismatch is recorded and the record is not reused.
func TestLocalTools_pin_mismatch_is_recorded_and_never_reused(t *testing.T) {
	pins := map[string]string{"go": "1.27.1", "node": "24.20.0", "gitleaks": "8.30.1"}
	observed := map[string]string{"go": "1.27.1", "node": "22.0.0", "gitleaks": "8.30.1"}
	mismatch := localPinMismatch(pins, observed)
	if len(mismatch) != 1 || mismatch[0] != "node" {
		t.Fatalf("pinMismatch = %v, want [node]", mismatch)
	}
	record := verificationRecord{
		Schema: recordSchema, Result: localPass, PinMismatch: mismatch,
		TreeHash: "tree", CiDigest: "ci", OS: "linux", Arch: "amd64",
		Tools: observed, Dependencies: map[string]string{"go.sum": "sum"},
	}
	current := verificationRecord{
		TreeHash: "tree", CiDigest: "ci", OS: "linux", Arch: "amd64",
		Tools: observed, Dependencies: map[string]string{"go.sum": "sum"},
	}
	if ok, why := localReuse(record, current); ok {
		t.Errorf("a record with a pin mismatch is reused (%s)", why)
	}
	// A tool the tree pins and the host does not have is a mismatch too.
	if got := localPinMismatch(map[string]string{"go": "1.27.1"}, map[string]string{"go": ""}); len(got) != 1 {
		t.Errorf("a missing pinned tool is not a mismatch: %v", got)
	}
	// An unpinned tool is not a mismatch.
	if got := localPinMismatch(map[string]string{}, map[string]string{"go": ""}); len(got) != 0 {
		t.Errorf("an unpinned tool is a mismatch: %v", got)
	}
}

// comment-c6: the run verifies the commit, not the caller's working tree. An uncommitted change in
// the checkout does not reach the clean worktree, and the tree the record names is the commit's.
func TestLocal_verifies_the_commit_not_the_dirty_worktree(t *testing.T) {
	repo := newLocalFixture(t)
	// A dirty working tree: a changed tracked file and an untracked one.
	repo.write("file.txt", "changed but not committed\n")
	repo.write("untracked.txt", "untracked\n")
	committed, err := localRev(repo.root, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "record.json")
	// The step reads the file the commit holds, and the worktree it runs in does not have the
	// uncommitted change.
	plan := localFixturePlan("cat file.txt && test ! -e untracked.txt")
	made, _, err := localVerify(localRunOptions(repo, plan, record), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.HeadCommit != committed {
		t.Errorf("head = %s, want the commit %s", made.HeadCommit, committed)
	}
	if made.Result != localPass {
		t.Fatalf("the run over the commit failed: %#v", made.Jobs[0].Steps)
	}
	wantTree, err := localRev(repo.root, "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	if made.TreeHash != wantTree {
		t.Errorf("tree = %s, want the commit's tree %s", made.TreeHash, wantTree)
	}
	// The dirty file is still dirty: the run did not touch the caller's checkout.
	if data, err := os.ReadFile(filepath.Join(repo.root, "file.txt")); err != nil || string(data) != "changed but not committed\n" {
		t.Errorf("the caller's working tree was changed: %q, %v", data, err)
	}
}

// The engine leaves no worktree behind, and the repository's worktree list is as it was.
func TestLocal_removes_its_clean_worktree(t *testing.T) {
	repo := newLocalFixture(t)
	before := repo.git("worktree", "list")
	plan := localFixturePlan("echo hello")
	if _, _, err := localVerify(localRunOptions(repo, plan, filepath.Join(t.TempDir(), "r.json")), "", io.Discard); err != nil {
		t.Fatal(err)
	}
	after := repo.git("worktree", "list")
	if before != after {
		t.Errorf("the worktree list changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// The gate a heavy step goes through is prepended as argv, and the step's command travels as a
// script file, so a gate between the run and bash never sees the command's own characters.
func TestLocal_gate_prefixes_a_heavy_step_as_argv(t *testing.T) {
	gate := localGateArgv("gate.sh --flag", true, "/tmp/step-001.sh", "/tmp", nil)
	expectEqual(t, "a heavy step's argv", gate, []string{"gate.sh", "--flag", "env", "-i", "bash", "-c", localChdirScript, "bash", "/tmp", "bash", "--noprofile", "--norc", "-eo", "pipefail", "/tmp/step-001.sh"})
	light := localGateArgv("gate.sh", false, "/tmp/step-001.sh", "/tmp", nil)
	expectEqual(t, "a light step's argv", light, []string{"bash", "--noprofile", "--norc", "-eo", "pipefail", "/tmp/step-001.sh"})
	none := localGateArgv("", true, "/tmp/step-001.sh", "/tmp", nil)
	expectEqual(t, "no gate", none, []string{"bash", "--noprofile", "--norc", "-eo", "pipefail", "/tmp/step-001.sh"})
}

// A step's command reaches bash as a file, so no gate between the run and bash can expand it
// (systemd-run expands the arguments it is handed) and a block scalar survives verbatim.
func TestLocal_write_script_keeps_the_command_verbatim(t *testing.T) {
	dir := t.TempDir()
	opts := localOptions{output: filepath.Join(dir, "runner", "output")}
	command := localDistBuilds
	path, err := localWriteScript(opts, command)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != command+"\n" {
		t.Errorf("the script is not the command verbatim:\n%q\nwant\n%q", data, command+"\n")
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("the script is not executable: %v, %v", info.Mode(), err)
	}
	// The command itself is never an argv element, so the shell's characters cannot be touched.
	if strings.Contains(strings.Join(localGateArgv("gate.sh", true, path, "/tmp", nil), " "), "target in linux/amd64") {
		t.Error("the command reached the argv")
	}
}

// The two tools a step fetches are named from their pins whatever the host has: secrets.sh runs its
// own pinned Gitleaks and the lint leg runs the staticcheck the tree requires, so a host copy is
// never what ran and is not compared.
func TestLocalTools_a_fetched_tool_is_named_from_its_pin(t *testing.T) {
	pins := map[string]string{"go": "1.27.1", "gitleaks": "8.30.1", "staticcheck": "0.8.1"}
	observed := localObservedVersions(map[string]string{"go": "1.27.1", "gitleaks": "", "staticcheck": ""}, pins)
	if observed["gitleaks"] != "8.30.1" || observed["staticcheck"] != "0.8.1" {
		t.Errorf("the fetched tools are not named from their pins: %v", observed)
	}
	if got := localPinMismatch(pins, observed); len(got) != 0 {
		t.Errorf("a host without the fetched tools reports a mismatch: %v", got)
	}
	// A host copy that differs from the pin is not what ran, so it is not a mismatch either.
	host := localObservedVersions(map[string]string{"go": "1.27.1", "gitleaks": "8.18.0"}, pins)
	if host["gitleaks"] != "8.30.1" {
		t.Errorf("a host Gitleaks is named %q, want the pin it ran", host["gitleaks"])
	}
	if got := localPinMismatch(pins, host); len(got) != 0 {
		t.Errorf("a host Gitleaks that differs from the pin is reported as a mismatch: %v", got)
	}
}

// The record names the repository it was made in, so two repositories' records are told apart.
func TestLocalRecord_names_the_repository(t *testing.T) {
	repo := newLocalFixture(t)
	repo.git("remote", "add", "origin", "https://example.invalid/owner/repo.git")
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(t.TempDir(), "r.json")), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.Repository != "https://example.invalid/owner/repo.git" {
		t.Errorf("repository = %q, want the origin URL", made.Repository)
	}
}

// A base equal to the head would scan an empty range, so it is never chosen as the default base.
func TestLocalDefaultBase_never_returns_the_head(t *testing.T) {
	repo := newLocalFixture(t)
	head, err := localRev(repo.root, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	// origin/dev does not exist here, so the parent is the base; a root commit has no parent and
	// the base stays empty rather than becoming the head.
	base, err := localDefaultBase(repo.root, head)
	if err != nil {
		t.Fatal(err)
	}
	if base == head {
		t.Errorf("the default base is the head itself (%s)", head)
	}
}

// The heavy gate takes the plan's workdir into account: the screen commands run in web/.
func TestLocalPlan_the_screen_commands_run_in_web(t *testing.T) {
	for _, job := range localPlan() {
		if job.name != "gui" {
			continue
		}
		for _, step := range job.steps {
			switch step.name {
			case "Install the screen dependencies from the committed lockfile",
				"Run the screen tests",
				"Build the screens into a fresh tree":
				if step.workdir != "web" {
					t.Errorf("%q runs in %q, want web", step.name, step.workdir)
				}
			}
		}
	}
}

// A ci.yml run scalar written in YAML single quotes is unquoted by the parser, and the table
// carries the shell text, so the two compare as the runner sees them.
func TestLocalPlan_a_quoted_run_scalar_is_compared_unquoted(t *testing.T) {
	workflow, err := parseWorkflow(localWorkflow(t))
	if err != nil {
		t.Fatal(err)
	}
	job := workflowJobNamed(workflow, "validate")
	if job == nil {
		t.Fatal("ci.yml has no validate job")
	}
	found := false
	for _, step := range job.steps {
		if strings.Contains(step.run, "ci validate") {
			found = true
			if strings.HasPrefix(step.run, "'") {
				t.Errorf("the reader kept the YAML quotes: %q", step.run)
			}
		}
	}
	if !found {
		t.Error("validate has no ci validate step")
	}
}
