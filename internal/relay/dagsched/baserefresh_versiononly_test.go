package dagsched

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pluginversion"
)

// CRW-808: a single-parent commit that re-records only the plugin manifest's version line, sitting on
// a base merge already proved in the same chain, is accepted as a regenerate:plugin-version step. The
// shape is a clean base merge (whose tree is git's own merge result) landing with the base's version
// and the next commit recording the version the merged payload derives, because force-push is not
// allowed and the merge cannot be amended in place.

// pluginVersion808Shape merges dev into feature cleanly, records the version of the merged payload and
// commits it as a single-parent commit. mutate may change the work tree after the version is recorded
// and before the commit. It answers the merge and the new head, and leaves the checkout on dev.
func (s *refreshScenario) pluginVersion808Shape(t *testing.T, mutate func(t *testing.T, r *gitRepo, recorded string)) (merge, head string) {
	t.Helper()
	r := s.repo
	r.git("checkout", "-q", "feature")
	if _, err := r.tryGit("merge", "-q", "--no-ff", "-m", "Merge branch 'dev' into feature", "dev"); err != nil {
		t.Fatalf("the fixture's merge was meant to be clean: %v", err)
	}
	merge = r.git("rev-parse", "HEAD")
	recorded := pluginVersionRecordVersion(t, r)
	if mutate != nil {
		mutate(t, r, recorded)
	}
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "CRW-808: record the plugin version")
	head = r.git("rev-parse", "HEAD")
	s.head = head
	r.git("checkout", "-q", "dev")
	s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, head)
	return merge, head
}

// pluginVersionRecordOn records the version on the branch of the pull request and commits it as a
// single-parent commit whose parent is whatever the branch is at. It is how a version-only commit is
// placed on a commit that is not a proved merge.
func (s *refreshScenario) pluginVersionRecordOn(t *testing.T, message string) string {
	t.Helper()
	r := s.repo
	r.git("checkout", "-q", "feature")
	pluginVersionRecordVersion(t, r)
	r.git("add", "-A")
	r.git("commit", "-q", "-m", message)
	head := r.git("rev-parse", "HEAD")
	r.git("checkout", "-q", "dev")
	s.head = head
	s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, head)
	return head
}

