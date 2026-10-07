package dagsched

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pluginversion"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// The plugin manifest's version line through the relay's own proof (CRW-732). The manifest is the one
// path the proof does not refuse when the head differs from git's clean three-way result without a
// conflict, and the built-in regenerate:plugin-version rule decides it whether or not a declaration
// covers it. The fixture is a real repository whose plugins/crw package records the version its
// payload derives: the accepted head and the dev tip each change a different skill file, so with only
// one side re-recording the manifest git merges it cleanly.

func pluginVersionManifestText(version, description string) string {
	return "{\n  \"name\": \"crw\",\n  \"version\": \"" + version + "\",\n  \"description\": \"" + description + "\"\n}\n"
}

func pluginVersionSkillText(name string) string {
	return "---\nname: " + name + "\ndescription: d\n---\n"
}

func pluginVersionWrite(t *testing.T, r *gitRepo, file, content string) {
	t.Helper()
	path := filepath.Join(r.path, filepath.FromSlash(file))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// pluginVersionRecordVersion is what crw-dev ci plugin --record-version does: it writes the version
// that names the payload of the work tree into the manifest and answers it.
func pluginVersionRecordVersion(t *testing.T, r *gitRepo) string {
	t.Helper()
	p, errs := pluginversion.DirectoryPayload(filepath.Join(r.path, filepath.FromSlash(pluginversion.PluginRelative)))
	if len(errs) > 0 {
		t.Fatalf("the plugin payload: %v", errs)
	}
	current, err := pluginversion.ManifestVersion(p)
	if err != nil {
		t.Fatal(err)
	}
	next, err := pluginversion.PayloadVersion(p, current)
	if err != nil {
		t.Fatal(err)
	}
	pluginVersionWrite(t, r, pluginversion.ManifestRepoPath, pluginVersionManifestText(next, "d"))
	return next
}

func pluginVersionCommit(t *testing.T, r *gitRepo, message string, files map[string]string, record bool) string {
	t.Helper()
	for file, content := range files {
		pluginVersionWrite(t, r, file, content)
	}
	if record {
		pluginVersionRecordVersion(t, r)
	}
	r.git("add", "-A")
	r.git("commit", "-q", "-m", message)
	return r.git("rev-parse", "HEAD")
}

// newPluginVersionRefreshScenario is the CRW-732 picture on the integration kit: node I is accepted at
// its head on the forge and its child opens generation 2 to refresh the base. With bothSidesRecord
// false only the dev tip re-records the version, so git merges the manifest cleanly and the head's
// re-recording is the clean difference the proof must pass on; with it true both sides re-record, so
// the manifest conflicts.
func newPluginVersionRefreshScenario(t *testing.T, bothSidesRecord bool) *refreshScenario {
	return newPluginVersionRefreshScenarioDeclaring(t, bothSidesRecord, nil)
}

// newPluginVersionRefreshScenarioDeclaring is newPluginVersionRefreshScenario with the candidate's
// declaration holding extra as well: the regions a node declares describe its pull request, so they
// are recorded before the acceptance.
func newPluginVersionRefreshScenarioDeclaring(t *testing.T, bothSidesRecord bool, extra []Region) *refreshScenario {
	t.Helper()
	k := newIntegrationKit(t)
	repo := k.repo
	for _, f := range []struct{ path, content string }{
		{pluginversion.ManifestRepoPath, pluginVersionManifestText("0.4.0", "d")},
		{pluginversion.PluginRelative + "/LICENSE", "MIT\n"},
		{pluginversion.PluginRelative + "/skills/crw-run/SKILL.md", pluginVersionSkillText("crw-run")},
		{pluginversion.PluginRelative + "/skills/crw-plan/SKILL.md", pluginVersionSkillText("crw-plan")},
	} {
		pluginVersionWrite(t, repo, f.path, f.content)
	}
	pluginVersionRecordVersion(t, repo)
	repo.git("add", "-A")
	repo.git("commit", "-q", "-m", "plugin package")
	repo.git("checkout", "-q", "-b", "feature")
	h1 := pluginVersionCommit(t, repo, "previous work", map[string]string{
		pluginversion.PluginRelative + "/skills/crw-plan/SKILL.md": pluginVersionSkillText("crw-plan") + "previous paragraph.\n",
	}, bothSidesRecord)
	repo.git("checkout", "-q", "dev")
	pluginVersionCommit(t, repo, "dev work", map[string]string{
		pluginversion.PluginRelative + "/skills/crw-run/SKILL.md": pluginVersionSkillText("crw-run") + "dev paragraph.\n",
	}, true)
	k.putPlan("g", 1, "g-r2", addRelNode("A", dag.NodeNonPR), addEdge("ia", "I", "A", dag.EdgeArtifactVerified, doc{"pins_code_head": true, "target_repository": forgeKitRepository, "target_base_ref": "dev"}))
	k.declare("g", "I", "feature.txt")
	if len(extra) > 0 {
		regions := append([]Region{}, extra...)
		for i := range regions {
			regions[i].Repository = forgeKitRepository
		}
		if _, err := k.sched.DeclareRegions(context.Background(), "g", "I", "parent", regions); err != nil {
			t.Fatal(err)
		}
	}
	a := k.acceptOnForge("g", "I", acceptOpts{HeadSHA: h1, PR: 7})
	k.holdSlotsFor("g", "I")
	n, _ := nodeOf(k.snapshot("g"), "I")
	k.forge.by["owner/repo#7"] = openPR("owner/repo", 7, h1)
	s := &refreshScenario{integrationKit: k, rid: a.Acceptance.RelationshipID, accepted: a, h1: h1, head: h1, criteria: n.CriteriaSetDigest, checkout: repo.path}
	s.openGeneration()
	return s
}

// pluginVersionMerge is the parent's update: merge dev into feature, record the version of the merged
// payload again, let mutate change the work tree further, and commit. expectConflict says whether git
// was meant to stop on the manifest (both sides re-recorded).
func (s *refreshScenario) pluginVersionMerge(t *testing.T, expectConflict bool, mutate func(t *testing.T, r *gitRepo, recorded string)) string {
	t.Helper()
	r := s.repo
	r.git("checkout", "-q", "feature")
	// --no-commit leaves the merge for the caller to commit after the manifest is recorded again,
	// so the head is one merge of exactly two parents.
	_, mergeErr := r.tryGit("merge", "-q", "--no-commit", "--no-ff", "-m", "Merge branch 'dev' into feature", "dev")
	if expectConflict != (mergeErr != nil) {
		t.Fatalf("the merge conflicted=%v, want %v: %v", mergeErr != nil, expectConflict, mergeErr)
	}
	pluginVersionWrite(t, r, pluginversion.ManifestRepoPath, pluginVersionManifestText("0.4.0", "d"))
	recorded := pluginVersionRecordVersion(t, r)
	if mutate != nil {
		mutate(t, r, recorded)
	}
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "Merge branch 'dev' into feature")
	s.head = r.git("rev-parse", "HEAD")
	r.git("checkout", "-q", "dev")
	s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, s.head)
	return s.head
}

