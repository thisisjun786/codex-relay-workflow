package skill

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

func relayWitness(t *testing.T, f *mechFixture, head string, regions []dagsched.Region, paths ...string) *dagsched.RefreshMechanicalRefusal {
	t.Helper()
	st := dagsched.RefreshStep{Previous: f.previous, BaseParent: f.devTip, Head: head, Tree: f.r.git("rev-parse", head+"^{tree}")}
	why, err := settleRelayRefresh(context.Background(), f.r.path, st, regions, paths)
	if err != nil {
		t.Fatal(err)
	}
	return why
}

func regenRegion(path, command string) dagsched.Region {
	return dagsched.Region{Repository: mechRepository, Path: path, Kind: "file", Grade: dagsched.GradeMechanical, Rule: "regenerate:" + command}
}

func TestRelayRegenerateParentPreservation(t *testing.T) {
	for _, kind := range []string{"owner serializer", "partly generated updater", "HEAD copy", "circular outputs", "failed command", "fully generated"} {
		t.Run(kind, func(t *testing.T) {
			f := newMechFixture(t)
			command := "sh regen.sh"
			if kind == "partly generated updater" {
				// This updater needs the existing file, but rebuilds only the version.
				f.r.git("checkout", "-q", "dev")
				f.put("regen.sh", "test -f plugin.json || exit 1\n"+mechRegenScript)
				f.r.git("add", "regen.sh")
				f.r.git("commit", "-q", "-m", "partial updater")
				f.r.git("checkout", "-q", "feature")
				f.r.git("merge", "-q", "--ff-only", "dev")
			}
			f.standard()
			if kind == "owner serializer" || kind == "partly generated updater" || kind == "HEAD copy" || kind == "circular outputs" {
				f.r.git("checkout", "-q", "feature")
				f.put("plugin.json", f.read("plugin.json")+"owner: old\n")
				f.r.git("add", "plugin.json")
				f.r.git("commit", "-q", "-m", "child handwritten field")
				f.previous = f.r.git("rev-parse", "HEAD")
				f.r.git("checkout", "-q", "dev")
				f.put("plugin.json", f.read("plugin.json")+"owner: new\n")
				f.r.git("add", "plugin.json")
				f.r.git("commit", "-q", "-m", "dev handwritten field")
				f.devTip = f.r.git("rev-parse", "HEAD")
			}
			regions := []dagsched.Region{regenRegion("plugin.json", command)}
			regions = append(regions, dagsched.Region{Repository: mechRepository, Path: "backlog.md", Kind: "file", Grade: dagsched.GradeMechanical, Rule: "union"})
			paths := []string{"backlog.md", "plugin.json"}
			if kind == "HEAD copy" {
				command = "git show HEAD:plugin.json > plugin.json"
			}
			if kind == "circular outputs" {
				command = "git show HEAD:plugin.json > plugin.json || git show HEAD:copy.json > plugin.json"
				// The peer belongs to a different command.
				for _, branch := range []string{"feature", "dev"} {
					f.r.git("checkout", "-q", branch)
					f.put("copy.json", f.read("plugin.json"))
					f.r.git("add", "copy.json")
					f.r.git("commit", "-q", "-m", "copied output")
					if branch == "feature" {
						f.previous = f.r.git("rev-parse", "HEAD")
					} else {
						f.devTip = f.r.git("rev-parse", "HEAD")
					}
				}
				regions = append(regions, regenRegion("copy.json", "cat plugin.json > copy.json"))
			}
			if kind == "failed command" {
				command = "command-that-does-not-exist"
			}
			regions[0] = regenRegion("plugin.json", command)
			f.startMerge()
			f.put("backlog.md", backlogBase+"- p1\n- p2\n- d1\n")
			f.regen()
			if kind == "circular outputs" {
				f.put("copy.json", f.read("plugin.json"))
			}
			head := f.finish()
			why := relayWitness(t, f, head, regions, paths...)
			if kind == "fully generated" {
				if why != nil {
					t.Fatalf("full generator: %+v", why)
				}
			} else if why == nil || why.Detail != "" || strings.Join(why.Manual, ",") != "plugin.json" {
				t.Fatalf("must classify as manual (not automatic or hard refusal): %+v", why)
			}
		})
	}
}
