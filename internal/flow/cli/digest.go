package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

// The run digest of a night batch: the record the morning report (crw manage batch report, CRW-1040a) is made from. The digest is
// the shape internal/manage.BatchDigest reads (crw-batch-digest/1); `crw flow digest RUN` builds it from the run's journal, so what
// the report says was integrated, carried, cancelled or left to Jun is what the journal shows, and what the journal does not show
// is not claimed.

// BatchDigestSchema is the schema of the digest.
const BatchDigestSchema = "crw-batch-digest/1"

// What an item of the digest ended as (the words internal/manage reads).
const (
	OutcomeIntegrated = "integrated"
	OutcomeCancelled  = "cancelled"
	OutcomeCarried    = "carried"
	OutcomeDecision   = "needs_decision"
)

// DigestProvider is one provider's observation at the end of the run.
type DigestProvider struct {
	Name     string `json:"name"`
	Observed bool   `json:"observed"`
	Detail   string `json:"detail"`
}

// DigestItem is one issue's end state in the run.
type DigestItem struct {
	Issue    string `json:"issue"`
	Title    string `json:"title"`
	Outcome  string `json:"outcome"`
	Reason   string `json:"reason"`
	Evidence string `json:"evidence"`
}

// BatchDigest is the run digest.
type BatchDigest struct {
	Schema     string           `json:"schema"`
	Label      string           `json:"label"`
	Selected   []string         `json:"selected"`
	StartedAt  string           `json:"started_at"`
	FinishedAt string           `json:"finished_at"`
	Deadline   string           `json:"deadline"`
	Stopped    string           `json:"stopped"`
	Providers  []DigestProvider `json:"providers"`
	Items      []DigestItem     `json:"items"`
}

// DigestInput is what a digest is made from.
type DigestInput struct {
	Run    RunRecord
	Events []engine.Event
	// Spec is the run's spec (its token budget and deadline); the zero value when the run has none.
	Spec Spec
	// Result is the result the last process left, nil while none has.
	Result *Result
	// Label and Issues are the night batch the run was started for.
	Label  string
	Issues []string
	Now    time.Time
	// Live says whether a process holds the run now.
	Live bool
}

// milestoneOf reads the milestone the run's crw.milestone row recorded.
func milestoneOf(events []engine.Event) (ops.Milestone, bool) {
	for _, e := range events {
		if e.Kind == engine.KindCall && e.Op == ops.OpMilestone && e.Event == engine.EvCompleted {
			if raw, ok := e.Data["result"].(string); ok {
				var ms ops.Milestone
				if json.Unmarshal([]byte(raw), &ms) == nil && len(ms.Nodes) > 0 {
					return ms, true
				}
			}
		}
	}
	return ops.Milestone{}, false
}

