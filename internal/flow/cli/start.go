package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// PreviewNode is a node a run will work on.
type PreviewNode struct {
	ID    string `json:"id"`
	Issue string `json:"issue,omitempty"`
	Title string `json:"title,omitempty"`
}

// Preview is the nodes of a run and how many of them it works on at once.
type Preview struct {
	Nodes []PreviewNode `json:"nodes"`
	Width int           `json:"width"`
}

// Plan is what `crw flow start` shows before it starts anything.
type Plan struct {
	Workflow    string              `json:"workflow"`
	Phases      []string            `json:"phases,omitempty"`
	Thread      string              `json:"thread"`
	Nodes       []PreviewNode       `json:"nodes"`
	Width       int                 `json:"width"`
	Roles       []engine.RoleReport `json:"roles"`
	Policy      string              `json:"policy"`
	TokenBudget int64               `json:"token_budget"`
	Pool        int                 `json:"pool"`
	Deadline    string              `json:"deadline,omitempty"`
	Decision    string              `json:"decision,omitempty"`
	// Handoff is the sha256 of the crw-plan handoff the run reads its compiled milestone from ("" when it names none).
	Handoff string `json:"handoff,omitempty"`
	// Facts are the preflight facts, each pass, warn or fail with its reading. They never stop the start: the ones that did not
	// pass are printed and recorded in the run's spec.
	Facts []Fact `json:"facts"`
	// Blocked says why the run cannot start (a build without the lane operations, a lane without its compiled milestone).
	Blocked string `json:"blocked,omitempty"`
	// Backend is the backend the run starts on (session or app-server).
	Backend string `json:"backend"`
	// Spawns are the spawn formats of the six roles a session run's steps are spawned with; Loop is how the owner takes them.
	Spawns []RoleSpawn `json:"spawn_formats,omitempty"`
	Loop   string      `json:"loop,omitempty"`
	// PolicyWarnings are the role table's warnings (CRW-1330: a flow-only key at its compiled default, an undecided role, a
	// reviewer on an author's family, an execution policy still declaring agent-roles); the run starts on the table and its
	// started row records them. Absent when there are none.
	PolicyWarnings []string `json:"policyWarnings,omitempty"`
}

// StartAnswer is the JSON answer of `crw flow start --json`.
type StartAnswer struct {
	Plan  Plan   `json:"preview"`
	Run   string `json:"run_id,omitempty"`
	PID   int    `json:"pid,omitempty"`
	State string `json:"state_dir,omitempty"`
	// Bound says the owner thread holds the run's orchestrator line; BindWarning why it does not (a session run).
	Bound       bool   `json:"bound,omitempty"`
	BindWarning string `json:"bind_warning,omitempty"`
	// Next is the command the owner takes the run's steps with (a session run).
	Next string `json:"next,omitempty"`
}

