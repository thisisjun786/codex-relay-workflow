package manage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	crw838JSONFirst  = "{\"schema\":\"crw-audit-result/1\",\"criteria\":[{\"id\":\"c1\",\"verdict\":\"PASS\",\"note\":\"n\"}],\"defects\":[{\"severity\":\"P1\",\"what\":\"first defect\",\"where\":\"a.go:2\",\"repro\":\"run it\"}],\"score\":6}"
	crw838JSONSecond = "{\"schema\":\"crw-audit-result/1\",\"criteria\":[{\"id\":\"c1\",\"verdict\":\"FAIL\",\"note\":\"n\"}],\"defects\":[{\"severity\":\"P1\",\"what\":\"second defect\",\"where\":\"b.go:9\",\"repro\":\"run it\"}],\"score\":3}"
)

// crw838Env is an Env whose clock the test moves, so two grades of one bundle carry two
// graded_at values.
func crw838Env(t *testing.T, now *time.Time) *Env {
	t.Helper()
	e, _, _ := auditTestEnv(t)
	e.Now = func() time.Time { return *now }
	return e
}

func crw838Grade(t *testing.T, e *Env, cfg *Config, bundle string) AuditResult {
	t.Helper()
	results, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: bundle}})
	if err != nil {
		t.Fatalf("AuditGrade: %v", err)
	}
	if len(results) != 1 || results[0].Status != auditStatusOK {
		t.Fatalf("results = %+v, want one ok", results)
	}
	return results[0]
}

func crw838Rows(t *testing.T, state string) []map[string]json.RawMessage {
	t.Helper()
	return auditLines(t, filepath.Join(state, "audit", auditLedgerFile))
}

func crw838String(t *testing.T, row map[string]json.RawMessage, key string) string {
	t.Helper()
	var out string
	if raw, ok := row[key]; ok {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
	}
	return out
}

// The row id is the first sixteen hex characters of sha256(target "\n" head "\n" gradedAt),
// the copy is at <audit state>/results/<id>.json, and the row names the copy's sha256.
func TestCRW838OkRowCarriesAnIDAndAResultCopy(t *testing.T) {
	state := t.TempDir()
	now := time.Date(2026, 10, 10, 1, 2, 3, 0, time.UTC)
	e := crw838Env(t, &now)
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditFake(t, "json", crw838JSONFirst)})
	bundle := auditGoodBundle(t, auditModePR)
	crw838Grade(t, e, cfg, bundle)
	rows := crw838Rows(t, state)
	if len(rows) != 1 {
		t.Fatalf("the ledger has %d rows", len(rows))
	}
	sum := sha256.Sum256([]byte("s\nh\n2026-10-10T01:02:03Z"))
	wantID := hex.EncodeToString(sum[:])[:16]
	if got := crw838String(t, rows[0], "id"); got != wantID {
		t.Fatalf("id = %q, want %q", got, wantID)
	}
	copyPath := filepath.Join(state, "audit", "results", wantID+".json")
	data, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatalf("the result copy: %v", err)
	}
	if string(data) != crw838JSONFirst {
		t.Errorf("the copy is %q, want the grade.json bytes", data)
	}
	digest := sha256.Sum256(data)
	if got := crw838String(t, rows[0], "result_sha256"); got != hex.EncodeToString(digest[:]) {
		t.Errorf("result_sha256 = %q, want the digest of the copy", got)
	}
	if got := crw838String(t, rows[0], "result"); got != copyPath {
		t.Errorf("result = %q, want %q", got, copyPath)
	}
	info, err := os.Stat(copyPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the copy mode is %v (%v), want 0600", info, err)
	}
	// An invalid or timed out run has no result to copy and no id.
	cfgBad := auditSectionConfig(t, t.TempDir(), map[string]any{"grader": auditFake(t, "nofile", "")})
	crw838Grade2 := func() {
		if _, err := AuditGrade(context.Background(), e, cfgBad, []AuditJob{{Bundle: auditGoodBundle(t, auditModePR)}}); err != nil {
			t.Fatal(err)
		}
	}
	crw838Grade2()
	bad := auditLines(t, filepath.Join(cfgBad.StateDir, "audit", auditLedgerFile))
	if _, has := bad[0]["id"]; has {
		t.Errorf("a run without a result carries an id: %v", bad[0])
	}
	if entries, err := os.ReadDir(filepath.Join(cfgBad.StateDir, "audit", "results")); err == nil && len(entries) != 0 {
		t.Errorf("a run without a result left copies: %v", entries)
	}
}

