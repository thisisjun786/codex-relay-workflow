package adapter

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// managed-start reads the worker's scope claim and lock where the service that wrote them does:
// cmd_managed_start's _service_for resolves the scope directory as pathlib spells it, so a
// '..' after a symlink stays and names the directory the kernel resolves, not the lexically
// cleaned one (a filepath.Abs spelling read another directory's claim).
func TestManagedStartReadsTheScopeTheServiceWrites(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "far", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "far", "deep"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	spelled := filepath.Join(root, "link") + "/../scopes"
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", spelled)
	observer, err := workerObserver(cli.Services{Selection: store.StateSelection{Path: filepath.Join(root, "S")}, SocketPath: filepath.Join(root, "app.sock")})
	if err != nil {
		t.Fatal(err)
	}
	if observer.Scope != spelled || observer.Authority != "isolated" {
		t.Fatalf("scope %q (%s), want %q (isolated)", observer.Scope, observer.Authority, spelled)
	}
}
