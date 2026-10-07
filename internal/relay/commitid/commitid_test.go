package commitid_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/commitid"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Each row is a pair of commit identities and whether they name the same commit. Surrounding Unicode whitespace
// is removed and the case is folded before comparing; an empty identity names no commit. The SQL function must
// answer as Same does for every row, on a store connection.
var commitPairs = []struct {
	a, b string
	same bool
}{
	{"head-a", "head-a", true},
	{"HEAD-A", "head-a", true},
	{" head-a ", "head-a", true},
	{"\thead-a\n", "head-a", true},
	{"head-a\v", "head-a", true},
	{"\fhead-a\r", "head-a", true},
	{"\u00a0head-a", "head-a", true},
	{"head-a\u0085", "head-a", true},
	{"\u2003head-a", "head-a", true},
	{"head-a\u3000", "head-a", true},
	{"head-a", "head-b", false},
	{"", "", false},
	{" ", " ", false},
	{"\u3000", "\u3000", false},
	{"head-a", "", false},
}

func TestSameDefinesOneCommitIdentity(t *testing.T) {
	for _, p := range commitPairs {
		if got := commitid.Same(p.a, p.b); got != p.same {
			t.Fatalf("Same(%q, %q) = %v, want %v", p.a, p.b, got, p.same)
		}
	}
}

func TestTheStoreFunctionAnswersAsSameDoes(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, p := range commitPairs {
		var got int64
		if err := s.DB.QueryRowContext(ctx, "SELECT "+commitid.SQLFunction+"(?, ?)", p.a, p.b).Scan(&got); err != nil {
			t.Fatalf("%s(%q, %q): %v", commitid.SQLFunction, p.a, p.b, err)
		}
		if (got == 1) != p.same {
			t.Fatalf("%s(%q, %q) = %d, want same=%v", commitid.SQLFunction, p.a, p.b, got, p.same)
		}
	}
	var null int64
	if err := s.DB.QueryRowContext(ctx, "SELECT "+commitid.SQLFunction+"(NULL, 'head-a')").Scan(&null); err != nil || null != 0 {
		t.Fatalf("a NULL identity = %d, %v; want 0", null, err)
	}
}
