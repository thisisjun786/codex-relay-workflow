//go:build dev

package ci

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// localMarkerLines counts the lines a step wrote to the marker file, one per run of the step.
func localMarkerLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "\n")
}

// Parent ruling 1: a step never sees the caller's Go or Node variables. The engine sets GOFLAGS
// itself, and the record names the variables it ignored.
func TestLocal_a_step_never_sees_the_callers_Go_or_Node_variables(t *testing.T) {
	t.Setenv("GOFLAGS", "-exec=/bin/true -run=Nothing")
	t.Setenv("GOTOOLCHAIN", "go9.9.9")
	t.Setenv("NODE_OPTIONS", "--no-such-option")
	t.Setenv("GOEXPERIMENT", "nosuch")
	repo := newLocalFixture(t)
	record := filepath.Join(t.TempDir(), "record.json")
	command := `test "$GOFLAGS" = "-p=4" && test "$GOTOOLCHAIN" = local && test -z "${NODE_OPTIONS:-}" && test -z "${GOEXPERIMENT:-}"`
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan(command), record), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if step := made.Jobs[0].Steps[1]; step.Result != localPassed {
		t.Errorf("the step is %q (%s): a caller variable reached it", step.Result, step.Reason)
	}
	for _, name := range []string{"GOFLAGS", "GOTOOLCHAIN", "NODE_OPTIONS", "GOEXPERIMENT"} {
		if !localContains(made.IgnoredEnv, name) {
			t.Errorf("the record does not name the ignored %s: %v", name, made.IgnoredEnv)
		}
	}
}

// Parent ruling 3: a record is reusable only when its digest is valid AND it matches the plan one to
// one, every step passed (or is an allowed not-applicable step), its result is recomputed, and it is
// sealed. Each mutation below keeps the digest valid, so only the structural check refuses it.
func TestLocalReuse_a_record_that_does_not_match_the_plan_is_never_reused(t *testing.T) {
	cases := map[string]func(*verificationRecord){
		"no jobs":             func(r *verificationRecord) { r.Jobs = nil },
		"a skipped step":      func(r *verificationRecord) { r.Jobs[0].Steps[1].Result = localSkipped },
		"a failed step, pass": func(r *verificationRecord) { r.Jobs[0].Steps[1].Result = localFailed },
		"a step missing":      func(r *verificationRecord) { r.Jobs[0].Steps = r.Jobs[0].Steps[:1] },
		"a changed command":   func(r *verificationRecord) { r.Jobs[0].Steps[1].Command = "echo other" },
		"a changed scope":     func(r *verificationRecord) { r.Jobs[0].Steps[1].Scope = "range" },
		"a failed job, pass":  func(r *verificationRecord) { r.Jobs[0].Result = localFail },
		"not sealed":          func(r *verificationRecord) { r.Sealed = false },
		"a stale plan digest": func(r *verificationRecord) { r.PlanDigest = "sha256:stale" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			repo := newLocalFixture(t)
			record := filepath.Join(t.TempDir(), "record.json")
			opts := localRunOptions(repo, localFixturePlan("echo hello"), record)
			made, _, err := localVerify(opts, "", io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if made.Result != localPass {
				t.Fatalf("the fixture run is %q, want pass", made.Result)
			}
			mutate(&made)
			if _, err := writeRecord(record, made); err != nil {
				t.Fatal(err)
			}
			_, reused, err := localVerify(localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(t.TempDir(), "again.json")), record, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if reused {
				t.Error("a record that does not match the plan is reused")
			}
		})
	}
}

