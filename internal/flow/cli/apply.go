package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/apply"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// applyDeps is apply's wiring over the relay store the operator named; a test replaces it.
var applyDeps = func(ctx context.Context, env Env, state, socket, repo string) (apply.Deps, func(), error) {
	st, statePath, err := env.Open(ctx, state, socket)
	if err != nil {
		return apply.Deps{}, nil, err
	}
	relay := apply.RelayCommand{Program: env.Program, State: statePath, Socket: socket}
	deps := apply.Deps{Store: st, Events: apply.StoreEvents{Store: st}, Commits: apply.Git, Receive: relay.Receive, Correct: relay.Correct, Repo: repo}
	// every time apply runs the plan summary is brought up to date and the orchestrator thread is told what is owed; the plan is the
	// child's run's, else the relationship's
	deps.Reconcile = func(ctx context.Context, ev apply.EventInfo, childRun string) error {
		plan := ev.Plan
		if childRun != "" {
			child, ok, err := report.ReadRun(ctx, st.DB, childRun)
			if err != nil {
				return err
			}
			if ok {
				plan = child.PlanID
			}
		}
		if plan == "" {
			return nil
		}
		run, document, found, err := report.SummaryTarget(ctx, st.DB, plan, ev.ParentTask)
		if err != nil || !found {
			return err
		}
		_, err = report.ReconcileSummary(ctx, report.SummaryEnv{Store: st, Sched: &dagsched.Scheduler{Store: st}, Rows: report.ForeignAppender{Store: st, Run: run}, Run: run,
			Plan: plan, Document: document, Actor: ev.ParentTask, Sender: env.sender()})
		return err
	}
	return deps, func() { _ = st.Close() }, nil
}

// applyCmd is `crw flow apply`. It prints the Result as JSON and exits 0 when the verdict is recorded, 1 when a check or the relay
// refused, 3 when the wait ended first (apply again in a later turn) and 2 for a command line or a failure of the store or the relay.
func applyCmd(ctx context.Context, args []string, env Env) int {
	var event, turn, run, base, override, manifest, repo, state, socket string
	var wait time.Duration
	positional, code, ok := flags("apply", args, env, func(fs *flag.FlagSet) {
		fs.StringVar(&event, "event", "", "the relay event of the child's receipt")
		fs.StringVar(&turn, "turn", "", "the orchestrator's own turn id")
		fs.StringVar(&run, "run", "", "the child's issue-lane run (default: the newest of the event's child task)")
		fs.StringVar(&base, "base", "", "the commit the child's work started from (default: the event's, then the run's first executor worktree)")
		fs.DurationVar(&wait, "wait", 0, "how long to wait for the reviewer verdict of the run (for example 10m)")
		fs.StringVar(&override, "override", "", "a reason that lets a reviewer of an author's model family go on; recorded")
		fs.StringVar(&manifest, "manifest-request", "", "the dag-correct --prepare manifest request a needs_changes verdict needs")
		fs.StringVar(&repo, "repo", "", "the git repository the commits are listed in (default: the working directory's)")
		fs.StringVar(&state, "state", "", "the relay state directory")
		fs.StringVar(&socket, "socket", "", "the App Server control socket")
	})
	if !ok {
		return code
	}
	if len(positional) != 0 || event == "" || turn == "" {
		return usageErr(env, "apply", "--event and --turn are required and nothing else is positional")
	}
	if wait < 0 {
		return usageErr(env, "apply", "--wait is not negative")
	}
	deps, closeDeps, err := applyDeps(ctx, env, state, socket, repo)
	if err != nil {
		return failure(env, "apply", err)
	}
	defer closeDeps()
	res, err := apply.Apply(ctx, deps, apply.Request{Event: event, Turn: turn, Run: run, Base: base, Wait: wait, Override: override, ManifestRequest: manifest})
	if err != nil {
		return failure(env, "apply", err)
	}
	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		return failure(env, "apply", err)
	}
	switch res.State {
	case apply.StateApplied:
		return ExitOK
	case apply.StateNextTurn:
		return ExitNextTurn
	}
	fmt.Fprintf(env.Stderr, "crw flow apply: refused: %s: %s\n", res.Reason, res.Detail)
	return ExitRefused
}
