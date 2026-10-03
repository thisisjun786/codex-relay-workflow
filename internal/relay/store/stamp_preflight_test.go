package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// The in-place preflight (CheckStartLikeFence) judges the whole durable stamp as the writable open
// does, so a marker-only command never passes a store no write could then use: a store that still
// says owner=go but lost another ownership key is refused by both.
func TestThePreflightJudgesTheWholeStampAsTheWritableOpenDoes(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"writer_protocol", "owner_epoch", "python_compatibility_build", "rollback_allowed"} {
		t.Run(key, func(t *testing.T) {
			path := filepath.Join(stateDir(t), "relay.sqlite3")
			s, err := Open(t.Context(), path, "")
			must(t, err)
			if err := CheckStartLikeFence(t.Context(), path); err != nil {
				t.Fatalf("a whole stamp was refused: %v", err)
			}
			if _, err := s.DB.ExecContext(t.Context(), "DELETE FROM schema_meta WHERE key=?", key); err != nil {
				t.Fatal(err)
			}
			must(t, s.Close())
			preflight := CheckStartLikeFence(t.Context(), path)
			var refused *RefusedError
			if !errors.As(preflight, &refused) || !strings.Contains(refused.Detail, key) {
				t.Fatalf("preflight with %s missing: %v", key, preflight)
			}
			if again, err := Open(t.Context(), path, ""); err == nil {
				_ = again.Close()
				t.Fatalf("the writable open admitted a store missing %s", key)
			}
		})
	}
}
