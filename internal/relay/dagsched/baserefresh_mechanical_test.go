package dagsched

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The production checker is bootstrapped by the external test package, not mocked.
func mechanicalRefresh(t *testing.T, rule string, mixed bool, declared ...Region) *refreshScenario {
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
	if len(declared) == 0 {
		declared = []Region{gr("shared.json", GradeMechanical, rule)}
	}
	if _, err := k.sched.DeclareRegions(context.Background(), "g", "I", "parent", declared); err != nil {
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

func TestBaseRefreshMechanicalLegacyReplayAndUnavailableChecker(t *testing.T) {
	s := mechanicalRefresh(t, RuleUnion, false)
	mechanicalMerge(t, s, "base\nchild\ndev\n", false)
	checker := refreshMechanical
	refreshMechanical = nil
	t.Cleanup(func() { refreshMechanical = checker })
	if _, err := s.record(); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("unlinked checker silently accepted: %v", err)
	}
	legacy, err := s.record("shared.json")
	if err != nil {
		t.Fatal(err)
	}
	refreshRuleRecord(t, s, "shared.json", "")
	calls := 0
	refreshMechanical = func(context.Context, string, RefreshStep, []Region, []string) (*RefreshMechanicalRefusal, error) {
		calls++
		t.Error("immutable replay invoked checker")
		return nil, nil
	}
	replay, err := s.record("shared.json")
	if err != nil || !replay.Replayed || replay.RefreshID != legacy.RefreshID || calls != 0 {
		t.Fatalf("legacy replay=%v %+v calls=%d", err, replay, calls)
	}
	if _, err := s.record(); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("legacy replay lost its names: %v", err)
	}
}

func TestBaseRefreshMechanicalStaleEpochNeverEvaluates(t *testing.T) {
	s := mechanicalRefresh(t, "regenerate:printf 'generated\\n' > shared.json", false)
	mechanicalMerge(t, s, "generated\n", false)
	if _, err := s.sched.ClaimEpoch(context.Background(), "g", ClaimInput{Actor: "parent", SessionNonce: "fresh"}); err != nil {
		t.Fatal(err)
	}
	checker := refreshMechanical
	calls := 0
	refreshMechanical = func(context.Context, string, RefreshStep, []Region, []string) (*RefreshMechanicalRefusal, error) {
		calls++
		return nil, nil
	}
	t.Cleanup(func() { refreshMechanical = checker })
	_, err := s.record()
	if refusalReason(err) != "stale_coordinator_epoch" || calls != 0 || s.refreshRows() != 0 {
		t.Fatalf("stale epoch=%v calls=%d", err, calls)
	}
}

func TestBaseRefreshMechanicalCoverageKeepsManualNames(t *testing.T) {
	symbol := gr("shared.json", GradeMechanical, RuleUnion)
	symbol.Kind, symbol.Key = "symbol", "part"
	other := gr("shared.json", GradeMechanical, RuleUnion)
	other.Repository = "owner/other"
	local := gr("shared.json", GradeLocal, "")
	overlap := local
	overlap.Kind, overlap.Key = "symbol", "another"
	cases := map[string][]Region{"symbol": {symbol}, "another repository": {other}, "local": {local}, "overlapping local symbol": {gr("shared.json", GradeMechanical, RuleUnion), overlap}}
	for name, regions := range cases {
		t.Run(name, func(t *testing.T) {
			s := mechanicalRefresh(t, RuleUnion, false, regions...)
			mechanicalMerge(t, s, "hand resolution\n", false)
			if _, err := s.record(); refusalReason(err) != "disposition_conflict" {
				t.Fatalf("uncovered=%v", err)
			}
			if _, err := s.record("shared.json"); err != nil {
				t.Fatal(err)
			}
			refreshRuleRecord(t, s, "shared.json", "")
		})
	}
}

func TestBaseRefreshMechanicalPathsAggregateManualHops(t *testing.T) {
	proof := refreshProof{Steps: []RefreshStep{{Resolved: []RefreshResolved{{Path: "a", Rule: RuleUnion}, {Path: "b"}}}, {Resolved: []RefreshResolved{{Path: "a"}, {Path: "b", Rule: RuleUnion}}}}}
	if got := strings.Join(proof.resolvedPaths(), ","); got != "a,b" {
		t.Fatalf("manual union across hops=%s", got)
	}
}

func TestBaseRefreshMechanicalMultiHop(t *testing.T) {
	s := mechanicalRefresh(t, RuleUnion, false)
	mechanicalMerge(t, s, "base\nchild\ndev\n", false)
	s.repo.commit("later.txt", "later base")
	s.head = s.mergeDev("another base merge", "", "")
	s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, s.head)
	got, err := s.record()
	if err != nil || len(got.Steps) != 2 || len(got.Resolved) != 0 {
		t.Fatalf("two-hop=%v %+v", err, got)
	}
	refreshRuleRecord(t, s, "shared.json", RuleUnion)
}