// start is `crw flow start WORKFLOW`: the preview, then the detached run process, then the run id.
func (c *command) start(args []string) int {
	fs, cm := c.flags()
	argsJSON := fs.String("args", "", "the workflow's args, as JSON")
	argsFile := fs.String("args-file", "", "a file holding the workflow's args as JSON")
	thread := fs.String("thread", "", "the orchestrator thread that owns the run (default: CODEX_THREAD_ID)")
	budget := fs.Int64("token-budget", 0, "the run's token ceiling (0: none; a night batch takes its decision's)")
	pool := fs.Int("pool", 0, "agent calls in flight at once (default 6; a session run at most agents.max_threads - 1)")
	backend := fs.String("backend", BackendSession, "session (the calls are role subagents of the owner thread) or app-server (a thread per call: tests and shadow runs)")
	decision := fs.String("decision", "", "the decision record a night batch starts from")
	deadline := fs.String("deadline", "", "an RFC 3339 time the run pauses at")
	repo := fs.String("repo", "", "the git repository the run's worktrees are made from (default: the working directory's)")
	handoff := fs.String("handoff", "", "the crw-plan handoff (crw-flow-compile-input/1) that produced the plan's current revision: the run reads its compiled milestone from a copy of it")
	previewOnly := fs.Bool("preview", false, "show the preview and the preflight facts and start nothing")
	_ = fs.Bool("force", false, "accepted and without effect: a preflight fact never stops the start")
	asJSON := fs.Bool("json", false, "print the answer as JSON")
	positional, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(positional) != 1 {
		return c.usage("exactly one workflow (a built-in name or a script file) is required")
	}
	if *budget < 0 {
		return c.usage("the token budget cannot be negative (0 names none)")
	}
	if *backend != BackendSession && *backend != BackendAppServer {
		return c.usage("--backend is session or app-server, not %q", *backend)
	}
	spec := Spec{Schema: SpecSchema, Workflow: positional[0], Thread: *thread, TokenBudget: *budget, Pool: *pool, Deadline: *deadline, Socket: cm.socket, Decision: *decision, Backend: *backend}
	if spec.Thread == "" {
		spec.Thread = c.getenv("CODEX_THREAD_ID")
	}
	if spec.Workflow != "" && !isBuiltin(spec.Workflow) {
		abs, err := filepath.Abs(spec.Workflow)
		if err != nil {
			return c.fail("%v", err)
		}
		if _, err := os.Stat(abs); err != nil {
			return c.fail("%q is neither a built-in workflow (%s) nor a script file", spec.Workflow, strings.Join(workflows.Names(), ", "))
		}
		spec.Script, spec.Workflow = abs, strings.TrimSuffix(filepath.Base(abs), ".js")
	}
	switch {
	case *argsJSON != "" && *argsFile != "":
		return c.fail("--args and --args-file exclude each other")
	case *argsJSON != "":
		spec.Args = json.RawMessage(*argsJSON)
	case *argsFile != "":
		data, err := os.ReadFile(*argsFile)
		if err != nil {
			return c.fail("%v", err)
		}
		spec.Args = data
	}
	if len(spec.Args) > 0 && !json.Valid(spec.Args) {
		return c.fail("args is not valid JSON")
	}
	spec.Args = compactJSON(spec.Args)

	r, rerr := c.reader(cm)
	if r != nil {
		defer r.Close()
	}
	// the compiled milestone: the bytes of the handoff the run pins (a night batch's decision names its own)
	var handoffRaw []byte
	if *handoff != "" {
		raw, err := os.ReadFile(*handoff)
		if err != nil {
			return c.fail("the handoff cannot be read: %v", err)
		}
		handoffRaw = raw
	}
	// a night batch starts from the manager's decision record
	if spec.Script == "" && spec.Workflow == NightBatchWorkflow {
		if *decision == "" {
			return c.usage("night-batch starts from a decision record: --decision D")
		}
		if rerr != nil {
			return c.fail("the decision %s cannot be read: %v", *decision, rerr)
		}
		batch, err := LoadNightBatch(c.ctx, r.St, *decision)
		if err != nil {
			return c.fail("the decision %s: %v", *decision, err)
		}
		if len(spec.Args) > 0 {
			return c.usage("night-batch takes its arguments from the decision; --args is not given")
		}
		spec.Args = compactJSON(batch.Args(*decision))
		if *handoff != "" {
			return c.usage("night-batch reads the handoff its decision names; --handoff is not given")
		}
		raw, _, err := ReadHandoff(*batch.Handoff)
		if err != nil {
			return c.fail("the handoff of the decision %s: %v", *decision, err)
		}
		handoffRaw = raw
		// the flags narrow what the manager approved and never widen it: a budget above the decision's and a later deadline are refused
		switch {
		case spec.TokenBudget == 0:
			spec.TokenBudget = batch.TokenBudget
		case spec.TokenBudget > batch.TokenBudget:
			return c.fail("the token budget %d is above the %d tokens the decision %s approved", spec.TokenBudget, batch.TokenBudget, *decision)
		}
		if spec.Deadline == "" {
			spec.Deadline = batch.Deadline
		} else {
			asked, err := time.Parse(time.RFC3339, spec.Deadline)
			if err != nil {
				return c.fail("the deadline %q is not an RFC 3339 time", spec.Deadline)
			}
			approved, _ := time.Parse(time.RFC3339, batch.Deadline) // Validate parsed it
			if asked.After(approved) {
				return c.fail("the deadline %s is later than the %s the decision %s approved", spec.Deadline, batch.Deadline, *decision)
			}
		}
	} else if *decision != "" {
		return c.usage("--decision starts a night batch only")
	}
	if spec.Deadline != "" {
		if _, err := time.Parse(time.RFC3339, spec.Deadline); err != nil {
			return c.fail("the deadline %q is not an RFC 3339 time", spec.Deadline)
		}
	}
	handoffSum := ""
	var handoffIn *compile.Input
	if handoffRaw != nil {
		in, err := DecodeHandoff(handoffRaw)
		if err != nil {
			return c.fail("%v", err)
		}
		if why := CheckHandoff(in, planOf(spec.Args), milestoneArg(spec.Args)); why != "" {
			return c.fail("%s", why)
		}
		handoffIn = &in
		if _, named := HandoffOf(spec.Args); named {
			return c.usage("the args name a handoff of their own: the run's handoff is given with --handoff")
		}
		handoffSum = sha256Hex(handoffRaw)
	}
	src, err := spec.Load()
	if err != nil {
		return c.fail("the workflow cannot be loaded: %v", err)
	}
	if spec.Thread == "" && !*previewOnly {
		return c.usage("the owner thread is required: --thread T (or CODEX_THREAD_ID)")
	}

	plan := Plan{Workflow: spec.Workflow, Thread: spec.Thread, TokenBudget: spec.TokenBudget, Pool: spec.Pool, Deadline: spec.Deadline, Decision: spec.Decision, Handoff: handoffSum, Nodes: []PreviewNode{},
		Backend: spec.Backend}
	if plan.Pool <= 0 {
		plan.Pool = engine.DefaultPool
	}
	session := spec.Backend == BackendSession
	if session {
		// the owner's host keeps agents.max_threads subagents open at once, one of them its own helper (CRW-1316 §14)
		spec.Pool = sessionPool(spec.Pool, readCodexAgents(c.getenv).MaxThreads)
		plan.Pool = spec.Pool
	}
	if phases, ok := src.Program.Meta["phases"].([]any); ok {
		for _, p := range phases {
			if s, ok := p.(string); ok {
				plan.Phases = append(plan.Phases, s)
			}
		}
	}
	table, policy, err := engine.LoadPolicy()
	if err != nil {
		return c.fail("%v", err)
	}
	rep := engine.DescribePolicy(table, policy)
	plan.Roles, plan.PolicyWarnings = rep.Roles, rep.Warnings
	if session {
		plan.Spawns, plan.Loop = roleSpawns(table), sessionLoop("<run>")
	}
	if rep.Source != nil {
		plan.Policy = *rep.Source
	} else {
		plan.Policy = "placeholder table (a test with no helper role store)"
	}
	nodes := c.Nodes
	if nodes == nil {
		nodes = nodesOfArgs
	}
	pv, err := nodes(c.ctx, spec, src)
	if err != nil {
		return c.fail("the nodes cannot be listed: %v", err)
	}
	plan.Nodes, plan.Width = pv.Nodes, pv.Width
	if plan.Nodes == nil {
		plan.Nodes = []PreviewNode{}
	}

	if rerr != nil {
		for _, name := range Facts {
			plan.Facts = append(plan.Facts, Fact{Name: name, Level: LevelFail, Reading: "the relay store cannot be read: " + rerr.Error(), Reason: "the store cannot be read"}.leveled())
		}
	} else {
		probe := c.Probe
		if probe == nil {
			probe = StoreProber{}
		}
		for _, f := range probe.Facts(c.ctx, PreflightRequest{Reader: r, Thread: spec.Thread, Waits: Dispositions(src), Now: c.now()}) {
			plan.Facts = append(plan.Facts, f.leveled())
		}
	}
	// the plan's project key against the owner thread's parent bindings, before the run process and so before any role call
	if rerr == nil {
		if f, ok := projectKeyFact(c.ctx, r, planOf(spec.Args), spec.Thread); ok {
			plan.Facts = append(plan.Facts, f)
		}
	}
	// a land of a head made outside a run while another run of its plan has not ended (CRW-1397): a warning, recorded, never a refusal
	if rerr == nil && spec.Workflow == "land-head" {
		if f, ok := planLiveRunFact(c.ctx, r, planOf(spec.Args)); ok {
			plan.Facts = append(plan.Facts, f)
		}
	}
	// the release gate over the issues whose body the handoff carries (CRW-1328): a held issue is a warning, recorded, never a refusal
	if handoffIn != nil {
		if f, ok := issueReadyFact(*handoffIn, planOf(spec.Args), milestoneArg(spec.Args)); ok {
			plan.Facts = append(plan.Facts, f)
		}
	}
	// the facts of a session run (CRW-1316 §11): warnings, never a refusal
	if session {
		req := sessionFactRequest{Thread: spec.Thread, Table: table, Getenv: c.getenv, Socket: cm.socket, State: cm.state}
		if sel, err := ResolveState(cm.state, cm.socket); err == nil {
			req.State = sel.Path
		}
		if req.Socket == "" {
			req.Socket, _ = store.DefaultSocket()
		}
		req.Repo, _ = repository(c.ctx, *repo)
		plan.Facts = append(plan.Facts, sessionFacts(req)...)
	}
	if c.Verifier != nil {
		// the verifier's prerequisites (CRW-1319) do not depend on the store; the fact is a pass or a warning with its reason
		tree, _ := repository(c.ctx, *repo)
		plan.Facts = append(plan.Facts, c.Verifier(c.ctx, tree).leveled())
	}
	// the facts that did not pass are printed with the preview and recorded in the run's spec; none of them stops the start
	var short []Fact
	var shortNames []string
	for _, f := range plan.Facts {
		if f.Level != LevelPass {
			short, shortNames = append(short, f), append(shortNames, f.Name+" "+f.Level)
		}
	}

	if c.LaneOps != nil && !c.LaneOps() {
		if need := LaneOperations(src); len(need) > 0 {
			plan.Blocked = fmt.Sprintf("this build installs no project lane operations (cli.OpsFor is not set) and %s calls %s: the run would end in a script error at its first call", spec.Workflow, strings.Join(need, ", "))
		}
	}
	if plan.Blocked == "" && handoffRaw == nil && slices.Contains(LaneOperations(src), "crw."+ops.NameMilestone) {
		plan.Blocked = fmt.Sprintf("%s reads its milestone with crw.milestone and the run names no compiled milestone: start it with --handoff FILE, the crw-plan handoff (%s) that produced the plan's current revision", spec.Workflow, compile.InputSchema)
	}
	answer := StartAnswer{Plan: plan}
	if !*asJSON {
		c.printPlan(plan)
	}
	switch {
	case plan.Blocked != "":
		if *asJSON {
			_, _ = c.stdout.Write(JSON(answer))
		}
		fmt.Fprintf(c.stderr, "crw flow start: the run is not started: %s\n", plan.Blocked)
		return 1
	case *previewOnly:
		if *asJSON {
			_, _ = c.stdout.Write(JSON(answer))
		}
		return 0
	}

	sel, err := ResolveState(cm.state, cm.socket)
	if err != nil {
		return c.fail("%v", err)
	}
	if spec.RunID, err = NewRunID(); err != nil {
		return c.fail("%v", err)
	}
	spec.CreatedAt = c.now().UTC().Format(time.RFC3339)
	if handoffRaw != nil {
		ref, err := PinHandoff(sel.Path, spec.RunID, handoffRaw)
		if err != nil {
			return c.fail("the handoff cannot be pinned for run %s: %v", spec.RunID, err)
		}
		if spec.Args, err = withHandoff(spec.Args, ref); err != nil {
			return c.fail("%v", err)
		}
	}
	spec.Preflight = short
	if resolved, err := repository(c.ctx, *repo); err == nil {
		spec.Repo = resolved
	} else if *repo != "" {
		return c.fail("%v", err)
	}
	if err := WriteSpec(sel.Path, spec); err != nil {
		return c.fail("the run's spec cannot be written: %v", err)
	}
	if len(short) > 0 {
		fmt.Fprintf(c.stderr, "crw flow start: warning: the run starts with preflight facts that did not pass: %s (recorded in the run's spec)\n", strings.Join(shortNames, ", "))
	}
	// a session run binds its owner thread before its process starts (CRW-1316 §9): the hook decides the owner's spawns by the line
	bound := false
	if session {
		// the start lock stands for the run's process until that process holds the run lock: a start that binds the same owner in that
		// window finds this run live and is not bound (CRW-1372)
		if startLock, err := takeStartLock(sel.Path, spec.RunID); err != nil {
			fmt.Fprintf(c.stderr, "crw flow start: warning: %v: another start binding thread %s before this run's process holds its lock may take the thread\n", err, spec.Thread)
		} else {
			defer startLock.Release()
		}
		res, err := bindOwner(sel.Path, spec.RunID, spec.Thread, table)
		if w := bindWarning(res, err, spec.Thread, spec.RunID); w != "" {
			answer.BindWarning = w
			fmt.Fprintf(c.stderr, "crw flow start: warning: %s\n", w)
		}
		bound = res.Bound || res.Already
		answer.Bound = bound
	}
	dir, _ := os.Getwd()
	spawned := c.now()
	pid, err := c.Spawn(c.ctx, SpawnRequest{Spec: spec, State: sel.Path, Dir: dir})
	if err != nil {
		if bound {
			_, _ = closeOwner(spec.RunID, spec.Thread)
		}
		return c.fail("the run process cannot be started: %v", err)
	}
	if c.Ready != nil {
		if err := c.Ready(c.ctx, sel.Path, spec.RunID, pid, spawned); err != nil {
			return c.fail("run %s: %v", spec.RunID, err)
		}
	}
	answer.Run, answer.PID, answer.State = spec.RunID, pid, sel.Path
	if session {
		answer.Next = "crw flow next " + spec.RunID + " --wait 600 --json"
		answer.Plan.Loop = sessionLoop(spec.RunID)
	}
	if *asJSON {
		_, _ = c.stdout.Write(JSON(answer))
		return 0
	}
	fmt.Fprintf(c.stdout, "run %s started (process %d)\n", spec.RunID, pid)
	fmt.Fprintf(c.stdout, "follow it: crw flow watch %s --follow    look at it: crw flow show %s\n", spec.RunID, spec.RunID)
	if session {
		fmt.Fprintf(c.stdout, "its steps: %s\n  %s\n", answer.Next, sessionLoop(spec.RunID))
	}
	return 0
}

