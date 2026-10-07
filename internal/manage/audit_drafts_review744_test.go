package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		},
	})
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	report := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(report.Created) != 2 {
		t.Fatalf("the run created %d drafts, want two: %+v", len(report.Created), report.Created)
	}
	var short, long *auditDraft
	for _, summary := range report.Created {
		draft := auditDraftLoadAt(t, state, summary.Fingerprint)
		if !strings.Contains(draft.Body, korean) {
			t.Errorf("the body lost the original what:\n%s", draft.Body)
		}
		if len([]rune(draft.Title)) > auditDraftTitleLimit {
			t.Errorf("the title is %d characters: %q", len([]rune(draft.Title)), draft.Title)
		}
		if draft.Title == "P1: audit defect in a.go" {
			short = draft
		} else {
			long = draft
		}
	}
	if short == nil {
		t.Errorf("no draft carries the short English fallback title: %+v", report.Created)
	}
	if long == nil || !strings.HasPrefix(long.Title, "P1: audit defect in internal/manage/deep/") {
		t.Errorf("the long title is %+v, want the truncated English fallback", long)
	}
	for _, mode := range []string{auditModePR, auditModePackage} {
		prompt := auditPrompt(&auditBundle{Mode: mode})
		if !strings.Contains(prompt, "Write every what, where and repro in English.") {
			t.Errorf("mode %s: the prompt does not ask for English:\n%s", mode, prompt)
		}
	}
}
