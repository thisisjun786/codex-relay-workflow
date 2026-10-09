package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This file pins the four defects the post-merge grade of the audit drafts pull request found
// in crw manage audit drafts. Every package-level name it adds carries the auditDraftsReview744
// prefix, so a sibling issue working in the same package never collides with it.
//
// The file builds against the baseline too: the report is read through a local shape and the
// title is exercised through the command, so the red run fails on behaviour and not on a compile
// error.

// auditDraftsReview744Report is the report the drafts command prints, read through a local shape
// so this file also builds where the package report carries no posted_escalations field.
type auditDraftsReview744Report struct {
	Created           []auditDraftSummary `json:"created"`
	Updated           []auditDraftSummary `json:"updated"`
	OwnerUnknown      []string            `json:"owner_unknown"`
	Skipped           []auditDraftSkip    `json:"skipped"`
	PostedEscalations []struct {
		Fingerprint string `json:"fingerprint"`
		Issue       string `json:"issue"`
		From        string `json:"from"`
		To          string `json:"to"`
	} `json:"posted_escalations"`
}

// auditDraftsReview744Run runs the drafts command and reads the report it printed.
func auditDraftsReview744Run(t *testing.T, args ...string) auditDraftsReview744Report {
	t.Helper()
	e, out, errOut := auditEnv(t)
	if code := auditRunDrafts(context.Background(), e, args); code != 0 {
		t.Fatalf("audit drafts %v: exit %d %q", args, code, errOut.String())
	}
	var report auditDraftsReview744Report
	if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, out.String())
	}
	return report
}

// auditDraftsReview744RawKeys reads a draft file as its raw key-value pairs, so a test can
// compare every field the contract freezes byte for byte instead of only the named ones.
func auditDraftsReview744RawKeys(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s is not a JSON object: %v", path, err)
	}
	return doc
}

// auditDraftsReview744Seen reads one draft document's seen list.
func auditDraftsReview744Seen(t *testing.T, doc map[string]json.RawMessage) []auditDraftSeen {
	t.Helper()
	var seen []auditDraftSeen
	if err := json.Unmarshal(doc["seen"], &seen); err != nil {
		t.Fatalf("the seen list is not readable: %v", err)
	}
	return seen
}

// C1: the newest ledger row of a bundle is the last row naming it whatever its status, so a
// bundle whose regrade timed out produces no draft and its older ok row is named and skipped.
func TestAuditDraftsReview744TimedOutRegradeVoidsTheBundle(t *testing.T) {
	state := auditDraftHome(t)
	bundle := filepath.Join(t.TempDir(), "bundle-regraded")
	// R1 succeeded and found nothing.
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
		bundle: bundle,
	})
	// R2 graded the same bundle again and timed out, leaving its own grade.json behind.
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s", head: "h", round: "r2", gradedAt: "2026-02-01T00:00:00Z",
		bundle: bundle, status: auditStatusTimeout,
		defects: []AuditDefect{{Severity: "P1", What: "a defect the timed out regrade left", Where: "a.go:1"}},
	})
	report := auditDraftsReview744Run(t, "--round", "r1")
	if len(report.Created) != 0 || len(report.Updated) != 0 {
		t.Fatalf("the timed out regrade still produced a draft: %+v", report)
	}
	if len(report.Skipped) != 1 {
		t.Fatalf("the report names %d skipped rows, want the older ok row: %+v", len(report.Skipped), report.Skipped)
	}
	if skipped := report.Skipped[0]; skipped.Subject != "s" || skipped.Head != "h" || !strings.Contains(skipped.Reason, "graded again") {
		t.Errorf("the skipped row reads %+v, want the ok row named as graded again", skipped)
	}
}

// C2: a posted draft only grows its seen list; every other field stays byte-identical, and the
// higher severity a later audit reports lands in the report posted_escalations.
func TestAuditDraftsReview744PostedDraftOnlyGrowsSeen(t *testing.T) {
	state := auditDraftHome(t)
	defect := AuditDefect{Severity: "P1", What: "a crash", Where: "a.go:3", Repro: "run it"}
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s1", head: "h1", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
		defects: []AuditDefect{defect},
	})
	first := auditDraftsReview744Run(t)
	if len(first.Created) != 1 {
		t.Fatalf("the first run created %d drafts, want one: %+v", len(first.Created), first.Created)
	}
	fingerprint := first.Created[0].Fingerprint
	e, _, errOut := auditEnv(t)
	if code := auditRunDrafts(context.Background(), e, []string{"mark", "--fingerprint", fingerprint, "--posted", "CRW-1"}); code != 0 {
		t.Fatalf("mark: exit %d %q", code, errOut.String())
	}
	path := filepath.Join(state, "drafts", fingerprint+".json")
	before := auditDraftsReview744RawKeys(t, path)
	// The same defect comes back at P0, with the higher grade's own evidence.
	raised := defect
	raised.Severity = "P0"
	raised.Repro = "the higher repro"
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s2", head: "h2", round: "r2", gradedAt: "2026-02-01T00:00:00Z",
		defects: []AuditDefect{raised},
	})
	second := auditDraftsReview744Run(t)
	after := auditDraftsReview744RawKeys(t, path)
	for key, value := range before {
		if key == "seen" {
			continue
		}
		got, ok := after[key]
		if !ok {
			t.Errorf("the posted draft lost its %s field", key)
			continue
		}
		if string(got) != string(value) {
			t.Errorf("the posted draft %s changed from %s to %s", key, value, got)
		}
	}
	seenBefore, seenAfter := auditDraftsReview744Seen(t, before), auditDraftsReview744Seen(t, after)
	if len(seenBefore) != 1 || len(seenAfter) != 2 {
		t.Fatalf("the seen list went from %d to %d entries, want one more", len(seenBefore), len(seenAfter))
	}
	if seenAfter[0] != seenBefore[0] {
		t.Errorf("the first sighting changed: %+v -> %+v", seenBefore[0], seenAfter[0])
	}
	if len(second.PostedEscalations) != 1 {
		t.Fatalf("the report carries %d posted escalations, want one: %+v", len(second.PostedEscalations), second.PostedEscalations)
	}
	escalation := second.PostedEscalations[0]
	if escalation.Fingerprint != fingerprint || escalation.Issue != "CRW-1" || escalation.From != "P1" || escalation.To != "P0" {
		t.Errorf("the posted escalation reads %+v, want the draft, its issue and P1 to P0", escalation)
	}
}

