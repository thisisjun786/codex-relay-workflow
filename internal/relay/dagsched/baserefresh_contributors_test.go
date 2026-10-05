package dagsched

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// A real landing whose second parent is an accepted node, not a fabricated
// provenance answer. The scheduler and store remain the synthetic integration kit;
// the contributor is accepted on the repository the scenario lands on.
func contributorMerge(t *testing.T, s *refreshScenario, regions []Region, known bool) {
	t.Helper()
	r := s.repo
	r.git("checkout", "-q", "-b", "contributor", "dev")
	head := r.commit("shared.json", "base\ndev\n")
	if known {
		for i := range regions {
			regions[i].Repository = s.target()
		}
		if _, err := s.sched.DeclareRegions(context.Background(), "g", "D", "parent", regions); err != nil {
			t.Fatal(err)
		}
		s.acceptRefreshNode("D", acceptOpts{HeadSHA: head, PR: 8})
	}
	r.git("checkout", "-q", "dev")
	r.git("merge", "-q", "--no-ff", "-m", "land contributor", "contributor")
	s.head = s.mergeDev("refresh", "shared.json", "base\nchild\ndev\n")
	s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, s.head)
}

func TestBaseRefreshContributorsAcrossHops(t *testing.T) {
	for _, grade := range []string{GradeMechanical, GradeLocal} {
		t.Run(grade, func(t *testing.T) {
			s := mechanicalRefresh(t, RuleUnion, false)
			s.putPlan("g", 1, "add-contributor", addRelNode("E", dag.NodeImplementation))
			contributorMerge(t, s, []Region{gr("shared.json", GradeMechanical, RuleUnion)}, true)
			r := s.repo
			r.git("checkout", "-q", "-b", "second-contributor", "dev")
			head := r.commit("shared.json", "base\nsecond\ndev\n")
			rule := ""
			if grade == GradeMechanical {
				rule = RuleUnion
			}
			region := gr("shared.json", grade, rule)
			region.Repository = s.target()
			if _, err := s.sched.DeclareRegions(context.Background(), "g", "E", "parent", []Region{region}); err != nil {
				t.Fatal(err)
			}
			s.acceptRefreshNode("E", acceptOpts{HeadSHA: head, PR: 9})
			r.git("checkout", "-q", "dev")
			r.git("merge", "-q", "--no-ff", "-m", "land second contributor", "second-contributor")
			s.head = s.mergeDev("second refresh", "shared.json", "base\nchild\nsecond\ndev\n")
			s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, s.head)
			got, err := s.record()
			if grade == GradeMechanical {
				if err != nil || len(got.Steps) != 2 || len(got.Resolved) != 0 {
					t.Fatalf("unanimous chain: %v %+v", err, got)
				}
			} else {
				if refusalReason(err) != "disposition_conflict" {
					t.Fatalf("later disagreement: %v", err)
				}
				got, err = s.record("shared.json")
				if err != nil || got.Steps[0].Resolved[0].Rule != RuleUnion || got.Steps[1].Resolved[0].Rule != "" {
					t.Fatalf("per-hop rules: %v %+v", err, got)
				}
			}
		})
	}
}

func TestBaseRefreshUnknownDeltaVetoesKnownContributor(t *testing.T) {
	s := mechanicalRefresh(t, RuleUnion, false)
	contributorMerge(t, s, []Region{gr("shared.json", GradeMechanical, RuleUnion)}, true)
	r := s.repo
	r.commit("shared.json", "base\nunknown\ndev\n")
	// Refresh the original head so both the known and unknown landing contribute
	// to this one hop, exercising the unknown veto of an accumulated known set.
	r.git("checkout", "-q", "-b", "single-hop", s.h1)
	if _, err := r.tryGit("merge", "-q", "--no-ff", "-m", "unknown refresh", "dev"); err == nil {
		t.Fatal("expected a conflict")
	}
	r.write("shared.json", "base\nchild\nunknown\ndev\n")
	r.git("add", "shared.json")
	r.git("commit", "-q", "-m", "unknown refresh")
	s.head = r.git("rev-parse", "HEAD")
	r.git("checkout", "-q", "dev")
	s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, s.head)
	if _, err := s.record(); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("unknown path delta: %v", err)
	}
	if _, err := s.record("shared.json"); err != nil {
		t.Fatal(err)
	}
}

