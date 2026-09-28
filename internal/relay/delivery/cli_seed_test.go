package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

var cliSeedRoot string
var cliSeedOnce sync.Once
var cliSeedBytes []byte
var cliSeedErr error

// The Python registry creates the seed once. Each CLI side gets its own database
// and workspace; only the seed's two workspace fields need rebinding. Every CLI
// operation still executes independently in Go and live Python.
func copyCLISeed(t *testing.T, state, work string) {
	t.Helper()
	cliSeedOnce.Do(func() {
		command := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repoRoot(t), "internal/relay/delivery/testdata/cliseed.py"), cliSeedRoot, filepath.Join(cliSeedRoot, "work"))
		command.Dir = filepath.Join(repoRoot(t), "packages", "codex-session-relay")
		output, err := command.CombinedOutput()
		if err != nil {
			cliSeedErr = fmt.Errorf("Python CLI seed: %w: %s", err, output)
			return
		}
		if !strings.HasPrefix(strings.TrimSpace(string(output)), "rel-") {
			cliSeedErr = fmt.Errorf("unexpected Python CLI seed: %s", output)
			return
		}
		cliSeedBytes, cliSeedErr = os.ReadFile(filepath.Join(cliSeedRoot, "relay.sqlite3"))
	})
	mustDo(t, cliSeedErr)
	mustDo(t, os.MkdirAll(state, 0700))
	mustDo(t, os.MkdirAll(work, 0700))
	path := filepath.Join(state, "relay.sqlite3")
	mustDo(t, os.WriteFile(path, cliSeedBytes, 0600))
	ctx := context.Background()
	s, err := store.Open(ctx, path, "")
	mustDo(t, err)
	roots, err := json.Marshal([]string{work})
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