// C3: a where's line range is not part of the path, so the same defect in the same file is one
// draft and the file's owners entry decides the project.
func TestAuditDraftsReview744LineRangeIsNotInThePath(t *testing.T) {
	state := t.TempDir()
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
		defects: []AuditDefect{
			{Severity: "P1", What: "the same defect", Where: "internal/manage/a.go:12-15"},
			{Severity: "P1", What: "the same defect", Where: "internal/manage/a.go:12"},
		},
	})
	cfg := auditDraftSectionOfState(t, state, map[string]string{"internal/manage/a.go": "CRW"}, 0)
	report := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(report.Created) != 1 {
		t.Fatalf("the two line spellings produced %d drafts, want one: %+v", len(report.Created), report.Created)
	}
	if len(report.OwnerUnknown) != 0 {
		t.Errorf("the report names owner_unknown %v, want the file owners entry to resolve", report.OwnerUnknown)
	}
	draft := auditDraftLoadAt(t, state, report.Created[0].Fingerprint)
	if draft.Project != "CRW" {
		t.Errorf("the draft project is %q, want CRW from the owners entry", draft.Project)
	}
	if len(draft.Seen) != 1 {
		t.Errorf("the draft carries %d sightings, want the one row both defects came from", len(draft.Seen))
	}
}

// C4: a what outside ASCII gets the English fallback title while the original text stays in the
// body, the same length limit holds, and the grader prompt asks for English.
func TestAuditDraftsReview744NonEnglishWhatGetsAnEnglishTitle(t *testing.T) {
	state := t.TempDir()
	const korean = "\uc624\ub958 \uc751\ub2f5\uc744 \uc131\uacf5\uc73c\ub85c \ucc98\ub9ac\ud568"
	longPath := "internal/manage/" + strings.Repeat("deep/", 30) + "a.go"
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
		defects: []AuditDefect{
			{Severity: "P1", What: korean, Where: "a.go:1"},
			{Severity: "P1", What: korean, Where: longPath + ":12"},
			// A where that carries no path at all: the fallback has nothing to name, so the
			// title stays English with the bare token rather than an empty path.
			{Severity: "P1", What: korean, Where: ""},
		},
	})
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	report := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(report.Created) != 3 {
		t.Fatalf("the run created %d drafts, want three: %+v", len(report.Created), report.Created)
	}
	titles := map[string]bool{}
	for _, summary := range report.Created {
		draft := auditDraftLoadAt(t, state, summary.Fingerprint)
		if !strings.Contains(draft.Body, korean) {
			t.Errorf("the body lost the original what:\n%s", draft.Body)
		}
		if len([]rune(draft.Title)) > auditDraftTitleLimit {
			t.Errorf("the title is %d characters: %q", len([]rune(draft.Title)), draft.Title)
		}
		titles[draft.Title] = true
	}
	for _, want := range []string{"P1: audit defect in a.go", "P1: audit defect"} {
		if !titles[want] {
			t.Errorf("no draft carries the title %q: %v", want, titles)
		}
	}
	long := false
	for title := range titles {
		if strings.HasPrefix(title, "P1: audit defect in internal/manage/deep/") {
			long = true
		}
	}
	if !long {
		t.Errorf("no draft carries the truncated long-path fallback: %v", titles)
	}
	for _, mode := range []string{auditModePR, auditModePackage} {
		prompt := auditPrompt(&auditBundle{Mode: mode})
		if !strings.Contains(prompt, "Write every what, where and repro in English.") {
			t.Errorf("mode %s: the prompt does not ask for English:\n%s", mode, prompt)
		}
	}
}

// C1: two spellings of one bundle directory are one bundle, so an ok row is not drafted from
// the grade.json a later timed-out regrade of the same directory left behind.
func TestAuditDraftsReview744BundlePathAliasesAreOneBundle(t *testing.T) {
	state := auditDraftHome(t)
	bundle := filepath.Join(t.TempDir(), "bundle-aliased")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	// R1 succeeded and found nothing, naming the bundle without a trailing separator.
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
		bundle: bundle,
	})
	// R2 graded the same directory again, timed out, and named it with a trailing separator.
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s", head: "h", round: "r2", gradedAt: "2026-02-01T00:00:00Z",
		bundle: bundle + string(filepath.Separator), status: auditStatusTimeout,
		defects: []AuditDefect{{Severity: "P1", What: "a defect the timed out regrade left", Where: "a.go:1"}},
	})
	report := auditDraftsReview744Run(t, "--round", "r1")
	if len(report.Created) != 0 || len(report.Updated) != 0 {
		t.Fatalf("the aliased regrade still produced a draft: %+v", report)
	}
	if len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0].Reason, "graded again") {
		t.Fatalf("the older ok row is not named as graded again: %+v", report.Skipped)
	}
}

