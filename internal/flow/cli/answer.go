package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/decisions"
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
	// DrainAsked says this answer asked a run process that held the run to stop at its next boundary (SIGUSR2 sent, CRW-1492). A sent
	// signal is an ask, not a stop: a process an older crw runs writes the same holder record and drops the signal.
	DrainAsked bool `json:"drain_asked,omitempty"`
	// Drained says every run process this answer asked let the run go within --wait (the answer then held the lock, or found it held
	// by another holder).
	Drained bool `json:"drained,omitempty"`
}

// RefusedRunDraining is the refusal of an answer that asked the run process holding the run to stop at its next boundary and did not
// get the run lock within --wait (CRW-1492). The stop is asked, not known to be made: a run process of this crw ends paused by
// `answer`, and the same answer, run again once the run is paused, applies once and resumes the run; a process that does not take the
// ask (an older crw) goes on, and is paused with `crw flow pause RUN` before the answer.
const RefusedRunDraining = "run_draining"

// answer is `crw flow answer`: it compares the manager's answer record with the run's journal, appends the one `decision_applied`
// row that resumes the wait, and resumes that wait. The record is the only input: what the caller names (the run, the node, the
// question and its revision) is a claim that has to be the record's and the journal's too. A run a process holds is asked to stop
// at its next boundary first (drainAndTake, CRW-1492); --wait says how long the answer waits for it to let the run go.
func (c *command) answer(args []string) int {
	fs, cm := c.flags()
	decision := fs.String("decision", "", "the answer record (crw manage decisions) to apply")
	runID := fs.String("run", "", "the run the answer is for")
	node := fs.String("node", "", "the node the question is about (empty for a question of the milestone)")
	question := fs.String("question", "", "the id of the question the record answers")
	revision := fs.Int("revision", 0, "the revision of the question the record answers")
	thread := fs.String("thread", "", "the orchestrator thread that applies the answer (default: CODEX_THREAD_ID)")
	noResume := fs.Bool("no-resume", false, "append the row and do not resume the run")
	wait := fs.Int("wait", 0, "seconds to wait for a run process this answer drains to let the run go (0: ask the drain and return)")
	// the parts of the recorded answer for the questions an ask joins (CRW-1381), by question digest
	var parts map[string]string
	fs.Func("answer-for", "DIGEST=TEXT: the part of the recorded answer for one question an ask joins, by its digest (repeatable; text of the record's answer)", func(v string) error {
		digest, text, ok := strings.Cut(v, "=")
		if !ok || strings.TrimSpace(digest) == "" || strings.TrimSpace(text) == "" {
			return fmt.Errorf("--answer-for %q is not DIGEST=TEXT", v)
		}
		if parts == nil {
			parts = map[string]string{}
		}
		if _, dup := parts[digest]; dup {
			return fmt.Errorf("--answer-for names question %s twice", digest)
		}
		parts[digest] = text
		return nil
	})
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
	case *wait < 0:
		return c.usage("--wait %d is not 0 or more seconds", *wait)
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
	reply, code := c.applyAnswer(st, state, Claim{Decision: *decision, Run: *runID, Node: *node, Question: *question, Revision: *revision, By: by, Answers: parts}, time.Duration(*wait)*time.Second)
	for _, w := range rowWarnings(reply.Row) {
		fmt.Fprintf(c.stderr, "crw flow answer: warning: %s\n", w)
	}
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

// rowWarnings are the warnings of a decision_applied row (CRW-1381).
func rowWarnings(row map[string]any) []string {
	var out []string
	switch w := row["warnings"].(type) {
	case []string:
		out = w
	case []any:
		for _, x := range w {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// applyAnswer judges the claim against the record and the journal and, when it holds, appends the row under the run lock. A legacy
// `child` session run (legacyRun) is refused under the lock before the claim is judged: nothing is appended to it. A run a run process
// holds is drained first (drainAndTake): the answer is applied only under the lock, once that process let it go, within wait. The
// decision record is judged as the store holds it after the last wait: `crw relay decision-withdraw` changes a record without the run
// lock, so a record read before a writer's hold or a drain ended is never the one applied (decisionNow). It returns the exit status
// of the command.
func (c *command) applyAnswer(st *store.Store, state string, claim Claim, wait time.Duration) (AnswerReply, int) {
	reply := AnswerReply{Run: claim.Run}
	// an unknown decision fails before the run lock is asked for; the record itself is read again after every wait
	if _, ok := c.decisionNow(st, claim); !ok {
		return reply, 2
	}
	r := &Reader{St: st, State: state}
	run, err := r.Record(c.ctx, claim.Run)
	if err != nil {
		fmt.Fprintf(c.stderr, "crw flow answer: error: %v\n", err)
		return reply, 2
	}
	// a writer that holds the run lock for a moment (an audit, another answer, a cancel) is waited out, briefly (CRW-1495 item 6)
	lock, err := takeLockPastWriters(state, claim.Run, "answer", c.now(), writerWait)
	if errors.Is(err, ErrRunLive) {
		// a run process (or a holder this command cannot name) holds the run: drained, and the lock taken once it let go (CRW-1492)
		var done bool
		lock, reply, done = c.drainAndTake(st, state, run, claim, wait)
		if done {
			return reply, replyCode(reply)
		}
	} else if err != nil {
		reply.Refusal = &Refusal{Code: "run_live", Detail: err.Error() + "; an answer is applied to the journal of a run no process holds"}
		return reply, 1
	}
	defer lock.Release()
	j, err := journal.Open(c.ctx, st, claim.Run, journal.Identity{}, journal.Options{Immediate: true})
	if err != nil {
		fmt.Fprintf(c.stderr, "crw flow answer: error: %v\n", err)
		return reply, 2
	}
	// a legacy `child` session run is read only (CRW-1415): no answer is appended to it, whether the run is to be resumed after or not
	if why := legacyRun(j.Events(), claim.Run, run.Workflow); why != "" {
		reply.Refusal = &Refusal{Code: RefusedLegacyRun, Detail: why}
		return reply, 1
	}
	// the record as it is now, under the lock, after the writer's hold or the drain this answer waited for: a decision withdrawn (or
	// expired) meanwhile is refused here with its reason, and nothing is appended or resumed
	rec, ok := c.decisionNow(st, claim)
	if !ok {
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

// decisionNow reads the answer's decision record as the store holds it at this moment (an error is said on stderr: exit 2). Every
// judgment of an answer reads it here, right before it judges: the record can change while the answer waits (decision-withdraw takes
// no run lock, and an answered record may be withdrawn or expire), so a record read before a wait is never judged after it.
func (c *command) decisionNow(st *store.Store, claim Claim) (decisions.Record, bool) {
	rec, err := st.Get(c.ctx, claim.Decision)
	if err != nil {
		fmt.Fprintf(c.stderr, "crw flow answer: error: %v\n", err)
		return decisions.Record{}, false
	}
	return rec, true
}

// replyCode is the exit status of an answer that ends with its reply: 2 for a failure that is no refusal (said on stderr), 1 for a
// refusal, 0 for a replay or an application.
func replyCode(reply AnswerReply) int {
	switch {
	case reply.Refusal != nil:
		return 1
	case reply.OK:
		return 0
	}
	return 2
}

// prejudge judges the claim on a snapshot of the run's journal read without the run lock (the rows without their large payloads: the
// questions and the applications are flow rows, read whole), as the locked path judges it: a legacy `child` session run, then Judge, on
// the decision record read right then (decisionNow: one withdrawn while the answer waited is refused before anything more is asked).
// It appends nothing and opens no journal handle (a handle numbers its rows from the rows it loaded, so one opened before a drained
// process's last rows would fail its append). It answers done with the reply when the answer ends here: a refusal, a replay (OK), or
// a journal or a record that cannot be read (neither: exit 2, said on stderr).
func (c *command) prejudge(st *store.Store, run RunRecord, claim Claim) (AnswerReply, bool) {
	reply := AnswerReply{Run: claim.Run}
	events, _, err := loadLeanJournal(c.ctx, st, claim.Run)
	if err != nil {
		fmt.Fprintf(c.stderr, "crw flow answer: error: the journal of run %s cannot be read: %v\n", claim.Run, err)
		return reply, true
	}
	if why := legacyRun(events, claim.Run, run.Workflow); why != "" {
		reply.Refusal = &Refusal{Code: RefusedLegacyRun, Detail: why}
		return reply, true
	}
	rec, ok := c.decisionNow(st, claim)
	if !ok {
		return reply, true
	}
	app, refusal := Judge(rec, events, run, claim)
	if refusal != nil {
		reply.Refusal = refusal
		return reply, true
	}
	if app.Replayed {
		reply.Row, reply.Replayed, reply.Seq, reply.OK = app.Data, true, app.Seq, true
		return reply, true
	}
	return reply, false
}

// drainPoll is how often an answer that drained a run looks for the run lock again.
const drainPoll = 200 * time.Millisecond

// holderID is who holds a run lock, as its record says: a holder this answer signalled is not signalled again.
type holderID struct {
	pid            int
	since, command string
}

// drainAndTake is the answer to a run a process holds (CRW-1492). The claim is judged on the journal and on the decision record as
// they are first (prejudge): a refusal or a replay is the answer, and nothing is signalled. Then, in each round, a run process that
// holds the run (a `_run` or `resume` record naming a pid) and was not signalled by this answer is judged again, on the decision record
// read again (a decision withdrawn while the answer waited is refused and drains nothing more) and on a snapshot read after its holder
// record - a run process can hold the run after the first judgment only because it started after it, and the resume of another
// answer starts only after that answer's row is stored, so this judgment refuses (another decision) or replays (the same decision)
// instead of draining it -, its holder record is read again (a holder that changed meanwhile waits for the next round), and it is
// sent SIGUSR2: a run process of this crw stops at its next boundary (the turns in flight end, the next requests are held) and ends
// paused by `answer`. A sent signal is an ask (DrainAsked), not a stop: a process an older crw runs writes the same holder record and
// drops it, so the answer says the run drained (Drained) only once every process it asked let the run go within wait. Any other
// holder (a writer, a record naming no command or no pid) is never signalled. The lock is taken every drainPoll until wait has passed
// (with wait 0, once, right after the signal). Taken, the locked path judges and appends as for a run no process holds; the journal
// is opened there, after the drained process's rows. Not taken: run_draining when this answer asked a stop (its words say whether the
// process it asked still holds the run), run_live when it asked nothing. It answers done with the reply when the answer ends here, or
// the lock it holds.
func (c *command) drainAndTake(st *store.Store, state string, run RunRecord, claim Claim, wait time.Duration) (*Lock, AnswerReply, bool) {
	if reply, done := c.prejudge(st, run, claim); done {
		return nil, reply, true
	}
	signal := c.Signal
	if signal == nil {
		signal = killProcess
	}
	signalled := map[holderID]bool{}
	// asked is the last run process this answer sent SIGUSR2 (0: none): an ask, which only the process letting the run go answers
	asked := 0
	deadline := time.Now().Add(wait)
	var lastErr error
	for {
		if holder, held := lockHolderRecord(state, claim.Run); held && holder.PID > 1 && runProcessCommand(holder.Command) && !signalled[idOf(holder)] {
			if reply, done := c.prejudge(st, run, claim); done {
				// a run process this answer did not signal holds the run: every process it asked let the run go
				reply.DrainAsked, reply.Drained = asked > 0, asked > 0
				return nil, reply, true
			}
			if again, still := lockHolderRecord(state, claim.Run); still && idOf(again) == idOf(holder) {
				signalled[idOf(holder)] = true
				switch err := signal(holder.PID, syscall.SIGUSR2); {
				case err == nil:
					asked = holder.PID
				case !errors.Is(err, syscall.ESRCH):
					fmt.Fprintf(c.stderr, "crw flow answer: warning: process %d of run %s cannot be asked to stop at its next boundary: %v\n", holder.PID, claim.Run, err)
				}
			}
		}
		lock, err := takeLockPastWriters(state, claim.Run, "answer", c.now(), 0)
		if err == nil {
			// the lock is this answer's: every process it asked let the run go within the wait
			return lock, AnswerReply{Run: claim.Run, DrainAsked: asked > 0, Drained: asked > 0}, false
		}
		if !errors.Is(err, ErrRunLive) {
			return nil, AnswerReply{Run: claim.Run, DrainAsked: asked > 0, Refusal: &Refusal{Code: "run_live", Detail: err.Error()}}, true
		}
		lastErr = err
		if !time.Now().Before(deadline) {
			break
		}
		timer := time.NewTimer(min(drainPoll, time.Until(deadline)))
		select {
		case <-c.ctx.Done():
			timer.Stop()
			lastErr = errors.Join(lastErr, c.ctx.Err())
			deadline = time.Now()
		case <-timer.C:
		}
		if c.ctx.Err() != nil {
			break
		}
	}
	reply := AnswerReply{Run: claim.Run, DrainAsked: asked > 0}
	if asked > 0 {
		reply.Refusal = &Refusal{Code: RefusedRunDraining}
		h, held := lockHolderRecord(state, claim.Run)
		switch {
		case held && h.PID > 1 && signalled[idOf(h)]:
			// the kill succeeded and the process still holds the run: a stop asked, not one made (an older crw drops the signal)
			reply.Refusal.Detail = fmt.Sprintf("this answer asked process %d of run %s to stop at its next boundary (SIGUSR2) and the process still holds the run after --wait %s: the stop is asked, not made. "+
				"A run process of this crw stops there (the calls in flight end, the next ones are held) and ends paused by `answer`: run the same crw flow answer again once crw flow next reads the run paused; it applies once and resumes the run. "+
				"A process that does not take the ask (one an older crw runs) goes on as it would have: crw flow pause %s, then the same crw flow answer", h.PID, claim.Run, wait, claim.Run)
		case held && h.PID <= 1:
			// the record names no process: whether the asked process let go is not known
			reply.Refusal.Detail = fmt.Sprintf("this answer asked process %d of run %s to stop at its next boundary (SIGUSR2) and the run lock is still held after --wait %s (holder unknown): the stop is asked, not known to be made; run the same crw flow answer again once crw flow next reads the run paused", asked, claim.Run, wait)
		default:
			// every process this answer asked let the run go; another holder took the lock first (or let it go right then)
			reply.Drained = true
			holder := "free again"
			if held {
				holder = "held by crw flow " + cell(h.Command)
			}
			reply.Refusal.Detail = fmt.Sprintf("process %d of run %s, which this answer asked to stop at its next boundary, let the run go within --wait %s, and the run lock was not this answer's when the wait ended (%s): run the same crw flow answer again; it judges the run as it is then", asked, claim.Run, wait, holder)
		}
		return nil, reply, true
	}
	holder := "holder unknown"
	if h, held := lockHolderRecord(state, claim.Run); held && h.Command != "" {
		holder = "held by crw flow " + h.Command
	}
	reply.Refusal = &Refusal{Code: "run_live", Detail: fmt.Sprintf("%v (%s); nothing was asked of it: an answer drains only a run process of run %s", lastErr, holder, claim.Run)}
	return nil, reply, true
}

// runProcessCommand says whether a holder's command is a run process an answer may drain: `crw flow _run` and `crw flow resume`.
func runProcessCommand(cmd string) bool { return cmd == ProcessCommand || cmd == "resume" }

func idOf(r lockRecord) holderID { return holderID{pid: r.PID, since: r.Since, command: r.Command} }

// resumeDetached starts the process of a run again from its journal; a run with nothing left of its spec's budget, and a legacy
// `child` session run, is refused before (resumeRefused, over the journal in st).
func (c *command) resumeDetached(st *store.Store, state, run string) (int, error) {
	spec, err := ReadSpec(state, run)
	if err != nil {
		return 0, err
	}
	if err := resumeRefused(c.ctx, st, spec); err != nil {
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
// refused, since nothing runs. A legacy `child` session run is refused before anything (legacyRefused).
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
	// a legacy `child` session run is read only (CRW-1415): no finished row, no execution given up, and no signal that makes its process write
	if code, refused := c.legacyRefused(cm, state, run); refused {
		return code
	}
	if Over(info.State) {
		if verb == "cancel" {
			// a cancel whose process ended before the executions were given up (or whose settlement failed) is finished here, and so are
			// the executions a run that ended otherwise still holds (a land whose release was not given back, CRW-1397): its cancel gives
			// them up; a run that ended with none open has nothing to cancel
			settled, code := c.settleEndedRun(state, cm, rec)
			if code != 0 {
				return code
			}
			return c.endedCancel(run, info.State, settled)
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
				fmt.Fprintf(c.stderr, "crw flow cancel: process %d still holds run %s after %ds: %s; when they end the run gives up its executions itself (no second cancel)\n",
					info.PID, run, *wait, c.endingCalls(cm, run))
				return 1
			}
			time.Sleep(50 * time.Millisecond)
		}
		fmt.Fprintf(c.stdout, "run %s ended\n", run)
		return c.settleEnded(state, cm, rec)
	}
	return 0
}

// endingCalls says what a run whose process still holds it after a cancel's wait is ending (CRW-1573): the run's journal is read
// again, since the run process wrote its cancel_waiting row after the signal, and inflightCalls names the calls in it.
func (c *command) endingCalls(cm *common, run string) string {
	r, err := c.reader(cm)
	if err != nil {
		return fmt.Sprintf("no call in flight is known (the run's journal cannot be read: %v)", err)
	}
	events, _, err := loadLeanJournal(c.ctx, r.St, run)
	_ = r.Close()
	if err != nil {
		return fmt.Sprintf("no call in flight is known (the run's journal cannot be read: %v)", err)
	}
	calls := inflightCalls(events)
	if len(calls) == 0 {
		return "no call in flight is known"
	}
	return "it is ending " + strings.Join(calls, ", ")
}

// settleEnded gives up the executions of a cancelled run whose process has ended: under the run lock, so no process works on the
// run, from the rows of its journal. A second call finds nothing open.
func (c *command) settleEnded(state string, cm *common, rec RunRecord) int {
	_, code := c.settleEndedRun(state, cm, rec)
	return code
}

// settleEndedRun is settleEnded with what it gave up.
func (c *command) settleEndedRun(state string, cm *common, rec RunRecord) (Settlement, int) {
	write := c.Writer
	if write == nil {
		write = OpenWriter
	}
	st, wstate, err := write(c.ctx, cm.state, cm.socket)
	if err != nil {
		return Settlement{}, c.fail("%v", err)
	}
	defer st.Close()
	if wstate != "" {
		state = wstate
	}
	lock, err := takeLockPastWriters(state, rec.RunID, "cancel", c.now(), writerWait)
	if err != nil {
		return Settlement{}, c.fail("the executions of run %s cannot be given up yet: %v", rec.RunID, err)
	}
	defer lock.Release()
	events, _, err := journal.Load(c.ctx, st, rec.RunID)
	if err != nil {
		return Settlement{}, c.fail("%v", err)
	}
	settled, err := c.settle(st, rec.RunID, events)
	if err != nil {
		return Settlement{}, c.fail("%v", err)
	}
	c.saySettled(rec.RunID, settled)
	if Summarize(rec, events, false, 0).State == StateCancelled {
		if err := c.releaseCancelledBinding(st, state, rec.RunID); err != nil {
			return settled, c.fail("the run ended but its binding could not be released: %v", err)
		}
	}
	return settled, 0
}

// saySettled says what giving up the executions of an ended run did, when it did something.
func (c *command) saySettled(run string, settled Settlement) {
	if settled.Closed > 0 || settled.SlotsReturned > 0 {
		fmt.Fprintf(c.stdout, "%d execution(s) of run %s given up, %d slot(s) returned\n", settled.Closed, run, settled.SlotsReturned)
	}
}

// endedCancel is the exit of a cancel of a run that is over (state): one that ended otherwise and still held executions is cancelled by
// giving them up (0); a cancelled run, or one with nothing left to give up, had nothing to cancel (1).
func (c *command) endedCancel(run, state string, settled Settlement) int {
	if state != StateCancelled && (settled.Closed > 0 || settled.SlotsReturned > 0) {
		return 0
	}
	fmt.Fprintf(c.stderr, "crw flow cancel: run %s is %s: nothing to cancel\n", run, state)
	return 1
}

// settle gives up the executions the run's rows show it released and did not end.
func (c *command) settle(st *store.Store, runID string, events []engine.Event) (Settlement, error) {
	settle := c.Settle
	if settle == nil {
		settle = SchedulerSettle
	}
	return settle(c.ctx, st, runID, ops.OpenReleases(events))
}

// cancelDead ends a run no process holds: one finished row, error kind cancelled, appended under the run lock. The row says the run
// was dead (`dead: true`) only when the rows read under the lock show a dead run (a start with no end): a paused end or a held run
// had no process because it ended, not because its process died (CRW-1495). What the caller read before the lock is not used.
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
	lock, err := takeLockPastWriters(state, rec.RunID, "cancel", c.now(), writerWait)
	if err != nil {
		return c.fail("%v", err)
	}
	defer lock.Release()
	j, err := journal.Open(c.ctx, st, rec.RunID, journal.Identity{}, journal.Options{Immediate: true})
	if err != nil {
		return c.fail("%v", err)
	}
	// a cancel that read the run before another cancel (or the run's own end) stored its end row finds the run over under the lock:
	// it appends nothing, so two cancels of one run write one end row (CRW-1495 item 6), and it gives up what the ended run still
	// holds from the rows read under the lock, as a cancel that read the ended run from the start does (settleEndedRun)
	if now := Summarize(rec, j.Events(), false, 0).State; Over(now) {
		events := j.Events()
		_ = j.Close()
		settled, err := c.settle(st, rec.RunID, events)
		if err != nil {
			return c.fail("%v", err)
		}
		c.saySettled(rec.RunID, settled)
		return c.endedCancel(rec.RunID, now, settled)
	}
	// the run's executions are given up before the run is marked cancelled: a run that is cancelled in the journal and still holds
	// slots is hidden from monitoring and blocks the nodes that overlap it. A failure leaves the run as it was, so the cancel can be
	// repeated.
	events := j.Events()
	dead := Summarize(rec, events, false, 0).State == StateDead
	settled, err := c.settle(st, rec.RunID, events)
	if err != nil {
		return c.fail("%v", err)
	}
	row := map[string]any{"status": engine.RunScriptError, "error_kind": engine.ErrKindCancelled, "by": "operator", "executions_closed": settled.Closed, "slots_returned": settled.SlotsReturned}
	if dead {
		row["dead"] = true
	}
	j.Append(engine.Event{RunID: rec.RunID, Kind: engine.KindRun, Event: "finished", Data: row})
	if err := j.Close(); err != nil || j.Err() != nil {
		return c.fail("the row could not be stored: %v", errors.Join(err, j.Err()))
	}
	if err := c.releaseCancelledBinding(st, state, rec.RunID); err != nil {
		return c.fail("the run ended but its binding could not be released: %v", err)
	}
	fmt.Fprintf(c.stdout, "run %s had no process; it is cancelled (%d execution(s) given up, %d slot(s) returned)\n", rec.RunID, settled.Closed, settled.SlotsReturned)
	return 0
}
