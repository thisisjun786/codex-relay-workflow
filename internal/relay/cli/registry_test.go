package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
)

// A line the parser cannot read exits 2 with usage on stderr and nothing on stdout (contract
// fixture test_management_cli__test_a_disposition_outside_the_vocabulary_is_rejected_by_the_parser
// expects the same).
const argparseExit = 2

func TestExecute_returnsUsageOnlyOnStderr_whenCommandUnregistered(t *testing.T) {
	// Given
	var stdout, stderr bytes.Buffer

	// When
	code := cli.Execute(context.Background(), []string{"nonexistent"}, &stdout, &stderr)

	// Then
	if code != argparseExit || stdout.Len() != 0 || !strings.Contains(stderr.String(), `invalid choice: "nonexistent"`) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestExecute_returnsUsageOnlyOnStderr_whenGlobalFlagMalformed(t *testing.T) {
	// Given
	var stdout, stderr bytes.Buffer

	// When
	code := cli.Execute(context.Background(), []string{"--bogus"}, &stdout, &stderr)

	got := processResult{code, stdout.String(), stderr.String()}
	expectRunErr(t, "build_parser --bogus", got.code, got.out, got.err)
}
