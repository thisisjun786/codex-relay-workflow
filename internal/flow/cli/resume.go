package cli

import (
	"context"
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
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// resume is `crw flow resume RUN [SCRIPT] [--detach]`. The run is taken from its journal once its run lock is free: a run a process
// holds is refused (two processes would write one journal). With a SCRIPT it is the engine's command (the script and the
// arguments are given), under the run lock and held to the budget and the deadline of the run's spec (specLimits); without one
// the workflow and the arguments are those the run started with (its spec), and --detach lets the process go on in the background.
// The budget is the run's: a resume allows what the spec's budget leaves after the tokens the run's journal shows it spent, and
// one with nothing left is refused (runBudget). A foreground resume pauses on SIGUSR1 (`crw flow pause`) and drains on SIGUSR2
// (`crw flow answer`) as the run process does.
func (c *command) resume(args []string) int {
	fs, cm := c.flags()
	repo := fs.String("repo", "", "the git repository the run's worktrees are made from")
	argsJSON := fs.String("args", "", "the script's args, as JSON")
	argsFile := fs.String("args-file", "", "a file holding the script's args as JSON")
	pool := fs.Int("pool", 0, "agent calls in flight at once (default 6)")
	budget := fs.Int64("token-budget", 0, "the tokens this resume may spend, at most what the budget of the run's spec leaves (0: that remainder, or none without one)")
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
		if ctl == nil {
			ctl = &deadlineControl{Control: &engine.Control{}}
		}
		defer ctl.stop()
		// `crw flow pause` (SIGUSR1) and `crw flow answer` (SIGUSR2) signal the lock holder: this process pauses or drains the run as
		// the run process does (process.go). The signals are taken from before this process's holder record exists, so one sent the
		// moment it holds the run is not lost (CRW-1492).
		sigs := make(chan os.Signal, 4)
		signal.Notify(sigs, syscall.SIGUSR1, syscall.SIGUSR2)
		defer signal.Stop(sigs)
		defer pauseOnSignal(sigs, ctl.Control)()
		// a session run's process holds its process lock from before the run lock until after it, as the run process does
		var process *processLock
		if spec, err := ReadSpec(sel.Path, run); err == nil {
			process = c.processLock(sel.Path, spec)
			defer process.Release()
		}
		lock, err := takeLockPastWriters(sel.Path, run, "resume", c.now(), writerWait)
		if err != nil {
			return c.fail("%v", err)
		}
		defer lock.Release()
		if afterResumeLock != nil {
			afterResumeLock()
		}
		sweepProcessLocks(sel.Path, run, process.Token())
		// a cancelled run is over: it is refused under the run lock, before the engine appends anything (CRW-1495)
		if code, refused := c.refuseCancelled(cm, run); refused {
			return code
		}
		ctx := engine.WithControl(c.ctx, ctl.Control)
		hold := c.resumeHold(sel.Path, run)
		hold.Process = process.Token()
		ctx = engine.WithResumeHold(ctx, hold)
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
		// a run process that holds the run refuses the resume; a writer's hold (an audit, an answer, a cancel) is waited out, briefly
		// (CRW-1495 item 6)
		for deadline := time.Now().Add(writerWait); ; time.Sleep(20 * time.Millisecond) {
			live, _, holder := runHolder(sel.Path, run)
			if !live && holder == "" {
				break
			}
			if live || !time.Now().Before(deadline) {
				return c.fail("%v %s", ErrRunLive, run)
			}
		}
		// a legacy issue-child run and a run with nothing left are refused here, before a process is started for it (resumeRefused)
		r, err := c.reader(cm)
		if err != nil {
			return c.fail("%v", err)
		}
		err = resumeRefused(c.ctx, r.St, spec)
		_ = r.Close()
		if err != nil {
			return c.fail("%v", err)
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
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2)
	defer signal.Stop(sigs)
	return c.execute(spec, sel.Path, true, sigs)
}

// afterResumeLock, when set (tests only), runs the moment a SCRIPT foreground resume holds the run lock.
var afterResumeLock func()

// drainReason is the reason of the drain a SIGUSR2 asks (`crw flow answer` on a run a process holds, CRW-1492).
const drainReason = "crw flow answer: the run stops at its next boundary for an answer"

// pauseOnSignal pauses the run of ctl with the operator's reason for every SIGUSR1 sigs delivers and drains it by answer for a
// SIGUSR2, as the run process does, until the returned stop is called.
func pauseOnSignal(sigs <-chan os.Signal, ctl *engine.Control) (stop func()) {
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case sig := <-sigs:
				switch sig {
				case syscall.SIGUSR1:
					ctl.Pause("operator", "crw flow pause")
				case syscall.SIGUSR2:
					ctl.Drain("answer", drainReason)
				}
			}
		}
	}()
	return func() { close(done) }
}

