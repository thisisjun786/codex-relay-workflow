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

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
)

// `crw flow next` and `crw flow dispatched` (CRW-1316 §3.2, CRW-1348): the owner thread's side of a session run's step exchange
// over the run's FileStepBus (<state>/flow/<run>/). next reads the steps the run published and hands them out (a close step's file
// is removed once handed out) with the run's state, the subagent slots and a hint; a run with no process that is not paused and not
// over is resumed (`crw flow resume RUN --detach`), and the owner's binding is written again when it is missing. dispatched writes
// a dispatch report into the run's inbox atomically and waits for the run's answer; the report of a run with no process waits in
// the inbox for the resume.

// The states next reports.
const (
	NextRunning         = "running"
	NextWaitingSession  = "waiting_session"
	NextWaitingDecision = "waiting_decision"
	NextPaused          = "paused"
	NextEnded           = "ended"
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
	// Spawns are the spawn formats of the six roles.
	Spawns []RoleSpawn `json:"spawn_formats"`
	Hint   string      `json:"hint"`
	// Warnings are what next did not find as it should (a binding it wrote again, a resume that did not start).
	Warnings []string `json:"warnings,omitempty"`
	// Resumed is the process next started for a run that had none (0: none was started).
	Resumed int `json:"resumed_pid,omitempty"`
}

// nextPoll is how often next and dispatched look again while they wait.
var nextPoll = 500 * time.Millisecond