// C1: the same directory reached through a link is one bundle, because os.Stat resolves both
// to the same file.
func TestAuditDraftsReview744BundleLinkIsOneBundle(t *testing.T) {
	state := auditDraftHome(t)
	base := t.TempDir()
	bundle := filepath.Join(base, "real-bundle")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "linked-bundle")
	if err := os.Symlink(bundle, link); err != nil {
		t.Skipf("this host cannot make a symlink: %v", err)
	}
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
		bundle: bundle,
	})
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s", head: "h", round: "r2", gradedAt: "2026-02-01T00:00:00Z",
		bundle: link, status: auditStatusTimeout,
		defects: []AuditDefect{{Severity: "P1", What: "a defect the timed out regrade left", Where: "a.go:1"}},
	})
	report := auditDraftsReview744Run(t, "--round", "r1")
	if len(report.Created) != 0 || len(report.Updated) != 0 {
		t.Fatalf("the linked regrade still produced a draft: %+v", report)
	}
	if len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0].Reason, "graded again") {
		t.Fatalf("the older ok row is not named as graded again: %+v", report.Skipped)
	}
}

// C2: a posted draft is frozen for every writer, not only for audit drafts. The improve
// surface rewrites an existing draft of its own source, and advances an audit draft's seen
// section; for a posted draft it must append the new sightings and change nothing else, so
// the issue the management session already opened still describes the file it opened.
func TestAuditDraftsReview744ImproveProposePostedDraftOnlyGrowsSeen(t *testing.T) {
	for _, source := range []string{improveProposeSource, auditDraftSource} {
		t.Run(source, func(t *testing.T) {
			w := improveProposeTestSetup(t)
			improveProposeTestConfigure(t, w, map[string]any{})
			dir := filepath.Join(w.stateDir, "drafts")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			key := auditDraftFingerprint(improveKindSplit, improveProposeTestFriction)
			posted := &auditDraft{
				Schema: auditDraftSchema, Fingerprint: key, Source: source,
				Project: "project-a", Title: "a posted defect", Severity: "P1",
				Labels: []string{source, "P1"}, State: auditDraftStatePosted, Posted: "CRW-1",
				Body: improveProposePostedBody(source),
				Seen: []auditDraftSeen{{Mode: auditModePR, Subject: "s", Head: "h", At: "2026-10-01T00:00:00Z"}},
			}
			path := filepath.Join(dir, key+".json")
			if err := auditDraftSave(path, posted); err != nil {
				t.Fatal(err)
			}
			before := auditDraftsReview744RawKeys(t, path)
			bundle := improveProposeTestBundle(t, w, improveProposeTestEightCases())
			code, stdout, stderr := improveProposeTestRun(t, w, "--bundle", bundle)
			if code != 0 {
				t.Fatalf("propose: exit %d, stderr %s", code, stderr)
			}
			report := improveProposeTestReport(t, stdout)
			if len(report.Updated) != 1 {
				t.Fatalf("updated = %+v, want the posted draft to grow", report.Updated)
			}
			after := auditDraftsReview744RawKeys(t, path)
			for field, value := range before {
				if field == "seen" {
					continue
				}
				got, ok := after[field]
				if !ok {
					t.Errorf("the posted draft lost its %s field", field)
					continue
				}
				if string(got) != string(value) {
					t.Errorf("the posted draft %s changed from %s to %s", field, value, got)
				}
			}
			seenBefore, seenAfter := auditDraftsReview744Seen(t, before), auditDraftsReview744Seen(t, after)
			if len(seenAfter) <= len(seenBefore) {
				t.Errorf("the seen list went from %d to %d entries, want it to grow", len(seenBefore), len(seenAfter))
			}
		})
	}
}

// improveProposePostedBody is a stored body in the shape its source writes: an improve draft
// carries the projects and the evidence its own renderer folds in, and an audit draft carries
// the source-audit and criteria sections the audit renderer advances. Both are the sections a
// rewrite would touch, so a test that reads them back unchanged is a real check.
func improveProposePostedBody(source string) string {
	if source == improveProposeSource {
		return "## What\n\na posted defect\n\n## Projects\n\n- project-a: 1\n\n## Evidence\n\n- events:rel-a\n"
	}
	return "## What\n\na posted defect\n\n## Where\n\na.go:1\n\n## Source audit\n\n- mode=pr subject=s head=h at=2026-10-01T00:00:00Z\n\n## Criteria\n\n- C1: PASS\n"
}

// auditDraftsReview744SlowGrader writes its result and then runs past the time limit, so a
// regrade records timeout while leaving a usable grade.json behind. It touches the marker once
// it has started, so a test can tell the grade is under way.
const auditDraftsReview744SlowGrader = "#!/bin/sh\n" +
	"prompt=$1\n" +
	"dir=$(dirname $prompt)\n" +
	"if [ -n \"$AUDIT_MARKER\" ]; then : > \"$AUDIT_MARKER\"; fi\n" +
	"printf '%s' \"$AUDIT_JSON\" > $dir/grade.json\n" +
	"if [ -n \"$AUDIT_SLOW\" ]; then sleep 30; fi\n" +
	"exit 0\n"

