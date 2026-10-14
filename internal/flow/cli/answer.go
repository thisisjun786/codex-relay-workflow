package cli

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// AnswerReply is the JSON answer of `crw flow answer`.
type AnswerReply struct {
	OK       bool           `json:"ok"`
	Run      string         `json:"run"`
	Applied  bool           `json:"applied"`
	Replayed bool           `json:"replayed,omitempty"`
	Seq      int            `json:"seq,omitempty"`
	Row      map[string]any `json:"row,omitempty"`
	Refusal  *Refusal       `json:"refusal,omitempty"`
	Resumed  int            `json:"resumed_pid,omitempty"`
}

// answer is `crw flow answer`: it compares the manager's answer record with the run's journal, appends the one `decision_applied`
// row that resumes the wait, and resumes that wait. The record is the only input: what the caller names (the run, the node, the
// question and its revision) is a claim that has to be the record's and the journal's too.
func (c *command) answer(args []string) int {
	fs, cm := c.flags()
	decision := fs.String("decision", "", "the answer record (crw manage decisions) to apply")
	runID := fs.String("run", "", "the run the answer is for")
	node := fs.String("node", "", "the node the question is about (empty for a question of the milestone)")
	question := fs.String("question", "", "the id of the question the record answers")
	revision := fs.Int("revision", 0, "the revision of the question the record answers")
	thread := fs.String("thread", "", "the orchestrator thread that applies the answer (default: CODEX_THREAD_ID)")
	noResume := fs.Bool("no-resume", false, "append the row and do not resume the run")
	asJSON := fs.Bool("json", false, "print the answer as JSON")
	positional, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	switch {
	case len(positional) != 0:
		return c.usage("takes no positional argument")
	case *decision == "" || *runID == "" || *question == "" || *revision < 1:
		return c.usage("--decision, --run, --question and --revision (1 or more) are required")
	case !validRunID(*runID):
		return c.usage("%q is not a run id", *runID)
	}
	by := *thread
	if by == "" {
		by = c.getenv("CODEX_THREAD_ID")
	}
	if by == "" {
		by = "operator"
	}
	write := c.Writer
	if write == nil {
		write = OpenWriter
	}
	st, state, err := write(c.ctx, cm.state, cm.socket)
	if err != nil {
		return c.fail("%v", err)
	}
	defer st.Close()
	reply, code := c.applyAnswer(st, state, Claim{Decision: *decision, Run: *runID, Node: *node, Question: *question, Revision: *revision, By: by})
	if reply.Refusal == nil && code == 0 && reply.Applied && !*noResume && !reply.Replayed {
		pid, err := c.resumeDetached(st, state, *runID)
		if err != nil {
			fmt.Fprintf(c.stderr, "crw flow answer: the answer is applied and the run is not resumed: %v\n", err)
			code = 1
		}
		reply.Resumed = pid
	}
	if *asJSON {
		_, _ = c.stdout.Write(JSON(reply))
		return code
	}
	switch {
	case reply.Refusal != nil:
		fmt.Fprintf(c.stderr, "crw flow answer: refused (%s): %s\n", reply.Refusal.Code, reply.Refusal.Detail)
	case reply.Replayed:
		fmt.Fprintf(c.stdout, "decision %s was applied to run %s at row %d already; nothing is appended\n", *decision, *runID, reply.Seq)
	case reply.Applied:
		fmt.Fprintf(c.stdout, "decision %s applied to run %s (question %s, node %s)", *decision, *runID, *question, cell(*node))
		if reply.Resumed > 0 {
			fmt.Fprintf(c.stdout, "; the run is resumed (process %d)", reply.Resumed)
		}
		fmt.Fprintln(c.stdout)
	}
	return code
}