func TestBaseRefreshMechanicalSerialization(t *testing.T) {
	s := mechanicalRefresh(t, RuleUnion, true)
	mechanicalMerge(t, s, "base\nchild\ndev\n", true)
	res, err := s.record("manual.txt")
	if err != nil {
		t.Fatal(err)
	}
	var proof string
	if err := s.s.DB.QueryRow("SELECT proof_json FROM dag_base_refreshes").Scan(&proof); err != nil {
		t.Fatal(err)
	}
	// The structural proof and CLI output keep all blobs, while only the manual file is named.
	var output bytes.Buffer
	if err := contract.Emit(&output, baseRefreshObject(res)); err != nil {
		t.Fatal(err)
	}
	data := output.Bytes()
	opts := []golden.Option{golden.Substitute(res.AcceptanceID, "<ACCEPTANCE>"), golden.Substitute(res.RefreshID, "<REFRESH>"), golden.Substitute(res.EventID, "<EVENT>"), golden.Substitute(s.repo.path, "<REPOSITORY>")}
	for _, st := range res.Steps {
		opts = append(opts, golden.Substitute(st.Previous, "<PREVIOUS>"), golden.Substitute(st.BaseParent, "<BASE>"), golden.Substitute(st.Head, "<HEAD>"), golden.Substitute(st.Tree, "<TREE>"))
	}
	golden.Check(t, "cli", data, opts...)
	golden.Check(t, "stored-proof", []byte(proof), opts...)
	replay, err := s.record("manual.txt")
	if err != nil {
		t.Fatal(err)
	}
	if replay.Steps[0].Resolved[1].Rule != RuleUnion {
		t.Fatalf("rule disappeared in replay: %+v", replay.Steps)
	}
}

func TestBaseRefreshMechanicalDeclarationChangeRefused(t *testing.T) {
	s := mechanicalRefresh(t, RuleUnion, false)
	mechanicalMerge(t, s, "base\nchild\ndev\n", false)
	// Append a synthetic competing declaration during the unlocked proof.
	s.sched.testBeforeRefreshTx = func() {
		s.exec("INSERT INTO dag_node_regions (plan_id,node_id,declaration_seq,repository,path,region_kind,region_key,change,exclusive,declared_by,declared_at) SELECT plan_id,node_id,declaration_seq+1,repository,'other.txt',region_kind,region_key,change,exclusive,declared_by,declared_at FROM dag_node_regions WHERE plan_id='g' AND node_id='I'")
	}
	_, err := s.record()
	if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "regions") || s.refreshRows() != 0 {
		t.Fatalf("changed declaration=%v", err)
	}
}

func TestBaseRefreshMechanicalEvaluationErrorIsUnreadable(t *testing.T) {
	s := mechanicalRefresh(t, RuleUnion, false)
	mechanicalMerge(t, s, "base\nchild\ndev\n", false)
	checker := refreshMechanical
	refreshMechanical = func(ctx context.Context, repo string, st RefreshStep, regions []Region, paths []string) (*RefreshMechanicalRefusal, error) {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		return checker(canceled, repo, st, regions, paths)
	}
	t.Cleanup(func() { refreshMechanical = checker })
	_, err := s.record()
	if refusalReason(err) != "merge_target_unreadable" || !strings.Contains(err.Error(), "shared.json") || !strings.Contains(err.Error(), RuleUnion) || s.refreshRows() != 0 {
		t.Fatalf("evaluation error=%v", err)
	}
}

func TestBaseRefreshMechanicalRegenerateEachHop(t *testing.T) {
	rule := "regenerate:cat recipe.txt > shared.json"
	s := mechanicalRefresh(t, rule, false)
	s.repo.commit("recipe.txt", "generated one\n")
	mechanicalMerge(t, s, "generated one\n", false)
	first := s.head
	s.repo.commit("recipe.txt", "generated two\n")
	s.repo.commit("shared.json", "second base version\n")
	s.head = s.mergeDev("second regenerated hop", "shared.json", "generated two\n")
	s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, s.head)
	got, err := s.record()
	if err != nil || len(got.Steps) != 2 || got.Steps[0].Head != first || len(got.Resolved) != 0 {
		t.Fatalf("regenerated chain=%v %+v", err, got)
	}
	for _, st := range got.Steps {
		if len(st.Resolved) != 1 || st.Resolved[0].Rule != rule {
			t.Fatalf("missing per-hop rule: %+v", st)
		}
	}
}
