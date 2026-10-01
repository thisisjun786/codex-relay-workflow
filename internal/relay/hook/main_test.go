package hook

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

var testRoot string

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
	code := m.Run()
	if err := errors.Join(cleanup(), testsupport.RemoveCRW()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
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
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
