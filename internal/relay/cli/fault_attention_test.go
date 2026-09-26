package cli_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// FaultLedger.attention reads inside `with self.store.transaction()`, so it is one BEGIN
// IMMEDIATE unit: refused at once inside another open transaction (sqlite3's "cannot start a
// transaction within a transaction"), and joined into an enclosing composing() scope, where it
// sees that scope's uncommitted writes. A reading taken outside a transaction does neither.
func TestFaultAttention_reads_inside_a_write_transaction_like_python(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(t.TempDir(), "state", "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	seed := func(id string) func(context.Context, *sql.Conn) error {
		return func(ctx context.Context, conn *sql.Conn) error {
			_, err := conn.ExecContext(ctx, "INSERT INTO fault_publications (publication_id, fault_id, kind,"+
				" trigger_key, summary, identity_digest, state, created_at, updated_at)"+
				" VALUES (?,'f1','open_record','t','s','d','failed','t','t')", id)
			return err
		}
	}

	t.Run("nested in an open transaction it is refused at once", func(t *testing.T) {
		err := opened.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
			if err := seed("nested")(ctx, conn); err != nil {
				return err
			}
			_, err := cli.FaultAttention(ctx, opened, 0)
			return err
		})
		if !errors.Is(err, store.ErrNestedTransaction) {
			t.Fatalf("want ErrNestedTransaction, got %v", err)
		}
	})

	t.Run("inside a composing scope it joins and sees the scope's writes", func(t *testing.T) {
		var failed any
		err := opened.Compose(ctx, func(ctx context.Context, conn *sql.Conn) error {
			if err := seed("composed")(ctx, conn); err != nil {
				return err
			}
			result, err := cli.FaultAttention(ctx, opened, 0)
			if err != nil {
				return err
			}
			failed = unsentField(result, "failed")
			return errors.New("roll back")
		})
		if err == nil || err.Error() != "transaction body: roll back" || failed != int64(1) {
			t.Fatalf("failed = %v, err = %v", failed, err)
		}
	})
}

func unsentField(result contract.OrderedObject, key string) any {
	for _, field := range result {
		if field.Key != "unsent" {
			continue
		}
		for _, inner := range field.Value.(contract.OrderedObject) {
			if inner.Key == key {
				return inner.Value
			}
		}
	}
	return nil
}