// auditDraftsReview744BundleAt writes a bundle document into a directory.
func auditDraftsReview744BundleAt(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	decl := map[string]any{"schema": auditBundleSchema, "mode": auditModePR, "subject": "s", "head": "h", "issue": "CRW-1"}
	body, err := json.Marshal(decl)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, auditBundleFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// auditDraftsReview744Grader writes the slow fake grader and returns its command.
func auditDraftsReview744Grader(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "slow-grader.sh")
	if err := os.WriteFile(path, []byte(auditDraftsReview744SlowGrader), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{path, "{prompt_file}", "{bundle}"}
}

// C1: a regrade that names the same bundle by a relative path is the same bundle wherever the
// reader runs, because the ledger records the bundle absolutely. The earlier ok row is then
// skipped rather than drafted from the grade.json the timed-out regrade left.
func TestAuditDraftsReview744RegradeFromAnotherCwdVoidsTheBundle(t *testing.T) {
	state := t.TempDir()
	base := t.TempDir()
	bundle := filepath.Join(base, "a", "B")
	auditDraftsReview744BundleAt(t, bundle)
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditDraftsReview744Grader(t), "grader_timeout_seconds": 1})
	e, _, _ := auditTestEnv(t)
	// R1 is graded from the bundle parent, naming the bundle absolutely, and finds nothing.
	t.Chdir(filepath.Join(base, "a"))
	t.Setenv("AUDIT_JSON", auditJSONClean)
	t.Setenv("AUDIT_SLOW", "")
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: bundle, Round: "r1"}}); err != nil {
		t.Fatal(err)
	}
	// R2 grades the same bundle again, naming it relatively, writes a usable P1 result and runs
	// past its time limit.
	t.Setenv("AUDIT_JSON", auditJSONWithP1)
	t.Setenv("AUDIT_SLOW", "1")
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: "B", Round: "r2"}}); err != nil {
		t.Fatal(err)
	}
	// Drafts runs from a directory where the relative spelling names nothing at all.
	other := filepath.Join(base, "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(other)
	report, err := auditDraftsRun(e, auditDraftSectionOfState(t, state, nil, 0), auditDraftScope{Round: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Created) != 0 || len(report.Updated) != 0 {
		t.Fatalf("the regrade from another cwd still produced a draft: %+v", report)
	}
	// CRW-838: the older ok row carries the copy of its own grade.json, so it is read from that
	// copy (clean, so no draft) and is not named as graded again: the bundle's later file cannot
	// reach it. A row without a copy is still skipped as graded again (the fixture-row tests).
	if len(report.Skipped) != 0 {
		t.Fatalf("the older ok row was skipped although it carries its own result copy: %+v", report.Skipped)
	}
}

// C1: a grade holds the drafts lock for its whole run, so the drafts surface cannot read the
// ledger and then a grade.json another run replaced: the grade file and the row that names it
// are one record, and the two writers do not interleave.
func TestAuditDraftsReview744GradeHoldsTheDraftsLock(t *testing.T) {
	state := t.TempDir()
	bundle := filepath.Join(t.TempDir(), "bundle")
	auditDraftsReview744BundleAt(t, bundle)
	marker := filepath.Join(t.TempDir(), "started")
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditDraftsReview744Grader(t), "grader_timeout_seconds": 2})
	e, _, _ := auditTestEnv(t)
	t.Setenv("AUDIT_JSON", auditJSONClean)
	t.Setenv("AUDIT_MARKER", marker)
	t.Setenv("AUDIT_SLOW", "1")
	done := make(chan error, 1)
	go func() {
		_, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: bundle}})
		done <- err
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the grader never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := auditDraftLock(e, cfg); err == nil {
		t.Error("a grade in flight did not hold the drafts lock")
	} else if !strings.Contains(err.Error(), "drafts_locked") {
		t.Errorf("the refusal reads %q, want drafts_locked", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// C1: two jobs naming one bundle directory in different spellings are refused before anything
// runs, because they would race over the same grade.json and the same prompt. The spelling does
// not decide identity: a link and a relative form name the same directory as the real path.
func TestAuditDraftsReview744GradeRefusesAliasedJobs(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	base := t.TempDir()
	bundle := filepath.Join(base, "real-bundle")
	auditDraftsReview744BundleAt(t, bundle)
	link := filepath.Join(base, "linked-bundle")
	if err := os.Symlink(bundle, link); err != nil {
		t.Skipf("this host cannot make a symlink: %v", err)
	}
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditDraftsReview744Grader(t)})
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: bundle}, {Bundle: link}}); err == nil {
		t.Fatal("a bundle and a link to it were accepted as two jobs")
	}
	t.Chdir(base)
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: "real-bundle"}, {Bundle: bundle}}); err == nil {
		t.Fatal("a relative and an absolute spelling of one bundle were accepted as two jobs")
	}
	if _, err := os.Stat(filepath.Join(state, "audit", auditLedgerFile)); !os.IsNotExist(err) {
		t.Errorf("a ledger was written for a refused batch: %v", err)
	}
}