func TestBaseRefreshRegenerateEligibilityRecord(t *testing.T) {
	for _, kind := range []string{"generated", "owner loss", "partial updater", "failed command"} {
		t.Run(kind, func(t *testing.T) {
			k := newForgeIntegrationKit(t)
			r := k.repo
			rule := "regenerate:cat left.txt right.txt > shared.json"
			if kind == "partial updater" {
				rule = "regenerate:test -f shared.json && cat left.txt right.txt > shared.json"
			}
			if kind == "failed command" {
				rule = "regenerate:command-that-does-not-exist"
			}
			r.commit("left.txt", "left\n")
			r.commit("right.txt", "right\n")
			r.commit("shared.json", "left\nright\n")
			r.git("checkout", "-q", "-b", "feature")
			r.commit("left.txt", "child\n")
			ours, theirs := "child\nright\n", "left\ndev\n"
			if kind == "owner loss" || kind == "partial updater" {
				ours += "owner: old\n"
				theirs += "owner: new\n"
			}
			head := r.commit("shared.json", ours)
			region := gr("shared.json", GradeMechanical, rule)
			region.Repository = forgeKitRepository
			for _, node := range []string{"I", "D"} {
				if _, err := k.sched.DeclareRegions(context.Background(), "g", node, "parent", []Region{region}); err != nil {
					t.Fatal(err)
				}
			}
			a := k.acceptOnForge("g", "I", acceptOpts{HeadSHA: head, PR: 7})
			n, _ := nodeOf(k.snapshot("g"), "I")
			s := &refreshScenario{integrationKit: k, rid: a.Acceptance.RelationshipID, accepted: a, h1: head, head: head, criteria: n.CriteriaSetDigest, checkout: r.path}
			s.openGeneration()
			r.git("checkout", "-q", "-b", "generated-contributor", "dev")
			r.commit("right.txt", "dev\n")
			dev := r.commit("shared.json", theirs)
			k.acceptOnForge("g", "D", acceptOpts{HeadSHA: dev, PR: 8})
			r.git("checkout", "-q", "dev")
			r.git("merge", "-q", "--no-ff", "-m", "land generated contributor", "generated-contributor")
			s.head = s.mergeDev("regenerated refresh", "shared.json", "child\ndev\n")
			s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, s.head)
			got, err := s.record()
			if kind == "generated" {
				if err != nil || len(got.Resolved) != 0 {
					t.Fatalf("full generated record: %v %+v", err, got)
				}
				refreshRuleRecord(t, s, "shared.json", rule)
			} else {
				if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "--resolved") {
					t.Fatalf("manual record: %v", err)
				}
				got, err = s.record("shared.json")
				if err != nil || strings.Join(got.Resolved, ",") != "shared.json" {
					t.Fatalf("exact manual: %v %+v", err, got)
				}
				refreshRuleRecord(t, s, "shared.json", "")
			}
		})
	}
}

func TestBaseRefreshContributorAgreement(t *testing.T) {
	for _, tc := range []struct {
		name              string
		regions           []Region
		known, mechanical bool
	}{
		{"unanimous", []Region{gr("shared.json", GradeMechanical, RuleUnion)}, true, true},
		{"local", []Region{gr("shared.json", GradeLocal, "")}, true, false},
		{"different rule", []Region{gr("shared.json", GradeMechanical, "regenerate:cat input > shared.json")}, true, false},
		{"missing declaration", []Region{gr("other.txt", GradeMechanical, RuleUnion)}, true, false},
		{"unknown landing", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mechanicalRefresh(t, RuleUnion, false)
			contributorMerge(t, s, tc.regions, tc.known)
			got, err := s.record()
			if tc.mechanical {
				if err != nil || len(got.Resolved) != 0 {
					t.Fatalf("agreed rule: %v %+v", err, got)
				}
				refreshRuleRecord(t, s, "shared.json", RuleUnion)
				return
			}
			if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "--resolved") || s.refreshRows() != 0 {
				t.Fatalf("must need exact manual name: %v %+v", err, got)
			}
			if _, err := s.record("shared.json", "other.txt"); refusalReason(err) != "disposition_conflict" {
				t.Fatalf("extra name: %v", err)
			}
			if _, err := s.record("shared.json"); err != nil {
				t.Fatal(err)
			}
			refreshRuleRecord(t, s, "shared.json", "")
		})
	}
}

func TestBaseRefreshContributorDeclarationFrozen(t *testing.T) {
	s := mechanicalRefresh(t, RuleUnion, false)
	contributorMerge(t, s, []Region{gr("shared.json", GradeMechanical, RuleUnion)}, true)
	s.sched.testBeforeRefreshTx = func() {
		s.exec("INSERT INTO dag_node_regions (plan_id,node_id,declaration_seq,repository,path,region_kind,region_key,change,exclusive,declared_by,declared_at) SELECT plan_id,node_id,declaration_seq+1,repository,'other.txt',region_kind,region_key,change,exclusive,declared_by,declared_at FROM dag_node_regions WHERE plan_id='g' AND node_id='D'")
	}
	_, err := s.record()
	if refusalReason(err) != "disposition_conflict" || s.refreshRows() != 0 {
		t.Fatalf("changed contributor: %v", err)
	}
}
