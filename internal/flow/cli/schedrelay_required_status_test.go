package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func TestRequiredStatusMilestoneRepairKeepsTheReceipt(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	r := l.relayFor("run-repair-01", l.handoffBytes(nil), 1)
	ctx := context.Background()
	base := l.git(l.repo, "rev-parse", l.target)
	l.git(l.repo, "checkout", "-q", "-b", "fix", l.target)
	if err := os.WriteFile(filepath.Join(l.repo, "fix.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.git(l.repo, "add", "fix.txt")
	l.git(l.repo, "commit", "-q", "-m", "fix")
	fixed := l.git(l.repo, "rev-parse", "HEAD")
	keys, err := dagsched.CommitVerificationKeys(ctx, l.repo, fixed)
	if err != nil {
		t.Fatal(err)
	}
	record, err := dagsched.SealVerificationRecord(dagsched.VerificationRecord{
		Runner: "local", Repository: "o/r", BaseCommit: base, HeadCommit: fixed,
		TreeHash: keys.Tree, CiDigest: keys.CiDigest, Tools: map[string]string{}, Pins: keys.Pins,
		PinMismatch: []string{}, Dependencies: keys.Dependencies, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Result: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := dagsched.VerificationRecordDigest(record)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := r.IntegrateMilestoneRepair(ctx, ops.MilestoneRepairRequest{
		Plan: l.plan, Milestone: "M1", Base: base, Head: fixed,
		VerificationDigest: digest, Verification: record,
	})
	if err != nil || rep.Tip != fixed {
		t.Fatalf("repair: %+v %v", rep, err)
	}
	sched := &dagsched.Scheduler{Store: l.w.st}
	raw, gotBase, err := sched.VerifiedMoveRecord(ctx, l.target, fixed)
	if err != nil {
		t.Fatalf("the move kept no receipt: %v", err)
	}
	if string(raw) != string(record) || gotBase != base {
		t.Errorf("the retained receipt is not the repair's record")
	}
	count := func() int {
		rows, err := store.IntegrationStagesOfPlan(ctx, l.w.st, l.plan)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, s := range rows {
			if s.Stage == "intent" {
				var d map[string]string
				if json.Unmarshal([]byte(s.Detail), &d) == nil && d["verified_head"] == fixed {
					n++
				}
			}
		}
		return n
	}
	if got := count(); got != 1 {
		t.Fatalf("%d intents name the fixed head, want 1", got)
	}
	rep2, err := r.IntegrateMilestoneRepair(ctx, ops.MilestoneRepairRequest{
		Plan: l.plan, Milestone: "M1", Base: base, Head: fixed, VerificationDigest: digest,
	})
	if err != nil || rep2.Tip != fixed {
		t.Fatalf("a digest-only replay: %+v %v", rep2, err)
	}
	if got := count(); got != 1 {
		t.Errorf("a digest-only replay wrote a second intent: %d", got)
	}
	raw2, _, err := sched.VerifiedMoveRecord(ctx, l.target, fixed)
	if err != nil || string(raw2) != string(record) {
		t.Errorf("the replay shadowed the retained receipt: %v", err)
	}
}
