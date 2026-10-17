package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/crwcmd"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

// `crw flow next` and `crw flow dispatched` (CRW-1316 §3.2, CRW-1348): the owner thread's side of a session run's step exchange
// over the run's FileStepBus (<state>/flow/<run>/). next reads the steps the run published and hands them out (a close step's file
// is removed once handed out) with the run's state, the subagent slots, the spawn formats of the pairs the run's process pinned and a
// hint; a run with no process that is not paused and not over is resumed (`crw flow resume RUN --detach`). next writes neither the
// owner's orchestrator line nor a row of the run: the run's process writes both from its own started row, and next reads them
// (CRW-1372). dispatched writes a dispatch report into the run's inbox atomically and waits for the run's answer; the report of a run
// with no process waits in the inbox for the resume.

// The states next reports. starting is a run whose process is coming up: it holds the run (or its start holds the start lock) and has
// not recorded its started row, or not bound the owner's line after it, within the time next reads it again.
const (
	NextRunning         = "running"
	NextWaitingSession  = "waiting_session"
	NextWaitingDecision = "waiting_decision"
	NextPaused          = "paused"
	NextEnded           = "ended"
	NextStarting        = "starting"
)

// ToolCall is the host tool call the owner makes for a step, with the argument names of the live declaration.
type ToolCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// NextStep is one step as next hands it out: the step, the tool call to make and the command to run right after it.
type NextStep struct {
	engine.Step
	Tool ToolCall `json:"tool"`
	// Then is the dispatched command a spawn or send_input step is reported with, "" for a close.
	Then string `json:"then,omitempty"`
}

// NextEnd is how a run that is over ended.
type NextEnd struct {
	Status string `json:"status"`
	State  string `json:"state"`
}

// NextSlots are the owner's subagent slots: agents.max_threads and how many of them the run's subagents hold.
type NextSlots struct {
	Limit int `json:"limit"`
	Open  int `json:"open"`
	// Agents are the open subagents (wait_agent's targets).
	Agents []string `json:"agents,omitempty"`
}

// NextAnswer is the JSON answer of `crw flow next --json`.
type NextAnswer struct {
	Run   string     `json:"run"`
	State string     `json:"state"`
	Ended *NextEnd   `json:"ended,omitempty"`
	Steps []NextStep `json:"steps"`
	Slots NextSlots  `json:"slots"`
	// Spawns are the spawn formats of the six roles: the pairs the run's process pinned in its started row, the role store's when no
	// process holds the run (a resume reads it), none while the process is starting.
	Spawns []RoleSpawn `json:"spawn_formats"`
	Hint   string      `json:"hint"`
	// Warnings are what next did not find as it should (an owner's line that is not the run's process's, a role store that moved since
	// the process pinned, a resume that did not start).
	Warnings []string `json:"warnings,omitempty"`
	// Resumed is the process next started for a run that had none (0: none was started).
	Resumed int `json:"resumed_pid,omitempty"`
	// answerAppliedAfterPause is whether any decision_applied row follows the run's newest paused end by `answer`: an answer is
	// already applied (the journal does not record which answer a drain was made for).
	answerAppliedAfterPause bool
	// observationPath is the owner's recorded start policy, supplied explicitly;
	// the run spec's advisory preflight facts do not select a wait path.
	observationPath string
}

const (
	observationEventIdle = "event-driven-idle"
	observationActive    = "active-observation"
)

// nextPoll is how often next and dispatched look again while they wait.
var nextPoll = 500 * time.Millisecond

// startingWait is how long next reads again a run whose process is starting, beyond --wait: a process records its started row and binds
// the owner's line soon after it holds the run.
var startingWait = 15 * time.Second

