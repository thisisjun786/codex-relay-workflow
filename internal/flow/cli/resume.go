package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
)

// resume is `crw flow resume RUN [SCRIPT] [--detach]`. The run is taken from its journal once its run lock is free: a run a process
// holds is refused (two processes would write one journal). With a SCRIPT it is the engine's command (the script and the
// arguments are given), under the run lock and held to the budget and the deadline of the run's spec (specLimits); without one
// the workflow and the arguments are those the run started with (its spec), and --detach lets the process go on in the background.
func (c *command) resume(args []string) int {
	fs, cm := c.flags()
	repo := fs.String("repo", "", "the git repository the run's worktrees are made from")
	argsJSON := fs.String("args", "", "the script's args, as JSON")
	argsFile := fs.String("args-file", "", "a file holding the script's args as JSON")
	pool := fs.Int("pool", 0, "agent calls in flight at once (default 6)")
	budget := fs.Int64("token-budget", 0, "the run's token ceiling (0: none)")
	detach := fs.Bool("detach", false, "go on in a process of its own and return")
	positional, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	// anything but a run id, or a run id and a script, is the engine's command line to refuse
	if len(positional) < 1 || len(positional) > 2 || !validRunID(positional[0]) {
		return engine.RunCLI(c.ctx, withoutDetach(args), c.stdout, c.stderr)
	}
	run := positional[0]
	sel, err := ResolveState(cm.state, cm.socket)
	if err != nil {
		return c.fail("%v", err)
	}
	if len(positional) == 2 {
		if *detach {
			return c.usage("--detach resumes with the run's own spec; a SCRIPT is given for the foreground command")
		}
		eargs, ctl, err := c.specLimits(fs, sel.Path, run, *budget, withoutDetach(args))
		if err != nil {
			return c.fail("%v", err)
		}
		lock, err := TakeLock(sel.Path, run, "resume", c.now())
		if err != nil {
			return c.fail("%v", err)
		}
		defer lock.Release()
		ctx := c.ctx
		if ctl != nil {
			ctx = engine.WithControl(ctx, ctl.Control)
			defer ctl.stop()
		}
		return engine.RunCLI(ctx, eargs, c.stdout, c.stderr)
	}
	if *argsJSON != "" || *argsFile != "" || *pool != 0 || *budget != 0 || *repo != "" {
		return c.usage("a resume without a SCRIPT takes the run's own arguments, pool and budget from its spec")
	}
	spec, err := ReadSpec(sel.Path, run)
	if err != nil {
		return c.fail("%v", err)
	}
	if *detach {
		if LockHeld(sel.Path, run) {
			return c.fail("%v %s", ErrRunLive, run)
		}
		dir, _ := os.Getwd()
		spawned := c.now()
		pid, err := c.Spawn(c.ctx, SpawnRequest{Spec: spec, State: sel.Path, Resume: true, Dir: dir})
		if err != nil {
			return c.fail("the run process cannot be started: %v", err)
		}
		if c.Ready != nil {
			if err := c.Ready(c.ctx, sel.Path, run, pid, spawned); err != nil {
				return c.fail("run %s: %v", run, err)
			}
		}
		fmt.Fprintf(c.stdout, "run %s resumed (process %d)\n", run, pid)
		return 0
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)
	return c.execute(spec, sel.Path, true, sigs)
}

// deadlineControl is the Control a resume binds its run to, and the timer that pauses the run at the spec's deadline.
type deadlineControl struct {
	*engine.Control
	timer *time.Timer
}

func (d *deadlineControl) stop() {
	if d.timer != nil {
		d.timer.Stop()
	}
}

// specLimits holds a `crw flow resume RUN SCRIPT` to the budget and the deadline of the run's spec (a night batch's are the
// decision's): the engine's command line, with the spec's budget when no --token-budget is given; a --token-budget of 0 (no
// ceiling), below 0 or above the spec's is refused, a lower one narrows it; and a Control that pauses the run at the spec's
// deadline (at once when it passed). A run that no `crw flow start` made has no spec and nothing to hold it to.
func (c *command) specLimits(fs *flag.FlagSet, state, run string, budget int64, eargs []string) ([]string, *deadlineControl, error) {
	spec, err := ReadSpec(state, run)
	if errors.Is(err, ErrNoSpec) {
		return eargs, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if spec.TokenBudget > 0 {
		given := false
		fs.Visit(func(f *flag.Flag) { given = given || f.Name == "token-budget" })
		switch {
		case !given:
			eargs = append(eargs, "--token-budget", strconv.FormatInt(spec.TokenBudget, 10))
		case budget <= 0 || budget > spec.TokenBudget:
			return nil, nil, fmt.Errorf("the token budget %d is not within the %d tokens of run %s's spec%s; give a budget from 1 to %d or none", budget, spec.TokenBudget, run, decisionOf(spec), spec.TokenBudget)
		}
	}
	if spec.Deadline == "" {
		return eargs, nil, nil
	}
	at, err := time.Parse(time.RFC3339, spec.Deadline)
	if err != nil {
		return nil, nil, fmt.Errorf("the deadline %q of run %s's spec is not an RFC 3339 time", spec.Deadline, run)
	}
	d := &deadlineControl{Control: &engine.Control{}}
	pause := func() { d.Pause("deadline", "the run's deadline "+spec.Deadline+" passed") }
	if wait := at.Sub(c.now()); wait > 0 {
		d.timer = time.AfterFunc(wait, pause)
	} else {
		pause()
	}
	return eargs, d, nil
}

func decisionOf(spec Spec) string {
	if spec.Decision == "" {
		return ""
	}
	return " (the decision " + spec.Decision + ")"
}

// withoutDetach is the engine's resume command line: the arguments of this command, with --detach (which the engine's command does
// not have) left out, behind the engine's own command word.
func withoutDetach(args []string) []string {
	out := []string{"resume"}
	for _, a := range args {
		if a != "--detach" && a != "-detach" {
			out = append(out, a)
		}
	}
	return out
}

// recoverCall is `crw flow recover [--run RUN] CALL (--adopt THREAD|TURN | --absent)`: the engine's command, under the run lock
// of the run that holds the call. A run a process holds is refused.
func (c *command) recoverCall(args []string) int {
	fs, cm := c.flags()
	runID := fs.String("run", "", "the run the call belongs to (default: the run that holds the call)")
	_ = fs.String("adopt", "", "")
	_ = fs.Bool("absent", false, "")
	positional, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	run := *runID
	if run == "" && len(positional) == 1 {
		run = c.runOfCall(cm, positional[0])
	}
	if run != "" && validRunID(run) {
		if sel, err := ResolveState(cm.state, cm.socket); err == nil {
			lock, err := TakeLock(sel.Path, run, "recover", c.now())
			if err != nil {
				return c.fail("%v: recover a call of a run no process holds", err)
			}
			defer lock.Release()
		}
	}
	return engine.RunCLI(c.ctx, append([]string{"recover"}, args...), c.stdout, c.stderr)
}

func (c *command) runOfCall(cm *common, call string) string {
	r, err := c.reader(cm)
	if err != nil {
		return ""
	}
	defer r.Close()
	run, _ := journal.RunOf(c.ctx, r.St, call)
	return run
}