// projectKeyFact compares the project key of the plan the run's arguments name with the scope keys of the live parent bindings of
// the owner thread (CRW-1317). The key a plan is put under is the scope_key of the orchestrator's parent binding (the project's UUID),
// and a plan put under another key (a Linear identifier such as P-CRW-140) is refused only at the run's first claim
// (scope_role_mismatch), after the design calls have spent their tokens. A key the thread is bound to answers no fact (nothing is
// printed); another key is a warning naming both, which the start prints and records and does not refuse. A plan with no revision in
// the store has no key to compare and answers none either.
func projectKeyFact(ctx context.Context, r *Reader, plan, thread string) (Fact, bool) {
	if plan == "" || thread == "" {
		return Fact{}, false
	}
	key, found, err := dag.PlanProject(ctx, r.St.DB, plan)
	var scopes []string
	if err == nil && found {
		scopes, err = dag.LiveParentScopes(ctx, r.St.DB, thread)
	}
	switch {
	case err != nil:
		return Fact{Name: FactProjectKey, Level: LevelFail, Reading: fmt.Sprintf("the plan %s or the bindings of thread %s cannot be read: %v", plan, thread, err), Reason: "the store cannot be read"}.leveled(), true
	case !found || slices.Contains(scopes, key):
		return Fact{}, false
	}
	holds := "thread " + thread + " holds no live 자식 binding"
	if len(scopes) > 0 {
		holds = "thread " + thread + " is the live 자식 of project " + strings.Join(scopes, ", ")
	}
	return Fact{Name: FactProjectKey, Level: LevelWarn,
		Reading: fmt.Sprintf("plan %s is put under project key %s; %s", plan, key, holds),
		Reason:  "put the plan again under the 자식 binding's key: a plan's project key is the scope_key of the orchestrator's 자식 binding (the project UUID), and the run's first claim is refused (scope_role_mismatch) under another key"}.leveled(), true
}