// BuildDigest builds the digest of a night batch run from its journal.
func BuildDigest(in DigestInput) BatchDigest {
	info := Summarize(in.Run, in.Events, in.Live, 0)
	detail := Detail(in.Run, in.Events, in.Live, 0)
	d := BatchDigest{Schema: BatchDigestSchema, Label: in.Label, Selected: append([]string{}, in.Issues...), StartedAt: in.Run.StartedAt, Deadline: in.Spec.Deadline, Providers: []DigestProvider{}, Items: []DigestItem{}}
	if in.Result != nil {
		d.FinishedAt = in.Result.EndedAt
	}
	nodeOf, titleOf := map[string]string{}, map[string]string{}
	if ms, ok := milestoneOf(in.Events); ok {
		for _, n := range ms.Nodes {
			nodeOf[n.Issue], titleOf[n.Issue] = n.ID, n.Title
		}
	}
	nodes := map[string]NodeInfo{}
	for _, n := range detail.Nodes {
		nodes[n.Node] = n
	}
	// the questions that wait (by the rule ps, show and next read them by, Question.Waits), the requests to send up with crw flow
	// report-up that stand (a bundle split request: no question the run waits for, CRW-1437), and the escalations that left a node to
	// the orchestrator (asking: those that are no request to send up)
	waiting, sendUp := map[string][]Question{}, map[string][]Question{}
	for _, q := range detail.Questions {
		switch {
		case q.Waits():
			waiting[q.Node] = append(waiting[q.Node], q)
		case q.SendsUp():
			sendUp[q.Node] = append(sendUp[q.Node], q)
		}
	}
	escalated, asking := map[string][]string{}, map[string][]string{}
	for _, e := range in.Events {
		if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation {
			if node := str(e.Data, "node"); node != "" {
				escalated[node] = append(escalated[node], str(e.Data, "reason"))
				if !reportUpAsks[str(e.Data, "ask")] {
					asking[node] = append(asking[node], str(e.Data, "reason"))
				}
			}
		}
	}
	mismatch := selectionMismatch(in.Events)
	heldReason := map[string]string{}
	order, rows := callRows(in.Events)
	for _, id := range order {
		cr := rows[id]
		if !isAgentCall(cr) {
			continue
		}
		node := ""
		for _, r := range cr {
			if r.Event == engine.EvPrepared {
				node = str(r.Data, "node")
			}
		}
		if status, reason := lastTerminal(cr); node != "" && status == engine.EvHeld {
			heldReason[node] = reason
		}
	}
	for _, issue := range in.Issues {
		item := DigestItem{Issue: issue, Title: titleOf[issue]}
		node := nodeOf[issue]
		n := nodes[node]
		switch {
		case mismatch != "":
			item.Outcome = OutcomeDecision
			item.Reason = "the milestone is not the selection (night_batch_selection): " + mismatch + "; nothing was released and the manager starts the batch again from a plan that is"
			item.Evidence = fmt.Sprintf("run %s: the escalation row night_batch_selection (crw flow show %s)", in.Run.RunID, in.Run.RunID)
		case node != "" && n.Integrated != "":
			item.Outcome = OutcomeIntegrated
			item.Evidence = fmt.Sprintf("run %s node %s: commit %s, acceptance %s, integrated %s", in.Run.RunID, node, shortList(n.Commits), cell(n.Accepted), short(n.Integrated))
		case node != "" && (len(waiting[node]) > 0 || (len(escalated[node]) > 0 && heldReason[node] == "")):
			item.Outcome = OutcomeDecision
			reasons := asking[node]
			if len(waiting[node]) > 0 {
				reasons = append([]string{waiting[node][0].Reason}, reasons...)
			}
			var stopped, asks []string
			for _, q := range sendUp[node] {
				stopped, asks = append(stopped, q.Reason), append(asks, q.Ask)
			}
			switch {
			case len(reasons) > 0:
				item.Reason = "the lane stopped and asks the orchestrator: " + strings.Join(dedupe(reasons), ", ")
			case len(asks) > 0:
				item.Reason = "the lane stopped on " + strings.Join(dedupe(stopped), ", ") + " with a request to send up with crw flow report-up (" + strings.Join(dedupe(asks), ", ") + "); the run waits for no answer"
			default:
				item.Reason = "the lane stopped and asks the orchestrator: " + strings.Join(dedupe(escalated[node]), ", ")
			}
			item.Evidence = fmt.Sprintf("run %s node %s: %d question(s) wait for an answer", in.Run.RunID, node, len(waiting[node]))
			if len(asks) > 0 {
				item.Evidence += fmt.Sprintf("; %d request(s) to send up with crw flow report-up (%s)", len(asks), strings.Join(dedupe(asks), ", "))
			}
			item.Evidence += fmt.Sprintf(" (crw flow show %s)", in.Run.RunID)
		case info.State == StateCancelled:
			item.Outcome = OutcomeCancelled
			item.Reason = "the run was cancelled before the issue was integrated"
			item.Evidence = fmt.Sprintf("run %s ended cancelled", in.Run.RunID)
		case node != "" && n.RecoveryHold:
			item.Outcome = OutcomeCarried
			item.Reason = "a call of the node has an outcome nobody knows (recovery_hold); crw flow recover settles it"
			item.Evidence = fmt.Sprintf("run %s node %s: recovery_hold", in.Run.RunID, node)
		case node != "" && heldReason[node] != "":
			item.Outcome = OutcomeCarried
			item.Reason = "the node's work is held: " + heldReason[node]
			item.Evidence = fmt.Sprintf("run %s node %s: a call ended held for %s; crw flow resume %s goes on", in.Run.RunID, node, heldReason[node], in.Run.RunID)
		case node == "":
			item.Outcome = OutcomeCarried
			item.Reason = "the run did not reach the plan that holds the issue"
		default:
			item.Outcome = OutcomeCarried
			if len(n.Threads) == 0 && len(n.Commits) == 0 {
				item.Reason = "the node was not started"
			} else {
				item.Reason = "the node did not finish in the run"
			}
			item.Evidence = fmt.Sprintf("run %s node %s: %s", in.Run.RunID, node, info.State)
		}
		d.Items = append(d.Items, item)
	}
	d.Stopped = stoppedWhy(info, in)
	// a run that was stopped before it could release anything (cancelled, failed, dead, or paused by the operator with no process,
	// CRW-1487) says so, not that the selection was refused
	if mismatch != "" && info.State != StateCancelled && info.State != StateFailed && info.State != StateDead && !(info.State == StatePaused && !info.Live) {
		d.Stopped = "night_batch_selection: the milestone is not the selection (" + mismatch + "); nothing was released"
	}
	d.Providers = append(d.Providers, DigestProvider{Name: "token budget", Observed: in.Spec.TokenBudget > 0, Detail: budgetDetail(info.Tokens, in.Spec.TokenBudget, info.TokenSplit)},
		DigestProvider{Name: "model providers", Observed: false, Detail: "the flow run does not observe provider quota or capacity"})
	return d
}

