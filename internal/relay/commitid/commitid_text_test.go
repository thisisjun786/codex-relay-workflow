package commitid_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/commitid"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	_ "github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Only TEXT names a commit: a BLOB, a number or NULL names none, whatever its bytes or digits spell.
func TestOnlyTextNamesACommitInSQL(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cases := []struct {
		name string
		a, b any
	}{
		{"a BLOB of the head's bytes", []byte("head-a"), "head-a"},
		{"a BLOB on both sides", []byte("head-a"), []byte("head-a")},
		{"an integer against its digits", int64(1), "1"},
		{"NULL", nil, "head-a"},
	}
	for _, c := range cases {
		var got int64
		if err := s.DB.QueryRowContext(ctx, "SELECT "+commitid.SQLFunction+"(?, ?)", c.a, c.b).Scan(&got); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != 0 {
			t.Fatalf("%s named a commit: %s(%v, %v) = %d, want 0", c.name, commitid.SQLFunction, c.a, c.b, got)
		}
	}
}