// pluginVersionStoredSteps is the stored proof's steps, oldest first.
func pluginVersionStoredSteps(t *testing.T, s *refreshScenario) []RefreshStep {
	t.Helper()
	var encoded string
	if err := s.s.DB.QueryRow("SELECT proof_json FROM dag_base_refreshes").Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	var proof struct {
		Steps []struct {
			Previous   string `json:"previous"`
			BaseParent string `json:"base_parent"`
			Head       string `json:"head"`
			Tree       string `json:"tree"`
			Resolved   []struct {
				Path string `json:"path"`
				Blob string `json:"blob"`
				Rule string `json:"rule"`
			} `json:"resolved"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(encoded), &proof); err != nil {
		t.Fatal(err)
	}
	steps := make([]RefreshStep, len(proof.Steps))
	for i, st := range proof.Steps {
		steps[i] = RefreshStep{Previous: st.Previous, BaseParent: st.BaseParent, Head: st.Head, Tree: st.Tree}
		for _, r := range st.Resolved {
			steps[i].Resolved = append(steps[i].Resolved, RefreshResolved{Path: r.Path, Blob: r.Blob, Rule: r.Rule})
		}
	}
	return steps
}

// TestBaseRefreshVersionOnlyCommit is the CRW-808 accept case: the version-only commit right after the
// merge is taken, recorded as a regenerate:plugin-version step, and needs no --resolved.
func TestBaseRefreshVersionOnlyCommit(t *testing.T) {
	s := newPluginVersionRefreshScenario(t, false)
	merge, head := s.pluginVersion808Shape(t, nil)
	got, err := s.record()
	if err != nil || got.Replayed || got.HeadSHA != head || len(got.Resolved) != 0 || s.refreshRows() != 1 {
		t.Fatalf("a version-only commit after a proved merge = %v %+v rows=%d", err, got, s.refreshRows())
	}
	steps := pluginVersionStoredSteps(t, s)
	if len(steps) != 2 {
		t.Fatalf("steps = %d, want the merge and the version-only commit: %+v", len(steps), steps)
	}
	if steps[0].Head != merge || steps[0].BaseParent == "" {
		t.Fatalf("the first step is not the proved merge: %+v", steps[0])
	}
	if steps[1].Head != head || steps[1].Previous != merge || steps[1].BaseParent != "" {
		t.Fatalf("the second step is not the version-only commit: %+v", steps[1])
	}
	if len(steps[1].Resolved) != 1 || steps[1].Resolved[0].Path != pluginversion.ManifestRepoPath || steps[1].Resolved[0].Rule != BuiltinPluginVersionRule {
		t.Fatalf("the version-only step does not carry the built-in rule: %+v", steps[1].Resolved)
	}
	refreshRuleRecord(t, s, pluginversion.ManifestRepoPath, BuiltinPluginVersionRule)
}

// TestBaseRefreshVersionOnlyCommitRefusals is the other half: every shape the issue refuses stays
// refused, and nothing is written.
func TestBaseRefreshVersionOnlyCommitRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, r *gitRepo, recorded string)
		says   string
	}{
		{
			"a version that is not the one the commit derives",
			func(t *testing.T, r *gitRepo, recorded string) {
				pluginVersionWrite(t, r, pluginversion.ManifestRepoPath, pluginVersionManifestText("0.4.0+000000000000", "d"))
			},
			pluginversion.ManifestRepoPath,
		},
		{
			"another line of the manifest changed",
			func(t *testing.T, r *gitRepo, recorded string) {
				pluginVersionWrite(t, r, pluginversion.ManifestRepoPath, pluginVersionManifestText(recorded, "another description"))
			},
			pluginversion.ManifestRepoPath,
		},
		{
			"another file changed as well",
			func(t *testing.T, r *gitRepo, recorded string) {
				pluginVersionWrite(t, r, "notes.md", "a product change"+string(rune(10)))
			},
			"notes.md",
		},
		{
			"the manifest's file mode changed",
			func(t *testing.T, r *gitRepo, recorded string) {
				if err := os.Chmod(filepath.Join(r.path, filepath.FromSlash(pluginversion.ManifestRepoPath)), 0o755); err != nil {
					t.Fatal(err)
				}
				pluginVersionRecordVersion(t, r)
			},
			pluginversion.ManifestRepoPath,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newPluginVersionRefreshScenario(t, false)
			s.pluginVersion808Shape(t, c.mutate)
			_, err := s.record()
			if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), c.says) || s.refreshRows() != 0 {
				t.Fatalf("refusal = %v rows=%d, want a refusal naming %s", err, s.refreshRows(), c.says)
			}
			// naming the file does not turn a refused head into a refresh
			if _, err := s.record(pluginversion.ManifestRepoPath); err == nil || s.refreshRows() != 0 {
				t.Fatalf("a refused version-only commit was recorded by naming the file: %v rows=%d", err, s.refreshRows())
			}
		})
	}
}

// A version-only commit whose parent is not a merge proved in the chain is refused.
func TestBaseRefreshVersionOnlyCommitNeedsAProvedMerge(t *testing.T) {
	t.Run("the chain's first commit", func(t *testing.T) {
		s := newPluginVersionRefreshScenario(t, false)
		// the accepted head itself: the version is recorded there and committed, so the chain's first
		// commit is a single-parent one no merge proved
		head := s.pluginVersionRecordOn(t, "version only, directly on the accepted head")
		if head == s.h1 {
			t.Fatal("the fixture did not make a new commit")
		}
		_, err := s.record()
		if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), head) || s.refreshRows() != 0 {
			t.Fatalf("a version-only commit on the accepted head = %v rows=%d", err, s.refreshRows())
		}
	})
	t.Run("on top of another single-parent commit", func(t *testing.T) {
		s := newPluginVersionRefreshScenario(t, false)
		_, versionOnly := s.pluginVersion808Shape(t, nil)
		// a second single-parent commit whose parent is the version-only one, not a proved merge
		r := s.repo
		r.git("checkout", "-q", "feature")
		pluginVersionWrite(t, r, "notes.md", "a product change"+string(rune(10)))
		r.git("add", "-A")
		r.git("commit", "-q", "-m", "a product change on top of the version-only commit")
		second := r.git("rev-parse", "HEAD")
		s.head = second
		r.git("checkout", "-q", "dev")
		s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, second)
		if second == versionOnly {
			t.Fatal("the second commit is not on top of the version-only one")
		}
		if _, err := s.record(); refusalReason(err) != "disposition_conflict" || s.refreshRows() != 0 {
			t.Fatalf("a commit on another single-parent commit = %v rows=%d", err, s.refreshRows())
		}
	})
}

// A product change that records the version too is still refused, and the merge chain on its own is
// unchanged.
func TestBaseRefreshVersionOnlyCommitLeavesTheMergeChainAlone(t *testing.T) {
	s := newPluginVersionRefreshScenario(t, false)
	_, merge := s.pluginVersion808Shape(t, nil)
	// a further single-parent commit that changes a skill file and records the version: not the shape
	r := s.repo
	r.git("checkout", "-q", "feature")
	pluginVersionWrite(t, r, pluginversion.PluginRelative+"/skills/crw-check/SKILL.md", pluginVersionSkillText("crw-check")+"an extra paragraph."+string(rune(10)))
	pluginVersionRecordVersion(t, r)
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "a product change with the version recorded")
	head := r.git("rev-parse", "HEAD")
	s.head = head
	r.git("checkout", "-q", "dev")
	s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, head)
	if _, err := s.record(); refusalReason(err) != "disposition_conflict" || s.refreshRows() != 0 {
		t.Fatalf("a product change with the version recorded = %v rows=%d", err, s.refreshRows())
	}
	// the merge chain on its own is unchanged: the 808 shape without its version-only commit still
	// passes as a plain base merge
	s2 := newPluginVersionRefreshScenario(t, false)
	s2.pluginVersion808Shape(t, nil)
	if _, err := s2.record(); err != nil {
		t.Fatalf("the 808 shape needs no --resolved name: %v", err)
	}
	_ = merge
}
