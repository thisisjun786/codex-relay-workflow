package faults

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

// executeAsCLI runs a fault command line through the relay CLI's one entry point.
func executeAsCLI(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	return dispatch.Execute(ctx, "codex-session-relay", argv, stdout, stderr)
}

// stateAbsent reports whether dir does not exist.
func stateAbsent(t testing.TB, dir string) bool {
	t.Helper()
	_, err := os.Lstat(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err != nil
}
