package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/skill"
)

// crw manage batch (CRW-1040, part a). A night batch is a set of Backlog issues chosen by one label and
// run once while nobody is watching; this file is the selection and the files around it. It is pure:
// the issue JSON is supplied by the caller (the Go product does not call Linear), nothing is released,
// and no relay is touched. The run that releases the chosen issues and stops at a budget is the flow
// night batch (a later part), which reads this plan's selected list and writes the digest the report is
// made from.

const batchUsage = "usage: crw manage batch plan --label LABEL [--issues FILE]\n" +
	"       crw manage batch decide --plan FILE --flow-plan PLAN --milestone M --token-budget N --deadline RFC3339 [--cap N] [--by TASK]\n" +
	"       crw manage batch report --digest FILE"

const batchPlanUsage = "usage: crw manage batch plan --label LABEL [--issues FILE]"

// batchPlanSchema is the schema of the plan document.
const batchPlanSchema = "crw-batch-plan/1"

// The gate that held an issue, in the order the selector applies them.
const (
	batchGateState     = "state"
	batchGateLabel     = "label"
	batchGateReady     = "ready"
	batchGateDuplicate = "duplicate"
)

var batchCommand = Command{Name: "batch", Summary: "select night batch issues from Linear issue JSON and write the morning report", Run: batchMain}

func init() { Register(batchCommand) }

// BatchSelected is an issue the plan selects.
type BatchSelected struct {
	Issue string `json:"issue"`
	Title string `json:"title"`
}

// BatchHeld is an issue the plan holds, with the gate that held it and every reason that gate found.
type BatchHeld struct {
	Issue   string   `json:"issue"`
	Title   string   `json:"title"`
	Gate    string   `json:"gate"`
	Reasons []string `json:"reasons"`
}

// BatchPlanDocument is what crw manage batch plan prints.
type BatchPlanDocument struct {
	Schema   string          `json:"schema"`
	Label    string          `json:"label"`
	Selected []BatchSelected `json:"selected"`
	Held     []BatchHeld     `json:"held"`
}

// BatchIssue is one issue as the Linear tools give it, reduced to what the selector reads.
type BatchIssue struct {
	// ID is the name the issue goes by in the plan: its identifier, or its id when it has none.
	ID string
	// Keys is every name the entry gave the issue (its identifier and its id, as given), so the same
	// issue written once by identifier and once by UUID is one issue. Empty means ID alone.
	Keys        []string
	Title       string
	Description string
	Labels      []string
	// States holds every state value the input carried (the name and the type), as given.
	States []string
}

// batchMain is crw manage batch.
func batchMain(ctx context.Context, e *Env, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.Stderr, batchUsage)
		return usageExit
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprintln(e.Stdout, batchUsage)
		return 0
	case "plan":
		return batchRunPlan(ctx, e, args[1:])
	case "decide":
		return batchRunDecide(ctx, e, args[1:])
	case "report":
		return batchRunReport(ctx, e, args[1:])
	}
	fmt.Fprintln(e.Stderr, batchUsage)
	fmt.Fprintf(e.Stderr, "crw manage batch: error: invalid command %q (choose from 'plan', 'decide', 'report')\n", args[0])
	return usageExit
}

// batchConfigRefused refuses a configuration file this product cannot use, in Run's words and status.
// Run skips its own refusal when any argument is -h or --help, but an option takes such a token as its
// value (--digest -h names the file -h), which is no help request; each subcommand therefore makes the
// refusal itself once it has answered a real help request, as crw manage audit package does, so a file
// this product cannot use never lets a batch command run on the defaults.
func batchConfigRefused(e *Env) (int, bool) {
	if err := coreConfigError(e); err != nil {
		fmt.Fprintf(e.Stderr, "crw manage: error: %v\n", err)
		return usageExit, true
	}
	return 0, false
}

// batchRefuse writes a usage-level refusal of one subcommand.
func batchRefuse(e *Env, usage, name string, err error) int {
	fmt.Fprintln(e.Stderr, usage)
	fmt.Fprintf(e.Stderr, "crw manage batch %s: error: %v\n", name, err)
	return usageExit
}

