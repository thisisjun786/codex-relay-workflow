package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

func TestProjectLane1670ReusesLandAfterInRunFix(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	node := l.nodeOf["A-1"]
	l.needsChanges("A-1")
	original := l.outsideHead("A-1", "outside-a1")
	author := l.authorThread("claude-fake-sonnet", "high")
	landRun := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
	landed := landValueOf(t, l.runToEnd(landRun.RunID))
	if landed.State != "landed" || landed.Head == "" || landed.Head == original {
		t.Fatalf("land did not fix and land the outside head: %+v", landed)
	}
	if fixes := landFixCalls(l, node); len(fixes) != 1 {
		t.Fatalf("land-fix calls: %v, want one", fixes)
	}
	var release, commit, verify, accept, integrate engine.Event
	for _, e := range l.events() {
		if e.Kind != engine.KindCall || e.Event != engine.EvCompleted || str(e.Data, "node") != node {
			continue
		}
		switch e.Op {
		case ops.OpRelease:
			release = e
		case ops.OpCommit:
			commit = e
		case ops.OpVerify:
			verify = e
		case ops.OpAccept:
			accept = e
		case ops.OpIntegrate:
			integrate = e
		}
	}
	if str(release.Data, "kind") != "land" || str(release.Data, "head") != original ||
		str(commit.Data, "commit") != landed.Head || str(commit.Data, "tree") == "" ||
		str(verify.Data, "head") != landed.Head || verify.Data["ok"] != true || verify.Data["ran"] != true ||
		str(accept.Data, "head") != landed.Head || str(integrate.Data, "head") != landed.Head {
		t.Fatalf("incomplete land-fix evidence: release=%v commit=%v verify=%v accept=%v integrate=%v",
			release.Data, commit.Data, verify.Data, accept.Data, integrate.Data)
	}
	if !(release.Seq < commit.Seq && commit.Seq < verify.Seq && verify.Seq < accept.Seq && accept.Seq < integrate.Seq) {
		t.Fatalf("wrong land-fix ordering: release=%d commit=%d verify=%d accept=%d integrate=%d",
			release.Seq, commit.Seq, verify.Seq, accept.Seq, integrate.Seq)
	}
	checks := l.unitCheckRowsOf(node)
	if len(checks) != 2 || str(checks[0].Data, "verdict") != "needs_changes" || str(checks[1].Data, "verdict") != "verified" {
		t.Fatalf("land checks did not go needs_changes -> verified: %+v", checks)
	}
	sourceJournal := journalBytes(t, l, landRun.RunID)
	tip, integrations := l.git(l.repo, "rev-parse", l.target), l.verifications

	projectRun := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	l.closed(l.runToEnd(projectRun.RunID))
	reuses := reuseIntegratedRows(t, l)
	if len(reuses) != 1 {
		t.Fatalf("reuse rows: %+v, want the one landed node", reuses)
	}
	r := reuses[0]
	if r["reused"] != true || r["node"] != node || r["head"] != landed.Head ||
		r["source_run"] != landRun.RunID || r["source_kind"] != "land" ||
		r["source_release_call"] != release.CallID || r["source_accept_call"] != accept.CallID ||
		r["source_integrate_call"] != integrate.CallID || r["review_base"] != str(release.Data, "base") {
		t.Fatalf("reuse lost the fixed head or the land's provenance: %+v", r)
	}
	closes := 0
	for _, e := range l.events() {
		if e.Kind != engine.KindCall || e.Event != engine.EvCompleted {
			continue
		}
		switch e.Op {
		case ops.OpMilestoneClose:
			closes++
		case ops.OpRelease, ops.OpCommit, ops.OpAccept, ops.OpIntegrate:
			if str(e.Data, "node") == node {
				t.Errorf("reused node ran %s again", e.Op)
			}
		}
	}
	if closes != 1 {
		t.Errorf("milestone closes=%d, want one", closes)
	}
	for _, c := range labCalls(l) {
		if c.Role == "executor" || c.Role == "executor-t1" || strings.HasPrefix(c.Label, "build:") {
			t.Errorf("reuse started a new build: %+v", c)
		}
	}
	if l.git(l.repo, "rev-parse", l.target) != tip || l.verifications != integrations {
		t.Error("reuse moved the integration target or made another integration batch")
	}
	if !bytes.Equal(sourceJournal, journalBytes(t, l, landRun.RunID)) {
		t.Error("reuse changed the source journal")
	}
	l.effectsOnce("A-1")
}