// Grading the same bundle again leaves the first row's copy as it was, and the first row
// keeps its own result: the drafts surface drafts from both rows.
func TestCRW838RegradingABundleKeepsTheFirstCopy(t *testing.T) {
	state := t.TempDir()
	now := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	e := crw838Env(t, &now)
	t.Setenv("AUDIT_FAKE", "json")
	t.Setenv("AUDIT_JSON", crw838JSONFirst)
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditFakeGrader(t)})
	bundle := auditGoodBundle(t, auditModePR)
	crw838Grade(t, e, cfg, bundle)
	first := crw838String(t, crw838Rows(t, state)[0], "id")
	firstBytes, err := os.ReadFile(filepath.Join(state, "audit", "results", first+".json"))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	t.Setenv("AUDIT_JSON", crw838JSONSecond)
	crw838Grade(t, e, cfg, bundle)
	rows := crw838Rows(t, state)
	if len(rows) != 2 {
		t.Fatalf("the ledger has %d rows", len(rows))
	}
	second := crw838String(t, rows[1], "id")
	if second == first {
		t.Fatalf("two grades share the id %s", first)
	}
	after, err := os.ReadFile(filepath.Join(state, "audit", "results", first+".json"))
	if err != nil || string(after) != string(firstBytes) || string(after) != crw838JSONFirst {
		t.Errorf("the first copy changed: %q (%v)", after, err)
	}
	if live, _ := os.ReadFile(filepath.Join(bundle, auditGradeFile)); string(live) != crw838JSONSecond {
		t.Errorf("the bundle's grade.json is %q, want the second result", live)
	}
	// Drafts read the rows' own copies.
	report := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(report.Created) != 2 || len(report.Skipped) != 0 {
		t.Fatalf("drafts created %d and skipped %+v, want both rows drafted from their own copies", len(report.Created), report.Skipped)
	}
	titles := report.Created[0].Title + "|" + report.Created[1].Title
	if !strings.Contains(titles, "first defect") || !strings.Contains(titles, "second defect") {
		t.Errorf("the drafts are %q", titles)
	}
}

// A copy that does not match the row's digest is not read: the row is named and skipped.
func TestCRW838DraftsRefuseACopyThatDoesNotMatchTheRow(t *testing.T) {
	state := t.TempDir()
	now := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	e := crw838Env(t, &now)
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditFake(t, "json", crw838JSONFirst)})
	crw838Grade(t, e, cfg, auditGoodBundle(t, auditModePR))
	id := crw838String(t, crw838Rows(t, state)[0], "id")
	path := filepath.Join(state, "audit", "results", id+".json")
	if err := os.WriteFile(path, []byte(crw838JSONSecond), 0o600); err != nil {
		t.Fatal(err)
	}
	report := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(report.Created) != 0 || len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0].Reason, "sha256") {
		t.Errorf("created %d, skipped %+v, want the row skipped for its digest", len(report.Created), report.Skipped)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	report = auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(report.Created) != 0 || len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0].Reason, "result copy") {
		t.Errorf("created %d, skipped %+v, want the row skipped for its missing copy", len(report.Created), report.Skipped)
	}
}

