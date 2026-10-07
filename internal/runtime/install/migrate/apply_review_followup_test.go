package migrate

// apply_review_followup_test.go holds the CRW-879 cases for the two findings the operator's pair evaluation of the
// state-copy apply left open after CRW-812: the dependency order judged a QA receipt by its file name instead of its
// content, and the write count inferred a completed rename from the destination's bytes. Each case names the behaviour it
// pins and fails on the code before CRW-879 for the reason its name gives.

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// R1: a receipt under another name still publishes after the artifact its own manifest names, so an interruption between
// the two publications cannot leave the reference dangling.
func TestMigrateApplyReviewFollowupPublishesTheArtifactOfAReceiptUnderAnotherName(t *testing.T) {
	_, r, p := apPlan(t, map[string]string{
		"evidence/s/a.json":         "{\"artifactManifest\":[{\"path\":\"z/verdict.json\",\"kind\":\"verdict\"}]}",
		"evidence/s/z/verdict.json": "v",
	}, nil)
	pub, leaves := migrateApplyReviewRenames(t)
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatal(err)
	}
	got := leaves()
	if v, i := slices.Index(got, "verdict.json"), slices.Index(got, "a.json"); v < 0 || i < 0 || v > i {
		t.Errorf("the artifact must publish before the receipt that names it: %v", got)
	}
}

// Control: the QA receipt name still takes the referrer's place, for a manifest this run can read and for one it cannot,
// so the dependencies-first order an unreadable record kept before is unchanged.
func TestMigrateApplyReviewFollowupKeepsTheQaReceiptOrder(t *testing.T) {
	for name, receipt := range map[string]string{
		"readable manifest": "{\"artifactManifest\":[{\"path\":\"verdict.json\",\"kind\":\"verdict\"}]}",
		"manifest too large to read": "{\"note\":\"" + strings.Repeat("x", attentionReadCap) +
			"\",\"artifactManifest\":[{\"path\":\"verdict.json\",\"kind\":\"verdict\"}]}",
	} {
		t.Run(name, func(t *testing.T) {
			_, r, p := apPlan(t, map[string]string{
				"evidence/s/qa-receipt.json": receipt,
				"evidence/s/verdict.json":    "v",
			}, nil)
			pub, leaves := migrateApplyReviewRenames(t)
			if _, err := applyWith(r, p, pub); err != nil {
				t.Fatal(err)
			}
			got := leaves()
			if v, i := slices.Index(got, "verdict.json"), slices.Index(got, "qa-receipt.json"); v < 0 || i < 0 || v > i {
				t.Errorf("the artifact must publish before the receipt that names it: %v", got)
			}
		})
	}
}

// Control: a directory sync failure after this run's own rename still counts the write and records the sync failure.
func TestMigrateApplyReviewFollowupCountsARenameThatThenLostItsSync(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}"}, nil)
	pub := newPub(t)
	seen := 0
	pub.at = func(step string) error {
		if step == "dirsync" {
			if seen++; seen == 2 { // 1 the canonical .gitignore, 2 the session file this run renamed
				return errApplyInterrupted
			}
		}
		return nil
	}
	res, err := applyWith(r, p, pub)
	if !errors.Is(err, errApplyInterrupted) {
		t.Fatalf("a failed directory sync must stop the run: %v", err)
	}
	if res.WritesCompleted != 1 {
		t.Errorf("the rename this run completed must be counted: %d", res.WritesCompleted)
	}
	if ai := apItem(t, res, "sessions/a.json"); !strings.Contains(ai.Note, "directory sync failed") {
		t.Errorf("the item must record the directory sync failure: %q", ai.Note)
	}
	if got := get(t, apDst(ws, "sessions/a.json")); got != "{\"phase\":\"P\"}" {
		t.Errorf("the final file must be whole: %q", got)
	}
}

// R2: a failure before this run's rename is not this run's write, whatever the destination holds. settle saw nothing, a
// competing writer publishes the same bytes and mode in the window that opens, and this run's own temporary create then
// fails with ENOSPC: nothing of this run's was renamed, so nothing may be counted and no directory sync failed.
func TestMigrateApplyReviewFollowupDoesNotCountAWriteItNeverRenamed(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}"}, nil)
	pub := newPub(t)
	real := pub.createTemp
	seen := 0
	pub.createTemp = func(dir *Dir, name string) (int, error) {
		if seen++; seen == 1 { // 1 the canonical .gitignore, 2 the session file under test
			return real(dir, name)
		}
		put(t, apDst(ws, "sessions/a.json"), "{\"phase\":\"P\"}", 0o644)
		return -1, unix.ENOSPC
	}
	res, err := applyWith(r, p, pub)
	if !errors.Is(err, unix.ENOSPC) {
		t.Fatalf("a failed temporary create must stop the run: %v", err)
	}
	if res.WritesCompleted != 0 {
		t.Errorf("a file this run never renamed must not be counted: %d", res.WritesCompleted)
	}
	if ai := apItem(t, res, "sessions/a.json"); ai.Note != "" {
		t.Errorf("no directory sync failed, so the item must carry no note: %q", ai.Note)
	}
	if got := get(t, apDst(ws, "sessions/a.json")); got != "{\"phase\":\"P\"}" {
		t.Errorf("the competing writer's file must stay: %q", got)
	}
}

// Control: a file past the bound the scan reads is not judged by content, so it keeps the plan order the name judgement
// gave it and its dependencies are not hoisted.
func TestMigrateApplyReviewFollowupKeepsPlanOrderPastTheReadBound(t *testing.T) {
	receipt := "{\"note\":\"" + strings.Repeat("x", attentionReadCap) +
		"\",\"artifactManifest\":[{\"path\":\"z/verdict.json\",\"kind\":\"verdict\"}]}"
	_, r, p := apPlan(t, map[string]string{
		"evidence/s/a.json":         receipt,
		"evidence/s/z/verdict.json": "v",
	}, nil)
	pub, leaves := migrateApplyReviewRenames(t)
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatal(err)
	}
	got := leaves()
	if a, v := slices.Index(got, "a.json"), slices.Index(got, "verdict.json"); a < 0 || v < 0 || a > v {
		t.Errorf("a record past the bound must keep the plan order: %v", got)
	}
}