// FactPlanLiveRun is the warning of a land-head start while another run of the same plan has not ended (CRW-1397): the land's release
// claims the plan's coordinator epoch, as every run's does (one run orchestrates one plan), so that run stops epoch_lost at its next
// fenced write. The start goes on: warn and record, never a refusal.
const FactPlanLiveRun = "plan_live_run"

// planLiveRunFact names the runs of the plan that have not ended (running, paused, held or dead).
func planLiveRunFact(ctx context.Context, r *Reader, plan string) (Fact, bool) {
	if plan == "" {
		return Fact{}, false
	}
	infos, err := r.Infos(ctx)
	if err != nil {
		return Fact{Name: FactPlanLiveRun, Level: LevelWarn, Reading: fmt.Sprintf("the runs of plan %s cannot be read: %v", plan, err), Reason: "the store cannot be read"}.leveled(), true
	}
	var live []string
	for _, info := range infos {
		if info.PlanID == plan && !Over(info.State) {
			live = append(live, fmt.Sprintf("%s (%s, %s)", info.RunID, info.Workflow, info.State))
		}
	}
	if len(live) == 0 {
		return Fact{}, false
	}
	return Fact{Name: FactPlanLiveRun, Level: LevelWarn,
		Reading: fmt.Sprintf("plan %s has a run that has not ended: %s", plan, strings.Join(live, ", ")),
		Reason:  "a land-head release claims the plan's coordinator epoch, so a run of the plan that is still going stops epoch_lost at its next fenced write; land the head when no other run of its plan is going (crw flow ps)"}.leveled(), true
}

