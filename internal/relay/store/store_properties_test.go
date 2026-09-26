package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
)

func TestStore_python_durable_properties(t *testing.T) {
	t.Run("test_schema_v1_carries_every_table_the_later_phases_need", func(t *testing.T) {
		s := recordStore(t)
		var count int
		if err := s.DB.QueryRowContext(context.Background(), `SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('acks','attempts','deliveries','events','generations','journal','observations','recipient_lifecycle','recipient_rate','refusals','relationships','schema_meta','verdicts','verification_claims','fault_ledger','fault_occurrences','fault_timeline','fault_remediations','fault_publications','fault_targets','fault_cursors','fault_target_projects','fault_publication_payloads','fault_links','fault_adoptions','fault_aliases','fault_publication_attempts','fault_budget_uses','fault_limits','fault_notifications','fault_policies','fault_overtaken_deliveries')`).Scan(&count); err != nil || count != 32 {
			t.Fatalf("required tables %d: %v", count, err)
		}
		loc, err := s.Locate(context.Background())
		if err != nil || loc.SchemaVersion != "1" {
			t.Fatalf("schema %+v %v", loc, err)
		}
	})
	t.Run("test_columns_the_delivery_and_ack_layers_need_exist_now", func(t *testing.T) {
		s := recordStore(t)
		for _, spec := range []struct{ table, column string }{{"deliveries", "kind"}, {"deliveries", "recipient_task_id"}, {"deliveries", "recipient_thread_id"}, {"deliveries", "lease_owner"}, {"deliveries", "lease_until"}, {"deliveries", "hold_reason"}, {"deliveries", "dispatch_evidence"}, {"deliveries", "provenance"}, {"attempts", "internal_state"}, {"attempts", "sealed"}, {"attempts", "operation_observation"}, {"attempts", "recipient_scan"}, {"attempts", "affirmative_evidence"}, {"acks", "verified"}, {"recipient_lifecycle", "archived"}, {"recipient_lifecycle", "goal_status"}, {"recipient_lifecycle", "can_accept_input"}, {"recipient_lifecycle", "deliverable"}, {"recipient_lifecycle", "withhold_reason"}} {
			rows, err := s.DB.QueryContext(context.Background(), "PRAGMA table_info("+spec.table+")")
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for rows.Next() {
				var index int
				var name, kind string
				var nullable, primary int
				var defaultValue any
				if err := rows.Scan(&index, &name, &kind, &nullable, &defaultValue, &primary); err != nil {
					t.Fatal(err)
				}
				if name == spec.column {
					found = true
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatalf("%s missing %s", spec.table, spec.column)
			}
		}
	})
	t.Run("test_the_database_file_is_private", func(t *testing.T) {
		s := recordStore(t)
		info, err := os.Stat(s.Path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("mode %v: %v", info, err)
		}
	})
	t.Run("test_an_exception_inside_a_transaction_leaves_no_partial_row", func(t *testing.T) {
		s := recordStore(t)
		ctx := context.Background()
		if err := s.Compose(ctx, func(joined context.Context, _ *sql.Conn) error {
			if err := s.AppendJournal(joined, JournalEntry{At: "t", Kind: "k", Subject: "s", Detail: "d"}); err != nil {
				return err
			}
			return errors.New("interrupted")
		}); err == nil {
			t.Fatal("failure lost")
		}
		rows, err := s.Journal(ctx, "k", "s")
		if err != nil || len(rows) != 0 {
			t.Fatalf("partial %+v %v", rows, err)
		}
	})
	t.Run("test_a_fault_after_the_body_still_rolls_back", func(t *testing.T) {
		s := recordStore(t)
		ctx := context.Background()
		if _, err := s.DB.ExecContext(ctx, `CREATE TABLE deferred_guard (id INTEGER PRIMARY KEY, parent INTEGER REFERENCES missing_parent(id) DEFERRABLE INITIALLY DEFERRED)`); err != nil {
			t.Fatal(err)
		}
		if err := s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
			_, err := conn.ExecContext(ctx, `INSERT INTO deferred_guard VALUES (1,1)`)
			return err
		}); err == nil {
			t.Fatal("commit accepted")
		}
		var count int
		if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM deferred_guard`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial %d %v", count, err)
		}
	})
	t.Run("test_a_failed_registration_is_not_a_registration", func(t *testing.T) {
		// Given: a fresh store.
		s := recordStore(t)
		r := Relationship{ID: "rel-0123456789abcdef", IssueKey: "REL-1", Status: StatusActive, ParentTaskID: "p", ChildTaskID: "c", Generation: 1, ArtifactRoots: "[]", AllowedRecipients: "[]", CreatedAt: "t", UpdatedAt: "t"}
		g := Generation{RelationshipID: r.ID, Number: 1, DispatchRequestID: "dispatch-1", AnchorState: AnchorPending, OpenedAt: "t"}
		// When: the registering process dies inside the registration transaction, after both
		// the relationship and its generation were written and before COMMIT.
		dieInsideTransaction(t, s, func(ctx context.Context, child *Store) error {
			return child.RecordRelationship(ctx, r, g, "h", "h")
		})
		// Then: neither the relationship nor any generation row survived.
		if _, err := s.Relationship(context.Background(), r.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("partial registration: %v", err)
		}
		var generations int
		if err := s.DB.QueryRowContext(context.Background(), `SELECT count(*) FROM generations`).Scan(&generations); err != nil || generations != 0 {
			t.Fatalf("generation rows %d: %v", generations, err)
		}
	})
	t.Run("test_state_survives_reopening_the_database", func(t *testing.T) {
		s := recordStore(t)
		ctx := context.Background()
		if err := s.AppendJournal(ctx, JournalEntry{At: "t", Kind: "durable", Subject: "s", Detail: "d"}); err != nil {
			t.Fatal(err)
		}
		other, err := Open(ctx, s.Path, "")
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		rows, err := other.Journal(ctx, "durable", "s")
		if err != nil || len(rows) != 1 {
			t.Fatalf("reopened %+v %v", rows, err)
		}
	})
	t.Run("test_identity_is_minted_once_and_survives_reopening", func(t *testing.T) {
		s := recordStore(t)
		ctx := context.Background()
		first, err := s.Locate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		other, err := Open(ctx, s.Path, "")
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		again, err := other.Locate(ctx)
		if err != nil || again.StoreID != first.StoreID || again.CreatedAt != first.CreatedAt {
			t.Fatalf("first %+v reopened %+v: %v", first, again, err)
		}
	})
}