// Parent ruling 2: a job's Node pin comes from its own setup-node step; a job whose pin differs from
// the observed Node is a mismatch, the record is not reused, and the engine really runs the step again.
func TestLocalPins_a_job_whose_node_pin_differs_is_a_mismatch_and_re_runs(t *testing.T) {
	node := localToolVersions(localPathEnv(nil))["node"]
	if node == "" {
		t.Skip("no node on PATH")
	}
	repo := newLocalFixture(t)
	workflow := "name: fixture\n\non:\n  push:\n\njobs:\n" +
		"  validate:\n    runs-on: ubuntu-latest\n    steps:\n" +
		"      - uses: actions/setup-node@0000000000000000000000000000000000000000 # v4\n        with:\n          node-version: '" + node + "'\n" +
		"      - name: Say hello\n        run: echo hello\n" +
		"  gui:\n    runs-on: ubuntu-latest\n    steps:\n" +
		"      - uses: actions/setup-node@0000000000000000000000000000000000000000 # v4\n        with:\n          node-version: '0.0.1'\n" +
		"      - name: Say gui\n        run: echo \"$MARK\" >> \"$MARK_FILE\"\n"
	repo.write(".github/workflows/ci.yml", workflow)
	repo.commit()
	mark := filepath.Join(t.TempDir(), "marks")
	plan := []localJob{
		{name: "validate", steps: []localStep{{name: "Say hello", kind: localRun, command: "echo hello", scope: "full"}}},
		{name: "gui", steps: []localStep{{name: "Say gui", kind: localRun, command: "echo \"$MARK\" >> \"$MARK_FILE\"", scope: "full"}}},
	}
	options := func(record string) localOptions {
		opts := localRunOptions(repo, plan, record)
		opts.Env = []string{"MARK_FILE=" + mark, "MARK=run"}
		return opts
	}
	first := filepath.Join(t.TempDir(), "first.json")
	made, _, err := localVerify(options(first), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !localContains(made.PinMismatch, "gui:node") {
		t.Errorf("the gui job's Node pin differs from the host and is not named: %v", made.PinMismatch)
	}
	if _, err := writeRecord(first, made); err != nil {
		t.Fatal(err)
	}
	before := localMarkerLines(t, mark)
	_, reused, err := localVerify(options(filepath.Join(t.TempDir(), "again.json")), first, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if reused {
		t.Error("a record with a pin mismatch is reused")
	}
	if after := localMarkerLines(t, mark); after <= before {
		t.Errorf("the engine did not really run the step again (%d -> %d)", before, after)
	}
}

// Parent ruling 3 and 2: changing any one key of answer 4 (or a tool version) re-runs the table; the
// engine's own step runs, observed through a marker file, not only the boolean.
func TestLocalReuse_each_key_changed_alone_re_runs_the_table(t *testing.T) {
	keys := map[string]func(*verificationRecord){
		"tree":        func(r *verificationRecord) { r.TreeHash = "sha1:other" },
		"ci.yml":      func(r *verificationRecord) { r.CiDigest = "sha256:other" },
		"base":        func(r *verificationRecord) { r.BaseCommit = "0000000000000000000000000000000000000000" },
		"range":       func(r *verificationRecord) { r.Range = "sha256:other" },
		"os":          func(r *verificationRecord) { r.OS = "plan9" },
		"arch":        func(r *verificationRecord) { r.Arch = "mips" },
		"go tool":     func(r *verificationRecord) { r.Tools["go"] = "0.0.1" },
		"node tool":   func(r *verificationRecord) { r.Tools["node"] = "0.0.1" },
		"gitleaks":    func(r *verificationRecord) { r.Tools["gitleaks"] = "0.0.1" },
		"staticcheck": func(r *verificationRecord) { r.Tools["staticcheck"] = "0.0.1" },
		"go.sum":      func(r *verificationRecord) { r.Dependencies["go.sum"] = "sha256:other" },
		"lockfile":    func(r *verificationRecord) { r.Dependencies["web/package-lock.json"] = "sha256:other" },
		"GOFLAGS":     func(r *verificationRecord) { r.GoFlags = "-p=9" },
		"heavy gate":  func(r *verificationRecord) { r.HeavyGate = "sha256:other" },
	}
	for name, change := range keys {
		t.Run(name, func(t *testing.T) {
			repo := newLocalFixture(t)
			mark := filepath.Join(t.TempDir(), "marks")
			options := func(record string) localOptions {
				opts := localRunOptions(repo, localFixturePlan("echo \"$MARK\" >> \"$MARK_FILE\""), record)
				opts.Env = []string{"MARK_FILE=" + mark, "MARK=run"}
				return opts
			}
			record := filepath.Join(t.TempDir(), "record.json")
			made, _, err := localVerify(options(record), "", io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if made.Tools == nil {
				made.Tools = map[string]string{}
			}
			if made.Dependencies == nil {
				made.Dependencies = map[string]string{}
			}
			change(&made)
			if _, err := writeRecord(record, made); err != nil {
				t.Fatal(err)
			}
			before := localMarkerLines(t, mark)
			_, reused, err := localVerify(options(filepath.Join(t.TempDir(), "again.json")), record, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if reused {
				t.Errorf("changing %s alone is reused", name)
			}
			if after := localMarkerLines(t, mark); after <= before {
				t.Errorf("changing %s alone did not run the step again (%d -> %d)", name, before, after)
			}
		})
	}
	_ = fmt.Sprint
}

// Parent ruling 5: a clean worktree never lives under TMPDIR, /tmp or /var/tmp; such a root is refused.
func TestLocalWorkRoot_a_root_inside_TMPDIR_or_tmp_is_refused(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	for _, root := range []string{filepath.Join(tmp, "work"), "/tmp/crw-ci-work", "/var/tmp/crw-ci-work"} {
		if _, err := localWorkRoot(localOptions{WorkRoot: root}); err == nil || !strings.Contains(err.Error(), "work_root_in_tmp") {
			t.Errorf("the root %s is not refused as work_root_in_tmp: %v", root, err)
		}
	}
	base := os.Getenv("CRW_CI_TEST_WORK_ROOT")
	if base == "" {
		t.Skip("set CRW_CI_TEST_WORK_ROOT to a directory outside TMPDIR, /tmp and /var/tmp")
	}
	if _, err := localWorkRoot(localOptions{WorkRoot: filepath.Join(base, "accepted")}); err != nil {
		t.Errorf("a root outside the temporary directories is refused: %v", err)
	}
}

// A caller's module cache never reaches a step: a module tree is trusted from the cache without the
// go.sum check, so the cache is not part of what a sealed run may read.
func TestLocal_a_step_never_sees_the_callers_module_cache(t *testing.T) {
	t.Setenv("GOMODCACHE", "/somewhere/else")
	repo := newLocalFixture(t)
	record := filepath.Join(t.TempDir(), "record.json")
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan(`test -z "${GOMODCACHE:-}"`), record), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if step := made.Jobs[0].Steps[1]; step.Result != localPassed {
		t.Errorf("the step is %q (%s): the caller's module cache reached it", step.Result, step.Reason)
	}
}

// A step runs with replacement refs disabled, as the run's own reads do, so no replaced object can
// change what a step reads.
func TestLocal_a_step_reads_git_objects_without_replacement_refs(t *testing.T) {
	repo := newLocalFixture(t)
	record := filepath.Join(t.TempDir(), "record.json")
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan(`test "$GIT_NO_REPLACE_OBJECTS" = 1`), record), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if step := made.Jobs[0].Steps[1]; step.Result != localPassed {
		t.Errorf("the step is %q (%s): replacement refs are not disabled for it", step.Result, step.Reason)
	}
}
