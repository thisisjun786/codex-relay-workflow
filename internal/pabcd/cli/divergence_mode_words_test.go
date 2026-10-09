package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// TestDivergenceCliModeWriteFailureIsSpelledAsNodeSpellsIt pins the one uncaught path of the oracle's divergence CLI
// (divergence-cli.ts:115): a mode write that fails reaches the entry point's handler as "codexclaw cli failed: <err.message>",
// where Node's message for a refused create is "EACCES: permission denied, open '<path>'". On dev the caller printed Go's
// "open <path>: permission denied" (CRW-750, folded into CRW-749). The temp file the write creates sits beside the mode file
// and carries the oracle's name, so the path ends in ".tmp".
func TestDivergenceCliModeWriteFailureIsSpelledAsNodeSpellsIt(t *testing.T) {
	cwd := divergenceCliSandbox(t)
	dir := filepath.Join(cwd, crwdir.DirName, "divergence")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if probe, err := os.CreateTemp(dir, "probe"); err == nil {
		probe.Close()
		t.Skip("the divergence directory stays writable despite mode 0555 (privileged user)")
	}
	_, err := RunDivergenceCli([]string{"mode", "on", "--session", "cli", "--collapse", "P", "--reason", "plateau"}, cwd)
	if err == nil {
		t.Fatal("a mode write into a read-only directory succeeded")
	}
	want := regexp.MustCompile(`^EACCES: permission denied, open '` + regexp.QuoteMeta(filepath.Join(dir, "cli.mode.json")) + `\.\d+\.\d+\.tmp'$`)
	if !want.MatchString(err.Error()) {
		t.Errorf("the failure reads %q, want Node's EACCES spelling of the temp file", err.Error())
	}
	var path *os.PathError
	if !errors.As(err, &path) {
		t.Errorf("the failure no longer unwraps to the *os.PathError: %v", err)
	}
}

// A cancelled run keeps the context's own error, so the harness still answers Interrupted.
func TestDivergenceCliModeWriteKeepsTheContextError(t *testing.T) {
	cwd := divergenceCliSandbox(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := RunDivergenceCliContext(ctx, []string{"mode", "on", "--session", "cli", "--collapse", "P", "--reason", "plateau"}, cwd)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled mode write: %v", err)
	}
}