func isBuiltin(name string) bool {
	for _, n := range workflows.Names() {
		if n == name {
			return true
		}
	}
	return false
}

// nodesOfArgs lists the nodes of the workflows whose arguments name their issues (the night batch); a workflow that reads its plan
// from the relay when it runs lists none.
func nodesOfArgs(_ context.Context, spec Spec, _ Source) (Preview, error) {
	var a struct {
		Issues []string `json:"issues"`
		Cap    int      `json:"cap"`
	}
	_ = json.Unmarshal(spec.Args, &a)
	pv := Preview{Nodes: []PreviewNode{}}
	for _, key := range a.Issues {
		pv.Nodes = append(pv.Nodes, PreviewNode{ID: key, Issue: key})
	}
	pv.Width = min(len(pv.Nodes), max(a.Cap, 0))
	if a.Cap == 0 {
		pv.Width = min(len(pv.Nodes), 3)
	}
	return pv, nil
}

func (c *command) printPlan(p Plan) {
	w := c.stdout
	fmt.Fprintf(w, "crw flow start %s  (owner thread %s, backend %s)\n", p.Workflow, cell(p.Thread), cell(p.Backend))
	if len(p.Phases) > 0 {
		fmt.Fprintf(w, "  phases: %s\n", strings.Join(p.Phases, " > "))
	}
	if len(p.Nodes) == 0 {
		fmt.Fprintln(w, "  nodes: the workflow reads its plan when it runs")
	} else {
		fmt.Fprintf(w, "  nodes: %d, up to %d at once\n", len(p.Nodes), p.Width)
		for _, n := range p.Nodes {
			fmt.Fprintf(w, "    %s %s\n", n.ID, oneLine(n.Title))
		}
	}
	fmt.Fprintf(w, "  role pairs (%s):\n", p.Policy)
	for _, r := range p.Roles {
		fmt.Fprintf(w, "    %-15s %s at %s\n", r.Role, r.Model, r.ReasoningEffort)
	}
	for _, warning := range p.PolicyWarnings {
		fmt.Fprintf(w, "    warning: %s\n", warning)
	}
	if len(p.Spawns) > 0 {
		fmt.Fprintln(w, "  spawn formats of the role subagents:")
		for _, r := range p.Spawns {
			fmt.Fprintf(w, "    %-15s %s\n", r.Role, r.Spawn)
		}
	}
	budget := "none"
	if p.TokenBudget > 0 {
		budget = fmt.Sprintf("%d tokens", p.TokenBudget)
	}
	fmt.Fprintf(w, "  budget: %s   pool: %d", budget, p.Pool)
	if p.Deadline != "" {
		fmt.Fprintf(w, "   deadline: %s", p.Deadline)
	}
	fmt.Fprintln(w)
	if p.Handoff != "" {
		fmt.Fprintf(w, "  handoff: sha256 %s\n", p.Handoff)
	}
	if p.Blocked != "" {
		fmt.Fprintf(w, "  blocked: %s\n", p.Blocked)
	}
	fmt.Fprintln(w, "  preflight:")
	for _, f := range p.Facts {
		mark := f.Level
		if f.Level == LevelFail {
			mark = "FAIL"
		}
		text := f.Reading
		if f.Reason != "" {
			text += "; " + f.Reason
		}
		fmt.Fprintf(w, "    %s  %-18s %s\n", mark, f.Name, oneLine(text))
	}
}
