package dagsched

import (
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pluginversion"
)

// CRW-898 item 7: MaxRefreshHops counts the merges between the heads, not the steps of the chain. A
// version-only re-record is a step and never a merge, and it can only sit on a merge this chain
// proved, so a chain of MaxRefreshHops merges with version-only steps on top of them stays inside
// the bound, while one merge more is refused chain_too_long.
func TestBaseRefreshHopBoundCountsMergesOnly(t *testing.T) {
	t.Run("the bound still refuses one merge more than MaxRefreshHops", func(t *testing.T) {
		s := newRefreshScenario(t)
		s.openGeneration()
		for i := 0; i <= MaxRefreshHops; i++ {
			s.repo.commit("bulk.txt", strings.Repeat("x", i+1))
			s.head = s.mergeDev("merge dev", "", "")
		}
		s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, s.head)
		if _, err := s.record(); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "("+RefreshChainTooLong+")") {
			t.Fatalf("a chain of %d merges = %v, want chain_too_long", MaxRefreshHops+1, err)
		}
	})

	t.Run("MaxRefreshHops merges with a version-only step each stay inside the bound", func(t *testing.T) {
		// newPluginVersionRefreshScenario already opens generation 2 and leaves the checkout on dev
		s := newPluginVersionRefreshScenario(t, false)
		for i := 0; i < MaxRefreshHops; i++ {
			// dev changes the plugin payload, so the version the merge carries is no longer the derived one
			pluginVersionWrite(t, s.repo, pluginversion.PluginRelative+"/bulk-"+strconv.Itoa(i)+".txt", strings.Repeat("x", i+1))
			s.repo.git("add", "-A")
			s.repo.git("commit", "-q", "-m", "dev payload "+strconv.Itoa(i))
			s.head = s.mergeDev("merge dev", "", "")
			// a version-only re-record on top of each proved merge: a step, not a merge
			s.head = s.pluginVersionRecordOn(t, "record the version")
		}
		s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, s.head)
		got, err := s.record()
		if err != nil {
			t.Fatalf("%d merges with a version-only step each were refused: %v", MaxRefreshHops, err)
		}
		if got.HeadSHA != s.head || s.refreshRows() != 1 {
			t.Fatalf("the record = %+v rows=%d", got, s.refreshRows())
		}
		steps := pluginVersionStoredSteps(t, s)
		if len(steps) != 2*MaxRefreshHops {
			t.Fatalf("steps = %d, want %d (one merge and one version-only step each)", len(steps), 2*MaxRefreshHops)
		}
	})
}
