package fakehost

import "fmt"

// This file models the subagent threads of multi-agent V1 (spawn_agent), as the host shows them to a client that reads
// them: what a session run sees of the role subagents a project orchestrator spawns (CRW-1316, CRW-1332). Facts a live
// 0.154.0 host gave on 2026-10-14 (a read-only probe of a finished subagent thread; evidence under
// /state/crw/crw-lane-session-host-1316/evidence/probe):
//
//   - thread/read of a subagent names parentThreadId, threadSource "subagent", the source
//     {"subAgent": {"thread_spawn": {parent_thread_id, depth, agent_path, agent_nickname, agent_role}}}, agentRole,
//     agentNickname, the configured model and reasoningEffort, the cwd (the parent's) and path (its rollout);
//   - a finished subagent reads notLoaded, or idle while its parent session keeps it loaded (StayLoaded);
//   - thread/turns/list lists its turns newest first; the summary view (the default) holds the first userMessage and the
//     last agentMessage, the full view every item;
//   - the parent's turn holds a collabAgentToolCall item (tool spawnAgent) whose id is the tool use id the spawn hook
//     records and whose receiverThreadIds name the subagent; its prompt is the spawn message.
//
// A running subagent does not survive an App Server restart (10-13 21:29: the rollout ends without a task_complete and
// the parent's wait answered not_found). Restart models that: the running turn never ends, the thread reads notLoaded, and
// the cut turn reads interrupted unless LostTurnStatus names another status (what a 0.154 host reports for a cut turn was
// not probed live; a test that depends on it says which it assumes).

// SubagentSourceKind is the ThreadSourceKind a subagent thread is listed under (thread/list sourceKinds).
const SubagentSourceKind = "subAgentThreadSpawn"

// SubagentSpec describes a subagent an owner thread spawns.
type SubagentSpec struct {
	// Parent is the owner thread; it must exist.
	Parent string
	// ParentTurn is the owner turn the collabAgentToolCall item goes to ("": the owner's turn in progress, else its last).
	ParentTurn string
	// ToolUseID is the id of that item ("": call-<subagent id>).
	ToolUseID string
	// AgentType is the agentRole ("" reads null); Nickname the agentNickname.
	AgentType, Nickname string
	// Model and Effort are the configured pair ("": the parent's); Cwd the working directory ("": the parent's).
	Model, Effort, Cwd string
	// Message is the spawn message: the subagent's first user message and the item's prompt.
	Message string
	// RolloutPath is Thread.path ("" reads null).
	RolloutPath string
	// Plan decides how the first turn runs, as a TurnPlan decides a turn/start (StartError aside); an empty Message is "ok".
	Plan TurnPlan
	// StayLoaded keeps the subagent idle after its turn ends instead of notLoaded.
	StayLoaded bool
}

