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

func (env Env) sender() report.Sender {
	if env.Sender != nil {
		return env.Sender
	}
	return report.ManageSender{Program: env.Program}
}

func (env Env) resolver(st *store.Store) report.Resolver {
	if env.Resolver != nil {
		return env.Resolver
	}
	return report.StoreResolver{Store: st}
}

func printJSON(env Env, v any) error {
	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// reportUpCmd is `crw flow report-up`: the orchestrator lifts a project-level blocker or decision request to the initiative lead, and a
// decision only Jun can make to the store seat (the manager's pump tells Jun). The evidence file is the body of the report. It prints
// the result as JSON and exits 0 when the report is on record (delivered, queued or standing), 1 when the delivery was refused.
func reportUpCmd(ctx context.Context, args []string, env Env) int {
	var kind, run, evidence, subject, milestone, state, socket string
	positional, code, ok := flags("report-up", args, env, func(fs *flag.FlagSet) {
		fs.StringVar(&kind, "kind", "", "project_blocked, project_decision_request, jun_decision_request (milestone_report is the run's)")
		fs.StringVar(&run, "run", "", "the run the report is about")
		fs.StringVar(&evidence, "evidence", "", "the file that holds the body of the report")
		fs.StringVar(&subject, "subject", "", "one line that says what the report is about")
		fs.StringVar(&milestone, "milestone", "", "the milestone (a milestone_report)")
		fs.StringVar(&state, "state", "", "the relay state directory")
		fs.StringVar(&socket, "socket", "", "the App Server control socket")
	})
	if !ok {
		return code
	}
	if len(positional) != 0 || kind == "" || run == "" || evidence == "" {
		return usageErr(env, "report-up", "--kind, --run and --evidence are required")
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
	w := &report.Writer{Store: st, Rows: report.ForeignAppender{Store: st, Run: run}, Resolver: env.resolver(st), Sender: env.sender(), ArtifactDir: filepath.Join(statePath, "flow", run, "reports")}
	res, err := w.Up(ctx, report.UpRequest{Kind: kind, Run: run, Milestone: milestone, Subject: subject, Body: body})
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

// reportsCmd is `crw flow reports`: the reports of the store, those of an initiative, the unread ones.
func reportsCmd(ctx context.Context, args []string, env Env) int {
	var initiative, state, socket string
	var unread, asJSON bool
	positional, code, ok := flags("reports", args, env, func(fs *flag.FlagSet) {
		fs.StringVar(&initiative, "initiative", "", "only the reports for this initiative")
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
	views, err := report.Reports(ctx, st.DB, initiative, unread)
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
func reportAckCmd(ctx context.Context, args []string, env Env) int {
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
func reportDeliveredCmd(ctx context.Context, args []string, env Env) int {
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
