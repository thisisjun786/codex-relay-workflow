package cli_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
)

func TestExecute_returnsUsageOnlyOnStderr_whenCommandUnregistered(t *testing.T) {
	// Given
	var stdout, stderr bytes.Buffer

	// When
	code := cli.Execute(context.Background(), []string{"nonexistent"}, &stdout, &stderr)

	// Then
	if code != contract.ExitUsage || stdout.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestExecute_returnsUsageOnlyOnStderr_whenGlobalFlagMalformed(t *testing.T) {
	// Given
	var stdout, stderr bytes.Buffer

	// When
	code := cli.Execute(context.Background(), []string{"--bogus"}, &stdout, &stderr)

	// Then
	if code != contract.ExitUsage || stdout.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