// applyAnswer judges the claim against the record and the journal and, when it holds, appends the row under the run lock. It
// returns the exit status of the command.
func (c *command) applyAnswer(st *store.Store, state string, claim Claim) (AnswerReply, int) {
	reply := AnswerReply{Run: claim.Run}
	rec, err := st.Get(c.ctx, claim.Decision)
	if err != nil {
		fmt.Fprintf(c.stderr, "crw flow answer: error: %v\n", err)
		return reply, 2
	}
	r := &Reader{St: st, State: state}
	run, err := r.Record(c.ctx, claim.Run)
	if err != nil {
		fmt.Fprintf(c.stderr, "crw flow answer: error: %v\n", err)
		return reply, 2
	}
	lock, err := TakeLock(state, claim.Run, "answer", c.now())
	if err != nil {
		reply.Refusal = &Refusal{Code: "run_live", Detail: err.Error() + "; an answer is applied to the journal of a run no process holds (crw flow pause " + claim.Run + " first)"}
		return reply, 1
	}
	defer lock.Release()
	j, err := journal.Open(c.ctx, st, claim.Run, journal.Identity{}, journal.Options{Immediate: true})
	if err != nil {
		fmt.Fprintf(c.stderr, "crw flow answer: error: %v\n", err)
		return reply, 2
	}
	app, refusal := Judge(rec, j.Events(), run, claim)
	if refusal != nil {
		reply.Refusal = refusal
		return reply, 1
	}
	reply.Row, reply.Replayed, reply.Seq = app.Data, app.Replayed, app.Seq
	if app.Replayed {
		reply.OK = true
		return reply, 0
	}
	ev := j.Append(engine.Event{RunID: claim.Run, Kind: engine.KindFlow, Event: ops.EvDecisionApplied, Data: app.Data})
	if err := j.Close(); err != nil || j.Err() != nil {
		fmt.Fprintf(c.stderr, "crw flow answer: error: the row could not be stored: %v\n", errors.Join(err, j.Err()))
		return reply, 2
	}
	reply.OK, reply.Applied, reply.Seq = true, true, ev.Seq
	return reply, 0
}

// resumeDetached starts the process of a run again from its journal; a run with nothing left of its spec's budget is refused
// before (budgetLeft, over the journal in st).
func (c *command) resumeDetached(st *store.Store, state, run string) (int, error) {
	spec, err := ReadSpec(state, run)
	if err != nil {
		return 0, err
	}
	if err := budgetLeft(c.ctx, st, spec); err != nil {
		return 0, err
	}
	dir, _ := os.Getwd()
	spawned := c.now()
	pid, err := c.Spawn(c.ctx, SpawnRequest{Spec: spec, State: state, Resume: true, Dir: dir})
	if err != nil {
		return 0, err
	}
	if c.Ready != nil {
		if err := c.Ready(c.ctx, state, run, pid, spawned); err != nil {
			return pid, err
		}
	}
	return pid, nil
}

// control is `crw flow pause RUN` and `crw flow cancel RUN`. The process that holds the run lock is signalled: SIGUSR1 pauses it
// (the calls in flight end held, and `crw flow resume` goes on with them), SIGTERM ends it (the calls in flight are cancelled and
// the run finishes cancelled). A run no process holds is cancelled by a finished row written under the lock; a pause of it is
// refused, since nothing runs.
func (c *command) control(args []string, verb string) int {
	fs, cm := c.flags()
	wait := fs.Int("wait", 20, "seconds to wait for the process to end after a cancel (0: do not wait)")
	positional, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(positional) != 1 {
		return c.usage("exactly one run id is required")
	}
	run := positional[0]
	r, err := c.reader(cm)
	if err != nil {
		return c.fail("%v", err)
	}
	rec, err := r.Record(c.ctx, run)
	if err != nil {
		_ = r.Close()
		return c.fail("%v", err)
	}
	state := r.State
	info, err := r.Info(c.ctx, rec)
	_ = r.Close()
	if err != nil {
		return c.fail("%v", err)
	}
	if Over(info.State) {
		if verb == "cancel" && info.State == StateCancelled {
			// a cancel whose process ended before the executions were given up (or whose settlement failed) is finished here
			if code := c.settleEnded(state, cm, rec); code != 0 {
				return code
			}
		}
		fmt.Fprintf(c.stderr, "crw flow %s: run %s is %s: nothing to %s\n", verb, run, info.State, verb)
		return 1
	}
	if !info.Live {
		if verb == "pause" {
			fmt.Fprintf(c.stderr, "crw flow pause: no process holds run %s (it is %s): nothing runs to pause; crw flow resume %s goes on\n", run, info.State, run)
			return 1
		}
		return c.cancelDead(state, cm, rec)
	}
	if info.PID <= 1 {
		// a signal to pid 0 goes to the whole process group of this command, and to pid -n to a group: a holder that cannot be named is not signalled
		fmt.Fprintf(c.stderr, "crw flow %s: a process holds run %s but its lock names no process (holder unknown): nothing was signalled; look at %s and end the process by hand\n", verb, run, lockPath(state, run))
		return 1
	}
	sig, what := syscall.SIGUSR1, "pause"
	if verb == "cancel" {
		sig, what = syscall.SIGTERM, "cancel"
	}
	signal := c.Signal
	if signal == nil {
		signal = killProcess
	}
	if err := signal(info.PID, sig); err != nil {
		return c.fail("process %d of run %s cannot be signalled: %v", info.PID, run, err)
	}
	fmt.Fprintf(c.stdout, "%s requested of run %s (process %d)\n", what, run, info.PID)
	if verb == "cancel" && *wait > 0 {
		deadline := time.Now().Add(time.Duration(*wait) * time.Second)
		for LockHeld(state, run) {
			if time.Now().After(deadline) {
				fmt.Fprintf(c.stderr, "crw flow cancel: process %d still holds run %s after %ds\n", info.PID, run, *wait)
				return 1
			}
			time.Sleep(50 * time.Millisecond)
		}
		fmt.Fprintf(c.stdout, "run %s ended\n", run)
		return c.settleEnded(state, cm, rec)
	}
	return 0
}