// C1: a grade whose ledger row was never recorded leaves no result for an older ok row to be
// drafted from. The run marks its bundle before the grader can leave a file and clears the mark
// once its row is on disk, so the file it left is still there for the operator to read while the
// drafts surface refuses to read it as the result of a row.
func TestAuditDraftsReview744FailedRecordVoidsTheBundle(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the file permission this test needs does not stop root")
	}
	state := t.TempDir()
	bundle := filepath.Join(t.TempDir(), "bundle-unrecorded")
	auditDraftsReview744BundleAt(t, bundle)
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditDraftsReview744Grader(t), "grader_timeout_seconds": 1})
	e, _, _ := auditTestEnv(t)
	// R1 is graded and recorded: the bundle's ok row, with no defects.
	t.Setenv("AUDIT_JSON", auditJSONClean)
	t.Setenv("AUDIT_SLOW", "")
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: bundle, Round: "r1"}}); err != nil {
		t.Fatal(err)
	}
	// R2 grades the same bundle again, leaves a usable P1 result and times out, and the ledger
	// cannot be appended to, so the run fails and its row is never recorded.
	ledger := filepath.Join(state, "audit", auditLedgerFile)
	if err := os.Chmod(ledger, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ledger, 0o600) })
	t.Setenv("AUDIT_JSON", auditJSONWithP1)
	t.Setenv("AUDIT_SLOW", "1")
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: bundle, Round: "r2"}}); err == nil {
		t.Fatal("a grade whose ledger row could not be recorded reported success")
	}
	if _, err := os.Stat(filepath.Join(bundle, auditGradeFile)); err != nil {
		t.Errorf("the unrecorded result's file must stay for the operator to read: %v", err)
	}
	report, err := auditDraftsRun(e, auditDraftSectionOfState(t, state, nil, 0), auditDraftScope{Round: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Created) != 0 || len(report.Updated) != 0 {
		t.Fatalf("the unrecorded regrade still produced a draft: %+v", report)
	}
	// CRW-838: the ok row is read from its own result copy, so the unrecorded file the bundle
	// carries is never read for it; a row without a copy is still skipped for the unrecorded
	// grade (the fixture-row tests).
	if len(report.Skipped) != 0 {
		t.Fatalf("the ok row was skipped although it carries its own result copy: %+v", report.Skipped)
	}
}

// C1: the record is per result. A failure that comes after some rows are on disk (an alert write,
// for one) must not throw away the results that were recorded: each recorded bundle keeps its
// file for the row that names it, and only the results whose rows are missing stay unreadable.
func TestAuditDraftsReview744RecordedResultsSurviveALaterRecordFailure(t *testing.T) {
	state := t.TempDir()
	dir := filepath.Join(state, "audit")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The alert queue is where a P1 result goes, and this host's full device refuses every write to
	// it, so the run records its ledger rows and then fails on the first alert.
	if err := os.Symlink("/dev/full", filepath.Join(dir, auditAlertFile)); err != nil {
		t.Skipf("this host cannot make the failing alert target: %v", err)
	}
	first := filepath.Join(t.TempDir(), "bundle-first")
	second := filepath.Join(t.TempDir(), "bundle-second")
	auditDraftsReview744BundleAt(t, first)
	auditDraftsReview744BundleAt(t, second)
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditDraftsReview744Grader(t), "grader_timeout_seconds": 5})
	e, _, errOut := auditTestEnv(t)
	t.Setenv("AUDIT_JSON", auditJSONWithP1)
	t.Setenv("AUDIT_SLOW", "")
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: first}, {Bundle: second}}); err == nil {
		t.Fatal("a grade whose alert could not be written reported success")
	}
	if _, err := os.Stat(filepath.Join(first, auditGradeFile)); err != nil {
		t.Errorf("the first result was recorded, so its file must stay: %v", err)
	}
	report, err := auditDraftsRun(e, auditDraftSectionOfState(t, state, nil, 0), auditDraftScope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Created) != 1 {
		t.Fatalf("the recorded result must still draft and the unrecorded one must not: %+v (stderr %s)", report, errOut.String())
	}
}

// C1: the ledger records the directory a grade really graded, so a link used for one regrade is
// the same bundle after it is gone: the older ok row stays skipped rather than drafting the file
// the timed-out regrade left.
func TestAuditDraftsReview744ResolvedBundleSurvivesItsLink(t *testing.T) {
	state := t.TempDir()
	base := t.TempDir()
	bundle := filepath.Join(base, "real-bundle")
	auditDraftsReview744BundleAt(t, bundle)
	link := filepath.Join(base, "linked-bundle")
	if err := os.Symlink(bundle, link); err != nil {
		t.Skipf("this host cannot make a symlink: %v", err)
	}
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditDraftsReview744Grader(t), "grader_timeout_seconds": 1})
	e, _, _ := auditTestEnv(t)
	t.Setenv("AUDIT_JSON", auditJSONClean)
	t.Setenv("AUDIT_SLOW", "")
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: bundle, Round: "r1"}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUDIT_JSON", auditJSONWithP1)
	t.Setenv("AUDIT_SLOW", "1")
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: link, Round: "r2"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	report, err := auditDraftsRun(e, auditDraftSectionOfState(t, state, nil, 0), auditDraftScope{Round: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Created) != 0 || len(report.Updated) != 0 {
		t.Fatalf("the resolved regrade still produced a draft after its link went: %+v", report)
	}
	// CRW-838: the older ok row carries the copy of its own grade.json, so it is read from that
	// copy (clean, so no draft) and is not named as graded again: the bundle's later file cannot
	// reach it. A row without a copy is still skipped as graded again (the fixture-row tests).
	if len(report.Skipped) != 0 {
		t.Fatalf("the older ok row was skipped although it carries its own result copy: %+v", report.Skipped)
	}
}