// next is `crw flow next RUN [--observation-path MODE] [--wait S] [--json]`.
func (c *command) next(args []string) int {
	fs, cm := c.flags()
	wait := fs.Int("wait", 0, "seconds to wait for a step or a change of the run's state (0: answer at once)")
	observation := fs.String("observation-path", observationActive, "the recorded start policy: event-driven-idle or active-observation (does not establish wake readiness)")
	asJSON := fs.Bool("json", false, "print the answer as JSON")
	positional, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(positional) != 1 || !validRunID(positional[0]) {
		return c.usage("exactly one run id is required")
	}
	if *wait < 0 {
		return c.usage("--wait is a number of seconds, 0 or more")
	}
	if *observation != observationEventIdle && *observation != observationActive {
		return c.usage("--observation-path is event-driven-idle or active-observation")
	}
	run := positional[0]
	sel, err := ResolveState(cm.state, cm.socket)
	if err != nil {
		return c.fail("%v", err)
	}
	spec, err := ReadSpec(sel.Path, run)
	if err != nil {
		return c.fail("%v", err)
	}
	if specBackend(spec) != BackendSession {
		return c.fail("run %s runs on the %s backend: it publishes no steps (crw flow show %s)", run, specBackend(spec), run)
	}
	table, _, err := engine.LoadPolicy()
	if err != nil {
		return c.fail("%v", err)
	}
	bus := engine.NewFileStepBus(RunDir(sel.Path, run))
	// a run no process holds spawns with the role store when it is resumed; the process that holds it, with the pairs it pinned (below)
	ans := NextAnswer{Run: run, Steps: []NextStep{}, Spawns: roleSpawns(table), observationPath: *observation}
	deadline := time.Now().Add(time.Duration(*wait) * time.Second)
	first, resumed := "", false
	var steps []engine.Step
	var info RunInfo
	var held heldProcess
	var live bool
	// until bounds the reads of a process that is starting
	var until time.Time
	for {
		info = c.runInfo(cm, run)
		live = LockHeld(sel.Path, run)
		// a start holds the start lock until its process holds the run lock: that run is starting, not one to resume
		starting := !live && startLockHeld(sel.Path, run)
		if !live && !starting && !resumed && info.State == StateDead && info.Paused == "" {
			// no process, not paused and not over: next resumes the run as `crw flow resume RUN --detach` does
			resumed = true
			var legacy *legacyRunError
			if pid, err := c.detachResume(cm, sel.Path, spec); errors.As(err, &legacy) {
				// a legacy `child` session run is not resumed by any later look (CRW-1415): the refusal is next's answer, not a wait
				return c.fail("%v", err)
			} else if err != nil {
				ans.Warnings = append(ans.Warnings, fmt.Sprintf("no process holds run %s and its resume did not start: %v", run, err))
			} else {
				ans.Resumed = pid
				info, live = c.runInfo(cm, run), LockHeld(sel.Path, run)
			}
		}
		held = heldProcess{}
		if live {
			// what the process that holds the run recorded, read now: next never takes an earlier process's pins for it (CRW-1372)
			held = c.heldProcess(cm, sel.Path, spec, table)
			starting = held.starting
		}
		if starting || held.binding {
			// read again, startingWait at most: past it a starting run is answered as such, and a line not bound is reported
			if until.IsZero() {
				until = time.Now().Add(startingWait)
			}
			if time.Now().Before(until) {
				time.Sleep(nextPoll)
				continue
			}
		} else {
			until = time.Time{}
		}
		if steps, err = bus.Steps(); err != nil {
			return c.fail("the steps of run %s cannot be read: %v", run, err)
		}
		state := nextState(info, live, steps)
		if starting {
			state = NextStarting
		}
		if first == "" {
			first = state
		}
		if len(steps) > 0 || state != first || state == NextEnded || !time.Now().Before(deadline) {
			ans.State = state
			break
		}
		time.Sleep(nextPoll)
	}
	switch {
	case ans.State == NextStarting:
		// the process has recorded no pairs yet: next prints none rather than an earlier process's or the store's
		ans.Spawns = []RoleSpawn{}
	case live:
		ans.Spawns = roleSpawns(held.table)
		ans.Warnings = append(ans.Warnings, held.warnings(run, table, ans.Resumed > 0)...)
	}
	if ans.State == NextEnded {
		ans.Ended = &NextEnd{Status: info.Status, State: info.State}
	}
	maxThreads := readCodexAgents(c.getenv).MaxThreads
	events := c.runEvents(cm, run)
	agents := openSubagents(events, steps)
	ans.Slots = NextSlots{Limit: maxThreads, Open: len(agents), Agents: agents}
	var closes []string
	// the owner runs a then as printed: it names the crw that runs, not the bare crw a PATH without it does not find (CRW-1572)
	crw := crwcmd.Word()
	if w := crwcmd.Warning(); w != "" {
		ans.Warnings = append(ans.Warnings, w)
	}
	for _, st := range steps {
		ns := NextStep{Step: st}
		switch st.Action {
		case engine.StepSpawn:
			ns.Tool = ToolCall{Name: "spawn_agent", Arguments: map[string]any{"agent_type": st.AgentType, "message": st.Message, "model": st.Model,
				"reasoning_effort": st.ReasoningEffort, "fork_context": false}}
			ns.Then = fmt.Sprintf("%s flow dispatched %s %s --agent <the id spawn_agent returned> --k %d --attempt %d", crw, run, st.Call, st.K, st.Attempt)
		case engine.StepSendInput:
			ns.Tool = ToolCall{Name: "send_input", Arguments: map[string]any{"target": st.AgentID, "message": st.Message}}
			ns.Then = fmt.Sprintf("%s flow dispatched %s %s --agent %s --k %d --attempt %d", crw, run, st.Call, st.AgentID, st.K, st.Attempt)
		case engine.StepClose:
			ns.Tool = ToolCall{Name: "close_agent", Arguments: map[string]any{"target": st.AgentID}}
			closes = append(closes, st.AgentID)
		}
		ans.Steps = append(ans.Steps, ns)
	}
	// a close step is the owner's to do once it is handed out: its file goes, so it is handed out once
	for _, st := range steps {
		if st.Action == engine.StepClose {
			if err := os.Remove(filepath.Join(bus.StepsDir(st.Call), st.File)); err != nil && !errors.Is(err, os.ErrNotExist) {
				ans.Warnings = append(ans.Warnings, fmt.Sprintf("the close step of %s could not be removed: %v", st.AgentID, err))
			}
		}
	}
	ans.answerAppliedAfterPause = answerAfterPaused(events, run)
	ans.Hint = nextHint(ans, info, closes)
	if *asJSON {
		_, _ = c.stdout.Write(JSON(ans))
	} else {
		c.printNext(ans)
	}
	for _, w := range ans.Warnings {
		fmt.Fprintf(c.stderr, "crw flow next: warning: %s\n", w)
	}
	return 0
}

