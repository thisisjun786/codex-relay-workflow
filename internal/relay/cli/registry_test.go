package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
)

// argparse exits 2 with usage on stderr and nothing on stdout (measured against the Python
// relay CLI; contract fixture test_management_cli__test_a_disposition_outside_the_vocabulary_
// is_rejected_by_the_parser expects the same).
const argparseExit = 2

func TestExecute_returnsUsageOnlyOnStderr_whenCommandUnregistered(t *testing.T) {
	// Given
	var stdout, stderr bytes.Buffer

	// When
	code := cli.Execute(context.Background(), []string{"nonexistent"}, &stdout, &stderr)

	// Then
	if code != argparseExit || stdout.Len() != 0 || !strings.Contains(stderr.String(), "invalid choice: 'nonexistent'") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestExecute_returnsUsageOnlyOnStderr_whenGlobalFlagMalformed(t *testing.T) {
	// Given
	var stdout, stderr bytes.Buffer

	// When
	code := cli.Execute(context.Background(), []string{"--bogus"}, &stdout, &stderr)

	// argparse requires the command before reporting an unknown root option.
	// Compare the live parser so this test cannot preserve the old Go-only order.
	want := runParityProcess(t, os.Environ(), filepath.Join(repositoryRoot(t), ".venv/bin/python"),
		"-c", `from codex_session_relay.cli import build_parser; build_parser().parse_args(['--bogus'])`)
	got := processResult{code, stdout.String(), stderr.String()}
	if got != want {
		t.Fatalf("global parser byte diff\nGo=%+v\nPython=%+v", got, want)
	}
}
