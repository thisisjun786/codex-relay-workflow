package migrate

// apply_review_test.go holds the CRW-812 cases for the five post-merge evaluation findings of the state-copy apply: the
// source recheck after a publication, the project-root initialization order, the second ordering key, the write count and
// the attention session field list. Each case names the behaviour it pins and fails on the code before CRW-812 for the
// reason its name gives.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// migrateApplyReviewRenames returns a publisher whose no-replace rename records every leaf it publishes, in order, and a
// reader for the
// recorded list. The real rename still runs, so the copy completes.
func migrateApplyReviewRenames(t *testing.T) (*Publisher, func() []string) {
	t.Helper()
	p := newPub(t)
	real := p.rename
	var leaves []string
	p.rename = func(dirfd int, oldName, newName string) error {
		leaves = append(leaves, newName)
		return real(dirfd, oldName, newName)
	}
	return p, func() []string { return slices.Clone(leaves) }
}

// F1: a source that gains a byte after its own publication is refused as a changed source and SourceVerified is false.
func TestMigrateApplyReviewRefusesASourceThatMovedAfterItsPublish(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "abc"}, nil)
	src := filepath.Join(ws, ProjectSourceName, "sessions", "a.json")
	pub := newPub(t)
	real := pub.rename
	pub.rename = func(dirfd int, oldName, newName string) error {
		if newName == "a.json" {
			// The temporary already holds the plan's bytes, so the write itself still lands them.
			put(t, src, "abcd", 0o644)
		}
		return real(dirfd, oldName, newName)
	}
	res, err := applyWith(r, p, pub)
	wantRefusal(t, err, applyReasonChanged)
	if res.SourceVerified {
		t.Error("a run that stopped on a moved source reported a verified source")
	}
	if ai := apItem(t, res, "sessions/a.json"); ai.Result != ResultRefused {
		t.Errorf("the moved source must fail its own file: %q", ai.Result)
	}
	if got := get(t, apDst(ws, "sessions/a.json")); got != "abc" {
		t.Errorf("the plan's bytes must still land: %q", got)
	}
}

// F1: a file published early that moves while a later file publishes is caught by the end-of-run recheck.
func TestMigrateApplyReviewRefusesASourceThatMovedDuringTheRun(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "abc", "sessions/b.json": "xyz"}, nil)
	src := filepath.Join(ws, ProjectSourceName, "sessions", "a.json")
	pub := newPub(t)
	real := pub.rename
	pub.rename = func(dirfd int, oldName, newName string) error {
		if newName == "b.json" {
			// a.json is already published and checked; only the end-of-run pass can still see this.
			put(t, src, "abcd", 0o644)
		}
		return real(dirfd, oldName, newName)
	}
	res, err := applyWith(r, p, pub)
	wantRefusal(t, err, applyReasonChanged)
	if res.SourceVerified {
		t.Error("a run that stopped on a moved source reported a verified source")
	}
	if ai := apItem(t, res, "sessions/a.json"); ai.Result != ResultRefused {
		t.Errorf("the moved source must fail its own file: %q", ai.Result)
	}
}

// F2: a .gitignore a racer makes while the root is created is an initialization race, so the copy stops before any state.
func TestMigrateApplyReviewRefusesAnIgnoreRaceInANewRoot(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}"}, nil)
	put(t, apDst(ws, ".gitignore"), "mine", 0o600) // the racer creates the root and its ignore after Open
	res, err := apply(r, p)
	wantRefusal(t, err, ReasonDiffers)
	if res.SourceVerified {
		t.Error("a stopped run reported a verified source")
	}
	if _, err := os.Lstat(apDst(ws, "sessions/a.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the state copy must not start: %v", err)
	}
	if got := get(t, apDst(ws, ".gitignore")); got != "mine" {
		t.Errorf("the racer's .gitignore changed: %q", got)
	}
}

// F2: an owner .gitignore that was there when Open ran is retained, so the run still succeeds.
func TestMigrateApplyReviewKeepsAnOwnersIgnore(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}"}, func(src string) {
		put(t, filepath.Join(filepath.Dir(src), crwdir.DirName, ".gitignore"), "mine", 0o600)
	})
	res, err := apply(r, p)
	must(t, err)
	if !res.SourceVerified {
		t.Error("the run must verify its sources")
	}
	if got := get(t, apDst(ws, ".gitignore")); got != "mine" {
		t.Errorf("the owner's .gitignore changed: %q", got)
	}
}