// C1: a batch that is refused while it is taking a marker stops before it reaches the bundles it
// has not started, so an unchanged, recorded grade keeps drafting.
func TestAuditDraftsReview744RefusedBatchLeavesNoMarker(t *testing.T) {
	state := t.TempDir()
	good := filepath.Join(t.TempDir(), "bundle-good")
	auditDraftsReview744BundleAt(t, good)
	bad := filepath.Join(t.TempDir(), "bundle-bad")
	auditDraftsReview744BundleAt(t, bad)
	// One worker, and the refused bundle first: the good bundle is never reached.
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditDraftsReview744Grader(t), "workers": 1})
	e, _, _ := auditTestEnv(t)
	t.Setenv("AUDIT_JSON", auditJSONWithP1)
	t.Setenv("AUDIT_SLOW", "")
	// The first bundle is graded and recorded, so it has a usable result and no marker.
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: good}}); err != nil {
		t.Fatal(err)
	}
	// The refused job's marker cannot be taken, because a directory already sits at its path. The
	// batch stops there, so the good bundle must be left exactly as it was: no marker, and its
	// recorded result still drafting.
	if err := os.MkdirAll(auditPendingPath(e, cfg, bad), 0o700); err != nil {
		t.Fatal(err)
	}
	// Any later grader run writes a clean result, so a bundle that is regraded loses its P1 file
	// and stops drafting. The count below is therefore the regrade itself, not a coincidence.
	t.Setenv("AUDIT_JSON", auditJSONClean)
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: bad}, {Bundle: good}}); err == nil {
		t.Fatal("a batch whose marker could not be taken was accepted")
	}
	if auditPending(e, cfg, good) {
		t.Fatal("the refused batch left a marker on a bundle it never graded")
	}
	report, err := auditDraftsRun(e, auditDraftSectionOfState(t, state, nil, 0), auditDraftScope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Created) != 1 {
		t.Fatalf("the recorded grade must still draft: %+v", report)
	}
	if report.Created[0].Severity != "P1" {
		t.Errorf("the surviving draft is %s, want the recorded P1 the refused batch must not regrade", report.Created[0].Severity)
	}
}

// C1: the marker is never opened through a path the bundle controls, so a link planted at its
// name cannot make a grade create a file outside the state directory.
func TestAuditDraftsReview744MarkerIsNotOpenedThroughTheBundle(t *testing.T) {
	state := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	bundle := filepath.Join(t.TempDir(), "bundle")
	auditDraftsReview744BundleAt(t, bundle)
	// A link at the marker's name inside the bundle points outside it. The marker no longer lives
	// there, so grading neither follows nor creates that target.
	if err := os.Symlink(outside, filepath.Join(bundle, ".crw-audit-pending")); err != nil {
		t.Skipf("this host cannot make a symlink: %v", err)
	}
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditDraftsReview744Grader(t)})
	e, _, _ := auditTestEnv(t)
	t.Setenv("AUDIT_JSON", auditJSONClean)
	t.Setenv("AUDIT_SLOW", "")
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: bundle}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(outside); !os.IsNotExist(err) {
		t.Errorf("the marker path created something outside the state directory: %v", err)
	}
}

// C1: the kernel resolves the path before anything is read, so a spelling that carries a link and
// then ".." names the directory the caller's path names, not the one a lexical clean picks.
func TestAuditDraftsReview744BundlePathResolvesLikeTheKernel(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(real, "B"), 0o700); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(filepath.Join(work, "B"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "child"), filepath.Join(work, "link")); err != nil {
		t.Skipf("this host cannot make a symlink: %v", err)
	}
	// The kernel reads "link/../B" as real/B, while a lexical clean would name work/B. The spelling
	// is joined by hand, because filepath.Join would clean it before the resolver ever saw it.
	got, err := auditBundleResolvedPath(work + "/link/../B")
	if err != nil {
		t.Fatal(err)
	}
	want, err := auditBundleResolvedPath(filepath.Join(real, "B"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("the resolved path is %q, want the directory the kernel names, %q", got, want)
	}
}

// C1: a spelling the kernel cannot resolve is refused rather than lexically cleaned, so grading
// never substitutes a different existing directory for the one the caller named.
func TestAuditDraftsReview744UnresolvableBundlePathIsRefused(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "child"), filepath.Join(work, "link")); err != nil {
		t.Skipf("this host cannot make a symlink: %v", err)
	}
	// /work/B exists and holds a valid bundle, while the path the kernel resolves
	// (/real/B) does not. Cleaning first would grade /work/B; refusing is the only safe answer.
	auditDraftsReview744BundleAt(t, filepath.Join(work, "B"))
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditDraftsReview744Grader(t)})
	e, _, _ := auditTestEnv(t)
	t.Setenv("AUDIT_JSON", auditJSONClean)
	t.Setenv("AUDIT_SLOW", "")
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: work + "/link/../B"}}); err == nil {
		t.Fatal("a spelling whose kernel-resolved target does not exist was graded anyway")
	}
	if _, err := os.Stat(filepath.Join(work, "B", auditGradeFile)); !os.IsNotExist(err) {
		t.Errorf("the other directory was graded through the substitution: %v", err)
	}
}

