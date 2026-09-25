package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestRecordQueries_read_frozen_rows_when_present(t *testing.T) {
	// Given: records in the shipped schema, with nullable fields left unset.
	store := recordStore(t)
	ctx := context.Background()
	inserts := []string{
		`INSERT INTO generations (relationship_id,execution_generation,dispatch_request_id,anchor_state,opened_at) VALUES ('r',2,'req','pending','t')`,
		`INSERT INTO generation_turns (relationship_id,execution_generation,turn_id,evidence,admitted_at) VALUES ('r',2,'turn','host','t')`,
		`INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) VALUES ('event','r',2,'hash','failed','child','thread','turn','completed','{"z":1,"a":2}','t','t')`,
		`INSERT INTO observations (thread_id,turn_id,terminal_status,classification,observed_at) VALUES ('thread','turn','completed','ordinary','t')`,
		`INSERT INTO deliveries (event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,created_at,updated_at) VALUES ('event','r','handoff','parent','thread','pending','t','t')`,
		`INSERT INTO attempts (request_id,event_id,attempt_no,kind,internal_state,observed_at) VALUES ('req','event',1,'send','allocated','t')`,
		`INSERT INTO attempt_messages (request_id,event_id,attempt_no,kind,message,rendered_at) VALUES ('req','event',1,'send','frozen bytes','t')`,
		`INSERT INTO revision_lineage (relationship_id,execution_generation,event_id,revision_hash,declared_by,recorded_at) VALUES ('r',2,'event','hash','child','t')`,
		`INSERT INTO acks (event_id,record,ack_turn_id,accepted,verified,ack_at) VALUES ('event','{"accepted":true}','ack-turn',1,'host_read','t')`,
		`INSERT INTO verdicts (event_id,record,verdict,verdict_turn_id,decided_at) VALUES ('event','{"verdict":"pass"}','pass','verdict-turn','t')`,
		`INSERT INTO ack_evidence (event_id,tier,observed_at) VALUES ('event','host_read','t')`,
	}
	for _, statement := range inserts {
		if _, err := store.DB.ExecContext(ctx, statement); err != nil {
			t.Fatalf("fixture %s: %v", statement, err)
		}
	}
	// When/Then: each typed query reads the persisted value, not a reconstructed one.
	checks := []struct {
		name  string
		query func() (string, error)
		want  string
	}{
		{"generation", func() (string, error) { r, e := store.Generation(ctx, "r", 2); return r.DispatchRequestID, e }, "req"},
		{"generation_turn", func() (string, error) { r, e := store.GenerationTurn(ctx, "r", 2, "turn"); return r.Evidence, e }, "host"},
		{"event_receipt", func() (string, error) { r, e := store.Event(ctx, "event"); return r.Receipt, e }, `{"z":1,"a":2}`},
		{"observation", func() (string, error) {
			r, e := store.Observation(ctx, "thread", "turn", "completed")
			return r.Classification, e
		}, "ordinary"},
		{"delivery", func() (string, error) { r, e := store.Delivery(ctx, "event"); return r.State, e }, "pending"},
		{"attempt", func() (string, error) { r, e := store.Attempt(ctx, "req"); return r.InternalState, e }, "allocated"},
		{"attempt_message", func() (string, error) { r, e := store.AttemptMessage(ctx, "req"); return r.Message, e }, "frozen bytes"},
		{"lineage", func() (string, error) { r, e := store.RevisionLineage(ctx, "r", 2, "event"); return r.RevisionHash, e }, "hash"},
		{"ack", func() (string, error) { r, e := store.Ack(ctx, "event"); return r.Record, e }, `{"accepted":true}`},
		{"verdict", func() (string, error) { r, e := store.Verdict(ctx, "event"); return r.Decision, e }, "pass"},
		{"ack_evidence", func() (string, error) { r, e := store.AckEvidence(ctx, "event"); return r.Tier, e }, "host_read"},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			got, err := check.query()
			if err != nil || got != check.want {
				t.Fatalf("got %q want %q: %v", got, check.want, err)
			}
		})
	}
}

func TestTransaction_refuses_immediate_foreign_key_violation(t *testing.T) {
	// Given: an explicitly constrained table in a test-only database.
	store := recordStore(t)
	ctx := context.Background()
	if _, err := store.DB.ExecContext(ctx, `CREATE TABLE immediate_guard (id INTEGER PRIMARY KEY, parent INTEGER REFERENCES immediate_guard(id))`); err != nil {
		t.Fatal(err)
	}
	// When: writing a child without a parent in the transaction.
	err := store.Transaction(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO immediate_guard (id,parent) VALUES (1,2)`)
		return err
	})
	// Then: the violation is returned and the row remains absent.
	if err == nil {
		t.Fatal("foreign key violation accepted")
	}
	var count int
	if scanErr := store.DB.QueryRowContext(ctx, `SELECT count(*) FROM immediate_guard`).Scan(&count); scanErr != nil || count != 0 {
		t.Fatalf("partial row: %d: %v", count, scanErr)
	}
}

func TestRecordQueries_distinguish_missing_rows(t *testing.T) {
	// Given: an empty store.
	store := recordStore(t)
	ctx := context.Background()
	queries := []func() error{
		func() error { _, err := store.Generation(ctx, "r", 1); return err },
		func() error { _, err := store.GenerationTurn(ctx, "r", 1, "turn"); return err },
		func() error { _, err := store.Observation(ctx, "thread", "turn", "completed"); return err },
		func() error { _, err := store.Delivery(ctx, "event"); return err },
		func() error { _, err := store.Attempt(ctx, "request"); return err },
		func() error { _, err := store.AttemptMessage(ctx, "request"); return err },
		func() error { _, err := store.RevisionLineage(ctx, "r", 1, "event"); return err },
		func() error { _, err := store.Ack(ctx, "event"); return err },
		func() error { _, err := store.Verdict(ctx, "event"); return err },
		func() error { _, err := store.AckEvidence(ctx, "event"); return err },
	}
	// When/Then: an absent row preserves the standard sql.ErrNoRows identity.
	for _, query := range queries {
		if err := query(); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("missing row returned %v", err)
		}
	}
}