// runInfo reads the run's line; a run whose first process has not stored its start yet reads as running while the lock is held,
// as dead otherwise.
func (c *command) runInfo(cm *common, run string) RunInfo {
	r, err := c.reader(cm)
	if err != nil {
		return RunInfo{State: StateDead}
	}
	defer r.Close()
	rec, err := r.Record(c.ctx, run)
	if err != nil {
		if LockHeld(r.State, run) {
			return RunInfo{State: StateRunning, Live: true}
		}
		return RunInfo{State: StateDead}
	}
	info, err := r.Info(c.ctx, rec)
	if err != nil {
		return RunInfo{State: StateDead}
	}
	return info
}

// runEvents reads the run's rows ("" when they cannot be read: no slot is counted).
func (c *command) runEvents(cm *common, run string) []engine.Event {
	r, err := c.reader(cm)
	if err != nil {
		return nil
	}
	defer r.Close()
	events, _, err := journal.LoadLean(c.ctx, r.St, run)
	if err != nil {
		return nil
	}
	return events
}

// draining is whether the run's process lives and the ask that will end the run paused is an answer's drain (CRW-1492): the run stops
// at its next boundary and the turns in flight run to their end, so the steps the run published before the drain are still the owner's
// to take. info.Live is a run process's hold only (a writer's hold, such as the answer's own, is not one); info.Paused is `by: reason`
// of the last ask that ends the run paused (Summarize), so an operator's pause after the drain names the end.
func draining(info RunInfo) bool {
	return info.Live && pausedForAnswer(info)
}

// pausedForAnswer is whether the last ask that ends the run paused is an answer's drain.
func pausedForAnswer(info RunInfo) bool { return strings.HasPrefix(info.Paused, "answer: ") }

// answerAfterPaused is whether a decision_applied row follows the run's newest paused end by `answer`. Any such row counts - the
// journal does not name the answer a pause was made for - so a later application after a later paused end is a different one.
func answerAfterPaused(events []engine.Event, run string) bool {
	paused, applied := false, false
	for _, ev := range events {
		if ev.RunID != run {
			continue
		}
		if ev.Kind == engine.KindRun {
			switch ev.Event {
			case "started", "finished":
				paused, applied = false, false
			case "paused":
				paused, applied = str(ev.Data, "by") == "answer", false
			}
		}
		if paused && ev.Kind == engine.KindFlow && ev.Event == ops.EvDecisionApplied {
			applied = true
		}
	}
	return applied
}

