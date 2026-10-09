package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// crw962Posted creates a draft from a P2 defect, marks it posted as CRW-999 and returns the
// state directory, the configuration and the draft's fingerprint. Every run uses the P3
// threshold so a P2 defect is drafted.
func crw962Posted(t *testing.T) (string, *Config, string) {
	t.Helper()
	state := auditDraftHome(t)
	defect := AuditDefect{Severity: "P2", What: "a weak guard", Where: "a.go:3"}
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s1", head: "h1", round: "r1", gradedAt: "2026-01-01T00:00:00Z", defects: []AuditDefect{defect},
	})
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	first := auditDraftRunOf(t, cfg, auditDraftScope{Severity: "P3"})
	if len(first.Created) != 1 {
		t.Fatalf("the first run created %d drafts: %+v", len(first.Created), first)
	}
	fingerprint := first.Created[0].Fingerprint
	e, _, errOut := auditEnv(t)
	if code := auditRunDrafts(context.Background(), e, []string{"mark", "--fingerprint", fingerprint, "--posted", "CRW-999"}); code != 0 {
		t.Fatalf("mark: exit %d %q", code, errOut.String())
	}
	return state, cfg, fingerprint
}

// crw962Raise records a later audit that reports the same defect at a higher severity.
func crw962Raise(t *testing.T, state, severity, subject string) {
	t.Helper()
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: subject, head: "h-" + subject, round: "r-" + subject, gradedAt: "2026-02-01T00:00:00Z",
		defects: []AuditDefect{{Severity: severity, What: "a weak guard", Where: "a.go:3"}},
	})
}

func crw962MarkPosted(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	e, out, errOut := auditTestEnv(t)
	code := auditRunDrafts(context.Background(), e, append([]string{"--mark-posted"}, args...))
	return code, out.String(), errOut.String()
}

func crw962Ledger(t *testing.T, state string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state, "audit", auditLedgerFile))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// End condition 1: after the escalation is posted, a run over the same ledger does not report
// it again. Before it is posted, every run reports it, as before.
func TestCRW962PostedEscalationIsNotReportedAgain(t *testing.T) {
	state, cfg, fingerprint := crw962Posted(t)
	crw962Raise(t, state, "P1", "s2")
	for i := 0; i < 2; i++ {
		report := auditDraftRunOf(t, cfg, auditDraftScope{Severity: "P3"})
		if len(report.PostedEscalations) != 1 || report.PostedEscalations[0].To != "P1" || report.PostedEscalations[0].From != "P2" {
			t.Fatalf("run %d before posting: %+v, want the P2 to P1 raise", i, report.PostedEscalations)
		}
	}
	if code, _, errOut := crw962MarkPosted(t, fingerprint, "--to", "P1", "--ref", "https://example.test/comment/1"); code != 0 {
		t.Fatalf("--mark-posted: exit %d %q", code, errOut)
	}
	for i := 0; i < 2; i++ {
		report := auditDraftRunOf(t, cfg, auditDraftScope{Severity: "P3"})
		if len(report.PostedEscalations) != 0 {
			t.Fatalf("run %d after posting still reports %+v", i, report.PostedEscalations)
		}
	}
}

// End condition 2: --mark-posted writes the row append-only, in the shape the decision fixes.
func TestCRW962MarkPostedAppendsOneLedgerRow(t *testing.T) {
	state, _, fingerprint := crw962Posted(t)
	crw962Raise(t, state, "P1", "s2")
	before := crw962Ledger(t, state)
	code, out, errOut := crw962MarkPosted(t, fingerprint, "--to", "P1", "--ref", "https://example.test/comment/1")
	if code != 0 {
		t.Fatalf("--mark-posted: exit %d %q", code, errOut)
	}
	after := crw962Ledger(t, state)
	if !strings.HasPrefix(string(after), string(before)) {
		t.Fatalf("the ledger was rewritten, not appended to:\nbefore: %q\nafter:  %q", before, after)
	}
	added := strings.TrimSpace(strings.TrimPrefix(string(after), string(before)))
	if strings.Contains(added, "\n") || added == "" {
		t.Fatalf("--mark-posted appended %q, want exactly one line", added)
	}
	var row map[string]string
	if err := json.Unmarshal([]byte(added), &row); err != nil {
		t.Fatal(err)
	}
	if row["kind"] != "escalation_posted" || row["target"] != fingerprint || row["from"] != "P2" || row["to"] != "P1" ||
		row["ref"] != "https://example.test/comment/1" || row["at"] == "" {
		t.Errorf("the appended row is %v", row)
	}
	if strings.TrimSpace(out) != added {
		t.Errorf("--mark-posted printed %q, want the row it wrote", out)
	}
	// The same mark again is the same record: nothing more is written.
	if code, _, errOut := crw962MarkPosted(t, fingerprint, "--to", "P1"); code != 0 {
		t.Fatalf("the repeated --mark-posted: exit %d %q", code, errOut)
	}
	if again := crw962Ledger(t, state); string(again) != string(after) {
		t.Errorf("the repeated mark wrote another row:\n%s", again)
	}
}