// runBudget is the token ceiling a resume holds a run to (CRW-1278): the budget of its spec is the run's, so a resume allows what the
// budget leaves after the tokens the run spent (engine.RunSpent over its journal), a --token-budget (asked, 0 for none) narrows that
// remainder and never widens it, and a run with nothing left is refused with budget_exhausted: more budget is a new decision. The
// ceiling counts the tokens spent before (engine.Config.SpentBefore). A spec without a budget sets none, unless asked gives this
// resume one of its own.
func runBudget(spec Spec, spent, asked int64) (int64, error) {
	if spec.TokenBudget <= 0 {
		if asked > 0 {
			return spent + asked, nil
		}
		return 0, nil
	}
	left := spec.TokenBudget - spent
	if left <= 0 {
		return 0, fmt.Errorf("%s: run %s spent %d of the %d tokens of its spec%s; it is not resumed, and %s", engine.ReasonBudgetExhausted, spec.RunID, spent, spec.TokenBudget, decisionOf(spec), moreBudget(spec))
	}
	if asked <= 0 || asked > left {
		asked = left
	}
	return spent + asked, nil
}

// moreBudget is what the refusal of a run whose budget is spent says gives more (CRW-1392): a night batch's budget is its decision's,
// so more needs a new decision; a budget the operator gave at the start (crw flow start --token-budget) no command raises, so more
// tokens need a new run with a budget of its own. The refusal stays: tokens spent over a budget cannot be taken back.
func moreBudget(spec Spec) string {
	if spec.Decision != "" {
		return "more budget needs a new decision"
	}
	workflow := spec.Workflow
	if workflow == "" {
		workflow = "WORKFLOW"
	}
	return fmt.Sprintf("no command raises a run's budget: the budget was given at its start, so more tokens need a new run (crw flow start %s --token-budget N)", workflow)
}

// budgetLeft refuses the resume of a run whose spec's budget the run has spent (runBudget over the run's rows in events): `crw flow
// resume RUN --detach` and an answer's resume ask it (through resumeRefused) before they start the run process, so the refusal,
// budget_exhausted with the tokens spent and the decision, is the command's own answer and not a process that takes the run lock and ends
// at once. The run process judges the budget again under the run lock (execute). A spec without a budget has nothing to refuse.
func budgetLeft(spec Spec, events []engine.Event) error {
	if spec.TokenBudget <= 0 {
		return nil
	}
	_, err := runBudget(spec, engine.RunSpent(events, spec.RunID).Total, spec.TokenBudget)
	return err
}

// loadLeanJournal reads the rows of a run without their large payloads (journal.LoadLean); a test counts the reads through it.
var loadLeanJournal = journal.LoadLean

// legacyRunError is the refusal of a legacy issue-child run (legacyRun): a refusal that stands, which no retry lifts. `crw flow next`
// answers it as its own error and not as a resume that did not start.
type legacyRunError struct{ why string }

func (e *legacyRunError) Error() string { return e.why }

// legacyRun is why a run is a legacy issue-child run ("" when it is not): its rows (engine.LegacyIssueChild: a crw.releaseChild row, a
// child release whose spawn the old lane decided, a started row of the retired issue lane), or the workflow its spec or its record names
// (LegacyIssueLaneWorkflow), CRW-1415 (engine.LegacyRun). Its records stay read only: nothing is written to it and it is not resumed.
func legacyRun(events []engine.Event, runID, workflow string) string {
	return engine.LegacyRun(events, runID, workflow)
}

// legacyOf is why run is a legacy issue-child run ("" when it is not), read from st (its journal, without the large payloads) and from
// the workflow its spec in state or its record (recorded) names.
func legacyOf(ctx context.Context, st *store.Store, state, run, recorded string) (string, error) {
	events, _, err := loadLeanJournal(ctx, st, run)
	if err != nil {
		return "", fmt.Errorf("the journal of run %s cannot be read: %w", run, err)
	}
	workflow := recorded
	if spec, err := ReadSpec(state, run); err == nil && spec.Workflow == LegacyIssueLaneWorkflow {
		workflow = spec.Workflow
	}
	return legacyRun(events, run, workflow), nil
}

// legacyRefused refuses a command that would write to a legacy issue-child run, or make its process write (CRW-1415, PR #889 round 2):
// `crw flow cancel`, `pause`, `recover` and `dispatched` ask it before they write, signal or append, as `crw flow answer` asks legacyRun
// under the run lock and the resumes ask resumeRefused. The refusal is the command's answer, exit 1 with legacy_issue_child_run; a
// store that cannot be read is exit 2. It answers false for a run that is not legacy.
func (c *command) legacyRefused(cm *common, state, run string) (int, bool) {
	r, err := c.reader(cm)
	if err != nil {
		return c.fail("%v", err), true
	}
	defer r.Close()
	recorded := ""
	if rec, err := r.Record(c.ctx, run); err == nil {
		recorded = rec.Workflow
	}
	why, err := legacyOf(c.ctx, r.St, state, run, recorded)
	if err != nil {
		return c.fail("%v", err), true
	}
	if why == "" {
		return 0, false
	}
	fmt.Fprintf(c.stderr, "crw flow %s: refused (%s): %s\n", c.name, RefusedLegacyRun, why)
	return ExitRefused, true
}

