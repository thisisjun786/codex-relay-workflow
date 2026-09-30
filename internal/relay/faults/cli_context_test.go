package faults

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// executeAsCLI runs a fault command line as the relay CLI hands it to ExecuteAs
// (cli/registry.go, readOnlyContext): a read-only form (argparse.ReadOnlyForm, cli.py's
// _read_only_command) runs under store.WithReadOnlyCommand, so it never creates an absent
// store and reads a store this runtime may not write through the read-only store, as
// Services.store serves it. ExecuteAs alone is not a surface any host runs.
func executeAsCLI(ctx context.Context, argv []string, stdout, stderr io.Writer) (int, bool) {
	for i := 0; i < len(argv); i++ {
		name, _, has := strings.Cut(argv[i], "=")
		switch name {
		case "--json":
			continue
		case "--state", "--socket", "--kind-module":
			if !has {
				i++
			}
			continue
		}
		if argparse.ReadOnlyForm(argv[i:]) {
			ctx = store.WithReadOnlyCommand(ctx)
		}
		break
	}
	return ExecuteAs(ctx, "codex-session-relay", argv, stdout, stderr, nil)
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
