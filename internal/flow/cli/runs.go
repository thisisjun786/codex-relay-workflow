package cli

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/apply"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/callstate"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

// The states a run is shown in. A run that is not over has a process (running, paused) or none (dead: a run lock nobody holds
// and no finished row after the last start); a run that is over is completed, held (it finished, but a call or a wait is left to
// go on with), blocked (it finished completed and started no node: see Summarize), cancelled or failed.
const (
	StateRunning   = "running"
	StatePaused    = "paused"
	StateDead      = "dead"
	StateHeld      = "held"
	StateCompleted = "completed"
	StateBlocked   = "blocked"
	StateCancelled = "cancelled"
	StateFailed    = "failed"
)

// Over reports whether a run in this state needs nothing more from its owner: it completed, was blocked, was cancelled or failed.
func Over(state string) bool {
	return state == StateCompleted || state == StateBlocked || state == StateCancelled || state == StateFailed
}

// RunRecord is the flow_runs row of a run: its identity.
type RunRecord struct {
	RunID        string `json:"run_id"`
	Workflow     string `json:"workflow"`
	ScriptDigest string `json:"script_digest"`
	ArgsDigest   string `json:"args_digest"`
	PlanID       string `json:"plan_id"`
	PlanRevision int64  `json:"plan_revision"`
	Owner        string `json:"owner"`
	ResumeFrom   string `json:"resume_from,omitempty"`
	StartedAt    string `json:"started_at"`
}

// RunInfo is the one-line reading of a run (`crw flow ps`).
type RunInfo struct {
	RunRecord
	State string `json:"state"`
	// Live is true while a process holds the run lock; PID is that process.
	Live bool `json:"live"`
	PID  int  `json:"pid,omitempty"`
	// Processes counts the process starts of the run (the first start and every resume).
	Processes int `json:"processes"`
	Calls     int `json:"calls"`
	// Unsettled counts the calls a resume goes on with, Holds the calls in recovery_hold, Waiting the questions no decision has
	// answered yet.
	Unsettled int    `json:"unsettled"`
	Holds     int    `json:"recovery_holds"`
	Waiting   int    `json:"waiting"`
	Tokens    int64  `json:"tokens"`
	Phase     string `json:"phase,omitempty"`
	// Status and ErrorKind are those of the last finished row; Paused says who paused the run and why.
	Status    string `json:"status,omitempty"`
	ErrorKind string `json:"error_kind,omitempty"`
	Paused    string `json:"paused,omitempty"`
	// HeldReasons are the reasons calls are held for (budget_exhausted, engine_paused, ...), once each.
	HeldReasons []string `json:"held_reasons,omitempty"`
	LastSeq     int      `json:"last_seq"`
	// ItemErrors are the join items that threw in the last process of the run (its item_error rows, engine/scriptvm.go): a lane of
	// crw.dag whose release failed is one.
	ItemErrors []ItemError `json:"item_errors,omitempty"`
}

// ItemError is a join item that threw: the key of the item (the node of a lane), the path of the item and the error it threw.
type ItemError struct {
	Item  string `json:"item"`
	Path  string `json:"path"`
	Error string `json:"error"`
}

// Question is a question a run asked the orchestrator (an escalation that carries an ask) and whether a decision answered it.
type Question struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
	Node     string `json:"node"`
	Reason   string `json:"reason"`
	Ask      string `json:"ask"`
	Call     string `json:"call,omitempty"`
	Attempt  int    `json:"attempt,omitempty"`
	Seq      int    `json:"seq"`
	// Current is true for the latest question of its node and ask: an answer to an earlier revision is stale.
	Current bool `json:"current"`
	// Applied is the decision that answered it, "" while it waits.
	Applied string `json:"applied,omitempty"`
	// Questions are the digests of the questions a design ask joins (its evidence, CRW-1366): an answer can give each its part
	// (CRW-1381). Absent for an ask that names none.
	Questions []string `json:"questions,omitempty"`
}

