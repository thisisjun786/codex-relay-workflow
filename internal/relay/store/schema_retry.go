package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SQLITE_SCHEMA is the result code of a statement that was prepared against a schema another connection
// has changed since, and that SQLite could not prepare again.
const sqliteSchema = 17

// schemaChangeRetries bounds how often execSchema runs a statement again after SQLITE_SCHEMA. SQLite
// prepares a changed statement again by itself, so the code reaches the caller only when the schema
// changed once more between the second preparation and the run: every concurrent opener that is still
// creating objects makes that possible, and each of them makes progress, so a handful of further
// attempts finds a schema that stays put.
const schemaChangeRetries = 8

// schemaAttempt is a deterministic seam for tests, called before every attempt of execSchema with the
// stage the statement belongs to; an error it returns stands for the attempt's own result. Production
// leaves it inert.
var schemaAttempt = func(stage string) error { return nil }

// isSchemaChanged reports whether err is SQLite's SQLITE_SCHEMA (database schema has changed), by result
// code (the driver's *sqlite.Error answers Code), whatever extended code it carries.
func isSchemaChanged(err error) bool {
	var coded interface{ Code() int }
	return errors.As(err, &coded) && coded.Code()&0xff == sqliteSchema
}

// retrySchemaChanged runs op, and runs it again, up to schemaChangeRetries times, while SQLite answers that
// the database schema has changed. It is for the steps of a writable open that read or create schema
// objects while a peer open may still be creating some: each is idempotent (CREATE ... IF NOT EXISTS, or
// a read), so a repeat after the peer's commit is the step's normal run against the new schema. Another
// store error, and the last SQLITE_SCHEMA after the bound, are returned as they are.
//
// Without it, two processes that open a store gaining the DAG zone at the same moment (the first
// writable open after an upgrade) fail one of them with "database schema has changed (17)": CRW-1054.
func retrySchemaChanged(ctx context.Context, stage string, op func() error) error {
	for attempt := 0; ; attempt++ {
		err := schemaAttempt(stage)
		if err == nil {
			err = op()
		}
		if err == nil || !isSchemaChanged(err) || attempt >= schemaChangeRetries {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}

// execSchema is retrySchemaChanged for one statement.
func execSchema(ctx context.Context, db *sql.DB, stage, statement string) error {
	return retrySchemaChanged(ctx, stage, func() error {
		_, err := db.ExecContext(ctx, statement)
		return err
	})
}
