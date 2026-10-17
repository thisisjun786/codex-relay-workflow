package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// ConfiguredIntegration is the verification of an integration batch's merged tree by the verifier the
// configuration selects (CRW-1594): a local section keeps CILocalIntegration unchanged; a github-actions
// section verifies through GitHubActionsVerifier, reading the batch's CRW_VERIFY_* from env, writing the
// sealed record to CRW_VERIFY_RECORD and keeping the caller's HOME and environment, so gh reads its own
// login. A host failure is IntegrationVerifierHostError; a failed verification is an ordinary error naming
// the failed jobs and tests.
func ConfiguredIntegration(tools crwconfig.VerifierTools, notes io.Writer) dagsched.IntegrationBatchVerifier {
	if tools.VerifierKind() == crwconfig.VerifierGitHubActions {
		return githubIntegration(tools, notes)
	}
	return CILocalIntegration(tools, notes)
}

func githubIntegration(tools crwconfig.VerifierTools, notes io.Writer) dagsched.IntegrationBatchVerifier {
	return func(ctx context.Context, dir string, env []string) error {
		vars := map[string]string{}
		for _, kv := range env {
			if k, v, ok := strings.Cut(kv, "="); ok {
				vars[k] = v
			}
		}
		if err := tools.Validate(); err != nil {
			return &dagsched.IntegrationVerifierHostError{Detail: err.Error()}
		}
		out, err := ops.GitHubActionsVerifier{Tools: tools}.Verify(ctx, ops.VerifyRequest{
			Head: vars["CRW_VERIFY_HEAD"], Base: vars["CRW_VERIFY_BASE"], Worktree: dir, Command: ops.CILocalCommand})
		if record := vars["CRW_VERIFY_RECORD"]; record != "" && len(out.Record) > 0 {
			// the receipt is written even when the run itself failed, before the error is judged
			if werr := verifyRecordWrite(record, out.Record); werr != nil {
				return &dagsched.IntegrationVerifierHostError{Detail: fmt.Sprintf("the verification record could not be written: %v", werr)}
			}
		}
		if err != nil {
			return &dagsched.IntegrationVerifierHostError{Detail: err.Error()}
		}
		if out.Note != "" && notes != nil {
			fmt.Fprintf(notes, "[crw note] integration verification: %s\n", out.Note)
		}
		if !out.Pass {
			failed := out.Summary
			for _, step := range out.FailedTests {
				for _, test := range step.Tests {
					failed += "; " + test
				}
			}
			return fmt.Errorf("the integration verification failed: %s", failed)
		}
		return nil
	}
}
