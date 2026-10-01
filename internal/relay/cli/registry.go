package cli

import (
	"context"
	"io"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

// Version and Build are set by cmd/crw; doctor reports them in its runtime block.
var Version, Build = "dev", ""

// Execute is the codex-session-relay console script: the relay CLI under its own name.
func Execute(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	return ExecuteAs(ctx, "codex-session-relay", argv, stdout, stderr)
}

// ExecuteAs is cli.main: the relay CLI invoked as argv0, through the one command table
// (dispatch.Execute) with every command of this package registered.
func ExecuteAs(ctx context.Context, argv0 string, argv []string, stdout, stderr io.Writer) int {
	return dispatch.Execute(ctx, argv0, argv, stdout, stderr)
}

// The commands this package implements, in cli.py's add_parser order.
func init() {
	dispatch.Register(nil,
		reportingShowCommand, reportingDeriveCommand, supervisorSelectCommand, supervisorStandingCommand,
		supervisorReportRecordedCommand, supervisorStageCommand, supervisorSendCommand, supervisorReadCommand,
		supervisorShowCommand, showCommand, statusCommand, daemonCommand)
	dispatch.Register(nil, serviceCommands()...)
	dispatch.Register(nil, doctorCommand, storeIdentityCommand, storeChallengeCommand, mergeEvidenceCommand)
}