// nextState is the state next reports. A paused run is paused, except while an answer drains it and its process lives: next answers
// from its waits and steps as for any live run, until the process ends (an operator's pause interrupts the turns: paused at once).
func nextState(info RunInfo, live bool, steps []engine.Step) string {
	switch {
	case info.State != StateDead && info.State != StateRunning && info.State != StatePaused:
		return NextEnded // completed, held, cancelled, failed: the process ended with a finished row
	case info.Paused != "" && !draining(info):
		return NextPaused
	case info.Waiting > 0:
		return NextWaitingDecision
	}
	for _, st := range steps {
		if st.Action != engine.StepClose {
			return NextWaitingSession
		}
	}
	return NextRunning
}

// detachResume starts the run's process as `crw flow resume RUN --detach` does and waits until it holds the run. Like that command and
// `crw flow answer` it asks resumeRefused first (CRW-1390, CRW-1415): a run whose spec's budget is spent, or a legacy `child` session run (a
// *legacyRunError, which next answers as its error), is refused with the same words, before a process is started that would take the run
// lock and end at once; so is a run whose recorded TMPDIR cannot be used (checkRunEnv, CRW-1570).
func (c *command) detachResume(cm *common, state string, spec Spec) (int, error) {
	r, err := c.reader(cm)
	if err != nil {
		return 0, err
	}
	err = resumeRefused(c.ctx, r.St, spec)
	_ = r.Close()
	if err != nil {
		return 0, err
	}
	// a recorded TMPDIR that cannot be used is refused before a process is started (CRW-1570); next answers it as its warning
	if err := checkRunEnv(spec); err != nil {
		return 0, err
	}
	dir, _ := os.Getwd()
	spawned := c.now()
	pid, err := c.Spawn(c.ctx, SpawnRequest{Spec: spec, State: state, Resume: true, Dir: dir})
	if err != nil {
		return 0, err
	}
	if c.Ready != nil {
		if err := c.Ready(c.ctx, state, spec.RunID, pid, spawned); err != nil {
			return 0, err
		}
	}
	return pid, nil
}

