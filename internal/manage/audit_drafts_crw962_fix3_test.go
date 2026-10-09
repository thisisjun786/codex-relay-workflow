package manage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-962 (post-evaluation): --mark-posted records the severity the management session says it
// posted (--to), not the highest severity the ledger holds when the command runs. A higher raise
// that arrived after the post stays reported.
func TestCRW962MarkPostedRecordsThePostedSeverityNotTheLatest(t *testing.T) {
	state, cfg, fingerprint := crw962Posted(t)
	crw962Raise(t, state, "P1", "s2")
	// The P1 raise is posted; before the mark is recorded another audit reports P0.
	crw962Raise(t, state, "P0", "s3")
	code, out, errOut := crw962MarkPosted(t, fingerprint, "--to", "P1")
	if code != 0 {
		t.Fatalf("--mark-posted: exit %d %q", code, errOut)
	}
	if !strings.Contains(out, `"from":"P2"`) || !strings.Contains(out, `"to":"P1"`) {
		t.Errorf("the row written is %q, want P2 to P1", out)
	}
	report := auditDraftRunOf(t, cfg, auditDraftScope{Severity: "P3"})
	if len(report.PostedEscalations) != 1 || report.PostedEscalations[0].From != "P1" || report.PostedEscalations[0].To != "P0" {
		t.Fatalf("the unposted P0 raise is not reported: %+v", report.PostedEscalations)
	}
}

// A severity the audits did not report cannot be recorded as posted, and nothing is written.
func TestCRW962MarkPostedRefusesASeverityNoAuditReported(t *testing.T) {
	state, _, fingerprint := crw962Posted(t)
	crw962Raise(t, state, "P1", "s2")
	before := crw962Ledger(t, state)
	code, _, errOut := crw962MarkPosted(t, fingerprint, "--to", "P0")
	if code == 0 || !strings.Contains(errOut, "no_escalation") {
		t.Fatalf("exit %d %q, want a no_escalation refusal", code, errOut)
	}
	if after := crw962Ledger(t, state); string(after) != string(before) {
		t.Errorf("a refused mark wrote:\n%s", after)
	}
}

// An acknowledgement is not skipped, and not made, from a partial reading: when the result that
// reported the raise cannot be read, the mark is refused rather than answered from what is left.
func TestCRW962MarkPostedIsNotAnsweredFromAPartialReading(t *testing.T) {
	state, cfg, fingerprint := crw962Posted(t)
	crw962Raise(t, state, "P1", "s2")
	if code, _, errOut := crw962MarkPosted(t, fingerprint, "--to", "P1"); code != 0 {
		t.Fatalf("--mark-posted: exit %d %q", code, errOut)
	}
	bundle := filepath.Join(t.TempDir(), "p0-bundle")
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s3", head: "h-s3", round: "r-s3", gradedAt: "2026-03-01T00:00:00Z", bundle: bundle,
		defects: []AuditDefect{{Severity: "P0", What: "a weak guard", Where: "a.go:3"}},
	})
	if err := os.Remove(filepath.Join(bundle, auditGradeFile)); err != nil {
		t.Fatal(err)
	}
	before := crw962Ledger(t, state)
	code, _, errOut := crw962MarkPosted(t, fingerprint, "--to", "P0")
	if code == 0 {
		t.Fatalf("a P0 acknowledgement was made from a reading that could not see the P0 result: %q", errOut)
	}
	if after := crw962Ledger(t, state); string(after) != string(before) {
		t.Errorf("a refused mark wrote:\n%s", after)
	}
	// The result comes back: the same mark now records the P0 raise.
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s4", head: "h-s4", round: "r-s4", gradedAt: "2026-03-02T00:00:00Z",
		defects: []AuditDefect{{Severity: "P0", What: "a weak guard", Where: "a.go:3"}},
	})
	if code, _, errOut := crw962MarkPosted(t, fingerprint, "--to", "P0"); code != 0 {
		t.Fatalf("exit %d %q", code, errOut)
	}
	_ = cfg
}
