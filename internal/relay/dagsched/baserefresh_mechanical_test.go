package dagsched

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The production checker is bootstrapped by the external test package, not mocked.
func mechanicalRefresh(t *testing.T, rule string, mixed bool) *refreshScenario {
	t.Helper()
	k := newIntegrationKit(t)
	r := k.repo
	r.commit("shared.json", "base\n")
	if mixed {
		r.commit("manual.txt", "base\n")
	}
	r.git("checkout", "-q", "-b", "feature")
	r.write("shared.json", "base\nchild\n")
	r.git("add", "shared.json")
	if mixed {
		r.write("manual.txt", "child\n")
		r.git("add", "manual.txt")
	}
	head := r.commit("feature.txt", "feature")
	r.git("checkout", "-q", "dev")
	if _, err := k.sched.DeclareRegions(context.Background(), "g", "I", "parent", []Region{gr("shared.json", GradeMechanical, rule)}); err != nil {
		t.Fatal(err)
	}
	a := k.acceptNode("g", "I", acceptOpts{HeadSHA: head, PR: 7, Forge: "owner/repo", Repository: r.path})
	n, _ := nodeOf(k.snapshot("g"), "I")
	s := &refreshScenario{integrationKit: k, rid: a.Acceptance.RelationshipID, accepted: a, h1: head, head: head, criteria: n.CriteriaSetDigest}
	s.openGeneration()
	return s
}

func mechanicalMerge(t *testing.T, s *refreshScenario, resolution string, mixed bool) {
	t.Helper()
	r := s.repo
	r.commit("shared.json", "base\ndev\n")
	if mixed {
		r.commit("manual.txt", "dev\n")
	}
	r.git("checkout", "-q", "feature")
	if _, err := r.tryGit("merge", "-q", "--no-ff", "-m", "refresh base", "dev"); err == nil {
		t.Fatal("expected real conflicts")
	}
	r.write("shared.json", resolution)
	if mixed {
		r.write("manual.txt", "hand resolution\n")
	}
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "refresh base")
	s.head = r.git("rev-parse", "HEAD")
	r.git("checkout", "-q", "dev")
	s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, s.head)
}

func refreshRuleRecord(t *testing.T, s *refreshScenario, path, rule string) {
	t.Helper()
	var encoded string
	if err := s.s.DB.QueryRow("SELECT proof_json FROM dag_base_refreshes").Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	var proof struct {
		Steps []struct {
			Resolved []struct{ Path, Blob, Rule string }
		}
	}
	if err := json.Unmarshal([]byte(encoded), &proof); err != nil {
		t.Fatal(err)
	}
	for _, st := range proof.Steps {
		for _, r := range st.Resolved {
			if r.Path == path {
				if r.Rule != rule || r.Blob == "" {
					t.Fatalf("record=%+v", r)
				}
				return
			}
		}
	}
	t.Fatalf("missing resolution of %s: %s", path, encoded)
}

func TestBaseRefreshMechanicalUnion(t *testing.T) {
	s := mechanicalRefresh(t, RuleUnion, false)
	mechanicalMerge(t, s, "base\nchild\ndev\n", false)
	got, err := s.record()
	if err != nil || len(got.Resolved) != 0 || s.refreshRows() != 1 {
		t.Fatalf("matching union without --resolved: %v %+v", err, got)
	}
	refreshRuleRecord(t, s, "shared.json", RuleUnion)
	replay, err := s.record()
	if err != nil || !replay.Replayed || replay.RefreshID != got.RefreshID {
		t.Fatalf("replay=%v %+v", err, replay)
	}
	// The same rule path is not an extra manual name.
	if _, err := s.record("shared.json"); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("extra manual name=%v", err)
	}
}

func TestBaseRefreshMechanicalAgainstRule(t *testing.T) {
	for _, rule := range []string{RuleUnion, "regenerate:printf 'generated\\n' > shared.json"} {
		t.Run(rule, func(t *testing.T) {
			s := mechanicalRefresh(t, rule, false)
			mechanicalMerge(t, s, "base\nchild\n", false)
			for _, named := range [][]string{nil, {"shared.json"}} {
				_, err := s.record(named...)
				if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "tree_differs") || !strings.Contains(err.Error(), "shared.json") || !strings.Contains(err.Error(), rule) || s.refreshRows() != 0 {
					t.Fatalf("broken %s named %v=%v rows=%d", rule, named, err, s.refreshRows())
				}
			}
		})
	}
}

func TestBaseRefreshMechanicalRegenerate(t *testing.T) {
	rule := "regenerate:printf 'generated\\n' > shared.json"
	s := mechanicalRefresh(t, rule, false)
	mechanicalMerge(t, s, "generated\n", false)
	got, err := s.record()
	if err != nil || len(got.Resolved) != 0 {
		t.Fatalf("matching regeneration=%v %+v", err, got)
	}
	refreshRuleRecord(t, s, "shared.json", rule)
}

func TestBaseRefreshMechanicalMixedNeedsExactManualNames(t *testing.T) {
	s := mechanicalRefresh(t, RuleUnion, true)
	mechanicalMerge(t, s, "base\nchild\ndev\n", true)
	for _, names := range [][]string{nil, {"shared.json"}, {"manual.txt", "shared.json"}, {"manual.txt", "manual.txt"}, {"other.txt"}} {
		_, err := s.record(names...)
		if refusalReason(err) != "disposition_conflict" || s.refreshRows() != 0 {
			t.Fatalf("manual names %v=%v rows=%d", names, err, s.refreshRows())
		}
	}
	got, err := s.record("manual.txt")
	if err != nil || strings.Join(got.Resolved, ",") != "manual.txt" {
		t.Fatalf("exact manual name=%v %+v", err, got)
	}
	refreshRuleRecord(t, s, "shared.json", RuleUnion)
	refreshRuleRecord(t, s, "manual.txt", "")
}

func TestBaseRefreshMechanicalUnevaluableRefused(t *testing.T) {
	for _, rule := range []string{RuleRenumber, "regenerate:command-that-does-not-exist", "regenerate:printf 'generated\\n' > shared.json; printf 'changed' > feature.txt"} {
		t.Run(rule, func(t *testing.T) {
			s := mechanicalRefresh(t, rule, false)
			mechanicalMerge(t, s, "generated\n", false)
			_, err := s.record("shared.json")
			if err == nil || !strings.Contains(err.Error(), "shared.json") || !strings.Contains(err.Error(), rule) || s.refreshRows() != 0 {
				t.Fatalf("unevaluable %s=%v", rule, err)
			}
		})
	}
}