// The copy is written before the row and atomically: a copy that cannot be made keeps its row
// out of the ledger; a finished copy is whole, with no temporary file beside it; and a copy that
// exists is never replaced.
func TestCRW838TheCopyComesBeforeTheRowAndIsAtomic(t *testing.T) {
	state := t.TempDir()
	now := time.Date(2026, 10, 10, 1, 2, 3, 0, time.UTC)
	e := crw838Env(t, &now)
	bundle := auditGoodBundle(t, auditModePR)
	if err := os.WriteFile(filepath.Join(bundle, auditGradeFile), []byte(crw838JSONFirst), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := auditSectionConfig(t, state, nil)
	result := AuditResult{
		Mode: auditModePR, Subject: "s", Head: "h", Issue: "CRW-1", Status: auditStatusOK, Score: 6,
		GradedAt: "2026-10-10T01:02:03Z", Bundle: bundle,
	}
	id := auditRowID("s", "h", "2026-10-10T01:02:03Z")
	copyPath := filepath.Join(state, "audit", "results", id+".json")
	// A copy already under the id (another grade of the same target in the same second) is never
	// replaced, and the new grade is not refused: its row takes the next free second, so its id
	// and its copy are its own.
	if err := os.MkdirAll(filepath.Dir(copyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, []byte(crw838JSONSecond), 0o600); err != nil {
		t.Fatal(err)
	}
	if rows, err := auditRecord(e, cfg, []AuditResult{result}); err != nil || rows != 1 {
		t.Fatalf("auditRecord = %d, %v, want the row recorded", rows, err)
	}
	if data, _ := os.ReadFile(copyPath); string(data) != crw838JSONSecond {
		t.Errorf("the copy was replaced: %q", data)
	}
	nextID := auditRowID("s", "h", "2026-10-10T01:02:04Z")
	if data, err := os.ReadFile(filepath.Join(filepath.Dir(copyPath), nextID+".json")); err != nil || string(data) != crw838JSONFirst {
		t.Errorf("the copy of the second grade is %q (%v)", data, err)
	}
	if rows := crw838Rows(t, state); len(rows) != 1 || crw838String(t, rows[0], "id") != nextID || crw838String(t, rows[0], "graded_at") != "2026-10-10T01:02:04Z" {
		t.Errorf("the ledger holds %v, want one row at the next free second", rows)
	}
	// A fresh id gets a new copy through the temporary file and the link, and leaves no
	// temporary file behind.
	result.GradedAt = "2026-10-10T01:02:10Z"
	if rows, err := auditRecord(e, cfg, []AuditResult{result}); err != nil || rows != 1 {
		t.Fatalf("a second id: %d, %v", rows, err)
	}
	entries, err := os.ReadDir(filepath.Dir(copyPath))
	if err != nil || len(entries) != 3 {
		t.Errorf("results holds %v (%v), want three finished copies", entries, err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "tmp") {
			t.Errorf("a temporary file is left: %s", entry.Name())
		}
	}
	if rows := crw838Rows(t, state); len(rows) != 2 {
		t.Errorf("the ledger has %d rows, want 2", len(rows))
	}
}

// A relative --bundle is recorded as the absolute directory it names, the letters the caller
// gave are kept in bundleGiven, and a link followed by .. is not collapsed.
func TestCRW838RelativeBundleIsRecordedAbsoluteAndTheGivenTextIsKept(t *testing.T) {
	state := t.TempDir()
	e, _, _ := auditTestEnv(t)
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditFake(t, "json", crw838JSONFirst)})
	work := t.TempDir()
	real, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(real, "sub", "b")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, auditBundleFile), []byte(auditBundleText(t, auditBundleSchema, auditModePR)), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(real, "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "sub"), filepath.Join(other, "link")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(real)
	given := "other/link/b"
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: given}}); err != nil {
		t.Fatal(err)
	}
	rows := crw838Rows(t, state)
	if got := crw838String(t, rows[0], "bundle"); got != bundle {
		t.Errorf("bundle = %q, want the absolute directory %q", got, bundle)
	}
	if got := crw838String(t, rows[0], "bundleGiven"); got != given {
		t.Errorf("bundleGiven = %q, want the given text %q", got, given)
	}
	// A spelling with .. after a link names the link target's parent, as the kernel resolves it.
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: "other/link/../sub/b"}}); err != nil {
		t.Fatal(err)
	}
	rows = crw838Rows(t, state)
	if got := crw838String(t, rows[1], "bundle"); got != bundle {
		t.Errorf("bundle = %q, want %q", got, bundle)
	}
	if got := crw838String(t, rows[1], "bundleGiven"); got != "other/link/../sub/b" {
		t.Errorf("bundleGiven = %q", got)
	}
}

// Ledger rows written before the id existed are read as they were: no id, no copy, and the
// drafts surface still reads the bundle's grade.json for them.
func TestCRW838OldRowsAreReadAsBefore(t *testing.T) {
	state := t.TempDir()
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "old", head: "h", round: "r", gradedAt: "2026-01-01T00:00:00Z",
		defects: []AuditDefect{{Severity: "P1", What: "an old defect", Where: "a.go:1"}},
	})
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	e, _, _ := auditTestEnv(t)
	rows, err := auditReportLedger(e, cfg)
	if err != nil || len(rows) != 1 || rows[0].ID != "" || rows[0].Result != "" || rows[0].ResultSHA256 != "" || rows[0].BundleGiven != "" {
		t.Fatalf("rows = %+v (%v)", rows, err)
	}
	line, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"\"id\"", "bundleGiven", "\"result\"", "result_sha256"} {
		if strings.Contains(string(line), key) {
			t.Errorf("an old row now serializes with %s: %s", key, line)
		}
	}
	report := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(report.Created) != 1 || len(report.Skipped) != 0 {
		t.Errorf("created %d, skipped %+v, want the old row drafted from its bundle", len(report.Created), report.Skipped)
	}
	if _, err := os.Stat(filepath.Join(state, "audit", "results")); !os.IsNotExist(err) {
		t.Errorf("reading an old row made a results directory: %v", err)
	}
}