// TestBaseRefreshPluginVersionCleanMerge is criteria c1 and c2 of CRW-732 on the relay path: a manifest
// that merged cleanly from two parents that changed different skill files and whose head re-recorded
// only the derived version is accepted, and the record names the built-in rule.
func TestBaseRefreshPluginVersionCleanMerge(t *testing.T) {
	s := newPluginVersionRefreshScenario(t, false)
	head := s.pluginVersionMerge(t, false, nil)
	got, err := s.record()
	if err != nil || got.Replayed || got.HeadSHA != head || len(got.Resolved) != 0 || s.refreshRows() != 1 {
		t.Fatalf("a clean-merge manifest recorded again = %v %+v rows=%d", err, got, s.refreshRows())
	}
	refreshRuleRecord(t, s, pluginversion.ManifestRepoPath, BuiltinPluginVersionRule)
}

// A conflicted manifest re-recorded to the version the head derives is accepted as well.
func TestBaseRefreshPluginVersionConflictedManifest(t *testing.T) {
	s := newPluginVersionRefreshScenario(t, true)
	head := s.pluginVersionMerge(t, true, nil)
	got, err := s.record()
	if err != nil || got.Replayed || got.HeadSHA != head || len(got.Resolved) != 0 || s.refreshRows() != 1 {
		t.Fatalf("a conflicted manifest recorded again = %v %+v rows=%d", err, got, s.refreshRows())
	}
	refreshRuleRecord(t, s, pluginversion.ManifestRepoPath, BuiltinPluginVersionRule)
}

