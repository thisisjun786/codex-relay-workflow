package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// sender is the sender of the notices: the relay the command works in (the state directory it resolved and the socket the operator named)
// goes with every supervisor notice, so `crw manage send-parent` reads the binding there and not in the configuration's relay.
//
// A report goes through the supervisor channel of the store (RelaySender, CRW-1380); send-parent is the fallback for what the channel
// has no kind for (the plan summary notice).
func (env Environment) sender(st *store.Store, statePath, socket string) report.Sender {
	if env.Sender != nil {
		return env.Sender
	}
	return report.RelaySender{Store: st, Fallback: report.ManageSender{Program: env.Program, State: statePath, Socket: socket}}
}

func (env Environment) resolver(st *store.Store) report.Resolver {
	if env.Resolver != nil {
		return env.Resolver
	}
	return report.StoreResolver{Store: st}
}

func printJSON(env Environment, v any) error {
	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// reportUpCmd is `crw flow report-up`: the orchestrator lifts a project-level blocker or decision request to the initiative lead, and a
// decision only Jun can make to the store seat (the manager's pump tells Jun). The evidence file is the body of the report. It prints
// the result as JSON and exits 0 when the report is on record (delivered, queued or standing), 1 when the delivery was refused.
//
// A project_decision_request that lifts a question the run asked names it with --question (CRW-1574): the run must wait on it (the
// current question of its node and ask, no decision answered it, no request to send up); else the command refuses it with exit 1 and
// writes nothing. The report row then carries the question, its revision and its node, so the parent's decision record whose source is
// the report binds that question.
func reportUpCmd(ctx context.Context, args []string, env Environment) int {
	var kind, run, evidence, subject, milestone, question, state, socket string
	positional, code, ok := flags("report-up", args, env, func(fs *flag.FlagSet) {
		fs.StringVar(&kind, "kind", "", "project_blocked, project_decision_request, jun_decision_request (milestone_report is the run's)")
		fs.StringVar(&run, "run", "", "the run the report is about")
		fs.StringVar(&evidence, "evidence", "", "the file that holds the body of the report")
		fs.StringVar(&subject, "subject", "", "one line that says what the report is about")
		fs.StringVar(&milestone, "milestone", "", "the milestone (a milestone_report)")
		fs.StringVar(&question, "question", "", "the escalation id of the run's question a project_decision_request lifts")
		fs.StringVar(&state, "state", "", "the relay state directory")
		fs.StringVar(&socket, "socket", "", "the App Server control socket")
	})
	if !ok {
		return code
	}
	if len(positional) != 0 || kind == "" || run == "" || evidence == "" {
		return usageErr(env, "report-up", "--kind, --run and --evidence are required")
	}
	if question != "" && kind != report.KindProjectDecisionRequest {
		return usageErr(env, "report-up", "--question names the run's question a %s lifts, not a %s", report.KindProjectDecisionRequest, kind)
	}
	body, err := os.ReadFile(evidence)
	if err != nil {
		return failure(env, "report-up", err)
	}
	st, statePath, err := env.Open(ctx, state, socket)
	if err != nil {
		return failure(env, "report-up", err)
	}
	defer st.Close()
	// a legacy `child` session run is read only (CRW-1415): no report row is written to it
	if why, err := legacyOf(ctx, st, statePath, run, ""); err != nil {
		return failure(env, "report-up", err)
	} else if why != "" {
		fmt.Fprintf(env.Stderr, "crw flow report-up: refused (%s): %s\n", RefusedLegacyRun, why)
		return ExitRefused
	}
	req := report.UpRequest{Kind: kind, Run: run, Milestone: milestone, Subject: subject, Body: body}
	if question != "" {
		q, why, err := waitingQuestion(ctx, st, run, question)
		if err != nil {
			return failure(env, "report-up", err)
		}
		if why != "" {
			fmt.Fprintf(env.Stderr, "crw flow report-up: refused: %s\n", why)
			return ExitRefused
		}
		req.Question, req.Revision, req.Node = q.ID, q.Revision, q.Node
	}
	w := &report.Writer{Store: st, Rows: report.ForeignAppender{Store: st, Run: run}, Resolver: env.resolver(st), Sender: env.sender(st, statePath, socket), ArtifactDir: filepath.Join(statePath, "flow", run, "reports")}
	res, err := w.Up(ctx, req)
	if err != nil {
		return failure(env, "report-up", err)
	}
	if err := printJSON(env, res); err != nil {
		return failure(env, "report-up", err)
	}
	if res.Delivery == report.ResultRefused {
		return ExitRefused
	}
	return ExitOK
}

// waitingQuestion is the question id of run when the run waits on it (Question.Waits), else why it does not ("" with the question).
func waitingQuestion(ctx context.Context, st *store.Store, run, id string) (Question, string, error) {
	events, _, err := loadLeanJournal(ctx, st, run)
	if err != nil {
		return Question{}, "", fmt.Errorf("the journal of run %s cannot be read: %w", run, err)
	}
	for _, q := range Questions(events) {
		if q.ID != id {
			continue
		}
		switch {
		case q.ReportUp:
			return q, fmt.Sprintf("question %s of run %s is a request to send up (%s), not a question the run waits on", id, run, q.Ask), nil
		case !q.Current:
			return q, fmt.Sprintf("question %s of run %s (revision %d) was replaced by a later ask of node %q (%s)", id, run, q.Revision, q.Node, q.Ask), nil
		case q.Applied != "":
			return q, fmt.Sprintf("question %s of run %s is answered (%s)", id, run, q.Applied), nil
		}
		return q, "", nil
	}
	return Question{}, fmt.Sprintf("run %s has no question %s", run, id), nil
}

// reportsCmd is `crw flow reports`: the reports of the store, those of an initiative or of a project, the unread ones.
func reportsCmd(ctx context.Context, args []string, env Environment) int {
	var initiative, project, state, socket string
	var unread, asJSON bool
	positional, code, ok := flags("reports", args, env, func(fs *flag.FlagSet) {
		fs.StringVar(&initiative, "initiative", "", "only the reports for this initiative")
		fs.StringVar(&project, "project", "", "only the reports of this project (a management session reads its own projects')")
		fs.BoolVar(&unread, "unread", false, "only the reports nobody acknowledged")
		fs.BoolVar(&asJSON, "json", false, "print JSON")
		fs.StringVar(&state, "state", "", "the relay state directory")
		fs.StringVar(&socket, "socket", "", "the App Server control socket")
	})
	if !ok {
		return code
	}
	if len(positional) != 0 {
		return usageErr(env, "reports", "takes no positional argument")
	}
	st, _, err := env.Open(ctx, state, socket)
	if err != nil {
		return failure(env, "reports", err)
	}
	defer st.Close()
	views, err := report.ReportsMatching(ctx, st.DB, report.ReportFilter{Initiative: initiative, Project: project, Unread: unread})
	if err != nil {
		return failure(env, "reports", err)
	}
	if asJSON {
		if err := printJSON(env, views); err != nil {
			return failure(env, "reports", err)
		}
		return ExitOK
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "REPORT\tKIND\tRUN\tINITIATIVE\tDELIVERY\tREAD\tAT")
	for _, v := range views {
		delivery, read := v.LastDelivery, "unread"
		if delivery == "" {
			delivery = "none (" + v.Recipient.State + ")"
		}
		if v.Read {
			read = "read in " + v.ReadTurn
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", v.ID, v.Kind, v.Run, v.Recipient.InitiativeKey, delivery, read, v.RecordedAt)
	}
	if err := tw.Flush(); err != nil {
		return failure(env, "reports", err)
	}
	return ExitOK
}

// reportAckCmd is `crw flow report-ack REPORT --turn T`: the lead read the report in the turn.
func reportAckCmd(ctx context.Context, args []string, env Environment) int {
	var turn, state, socket string
	positional, code, ok := flags("report-ack", args, env, func(fs *flag.FlagSet) {
		fs.StringVar(&turn, "turn", "", "the lead's own turn id")
		fs.StringVar(&state, "state", "", "the relay state directory")
		fs.StringVar(&socket, "socket", "", "the App Server control socket")
	})
	if !ok {
		return code
	}
	if len(positional) != 1 || turn == "" {
		return usageErr(env, "report-ack", "a report id and --turn are required")
	}
	st, _, err := env.Open(ctx, state, socket)
	if err != nil {
		return failure(env, "report-ack", err)
	}
	defer st.Close()
	written, err := report.Ack(ctx, st.DB, func(run string) report.Appender { return report.ForeignAppender{Store: st, Run: run} }, positional[0], turn)
	if err != nil {
		return failure(env, "report-ack", err)
	}
	if err := printJSON(env, map[string]any{"report_id": positional[0], "turn": turn, "written": written}); err != nil {
		return failure(env, "report-ack", err)
	}
	return ExitOK
}

// reportDeliveredCmd is `crw flow report-delivered`: the delivery row a delivery the pump made came to. The pump reads the relay store and
// writes none of it; this command is the one writer of the rows.
func reportDeliveredCmd(ctx context.Context, args []string, env Environment) int {
	var id, result, request, observation, state, socket string
	positional, code, ok := flags("report-delivered", args, env, func(fs *flag.FlagSet) {
		fs.StringVar(&id, "report", "", "the report id")
		fs.StringVar(&result, "result", "", "accepted, queued, refused or unknown")
		fs.StringVar(&request, "request-id", "", "the request id of the delivery")
		fs.StringVar(&observation, "observation", "", "the state of the thread, for a queued delivery")
		fs.StringVar(&state, "state", "", "the relay state directory")
		fs.StringVar(&socket, "socket", "", "the App Server control socket")
	})
	if !ok {
		return code
	}
	if len(positional) != 0 || id == "" || result == "" {
		return usageErr(env, "report-delivered", "--report and --result are required")
	}
	st, _, err := env.Open(ctx, state, socket)
	if err != nil {
		return failure(env, "report-delivered", err)
	}
	defer st.Close()
	written, err := report.Delivered(ctx, st.DB, func(run string) report.Appender { return report.ForeignAppender{Store: st, Run: run} }, id, strings.TrimSpace(result), request, observation)
	if err != nil {
		return failure(env, "report-delivered", err)
	}
	if err := printJSON(env, map[string]any{"report_id": id, "result": result, "request_id": request, "written": written}); err != nil {
		return failure(env, "report-delivered", err)
	}
	return ExitOK
}