// ReviewLink is one review verdict of a node: the reviewer's call and what it judged.
type ReviewLink struct {
	Call    string `json:"call"`
	Head    string `json:"head"`
	State   string `json:"state"`
	Verdict string `json:"verdict,omitempty"`
}

// AgentNode is one agent call in the tree.
type AgentNode struct {
	CallID       string `json:"call_id"`
	Label        string `json:"label,omitempty"`
	Role         string `json:"role"`
	Node         string `json:"node,omitempty"`
	Attempt      int    `json:"attempt,omitempty"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	ThreadID     string `json:"thread_id,omitempty"`
	Tokens       int64  `json:"tokens"`
	RecoveryHold bool   `json:"recovery_hold,omitempty"`
}

// PhaseNode is a phase of the run with the agent calls made in it.
type PhaseNode struct {
	Title  string      `json:"title"`
	Agents []AgentNode `json:"agents"`
}

// NodeInfo is what a plan node of the run has so far: its threads, its commits, its reviews, its tokens and whether a call of it is
// in recovery_hold.
type NodeInfo struct {
	Node         string       `json:"node"`
	Threads      []string     `json:"threads,omitempty"`
	Commits      []string     `json:"commits,omitempty"`
	Reviews      []ReviewLink `json:"reviews,omitempty"`
	Accepted     string       `json:"acceptance,omitempty"`
	Integrated   string       `json:"integrated,omitempty"`
	Tokens       int64        `json:"tokens"`
	RecoveryHold bool         `json:"recovery_hold,omitempty"`
	Held         bool         `json:"held,omitempty"`
}

// RunDetail is `crw flow show`: the reading of a run and its tree.
type RunDetail struct {
	RunInfo
	// PolicyPath and PolicyDigest are the execution policy file the run recorded when it started and the digest of its bytes
	// (the run's last start or repin), both empty for a run on the placeholder table, which reads no file. PolicyRepinnedFrom
	// is the digest the run held before a resume read another one, empty if no resume did (CRW-1324).
	PolicyPath         string `json:"policy_path"`
	PolicyDigest       string `json:"policy_digest"`
	PolicyRepinnedFrom string `json:"policy_repinned_from,omitempty"`
	// PolicyUnrecorded: the run's latest start was journaled before policy_source existed, so its policy file is not known; the
	// digest is shown when the row has one (CRW-1360).
	PolicyUnrecorded bool        `json:"policy_unrecorded,omitempty"`
	Phases           []PhaseNode `json:"phases"`
	Nodes            []NodeInfo  `json:"nodes"`
	Questions        []Question  `json:"questions"`
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func num(m map[string]any, key string) int {
	switch n := m[key].(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// tokensOf is the token total a row carries, whatever form the journal gave it (the stored rows are decoded JSON, the rows of a
// memory journal are the engine's values).
func tokensOf(d map[string]any) int64 {
	switch t := d["tokens"].(type) {
	case engine.Tokens:
		return t.Total
	case map[string]any:
		switch n := t["total"].(type) {
		case float64:
			return int64(n)
		case int64:
			return n
		case int:
			return int64(n)
		}
	}
	return 0
}

// callRows groups the call rows by call id, in the order of each call's first row.
func callRows(events []engine.Event) (order []string, rows map[string][]engine.Event) {
	rows = map[string][]engine.Event{}
	for _, e := range events {
		if e.Kind != engine.KindCall || e.CallID == "" {
			continue
		}
		if _, ok := rows[e.CallID]; !ok {
			order = append(order, e.CallID)
		}
		rows[e.CallID] = append(rows[e.CallID], e)
	}
	return order, rows
}

func isAgentCall(rows []engine.Event) bool {
	for _, r := range rows {
		if r.Op == "agent" || r.Event == engine.EvPrepared {
			return true
		}
	}
	return false
}

func classify(rows []engine.Event) callstate.Class {
	in := make([]callstate.Row, 0, len(rows))
	for _, r := range rows {
		in = append(in, callstate.Row{Event: r.Event, Reason: str(r.Data, "reason")})
	}
	return callstate.Classify(in)
}

// lastTerminal is the status and reason of a call's last terminal row, or running when it has none since its last start.
func lastTerminal(rows []engine.Event) (status, reason string) {
	status = "running"
	for _, r := range rows {
		switch r.Event {
		case engine.EvPrepared, engine.EvThreadRequested, engine.EvForkRequested, engine.EvTurnRequested, engine.EvThreadStarted, engine.EvForkStarted, engine.EvTurnStarted, "recovery_cleared",
			engine.EvStepPublished, engine.EvSessionAttemptLost:
			status, reason = "running", ""
		case engine.EvCompleted, engine.EvNullModel, engine.EvHeld, engine.EvRefused, engine.EvOutcomeUnknown, engine.EvSuperseded:
			status, reason = r.Event, str(r.Data, "reason")
		}
	}
	return status, reason
}

// Questions reads the questions a run asked: its escalation rows that carry an ask, the revision each has among those of its
// node and ask, and the decision_applied row that answered it. The latest question of a node and ask is the current one.
func Questions(events []engine.Event) []Question {
	attempts := map[string]int{}
	for _, e := range events {
		if e.Kind == engine.KindCall && e.Event == engine.EvPrepared {
			attempts[e.CallID] = num(e.Data, "attempt")
		}
	}
	applied := map[string]string{}
	for _, e := range events {
		if e.Kind == engine.KindFlow && e.Event == ops.EvDecisionApplied {
			if d, ok := e.Data["decision"].(map[string]any); ok {
				applied[str(e.Data, "ask")] = str(d, "id")
			} else {
				applied[str(e.Data, "ask")] = "applied"
			}
		}
	}
	var out []Question
	count := map[string]int{}
	last := map[string]int{}
	for _, e := range events {
		if e.Kind != engine.KindFlow || e.Event != engine.EvEscalation {
			continue
		}
		ask := str(e.Data, "ask")
		if ask == "" {
			continue
		}
		key := str(e.Data, "node") + "\x00" + ask
		count[key]++
		q := Question{ID: str(e.Data, "escalation_id"), Revision: count[key], Node: str(e.Data, "node"), Reason: str(e.Data, "reason"), Ask: ask, Call: str(e.Data, "call_id"), Seq: e.Seq}
		q.Attempt = attempts[q.Call]
		q.Applied = applied[q.ID]
		if evidence, ok := e.Data["evidence"].(map[string]any); ok {
			list, _ := evidence["questions"].([]any)
			for _, d := range list {
				if s, _ := d.(string); s != "" {
					q.Questions = append(q.Questions, s)
				}
			}
		}
		last[key] = len(out)
		out = append(out, q)
	}
	for _, i := range last {
		out[i].Current = true
	}
	return out
}

// Summarize reads the rows of a run: its state, its counts and its last phase. live says whether a process holds the run lock.
//
// A run meant to start the nodes of a plan (StartsNodes: a lane workflow, or a run whose arguments name a plan) whose last process finished
// completed with nothing left to go on with is blocked, not completed, when it started no node (no crw.release released one, no
// crw.releaseChild released a child and nothing landed, in any process) and join items of its last process threw (the releases of crw.dag
// that failed are such items). The engine's status of the run stays completed. A run whose last process has a node the scheduler held back
// unreleased (a wait a resume asks again, CRW-1201), a run where a node started and a run not meant to start one (a judge panel, a scout)
// keep the state completed.
func Summarize(rec RunRecord, events []engine.Event, live bool, pid int) RunInfo {
	info := RunInfo{RunRecord: rec, Live: live, PID: pid}
	lastStart, lastFinish := -1, -1
	for i, e := range events {
		if int64(e.Seq) < engine.ForeignSeqBase {
			// the cursor of the tree is the run's own last row: a command's row (CRW-1204) is numbered apart (journal.ReadPage)
			info.LastSeq = max(info.LastSeq, e.Seq)
		}
		if e.Kind == engine.KindRun {
			switch e.Event {
			case "started":
				info.Processes++
				lastStart = i
			case "finished":
				lastFinish = i
				info.Status, info.ErrorKind = str(e.Data, "status"), str(e.Data, "error_kind")
			}
		}
		if e.Kind == engine.KindFlow && e.Event == "phase" {
			info.Phase = str(e.Data, "title")
		}
	}
	heldBack := false
	for i := lastStart + 1; i < len(events); i++ {
		e := events[i]
		switch {
		case e.Kind == engine.KindFlow && e.Event == engine.EvRunPaused:
			info.Paused = str(e.Data, "by") + ": " + str(e.Data, "reason")
		case e.Kind == engine.KindFlow && e.Event == evItemError:
			info.ItemErrors = append(info.ItemErrors, ItemError{Item: itemKey(e.Path), Path: e.Path, Error: str(e.Data, "error")})
		case e.Kind == engine.KindCall && e.Op == ops.OpRelease && e.Event == engine.EvCompleted && !releasedRow(e) && str(e.Data, "kind") == "executor" && str(e.Data, "hold") == "":
			// a node the scheduler held back (no slot, no memory, a region in use, ...): unreleased, and asked again by a resume (CRW-1201)
			heldBack = true
		}
	}
	order, rows := callRows(events)
	held := map[string]bool{}
	for _, id := range order {
		cr := rows[id]
		if !isAgentCall(cr) {
			continue
		}
		info.Calls++
		for _, r := range cr {
			if r.Event == engine.EvCompleted || r.Event == engine.EvHeld || r.Event == engine.EvNullModel || r.Event == engine.EvRefused {
				info.Tokens += tokensOf(r.Data)
			}
		}
		switch classify(cr) {
		case callstate.RecoveryHold:
			info.Holds++
		case callstate.Unsettled:
			info.Unsettled++
		}
		if status, reason := lastTerminal(cr); status == engine.EvHeld && reason != "" {
			held[reason] = true
		}
	}
	for reason := range held {
		info.HeldReasons = append(info.HeldReasons, reason)
	}
	sort.Strings(info.HeldReasons)
	for _, q := range Questions(events) {
		if q.Current && q.Applied == "" {
			info.Waiting++
		}
	}
	switch {
	case live && info.Paused != "":
		info.State = StatePaused
	case live:
		info.State = StateRunning
	case lastFinish > lastStart && info.Status == engine.RunCompleted:
		info.State = StateCompleted
		switch {
		case info.Unsettled > 0 || info.Holds > 0 || info.Waiting > 0:
			info.State = StateHeld
		case len(info.ItemErrors) > 0 && !heldBack && !nodeStarted(events) && StartsNodes(rec, events):
			info.State = StateBlocked
		}
	case lastFinish > lastStart && info.ErrorKind == engine.ErrKindCancelled:
		info.State = StateCancelled
	case lastFinish > lastStart:
		info.State = StateFailed
	default:
		info.State = StateDead
	}
	return info
}

// evItemError is the row a join item that threw leaves (engine/scriptvm.go, itemFailed).
const evItemError = "item_error"

// itemKey is the key of a join item from its path (<parent>/<position>:<key>, the key percent-escaped, engine/paths.go).
func itemKey(path string) string {
	seg := path[strings.LastIndex(path, "/")+1:]
	key := seg[strings.Index(seg, ":")+1:]
	if k, err := url.PathUnescape(key); err == nil {
		return k
	}
	return key
}

func releasedRow(e engine.Event) bool {
	released, _ := e.Data["released"].(bool)
	return released
}

// nodeStarted says whether any process of the run started a node of its milestone: a crw.release released one to an executor, a
// crw.releaseChild released an issue child, or a crw.integrate landed one. The reading is the lane's results, never the thread or turn rows
// of the journal.
func nodeStarted(events []engine.Event) bool {
	for _, e := range events {
		if e.Kind != engine.KindCall || e.Event != engine.EvCompleted {
			continue
		}
		switch {
		case e.Op == ops.OpRelease && releasedRow(e), e.Op == ops.OpReleaseChild, e.Op == ops.OpIntegrate:
			return true
		}
	}
	return false
}

// ProjectLaneWorkflow is the built-in workflow that runs the issue nodes of a milestone.
const ProjectLaneWorkflow = "project-lane"

// laneWorkflows are the built-in workflows whose run is meant to start the nodes of a plan: the project lane, the issue lane of a child and
// the night batch.
var laneWorkflows = map[string]bool{ProjectLaneWorkflow: true, apply.IssueLane: true, NightBatchWorkflow: true}

// StartsNodes says whether a run is meant to start the nodes of a plan, so that a run of it that started none is blocked (CRW-1320): a run of
// a lane workflow, or a run whose arguments name a plan (the record's plan). The workflow is the record's, else the one the run's first
// started row names. Another script run (a judge panel, a scout) that fans out and has an item that threw never meant to start a node.
func StartsNodes(rec RunRecord, events []engine.Event) bool {
	if rec.PlanID != "" {
		return true
	}
	workflow := rec.Workflow
	for _, e := range events {
		if workflow != "" {
			break
		}
		if e.Kind == engine.KindRun && e.Event == "started" {
			workflow = str(e.Data, "workflow")
		}
	}
	return laneWorkflows[workflow]
}

// Blocked is the reading `crw flow show` prints and report.RunHook asks for when a run finishes (CRW-1320): whether Summarize shows the
// run of this record blocked, and the reason of each item that failed (<item>: <error>). The report and the shown state are this one
// reading.
func Blocked(rec RunRecord, events []engine.Event) (reasons []string, blocked bool) {
	info := Summarize(rec, events, false, 0)
	if info.State != StateBlocked {
		return nil, false
	}
	for _, ie := range info.ItemErrors {
		reasons = append(reasons, ie.Item+": "+oneLine(ie.Error))
	}
	return reasons, true
}

// recordedPolicy is the policy the run holds: what its latest start recorded, replaced by what a repin recorded after it, and the
// digest a repin replaced. Every start resets the repin: a start that read the digest the run already held repinned nothing, and a
// repin is journaled after the start it belongs to. unrecorded is true when the latest start predates the policy_source field, so
// the run's policy file is not known (a row that records an empty policy_source is the placeholder table, not unrecorded). The
// journal is only read.
func recordedPolicy(events []engine.Event) (path, digest, repinnedFrom string, unrecorded bool) {
	for _, e := range events {
		if e.Kind != engine.KindRun {
			continue
		}
		switch e.Event {
		case "started":
			_, recorded := e.Data["policy_source"]
			path, digest, repinnedFrom, unrecorded = str(e.Data, "policy_source"), str(e.Data, "policy_digest"), "", !recorded
		case engine.EvPolicyRepinned:
			path, digest, repinnedFrom, unrecorded = str(e.Data, "new_source"), str(e.Data, "new"), str(e.Data, "old"), false
		}
	}
	return path, digest, repinnedFrom, unrecorded
}

// Detail builds the tree of a run: its phases with the agent calls made in each, what each plan node has so far, and the
// questions it asked.
func Detail(rec RunRecord, events []engine.Event, live bool, pid int) RunDetail {
	d := RunDetail{RunInfo: Summarize(rec, events, live, pid), Phases: []PhaseNode{}, Nodes: []NodeInfo{}, Questions: Questions(events)}
	d.PolicyPath, d.PolicyDigest, d.PolicyRepinnedFrom, d.PolicyUnrecorded = recordedPolicy(events)
	order, rows := callRows(events)
	// the phase a call was made in is the last phase row before its first row
	phaseOf := map[string]string{}
	title := ""
	seen := map[string]bool{}
	for _, e := range events {
		if e.Kind == engine.KindFlow && e.Event == "phase" {
			title = str(e.Data, "title")
			continue
		}
		if e.Kind == engine.KindCall && e.CallID != "" && !seen[e.CallID] {
			seen[e.CallID] = true
			phaseOf[e.CallID] = title
		}
	}
	nodes := map[string]*NodeInfo{}
	var nodeOrder []string
	node := func(id string) *NodeInfo {
		if id == "" {
			return nil
		}
		n, ok := nodes[id]
		if !ok {
			n = &NodeInfo{Node: id}
			nodes[id] = n
			nodeOrder = append(nodeOrder, id)
		}
		return n
	}
	byPhase := map[string]*PhaseNode{}
	var phaseOrder []string
	phase := func(title string) *PhaseNode {
		p, ok := byPhase[title]
		if !ok {
			p = &PhaseNode{Title: title, Agents: []AgentNode{}}
			byPhase[title] = p
			phaseOrder = append(phaseOrder, title)
		}
		return p
	}
	// phases appear in the order the script opened them, also those that made no call
	for _, e := range events {
		if e.Kind == engine.KindFlow && e.Event == "phase" {
			phase(str(e.Data, "title"))
		}
	}
	for _, id := range order {
		cr := rows[id]
		if isAgentCall(cr) {
			a := AgentNode{CallID: id, Role: "", Tokens: 0}
			for _, r := range cr {
				if r.Label != "" {
					a.Label = r.Label
				}
				if r.Event == engine.EvPrepared {
					a.Role, a.Node, a.Attempt = str(r.Data, "role"), str(r.Data, "node"), num(r.Data, "attempt")
				}
				if t := str(r.Data, "thread_id"); t != "" {
					a.ThreadID = t
				}
				if r.Event == engine.EvCompleted || r.Event == engine.EvHeld || r.Event == engine.EvNullModel || r.Event == engine.EvRefused {
					a.Tokens += tokensOf(r.Data)
				}
			}
			a.Status, a.Reason = lastTerminal(cr)
			a.RecoveryHold = classify(cr) == callstate.RecoveryHold
			phase(phaseOf[id]).Agents = append(phase(phaseOf[id]).Agents, a)
			if n := node(a.Node); n != nil {
				n.Tokens += a.Tokens
				n.RecoveryHold = n.RecoveryHold || a.RecoveryHold
				n.Held = n.Held || a.Status == engine.EvHeld
				if a.ThreadID != "" && !contains(n.Threads, a.ThreadID) {
					n.Threads = append(n.Threads, a.ThreadID)
				}
			}
			continue
		}
		// an operation of the lane: its completed row records what a node produced
		for _, r := range cr {
			if r.Event != engine.EvCompleted {
				continue
			}
			n := node(str(r.Data, "node"))
			if n == nil {
				continue
			}
			switch r.Op {
			case ops.OpCommit:
				if c := str(r.Data, "commit"); c != "" && !contains(n.Commits, c) {
					n.Commits = append(n.Commits, c)
				}
			case ops.OpReview:
				n.Reviews = append(n.Reviews, ReviewLink{Call: str(r.Data, "call"), Head: str(r.Data, "head"), State: str(r.Data, "state"), Verdict: str(r.Data, "verdict")})
			case ops.OpAccept:
				n.Accepted = str(r.Data, "acceptance_id")
			case ops.OpIntegrate:
				n.Integrated = str(r.Data, "batch_commit")
			}
		}
	}
	for _, t := range phaseOrder {
		d.Phases = append(d.Phases, *byPhase[t])
	}
	for _, id := range nodeOrder {
		d.Nodes = append(d.Nodes, *nodes[id])
	}
	return d
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// JSON renders v as indented JSON with a trailing newline.
func JSON(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return append(b, '\n')
}

// oneLine collapses white space, so a value shows on one line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