// C1: a result whose ledger row was written is not thrown away when the recording call fails
// later. The writer reports how many rows it appended, so an alert write that fails after a row
// does not take back that row's marker even when the ledger cannot be read afterwards.
func TestAuditDraftsReview744RecordedRowSurvivesAnUnreadableLedger(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the file permission this test needs does not stop root")
	}
	state := t.TempDir()
	dir := filepath.Join(state, "audit")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/full", filepath.Join(dir, auditAlertFile)); err != nil {
		t.Skipf("this host cannot make the failing alert target: %v", err)
	}
	// The ledger exists and can be appended to, but cannot be read: a run that re-read it to count
	// its own rows would fail and answer zero, while the row it already wrote is on disk.
	ledger := filepath.Join(dir, auditLedgerFile)
	if err := os.WriteFile(ledger, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ledger, 0o200); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ledger, 0o600) })
	bundle := filepath.Join(t.TempDir(), "bundle")
	auditDraftsReview744BundleAt(t, bundle)
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditDraftsReview744Grader(t), "grader_timeout_seconds": 5})
	e, _, _ := auditTestEnv(t)
	t.Setenv("AUDIT_JSON", auditJSONWithP1)
	t.Setenv("AUDIT_SLOW", "")
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: bundle}}); err == nil {
		t.Fatal("a grade whose alert could not be written reported success")
	}
	// The ledger is readable again, as it would be once the operator fixes the mode; the marker
	// decision above was made while it was not.
	if err := os.Chmod(ledger, 0o600); err != nil {
		t.Fatal(err)
	}
	// The row is on disk, so the marker must be gone and the result must still draft.
	if auditPending(e, cfg, bundle) {
		t.Fatal("the marker of a recorded result was kept")
	}
	report, err := auditDraftsRun(e, auditDraftSectionOfState(t, state, nil, 0), auditDraftScope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Created) != 1 {
		t.Fatalf("the recorded result must still draft: %+v", report)
	}
}

// C1: a marker this call created is taken back when the lock cannot be taken, so a bundle nothing
// was graded under is not left looking unrecorded.
func TestAuditDraftsReview744UnlockableMarkerIsTakenBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "marker")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := auditPendingLockFailed(f, path, true, syscall.EWOULDBLOCK); err == nil {
		t.Fatal("a marker whose lock failed was accepted")
	} else if !strings.Contains(err.Error(), "bundle_locked") {
		t.Errorf("the refusal reads %q, want bundle_locked", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the marker this call created was left behind: %v", err)
	}
	// A marker another run left is kept: its lock is released, but the file is that run's record.
	other := filepath.Join(t.TempDir(), "marker-other")
	g, err := os.OpenFile(other, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := auditPendingLockFailed(g, other, false, syscall.EWOULDBLOCK); err == nil {
		t.Fatal("a marker whose lock failed was accepted")
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("a marker this call did not create was removed: %v", err)
	}
}

// C1: a bundle whose marker cannot be taken is not graded, and no grader runs.
func TestAuditDraftsReview744BundleWhoseMarkerIsRefusedIsNotGraded(t *testing.T) {
	state := t.TempDir()
	bundle := filepath.Join(t.TempDir(), "bundle")
	auditDraftsReview744BundleAt(t, bundle)
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditDraftsReview744Grader(t)})
	e, _, _ := auditTestEnv(t)
	// A directory at the marker path makes the exclusive create fail before anything is graded.
	if err := os.MkdirAll(auditPendingPath(e, cfg, bundle), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: bundle}}); err == nil {
		t.Fatal("a bundle whose marker could not be taken was graded anyway")
	}
	if _, err := os.Stat(filepath.Join(bundle, auditGradeFile)); !os.IsNotExist(err) {
		t.Errorf("a grader ran for a bundle whose marker was refused: %v", err)
	}
}

// C1: a job that is still waiting for a worker is not marked. A batch that is refused for one
// bundle must leave the bundles it never reached exactly as they were, so their recorded results
// keep drafting.
func TestAuditDraftsReview744QueuedBundleIsNotMarked(t *testing.T) {
	state := t.TempDir()
	good := filepath.Join(t.TempDir(), "bundle-good")
	auditDraftsReview744BundleAt(t, good)
	refused := filepath.Join(t.TempDir(), "bundle-refused")
	auditDraftsReview744BundleAt(t, refused)
	// One worker, and the refused bundle first: the good bundle never reaches a worker.
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditDraftsReview744Grader(t), "workers": 1})
	e, _, _ := auditTestEnv(t)
	t.Setenv("AUDIT_JSON", auditJSONWithP1)
	t.Setenv("AUDIT_SLOW", "")
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: good}}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(auditPendingPath(e, cfg, refused), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: refused}, {Bundle: good}}); err == nil {
		t.Fatal("a batch whose marker could not be taken was accepted")
	}
	if auditPending(e, cfg, good) {
		t.Fatal("a bundle that never reached a worker was marked")
	}
	report, err := auditDraftsRun(e, auditDraftSectionOfState(t, state, nil, 0), auditDraftScope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Created) != 1 {
		t.Fatalf("the recorded result must still draft: %+v", report)
	}
}