// settleEnded gives up the executions of a cancelled run whose process has ended: under the run lock, so no process works on the
// run, from the rows of its journal. A second call finds nothing open.
func (c *command) settleEnded(state string, cm *common, rec RunRecord) int {
	write := c.Writer
	if write == nil {
		write = OpenWriter
	}
	st, wstate, err := write(c.ctx, cm.state, cm.socket)
	if err != nil {
		return c.fail("%v", err)
	}
	defer st.Close()
	if wstate != "" {
		state = wstate
	}
	lock, err := TakeLock(state, rec.RunID, "cancel", c.now())
	if err != nil {
		return c.fail("the executions of run %s cannot be given up yet: %v", rec.RunID, err)
	}
	defer lock.Release()
	events, _, err := journal.Load(c.ctx, st, rec.RunID)
	if err != nil {
		return c.fail("%v", err)
	}
	settled, err := c.settle(st, rec.RunID, events)
	if err != nil {
		return c.fail("%v", err)
	}
	if settled.Closed > 0 || settled.SlotsReturned > 0 {
		fmt.Fprintf(c.stdout, "%d execution(s) of run %s given up, %d slot(s) returned\n", settled.Closed, rec.RunID, settled.SlotsReturned)
	}
	return 0
}

// settle gives up the executions the run's rows show it released and did not end.
func (c *command) settle(st *store.Store, runID string, events []engine.Event) (Settlement, error) {
	settle := c.Settle
	if settle == nil {
		settle = SchedulerSettle
	}
	return settle(c.ctx, st, runID, ops.OpenReleases(events))
}

// cancelDead ends a run no process holds: one finished row, error kind cancelled, appended under the run lock.
func (c *command) cancelDead(state string, cm *common, rec RunRecord) int {
	write := c.Writer
	if write == nil {
		write = OpenWriter
	}
	st, wstate, err := write(c.ctx, cm.state, cm.socket)
	if err != nil {
		return c.fail("%v", err)
	}
	defer st.Close()
	if wstate != "" {
		state = wstate
	}
	lock, err := TakeLock(state, rec.RunID, "cancel", c.now())
	if err != nil {
		return c.fail("%v", err)
	}
	defer lock.Release()
	j, err := journal.Open(c.ctx, st, rec.RunID, journal.Identity{}, journal.Options{Immediate: true})
	if err != nil {
		return c.fail("%v", err)
	}
	// the run's executions are given up before the run is marked cancelled: a run that is cancelled in the journal and still holds
	// slots is hidden from monitoring and blocks the nodes that overlap it. A failure leaves the run as it was, so the cancel can be
	// repeated.
	settled, err := c.settle(st, rec.RunID, j.Events())
	if err != nil {
		return c.fail("%v", err)
	}
	j.Append(engine.Event{RunID: rec.RunID, Kind: engine.KindRun, Event: "finished", Data: map[string]any{"status": engine.RunScriptError, "error_kind": engine.ErrKindCancelled, "by": "operator", "dead": true, "executions_closed": settled.Closed, "slots_returned": settled.SlotsReturned}})
	if err := j.Close(); err != nil || j.Err() != nil {
		return c.fail("the row could not be stored: %v", errors.Join(err, j.Err()))
	}
	fmt.Fprintf(c.stdout, "run %s had no process; it is cancelled (%d execution(s) given up, %d slot(s) returned)\n", rec.RunID, settled.Closed, settled.SlotsReturned)
	return 0
}
