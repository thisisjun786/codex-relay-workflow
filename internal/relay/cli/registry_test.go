package cli_test

import (
	"bytes"
	"context"
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

	// Then
	if code != argparseExit || stdout.Len() != 0 || !strings.Contains(stderr.String(), "unrecognized arguments: --bogus") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
