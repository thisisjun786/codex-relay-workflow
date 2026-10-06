package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pluginversion"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// The plugin manifest's version line through the relay's mechanical checker (CRW-732): the relay
// hands the manifest to settleRelayRefresh as a conflict, or as the one clean difference its proof
// does not refuse, and the built-in rule decides it there whether or not a declaration covers it.

func relayPluginVersionStep(f *pluginVersionFixture, head string) dagsched.RefreshStep {
	return dagsched.RefreshStep{Previous: f.previous, BaseParent: f.devTip, Head: head, Tree: f.r.git("rev-parse", head+"^{tree}")}
}

// mergeCleanlyRecordedAgain is the CRW-732 picture on this fixture: only the dev tip re-records the
// version, so git merges the manifest cleanly and the head's re-recording is a clean difference.
func (f *pluginVersionFixture) mergeCleanlyRecordedAgain() string {
	f.t.Helper()
	f.previous = f.commitOn("feature", "previous work", map[string]string{
		pluginversion.PluginRelative + "/skills/crw-plan/SKILL.md": pluginSkill("crw-plan") + "previous paragraph.\n"}, false)
	f.devTip = f.commitOn("dev", "dev work", map[string]string{
		pluginversion.PluginRelative + "/skills/crw-run/SKILL.md": pluginSkill("crw-run") + "dev paragraph.\n"}, true)
	f.r.git("checkout", "-q", "feature")
	f.r.git("merge", "-q", "--no-commit", "--no-ff", "-m", "Merge branch 'dev' into feature", "dev")
	f.put(pluginversion.ManifestRepoPath, pluginManifestText("0.4.0", "d"))
	f.record()
	f.r.git("add", "-A")
	f.r.git("commit", "-q", "-m", "Merge branch 'dev' into feature")
	return f.r.git("rev-parse", "HEAD")
}

// amendHead commits the work tree the mutation left as the head the checker is asked about.
func (f *pluginVersionFixture) amendHead() string {
	f.t.Helper()
	f.r.git("add", "-A")
	f.r.git("commit", "-q", "--amend", "--no-edit")
	return f.r.git("rev-parse", "HEAD")
}

// wantRelayPluginVersionSettled is the checker's answer for a manifest it settled by the built-in
// rule: no refusal detail, nothing manual, and the rule named for the path.
func wantRelayPluginVersionSettled(t *testing.T, f *pluginVersionFixture, head string, regions []dagsched.Region) {
	t.Helper()
	why, err := settleRelayRefresh(context.Background(), f.r.path, relayPluginVersionStep(f, head), regions, []string{pluginversion.ManifestRepoPath})
	if err != nil || why == nil || why.Detail != "" || len(why.Manual) != 0 {
		t.Fatalf("the manifest was not settled = %v %+v", err, why)
	}
	if why.Rules[pluginversion.ManifestRepoPath] != dagsched.BuiltinPluginVersionRule {
		t.Fatalf("the settled rule = %+v, want %s", why.Rules, dagsched.BuiltinPluginVersionRule)
	}
}

func TestRelayPluginVersionLine(t *testing.T) {
	t.Run("the manifest the head re-recorded after a clean merge is settled", func(t *testing.T) {
		f := newPluginVersionFixture(t)
		head := f.mergeCleanlyRecordedAgain()
		wantRelayPluginVersionSettled(t, f, head, nil)
	})
	t.Run("a conflicted manifest recorded again is settled", func(t *testing.T) {
		f := newPluginVersionFixture(t)
		f.standard()
		f.startMerge()
		head := f.resolve()
		wantRelayPluginVersionSettled(t, f, head, nil)
	})
	t.Run("an agreed declared rule keeps its say", func(t *testing.T) {
		f := newPluginVersionFixture(t)
		head := f.mergeCleanlyRecordedAgain()
		regions := []dagsched.Region{regenRegion(pluginversion.ManifestRepoPath, "false")}
		why, err := settleRelayRefresh(context.Background(), f.r.path, relayPluginVersionStep(f, head), regions, []string{pluginversion.ManifestRepoPath})
		if err != nil || why == nil || why.Detail != "" || strings.Join(why.Manual, ",") != pluginversion.ManifestRepoPath || len(why.Rules) != 0 {
			t.Fatalf("a declared rule = %v %+v, want the declared rule to leave the path manual", err, why)
		}
	})
	t.Run("a path the step did not settle is an error", func(t *testing.T) {
		f := newPluginVersionFixture(t)
		head := f.mergeCleanlyRecordedAgain()
		if _, err := settleRelayRefresh(context.Background(), f.r.path, relayPluginVersionStep(f, head), nil, []string{"notes.md"}); err == nil {
			t.Fatal("a path the step did not settle was accepted")
		}
	})
}

func TestRelayPluginVersionLineRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, f *pluginVersionFixture)
	}{
		{"a version that is not the one the head derives", func(t *testing.T, f *pluginVersionFixture) {
			f.put(pluginversion.ManifestRepoPath, pluginManifestText("0.4.0+000000000000", "d"))
		}},
		{"another line of the manifest differs from both parents", func(t *testing.T, f *pluginVersionFixture) {
			f.put(pluginversion.ManifestRepoPath, pluginManifestText(f.recorded, "another description"))
		}},
		{"the head records a release neither parent has", func(t *testing.T, f *pluginVersionFixture) {
			f.put(pluginversion.ManifestRepoPath, pluginManifestText("1.0.0", "d"))
			f.record()
		}},
		{"the head makes the manifest executable", func(t *testing.T, f *pluginVersionFixture) {
			f.put(pluginversion.ManifestRepoPath, pluginManifestText("0.4.0", "d"))
			if err := os.Chmod(filepath.Join(f.r.path, filepath.FromSlash(pluginversion.ManifestRepoPath)), 0o755); err != nil {
				t.Fatal(err)
			}
			f.record()
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPluginVersionFixture(t)
			f.mergeCleanlyRecordedAgain()
			c.mutate(t, f)
			head := f.amendHead()
			why, err := settleRelayRefresh(context.Background(), f.r.path, relayPluginVersionStep(f, head), nil, []string{pluginversion.ManifestRepoPath})
			if err != nil || why == nil || why.Detail == "" || !strings.Contains(why.Detail, pluginversion.ManifestRepoPath) {
				t.Fatalf("refusal = %v %+v, want a refusal naming the manifest", err, why)
			}
		})
	}
}