// One audit run at a time: while a run holds <audit state>/audit-run.lock, a second audit
// pr, package or round start is refused by name without waiting, and nothing it would write
// is touched. A dry run and round status only read, so they are not refused.
func TestCRW838ARunRefusesWhileAnotherHoldsTheRunLock(t *testing.T) {
	home := auditDraftHome(t)
	cfg := coreDefaultsForState(t, home)
	e, _, _ := auditTestEnv(t)
	release, err := auditRunLock(e, cfg)
	if err != nil {
		t.Fatalf("the first run lock: %v", err)
	}
	lockPath := filepath.Join(home, "audit", "audit-run.lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("the lock file is not at %s: %v", lockPath, err)
	}
	// audit pr
	prCfg := auditPRSectionFixture(t, home, map[string]any{"pr_since": "2026-10-01T00:00:00Z"})
	gh := auditPRFakeGh(t, "[]", nil)
	ePR, _, errPR := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), ePR, prCfg, 9, false); code != 1 || !strings.Contains(errPR.String(), "another audit run holds "+lockPath) {
		t.Errorf("audit pr: exit %d %q, want 1 naming the lock", code, errPR.String())
	}
	if len(*gh) != 0 {
		t.Errorf("the refused run still called gh: %v", *gh)
	}
	// audit package and audit round start, through the command line
	for name, args := range map[string][]string{
		"package":     {"package", "--round", "r1"},
		"round start": {"round", "start", "--name", "r1", "--package", "internal/x"},
	} {
		eCmd, _, errCmd := auditTestEnv(t)
		if code := auditRun(context.Background(), eCmd, args); code != 1 || !strings.Contains(errCmd.String(), "another audit run holds ") {
			t.Errorf("audit %s: exit %d %q, want 1 naming the lock", name, code, errCmd.String())
		}
	}
	if _, err := os.Stat(filepath.Join(home, "audit", "rounds", "r1.json")); !os.IsNotExist(err) {
		t.Errorf("the refused round start wrote its file: %v", err)
	}
	// Reads are not refused.
	eDry, _, errDry := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), eDry, prCfg, 9, true); code != 0 {
		t.Errorf("audit pr --dry-run was refused: exit %d %q", code, errDry.String())
	}
	release()
	eFree, _, errFree := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), eFree, prCfg, 9, false); code != 0 {
		t.Errorf("audit pr after the release: exit %d %q", code, errFree.String())
	}
	// The lock is released when the run ends, also for a second run in a row.
	eAgain, _, errAgain := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), eAgain, prCfg, 9, false); code != 0 {
		t.Errorf("a second sequential audit pr: exit %d %q", code, errAgain.String())
	}
}

// coreDefaultsForState is the Config the command line builds for a state directory.
func coreDefaultsForState(t *testing.T, state string) *Config {
	t.Helper()
	e, _, _ := auditTestEnv(t)
	cfg := coreDefaults(e)
	if got := auditStateDir(e, cfg); got != state {
		t.Fatalf("the default state directory is %q, want %q", got, state)
	}
	return cfg
}

// A row without an id has no copy, so the guards that protect the bundle's file still apply to
// it: a bundle that carries an unrecorded grade, or was graded again, is not read for the row.
func TestCRW838OldRowsStillHonourTheBundleGuards(t *testing.T) {
	state := t.TempDir()
	bundle := filepath.Join(t.TempDir(), "bundle")
	defect := []AuditDefect{{Severity: "P1", What: "an old defect", Where: "a.go:1"}}
	auditDraftFixture(t, state,
		auditDraftFixtureRow{mode: auditModePR, subject: "s1", head: "h1", round: "r1", gradedAt: "2026-01-01T00:00:00Z", bundle: bundle, defects: defect},
		auditDraftFixtureRow{mode: auditModePR, subject: "s2", head: "h2", round: "r2", gradedAt: "2026-02-01T00:00:00Z", bundle: bundle, defects: defect},
	)
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	report := auditDraftRunOf(t, cfg, auditDraftScope{Round: "r1"})
	if len(report.Created) != 0 || len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0].Reason, "graded again") {
		t.Errorf("created %d, skipped %+v, want the older row skipped as graded again", len(report.Created), report.Skipped)
	}
	e, _, _ := auditTestEnv(t)
	mark, _, err := auditPendingMark(auditPendingPath(e, cfg, bundle))
	if err != nil {
		t.Fatal(err)
	}
	auditPendingUnlock(mark)
	report = auditDraftRunOf(t, cfg, auditDraftScope{Round: "r2"})
	if len(report.Created) != 0 || len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0].Reason, "unrecorded grade") {
		t.Errorf("created %d, skipped %+v, want the newest row skipped for the unrecorded grade", len(report.Created), report.Skipped)
	}
}
