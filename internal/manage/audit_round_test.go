package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// auditRoundFixture starts a round and returns it with its file path.
func auditRoundFixture(t *testing.T, e *Env, cfg *Config, name string, pkgs ...string) (*auditRoundFile, string) {
	t.Helper()
	doc, err := auditRoundStart(e, cfg, name, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	path, err := auditRoundPath(e, cfg, name)
	if err != nil {
		t.Fatal(err)
	}
	return doc, path
}

// C6: a round moves a package from pending to audited, and a non-ok status to failed.
func TestAuditRoundMovesPendingToAuditedAndFailed(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, nil)
	doc, path := auditRoundFixture(t, e, cfg, "r1", "pkg/a", "pkg/b")
	if len(doc.Packages) != 2 || doc.Packages[0].State != auditRoundPending {
		t.Fatalf("round start wrote %+v", doc.Packages)
	}
	if len(auditRoundOutstanding(doc)) != 2 {
		t.Errorf("a fresh round owes %v, want both packages", auditRoundOutstanding(doc))
	}
	auditRoundApply(doc, "pkg/a", "head1", "/bundle/a", AuditResult{Status: auditStatusOK, Score: 9, GradedAt: "t1"})
	auditRoundApply(doc, "pkg/b", "head1", "/bundle/b", AuditResult{Status: auditStatusTimeout, GradedAt: "t1"})
	if doc.Packages[0].State != auditRoundAudited || doc.Packages[0].Status != auditStatusOK || doc.Packages[0].Head != "head1" || doc.Packages[0].Bundle != "/bundle/a" {
		t.Errorf("the ok package reads %+v", doc.Packages[0])
	}
	if doc.Packages[1].State != auditRoundFailed {
		t.Errorf("a non-ok status left the package %q, want failed", doc.Packages[1].State)
	}
	if pending := auditRoundOutstanding(doc); len(pending) != 1 || pending[0] != "pkg/b" {
		t.Errorf("the round owes %v, want only the failed package", pending)
	}
	if err := auditRoundSave(path, doc); err != nil {
		t.Fatal(err)
	}
	reloaded, err := auditRoundLoad(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Packages) != 2 || reloaded.Packages[1].State != auditRoundFailed {
		t.Errorf("the saved round reads %+v", reloaded.Packages)
	}
}

// C6: clean is false while a P1 stands and true after a re-audit that reports none, with
// the earlier result kept in the package's history.
func TestAuditRoundCleanAndHistory(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, nil)
	doc, path := auditRoundFixture(t, e, cfg, "r2", "pkg/a")
	auditRoundApply(doc, "pkg/a", "h1", "/b1", AuditResult{Status: auditStatusOK, Score: 4, GradedAt: "t1",
		Defects: []AuditDefect{{Severity: "P1", What: "wrong", Where: "a.go:1"}}})
	if status := auditRoundStatusOf(doc); status.Clean || status.P1 != 1 || status.Audited != 1 || status.Pending != 0 || status.Total != 1 {
		t.Errorf("the round status with one P1 reads %+v", status)
	}
	auditRoundApply(doc, "pkg/a", "h2", "/b2", AuditResult{Status: auditStatusOK, Score: 9, GradedAt: "t2"})
	if status := auditRoundStatusOf(doc); !status.Clean || status.P1 != 0 || status.Total != 1 {
		t.Errorf("the round status after a clean re-audit reads %+v", status)
	}
	if len(doc.Packages[0].History) != 1 || doc.Packages[0].History[0].Head != "h1" || doc.Packages[0].History[0].P1 != 1 {
		t.Errorf("the earlier result is not in the history: %+v", doc.Packages[0].History)
	}
	if err := auditRoundSave(path, doc); err != nil {
		t.Fatal(err)
	}
	saved, err := auditRoundLoad(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Packages[0].History) != 1 || saved.Packages[0].History[0].Head != "h1" {
		t.Errorf("the saved history reads %+v", saved.Packages[0].History)
	}
}

