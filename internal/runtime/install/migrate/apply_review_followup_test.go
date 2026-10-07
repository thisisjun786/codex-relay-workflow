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

// Control: the QA receipt name still takes the referrer's place, for a manifest this run can read and for one it cannot
// judge by content, so the dependencies-first order an unreadable record kept before is unchanged.
func TestMigrateApplyReviewFollowupKeepsTheQaReceiptOrder(t *testing.T) {
	for name, receipt := range map[string]string{
		"readable manifest":          "{\"artifactManifest\":[{\"path\":\"verdict.json\",\"kind\":\"verdict\"}]}",
		"manifest of the wrong type": "{\"artifactManifest\":\"verdict.json\"}",
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

// Control: a record under evidence/ that holds no manifest array is not a receipt, so its dependencies are not hoisted and
// the plan order stands. This is the case that keeps the content judgement from reordering every artifact group.
func TestMigrateApplyReviewFollowupKeepsPlanOrderWithoutAManifest(t *testing.T) {
	_, r, p := apPlan(t, map[string]string{
		"evidence/s/a.json":         "{\"verdict\":true}",
		"evidence/s/z/verdict.json": "v",
	}, nil)
	pub, leaves := migrateApplyReviewRenames(t)
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatal(err)
	}
	got := leaves()
	if a, v := slices.Index(got, "a.json"), slices.Index(got, "verdict.json"); a < 0 || v < 0 || a > v {
		t.Errorf("a record with no manifest array must keep the plan order: %v", got)
	}
}

// The two halves of the content judgement are pinned apart, because either one alone puts an artifact before the receipt
// that names it and would hide the other reverting to the name test.

// R1a, the ordering key half: the receipt itself is demoted to the referrer's place by its content, so a record under
// another name publishes after an unrelated record of its rank even though plan order puts the receipt first.
func TestMigrateApplyReviewFollowupDemotesAReceiptUnderAnotherName(t *testing.T) {
	_, r, p := apPlan(t, map[string]string{
		"evidence/s/a.json": "{\"artifactManifest\":[{\"path\":\"z/verdict.json\",\"kind\":\"verdict\"}]}",
		"evidence/s/b.json": "plain",
	}, nil)
	pub, leaves := migrateApplyReviewRenames(t)
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatal(err)
	}
	got := leaves()
	if b, a := slices.Index(got, "b.json"), slices.Index(got, "a.json"); b < 0 || a < 0 || b > a {
		t.Errorf("the receipt under another name must publish after the record of its rank: %v", got)
	}
}

// R1b, the manifest scan half: the artifact a receipt under another name lists is hoisted above a plain record of the same
// rank, which only the scan reading that receipt's content can do.
func TestMigrateApplyReviewFollowupHoistsTheArtifactOfAReceiptUnderAnotherName(t *testing.T) {
	_, r, p := apPlan(t, map[string]string{
		"evidence/s/a.json":         "{\"artifactManifest\":[{\"path\":\"z/verdict.json\",\"kind\":\"verdict\"}]}",
		"evidence/s/b.json":         "plain",
		"evidence/s/z/verdict.json": "v",
	}, nil)
	pub, leaves := migrateApplyReviewRenames(t)
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatal(err)
	}
	got := leaves()
	if v, b := slices.Index(got, "verdict.json"), slices.Index(got, "b.json"); v < 0 || b < 0 || v > b {
		t.Errorf("the artifact must be hoisted above the plain record: %v", got)
	}
}

// R1c: a record the receipt reader could not read is not judged by content, so the plan's own measurement decides it
// before anything is opened. The plan's size stands in for the file, so the case needs no record of that size.
func TestMigrateApplyReviewFollowupKeepsTheNameOrderPastTheReceiptReadBound(t *testing.T) {
	_, r, p := apPlan(t, map[string]string{
		"evidence/s/qa-receipt.json": "{\"artifactManifest\":[{\"path\":\"verdict.json\",\"kind\":\"verdict\"}]}",
	}, nil)
	it := apItem(t, &ApplyResult{Items: planItems(p)}, "evidence/s/qa-receipt.json").Item
	a := &applyRun{roots: r, plan: p, dirs: map[string]*Dir{}, srcs: map[string]*Dir{}, made: map[string]bool{}, result: &ApplyResult{}}
	defer a.close()
	if manifest, ok := a.migrateReviewFollowupReceiptManifest(it); !ok || len(manifest) != 1 {
		t.Fatalf("a record within the bound must be judged by content: %v %v", manifest, ok)
	}
	it.Size = migrateReviewFollowupReceiptReadCap + 1
	if manifest, ok := a.migrateReviewFollowupReceiptManifest(it); ok || manifest != nil {
		t.Errorf("a record past the receipt reader's bound must not be judged by content: %v %v", manifest, ok)
	}
}

// R1d: the bound is the bytes of the record itself, not the bytes up to the object's last token. The decoder consumes the
// whitespace after the object, so a record that grew past the bound after the plan measured it would otherwise be judged by
// content while the receipt reader refuses it. The helper takes the limit, so the case needs no record of that size.
func TestMigrateApplyReviewFollowupBoundsTheRecordNotTheObject(t *testing.T) {
	body := "{\"artifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}"
	for name, c := range map[string]struct {
		record string
		limit  int64
		want   bool
	}{
		"exactly the bound":             {body, int64(len(body)), true},
		"past the bound by whitespace":  {body + "\n", int64(len(body)), false},
		"past the bound by a long tail": {body + strings.Repeat(" ", 8), int64(len(body)), false},
		"whitespace within the bound":   {body + "\n", int64(len(body)) + 1, true},
		"truncated below the bound":     {body[:len(body)-1], int64(len(body)), false},
		"data after the object":         {body + "{}", int64(len(body)) + 2, false},
		"no manifest array":             {"{\"a\":1}", 8, false},
		// The receipt reader looks the key up by its exact spelling in a decoded object, so a key that differs only in
		// case is not the manifest it reads, and this order must not treat it as one.
		"key of another case": {"{\"ArtifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}", 200, false},
	} {
		t.Run(name, func(t *testing.T) {
			manifest, ok := migrateReviewFollowupDecodeManifest(strings.NewReader(c.record), c.limit)
			if ok != c.want {
				t.Errorf("ok = %v, want %v (record %d bytes, limit %d)", ok, c.want, len(c.record), c.limit)
			}
			if ok && len(manifest) != 1 {
				t.Errorf("manifest = %v, want one entry", manifest)
			}
			if !ok && manifest != nil {
				t.Errorf("a refused record must return no manifest: %v", manifest)
			}
		})
	}
}

// planItems wraps a plan's items as an ApplyResult so apItem can find one by source.
func planItems(p *Plan) []ApplyItem {
	items := make([]ApplyItem, len(p.Items))
	for i, it := range p.Items {
		items[i].Item = it
	}
	return items
}
