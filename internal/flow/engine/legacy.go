package engine

import "fmt"

// OpLegacyReleaseChild is the operation with which the old project lane started a node's issue child. The issue-child path is retired
// (CRW-1415): no script calls it and no lane installs it, and the rows an old run wrote with it are read only.
const OpLegacyReleaseChild = "crw.releaseChild"

// LegacyIssueChild says why a run is not resumed ("" when nothing in its journal stops it): a run whose journal holds a crw.releaseChild
// row is a legacy issue-child run. The current project lane sends a node sized for a child up as a size split at the position where the
// old lane decided and started the child, so its script, run again over that journal, would not replay the start: it would escalate the
// node live and leave the child the old row started running, and every later position on the parent's path would run live. Its records
// stay read only (crw flow show, the summary and the run readers read them); the run is not resumed: engine.Run refuses its resume
// before anything is written, and the commands that start a run process for a resume ask it first.
func LegacyIssueChild(events []Event, runID string) string {
	for _, ev := range events {
		if ev.Kind != KindCall || ev.RunID != runID || ev.Op != OpLegacyReleaseChild {
			continue
		}
		node := rowStr(ev.Data, "node")
		if node == "" {
			node = "?"
		}
		return fmt.Sprintf("run %s is a legacy issue-child run: its journal holds a %s row (call %s, node %s) of the retired issue-child path (CRW-1415), which the current project lane does not replay; the run is not resumed and its records stay read only (crw flow show and the summary read them)", runID, OpLegacyReleaseChild, ev.CallID, node)
	}
	return ""
}
