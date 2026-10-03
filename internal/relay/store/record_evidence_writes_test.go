package store

import (
	"context"
	"database/sql"
	"testing"
)

func TestEvidenceWrites_roundtrip_when_rows_are_recorded(t *testing.T) {
	t.Parallel()
	// Given: an isolated store and typed rows for the evidence tables the store writes.
	s := recordStore(t)
	ctx := context.Background()
	checks := []struct {
		name  string
		write func() error
		read  func() (string, error)
		want  string
	}{
		{"refusals", func() error {
			return s.RecordRefusal(ctx, Refusal{At: "t", RelationshipID: sql.NullString{String: "r", Valid: true}, Reason: "invalid"})
		}, func() (string, error) {
			r, e := s.Refusals(ctx, "r")
			if e != nil {
				return "", e
			}
			return r[0].Reason, nil
		}, "invalid"},
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