// Spawn makes a subagent thread of spec.Parent, records the spawn in the owner's turn and starts the subagent's first turn.
// It returns the subagent's thread id.
func (t *Threads) Spawn(spec SubagentSpec) string {
	t.mu.Lock()
	parent, ok := t.byID[spec.Parent]
	if !ok {
		t.mu.Unlock()
		panic(fmt.Sprintf("fakehost: Spawn: no parent thread %q", spec.Parent))
	}
	owner := parent.active
	if spec.ParentTurn != "" || owner == nil {
		owner = nil
		for _, tu := range parent.turns {
			if spec.ParentTurn == "" || tu.id == spec.ParentTurn {
				owner = tu
			}
		}
	}
	if owner == nil {
		t.mu.Unlock()
		panic(fmt.Sprintf("fakehost: Spawn: parent thread %s has no turn %q", spec.Parent, spec.ParentTurn))
	}
	t.nextID++
	th := &threadState{
		id: t.threadID(), cwd: spec.Cwd, model: spec.Model, effort: spec.Effort, sandbox: parent.sandbox, config: map[string]any{},
		start: map[string]any{}, created: t.createdAt(), source: SubagentSourceKind, parent: parent.id, subagent: true,
		agentRole: spec.AgentType, nickname: spec.Nickname, rolloutPath: spec.RolloutPath, depth: parent.depth + 1, unloadIdle: !spec.StayLoaded,
	}
	if th.cwd == "" {
		th.cwd = parent.cwd
	}
	if th.model == "" {
		th.model = parent.model
	}
	if th.effort == "" {
		th.effort = parent.effort
	}
	t.all = append(t.all, th)
	t.byID[th.id] = th
	toolUse := spec.ToolUseID
	if toolUse == "" {
		toolUse = "call-" + th.id
	}
	owner.items = append(owner.items, map[string]any{
		"type": "collabAgentToolCall", "id": toolUse, "tool": "spawnAgent", "status": "completed", "senderThreadId": parent.id,
		"receiverThreadIds": []any{th.id}, "prompt": spec.Message, "model": nullable(spec.Model), "reasoningEffort": nullable(spec.Effort),
		"agentsStates": map[string]any{th.id: map[string]any{"status": "pendingInit", "message": nil}},
	})
	plan := spec.Plan
	if plan.Message == "" {
		plan.Message = "ok"
	}
	tu := &turnState{
		id: fmt.Sprintf("turn-%s-1", th.id), status: "inProgress", plan: plan, params: map[string]any{}, text: spec.Message,
		startedAt: t.tick(), interrupt: make(chan struct{}), cut: make(chan struct{}), detached: true,
	}
	tu.items = append(tu.items, map[string]any{"type": "userMessage", "id": "u-" + tu.id, "clientId": nil,
		"content": []any{map[string]any{"type": "text", "text": spec.Message, "text_elements": []any{}}}})
	th.turns = append(th.turns, tu)
	th.active = tu
	t.mu.Unlock()
	go t.run(nil, th, tu)
	return th.id
}

// LostTurnStatus sets the status a turn cut by Restart reads afterwards ("" restores interrupted).
func (t *Threads) LostTurnStatus(status string) {
	t.mu.Lock()
	t.lostStatus = status
	t.mu.Unlock()
}

// Restart is an App Server restart: every turn in progress is lost (it never ends, and reads the LostTurnStatus), every
// thread reads notLoaded until a resume or a new turn loads it again, no connection follows any thread, and every
// connection is dropped. The threads, their finished turns and their items stay: they are the host's persisted history.
func (t *Threads) Restart() {
	t.mu.Lock()
	status := t.lostStatus
	if status == "" {
		status = "interrupted"
	}
	for _, th := range t.all {
		if tu := th.active; tu != nil {
			tu.lost, tu.status = true, status
			if status != "inProgress" {
				tu.endedAt = t.tick()
			}
			if tu.cut != nil {
				close(tu.cut)
			}
			th.active = nil
		}
		th.unloaded = true
	}
	t.subs = map[string][]*Conn{}
	t.mu.Unlock()
	t.server.DropConnections()
}

// DropConnections closes every client connection without answering; the server keeps listening.
func (s *Server) DropConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for conn := range s.conns {
		_ = conn.CloseNow()
	}
}

// subagentFields puts the members of a subagent thread into its Thread object.
func subagentFields(th *threadState, object map[string]any) {
	object["threadSource"] = "subagent"
	object["agentRole"] = nullable(th.agentRole)
	object["agentNickname"] = nullable(th.nickname)
	object["path"] = nullable(th.rolloutPath)
	object["source"] = map[string]any{"subAgent": map[string]any{"thread_spawn": map[string]any{
		"parent_thread_id": th.parent, "depth": th.depth, "agent_path": nil, "agent_nickname": nullable(th.nickname), "agent_role": nullable(th.agentRole),
	}}}
}

// summaryItems is a turn's summary view: its first userMessage and its last agentMessage.
func summaryItems(items []map[string]any) []any {
	out := []any{}
	for _, it := range items {
		if it["type"] == "userMessage" {
			out = append(out, it)
			break
		}
	}
	for i := len(items) - 1; i >= 0; i-- {
		if items[i]["type"] == "agentMessage" {
			out = append(out, items[i])
			break
		}
	}
	return out
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
