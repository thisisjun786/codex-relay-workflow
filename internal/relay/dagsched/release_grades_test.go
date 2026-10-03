package dagsched

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// Criteria c1 and c2 through the release path itself, not only the reading: the release judges again under its lock, with the managed engine, a held slot and an intent per node. Two nodes whose
// regions overlap only in mechanical or local grades are both released; an exclusive overlap refuses the second with the reason the release already had, and writes nothing for it.
func TestReleaseFollowsTheGradeOfAnOverlap(t *testing.T) {
	cases := []struct {
		name         string
		p, q         Region
		wantReleased bool
	}{
		{"mechanical against mechanical", gr("a.go", "mechanical", "union"), gr("a.go", "mechanical", "union"), true},
		{"local against local", gr("a.go", "local", ""), gr("a.go", "local", ""), true},
		{"mechanical against local", gr("a.go", "mechanical", "renumber"), gr("a.go", "local", ""), true},
		{"exclusive against local", gr("a.go", "exclusive", ""), gr("a.go", "local", ""), false},
		{"a shared contract path whatever the grade", gr("internal/relay/argparse/specs.json", "mechanical", "union"), gr("internal/relay/argparse/specs.json", "mechanical", "union"), false},
		{"no grade, as it was", gr("a.go", "", ""), gr("a.go", "", ""), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newReleaseKit(t)
			k.putPlan("gp", 0, "gp-r1", addRelNode("P", dag.NodeImplementation), addRelNode("Q", dag.NodeImplementation))
			for node, region := range map[string]Region{"P": c.p, "Q": c.q} {
				if _, err := k.sched.DeclareRegions(contextBackground(), "gp", node, "parent", []Region{region}); err != nil {
					t.Fatal(err)
				}
			}
			k.mustRelease("gp", "P")
			afterFirst := k.rows()
			second, err := k.release("gp", "Q")
			if c.wantReleased {
				if err != nil {
					t.Fatalf("release Q: %v", err)
				}
				if rows := k.rows(); rows.releases != 2 || rows.executions != 2 || rows.slots != 2 || rows.requests != 2 {
					t.Fatalf("rows = %+v, want an intent, an execution and a held slot for each of the two nodes", rows)
				}
				before := k.rows()
				if again := k.mustRelease("gp", "Q"); !again.Replayed || again.ManifestDigest != second.ManifestDigest || k.rows() != before {
					t.Fatalf("a repeat of the release = %+v, want a replay that writes nothing", again)
				}
				if created, _ := k.host.counts(); created != 2 {
					t.Fatalf("the host created %d children, want 2 and no third for the repeat", created)
				}
				return
			}
			if got := refusalReason(err); got != "region_overlap" {
				t.Fatalf("release Q: %v (%q), want region_overlap", err, got)
			}
			if !strings.Contains(err.Error(), "release rule defer") {
				t.Errorf("the refusal does not carry the rule: %v", err)
			}
			if rows := k.rows(); rows != afterFirst {
				t.Fatalf("rows = %+v after the refusal, want %+v: a refused release writes nothing", rows, afterFirst)
			}
			if created, _ := k.host.counts(); created != 1 {
				t.Fatalf("the host created %d children, want only P's", created)
			}
		})
	}
}