// next is `crw flow next RUN [--wait S] [--json]`.
func (c *command) next(args []string) int {
	fs, cm := c.flags()
	wait := fs.Int("wait", 0, "seconds to wait for a step or a change of the run's state (0: answer at once)")
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
	var pinWarnings []string
	// the live run spawns with the pairs it pinned: the line and the spawn formats are those, and a role store that moved since is
	// warned about and recorded, never written over the run's line (CRW-1372)
	if pinned, digest, ok := pinnedPairs(c.runEvents(cm, run), run); ok {
		if digest != table.Digest {
			pinWarnings = append(pinWarnings, c.roleStoreMoved(cm, run, digest, table.Digest))
		}
		table = withPinnedPairs(table, pinned, digest)
	}
	bus := engine.NewFileStepBus(RunDir(sel.Path, run))
	ans := NextAnswer{Run: run, Steps: []NextStep{}, Spawns: roleSpawns(table), Warnings: pinWarnings}
	deadline := time.Now().Add(time.Duration(*wait) * time.Second)
	first, resumed, bindChecked := "", false, false
	var steps []engine.Step
	var info RunInfo
	for {
		info = c.runInfo(cm, run)
		live := LockHeld(sel.Path, run)
		if !live && !resumed && info.State == StateDead && info.Paused == "" {
			// no process, not paused and not over: next resumes the run as `crw flow resume RUN --detach` does
			resumed = true
			if pid, err := c.detachResume(sel.Path, spec); err != nil {
				ans.Warnings = append(ans.Warnings, fmt.Sprintf("no process holds run %s and its resume did not start: %v", run, err))
			} else {
				ans.Resumed = pid
				live = true
				info.State, info.Live = StateRunning, true
			}
		}
		if live && !bindChecked {
			bindChecked = true
			if w := c.checkBinding(sel.Path, spec, table); w != "" {
				ans.Warnings = append(ans.Warnings, w)
			}
		}
		if steps, err = bus.Steps(); err != nil {
			return c.fail("the steps of run %s cannot be read: %v", run, err)
		}
		state := nextState(info, live, steps)
		if first == "" {
			first = state
		}
		if len(steps) > 0 || state != first || state == NextEnded || !time.Now().Before(deadline) {
			ans.State = state
			break
		}
		time.Sleep(nextPoll)
	}
	if ans.State == NextEnded {
		ans.Ended = &NextEnd{Status: info.Status, State: info.State}
	}
	maxThreads := readCodexAgents(c.getenv).MaxThreads
	agents := openSubagents(c.runEvents(cm, run), steps)
	ans.Slots = NextSlots{Limit: maxThreads, Open: len(agents), Agents: agents}
	var closes []string
	for _, st := range steps {
		ns := NextStep{Step: st}
		switch st.Action {
		case engine.StepSpawn:
			ns.Tool = ToolCall{Name: "spawn_agent", Arguments: map[string]any{"agent_type": st.AgentType, "message": st.Message, "model": st.Model,
				"reasoning_effort": st.ReasoningEffort, "fork_context": false}}
			ns.Then = fmt.Sprintf("crw flow dispatched %s %s --agent <the id spawn_agent returned> --k %d --attempt %d", run, st.Call, st.K, st.Attempt)
		case engine.StepSendInput:
			ns.Tool = ToolCall{Name: "send_input", Arguments: map[string]any{"target": st.AgentID, "message": st.Message}}
			ns.Then = fmt.Sprintf("crw flow dispatched %s %s --agent %s --k %d --attempt %d", run, st.Call, st.AgentID, st.K, st.Attempt)
		case engine.StepClose:
			ns.Tool = ToolCall{Name: "close_agent", Arguments: map[string]any{"target": st.AgentID}}
			closes = append(closes, st.AgentID)
		}
		ans.Steps = append(ans.Steps, ns)
	}
	// a close step is the owner's to do once it is handed out: its file goes, so it is handed out once
	for _, st := range steps {
		if st.Action == engine.StepClose {
			if err := os.Remove(filepath.Join(bus.StepsDir(st.Call), engine.CloseName(st.Attempt))); err != nil && !errors.Is(err, os.ErrNotExist) {
				ans.Warnings = append(ans.Warnings, fmt.Sprintf("the close step of %s could not be removed: %v", st.AgentID, err))
			}
		}
	}
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

// nextState is the state next reports.
func nextState(info RunInfo, live bool, steps []engine.Step) string {
	switch {
	case info.State != StateDead && info.State != StateRunning && info.State != StatePaused:
		return NextEnded // completed, held, cancelled, failed: the process ended with a finished row
	case info.Paused != "":
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

// detachResume starts the run's process as `crw flow resume RUN --detach` does and waits until it holds the run.
func (c *command) detachResume(state string, spec Spec) (int, error) {
	dir, _ := os.Getwd()
	spawned := c.now()
	pid, err := c.Spawn(c.ctx, SpawnRequest{Spec: spec, State: state, Resume: true, Dir: dir})
	if err != nil {
		return 0, err
	}
	if c.Ready != nil {
		if err := c.Ready(c.ctx, state, spec.RunID, pid, spawned); err != nil {
			return pid, err
		}
	}
	return pid, nil
}

// checkBinding writes the owner's orchestrator line again when it is missing (another line is last, or none), and says so.
func (c *command) checkBinding(state string, spec Spec, table *engine.Policy) string {
	res, err := bindOwner(state, spec.RunID, spec.Thread, table)
	if w := bindWarning(res, err, spec.Thread, spec.RunID); w != "" {
		return w
	}
	if res.Bound {
		return fmt.Sprintf("the orchestrator binding of thread %s to run %s was missing and is written again", spec.Thread, spec.RunID)
	}
	return ""
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

// pinnedPairs are the role pairs the run's newest started row pinned (a session run records them, CRW-1372) and the digest it pinned
// them under; ok is false for a run that recorded none (it started before the pairs were recorded): the role store is read as before.
func pinnedPairs(events []engine.Event, run string) (map[string]engine.Pair, string, bool) {
	var pairs map[string]engine.Pair
	digest := ""
	for _, ev := range events {
		if ev.Kind != engine.KindRun || ev.Event != "started" || ev.RunID != run {
			continue
		}
		raw, ok := ev.Data["role_pairs"].(map[string]any)
		if !ok {
			continue
		}
		pairs = map[string]engine.Pair{}
		for role, v := range raw {
			p, _ := v.(map[string]any)
			pairs[role] = engine.Pair{Model: str(p, "model"), Effort: str(p, "effort")}
		}
		digest = str(ev.Data, "policy_digest")
	}
	return pairs, digest, pairs != nil
}

// withPinnedPairs is the table with the pairs and the digest the run pinned in place of the store's.
func withPinnedPairs(table *engine.Policy, pinned map[string]engine.Pair, digest string) *engine.Policy {
	out := *table
	out.Roles = maps.Clone(table.Roles)
	if out.Roles == nil {
		out.Roles = map[string]engine.RoleSpec{}
	}
	for role, pair := range pinned {
		spec := out.Roles[role]
		spec.Pair = pair
		out.Roles[role] = spec
	}
	out.Digest = digest
	return &out
}

// roleStoreMoved records, once for the pair of digests, that the role store moved while the run lives (a role_store_changed row of the
// run), and says so. A row that cannot be written leaves the warning.
func (c *command) roleStoreMoved(cm *common, run, pinned, now string) string {
	warning := fmt.Sprintf("the role store changed while run %s lives (it pinned %s, the store now reads %s): the owner's line keeps the pairs the run pinned, "+
		"and a resume takes the new ones", run, cell(pinned), cell(now))
	open := c.Writer
	if open == nil {
		open = OpenWriter
	}
	st, _, err := open(c.ctx, cm.state, cm.socket)
	if err != nil {
		return warning + " (not recorded: " + err.Error() + ")"
	}
	defer st.Close()
	if _, _, err := (report.ForeignAppender{Store: st, Run: run}).AppendOnce(c.ctx, report.NewRow{Kind: KindRoleStoreChanged, Payload: map[string]any{
		"change_key": pinned + ">" + now, "pinned_digest": pinned, "store_digest": now}}, "change_key"); err != nil {
		return warning + " (not recorded: " + err.Error() + ")"
	}
	return warning
}

// KindRoleStoreChanged is the row `crw flow next` writes when the role store moved while a run lives.
const KindRoleStoreChanged = "role_store_changed"

// nextHint says what to do next.
func nextHint(ans NextAnswer, info RunInfo, closes []string) string {
	var parts []string
	if ans.Resumed > 0 {
		parts = append(parts, fmt.Sprintf("no process held run %s: next ran crw flow resume %s --detach (process %d)", ans.Run, ans.Run, ans.Resumed))
	}
	switch ans.State {
	case NextEnded:
		parts = append(parts, fmt.Sprintf("run %s is over (%s, status %s): crw flow show %s", ans.Run, info.State, cell(info.Status), ans.Run))
	case NextPaused:
		parts = append(parts, fmt.Sprintf("run %s is paused (%s): crw flow resume %s goes on", ans.Run, info.Paused, ans.Run))
	case NextWaitingDecision:
		parts = append(parts, fmt.Sprintf("run %s waits for a decision: crw flow show %s lists its questions", ans.Run, ans.Run))
	}
	if len(ans.Steps) > 0 {
		parts = append(parts, sessionLoop(ans.Run))
	} else if ans.State != NextEnded {
		parts = append(parts, fmt.Sprintf("no step to take: wait_agent(targets=[<open agents>], timeout_ms=600000) on the subagents that work, then crw flow next %s --wait 600 --json", ans.Run))
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
	if len(ans.Slots.Agents) > 0 {
		targets, _ := json.Marshal(ans.Slots.Agents)
		hint = strings.ReplaceAll(hint, "[<open agents>]", string(targets))
	}
	return hint
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
			answer["detail"] = fmt.Sprintf("no process holds run %s: the report waits in its inbox, and the run answers it when it is resumed (crw flow next %s resumes it)", run, run)
			break
		}
		if !time.Now().Before(deadline) {
			answer["status"] = "queued"
			answer["detail"] = fmt.Sprintf("run %s has not answered yet: the report stays in its inbox and the run answers it; crw flow next %s shows what is left", run, run)
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
