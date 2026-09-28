package hook

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

var testRoot, testBuild string
var testBinary = sync.OnceValues(func() (string, error) {
	path := filepath.Join(testBuild, "crw")
	cmd := exec.Command("go", "build", "-o", path, "./cmd/crw")
	cmd.Dir = testRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build: %w\n%s", err, out)
	}
	return path, nil
})

func TestMain(m *testing.M) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testRoot = root
	cleanup, err := testsupport.IsolateRelayState()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testBuild, err = os.MkdirTemp("", "crw-hook-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := cleanup(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	if err := os.RemoveAll(testBuild); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
func python(t *testing.T) string {
	t.Helper()
	path := filepath.Join(testRoot, ".venv", "bin", "python")
	if _, err := os.Stat(path); err != nil {
		t.Fatal("locked workspace Python missing: run uv sync --locked")
	}
	return path
}
func binary(t *testing.T) string {
	t.Helper()
	path, err := testBinary()
	if err != nil {
		t.Fatal(err)
	}
	return path
}
func fixtureStore(ctx context.Context, path, socket string) (*store.Store, error) {
	if err := testsupport.SeedOwnership(ctx, path, socket, "go"); err != nil {
		return nil, err
	}
	return store.Open(ctx, path, socket)
}
func writeTest(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
