package hook

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

var testRoot string

func TestMain(m *testing.M) {
	testsupport.Main(m, func(string) (func() error, error) {
		root, err := filepath.Abs("../../..")
		testRoot = root
		return nil, err
	})
}
func binary(t *testing.T) string {
	t.Helper()
	return testsupport.CRW(t)
}

// fixtureStore opens a store: Open creates an absent one as owner=go at epoch 1.
func fixtureStore(ctx context.Context, path, socket string) (*store.Store, error) {
	return store.Open(ctx, path, socket)
}
func writeTest(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	// A caller may chmod the result and run it (Test33ReviewD12 writes bin/crw this way), so the
	// descriptor is open only under syscall.ForkLock: a fork in that window would inherit it and
	// leave the path unexecutable (ETXTBSY, golang/go#22315).
	syscall.ForkLock.RLock()
	writeErr := os.WriteFile(path, raw, 0600)
	syscall.ForkLock.RUnlock()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
}
