package store

import (
	"context"
	"database/sql"
	"testing"
)

func TestEvidenceWrites_roundtrip_when_rows_are_recorded(t *testing.T) {
	// Given: an isolated store and typed rows for each evidence table.
	s := recordStore(t)
	ctx := context.Background()
	checks := []struct {
		name  string
		write func() error
		read  func() (string, error)
		want  string
	}{
		{"generation_turn", func() error {
			return s.RecordGenerationTurn(ctx, GenerationTurn{RelationshipID: "r", Generation: 1, TurnID: "turn", Evidence: "anchor", AdmittedAt: "t"})
		}, func() (string, error) { r, e := s.GenerationTurn(ctx, "r", 1, "turn"); return r.Evidence, e }, "anchor"},
		{"revision_lineage", func() error {
			return s.RecordLineage(ctx, RevisionLineage{RelationshipID: "r", Generation: 1, EventID: "e", RevisionHash: "hash", DeclaredBy: "child", RecordedAt: "t"})
		}, func() (string, error) { r, e := s.RevisionLineage(ctx, "r", 1, "e"); return r.RevisionHash, e }, "hash"},
		{"verification_claims", func() error { return s.RecordClaim(ctx, VerificationClaim{EventID: "e", ClaimedAt: "t"}) }, func() (string, error) { r, e := s.VerificationClaim(ctx, "e"); return r.ClaimedAt, e }, "t"},
		{"ack_evidence", func() error {
			return s.RecordAckEvidence(ctx, AckEvidence{EventID: "e", Tier: "host_read", ObservedAt: "t"})
		}, func() (string, error) { r, e := s.AckEvidence(ctx, "e"); return r.Tier, e }, "host_read"},
		{"refusals", func() error {
			return s.RecordRefusal(ctx, Refusal{At: "t", RelationshipID: sql.NullString{String: "r", Valid: true}, Reason: "invalid"})
		}, func() (string, error) {
			r, e := s.Refusals(ctx, "r")
			if e != nil {
				return "", e
			}
			return r[0].Reason, nil
		}, "invalid"},
		{"work_reports", func() error {
			return s.RecordWorkReport(ctx, WorkReport{EventID: "e", Submission: 1, RelationshipID: "r", Generation: 1, RevisionHash: "hash", Repository: "repo", Summary: "summary", NextAction: "review", RecordedAt: "t"})
		}, func() (string, error) { r, e := s.WorkReport(ctx, "e", 1); return r.Summary, e }, "summary"},
		{"work_report_handoffs", func() error {
			return s.RecordWorkReportHandoff(ctx, WorkReportHandoff{EventID: "e", Submission: 1, RequiredDeclared: "[]", Checks: "[]", ReviewCoverage: "[]", ThreadDispositions: "[]", RecordedAt: "t"})
		}, func() (string, error) { r, e := s.WorkReportHandoff(ctx, "e", 1); return r.Checks, e }, "[]"},
		{"attempt_report_submissions", func() error {
			return s.RecordAttemptReportSubmission(ctx, AttemptReportSubmission{RequestID: "req", EventID: "e", Submission: 1, FrozenAt: "t"})
		}, func() (string, error) { r, e := s.AttemptReportSubmission(ctx, "req"); return r.EventID, e }, "e"},
		{"failed_operations", func() error {
			return s.RecordFailedOperation(ctx, FailedOperation{ScopeKey: "scope", Operation: "send", Detail: "failed", OccurredAt: "t"})
		}, func() (string, error) { r, e := s.FailedOperation(ctx, "scope", "send"); return r.Detail, e }, "failed"},
		{"reconcile_gate", func() error {
			return s.RecordReconcileGate(ctx, ReconcileGate{RequestID: "req", RetryRequired: true, UpdatedAt: "t"})
		}, func() (string, error) { r, e := s.ReconcileGate(ctx, "req"); return r.UpdatedAt, e }, "t"},
		{"discovery_cursors", func() error {
			return s.RecordDiscoveryCursor(ctx, DiscoveryCursor{TaskID: "task", Listing: "archive", Cursor: sql.NullString{String: "next", Valid: true}, UpdatedAt: "t"})
		}, func() (string, error) { r, e := s.DiscoveryCursor(ctx, "task", "archive"); return r.Cursor.String, e }, "next"},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			// When: a typed row is written.
			if err := check.write(); err != nil {
				t.Fatal(err)
			}
			// Then: the typed reader returns its persisted field.
			got, err := check.read()
			if err != nil || got != check.want {
				t.Fatalf("got %q want %q: %v", got, check.want, err)
			}
		})
	}
}
