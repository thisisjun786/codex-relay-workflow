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
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

var cliSeedOnce sync.Once
var cliSeedBytes []byte
var cliSeedErr error

// The Python registry creates the seed once (recorded by recordedStore in CLISeed.json). Each
// CLI side gets its own database and workspace; only the seed's two workspace fields need
// rebinding. Every CLI operation still executes independently in Go and, when recording or
// checking, in live Python.
//
// Each copy is the Python-owned seed in a directory of its own. The runtime that serves the
// side (python or go) owns its copy and rebinds it itself: Go's copy is the seed after a
// takeover to Go.
func copyCLISeed(t *testing.T, state, work string, python bool) {
	t.Helper()
	cliSeedOnce.Do(func() {
		// The seed's own workspace path is written into the store: a fixed tree keeps it the same.
		seedRoot := processParityTree(t, "cli-seed")
		path := filepath.Join(seedRoot, "relay.sqlite3")
		if pyoracle.Live() {
			command := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repoRoot(t), "internal/relay/delivery/testdata/cliseed.py"), seedRoot, filepath.Join(seedRoot, "work"))
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
		}
		recordedStore(namedTB{t, "CLISeed"}, "seed", path)
		cliSeedBytes, cliSeedErr = os.ReadFile(path)
	})
	mustDo(t, cliSeedErr)
	mustDo(t, os.MkdirAll(state, 0700))
	mustDo(t, os.MkdirAll(work, 0700))
	path := filepath.Join(state, "relay.sqlite3")
	mustDo(t, os.WriteFile(path, cliSeedBytes, 0600))
	testsupport.Rehome(t, path)
	roots, err := json.Marshal([]string{work})
	mustDo(t, err)
	if python {
		command := exec.Command("uv", "run", "--no-sync", "python", "-c", `import sys
from contextlib import closing
from codex_session_relay.store import Store
with closing(Store(sys.argv[1])) as store:
    store.db.execute("UPDATE relationships SET child_cwd = ?, artifact_roots = ?", (sys.argv[2], sys.argv[3]))
`, path, work, string(roots))
		command.Dir = filepath.Join(repoRoot(t), "packages", "codex-session-relay")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("Python seed rebinding: %v: %s", err, output)
		}
		return
	}
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
		copyCLISeed(t, state, work, false)
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
