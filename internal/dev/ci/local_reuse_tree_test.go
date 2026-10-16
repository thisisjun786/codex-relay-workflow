//go:build dev

package ci

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	flowcli "github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
)

// The real executor materializes a range prerequisite, runs a reusable tree check,
// and judges their results through its aggregate barrier.
func crw1590Plan() []localJob {
	return []localJob{
		{name: "range", steps: []localStep{
			{name: "prepare range tool", kind: localRun, scope: "full",
				command: `set -e; printf 'ready\n' > "$RUNNER_TEMP/input"; echo prepare >> "$CRW1590_MARK/prepare"`},
			{name: "range checks", kind: localRun, scope: "range",
				command: `set -e; test "$(cat "$RUNNER_TEMP/input")" = ready; printf '%s %s\n' "$PR_BASE_SHA" "$PR_HEAD_SHA" >> "$CRW1590_MARK/range"; test "$PR_HEAD_SHA" != "$CRW1590_FAIL_HEAD"`,
				env:     []string{"PR_BASE_SHA=" + localBaseEnv, "PR_HEAD_SHA=" + localHeadEnv}},
		}},
		{name: "tree", steps: []localStep{{name: "tree tests", kind: localRun, scope: "full",
			command: `echo tree >> "$CRW1590_MARK/tree"; test "${CRW1590_FAIL_TREE:-}" != 1`}}},
		{name: "gate", steps: []localStep{{name: "aggregate", kind: localAction, action: localAggregate, scope: "full"}}},
	}
}

func crw1590Fixture(t *testing.T) (*localFixture, string, string) {
	t.Helper()
	repo := newLocalFixture(t)
	nodeVersion := localToolVersions(localPathEnv(nil))["node"]
	if nodeVersion == "" {
		t.Fatal("the reuse fixture requires Node on PATH to match its declared pin")
	}
	if workflow := strings.Replace(localFixtureWorkflow, "'24.20.0'", "'"+nodeVersion+"'", 1); workflow != localFixtureWorkflow {
		repo.write(".github/workflows/ci.yml", workflow)
		repo.commit()
	}
	base := crw1590Rev(repo, "HEAD")
	repo.write("node.txt", "implemented\n")
	repo.commit()
	return repo, base, crw1590Rev(repo, "HEAD")
}

func crw1590Rev(repo *localFixture, rev string) string {
	return strings.TrimSpace(repo.git("rev-parse", rev))
}

func crw1590Merge(t *testing.T, repo *localFixture, base, node string) string {
	t.Helper()
	repo.git("checkout", "--detach", base)
	repo.git("merge", "--no-ff", "-m", "integrate node", node)
	merged := crw1590Rev(repo, "HEAD")
	if parents := strings.Fields(repo.git("rev-list", "-1", "--parents", merged)); len(parents) != 3 {
		t.Fatalf("want a two-parent merge, got %v", parents)
	}
	if merged == node || crw1590Rev(repo, merged+"^{tree}") != crw1590Rev(repo, node+"^{tree}") {
		t.Fatal("fixture is not a different commit with the same tree")
	}
	return merged
}

func crw1590Options(repo *localFixture, mark, base, head, record string) localOptions {
	opts := localRunOptions(repo, crw1590Plan(), record)
	opts.Base, opts.Commit = base, head
	opts.Env = []string{"CRW1590_MARK=" + mark, "CRW1590_FAIL_HEAD="}
	return opts
}