// C6: an audited round is not clean while a package is still pending or failed.
func TestAuditRoundNotCleanWhilePending(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, nil)
	doc, _ := auditRoundFixture(t, e, cfg, "r3", "pkg/a", "pkg/b")
	auditRoundApply(doc, "pkg/a", "h1", "/b1", AuditResult{Status: auditStatusOK, Score: 9, GradedAt: "t"})
	if status := auditRoundStatusOf(doc); status.Clean || status.Pending != 1 || status.Audited != 1 {
		t.Errorf("a round with a pending package reads %+v", status)
	}
	auditRoundApply(doc, "pkg/b", "h1", "/b2", AuditResult{Status: auditStatusInvalid, GradedAt: "t"})
	if status := auditRoundStatusOf(doc); status.Clean || status.Failed != 1 {
		t.Errorf("a round with a failed package reads %+v", status)
	}
}

// C7: a second attachment to the same round is refused while the first holds the lock, and
// the lock is released so a later run succeeds.
func TestAuditRoundLockRefusesTheSecondRun(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, nil)
	path, err := auditRoundPath(e, cfg, "r4")
	if err != nil {
		t.Fatal(err)
	}
	release, err := auditRoundLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auditRoundLock(path); err == nil {
		t.Fatal("a second lock on the same round was granted")
	} else if !strings.Contains(err.Error(), "round_locked") {
		t.Errorf("the refusal is %q, want it to name round_locked", err)
	}
	release()
	second, err := auditRoundLock(path)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	second()
}

// The round file is replaced atomically: no temporary file is left behind and the content
// is complete.
func TestAuditRoundSaveIsAtomic(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, nil)
	doc, path := auditRoundFixture(t, e, cfg, "r5", "pkg/a")
	doc.Packages[0].State = auditRoundAudited
	if err := auditRoundSave(path, doc); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name != filepath.Base(path) && name != filepath.Base(path)+".lock" {
			t.Errorf("a temporary file was left behind: %s", name)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc2 auditRoundFile
	if err := json.Unmarshal(data, &doc2); err != nil {
		t.Fatalf("the round file is not valid JSON: %v", err)
	}
	if doc2.Round != "r5" || len(doc2.Packages) != 1 || doc2.Packages[0].State != auditRoundAudited {
		t.Errorf("the round file reads %+v", doc2)
	}
}

// A round name that is not a plain file name is refused, so a round can never read or
// write outside the rounds directory.
func TestAuditRoundNameRefusesAnEscape(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/b", "../x"} {
		if err := auditRoundName(name); err == nil {
			t.Errorf("the round name %q was accepted", name)
		}
	}
	if err := auditRoundName("round-1"); err != nil {
		t.Errorf("a plain round name was refused: %v", err)
	}
}

// A round with no package is refused rather than written empty.
func TestAuditRoundStartNeedsAPackage(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, nil)
	if _, err := auditRoundStart(e, cfg, "r6", nil); err == nil {
		t.Error("a round with no package was written")
	}
}

// Starting a round that already exists is refused, so a repeated start command never erases
// the results and the history a running round holds.
func TestAuditRoundStartRefusesAnExistingRound(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, nil)
	doc, path := auditRoundFixture(t, e, cfg, "r7", "pkg/a")
	auditRoundApply(doc, "pkg/a", "h1", "/b1", AuditResult{Status: auditStatusOK, Score: 9, GradedAt: "t"})
	if err := auditRoundSave(path, doc); err != nil {
		t.Fatal(err)
	}
	if _, err := auditRoundStart(e, cfg, "r7", []string{"pkg/a"}); err == nil {
		t.Fatal("an existing round was replaced")
	} else if !strings.Contains(err.Error(), "round_exists") {
		t.Errorf("the refusal is %q, want it to name round_exists", err)
	}
	reloaded, err := auditRoundLoad(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Packages[0].State != auditRoundAudited {
		t.Errorf("the saved round was changed: %+v", reloaded.Packages[0])
	}
}

// A package name that is repeated is kept once, so the round can still become clean.
func TestAuditRoundStartDeduplicatesPackages(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, nil)
	doc, _ := auditRoundFixture(t, e, cfg, "r8", "pkg/a", "pkg/b", "pkg/a")
	if len(doc.Packages) != 2 {
		t.Fatalf("the round holds %+v, want each package once", doc.Packages)
	}
	auditRoundApply(doc, "pkg/a", "h1", "/b1", AuditResult{Status: auditStatusOK, Score: 9, GradedAt: "t"})
	auditRoundApply(doc, "pkg/b", "h1", "/b2", AuditResult{Status: auditStatusOK, Score: 9, GradedAt: "t"})
	if status := auditRoundStatusOf(doc); !status.Clean || status.Total != 2 {
		t.Errorf("the round reads %+v, want it clean with two packages", status)
	}
}