// A declaration the candidate makes for the manifest does not become the built-in rule's mark: when
// the checker cannot prove the declared rule, the path stays one the parent has to name, and the
// record keeps it manual. Only the checker's own answer settles a path.
func TestBaseRefreshPluginVersionCandidateRuleIsNotOverriddenByTheBuiltin(t *testing.T) {
	s := newPluginVersionRefreshScenarioDeclaring(t, false, []Region{{Path: pluginversion.ManifestRepoPath, Kind: "file", Change: "edit", Grade: GradeMechanical, Rule: "regenerate:false"}})
	s.pluginVersionMerge(t, false, func(t *testing.T, r *gitRepo, recorded string) {
		pluginVersionWrite(t, r, pluginversion.ManifestRepoPath, pluginVersionManifestText("0.4.0+000000000000", "d"))
	})
	if _, err := s.record(); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), pluginversion.ManifestRepoPath) || s.refreshRows() != 0 {
		t.Fatalf("a declared rule the checker could not prove was marked built-in: %v rows=%d", err, s.refreshRows())
	}
	got, err := s.record(pluginversion.ManifestRepoPath)
	if err != nil || got.Replayed || s.refreshRows() != 1 {
		t.Fatalf("naming the file the parent read = %v %+v rows=%d", err, got, s.refreshRows())
	}
	refreshRuleRecord(t, s, pluginversion.ManifestRepoPath, "")
}

// TestBaseRefreshPluginVersionRefusals is the other half of c2: a non-derived version, another changed
// manifest line, a release change, a mode change and another file's clean difference all keep today's
// refusal, and nothing is written.
func TestBaseRefreshPluginVersionRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, r *gitRepo, recorded string)
		says   string
	}{
		{"a version that is not the one the head derives", func(t *testing.T, r *gitRepo, recorded string) {
			pluginVersionWrite(t, r, pluginversion.ManifestRepoPath, pluginVersionManifestText("0.4.0+000000000000", "d"))
		}, pluginversion.ManifestRepoPath},
		{"another line of the manifest differs from both parents", func(t *testing.T, r *gitRepo, recorded string) {
			pluginVersionWrite(t, r, pluginversion.ManifestRepoPath, pluginVersionManifestText(recorded, "another description"))
		}, pluginversion.ManifestRepoPath},
		{"the head records a release neither parent has", func(t *testing.T, r *gitRepo, recorded string) {
			pluginVersionWrite(t, r, pluginversion.ManifestRepoPath, pluginVersionManifestText("1.0.0", "d"))
			pluginVersionRecordVersion(t, r)
		}, pluginversion.ManifestRepoPath},
		{"the head makes the manifest executable", func(t *testing.T, r *gitRepo, recorded string) {
			pluginVersionWrite(t, r, pluginversion.ManifestRepoPath, pluginVersionManifestText("0.4.0", "d"))
			if err := os.Chmod(filepath.Join(r.path, filepath.FromSlash(pluginversion.ManifestRepoPath)), 0o755); err != nil {
				t.Fatal(err)
			}
			pluginVersionRecordVersion(t, r)
		}, pluginversion.ManifestRepoPath},
		{"another file's clean difference", func(t *testing.T, r *gitRepo, recorded string) {
			pluginVersionWrite(t, r, "smuggled.txt", "not from the base\n")
		}, "smuggled.txt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newPluginVersionRefreshScenario(t, false)
			s.pluginVersionMerge(t, false, c.mutate)
			_, err := s.record()
			if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "tree_differs") || !strings.Contains(err.Error(), c.says) || s.refreshRows() != 0 {
				t.Fatalf("refusal = %v rows=%d", err, s.refreshRows())
			}
		})
	}
}

