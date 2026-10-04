package cli

import "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"

// RunScanCli is the record runner on ParseScanCliArgs.
func RunScanCli(args ScanCliArgs) CliResult {
	return scanRecordRun(args, state.AppendInterviewEvent)
}

func scanRecordRun(args ScanCliArgs, appendEvent func(string, state.InterviewEvent) error) CliResult {
	return CliResult{Code: 1, Output: "scan record runner not implemented"}
}