// openSubagents are the subagents that hold the owner's slots, sorted: the subagents bound to calls that have not ended, and those
// whose close step waits.
func openSubagents(events []engine.Event, steps []engine.Step) []string {
	open := map[string]bool{}
	agentOf := map[string][]string{}
	for _, ev := range events {
		if ev.Kind != engine.KindCall || ev.Op != "agent" {
			continue
		}
		switch {
		case ev.Event == engine.EvThreadStarted && str(ev.Data, "via") == "session":
			if a := str(ev.Data, "thread_id"); a != "" {
				agentOf[ev.CallID] = append(agentOf[ev.CallID], a)
				open[a] = true
			}
		case ev.Event == engine.EvSessionAttemptLost:
			delete(open, str(ev.Data, "agent_id"))
		case engine.IsTerminalEvent(ev.Event):
			for _, a := range agentOf[ev.CallID] {
				delete(open, a)
			}
		}
	}
	for _, st := range steps {
		if st.Action == engine.StepClose && st.AgentID != "" {
			open[st.AgentID] = true
		}
	}
	out := make([]string, 0, len(open))
	for a := range open {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// heldProcess is what next reads of the process that holds a run (CRW-1372). The process is the one party that records its pairs and
// writes the owner's orchestrator line while it holds the run: its started row pins the table it runs on and names its process lock,
// and right after it the process binds the owner's line with that table. next reads both and writes neither.
type heldProcess struct {
	// starting: the process has not recorded its started row (the newest started row is an earlier process's, or there is none), so
	// next has no pairs of it. binding: the row is there and the owner's line after it is not yet.
	starting, binding bool
	// table is the role store's table with the pairs and the digest the process's started row pinned (the store's for a row that pinned
	// none: a run started before the pairs were pinned).
	table *engine.Policy
	// repinned is the policy_repinned row after the process's started row: its pairs moved from those an earlier process pinned.
	repinned *engine.Event
	// line says what next found of the owner's line when it is not the process's ("" when it is).
	line string
}

// heldProcess reads the process that holds the run: the newest started row is that process's when the process lock its token names is
// held. A newest row whose process lock was let go is an earlier process's, and so is a row that names no process while a process lock
// of the run is held, and next reads again (starting): the holder has not recorded its row yet, or the run lock is held for a moment by
// a command that writes a row and runs no process (an audit, a cancel, or an answer from the end of the process it drained until its
// row is stored; the answer lets the lock go before it starts the resume, which takes the lock after it). A row that names no process
// while no process lock is held is that of a process of a crw before the process lock, which bound the owner's line before it
// recorded its row.
func (c *command) heldProcess(cm *common, state string, spec Spec, store *engine.Policy) heldProcess {
	run := spec.RunID
	// Read the owner's line before the journal: the process commits started and
	// policy_repinned before binding. A line read after the journal could settle
	// an older snapshot that contains started but not policy_repinned (CRW-1638).
	last, present, lineErr := bound.OrchestratorLine(nil, spec.Thread)
	events := c.runEvents(cm, run)
	newest := -1
	for i, ev := range events {
		if ev.Kind == engine.KindRun && ev.Event == "started" && ev.RunID == run {
			newest = i
		}
	}
	token := ""
	if newest >= 0 {
		token = str(events[newest].Data, "process")
	}
	switch {
	case token != "" && !processHeld(state, run, token):
		return heldProcess{starting: true}
	case token == "" && anyProcessHeld(state, run):
		return heldProcess{starting: true}
	}
	out := heldProcess{table: store}
	if newest >= 0 {
		if pairs, digest, ok := rowPairs(events[newest]); ok {
			out.table = withPinnedPairs(store, pairs, digest)
		}
		for _, ev := range events[newest+1:] {
			if ev.Kind == engine.KindRun && ev.Event == engine.EvPolicyRepinned && ev.RunID == run {
				ev := ev
				out.repinned = &ev
			}
		}
	}
	settled, line := ownerLineFrom(state, spec, out.table.Digest, last, present, lineErr)
	// a process of this crw binds right after its started row: the line of a process that started within startingWait and is not
	// settled yet is read again; an older process's line is what it bound, and is reported
	out.binding, out.line = token != "" && !settled && processFresh(state, run, token), line
	return out
}

// warnings are what next says of the process it read: a line of the owner that is not the process's; for a process next resumed, that
// it pinned other pairs than the process before it (its policy_repinned row); for one next found, a role store that reads other pairs
// now than the process pinned. next records none of them: the process records its pairs in its started row, and a resume the change in
// a policy_repinned row.
func (h heldProcess) warnings(run string, store *engine.Policy, resumed bool) []string {
	var out []string
	if h.line != "" {
		out = append(out, h.line)
	}
	switch {
	case resumed && h.repinned != nil:
		out = append(out, fmt.Sprintf("run %s had no process and its resume runs on the role store as it reads now (%s), not on the pairs its earlier process pinned (%s): "+
			"the resumed process recorded them in its started row and the change in a policy_repinned row; the owner's line and the spawn formats are the resumed process's",
			run, cell(str(h.repinned.Data, "new")), cell(str(h.repinned.Data, "old"))))
	case !resumed && h.table.Digest != store.Digest:
		out = append(out, fmt.Sprintf("the role store changed while run %s lives (its process pinned %s, the store now reads %s): the process runs on the pairs it pinned and the owner's line keeps them, "+
			"and a resume takes the new ones (recorded then in its started row and a policy_repinned row)", run, cell(h.table.Digest), cell(store.Digest)))
	}
	return out
}

// ownerLineOf reads the owner thread's last line for the process that holds the run, which pinned digest: settled when it is that
// process's (the run's open line under digest), when another live run holds the thread (one thread binds one live run, so the
// process could not bind) or when no line can name the owner; line says what next found when it is not the process's line.
func ownerLineOf(state string, spec Spec, digest string) (settled bool, line string) {
	last, present, err := bound.OrchestratorLine(nil, spec.Thread)
	return ownerLineFrom(state, spec, digest, last, present, err)
}

// ownerLineFrom judges the captured line against the process's pinned digest.
func ownerLineFrom(state string, spec Spec, digest string, last bound.Line, present bool, err error) (settled bool, line string) {
	run, thread := spec.RunID, spec.Thread
	if !bound.ValidThreadID(thread) {
		return true, fmt.Sprintf("the owner %q is not a thread id: no flow-bound line names it, and the spawn hook gives its spawns no role pair", thread)
	}
	switch {
	case err != nil:
		return false, fmt.Sprintf("the orchestrator line of thread %s cannot be read (%v): the spawn hook decides the owner's spawns by it", thread, err)
	case present && last.Kind == bound.KindOrchestrator && last.RunID == run && last.PolicyDigest == digest:
		return true, ""
	case present && last.Kind != bound.KindClosed && last.RunID != run && validRunID(last.RunID) && runLive(state, last.RunID):
		return true, liveHolderText(thread, last.RunID, run)
	case present && last.Kind == bound.KindOrchestrator && last.RunID == run:
		return false, fmt.Sprintf("the orchestrator line of thread %s carries run %s's pairs under %s, not those its process pinned (%s): next writes no line; the run's next process binds it after its started row",
			thread, run, cell(last.PolicyDigest), cell(digest))
	}
	return false, fmt.Sprintf("thread %s holds no open line of run %s, so the spawn hook gives the owner's spawns no pair of the run: next writes no line; the run's process binds it after its started row, and its next process when it is resumed",
		thread, run)
}

// rowPairs are the role pairs a session run's started row pinned (CRW-1372) and the digest it pinned them under; ok is false for a row
// that recorded none (a run started before the pairs were recorded): the role store is read as before.
func rowPairs(ev engine.Event) (map[string]engine.Pair, string, bool) {
	raw, ok := ev.Data["role_pairs"].(map[string]any)
	if !ok {
		return nil, "", false
	}
	pairs := map[string]engine.Pair{}
	for role, v := range raw {
		p, _ := v.(map[string]any)
		pairs[role] = engine.Pair{Model: str(p, "model"), Effort: str(p, "effort")}
	}
	return pairs, str(ev.Data, "policy_digest"), true
}

// withPinnedPairs is the table with the pairs and the digest the run pinned in place of the store's.
func withPinnedPairs(table *engine.Policy, pinned map[string]engine.Pair, digest string) *engine.Policy {
	out := *table
	out.Roles = maps.Clone(table.Roles)
	if out.Roles == nil {
		out.Roles = map[string]engine.RoleSpec{}
	}
	for role, pair := range pinned {
		if role == execution.AgentUnitCheck {
			// the unit check's tool pair is pinned beside the agent roles, never among them (CRW-1589)
			out.UnitCheck = pair
			continue
		}
		spec := out.Roles[role]
		spec.Pair = pair
		out.Roles[role] = spec
	}
	out.Digest = digest
	return &out
}

// KindRoleStoreChanged is a flow row no command writes: the run's process is the one party that records a run's pairs and their change
// (CRW-1372). The journal of an older run may hold one.
const KindRoleStoreChanged = "role_store_changed"

// nextHint says what to do next.
func nextHint(ans NextAnswer, info RunInfo, closes []string) string {
	var parts []string
	// each command of the hint starts with the crw that runs (CRW-1572)
	crw := crwcmd.Word()
	if ans.Resumed > 0 {
		parts = append(parts, fmt.Sprintf("no process held run %s: next ran %s flow resume %s --detach (process %d)", ans.Run, crw, ans.Run, ans.Resumed))
	}
	switch ans.State {
	case NextEnded:
		parts = append(parts, fmt.Sprintf("run %s is over (%s, status %s): %s flow show %s", ans.Run, info.State, cell(info.Status), crw, ans.Run))
	case NextPaused:
		if pausedForAnswer(info) {
			if ans.answerAppliedAfterPause {
				parts = append(parts, fmt.Sprintf("run %s ended paused for an answer; an answer is already applied: %s flow resume %s goes on with it; replaying the same answer starts no process", ans.Run, crw, ans.Run))
				break
			}
			// the run stopped for an answer (its drain): the answer applies and resumes it once no process holds the run
			parts = append(parts, fmt.Sprintf("run %s ended paused for an answer: run the same %s flow answer again (it applies and resumes the run); %s flow resume %s goes on without it", ans.Run, crw, crw, ans.Run))
			break
		}
		parts = append(parts, fmt.Sprintf("run %s is paused (%s): %s flow resume %s goes on", ans.Run, info.Paused, crw, ans.Run))
	case NextWaitingDecision:
		parts = append(parts, fmt.Sprintf("run %s waits for a decision: %s flow show %s lists its questions", ans.Run, crw, ans.Run))
	case NextStarting:
		if ans.observationPath == observationEventIdle {
			parts = append(parts, fmt.Sprintf("run %s's process is starting: no spawn format is given yet; %s flow next %s --observation-path event-driven-idle --wait 0 --json reads it again after startup", ans.Run, crw, ans.Run))
			break
		}
		parts = append(parts, fmt.Sprintf("run %s's process is starting: it holds the run and has not recorded its started row or bound the owner's line yet, so no spawn format is given: "+
			"%s flow next %s --wait 600 --json reads it again", ans.Run, crw, ans.Run))
	}
	waiting := ans.State == NextRunning || ans.State == NextWaitingSession
	switch {
	case len(ans.Steps) > 0:
		parts = append(parts, "take every step's tool call and its dispatched command: spawn_agent with the step's values, send_input(target=<agent_id>, message=<message>), or close_agent(target=<agent_id>); then use the remaining open agents")
		parts = append(parts, nextAgentWait(ans))
	case waiting && ans.Slots.Open > 0:
		parts = append(parts, "no step to take: "+nextAgentWait(ans))
	case waiting && ans.observationPath == observationEventIdle:
		parts = append(parts, "no open subagents and no step to take: end this turn; the run keeps working; when the run publishes the next step, the relay wakes this owner")
	case waiting:
		parts = append(parts, fmt.Sprintf("no open subagents and no step to take: wait on %s flow next %s --wait 600 --json alone; keep at most one next command in flight", crw, ans.Run))
	}
	spawns := 0
	for _, st := range ans.Steps {
		if st.Action == engine.StepSpawn {
			spawns++
		}
	}
	free := ans.Slots.Limit - ans.Slots.Open - 1 // one slot stays the owner's own
	if len(closes) > 0 {
		parts = append(parts, "close_agent(target=...) "+strings.Join(closes, ", ")+" first: a finished subagent holds its slot until it is closed")
	}
	if spawns > max(free, 0) {
		parts = append(parts, fmt.Sprintf("%d of the %d subagent slots (agents.max_threads) are held: spawn_agent fails with `agent thread limit reached` until agents are closed; a step that could not be spawned stays and is shown again", ans.Slots.Open, ans.Slots.Limit))
	}
	hint := strings.Join(parts, ". ")
	// A batch can close these agents and spawn new ones; only an empty answer
	// can name the wait targets before the owner has run the tools.
	if len(ans.Steps) == 0 && len(ans.Slots.Agents) > 0 {
		targets, _ := json.Marshal(ans.Slots.Agents)
		hint = strings.ReplaceAll(hint, "[<open agents>]", string(targets))
	}
	return hint
}

// nextAgentWait is shared by an empty answer with open agents and a batch that
// can change that set. Only a completion, not an empty timeout, triggers next.
func nextAgentWait(ans NextAnswer) string {
	mode := ""
	if ans.observationPath == observationEventIdle {
		mode = " --observation-path event-driven-idle"
	}
	return fmt.Sprintf("if subagents remain open, wait_agent(targets=[<open agents>], timeout_ms=600000) on all of them, using the longest timeout the live tool permits; on an empty timeout repeat the same wait without calling next. "+
		"When wait reports completion, run any still-needed dispatched command, then %s flow next %s%s --wait 120 --json once so the engine can read the result; take its steps and wait on the remaining open agents again. "+
		"If the batch leaves no open agents, use that same next command once and follow its state and hint", crwcmd.Word(), ans.Run, mode)
}

func (c *command) printNext(ans NextAnswer) {
	w := c.stdout
	fmt.Fprintf(w, "run %s: %s", ans.Run, ans.State)
	if ans.Ended != nil {
		fmt.Fprintf(w, " (%s, status %s)", ans.Ended.State, cell(ans.Ended.Status))
	}
	fmt.Fprintf(w, "   slots %d/%d\n", ans.Slots.Open, ans.Slots.Limit)
	for _, st := range ans.Steps {
		fmt.Fprintf(w, "  %s %s/t%d-a%d %s", st.Action, st.Call, st.K, st.Attempt, st.Role)
		if st.AgentID != "" {
			fmt.Fprintf(w, " agent %s", st.AgentID)
		}
		fmt.Fprintln(w)
		args := []string{}
		for _, k := range []string{"agent_type", "target", "model", "reasoning_effort", "fork_context"} {
			if v, ok := st.Tool.Arguments[k]; ok {
				args = append(args, fmt.Sprintf("%s=%v", k, v))
			}
		}
		fmt.Fprintf(w, "    %s(%s)\n", st.Tool.Name, strings.Join(args, ", "))
		if st.Packet != "" {
			fmt.Fprintf(w, "    packet %s   workspace %s\n", st.Packet, st.Workspace)
		}
		if st.Then != "" {
			fmt.Fprintf(w, "    then: %s\n", st.Then)
		}
	}
	fmt.Fprintf(w, "hint: %s\n", ans.Hint)
}

// dispatched is `crw flow dispatched RUN CALL --agent ID --k K --attempt N [--wait S] [--json]`.
func (c *command) dispatched(args []string) int {
	fs, cm := c.flags()
	agentID := fs.String("agent", "", "the subagent spawn_agent returned (or the one send_input went to)")
	k := fs.Int("k", 0, "the step's turn k")
	attempt := fs.Int("attempt", 0, "the step's attempt")
	wait := fs.Int("wait", 120, "seconds to wait for the run's answer")
	asJSON := fs.Bool("json", false, "print the answer as JSON")
	positional, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	switch {
	case len(positional) != 2 || !validRunID(positional[0]) || positional[1] == "":
		return c.usage("a run id and a call id are required")
	case *agentID == "":
		return c.usage("--agent names the subagent")
	case *k < 1 || *attempt < 1:
		return c.usage("--k and --attempt are the step's, 1 or more")
	case *wait < 0:
		return c.usage("--wait is a number of seconds, 0 or more")
	}
	run, call := positional[0], positional[1]
	sel, err := ResolveState(cm.state, cm.socket)
	if err != nil {
		return c.fail("%v", err)
	}
	if _, err := ReadSpec(sel.Path, run); err != nil {
		return c.fail("%v", err)
	}
	// a legacy `child` session run is read only (CRW-1415): no report is put in its inbox, which its resume would journal
	if code, refused := c.legacyRefused(cm, sel.Path, run); refused {
		return code
	}
	bus := engine.NewFileStepBus(RunDir(sel.Path, run))
	id, err := reportID(c.now())
	if err != nil {
		return c.fail("%v", err)
	}
	rep := engine.DispatchReport{ID: id, Run: run, Call: call, K: *k, Attempt: *attempt, Agent: *agentID}
	if err := bus.Report(rep); err != nil {
		return c.fail("the report cannot be written to the inbox of run %s: %v", run, err)
	}
	answer := map[string]any{"id": id, "run": run, "call": call, "agent": *agentID}
	deadline := time.Now().Add(time.Duration(*wait) * time.Second)
	for {
		reply, found, err := bus.ReadReply(id)
		if err != nil {
			return c.fail("the answer to report %s cannot be read: %v", id, err)
		}
		if found {
			answer["status"], answer["detail"] = reply.Status, reply.Detail
			if reply.Close != "" {
				answer["close"] = reply.Close
			}
			break
		}
		if !LockHeld(sel.Path, run) {
			answer["status"] = "queued"
			answer["detail"] = fmt.Sprintf("no process holds run %s: the report waits in its inbox, and the run answers it when it is resumed (%s flow next %s resumes it)", run, crwcmd.Word(), run)
			break
		}
		if !time.Now().Before(deadline) {
			answer["status"] = "queued"
			answer["detail"] = fmt.Sprintf("run %s has not answered yet: the report stays in its inbox and the run answers it; %s flow next %s shows what is left", run, crwcmd.Word(), run)
			break
		}
		time.Sleep(nextPoll)
	}
	if *asJSON {
		_, _ = c.stdout.Write(JSON(answer))
	} else {
		fmt.Fprintf(c.stdout, "%s: %s\n", answer["status"], answer["detail"])
	}
	switch answer["status"] {
	case engine.ReplyBound, "queued":
		return 0
	}
	return 1
}

// reportID is the id of a dispatch report: the time it was written and a random part, so the inbox reads in the order written.
func reportID(now time.Time) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%020d-%s", now.UnixNano(), hex.EncodeToString(b[:])), nil
}
