package skill

// ReadyIssue is one issue as a caller that is not the command line holds it: the identity, the title,
// the body and the label names, the fields `crw skill issue-ready check` reads. It has no exception: the
// recorded bypass is the management session's approval of one issue, and a caller that selects many
// issues for release has no such record to give.
type ReadyIssue struct {
	ID          string
	Title       string
	Description string
	Labels      []string
}

// ReadyVerdict is the release gate's answer for one issue, the same one the command reports.
type ReadyVerdict struct {
	// Decision is "ready", "not_ready" or "design_first".
	Decision string
	// Ready is whether the issue may be released: Decision is "ready".
	Ready         bool
	DesignFirst   bool
	Missing       []string
	OpenQuestions []string
	// Reasons is every reason the issue is held, in the order the command lists them.
	Reasons []string
	// Warnings are the end condition's commands that may pass having run no test (CRW-1328): additive, never a reason it is held.
	Warnings []string
	// EndCondition are the commands the done condition names, read and never run (CRW-1382): the flow's design-stage explorer runs them
	// once and records the exit code and the tests they ran.
	EndCondition []string
}

// CheckIssueReady runs the release gate on one issue. It is the one implementation of the gate: the
// command and every caller that selects issues for release (the night batch plan, CRW-1040) judge
// through it, so the rules are never written twice. It reads and writes nothing. An error is a body the
// gate cannot read; it decides nothing, and the caller holds the issue.
func CheckIssueReady(issue ReadyIssue) (ReadyVerdict, error) {
	report, err := readyReportFor(readyInput{ID: issue.ID, Title: issue.Title, Description: issue.Description, Labels: labelList(issue.Labels)})
	if err != nil {
		return ReadyVerdict{}, err
	}
	return ReadyVerdict{
		Decision:      report.Decision,
		Ready:         report.Decision == "ready",
		DesignFirst:   report.DesignFirst,
		Missing:       report.Missing,
		OpenQuestions: report.OpenQuestions,
		Reasons:       report.Reasons,
		Warnings:      report.Warnings,
		EndCondition:  report.EndCondition,
	}, nil
}