// C1: a grading failure is the run's, not one target's. Two pull requests are selected and the
// grader is unconfigured, so the first grade fails on shared state; the run must stop there rather
// than rebuild and grade the second, and it must say why.
func TestAuditDraftsReview744GradeFailureStopsTheRun(t *testing.T) {
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{
		"pr_since": "2026-10-01T00:00:00Z",
		"pairs":    map[string]string{"deepseek": "if-deepseek"},
		"phases":   map[string]string{"2026-10-01T00:00:00Z": "live"},
	})
	patch := "diff --git a/internal/a.go b/internal/a.go\n--- a/internal/a.go\n+++ b/internal/a.go\n@@ -1 +1 @@\n-old\n+new\n"
	entries := auditPRListJSON(t,
		auditPRMergeEntry(12, "CRW-12: one", "2026-10-05T00:00:00Z", "m12"),
		auditPRMergeEntry(13, "CRW-13: two", "2026-10-05T00:00:00Z", "m12"))
	auditPRFakeGh(t, entries, map[int]string{12: patch, 13: patch})
	auditPRFakeRelay(t,
		map[string]string{"CRW-12": auditPRAssignmentJSON(t, "rel-1", "child-1"), "CRW-13": auditPRAssignmentJSON(t, "rel-2", "child-2")},
		map[string]string{"child-1": auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash"), "child-2": auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash")},
		map[string]string{"rel-1": auditPRCriteriaJSON(t, "c1", "it works"), "rel-2": auditPRCriteriaJSON(t, "c1", "it works")})
	auditPRFakeCheckout(t, "m12", map[string]string{"internal/a.go": "package a\n"})
	e, _, errOut := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), e, cfg, 9, false); code != 1 {
		t.Fatalf("audit pr: exit %d, want the run to stop; stderr %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "grader_unconfigured") {
		t.Errorf("the run did not report the shared failure: %s", errOut.String())
	}
	// Only one bundle was built: the run stopped on the first failure instead of rebuilding the
	// second target. The bundle directories are that record, so they are asserted directly rather
	// than through the ledger, which is empty for both an immediate stop and a loop that continues.
	section, err := auditPRSectionOf(cfg)
	if err != nil {
		t.Fatal(err)
	}
	bundles, err := os.ReadDir(auditPRBundleRoot(e, cfg, section))
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 1 {
		names := make([]string, 0, len(bundles))
		for _, entry := range bundles {
			names = append(names, entry.Name())
		}
		t.Errorf("the run built %d bundles (%v), want only the one it stopped on", len(bundles), names)
	}
	if _, err := os.Stat(filepath.Join(state, "audit", auditLedgerFile)); !os.IsNotExist(err) {
		t.Errorf("a ledger was written for a run that stopped before recording: %v", err)
	}
}

// C1: a spelling whose parent is a regular file names no bundle. The kernel refuses "file/..",
// so grading must refuse it rather than resolve the file's parent and grade the directory holding it.
func TestAuditDraftsReview744FileParentIsNotASubstitute(t *testing.T) {
	base := t.TempDir()
	auditDraftsReview744BundleAt(t, base)
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditDraftsReview744Grader(t)})
	e, _, _ := auditTestEnv(t)
	t.Setenv("AUDIT_JSON", auditJSONClean)
	t.Setenv("AUDIT_SLOW", "")
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: file + "/.."}}); err == nil {
		t.Fatal("a spelling through a regular file was graded as the directory holding the file")
	}
	if _, err := os.Stat(filepath.Join(base, auditGradeFile)); !os.IsNotExist(err) {
		t.Errorf("the directory holding the file was graded: %v", err)
	}
}

// C1: a relative bundle named under a symlinked working directory is the directory the kernel
// resolves, not the logical path the shell reports, so its ledger row still names the bundle
// after the link is removed and the older ok row is skipped.
func TestAuditDraftsReview744RelativeBundleUnderSymlinkedCwdIsOneBundle(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	bundle := filepath.Join(real, "B")
	auditDraftsReview744BundleAt(t, bundle)
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("this host cannot make a symlink: %v", err)
	}
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, map[string]any{"grader": auditDraftsReview744Grader(t), "grader_timeout_seconds": 1})
	e, _, _ := auditTestEnv(t)
	// R1 names the bundle relatively from inside the symlinked directory and finds nothing.
	t.Chdir(link)
	t.Setenv("AUDIT_JSON", auditJSONClean)
	t.Setenv("AUDIT_SLOW", "")
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: "B", Round: "r1"}}); err != nil {
		t.Fatal(err)
	}
	// R2 grades the same directory by its real path, writes a usable P1 result and runs past its limit.
	t.Chdir(base)
	t.Setenv("AUDIT_JSON", auditJSONWithP1)
	t.Setenv("AUDIT_SLOW", "1")
	if _, err := AuditGrade(context.Background(), e, cfg, []AuditJob{{Bundle: bundle, Round: "r2"}}); err != nil {
		t.Fatal(err)
	}
	// The link goes away; the drafts surface must still see that R1's row names a regraded bundle.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	report, err := auditDraftsRun(e, auditDraftSectionOfState(t, state, nil, 0), auditDraftScope{Round: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Created) != 0 || len(report.Updated) != 0 {
		t.Fatalf("the regrade under the symlinked cwd still produced a draft: %+v", report)
	}
	// CRW-838: the older ok row carries the copy of its own grade.json, so it is read from that
	// copy (clean, so no draft) and is not named as graded again: the bundle's later file cannot
	// reach it. A row without a copy is still skipped as graded again (the fixture-row tests).
	if len(report.Skipped) != 0 {
		t.Fatalf("the older ok row was skipped although it carries its own result copy: %+v", report.Skipped)
	}
}