// C6: a package whose last audit reported a P0 or a P1 is selected again, so a fixed defect
// can make the round clean, and the replaced result is kept in the history.
func TestAuditRoundReauditsAPackageWithABlockingDefect(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, nil)
	doc, _ := auditRoundFixture(t, e, cfg, "r9", "pkg/a")
	auditRoundApply(doc, "pkg/a", "h1", "/b1", AuditResult{Status: auditStatusOK, Score: 4, GradedAt: "t1",
		Defects: []AuditDefect{{Severity: "P1", What: "wrong", Where: "a.go:1"}}})
	if outstanding := auditRoundOutstanding(doc); len(outstanding) != 1 || outstanding[0] != "pkg/a" {
		t.Fatalf("the round owes %v, want the package with the blocking defect", outstanding)
	}
	auditRoundApply(doc, "pkg/a", "h2", "/b2", AuditResult{Status: auditStatusOK, Score: 9, GradedAt: "t2"})
	if outstanding := auditRoundOutstanding(doc); len(outstanding) != 0 {
		t.Errorf("the round still owes %v after a clean re-audit", outstanding)
	}
	if status := auditRoundStatusOf(doc); !status.Clean {
		t.Errorf("the round reads %+v, want clean", status)
	}
	if len(doc.Packages[0].History) != 1 || doc.Packages[0].History[0].P1 != 1 {
		t.Errorf("the blocking result is not in the history: %+v", doc.Packages[0].History)
	}
	// A package whose last audit is clean is not selected again.
	auditRoundApply(doc, "pkg/a", "h3", "/b3", AuditResult{Status: auditStatusOK, Score: 9, GradedAt: "t3"})
	if outstanding := auditRoundOutstanding(doc); len(outstanding) != 0 {
		t.Errorf("a clean package is still selected: %v", outstanding)
	}
}

// The round subcommands print their usage: exit 0 for the help flags and exit 2 for a bad
// line, and status of a round that does not exist fails.
func TestAuditRoundCommandsUsage(t *testing.T) {
	e, out, errOut := auditEnv(t)
	out.Reset()
	errOut.Reset()
	if code := auditRunRound(context.Background(), e, []string{"--help"}); code != 0 || !strings.Contains(out.String(), auditRoundUsage) {
		t.Errorf("round --help: exit %d %q", code, out.String())
	}
	for _, args := range [][]string{nil, {"nope"}, {"start"}, {"status"}, {"start", "--name", "r"}, {"start", "--name", "r", "--nope", "x"}} {
		out.Reset()
		errOut.Reset()
		if code := auditRunRound(context.Background(), e, args); code != usageExit {
			t.Errorf("%v: exit %d, want %d", args, code, usageExit)
		}
	}
	if code := auditRunRound(context.Background(), e, []string{"status", "--name", "absent"}); code != 1 {
		t.Errorf("status of a missing round: exit %d, want 1", code)
	}
	// A name that would leave the rounds directory is a usage error, not a read.
	errOut.Reset()
	if code := auditRunRound(context.Background(), e, []string{"status", "--name", "../escape"}); code != usageExit {
		t.Errorf("an escaping round name: exit %d, want %d: %q", code, usageExit, errOut.String())
	}
}

// The package and grade subcommands are reachable from the audit command's dispatch.
func TestAuditPackageIsDispatched(t *testing.T) {
	e, _, errOut := auditEnv(t)
	errOut.Reset()
	if code := auditRun(context.Background(), e, []string{"package"}); code != usageExit {
		t.Errorf("audit package with no round: exit %d, want %d", code, usageExit)
	}
	if !strings.Contains(errOut.String(), auditPkgUsage) {
		t.Errorf("the package usage is missing: %q", errOut.String())
	}
	errOut.Reset()
	if code := auditRun(context.Background(), e, []string{"round"}); code != usageExit {
		t.Errorf("audit round with no subcommand: exit %d, want %d", code, usageExit)
	}
	if !strings.Contains(errOut.String(), auditRoundUsage) {
		t.Errorf("the round usage is missing: %q", errOut.String())
	}
}
