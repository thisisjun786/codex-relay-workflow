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

// oracleState is the state directory Go is asked a command line in after live Python answered
// it in pyState (pyCreated: whether Python's run left pyState in place, part of its recorded
// answer): pyState itself when Python left it absent, so both runtimes answer about the same
// absent store and a refusal naming its path is compared byte for byte; goState when Python
// created its store there, as each runtime writes only a store it owns.
func oracleState(pyCreated bool, pyState, goState string) string {
	if !pyCreated {
		return pyState
	}
	return goState
}

// neverCreated fails the test when Go created the state directory it was asked about while
// absent: an absent store is never created by a form Python answered without creating it.
func neverCreated(t *testing.T, pyState, state string) {
	t.Helper()
	if state == pyState && !stateAbsent(t, state) {
		t.Errorf("Go created %s, which Python left absent", state)
	}
}

func stateAbsent(t testing.TB, dir string) bool {
	t.Helper()
	_, err := os.Lstat(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err != nil
}
