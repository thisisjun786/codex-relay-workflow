package cli

import (
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/skill"
)

// FactIssueReady is the release gate's verdict on the issues the run releases (CRW-1328): the flow path judges them with the one
// implementation of the gate (skill.CheckIssueReady) when its handoff carries their body (compile.Issue description and labels), as
// crw-run judges an issue before it releases one. A held issue (not_ready, design_first) and a warning of an end condition are a warn
// fact, printed with the preview and recorded in the run's spec as every preflight fact is (CRW-1318, the recording CRW-1316 follows);
// the run starts and releases the issue: the rule is warn and record, never a refusal. An issue whose body the handoff does not carry
// is not judged here, and the fact is listed only when it is not a pass, as the project key's is.
const FactIssueReady = "issue_ready"

// issueReadyFact judges the issues of a run's milestone (every issue of the plan's project when the run names no milestone).
func issueReadyFact(in compile.Input, plan, milestone string) (Fact, bool) {
	p, ok := HandoffProject(in, plan)
	if !ok {
		return Fact{}, false
	}
	var notes []string
	judged := 0
	for _, is := range p.Issues {
		if (milestone != "" && is.Milestone != milestone) || strings.TrimSpace(is.Description) == "" {
			continue
		}
		judged++
		verdict, err := skill.CheckIssueReady(skill.ReadyIssue{ID: is.Key, Title: is.Title, Description: is.Description, Labels: is.Labels})
		switch {
		case err != nil:
			notes = append(notes, fmt.Sprintf("%s: the release gate cannot read the body: %v", is.Key, err))
			continue
		case !verdict.Ready:
			notes = append(notes, fmt.Sprintf("%s is %s: %s", is.Key, verdict.Decision, strings.Join(verdict.Reasons, "; ")))
		}
		for _, w := range verdict.Warnings {
			notes = append(notes, is.Key+" "+w)
		}
	}
	if len(notes) == 0 {
		return Fact{}, false
	}
	return Fact{Name: FactIssueReady, Level: LevelWarn,
		Reading: fmt.Sprintf("the release gate over the %d issue(s) of the handoff that carry a body: %s", judged, strings.Join(notes, " | ")),
		Reason:  "the release gate holds or warns of these issues (crw skill issue-ready check); the flow path records it and releases them (warn and record, CRW-1318)"}.leveled(), true
}