// F3: the artifact files an evidence manifest names publish before the receipt that names them.
func TestMigrateApplyReviewPublishesEvidenceDependenciesFirst(t *testing.T) {
	receipt := "{\"artifactManifest\":[{\"path\":\"verdict.json\",\"kind\":\"verdict\"}," +
		"{\"path\":\"artifact-identity.json\",\"kind\":\"artifact-identity\"}]}"
	_, r, p := apPlan(t, map[string]string{
		"evidence/s/qa-receipt.json":        receipt,
		"evidence/s/verdict.json":           "v",
		"evidence/s/artifact-identity.json": "i",
	}, nil)
	pub, leaves := migrateApplyReviewRenames(t)
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatal(err)
	}
	got := leaves()
	for _, artifact := range []string{"verdict.json", "artifact-identity.json"} {
		if a, i := slices.Index(got, artifact), slices.Index(got, "qa-receipt.json"); a < 0 || i < 0 || a > i {
			t.Errorf("%s must publish before the receipt that names it: %v", artifact, got)
		}
	}
}

// F3: the config backup publishes before the install record whose backupPath names it.
func TestMigrateApplyReviewPublishesTheBackupBeforeTheInstallRecord(t *testing.T) {
	base := isolate(t)
	home := filepath.Join(base, "codex")
	mkdirs(t, home)
	put(t, filepath.Join(home, installSource), "{\"version\":2}", 0o644)
	put(t, filepath.Join(home, backupSource+"2026"+backupSuffix), "{\"config\":true}", 0o644)
	r, err := Open(Options{Scope: ScopeCodex, CodexHome: home})
	must(t, err)
	t.Cleanup(func() { _ = r.Close() })
	p, err := classify(r)
	must(t, err)
	pub, leaves := migrateApplyReviewRenames(t)
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatal(err)
	}
	got := leaves()
	backup, install := backupDest+"2026"+backupSuffix, installDest
	if b, i := slices.Index(got, backup), slices.Index(got, install); b < 0 || i < 0 || b > i {
		t.Errorf("the config backup must publish before the install record: %v", got)
	}
}

// F4: a file this run never wrote is not counted, even when its directory sync fails.
func TestMigrateApplyReviewDoesNotCountAnEqualFilesSyncFailure(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}"}, nil)
	put(t, apDst(ws, ".gitignore"), crwdir.GitignoreText, 0o644)
	put(t, apDst(ws, "sessions/a.json"), "{\"phase\":\"P\"}", 0o644)
	pub := newPub(t)
	seen := 0
	pub.at = func(step string) error {
		if step == "dirsync" {
			if seen++; seen == 2 { // 1 the canonical .gitignore, 2 the already-equal session file
				return errApplyInterrupted
			}
		}
		return nil
	}
	res, err := applyWith(r, p, pub)
	if err == nil {
		t.Fatal("a failed directory sync must stop the run")
	}
	if res.WritesCompleted != 0 {
		t.Errorf("a file this run never wrote must not be counted: %d", res.WritesCompleted)
	}
	if ai := apItem(t, res, "sessions/a.json"); ai.Result == ResultCopied {
		t.Errorf("the equal file must not be reported copied: %q", ai.Result)
	}
}

// F5: a claimed receipt path under the old root raises old-root-ref.
func TestMigrateApplyReviewReportsAReceiptClaimedUnderTheOldRoot(t *testing.T) {
	body := "{\"phase\":\"P\",\"unverifiedSubagents\":[{\"agentId\":\"a\"," +
		"\"receiptClaimed\":\"old/.codexclaw/evidence/r/test-receipt.json\"}]}"
	_, r, p := apPlan(t, map[string]string{"sessions/rec-1.json": body}, nil)
	got := attention(r, p)
	if !attHas(got, "sessions/rec-1.json", AttentionOldRoot, "unverifiedSubagents.*.receiptClaimed") {
		t.Fatalf("no old-root-ref entry for the claimed receipt: %v", got)
	}
}

// F5: the listed field is a path string, so a value of another type stays opaque.
func TestMigrateApplyReviewIgnoresANonStringReceiptClaimed(t *testing.T) {
	body := "{\"phase\":\"P\",\"unverifiedSubagents\":[{\"receiptClaimed\":true}]}"
	_, r, p := apPlan(t, map[string]string{"sessions/rec-1.json": body}, nil)
	if attHas(attention(r, p), "sessions/rec-1.json", AttentionOldRoot, "") {
		t.Error("a non-string receiptClaimed must not be reported")
	}
}
