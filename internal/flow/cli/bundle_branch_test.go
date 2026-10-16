package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
)

// bundleHandoff is the handoff of plan-1 whose project integrates on a checkout: base_ref is the bundle's own branch and pr_base, when
// given, the branch its pull request is opened against (CRW-1440).
func bundleHandoff(t *testing.T, repo, baseRef, prBase string) compile.Input {
	t.Helper()
	in := testHandoff("plan-1", map[string][]string{"M1": {"CRW-11", "CRW-12"}})
	in.Projects[0].Target = &compile.Target{Repository: repo, BaseRef: baseRef, PRBase: prBase}
	return in
}

// bundleRepo is a repository with one commit on dev and no other branch, in an isolated git environment.
func bundleRepo(t *testing.T) string {
	t.Helper()
	isolateGit(t)
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "dev"}, {"add", "-A"}, {"commit", "-q", "-m", "base"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return repo
}

func runDirs(t *testing.T, state string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(state, "flow"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestStartRefusesABundleRunWhoseBranchIsMissing (CRW-1440): a handoff whose target names a pr_base is a bundle: its run integrates on
// base_ref, the bundle's own branch, which `crw flow pr branch` makes. The start refuses such a run while that branch is not in the
// target checkout, preview included, names the command that makes it, and writes no run. With the branch present the refusal is
// absent. A handoff without pr_base never runs git (its checkout may not even exist), as before.
func TestStartRefusesABundleRunWhoseBranchIsMissing(t *testing.T) {
	repo := bundleRepo(t)
	w := newWorld(t)
	td := w.deps()
	lane := func(path string, extra ...string) []string {
		return append([]string{"start", "project-lane", "--backend", "app-server", "--thread", "thread-owner", "--state", w.state, "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", path}, extra...)
	}
	bundle := writeHandoff(t, bundleHandoff(t, repo, "bundle/M1", "dev"))

	for _, extra := range [][]string{nil, {"--preview"}} {
		code, out, errOut := td.do(lane(bundle, extra...)...)
		if code == 0 {
			t.Fatalf("a bundle run with no bundle branch started (%v):\n%s%s", extra, out, errOut)
		}
		for _, want := range []string{"bundle/M1", "crw flow pr branch", "--base dev", "--name bundle/M1"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("the refusal (%v) lacks %q: %s", extra, want, errOut)
			}
		}
		if len(td.spawned) != 0 || len(runDirs(t, w.state)) != 0 {
			t.Fatalf("a refused start left %d processes and runs %v", len(td.spawned), runDirs(t, w.state))
		}
	}

	if out, err := exec.Command("git", "-C", repo, "branch", "bundle/M1", "dev").CombinedOutput(); err != nil {
		t.Fatalf("git branch: %v\n%s", err, out)
	}
	if code, out, errOut := td.do(lane(bundle)...); code != 0 || len(td.spawned) != 1 {
		t.Fatalf("a bundle run with its branch: exit %d, %d processes\n%s%s", code, len(td.spawned), out, errOut)
	}

	// a branch given as a full ref is the same branch (Target.Branch)
	full := writeHandoff(t, bundleHandoff(t, repo, "refs/heads/bundle/other", "dev"))
	if code, _, errOut := td.do(lane(full, "--preview")...); code == 0 || !strings.Contains(errOut, `"bundle/other"`) {
		t.Errorf("a refs/heads/ base_ref whose branch is missing: exit %d %s", code, errOut)
	}

	// no pr_base: not a bundle, the checkout is not read
	plain := writeHandoff(t, bundleHandoff(t, filepath.Join(t.TempDir(), "absent"), "crw/not-there", ""))
	if code, out, errOut := td.do(lane(plain)...); code != 0 || len(td.spawned) != 2 {
		t.Fatalf("a run without pr_base: exit %d, %d processes\n%s%s", code, len(td.spawned), out, errOut)
	}
}

// TestBundleBranchProblemReadsTheTargetCheckout: the check is a pure answer ("" when the run may start), reads the target's
// repository (what the run integrates on) and not the caller's working directory, and leaves a foreign GIT_DIR out of it.
func TestBundleBranchProblemReadsTheTargetCheckout(t *testing.T) {
	repo := bundleRepo(t)
	ctx := context.Background()
	if out, err := exec.Command("git", "-C", repo, "branch", "bundle/M1", "dev").CombinedOutput(); err != nil {
		t.Fatalf("git branch: %v\n%s", err, out)
	}
	cases := []struct {
		name, plan, base, prBase string
		wantEmpty                bool
		want                     string
	}{
		{name: "present", plan: "plan-1", base: "bundle/M1", prBase: "dev", wantEmpty: true},
		{name: "full ref present", plan: "plan-1", base: "refs/heads/bundle/M1", prBase: "dev", wantEmpty: true},
		{name: "missing", plan: "plan-1", base: "bundle/M2", prBase: "dev", want: "crw flow pr branch --checkout " + repo + " --milestone <M> --base dev --name bundle/M2"},
		{name: "no pr_base", plan: "plan-1", base: "bundle/M2", prBase: "", wantEmpty: true},
		{name: "unknown plan", plan: "plan-x", base: "bundle/M2", prBase: "dev", wantEmpty: true},
		{name: "a tag is not a branch", plan: "plan-1", base: "v1", prBase: "dev", want: "not in the target checkout"},
	}
	if out, err := exec.Command("git", "-C", repo, "tag", "v1", "dev").CombinedOutput(); err != nil {
		t.Fatalf("git tag: %v\n%s", err, out)
	}
	// a routing variable of the caller must not send the check to another repository
	other := bundleRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := bundleHandoff(t, repo, c.base, c.prBase)
			got := BundleBranchProblem(ctx, nil, in, c.plan)
			switch {
			case c.wantEmpty && got != "":
				t.Errorf("BundleBranchProblem = %q, want none", got)
			case !c.wantEmpty && !strings.Contains(got, c.want):
				t.Errorf("BundleBranchProblem = %q, want it to hold %q", got, c.want)
			}
		})
	}
}