// pluginVersionContributor lands a node on dev that re-records the manifest and declares the rule
// given for it, so the manifest has an identified contributing node whose declaration covers it. With
// a contributor the selector agrees on a declared rule and the path takes the declared-rule branch of
// applyMechanical rather than the built-in one, which is a different code path.
func pluginVersionContributor(t *testing.T, s *refreshScenario, rule string) {
	t.Helper()
	r := s.repo
	r.git("checkout", "-q", "-b", "plugin-contributor", "dev")
	pluginVersionCommit(t, r, "contributor work", map[string]string{
		// a skill file neither side of the refresh touches, so the contributor's landing conflicts
		// with nothing but the manifest's version line
		pluginversion.PluginRelative + "/skills/crw-check/SKILL.md": pluginVersionSkillText("crw-check"),
	}, true)
	region := Region{Repository: s.target(), Path: pluginversion.ManifestRepoPath, Kind: "file", Change: "edit", Grade: GradeMechanical, Rule: rule}
	if _, err := s.sched.DeclareRegions(context.Background(), "g", "D", "parent", []Region{region}); err != nil {
		t.Fatal(err)
	}
	s.acceptRefreshNode("D", acceptOpts{HeadSHA: r.git("rev-parse", "HEAD"), PR: 8})
	r.git("checkout", "-q", "dev")
	r.git("merge", "-q", "--no-ff", "-m", "land plugin contributor", "plugin-contributor")
}

// A declared rule that every declaration agrees on and that the checker cannot prove leaves the
// manifest to the parent's --resolved name: it is never stamped with the built-in rule. This is the
// identified-contributor form of the same invariant, and it reaches applyMechanical by a different
// branch (the selector agrees on the declared rule, so the path is not one the built-in rule is tried
// on).
// TestManifestRefreshDecisionIsTheRelaysOwn is criterion c1 item 6 on the producer itself: the relay
// decides, per path, whether the declarations settle the plugin manifest with one agreed rule or
// whether the built-in rule is what the checker is handed. A candidate-only declaration keeps its
// say (CRW-732, so an unprovable rule still leaves the path to the parent's --resolved name), but a
// declaration that an identified contributor disagrees with, by naming another rule or none, is no
// agreement at all: the decision is the built-in rule, so the candidate's lone command can never
// block the built-in proof. The fourth case is the one the pre-CRW-898 producer got wrong: it put
// the candidate's own rule into the decision, ran that command, and left the path manual.
func TestManifestRefreshDecisionIsTheRelaysOwn(t *testing.T) {
	manifest := func(rule string) Region {
		return Region{Path: pluginversion.ManifestRepoPath, Kind: "file", Change: "edit", Grade: GradeMechanical, Rule: rule}
	}
	builtin := RefreshDecisionBuiltin
	cases := []struct {
		name        string
		candidate   []Region
		contributor [][]Region
		want        string
	}{
		{"the candidate declares no rule and no contributor touches the path", nil, nil, builtin},
		{"the candidate alone declares a rule", []Region{manifest("regenerate:sh candidate.sh")}, nil, "regenerate:sh candidate.sh"},
		{"the candidate and the contributor agree", []Region{manifest("regenerate:sh same.sh")}, [][]Region{{manifest("regenerate:sh same.sh")}}, "regenerate:sh same.sh"},
		{"the contributor declares another rule", []Region{manifest("regenerate:sh candidate.sh")}, [][]Region{{manifest("regenerate:false")}}, builtin},
		{"the contributor declares no rule at all", []Region{manifest("regenerate:sh candidate.sh")}, [][]Region{{{Path: "other.txt", Kind: "file", Change: "edit", Grade: GradeMechanical, Rule: "union"}}}, builtin},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := manifestRefreshDecision(c.candidate, c.contributor); got != c.want {
				t.Fatalf("the decision for the manifest = %q, want %q", got, c.want)
			}
		})
	}
}

func TestBaseRefreshPluginVersionDeclaredRuleWithContributorStaysManual(t *testing.T) {
	s := newPluginVersionRefreshScenarioDeclaring(t, true, []Region{{Path: pluginversion.ManifestRepoPath, Kind: "file", Change: "edit", Grade: GradeMechanical, Rule: "regenerate:false"}})
	pluginVersionContributor(t, s, "regenerate:false")
	s.pluginVersionMerge(t, true, nil)
	if _, err := s.record(); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), pluginversion.ManifestRepoPath) || s.refreshRows() != 0 {
		t.Fatalf("an agreed declared rule the checker could not prove was marked built-in: %v rows=%d", err, s.refreshRows())
	}
	got, err := s.record(pluginversion.ManifestRepoPath)
	if err != nil || got.Replayed || s.refreshRows() != 1 {
		t.Fatalf("naming the file the parent read = %v %+v rows=%d", err, got, s.refreshRows())
	}
	refreshRuleRecord(t, s, pluginversion.ManifestRepoPath, "")
}