func batchRunPlan(_ context.Context, e *Env, args []string) int {
	if auditOptionsHelpRequested(args) {
		fmt.Fprintln(e.Stdout, batchPlanUsage)
		return 0
	}
	if code, refused := batchConfigRefused(e); refused {
		return code
	}
	values, err := auditPkgParseArgs(args, map[string]bool{"label": true, "issues": true})
	if err != nil {
		return batchRefuse(e, batchPlanUsage, "plan", err)
	}
	label, ok := values["label"]
	if !ok || strings.TrimSpace(label) == "" {
		return batchRefuse(e, batchPlanUsage, "plan", errors.New("the option --label needs a value"))
	}
	label = strings.TrimSpace(label)
	raw, err := batchReadInput(e, values["issues"])
	if err != nil {
		return batchRefuse(e, batchPlanUsage, "plan", err)
	}
	issues, err := BatchParseIssues(raw)
	if err != nil {
		return batchRefuse(e, batchPlanUsage, "plan", err)
	}
	data, err := json.Marshal(BatchPlan(label, issues))
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage batch plan: error: %v\n", err)
		return 3
	}
	if _, err := fmt.Fprintf(e.Stdout, "%s\n", data); err != nil {
		fmt.Fprintf(e.Stderr, "crw manage batch plan: error: %v\n", err)
		return 3
	}
	return 0
}

// batchReadInput reads the file named, or stdin when no file is named or the name is "-".
func batchReadInput(e *Env, path string) ([]byte, error) {
	if path == "" || path == "-" {
		return io.ReadAll(e.Stdin)
	}
	return os.ReadFile(path)
}

// batchName is one name out of a Linear value that is a plain string or an object with a name.
func batchName(raw json.RawMessage) (name, kind string, ok bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, "", true
	}
	var o struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &o) != nil || (o.Name == "" && o.Type == "") {
		return "", "", false
	}
	return o.Name, o.Type, true
}

// BatchParseIssues reads the issue list the caller supplies: a JSON array of issues in the shapes the
// Linear tools return (id or identifier; status, state or statusType; labels as names or objects).
func BatchParseIssues(raw []byte) ([]BatchIssue, error) {
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil || list == nil {
		return nil, errors.New("the issues must be a JSON list of issues")
	}
	issues := make([]BatchIssue, 0, len(list))
	for i, item := range list {
		var in struct {
			ID          string          `json:"id"`
			Identifier  string          `json:"identifier"`
			Title       string          `json:"title"`
			Description string          `json:"description"`
			Labels      json.RawMessage `json:"labels"`
			State       json.RawMessage `json:"state"`
			Status      json.RawMessage `json:"status"`
			StatusType  string          `json:"statusType"`
		}
		if err := json.Unmarshal(item, &in); err != nil {
			return nil, fmt.Errorf("issue %d is not an object with text fields: %v", i+1, err)
		}
		issue := BatchIssue{ID: strings.TrimSpace(in.Identifier), Title: in.Title, Description: in.Description}
		if issue.ID == "" {
			issue.ID = strings.TrimSpace(in.ID)
		}
		if issue.ID == "" {
			return nil, fmt.Errorf("issue %d has no id or identifier", i+1)
		}
		for _, key := range []string{in.Identifier, in.ID} {
			if key = strings.TrimSpace(key); key != "" {
				issue.Keys = append(issue.Keys, key)
			}
		}
		if len(in.Labels) > 0 && string(in.Labels) != "null" {
			var names []json.RawMessage
			if err := json.Unmarshal(in.Labels, &names); err != nil {
				return nil, fmt.Errorf("issue %s: labels must be a list of names", issue.ID)
			}
			for _, n := range names {
				name, _, ok := batchName(n)
				if !ok || name == "" {
					return nil, fmt.Errorf("issue %s: labels must be a list of names", issue.ID)
				}
				issue.Labels = append(issue.Labels, name)
			}
		}
		for _, field := range []json.RawMessage{in.State, in.Status} {
			if len(field) == 0 || string(field) == "null" {
				continue
			}
			name, kind, ok := batchName(field)
			if !ok {
				return nil, fmt.Errorf("issue %s: the state must be a name or an object with a name", issue.ID)
			}
			for _, v := range []string{name, kind} {
				if strings.TrimSpace(v) != "" {
					issue.States = append(issue.States, v)
				}
			}
		}
		if strings.TrimSpace(in.StatusType) != "" {
			issue.States = append(issue.States, in.StatusType)
		}
		issues = append(issues, issue)
	}
	return issues, nil
}