// resumeRefused is why the run of spec is not resumed, asked before a process is started for it (`crw flow resume RUN --detach`, the
// resume of `crw flow answer` and of `crw flow next`): a legacy issue-child run (legacyRun, a *legacyRunError), a cancelled run
// (cancelledEnd), and a run whose spec's budget is spent (budgetLeft), all over one read of the run's journal. The run process judges
// them again under the run lock (engine.Run, execute), so the refusal is the command's own answer and not a process that takes the
// run lock and ends at once.
func resumeRefused(ctx context.Context, st *store.Store, spec Spec) error {
	events, _, err := loadLeanJournal(ctx, st, spec.RunID)
	if err != nil {
		return fmt.Errorf("the journal of run %s cannot be read: %w", spec.RunID, err)
	}
	if why := legacyRun(events, spec.RunID, spec.Workflow); why != "" {
		return &legacyRunError{why}
	}
	if err := cancelledEnd(events, spec.RunID); err != nil {
		return err
	}
	return budgetLeft(spec, events)
}

// cancelledEnd refuses the resume of a cancelled run (CRW-1495): its end after its last start is a finished row with error_kind
// cancelled (a `crw flow cancel` of a run no process held, or a run process that SIGTERM ended), so the run is over and is not taken
// up again. A run/paused end and a held finish stay resumable. Every resume asks it before anything is appended: resumeRefused before
// a process is started, the run process (execute) and the foreground `crw flow resume RUN SCRIPT` (refuseCancelled) under the run lock,
// since a cancel may end the run between that first look and the lock.
func cancelledEnd(events []engine.Event, run string) error {
	if Summarize(RunRecord{RunID: run}, events, false, 0).State == StateCancelled {
		return fmt.Errorf("run %s is cancelled: it is over and is not resumed (crw flow start runs the workflow again)", run)
	}
	return nil
}

// refuseCancelled reads the rows of run without their large payloads (the caller holds the run lock) and refuses a cancelled run
// (cancelledEnd). A store that cannot be read is refused as well: nothing is resumed past a journal that was not read.
func (c *command) refuseCancelled(cm *common, run string) (int, bool) {
	r, err := c.reader(cm)
	if err != nil {
		return c.fail("%v", err), true
	}
	events, _, err := loadLeanJournal(c.ctx, r.St, run)
	_ = r.Close()
	if err != nil {
		return c.fail("the journal of run %s cannot be read: %v", run, err), true
	}
	if err := cancelledEnd(events, run); err != nil {
		return c.fail("%v", err), true
	}
	return 0, false
}

// resumeHold holds a `crw flow resume RUN SCRIPT` the way the run process holds its run (CRW-1348): a spec that names another
// backend than the one the run's started row records is a mixed resume, refused; a session run keeps at most agents.max_threads - 1
// calls in flight whatever --pool says, and its owner thread holds the run's orchestrator line while the resume runs, written right
// after the resume's started row with the table it pinned (the open line of a run whose process is gone is closed first, CRW-1372),
// and closed when the run ends. A run with a spec is held to the budget the spec leaves (runBudget).
func (c *command) resumeHold(state, run string) engine.ResumeHold {
	hold := engine.ResumeHold{Session: func(owner string, pool int) (int, func(*engine.Policy), func()) {
		return sessionPool(pool, readCodexAgents(c.getenv).MaxThreads), c.bindFromStarted(state, run, owner), func() {
			if _, err := closeOwner(run, owner); err != nil {
				fmt.Fprintf(c.stderr, "crw flow %s: warning: the orchestrator line of thread %s could not be closed: %v\n", c.name, owner, err)
			}
		}
	}}
	if spec, err := ReadSpec(state, run); err == nil {
		hold.Backend = spec.Backend
		hold.Budget = func(spent, asked int64) (int64, error) { return runBudget(spec, spent, asked) }
	}
	return hold
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
// decision's): the engine's command line, with the spec's budget when no --token-budget is given; a --token-budget of 0 is the
// remainder of the spec's budget as the flag's help says (the same ceiling as none), one below 0 or above the spec's is refused, and a
// lower one narrows the remainder (what the run already spent counts against the spec's budget: runBudget, applied by the engine's
// command through resumeHold); and a Control that pauses the run at the spec's deadline (at once when it passed). A run that no
// `crw flow start` made has no spec and nothing to hold it to.
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
		case budget < 0 || budget > spec.TokenBudget:
			return nil, nil, fmt.Errorf("the token budget %d is not within the %d tokens of run %s's spec%s; give a budget from 1 to %d, or 0 or none for what the spec's budget leaves", budget, spec.TokenBudget, run, decisionOf(spec), spec.TokenBudget)
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
// of the run that holds the call. A run a process holds is refused, and so is a legacy issue-child run (legacyRefused).
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
			// a legacy issue-child run is read only (CRW-1415): no recovery row is appended to it (engine.Recover refuses it as well)
			if code, refused := c.legacyRefused(cm, sel.Path, run); refused {
				return code
			}
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