// End condition 3: a later raise to a higher severity than the posted one is reported again,
// from the severity the issue now stands at.
func TestCRW962AHigherEscalationIsReportedAgain(t *testing.T) {
	state, cfg, fingerprint := crw962Posted(t)
	crw962Raise(t, state, "P1", "s2")
	if code, _, errOut := crw962MarkPosted(t, fingerprint, "--to", "P1"); code != 0 {
		t.Fatalf("--mark-posted: exit %d %q", code, errOut)
	}
	crw962Raise(t, state, "P0", "s3")
	report := auditDraftRunOf(t, cfg, auditDraftScope{Severity: "P3"})
	if len(report.PostedEscalations) != 1 {
		t.Fatalf("the report holds %+v, want the P1 to P0 raise", report.PostedEscalations)
	}
	got := report.PostedEscalations[0]
	if got.Fingerprint != fingerprint || got.From != "P1" || got.To != "P0" || got.Issue != "CRW-999" {
		t.Errorf("the escalation reads %+v", got)
	}
	if code, _, errOut := crw962MarkPosted(t, fingerprint, "--to", "P0"); code != 0 {
		t.Fatalf("the second --mark-posted: exit %d %q", code, errOut)
	}
	if report := auditDraftRunOf(t, cfg, auditDraftScope{Severity: "P3"}); len(report.PostedEscalations) != 0 {
		t.Errorf("the P0 raise is reported after it was posted: %+v", report.PostedEscalations)
	}
}

// End condition 4: the ledger's existing rows are read as they were. A posted row is not a
// result row: the report, the list, the drafts surface and the pull request selection see the
// same results with it and without it, and it is not counted as a torn line.
func TestCRW962ExistingLedgerRowsAreReadAsBefore(t *testing.T) {
	state, cfg, fingerprint := crw962Posted(t)
	crw962Raise(t, state, "P1", "s2")
	e, _, _ := auditTestEnv(t)
	rowsBefore, err := auditReportLedger(e, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := crw962MarkPosted(t, fingerprint, "--to", "P1"); code != 0 {
		t.Fatalf("--mark-posted: exit %d %q", code, errOut)
	}
	rowsAfter, err := auditReportLedger(e, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rowsAfter) != len(rowsBefore) || len(rowsAfter) != 2 {
		t.Fatalf("the report reader sees %d results after the mark and %d before, want 2", len(rowsAfter), len(rowsBefore))
	}
	listing, err := AuditList(context.Background(), e, cfg, AuditListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Results) != 2 {
		t.Errorf("audit list shows %d results, want 2: %+v", len(listing.Results), listing.Results)
	}
	report := auditDraftRunOf(t, cfg, auditDraftScope{Severity: "P3"})
	if report.TornLines != 0 || len(report.Skipped) != 0 {
		t.Errorf("the drafts surface read the posted row as a result: torn=%d skipped=%+v", report.TornLines, report.Skipped)
	}
	if audited := auditPRAudited(rowsAfter); len(audited) != 2 {
		t.Errorf("the pull request selection sees %v", audited)
	}
}

// --mark-posted refuses what it cannot record, and writes nothing then.
func TestCRW962MarkPostedRefusals(t *testing.T) {
	state, _, fingerprint := crw962Posted(t)
	before := crw962Ledger(t, state)
	for name, args := range map[string][]string{
		"an unknown draft":            {"0123456789abcdef", "--to", "P1"},
		"a name that is not a draft":  {"../escape", "--to", "P1"},
		"no escalation to record yet": {fingerprint, "--to", "P1"},
		"no severity named":           {fingerprint},
		"a severity that is not one":  {fingerprint, "--to", "P9"},
	} {
		if code, _, errOut := crw962MarkPosted(t, args...); code == 0 {
			t.Errorf("%s: exit 0, want a refusal (%q)", name, errOut)
		}
	}
	// A draft nobody posted has no issue to raise.
	other := AuditDefect{Severity: "P2", What: "another defect", Where: "b.go:1"}
	auditDraftFixture(t, state, auditDraftFixtureRow{mode: auditModePR, subject: "s9", head: "h9", round: "r9", gradedAt: "2026-03-01T00:00:00Z", defects: []AuditDefect{other}})
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	report := auditDraftRunOf(t, cfg, auditDraftScope{Severity: "P3"})
	if len(report.Created) != 1 {
		t.Fatalf("the second defect created %d drafts", len(report.Created))
	}
	auditDraftFixture(t, state, auditDraftFixtureRow{mode: auditModePR, subject: "s10", head: "h10", round: "r10", gradedAt: "2026-03-02T00:00:00Z", defects: []AuditDefect{{Severity: "P0", What: "another defect", Where: "b.go:1"}}})
	if code, _, errOut := crw962MarkPosted(t, report.Created[0].Fingerprint, "--to", "P0"); code == 0 {
		t.Errorf("a draft that was never posted was marked: %q", errOut)
	}
	after := crw962Ledger(t, state)
	if strings.Contains(string(after), "escalation_posted") {
		t.Errorf("a refused mark wrote a row:\n%s", after)
	}
	_ = before
}
