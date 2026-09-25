package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestAuxiliaryQueries_read_rows_when_present(t *testing.T) {
	// Given: persisted evidence, report, refusal, and cursor rows.
	store := recordStore(t)
	ctx := context.Background()
	inserts := []string{
		`INSERT INTO verification_claims (event_id,claimed_at) VALUES ('event','t')`,
		`INSERT INTO refusals (at,relationship_id,reason) VALUES ('t','r','invalid')`,
		`INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event',2,'r',1,'hash','repo','pass','ok','1','summary','review','t')`,
		`INSERT INTO work_report_handoffs (event_id,submission_no,is_draft,required_declared,checks,review_coverage,thread_dispositions,recorded_at) VALUES ('event',2,1,'[]','[]','[]','[]','t')`,
		`INSERT INTO attempt_report_submissions (request_id,event_id,submission_no,frozen_at) VALUES ('req','event',2,'t')`,
		`INSERT INTO failed_operations (scope_key,operation,detail,occurred_at) VALUES ('scope','send','failed','t')`,
		`INSERT INTO reconcile_gate (request_id,retry_required,updated_at) VALUES ('req',1,'t')`,
		`INSERT INTO discovery_cursors (task_id,listing,scanned,updated_at) VALUES ('task','turns',3,'t')`,
	}
	for _, statement := range inserts {
		if _, err := store.DB.ExecContext(ctx, statement); err != nil {
			t.Fatalf("fixture: %v: %s", err, statement)
		}
	}
	// When/Then: each query retrieves its machine-consumed field.
	checks := []struct {
		name string
		read func() (string, error)
		want string
	}{
		{"verification_claim", func() (string, error) { r, e := store.VerificationClaim(ctx, "event"); return r.ClaimedAt, e }, "t"},
		{"refusal", func() (string, error) {
			r, e := store.Refusals(ctx, "r")
			if e != nil {
				return "", e
			}
			return r[0].Reason, nil
		}, "invalid"},
		{"work_report", func() (string, error) { r, e := store.WorkReport(ctx, "event", 2); return r.NextAction, e }, "review"},
		{"work_report_handoff", func() (string, error) {
			r, e := store.WorkReportHandoff(ctx, "event", 2)
			if r.IsDraft {
				return "draft", e
			}
			return "final", e
		}, "draft"},
		{"attempt_report_submission", func() (string, error) { r, e := store.AttemptReportSubmission(ctx, "req"); return r.EventID, e }, "event"},
		{"failed_operation", func() (string, error) { r, e := store.FailedOperation(ctx, "scope", "send"); return r.Detail, e }, "failed"},
		{"reconcile_gate", func() (string, error) {
			r, e := store.ReconcileGate(ctx, "req")
			if r.RetryRequired {
				return "retry", e
			}
			return "done", e
		}, "retry"},
		{"discovery_cursor", func() (string, error) { r, e := store.DiscoveryCursor(ctx, "task", "turns"); return r.TaskID, e }, "task"},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			got, err := check.read()
			if err != nil || got != check.want {
				t.Fatalf("got %q want %q: %v", got, check.want, err)
			}
		})
	}
}

func TestAuxiliaryQueries_report_missing_rows(t *testing.T) {
	// Given: an empty database.
	store := recordStore(t)
	ctx := context.Background()
	reads := []func() error{
		func() error { _, e := store.VerificationClaim(ctx, "missing"); return e },
		func() error { _, e := store.WorkReport(ctx, "missing", 1); return e },
		func() error { _, e := store.WorkReportHandoff(ctx, "missing", 1); return e },
		func() error { _, e := store.AttemptReportSubmission(ctx, "missing"); return e },
		func() error { _, e := store.FailedOperation(ctx, "missing", "send"); return e },
		func() error { _, e := store.ReconcileGate(ctx, "missing"); return e },
		func() error { _, e := store.DiscoveryCursor(ctx, "missing", "turns"); return e },
	}
	// When/Then: missing singular records preserve sql.ErrNoRows.
	for _, read := range reads {
		if err := read(); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("missing row: %v", err)
		}
	}
	refusals, err := store.Refusals(ctx, "missing")
	if err != nil || len(refusals) != 0 {
		t.Fatalf("missing refusals: %+v: %v", refusals, err)
	}
}
