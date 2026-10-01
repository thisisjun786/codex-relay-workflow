package delivery

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// copyCLISeed writes the CLI seed at state/relay.sqlite3 and binds it to work. The seed
// (testdata/fixtures/cli-seed.sqlite3.gz) is the store the Python registry created through
// testdata/cliseed.py before the Python implementation left: one registered relationship,
// Python-owned. Each copy is published again from its own stamp (testsupport.Rehome), taken over
// by Go, and its two workspace fields rebound to work.
func copyCLISeed(t *testing.T, state, work string) {
	t.Helper()
	mustDo(t, os.MkdirAll(state, 0700))
	mustDo(t, os.MkdirAll(work, 0700))
	path := filepath.Join(state, "relay.sqlite3")
	mustDo(t, os.WriteFile(path, golden.Fixture(t, "cli-seed.sqlite3"), 0600))
	testsupport.Rehome(t, path)
	roots, err := json.Marshal([]string{work})
	mustDo(t, err)
	testsupport.HandOver(t, path, "go")
	ctx := context.Background()
	s, err := store.Open(ctx, path, "")
	mustDo(t, err)
	_, updateErr := s.Querier(ctx).ExecContext(ctx, "UPDATE relationships SET child_cwd = ?, artifact_roots = ?", work, string(roots))
	closeErr := s.Close()
	mustDo(t, updateErr)
	mustDo(t, closeErr)
}

func TestCLI_seed_copies_are_independent(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		root := t.TempDir()
		state, work := filepath.Join(root, "state"), filepath.Join(root, "work")
		copyCLISeed(t, state, work)
		s, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
		mustDo(t, err)
		t.Cleanup(func() { mustDo(t, s.Close()) })
		var cwd, roots string
		mustDo(t, s.Querier(ctx).QueryRowContext(ctx, "SELECT child_cwd, artifact_roots FROM relationships").Scan(&cwd, &roots))
		wantRoots, err := json.Marshal([]string{work})
		mustDo(t, err)
		if cwd != work || roots != string(wantRoots) {
			t.Fatalf("seed workspace: cwd=%q roots=%q want=%q", cwd, roots, work)
		}
		// A later copy must retain its own workspace after this copy changes it.
		_, err = s.Querier(ctx).ExecContext(ctx, "UPDATE relationships SET child_cwd = 'changed', artifact_roots = '[]'")
		mustDo(t, err)
	}
}