func crw1590Verify(t *testing.T, opts localOptions, source string) (verificationRecord, bool) {
	t.Helper()
	got, reused, err := localVerify(opts, source, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return got, reused
}

func crw1590Write(t *testing.T, path string, record verificationRecord) verificationRecord {
	t.Helper()
	sealed, err := writeRecord(path, record)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// Decode the additive field so these behavior tests also compile against the
// pre-fix verifier for the recorded red run.
func crw1590Source(t *testing.T, step recordStep) map[string]any {
	t.Helper()
	raw, err := json.Marshal(step)
	if err != nil {
		t.Fatal(err)
	}
	var fields struct {
		ReusedFrom map[string]any `json:"reusedFrom"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	return fields.ReusedFrom
}

func crw1590NoReuse(t *testing.T, record verificationRecord) {
	t.Helper()
	for _, job := range record.Jobs {
		for _, step := range job.Steps {
			if source := crw1590Source(t, step); source != nil {
				t.Errorf("fresh %s/%s reused %v", job.Name, step.Name, source)
			}
		}
	}
}

func crw1590Read(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The subprocess uses the production reporting/exit-code path with a small plan.
// CILocalIntegration constructs and launches the argv as in a real integration.
func crw1590Command() int {
	if len(os.Args) < 3 || os.Args[1] != "ci" || os.Args[2] != "local" {
		return 2
	}
	fs := flag.NewFlagSet("ci local fixture", flag.ContinueOnError)
	head := fs.String("commit", "", "")
	base := fs.String("base", "", "")
	record := fs.String("record", "", "")
	runner := fs.String("runner", "", "")
	reuse := fs.String("reuse", "", "")
	if err := fs.Parse(os.Args[3:]); err != nil {
		return 2
	}
	root, err := os.Getwd()
	if err != nil {
		return 2
	}
	jobs, err := strconv.Atoi(os.Getenv("CRW1590_JOBS"))
	if err != nil {
		return 2
	}
	opts := localOptions{Root: root, Commit: *head, Base: *base, Record: *record, Runner: *runner,
		Plan: crw1590Plan(), Jobs: jobs, WorkRoot: os.Getenv("CRW1590_WORK"),
		Env: []string{"CRW1590_MARK=" + os.Getenv("CRW1590_MARK"), "CRW1590_FAIL_HEAD=" + os.Getenv("CRW1590_FAIL_HEAD")}}
	return localRunAndReport(opts, *reuse, os.Stdout, os.Stderr)
}

func crw1590CommandScript(t *testing.T, repo *localFixture, mark, failHead string, jobs int) (string, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	dir := t.TempDir()
	script, stderr := filepath.Join(dir, "crw-dev"), filepath.Join(dir, "stderr")
	body := fmt.Sprintf("#!/bin/sh\nCRW_TEST_REUSE_HELPER=1 CRW1590_MARK=%s CRW1590_WORK=%s CRW1590_FAIL_HEAD=%s CRW1590_JOBS=%d exec %s \"$@\" 2>%s\n",
		quote(mark), quote(repo.work), quote(failHead), jobs, quote(self), quote(stderr))
	syscall.ForkLock.RLock()
	err = os.WriteFile(script, []byte(body), 0o700)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	return script, stderr
}

func TestCRW1590OneNodeIntegrationReusesTreeSteps(t *testing.T) {
	for _, jobs := range []int{1, 3} {
		t.Run(fmt.Sprintf("jobs%d", jobs), func(t *testing.T) {
			crw1590Integration(t, jobs, false)
		})
	}
}

func TestCRW1590RangeFailureWithReusedTreeFails(t *testing.T) {
	crw1590Integration(t, 3, true)
}

func crw1590Integration(t *testing.T, jobs int, failRange bool) {
	t.Helper()
	repo, base, node := crw1590Fixture(t)
	mark, records := t.TempDir(), t.TempDir()
	sourcePath, out := filepath.Join(records, "node.json"), filepath.Join(records, "integration.json")
	opts := crw1590Options(repo, mark, base, node, sourcePath)
	opts.Jobs = jobs
	made, reused := crw1590Verify(t, opts, "")
	if reused || made.Result != localPass || len(made.PinMismatch) != 0 {
		t.Fatalf("node setup: reused=%t record=%+v", reused, made)
	}
	source := crw1590Write(t, sourcePath, made)
	sourceBytes := crw1590Read(t, sourcePath)
	merged := crw1590Merge(t, repo, base, node)
	failHead := ""
	if failRange {
		failHead = merged
	}
	script, stderr := crw1590CommandScript(t, repo, mark, failHead, jobs)
	err := flowcli.CILocalIntegration(crwconfig.VerifierTools{CrwDev: script}, io.Discard)(
		context.Background(), repo.root, []string{"CRW_VERIFY_HEAD=" + merged, "CRW_VERIFY_BASE=" + base,
			"CRW_VERIFY_RECORD=" + out, "CRW_VERIFY_REUSE=" + sourcePath})
	if (err != nil) != failRange {
		t.Fatalf("integration error=%v, failRange=%t; stderr=%s", err, failRange, crw1590Read(t, stderr))
	}
	got, err := readRecord(out)
	if err != nil {
		t.Fatal(err)
	}
	want := localPass
	if failRange {
		want = localFail
		if !strings.Contains(string(crw1590Read(t, stderr)), "range / range checks: failed") {
			t.Error("the production command lost the failing range diagnostic")
		}
	}
	if got.Result != want || got.HeadCommit != merged || got.BaseCommit != base || got.Range == source.Range {
		t.Errorf("new record result/head/base/range = %s/%s/%s/%s", got.Result, got.HeadCommit, got.BaseCommit, got.Range)
	}
	if digest, err := recordDigest(got); err != nil || digest != got.Digest || got.Digest == "" {
		t.Errorf("new digest %q does not seal the result: %s %v", got.Digest, digest, err)
	}
	for name, count := range map[string]int{"tree": 1, "prepare": 2, "range": 2} {
		if actual := localMarkerLines(t, filepath.Join(mark, name)); actual != count {
			t.Errorf("%s executions=%d, want %d", name, actual, count)
		}
	}
	lines := strings.Split(strings.TrimSpace(string(crw1590Read(t, filepath.Join(mark, "range")))), "\n")
	if last := lines[len(lines)-1]; last != base+" "+merged {
		t.Errorf("new range input=%q, want %q", last, base+" "+merged)
	}
	for _, job := range got.Jobs {
		for n, step := range job.Steps {
			r := crw1590Source(t, step)
			if job.Name == "tree" {
				if r == nil || r["digest"] != source.Digest || r["record"] != sourcePath ||
					r["headCommit"] != node || r["job"] != job.Name || r["step"] != float64(n) {
					t.Errorf("tree provenance=%v, want source %s at %s", r, source.Digest, node)
				}
			} else if r != nil {
				t.Errorf("fresh %s/%s has reusedFrom=%v", job.Name, step.Name, r)
			}
		}
	}
	if failRange && (got.Jobs[0].Result != localFail || got.Jobs[2].Result != localFail) {
		t.Error("fresh range and aggregate must both fail")
	}
	if !bytes.Equal(sourceBytes, crw1590Read(t, sourcePath)) {
		t.Error("reuse changed the source record")
	}
}

func TestCRW1590DifferentTreeRuns(t *testing.T) {
	repo, base, node := crw1590Fixture(t)
	mark, records := t.TempDir(), t.TempDir()
	path := filepath.Join(records, "node.json")
	made, _ := crw1590Verify(t, crw1590Options(repo, mark, base, node, path), "")
	crw1590Write(t, path, made)
	repo.write("other.txt", "another tree\n")
	repo.commit()
	head := crw1590Rev(repo, "HEAD")
	if crw1590Rev(repo, head+"^{tree}") == made.TreeHash {
		t.Fatal("fixture did not change the tree")
	}
	got, reused := crw1590Verify(t, crw1590Options(repo, mark, base, head, filepath.Join(records, "changed.json")), path)
	if reused || got.Result != localPass || localMarkerLines(t, filepath.Join(mark, "tree")) != 2 {
		t.Errorf("different tree reused=%t result=%s; expected a second execution", reused, got.Result)
	}
	crw1590NoReuse(t, got)
}

func TestCRW1590DifferentRulesRun(t *testing.T) {
	for _, key := range []string{"command", "tool", "ci.yml", "plan"} {
		t.Run(key, func(t *testing.T) {
			repo, base, node := crw1590Fixture(t)
			mark, records := t.TempDir(), t.TempDir()
			path := filepath.Join(records, "source.json")
			opts := crw1590Options(repo, mark, base, node, path)
			source, _ := crw1590Verify(t, opts, "")
			switch key {
			case "tool":
				source.Tools["go"] = "different"
			case "ci.yml":
				source.CiDigest = "sha256:different"
			case "plan":
				source.PlanDigest = "sha256:different"
			}
			crw1590Write(t, path, source)
			opts.Record = filepath.Join(records, "changed.json")
			if key == "command" {
				opts.Plan = crw1590Plan()
				opts.Plan[1].steps[0].command += "\n# a different validation command"
			}
			got, reused := crw1590Verify(t, opts, path)
			if reused || got.Result != localPass || localMarkerLines(t, filepath.Join(mark, "tree")) != 2 {
				t.Errorf("%s rule change: reused=%t result=%s; expected a fresh tree check", key, reused, got.Result)
			}
			crw1590NoReuse(t, got)
		})
	}
}

func TestCRW1590FailedRecordRuns(t *testing.T) {
	repo, base, node := crw1590Fixture(t)
	mark, records := t.TempDir(), t.TempDir()
	path := filepath.Join(records, "failed.json")
	opts := crw1590Options(repo, mark, base, node, path)
	opts.Env = append(opts.Env, "CRW1590_FAIL_TREE=1")
	failed, _ := crw1590Verify(t, opts, "")
	if failed.Result != localFail || failed.Jobs[1].Steps[0].Result != localFailed {
		t.Fatal("source must be produced by an actual failed tree command")
	}
	crw1590Write(t, path, failed)
	merged := crw1590Merge(t, repo, base, node)
	got, reused := crw1590Verify(t, crw1590Options(repo, mark, base, merged, filepath.Join(records, "good.json")), path)
	if reused || got.Result != localPass || localMarkerLines(t, filepath.Join(mark, "tree")) != 2 {
		t.Errorf("failed source: reused=%t result=%s; expected a fresh successful check", reused, got.Result)
	}
	crw1590NoReuse(t, got)
}

func TestCRW1590RangeInputsAlwaysRun(t *testing.T) {
	for _, changedBase := range []bool{false, true} {
		t.Run(fmt.Sprintf("changedBase%t", changedBase), func(t *testing.T) {
			repo, base, node := crw1590Fixture(t)
			mark, records := t.TempDir(), t.TempDir()
			path := filepath.Join(records, "source.json")
			source, _ := crw1590Verify(t, crw1590Options(repo, mark, base, node, path), "")
			crw1590Write(t, path, source)
			if changedBase {
				base = node
			}
			got, reused := crw1590Verify(t, crw1590Options(repo, mark, base, node, filepath.Join(records, "again.json")), path)
			if !reused || got.Result != localPass || got.BaseCommit != base ||
				localMarkerLines(t, filepath.Join(mark, "tree")) != 1 || localMarkerLines(t, filepath.Join(mark, "range")) != 2 {
				t.Errorf("range check did not execute independently: reused=%t record=%+v", reused, got)
			}
			lines := strings.Split(strings.TrimSpace(string(crw1590Read(t, filepath.Join(mark, "range")))), "\n")
			if lines[len(lines)-1] != base+" "+node {
				t.Errorf("fresh range got %q", lines[len(lines)-1])
			}
		})
	}
}
