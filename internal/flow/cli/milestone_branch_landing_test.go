package cli

import (
	"strings"
	"testing"
)

// TestMilestoneBranchTargetLandsTheRunsNodesThere (CRW-1355 c1): PR-unit landing is opt-in by the plan's target. A plan whose
// target base_ref names a milestone integration branch makes crw.integrate land the run's nodes on that branch, exactly as it lands
// them on dev today: the same merge commits (merge --no-ff "CRW-965: integrate <node>", in the run's order), the same merged tree, and
// no other branch moves. A plan whose base_ref is dev keeps the direct landing on dev. The milestone branch then goes to dev as one
// pull request through the dev queue (internal/integrator); nothing in the flow path knows about pull requests.
func TestMilestoneBranchTargetLandsTheRunsNodesThere(t *testing.T) {
	type landing struct {
		subjects []string
		tree     string
	}
	landed := map[string]landing{}
	for _, target := range []string{"crw/milestone-m1", "dev"} {
		t.Run(target, func(t *testing.T) {
			l := newLaneLabFor(t, labOwner, target, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1", After: []string{"A-1"}})
			base := l.git(l.repo, "rev-parse", "main")
			if target != "dev" {
				// the repository's dev, which a milestone-branch run must leave alone
				l.git(l.repo, "branch", "dev", base)
			}
			spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
			v := l.closed(l.runToEnd(spec.RunID))
			l.effectsOnce("A-1", "A-2")
			tip := l.git(l.repo, "rev-parse", target)
			if v.Tip != tip {
				t.Fatalf("the milestone closed over %s, the target branch %s is at %s", v.Tip, target, tip)
			}
			for _, key := range []string{"A-1", "A-2"} {
				var head string
				if err := l.w.st.DB.QueryRow("SELECT head FROM dag_agent_acceptances WHERE node_id = ?", l.nodeOf[key]).Scan(&head); err != nil {
					t.Fatal(err)
				}
				l.git(l.repo, "merge-base", "--is-ancestor", head, target)
			}
			if got := l.git(l.repo, "rev-parse", "dev"); target != "dev" && got != base {
				t.Errorf("dev moved to %s on a run whose target is %s", got, target)
			}
			if got := l.git(l.repo, "rev-parse", "main"); got != base {
				t.Errorf("main moved to %s", got)
			}
			if out := l.git(l.repo, "branch", "--list", "crw/*"); target == "dev" && out != "" {
				t.Errorf("a dev-target run made the branch %q", out)
			}
			subjects := strings.Split(l.git(l.repo, "log", "--first-parent", "--format=%s", base+".."+target), "\n")
			want := []string{"CRW-965: integrate " + l.nodeOf["A-2"], "CRW-965: integrate " + l.nodeOf["A-1"]}
			if strings.Join(subjects, "\n") != strings.Join(want, "\n") {
				t.Errorf("the landing on %s is %q, want the merge commits %q", target, subjects, want)
			}
			landed[target] = landing{subjects: subjects, tree: l.git(l.repo, "rev-parse", target+"^{tree}")}
		})
	}
	m, d := landed["crw/milestone-m1"], landed["dev"]
	if m.tree == "" || d.tree == "" {
		t.Fatal("a subtest did not land")
	}
	if m.tree != d.tree || strings.Join(m.subjects, "\n") != strings.Join(d.subjects, "\n") {
		t.Errorf("the milestone-branch landing differs from the direct landing: %+v vs %+v", m, d)
	}
}
