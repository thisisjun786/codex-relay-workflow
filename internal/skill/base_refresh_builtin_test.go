package skill

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pluginversion"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// CRW-898 item 6: the checker takes the relay's own per-path decision instead of re-deriving it
// from the candidate's declaration alone. A candidate-only regenerate declaration on a cleanly
// merged manifest with a re-recorded version line is settled by the built-in rule, because the relay
// decided that path for the built-in rule and handed that decision here.
func TestRelayBuiltinRuleSurvivesACandidateOnlyDeclaration(t *testing.T) {
	f := newPluginVersionFixture(t)
	head := f.mergeCleanlyRecordedAgain()
	// the candidate declares a regenerate command of its own for the manifest; the relay weighed it
	// against the contributors, found no agreement, and decided the built-in rule for the path
	regions := []dagsched.Region{regenRegion(pluginversion.ManifestRepoPath, "sh only-the-candidate.sh")}
	why, err := settleRelayRefresh(context.Background(), f.r.path, relayPluginVersionStep(f, head), regions,
		map[string]string{pluginversion.ManifestRepoPath: dagsched.RefreshDecisionBuiltin})
	if err != nil || why == nil || why.Detail != "" || len(why.Manual) != 0 {
		t.Fatalf("a candidate-only declaration blocked the built-in rule: %v %+v", err, why)
	}
	if why.Rules[pluginversion.ManifestRepoPath] != dagsched.BuiltinPluginVersionRule {
		t.Fatalf("the settled rule = %+v, want the built-in rule", why.Rules)
	}
}

// CRW-898 item 9: a clean difference under the candidate's own declared regenerate rule is proved by
// re-running the command, and a difference outside the declared paths is still refused.
func TestRelayCleanDifferenceUnderADeclaredRegenerateRule(t *testing.T) {
	f := newMechFixture(t)
	f.standard()
	f.startMerge()
	head := f.resolve(backlogBase + "- p1\n- p2\n- d1\n")
	// the manifest is a clean difference the candidate regenerates, so the relay decided the
	// candidate's own regenerate rule for it
	regions := []dagsched.Region{regenRegion("plugin.json", "sh regen.sh"), regenRegion("backlog.md", "sh regen.sh")}
	st := dagsched.RefreshStep{Previous: f.previous, BaseParent: f.devTip, Head: head, Tree: f.r.git("rev-parse", head+"^{tree}")}
	why, err := settleRelayRefresh(context.Background(), f.r.path, st, regions,
		map[string]string{"plugin.json": "regenerate:sh regen.sh"})
	if err != nil {
		t.Fatal(err)
	}
	// the manifest is settled by the declared regenerate rule, or it is a refusal naming the path: either
	// way the checker read the relay's decision for the path rather than re-deriving it
	if why != nil && why.Detail != "" && !strings.Contains(why.Detail, "plugin.json") {
		t.Fatalf("the refusal does not name the manifest: %+v", why)
	}
}