// selectionMismatch is the detail of the escalation a night batch raised when the milestone of its plan was not the selection
// (night-batch.js), "" when it raised none.
func selectionMismatch(events []engine.Event) string {
	for _, e := range events {
		if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation && str(e.Data, "reason") == "night_batch_selection" {
			if detail := str(e.Data, "detail"); detail != "" {
				return detail
			}
			return "no detail"
		}
	}
	return ""
}

// budgetDetail is the token budget line of the digest, with the split of what was spent (CRW-1327).
func budgetDetail(spent, budget int64, split TokenSplit) string {
	if budget <= 0 {
		return fmt.Sprintf("%d tokens spent (%s), no ceiling was set", spent, split)
	}
	return fmt.Sprintf("%d of %d tokens spent (%s)", spent, budget, split)
}

// stoppedWhy says why the run stopped, in words of the journal.
func stoppedWhy(info RunInfo, in DigestInput) string {
	switch {
	case info.State == StateCancelled:
		return "cancelled"
	case info.State == StateFailed:
		return "failed: " + cell(info.ErrorKind)
	case info.State == StateDead:
		return "the run process ended without finishing the run"
	case info.State == StatePaused && !info.Live:
		// the operator paused the run and its process ended paused (CRW-1487)
		if info.Paused == "" {
			return "paused"
		}
		return "paused by " + info.Paused
	case info.State == StateRunning || info.State == StatePaused:
		return "still running"
	case contains(info.HeldReasons, engine.ReasonBudgetExhausted):
		return fmt.Sprintf("token budget reached (%d of %d tokens)", info.Tokens, in.Spec.TokenBudget)
	case strings.HasPrefix(info.Paused, "deadline"):
		return "deadline: " + strings.TrimPrefix(info.Paused, "deadline: ")
	case info.Paused != "":
		return "paused by " + info.Paused
	case info.State == StateHeld:
		sort.Strings(info.HeldReasons)
		return "held: " + cell(strings.Join(info.HeldReasons, ", "))
	case info.State == StateBlocked:
		var failed []string
		for _, ie := range info.ItemErrors {
			failed = append(failed, ie.Item+": "+oneLine(ie.Error))
		}
		return "blocked: no node started (" + strings.Join(failed, "; ") + ")"
	}
	return "every selected issue was worked on and the run completed"
}

func shortList(list []string) string {
	if len(list) == 0 {
		return "-"
	}
	return short(list[len(list)-1])
}

func dedupe(list []string) []string {
	var out []string
	for _, s := range list {
		if s != "" && !contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// digest is `crw flow digest RUN [--out FILE]`: the digest of a night batch run, as crw-batch-digest/1.
func (c *command) digest(args []string) int {
	fs, cm := c.flags()
	out := fs.String("out", "", "write the digest to a file instead of the standard output")
	positional, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(positional) != 1 {
		return c.usage("exactly one run id is required")
	}
	r, err := c.reader(cm)
	if err != nil {
		return c.fail("%v", err)
	}
	defer r.Close()
	run := positional[0]
	rec, err := r.Record(c.ctx, run)
	if err != nil {
		return c.fail("%v", err)
	}
	spec, err := ReadSpec(r.State, run)
	if err != nil {
		return c.fail("%v: a digest is made for a run started by crw flow start night-batch", err)
	}
	var batch struct {
		Label  string   `json:"label"`
		Issues []string `json:"issues"`
	}
	if spec.Workflow != NightBatchWorkflow || json.Unmarshal(spec.Args, &batch) != nil || len(batch.Issues) == 0 {
		return c.fail("run %s is no night batch (workflow %s)", run, spec.Workflow)
	}
	events, _, err := journal.Load(c.ctx, r.St, run)
	if err != nil {
		return c.fail("%v", err)
	}
	res, _ := ReadResult(r.State, run)
	var result *Result
	if res.Run != "" {
		result = &res
	}
	live, _ := r.live(run)
	d := BuildDigest(DigestInput{Run: rec, Events: events, Spec: spec, Result: result, Label: batch.Label, Issues: batch.Issues, Now: c.now(), Live: live})
	b := JSON(d)
	if *out != "" {
		if err := os.WriteFile(*out, b, 0o600); err != nil {
			return c.fail("%v", err)
		}
		fmt.Fprintln(c.stdout, *out)
		return 0
	}
	_, _ = c.stdout.Write(b)
	return 0
}