// batchBacklog is whether every state value the issue carries says Backlog, and the value it shows when
// not. An issue with no state value is not Backlog: the state is not guessed.
func batchBacklog(states []string) (bool, string) {
	if len(states) == 0 {
		return false, ""
	}
	for _, s := range states {
		if !strings.EqualFold(strings.TrimSpace(s), "backlog") {
			return false, s
		}
	}
	return true, ""
}

// batchFirstEntries ties the entries of the input that name one issue: two entries that share a name (an
// identifier or an id, without case) are one issue, also when only a third entry gives the correspondence
// of the names, wherever it stands in the input. Every correspondence of the whole input is collected
// before any entry is judged. It gives, for each entry, the index of the first entry of its issue.
func batchFirstEntries(issues []BatchIssue) []int {
	root := make([]int, len(issues))
	for i := range root {
		root[i] = i
	}
	find := func(i int) int {
		for root[i] != i {
			root[i] = root[root[i]]
			i = root[i]
		}
		return i
	}
	owner := map[string]int{}
	for i, issue := range issues {
		keys := issue.Keys
		if len(keys) == 0 {
			keys = []string{issue.ID}
		}
		for _, key := range keys {
			key = strings.ToLower(key)
			j, ok := owner[key]
			if !ok {
				owner[key] = i
				continue
			}
			// The root of a group is its first entry: the later root joins the earlier one.
			a, b := find(i), find(j)
			if a > b {
				a, b = b, a
			}
			root[b] = a
		}
	}
	first := make([]int, len(issues))
	for i := range issues {
		first[i] = find(i)
	}
	return first
}

// BatchPlan is the selection. An issue is selected only when it is Backlog, carries the label (no case)
// and passes the release gate (skill.CheckIssueReady, the one implementation of it); every other issue is
// held with the first gate that stopped it and that gate's reasons. A repeated issue is judged once, by its
// first entry, whether it is repeated by the same name or by its identifier and its id, and whatever entry
// of the input ties the two (batchFirstEntries; the names compare without case); every later entry of it
// is held as a duplicate naming the first. The order of the input is kept, so the same input gives the
// same plan.
func BatchPlan(label string, issues []BatchIssue) BatchPlanDocument {
	plan := BatchPlanDocument{Schema: batchPlanSchema, Label: label, Selected: []BatchSelected{}, Held: []BatchHeld{}}
	hold := func(issue BatchIssue, gate string, reasons ...string) {
		plan.Held = append(plan.Held, BatchHeld{Issue: issue.ID, Title: issue.Title, Gate: gate, Reasons: reasons})
	}
	first := batchFirstEntries(issues)
	for i, issue := range issues {
		if f := first[i]; f != i {
			hold(issue, batchGateDuplicate, fmt.Sprintf("duplicate: %s is an issue an earlier entry of the input already names (first as %s, entry %d; the input ties their id and identifier); only its first entry is judged", issue.ID, issues[f].ID, f+1))
			continue
		}
		if ok, got := batchBacklog(issue.States); !ok {
			if got == "" {
				hold(issue, batchGateState, "no state in the input: the issue is not assumed to be in Backlog")
			} else {
				hold(issue, batchGateState, fmt.Sprintf("state is %q, not Backlog", got))
			}
			continue
		}
		labelled := false
		for _, l := range issue.Labels {
			if strings.EqualFold(strings.TrimSpace(l), label) {
				labelled = true
			}
		}
		if !labelled {
			hold(issue, batchGateLabel, fmt.Sprintf("the issue does not carry the label %q", label))
			continue
		}
		verdict, err := skill.CheckIssueReady(skill.ReadyIssue{ID: issue.ID, Title: issue.Title, Description: issue.Description, Labels: issue.Labels})
		switch {
		case err != nil:
			hold(issue, batchGateReady, "the release gate could not read the issue: "+err.Error())
		case !verdict.Ready:
			hold(issue, batchGateReady, append([]string{"release gate: " + verdict.Decision}, verdict.Reasons...)...)
		default:
			plan.Selected = append(plan.Selected, BatchSelected{Issue: issue.ID, Title: issue.Title})
		}
	}
	return plan
}
