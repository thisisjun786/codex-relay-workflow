package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// CRW-1331 (CRW-1316 S2a): the engine's session calls. Each test drives the run against the fakes of session_fake_test.go: the
// run publishes steps on a real FileStepBus, the fake owner thread spawns subagents on the fake host and reports them, and the run
// binds, harvests and records them.

const sessionRoles = `
const arch = await agent.call('design the change', { agentType: 'architect', label: 'arch' })
const rev = await agent.call('review the head', { agentType: 'reviewer', label: 'rev', head: 'h-review', schema: VERDICT })
const exe = await agent.call('build node n1', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
const exp = await agent.call('look around', { agentType: 'explorer', label: 'exp' })
const qa = await agent.call('capture the surface', { agentType: 'executor', label: 'qa', qa: true, head: 'h-gate', schema: RECEIPT })
return [arch.status, rev.status, exe.status, exp.status, qa.status]
`

// A session call of every role publishes a step for the owner thread, binds the subagent the dispatch report names with the rows
// relay acceptance reads (thread_started with the host-read pair, cwd P, host_cwd, attempt, via session, and turn_started in the
// same batch) and harvests the answer into the existing validation and terminal rows. No thread or turn is started through the
// backend.
func TestSessionCallEveryRole(t *testing.T) {
	k := newSessionKit(t)
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
		if st.Role == "executor" && !strings.Contains(st.Workspace, "/call/") {
			return orchAnswer{edit: "P:" + st.Op}
		}
		return orchAnswer{}
	}
	res := k.h.run(sessionRoles)
	got := value[[]string](t, res)
	if strings.Join(got, ",") != "completed,completed,completed,completed,completed" {
		t.Fatalf("statuses %v", got)
	}
	if n := len(k.placer.Requests()); n != 0 {
		t.Errorf("the backend got %d thread or turn requests; a session run sends none", n)
	}
	var started Event
	for _, e := range k.h.journal.Events() {
		if e.Kind == KindRun && e.Event == "started" {
			started = e
		}
	}
	if rowStr(started.Data, "backend") != BackendSession {
		t.Errorf("the started row records backend %q, want session", rowStr(started.Data, "backend"))
	}
	pairs := map[string]Pair{"arch": {"claude-fake-opus", "high"}, "rev": {"gpt-fake-sol", "high"}, "exe": {"claude-fake-haiku", "xhigh"}, "exp": {"claude-fake-haiku", "high"}, "qa": {"claude-fake-haiku", "xhigh"}}
	types := map[string]string{"arch": "architect", "rev": "reviewer", "exe": "executor", "exp": "explorer", "qa": "executor"}
	for label, pair := range pairs {
		rows := k.rowsOf(label)
		if got := eventsOf(rows); got != "prepared,step_published,thread_started,turn_started,completed" {
			t.Errorf("%s: rows %s", label, got)
			continue
		}
		prep, step, ts, turn := rows[0], rows[1], rows[2], rows[3]
		if rowStr(prep.Data, "backend") != BackendSession {
			t.Errorf("%s: prepared backend %q", label, rowStr(prep.Data, "backend"))
		}
		alloc, _ := prep.Data["allocation"].(map[string]any)
		p := rowStr(alloc, "path")
		if p == "" || p == orchCwd {
			t.Errorf("%s: the call has no directory of its own: %v", label, alloc)
		}
		if rowStr(ts.Data, "cwd") != p || rowStr(ts.Data, "host_cwd") != orchCwd || rowStr(ts.Data, "via") != "session" || rowInt(ts.Data, "attempt") != 1 ||
			rowStr(ts.Data, "parent_thread_id") != sessionOwner || rowStr(ts.Data, "agent_role") != types[label] {
			t.Errorf("%s: thread_started %v (P %s)", label, ts.Data, p)
		}
		if rowStr(ts.Data, "model") != pair.Model || rowStr(ts.Data, "effort") != pair.Effort {
			t.Errorf("%s: thread_started pair %s/%s, want %v", label, rowStr(ts.Data, "model"), rowStr(ts.Data, "effort"), pair)
		}
		op := prep.CallID + "/t1"
		if rowStr(step.Data, "op") != op || rowStr(step.Data, "marker") != op+"-a1" || rowInt(step.Data, "attempt") != 1 || rowStr(step.Data, "action") != StepSpawn ||
			rowStr(step.Data, "workspace") != p || rowStr(step.Data, "packet") == "" || rowStr(step.Data, "input_digest") == "" {
			t.Errorf("%s: step_published %v", label, step.Data)
		}
		if rowStr(turn.Data, "op") != op || rowStr(turn.Data, "turn_id") == "" || rowInt(turn.Data, "attempt") != 1 {
			t.Errorf("%s: turn_started %v", label, turn.Data)
		}
		if ts.Seq+1 != turn.Seq {
			t.Errorf("%s: thread_started and turn_started are not one batch (seq %d, %d)", label, ts.Seq, turn.Seq)
		}
		packet, err := os.ReadFile(rowStr(step.Data, "packet"))
		if err != nil {
			t.Fatalf("%s: packet: %v", label, err)
		}
		if !strings.Contains(string(packet), "Your workspace is "+p) || !strings.Contains(string(packet), "work only under the absolute path "+p) {
			t.Errorf("%s: the packet does not name P %s:\n%s", label, p, packet)
		}
		if strings.Contains(string(packet), "Native spawn") {
			t.Errorf("%s: the packet of a leaf subagent carries the native-spawn section", label)
		}
	}
	// the reviewer reads the head under review, not the owner thread's checkout
	if alloc, _ := k.rowsOf("rev")[0].Data["allocation"].(map[string]any); rowStr(alloc, "start_commit") != "h-review" {
		t.Errorf("the reviewer's directory is not at the head under review: %v", alloc)
	}
	// the executor's pending tree is the snapshot of P, never of the owner's cwd
	if done := k.rowsOf("exe")[4]; rowStr(done.Data, "pending_tree") != "P:"+done.CallID+"/t1-a1" {
		t.Errorf("executor pending_tree %q", rowStr(done.Data, "pending_tree"))
	}
	// the QA capture call records the source of its own directory
	if done := k.rowsOf("qa")[4]; done.Data["qa_source"] == nil {
		t.Errorf("the QA call recorded no qa_source: %v", done.Data)
	}
	// the message is short and names the marker, the role, P and the packet
	for _, st := range k.orch.spawned() {
		want := "crw-flow-op: " + st.Op + "\nCRW-ROLE: " + st.Role + "\n\nYour shell starts in a directory that is NOT your workspace. Your workspace is " + st.Workspace +
			": run every command with workdir " + st.Workspace + " and edit only by absolute paths under " + st.Workspace + ".\nRead the task packet " + st.Packet +
			" in full and follow it exactly. You are a leaf subagent: never spawn agents.\nYour final message is exactly the JSON object the packet names, and nothing else."
		if st.Message != want {
			t.Errorf("message of %s:\n%s\nwant\n%s", st.Op, st.Message, want)
		}
	}
	// every bound step is gone and every subagent is closed after its harvest
	steps, _ := k.bus.Steps()
	waitFor(t, "every subagent closed", func() bool { return len(k.orch.closedAgents()) == 5 })
	for _, st := range steps {
		if st.Action != StepClose {
			t.Errorf("a step is still published: %+v", st)
		}
	}
}

// waitFor polls a condition for up to five seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A schema repair goes to the same subagent with send_input: one thread_started, a second step and turn_started for k=2.
func TestSessionRepairThroughSendInput(t *testing.T) {
	k := newSessionKit(t)
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
		if st.K == 1 {
			return orchAnswer{text: "this is not JSON"}
		}
		return orchAnswer{}
	}
	res := k.h.run(`const r = await agent.call('review', { agentType: 'reviewer', label: 'rev', head: 'h1', schema: VERDICT })
return [r.status, r.meta.repairUsed]`)
	if got := value[[]any](t, res); got[0] != "completed" || got[1] != true {
		t.Fatalf("result %v", got)
	}
	rows := k.rowsOf("rev")
	if got := eventsOf(rows); got != "prepared,step_published,thread_started,turn_started,step_published,turn_started,completed" {
		t.Fatalf("rows %s", got)
	}
	second := rows[4]
	agent := rowStr(rows[2].Data, "thread_id")
	if rowStr(second.Data, "action") != StepSendInput || rowStr(second.Data, "agent_id") != agent || rowInt(second.Data, "k") != 2 || rowInt(second.Data, "attempt") != 1 || second.Data["repair"] != true {
		t.Errorf("the repair step %v (agent %s)", second.Data, agent)
	}
	in := k.orch.sentInput()
	if len(in) != 1 || in[0].AgentID != agent || !strings.HasPrefix(in[0].Message, "crw-flow-op: "+rows[0].CallID+"/t2-a1\n") || !strings.Contains(in[0].Message, "did not satisfy the output schema") {
		t.Errorf("send_input steps %+v", in)
	}
	if n := len(k.orch.spawned()); n != 1 {
		t.Errorf("%d spawns, want 1", n)
	}
}

// The answer is the final_answer message, or the phase-null message of a provider that sets none; a trailing JSON object is taken
// out of prose before it, and the raw text and the canonical bytes are both recorded. A commentary message is no answer.
func TestSessionHarvestSelectsTheAnswer(t *testing.T) {
	k := newSessionKit(t)
	valid := `{"verdict":"verified","summary":"ok","criteria":[],"findings":[]}`
	k.orch.answer = func(st Step, packet string, _ int) orchAnswer {
		schema := packetSchema(packet)
		doc := defaultMessage("", schema)
		_ = valid
		switch {
		case strings.Contains(packet, "prose first"):
			return orchAnswer{text: "Here is the verdict you asked for.\n\n" + doc}
		case strings.Contains(packet, "no phase"):
			return orchAnswer{text: doc, rawPhase: true}
		case strings.Contains(packet, "commentary") && st.K == 1:
			return orchAnswer{text: doc, phase: "commentary"}
		}
		return orchAnswer{}
	}
	res := k.h.run(`const a = await agent.call('prose first', { agentType: 'reviewer', label: 'prose', head: 'h1', schema: VERDICT })
const b = await agent.call('no phase', { agentType: 'reviewer', label: 'nophase', head: 'h1', schema: VERDICT })
const c = await agent.call('commentary only', { agentType: 'reviewer', label: 'comm', head: 'h1', schema: VERDICT })
return [a.status, a.meta.repairUsed, b.status, b.meta.repairUsed, c.status, c.meta.repairUsed]`)
	got := value[[]any](t, res)
	if fmt.Sprint(got) != "[completed false completed false completed true]" {
		t.Fatalf("results %v", got)
	}
	done := k.rowsOf("prose")[4]
	var canon any
	if err := json.Unmarshal([]byte(rowStr(done.Data, "result")), &canon); err != nil {
		t.Fatalf("result %q: %v", rowStr(done.Data, "result"), err)
	}
	if !strings.HasPrefix(rowStr(done.Data, "raw_text"), "Here is the verdict") || done.Data["extracted"] != true {
		t.Errorf("the raw text is not recorded beside the extracted result: %v", done.Data)
	}
	if rowStr(done.Data, "result_digest") != digestJSON([]byte(rowStr(done.Data, "result"))) {
		t.Errorf("result_digest is not the digest of the canonical result")
	}
}

// A failed subagent turn is a TurnOutcome.Failed: a usage limit holds the call and pauses the run as a host turn does, and a
// context window that was exceeded is a null_model.
func TestSessionFailedTurnIsAHostFailure(t *testing.T) {
	k := newSessionKit(t)
	k.orch.answer = func(st Step, packet string, _ int) orchAnswer {
		if strings.Contains(packet, "too long") {
			return orchAnswer{fail: "contextWindowExceeded"}
		}
		return orchAnswer{fail: "usageLimitExceeded"}
	}
	res := k.h.run(`const a = await agent.call('too long', { agentType: 'explorer', label: 'long' })
const b = await agent.call('quota', { agentType: 'explorer', label: 'quota' })
const c = await agent.call('after the pause', { agentType: 'explorer', label: 'after' })
return [a.status, a.reason, b.status, b.reason, c.status, c.reason]`)
	got := value[[]any](t, res)
	if fmt.Sprint(got) != "[null_model context_window_exceeded held host_infra_usageLimitExceeded held engine_paused]" {
		t.Fatalf("results %v", got)
	}
	if len(flowRows(k.h.journal, EvEscalation)) != 1 {
		t.Errorf("escalations %v", flowRows(k.h.journal, EvEscalation))
	}
	if got := eventsOf(k.rowsOf("after")); got != "prepared,held" {
		t.Errorf("a call after the pause published a step: %s", got)
	}
}

// A pause interrupts the bound subagent's turn and waits until the thread is no longer active before the held row.
func TestSessionPauseInterruptsTheSubagent(t *testing.T) {
	k := newSessionKit(t)
	k.orch.answer = func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }
	ctl := &Control{}
	k.h.cfg.Control = ctl
	go func() {
		for i := 0; i < 5000; i++ {
			if len(k.host.agent("agent-1").turns) > 0 && len(rowsNamed(k.h.journal.Events(), EvTurnStarted)) > 0 {
				ctl.Pause("operator", "test")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	res := k.h.run(`const r = await agent.call('work', { agentType: 'explorer', label: 'w' })
return [r.status, r.reason]`)
	if got := value[[]any](t, res); fmt.Sprint(got) != "[held engine_paused]" {
		t.Fatalf("result %v", got)
	}
	if got := k.host.interrupted(); len(got) != 1 || got[0] != "agent-1/agent-1-turn1" {
		t.Errorf("interrupts %v", got)
	}
	rows := k.rowsOf("w")
	held := rows[len(rows)-1]
	if rowStr(held.Data, "input_state") != "delivered" || len(rowsNamed(rows, EvInterruptRequested)) != 1 {
		t.Errorf("rows %s, held %v", eventsOf(rows), held.Data)
	}
	if a := k.host.agent("agent-1"); a.Status == "active" {
		t.Errorf("the held row was written while the subagent was still active")
	}
}

// A subagent whose thread the host no longer has loaded, with its turn unchanged for LostAfter, is a lost attempt: the call writes
// session_attempt_lost, closes the old agent and spawns a new attempt in the same allocation, whose packet says the previous
// attempt was cut off. A subagent that is slow but active is never lost.
func TestSessionLostAttemptGoesOnInTheSameAllocation(t *testing.T) {
	k := newSessionKit(t)
	k.orch.answer = func(st Step, packet string, _ int) orchAnswer {
		switch {
		case strings.Contains(packet, "slow") && st.Attempt == 1:
			return orchAnswer{after: 150 * time.Millisecond}
		case strings.Contains(packet, "build") && st.Attempt == 1:
			return orchAnswer{lose: true, edit: "half"}
		}
		return orchAnswer{edit: "P:" + st.Op}
	}
	res := k.h.run(`const a = await agent.call('build node n1', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
const b = await agent.call('slow but alive', { agentType: 'explorer', label: 'slow' })
return [a.status, b.status]`)
	if got := value[[]string](t, res); strings.Join(got, ",") != "completed,completed" {
		t.Fatalf("results %v", got)
	}
	rows := k.rowsOf("exe")
	if got := eventsOf(rows); got != "prepared,step_published,thread_started,turn_started,session_attempt_lost,step_published,thread_started,turn_started,completed" {
		t.Fatalf("rows %s", got)
	}
	lost := rows[4]
	if rowInt(lost.Data, "attempt") != 1 || rowStr(lost.Data, "agent_id") != "agent-1" || rowStr(lost.Data, "host_status") != "notLoaded" || rowStr(lost.Data, "reason") == "" {
		t.Errorf("session_attempt_lost %v", lost.Data)
	}
	second := rows[5]
	if rowInt(second.Data, "attempt") != 2 || rowStr(second.Data, "marker") != rows[0].CallID+"/t1-a2" || rowStr(second.Data, "action") != StepSpawn {
		t.Errorf("the new attempt's step %v", second.Data)
	}
	packet, _ := os.ReadFile(rowStr(second.Data, "packet"))
	if !strings.Contains(string(packet), "git -C /w/n1/a1 status") {
		t.Errorf("the new executor attempt's packet does not say to look at the worktree first:\n%s", packet)
	}
	if n := len(rowsNamed(rows, EvPrepared)); n != 1 {
		t.Errorf("%d prepared rows: the attempts do not share one allocation", n)
	}
	if got := k.placer.establishedPaths(); countOf(got, "/w/n1/a1") != 1 {
		t.Errorf("the worktree was made twice: %v", got)
	}
	if ts := rowsNamed(rows, EvThreadStarted); rowInt(ts[0].Data, "attempt") != 1 || rowInt(ts[1].Data, "attempt") != 2 {
		t.Errorf("thread_started attempts %v %v", ts[0].Data, ts[1].Data)
	}
	waitFor(t, "the lost agent closed", func() bool { return slices.Contains(k.orch.closedAgents(), "agent-1") })
	if got := eventsOf(k.rowsOf("slow")); got != "prepared,step_published,thread_started,turn_started,completed" {
		t.Errorf("a slow subagent was judged lost: %s", got)
	}
}

// A turn that ended interrupted without an error on a thread the host shows notLoaded is lost only when a second read still shows
// the thread gone: a thread the host has back on that read is not lost (its interrupted turn is the call's held outcome), and one
// that stays gone is lost and spawns the next attempt.
func TestSessionLostNeedsASecondRead(t *testing.T) {
	k := newSessionKit(t)
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
		if st.Attempt == 1 {
			return orchAnswer{hang: true}
		}
		return orchAnswer{}
	}
	k.orch.onSpawn = func(st Step, agent string) {
		if st.Attempt != 1 {
			return
		}
		back := 0
		if agent == "agent-1" {
			back = 1
		}
		go func() {
			for i := 0; i < 5000; i++ {
				for _, r := range rowsNamed(k.h.journal.Events(), EvTurnStarted) {
					if rowStr(r.Data, "turn_id") == agent+"-turn1" {
						k.host.set(agent, func(a *fakeAgent, t *SessionTurn) {
							a.Status, a.back = "notLoaded", back
							t.Status, t.UpdatedAt = "interrupted", time.Now()
						})
						return
					}
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	res := k.h.run(`const a = await agent.call('blip', { agentType: 'explorer', label: 'blip' })
const b = await agent.call('gone', { agentType: 'explorer', label: 'gone' })
return [a.status, a.reason, b.status]`)
	if got := value[[]any](t, res); fmt.Sprint(got) != "[held host_interrupted_external completed]" {
		t.Fatalf("results %v", got)
	}
	if got := eventsOf(k.rowsOf("blip")); got != "prepared,step_published,thread_started,turn_started,held" {
		t.Errorf("a thread the host had back on the second read was lost: %s", got)
	}
	if got := eventsOf(k.rowsOf("gone")); got != "prepared,step_published,thread_started,turn_started,session_attempt_lost,step_published,thread_started,turn_started,completed" {
		t.Errorf("a thread that stayed gone was not lost: %s", got)
	}
}

// A dispatch report for an attempt that ended (it was lost) is answered close; a report that names no published step (an early
// one) is answered close too; a second subagent reported for an attempt that is bound already is answered close. None of them is
// bound.
func TestSessionStaleAndEarlyReports(t *testing.T) {
	k := newSessionKit(t)
	var once sync.Once
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
		if st.Attempt == 1 {
			return orchAnswer{lose: true}
		}
		return orchAnswer{after: 100 * time.Millisecond}
	}
	k.orch.onSpawn = func(st Step, agent string) {
		if st.Attempt != 2 {
			return
		}
		once.Do(func() {
			go func() {
				for i := 0; i < 5000 && len(rowsNamed(k.h.journal.Events(), EvThreadStarted)) < 2; i++ {
					time.Sleep(time.Millisecond)
				}
				_ = k.bus.Report(DispatchReport{ID: "late-a1", Run: st.Run, Call: st.Call, K: 1, Attempt: 1, Agent: "agent-1"})
				_ = k.bus.Report(DispatchReport{ID: "nostep", Run: st.Run, Call: st.Call, K: 7, Attempt: 1, Agent: "agent-x"})
				_ = k.bus.Report(DispatchReport{ID: "twice", Run: st.Run, Call: st.Call, K: 1, Attempt: 2, Agent: "agent-other"})
			}()
		})
	}
	res := k.h.run(`const r = await agent.call('work', { agentType: 'explorer', label: 'w' })
return r.status`)
	if got := value[string](t, res); got != "completed" {
		t.Fatalf("status %s", got)
	}
	for id, want := range map[string]string{"late-a1": ReplyClose, "nostep": ReplyClose, "twice": ReplyClose} {
		var r DispatchReply
		waitFor(t, "the reply to "+id, func() bool {
			var ok bool
			r, ok, _ = k.bus.ReadReply(id)
			return ok
		})
		if r.Status != want {
			t.Errorf("%s: reply %+v, want %s", id, r, want)
		}
		if id == "nostep" && r.Close != "agent-x" {
			t.Errorf("the early report's reply names no subagent to close: %+v", r)
		}
	}
	if ts := rowsNamed(k.rowsOf("w"), EvThreadStarted); len(ts) != 2 || rowStr(ts[1].Data, "thread_id") != "agent-2" {
		t.Errorf("thread_started rows %v", ts)
	}
}

// A subagent whose configured pair is not the role's is not bound: the report is answered pair_mismatch with the pair to spawn
// with, the step stays, and the subagent spawned again with the right pair is bound.
func TestSessionPairMismatchIsNotBound(t *testing.T) {
	k := newSessionKit(t)
	wrong := Pair{"claude-fake-haiku", "low"}
	var spawnsM sync.Mutex
	spawns := 0
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
		spawnsM.Lock()
		defer spawnsM.Unlock()
		spawns++
		if spawns == 1 {
			return orchAnswer{pair: &wrong}
		}
		return orchAnswer{}
	}
	var mu sync.Mutex
	retried := false
	go func() {
		for i := 0; i < 5000; i++ {
			r, ok, _ := k.bus.ReadReply(firstReportID(k.orch))
			if ok && r.Status == ReplyPairMismatch {
				mu.Lock()
				retried = true
				mu.Unlock()
				k.orch.forget()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	res := k.h.run(`const r = await agent.call('look', { agentType: 'explorer', label: 'x' })
return r.status`)
	if got := value[string](t, res); got != "completed" {
		t.Fatalf("status %s", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if !retried {
		t.Fatalf("no pair_mismatch reply")
	}
	r, _, _ := k.bus.ReadReply(firstReportID(k.orch))
	if r.Close != "agent-1" || !strings.Contains(r.Detail, "claude-fake-haiku/high") {
		t.Errorf("pair_mismatch reply %+v", r)
	}
	ts := rowsNamed(k.rowsOf("x"), EvThreadStarted)
	if len(ts) != 1 || rowStr(ts[0].Data, "thread_id") != "agent-2" || rowStr(ts[0].Data, "effort") != "high" {
		t.Errorf("thread_started %v", ts)
	}
}

// Since the role helper snapshot (CRW-1330) a role can name a model and leave its effort undecided: the step names the model and
// no effort, and the host runs the subagent at an effort of its own. That subagent is bound on its model alone: thread_started
// records the effort the host applied as the call's pair with effort_unchecked (not pair_unchecked), and a continuation of the
// call's handle is not refused for that effort.
func TestSessionUndecidedEffortIsNotCompared(t *testing.T) {
	k := newSessionKit(t)
	pol := FakePolicy()
	exp := pol.Roles["explorer"]
	exp.Pair = Pair{Model: "claude-fake-haiku"}
	pol.Roles["explorer"] = exp
	k.h.cfg.Policy = pol
	applied := Pair{"claude-fake-haiku", "medium"}
	k.orch.answer = func(Step, string, int) orchAnswer { return orchAnswer{pair: &applied} }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	var mu sync.Mutex
	mismatch := ""
	done := make(chan struct{})
	defer func() {
		cancel()
		<-done
	}()
	// a pair_mismatch reply is never followed by another spawn here, so the first one ends the run at once
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			k.orch.mu.Lock()
			reps := append([]DispatchReport(nil), k.orch.reports...)
			k.orch.mu.Unlock()
			for _, rep := range reps {
				if r, ok, _ := k.bus.ReadReply(rep.ID); ok && r.Status == ReplyPairMismatch {
					mu.Lock()
					mismatch = r.Detail
					mu.Unlock()
					cancel()
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()
	res, err := Run(ctx, k.h.load(`const r = await agent.call('look', { agentType: 'explorer', label: 'x' })
const more = await agent.call('look again', { resume: r, label: 'more' })
return [r.status, more.status]`), k.h.cfg)
	mu.Lock()
	detail := mismatch
	mu.Unlock()
	if detail != "" {
		t.Fatalf("the undecided effort was compared: the report was answered %q", detail)
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := value[[]string](t, res); strings.Join(got, ",") != "completed,completed" {
		t.Fatalf("statuses %v (the continuation's rows: %v)", got, k.rowsOf("more"))
	}
	for _, label := range []string{"x", "more"} {
		rows := k.rowsOf(label)
		if got := eventsOf(rows); got != "prepared,step_published,thread_started,turn_started,completed" {
			t.Errorf("%s: rows %s", label, got)
			continue
		}
		step, ts := rows[1], rows[2]
		if rowStr(step.Data, "model") != applied.Model || rowStr(step.Data, "effort") != "" {
			t.Errorf("%s: step_published names %s/%s, want the role's model and no effort", label, rowStr(step.Data, "model"), rowStr(step.Data, "effort"))
		}
		if rowStr(ts.Data, "model") != applied.Model || rowStr(ts.Data, "effort") != applied.Effort || ts.Data["effort_unchecked"] != true || ts.Data["pair_unchecked"] != nil {
			t.Errorf("%s: thread_started %v, want the host-read pair %v with effort_unchecked", label, ts.Data, applied)
		}
	}
	if n := len(k.orch.spawned()); n != 2 {
		t.Errorf("%d spawns, want one subagent per call", n)
	}
	if info, _ := LookupCall(k.h.journal, k.rowsOf("x")[0].CallID); info.Configured != applied {
		t.Errorf("the call's pair %v, want the host-read %v", info.Configured, applied)
	}
}

// A decided effort is compared as before: a subagent of the role's model at another effort is answered pair_mismatch with the
// role's pair, the step stays, and the subagent spawned again with the role's pair is bound with no unchecked mark.
func TestSessionDecidedEffortThatDiffersIsAMismatch(t *testing.T) {
	k := newSessionKit(t)
	other := Pair{"claude-fake-haiku", "medium"}
	var spawnsM sync.Mutex
	spawns := 0
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
		spawnsM.Lock()
		defer spawnsM.Unlock()
		spawns++
		if spawns == 1 {
			return orchAnswer{pair: &other}
		}
		return orchAnswer{}
	}
	var mu sync.Mutex
	retried := false
	go func() {
		for i := 0; i < 5000; i++ {
			r, ok, _ := k.bus.ReadReply(firstReportID(k.orch))
			if ok && r.Status == ReplyPairMismatch {
				mu.Lock()
				retried = true
				mu.Unlock()
				k.orch.forget()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	res := k.h.run(`const r = await agent.call('look', { agentType: 'explorer', label: 'x' })
return r.status`)
	if got := value[string](t, res); got != "completed" {
		t.Fatalf("status %s", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if !retried {
		t.Fatalf("no pair_mismatch reply for a decided effort that differs")
	}
	r, _, _ := k.bus.ReadReply(firstReportID(k.orch))
	if r.Close != "agent-1" || !strings.Contains(r.Detail, "spawn again with claude-fake-haiku/high (it runs claude-fake-haiku/medium)") {
		t.Errorf("pair_mismatch reply %+v", r)
	}
	ts := rowsNamed(k.rowsOf("x"), EvThreadStarted)
	if len(ts) != 1 || rowStr(ts[0].Data, "thread_id") != "agent-2" || rowStr(ts[0].Data, "effort") != "high" || ts[0].Data["effort_unchecked"] != nil || ts[0].Data["pair_unchecked"] != nil {
		t.Errorf("thread_started %v", ts)
	}
}

// explorerOn is the fake role table with the explorer on the pair.
func explorerOn(pair Pair) *Policy {
	pol := FakePolicy()
	exp := pol.Roles["explorer"]
	exp.Pair = pair
	pol.Roles["explorer"] = exp
	return pol
}

// A role whose pair the role table leaves to the host entirely binds its subagent unchecked (pair_unchecked), and a continuation
// of the call is judged by the same rule: the undecided model is not compared with the pair the host read back, so the
// continuation runs on a new subagent whose thread_started records the host-read pair, marked pair_unchecked, as the bind does.
func TestSessionUndecidedModelIsNotComparedOnContinuation(t *testing.T) {
	k := newSessionKit(t)
	k.h.cfg.Policy = explorerOn(Pair{})
	applied := Pair{"claude-fake-haiku", "high"}
	k.orch.answer = func(Step, string, int) orchAnswer { return orchAnswer{pair: &applied} }
	res := k.h.run(`const r = await agent.call('look', { agentType: 'explorer', label: 'x' })
const more = await agent.call('look again', { resume: r, label: 'more' })
return [r.status, more.status]`)
	if got := value[[]string](t, res); strings.Join(got, ",") != "completed,completed" {
		t.Fatalf("statuses %v (the continuation's rows: %v)", got, k.rowsOf("more"))
	}
	for _, label := range []string{"x", "more"} {
		rows := k.rowsOf(label)
		if got := eventsOf(rows); got != "prepared,step_published,thread_started,turn_started,completed" {
			t.Errorf("%s: rows %s", label, got)
			continue
		}
		step, ts := rows[1], rows[2]
		if rowStr(step.Data, "model") != "" || rowStr(step.Data, "effort") != "" {
			t.Errorf("%s: step_published names %s/%s, want no pair", label, rowStr(step.Data, "model"), rowStr(step.Data, "effort"))
		}
		if rowStr(ts.Data, "model") != applied.Model || rowStr(ts.Data, "effort") != applied.Effort || ts.Data["pair_unchecked"] != true || ts.Data["effort_unchecked"] != nil {
			t.Errorf("%s: thread_started %v, want the host-read pair %v with pair_unchecked", label, ts.Data, applied)
		}
	}
	if n := len(k.orch.spawned()); n != 2 {
		t.Errorf("%d spawns, want one subagent per call", n)
	}
	if info, _ := LookupCall(k.h.journal, k.rowsOf("more")[0].CallID); info.Configured != applied {
		t.Errorf("the continuation's pair %v, want the host-read %v", info.Configured, applied)
	}
}

// A continuation is refused when a decided part of the role's pair differs from the pair the call's subagent was bound with: a call
// bound pair_unchecked whose role a later run reads with a decided model, or a decided effort, other than the one the host ran is
// refused pair_refused, and no subagent is spawned for it.
func TestSessionContinuationOnADecidedPairThatDiffersIsRefused(t *testing.T) {
	applied := Pair{"claude-fake-haiku", "high"}
	for _, tc := range []struct {
		name string
		now  Pair
	}{
		{"model", Pair{"claude-fake-sonnet", "high"}},
		{"effort", Pair{"claude-fake-haiku", "xhigh"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newSessionKit(t)
			k.h.cfg.Policy = explorerOn(Pair{})
			k.orch.answer = func(Step, string, int) orchAnswer { return orchAnswer{pair: &applied} }
			handle := value[string](t, k.h.run(`const r = await agent.call('look', { agentType: 'explorer', label: 'x' })
return r.handle`))
			// a later run of the journal reads a role table that decides the part; the clock read takes the first call's position
			k.h.cfg.Policy = explorerOn(tc.now)
			k.h.cfg.Args = mustJSON(map[string]any{"handle": handle})
			// a continuation that is not refused publishes a step and waits for a subagent of the role's pair that the owner never
			// spawns: the run is ended at that step and the assertions below say so, instead of the run's timeout (CRW-1392)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			go func() {
				for ctx.Err() == nil {
					if len(rowsNamed(k.rowsOf("more"), EvStepPublished)) > 0 {
						cancel()
						return
					}
					time.Sleep(time.Millisecond)
				}
			}()
			res, err := Run(ctx, k.h.load(`await crw.clock()
const more = await agent.call('look again', { resume: args.handle, label: 'more' })
return [more.status, more.reason]`), k.h.cfg)
			if err != nil {
				t.Fatal(err)
			}
			rows := k.rowsOf("more")
			if n := len(rowsNamed(rows, EvStepPublished)); n > 0 {
				t.Fatalf("the continuation was not refused: it published %d step(s), rows %s", n, eventsOf(rows))
			}
			got := value[[]string](t, res)
			want := "the thread reads back claude-fake-haiku/high, the role's pair is " + tc.now.Model + "/" + tc.now.Effort
			if strings.Join(got, ",") != "refused,pair_refused" || eventsOf(rows) != "prepared,refused" || rowStr(rows[1].Data, "detail") != want {
				t.Errorf("continuation %v, rows %v; want refused pair_refused %q", got, rows, want)
			}
			if n := len(k.orch.spawned()); n != 1 {
				t.Errorf("%d spawns, want only the first call's", n)
			}
		})
	}
}

func firstReportID(o *fakeOrch) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.reports) == 0 {
		return "none"
	}
	return o.reports[0].ID
}

// Tokens come from the rollout's last token_count; a rollout that cannot be read leaves the call unmeasured, which a session run
// records without pausing under a ceiling.
func TestSessionTokens(t *testing.T) {
	k := newSessionKit(t)
	k.host.tokens["/rollout/agent-1"] = Tokens{Input: 10, Output: 5, Total: 15}
	k.h.cfg.TokenBudget = 1000
	res := k.h.run(`const a = await agent.call('one', { agentType: 'explorer', label: 'a' })
const b = await agent.call('two', { agentType: 'explorer', label: 'b' })
const c = await agent.call('three', { agentType: 'explorer', label: 'c' })
return [a.meta.tokens.total, b.status, c.status]`)
	if got := value[[]any](t, res); fmt.Sprint(got) != "[15 completed completed]" {
		t.Fatalf("results %v", got)
	}
	un := flowRows(k.h.journal, "tokens_unmeasured")
	if len(un) != 2 || un[0].Data["pauses"] != false {
		t.Errorf("tokens_unmeasured rows %v", un)
	}
}

// The trace digests taken at the bind and at the harvest record a read role that wrote, an executor that wrote outside P and a
// change of the shared state, as warnings: the result is used.
func TestSessionTraceDigestsWarn(t *testing.T) {
	k := newSessionKit(t)
	var mu sync.Mutex
	n := map[string]int{}
	k.cfg.Trace = func(_ context.Context, ws string) (TraceDigests, error) {
		mu.Lock()
		defer mu.Unlock()
		n[ws]++
		return TraceDigests{Workspace: fmt.Sprintf("w%d", n[ws]), Outside: fmt.Sprintf("o%d", n[ws]), Shared: "s"}, nil
	}
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer { return orchAnswer{edit: "P:" + st.Op} }
	res := k.h.run(`const a = await agent.call('review', { agentType: 'reviewer', label: 'rev', head: 'h', schema: VERDICT })
const b = await agent.call('build', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
return [a.status, b.status]`)
	if got := value[[]string](t, res); strings.Join(got, ",") != "completed,completed" {
		t.Fatalf("results %v", got)
	}
	read, outside, shared := flowRows(k.h.journal, "read_role_wrote"), flowRows(k.h.journal, "executor_wrote_outside"), flowRows(k.h.journal, "shared_state_changed")
	if len(read) != 1 || read[0].CallID != k.rowsOf("rev")[0].CallID || len(outside) != 1 || outside[0].CallID != k.rowsOf("exe")[0].CallID || len(shared) != 0 {
		t.Errorf("warnings read %v outside %v shared %v", read, outside, shared)
	}
}

// An executor subagent that committed moved HEAD: the commit is kept on the attempt's stray ref and the completed row records
// executor_head_moved; the pending tree is still the snapshot of P.
func TestSessionExecutorThatCommitted(t *testing.T) {
	k := newSessionKit(t)
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer { return orchAnswer{edit: "P:" + st.Op, head: "stray-1"} }
	res := k.h.run(`const r = await agent.call('build', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
return r.status`)
	if got := value[string](t, res); got != "completed" {
		t.Fatalf("status %s", got)
	}
	done := k.rowsOf("exe")[4]
	moved, _ := done.Data["executor_head_moved"].(map[string]any)
	ref := StrayRef("run-test", "n1", 1, done.CallID)
	if rowStr(moved, "start") != "base" || rowStr(moved, "head") != "stray-1" || rowStr(moved, "ref") != ref {
		t.Errorf("executor_head_moved %v", done.Data["executor_head_moved"])
	}
	if k.placer.kept[ref] != "stray-1" {
		t.Errorf("kept refs %v", k.placer.kept)
	}
	if rowStr(done.Data, "pending_tree") != "P:"+done.CallID+"/t1-a1" {
		t.Errorf("pending_tree %q", rowStr(done.Data, "pending_tree"))
	}
}

// CRW-1367: a fix round is a second executor call of the same allocation. Each call that finds HEAD moved keeps the commit on a
// stray ref of its own (the call is in the ref's name), so the second never overwrites the record of the first.
func TestSessionSecondExecutorCallKeepsItsOwnStrayRef(t *testing.T) {
	k := newSessionKit(t)
	var mu sync.Mutex
	heads := map[string]string{}
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
		mu.Lock()
		defer mu.Unlock()
		if heads[st.Call] == "" {
			heads[st.Call] = fmt.Sprintf("stray-%d", len(heads)+1)
		}
		return orchAnswer{edit: "P:" + st.Op, head: heads[st.Call]}
	}
	res := k.h.run(`const r = await agent.call('build', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
const fix = await agent.call('fix what the review found', { resume: r, label: 'fix', schema: RECEIPT })
return [r.status, fix.status]`)
	if got := value[[]string](t, res); strings.Join(got, ",") != "completed,completed" {
		t.Fatalf("statuses %v", got)
	}
	var refs []string
	for i, label := range []string{"exe", "fix"} {
		rows := k.rowsOf(label)
		done := rows[len(rows)-1]
		moved, _ := done.Data["executor_head_moved"].(map[string]any)
		ref, head := rowStr(moved, "ref"), fmt.Sprintf("stray-%d", i+1)
		if rowStr(moved, "head") != head || !strings.HasPrefix(ref, StrayRefPrefix("run-test")) || !strings.Contains(ref, done.CallID) {
			t.Errorf("%s: executor_head_moved %v, want head %s on a stray ref that names the call %s", label, moved, head, done.CallID)
		}
		if got := k.placer.kept[ref]; got != head {
			t.Errorf("%s: %s keeps %q, want %s (kept %v)", label, ref, got, head, k.placer.kept)
		}
		refs = append(refs, ref)
	}
	if refs[0] == refs[1] {
		t.Errorf("both executor calls of the allocation keep their HEAD on %s", refs[0])
	}
}

// failingBus is a step bus whose writes fail (a full disk) the first times a step is written.
type failingBus struct {
	*FileStepBus
	mu    sync.Mutex
	fails int
}

func (b *failingBus) Publish(st Step) error {
	if st.Action != StepClose {
		b.mu.Lock()
		fail := b.fails > 0
		if fail {
			b.fails--
		}
		b.mu.Unlock()
		if fail {
			return errors.New("write steps/t1-a1.json: no space left on device")
		}
	}
	return b.FileStepBus.Publish(st)
}

// CRW-1367: a step that could not be written was sent to nobody. The run records it as not dispatched (a step_unwritten warning,
// input_state not_delivered) and writes it again, and the call goes on: no outcome_unknown, no dispatch_unknown escalation and no
// recovery hold of the node.
func TestSessionStepBusWriteFailureIsNotDispatched(t *testing.T) {
	k := newSessionKit(t)
	bus := &failingBus{FileStepBus: k.bus, fails: 3}
	k.cfg.Bus = bus
	res := k.h.run(`const r = await agent.call('build node n1', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
return r.status`)
	if got := value[string](t, res); got != "completed" {
		t.Fatalf("status %s; rows %s", got, eventsOf(k.rowsOf("exe")))
	}
	if got := eventsOf(k.rowsOf("exe")); got != oneAttempt {
		t.Errorf("rows %s, want %s", got, oneAttempt)
	}
	warn := flowRows(k.h.journal, "step_unwritten")
	if len(warn) != 1 || rowStr(warn[0].Data, "input_state") != "not_delivered" || !strings.Contains(rowStr(warn[0].Data, "error"), "no space left") ||
		rowStr(warn[0].Data, "marker") != k.rowsOf("exe")[0].CallID+"/t1-a1" {
		t.Errorf("step_unwritten rows %v, want one not_delivered warning naming the marker and the error", warn)
	}
	for _, e := range k.h.journal.Events() {
		if e.Event == EvOutcomeUnknown || e.Event == "escalation" {
			t.Errorf("a step that was never written ended %s: %v", e.Event, e.Data)
		}
	}
}

// CRW-1367: a close step's file names the subagent, so two closes of one attempt (the pair_mismatch close and the close after
// the harvest) are two files and the later one never overwrites the earlier.
func TestStepBusClosesOfOneAttemptAreKeptApart(t *testing.T) {
	bus := NewFileStepBus(t.TempDir())
	for _, agent := range []string{"agent-1", "agent-2"} {
		if err := bus.Publish(Step{Run: "run-test", Call: "c_000000000001", Attempt: 1, Action: StepClose, AgentID: agent}); err != nil {
			t.Fatal(err)
		}
	}
	steps, err := bus.Steps()
	if err != nil {
		t.Fatal(err)
	}
	var agents []string
	for _, st := range steps {
		agents = append(agents, st.AgentID)
	}
	if strings.Join(agents, ",") != "agent-1,agent-2" {
		t.Fatalf("the close steps of attempt 1: %v, want both subagents", agents)
	}
}

// A resume on another backend than the one the run's started row records is refused before anything is written.
func TestSessionMixedResumeIsRefused(t *testing.T) {
	k := newSessionKit(t)
	k.h.run(`return await agent('look', { agentType: 'explorer', label: 'x' })`)
	before := len(k.h.journal.Events())
	app := newHarness(t)
	app.journal = k.h.journal
	app.cfg.Journal = k.h.journal
	app.cfg.Resume = &ResumeOptions{}
	_, err := Run(context.Background(), app.load(`return 1`), app.cfg)
	if err == nil || !strings.Contains(err.Error(), "session") || !strings.Contains(err.Error(), "app-server") {
		t.Fatalf("a resume of a session run on the App Server backend: %v", err)
	}
	if len(k.h.journal.Events()) != before {
		t.Errorf("the refused resume wrote rows")
	}
	if got := RecordedBackend(k.h.journal.Events(), "run-test"); got != BackendSession {
		t.Errorf("RecordedBackend %q", got)
	}
	plain := NewMemoryJournal()
	plain.Append(Event{RunID: "r", Kind: KindRun, Event: "started", Data: map[string]any{}})
	if got := RecordedBackend(plain.Events(), "r"); got != BackendAppServer {
		t.Errorf("a run that records none is an App Server run, got %q", got)
	}
}

// deathCase is one place a session run's process dies.
type deathCase struct {
	name string
	// stop names the last row the process stored (nil: the death comes from the orchestrator's spawn or input).
	stop func(Event) bool
	// killOnSpawn kills the process right after the orchestrator spawned attempt 1, before it reports it; killOnInput right after
	// it sent attempt 1 its repair input, before it reports that.
	killOnSpawn, killOnInput bool
	// lose makes the first attempt's subagent lost; repair makes the first turn's answer fail the schema, so the call repairs on the
	// same subagent through send_input.
	lose, repair bool
	// spawns is how many subagents the call ends up with, inputs how many repair inputs they were sent.
	spawns, inputs int
	// rows is the call's exact row sequence after the resume: one thread_started per attempt.
	rows string
	// noFind makes the resumed process's marker search find nothing, so only the call's own rows name its subagent.
	noFind bool
	// gone makes the host forget attempt 1's subagent before the resume.
	gone bool
	// tokens are the rollout totals of agent-1 after its first and its second turn; the resumed call's completed row is to record
	// the second, the turn it harvested to add only its own, and the run's figure after the resume (the resumed process's, and
	// RunSpent over the journal) to be the second too.
	tokens []Tokens
}

// deathJournal is a journal a death case runs over: open gives the dying process's journal and the one the resumed process opens
// over what the first stored. The memory journal stores every row it is given; the SQLite journal commits an intent at once and the
// other rows in batches, so a death loses the rows of the open batch (the thread_started and turn_started of a bind among them).
type deathJournal struct {
	name string
	open func(t *testing.T) (first Journal, reopen func() Journal)
}

// SQLiteDeathJournal opens the SQLite journal of internal/flow/journal over a temporary store for a death case of run runID;
// session_sqlite_test.go (package engine_test, which may import that package) sets it.
var SQLiteDeathJournal func(t *testing.T, runID string) (first Journal, reopen func() Journal)

func deathJournals(t *testing.T) []deathJournal {
	t.Helper()
	if SQLiteDeathJournal == nil {
		t.Fatal("session_sqlite_test.go did not install the SQLite journal of the death cases")
	}
	return []deathJournal{
		{name: "memory", open: func(*testing.T) (Journal, func() Journal) {
			mem := NewMemoryJournal()
			return mem, func() Journal { return mem }
		}},
		{name: "sqlite", open: func(t *testing.T) (Journal, func() Journal) { return SQLiteDeathJournal(t, "run-test") }},
	}
}

// noFindHost is a host whose marker search finds no subagent.
type noFindHost struct{ *fakeSessionHost }

func (noFindHost) FindByMarker(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}

const (
	oneAttempt = "prepared,step_published,thread_started,turn_started,completed"
	repaired   = "prepared,step_published,thread_started,turn_started,step_published,turn_started,completed"
)

// The run dies at each last-row state of a session call, and a resume goes on from that row in the same allocation: prepared and
// step_published publish the step again after a marker search (a subagent spawned before it was reported is bound, not spawned
// again; a repair input sent before it was reported is bound on the subagent, not sent again), thread_started adopts the recorded
// subagent and turn_started harvests the bound one, and session_attempt_lost goes on with the next attempt. Each case's rows are
// exact - one thread_started per attempt - and the same over the memory journal and over the SQLite journal, which loses the rows
// of its open batch with the process (CRW-1367). No step the dead process left is still published when the resumed call ends, and
// a recovered repair turn counts only the tokens it added.
func TestSessionRunDeathAtEachRow(t *testing.T) {
	repairTokens := []Tokens{{Input: 10, Output: 5, Total: 15}, {Input: 30, Output: 10, Total: 40}}
	for _, dj := range deathJournals(t) {
		for _, c := range []deathCase{
			{name: "prepared", stop: deathAt(EvPrepared), spawns: 1, rows: oneAttempt},
			{name: "step_published", stop: deathAt(EvStepPublished), spawns: 1, rows: oneAttempt},
			{name: "spawned before the report", killOnSpawn: true, spawns: 1, rows: oneAttempt},
			{name: "thread_started", stop: deathAt(EvThreadStarted), spawns: 1, rows: oneAttempt},
			{name: "turn_started", stop: deathAt(EvTurnStarted), spawns: 1, rows: oneAttempt},
			{name: "session_attempt_lost", stop: deathAt(EvSessionAttemptLost), lose: true, spawns: 2,
				rows: "prepared,step_published,thread_started,turn_started,session_attempt_lost,step_published,thread_started,turn_started,completed"},
			{name: "repair sent before the report", killOnInput: true, repair: true, spawns: 1, inputs: 1, rows: repaired, tokens: repairTokens},
			{name: "repair turn_started", stop: deathAtK(EvTurnStarted, 2), repair: true, spawns: 1, inputs: 1, rows: repaired, tokens: repairTokens},
		} {
			t.Run(dj.name+"/"+c.name, func(t *testing.T) { runDeath(t, dj, c) })
		}
	}
}

// deathAt stops the process after it stored the call row.
func deathAt(name string) func(Event) bool {
	return func(e Event) bool { return e.Kind == KindCall && e.Event == name }
}

// deathAtK stops the process after it stored the call row of turn k.
func deathAtK(name string, k int) func(Event) bool {
	return func(e Event) bool { return e.Kind == KindCall && e.Event == name && rowInt(e.Data, "k") == k }
}

// A process that dies after it wrote thread_started and before turn_started leaves an attempt whose subagent is recorded: the
// resumed call adopts that subagent - with the marker search or without it - and writes only the missing turn_started, so the
// attempt keeps one thread_started, nothing is spawned again, and the harvest reads the recorded subagent's marker turn. A
// recorded subagent the host no longer knows loses the attempt, and the next attempt has its own thread_started. Over the SQLite
// journal the death loses the bind's rows with its batch: the resumed call finds the subagent by the marker or by the report the
// dead process never answered, and binds it once; one the host no longer knows is never bound, and the step it publishes again is
// taken by a new subagent for the same attempt.
func TestSessionResumeAfterThreadStartedAdoptsTheRecordedAgent(t *testing.T) {
	type want struct {
		rows   string
		bound  []string
		spawns int
		lost   bool
	}
	for _, dj := range deathJournals(t) {
		for _, tc := range []struct {
			name         string
			noFind, gone bool
			memory, sql  want
		}{
			{name: "marker search finds it", memory: want{rows: oneAttempt, bound: []string{"agent-1"}, spawns: 1}},
			{name: "marker search finds nothing", noFind: true, memory: want{rows: oneAttempt, bound: []string{"agent-1"}, spawns: 1}},
			{name: "recorded subagent gone", gone: true,
				memory: want{rows: "prepared,step_published,thread_started,session_attempt_lost,step_published,thread_started,turn_started,completed", bound: []string{"agent-1", "agent-2"}, spawns: 2, lost: true},
				sql:    want{rows: oneAttempt, bound: []string{"agent-2"}, spawns: 2}},
		} {
			w := tc.memory
			if dj.name == "sqlite" && tc.sql.rows != "" {
				w = tc.sql
			}
			t.Run(dj.name+"/"+tc.name, func(t *testing.T) {
				k, got := runDeath(t, dj, deathCase{name: "thread_started", stop: deathAt(EvThreadStarted), spawns: w.spawns, rows: w.rows, noFind: tc.noFind, gone: tc.gone})
				rows := k.rowsOf("exe")
				ts, turn := rowsNamed(rows, EvThreadStarted), rowsNamed(rows, EvTurnStarted)
				if len(ts) != len(w.bound) {
					t.Fatalf("thread_started %v, want one row per attempt naming %v", ts, w.bound)
				}
				for i, r := range ts {
					if rowInt(r.Data, "attempt") != i+1 || rowStr(r.Data, "thread_id") != w.bound[i] {
						t.Errorf("thread_started %d: %v, want attempt %d of %s", i, r.Data, i+1, w.bound[i])
					}
				}
				last := w.bound[len(w.bound)-1]
				if len(turn) != 1 || rowStr(turn[0].Data, "turn_id") != last+"-turn1" {
					t.Errorf("turn_started %v, want the marker turn of %s", turn, last)
				}
				if got[1] != last {
					t.Errorf("the handle's thread %s, want %s", got[1], last)
				}
				lost := rowsNamed(rows, EvSessionAttemptLost)
				if w.lost && (len(lost) != 1 || rowStr(lost[0].Data, "agent_id") != "agent-1" || rowInt(lost[0].Data, "attempt") != 1) || !w.lost && len(lost) != 0 {
					t.Errorf("session_attempt_lost %v (want lost %v: attempt 1 of agent-1)", lost, w.lost)
				}
			})
		}
	}
}

// runDeath kills a session run's process where the case says, resumes it in a new process, checks what every case shares, and
// returns the kit and the resumed call's [status, threadId].
func runDeath(t *testing.T, dj deathJournal, c deathCase) (*sessionKit, []string) {
	k := newSessionKit(t)
	sw := &deadSwitch{}
	first, reopen := dj.open(t)
	k.h.cfg.Journal = &dyingJournal{Journal: first, sw: sw, stop: c.stop}
	k.jr = first
	k.cfg.Bus = &dyingBus{FileStepBus: k.bus, sw: sw}
	k.cfg.Host = &dyingHost{fakeSessionHost: k.host, sw: sw}
	setTokens := func(st Step) {
		if len(c.tokens) >= st.K {
			k.host.mu.Lock()
			k.host.tokens["/rollout/agent-1"] = c.tokens[st.K-1]
			k.host.mu.Unlock()
		}
	}
	answer := func(st Step) orchAnswer {
		setTokens(st)
		if c.repair && st.K == 1 {
			return orchAnswer{text: "this is not JSON"}
		}
		return orchAnswer{edit: "P:" + st.Op}
	}
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
		a := answer(st)
		if c.lose && st.Attempt == 1 {
			a = orchAnswer{lose: true}
		}
		if c.killOnSpawn && st.Attempt == 1 && st.Action == StepSpawn || c.killOnInput && st.Attempt == 1 && st.Action == StepSendInput {
			a.noDispatch = true
		}
		if c.stop != nil && !c.lose {
			// the subagent finishes after the process has died
			a.after = 30 * time.Millisecond
		}
		return a
	}
	if c.killOnSpawn {
		k.orch.onSpawn = func(st Step, _ string) {
			if st.Attempt == 1 {
				sw.kill()
			}
		}
	}
	if c.killOnInput {
		k.orch.onInput = func(st Step, _ string) {
			if st.Attempt == 1 {
				sw.kill()
			}
		}
	}
	body := `const r = await agent.call('build node n1', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
return [r.status, r.meta.threadId, r.meta.tokens.total]`
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ctx1, cancel1 := context.WithCancel(ctx)
	sw.mu.Lock()
	sw.cancel = cancel1
	sw.mu.Unlock()
	if _, err := Run(ctx1, k.h.load(body), k.h.cfg); err != nil {
		t.Fatal(err)
	}
	if !sw.isDead() {
		t.Fatalf("the process never died")
	}
	// the process that resumes it opens the journal again: it holds what the dead process stored
	resumed := reopen()
	k.h.cfg.Journal = resumed
	k.jr = resumed
	k.cfg.Bus = k.bus
	k.cfg.Host = k.host
	if c.noFind {
		k.cfg.Host = noFindHost{k.host}
	}
	if c.gone {
		k.host.set("agent-1", func(a *fakeAgent, _ *SessionTurn) { a.gone = true })
	}
	k.h.cfg.Resume = &ResumeOptions{}
	// the resumed process counts what the run's journal shows it spent, as crw flow resume seeds it (CRW-1278)
	k.h.cfg.SpentBefore = RunSpent(resumed.Events(), k.h.cfg.RunID).Total
	// the orchestrator still runs: its hooks are swapped under its lock (CRW-1392)
	k.orch.mu.Lock()
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer { return answer(st) }
	k.orch.onSpawn, k.orch.onInput = nil, nil
	k.orch.mu.Unlock()
	res, err := Run(ctx, k.h.load(body), k.h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	k.resumed = res
	var out []any
	if err := json.Unmarshal(res.Value, &out); err != nil || len(out) != 3 {
		t.Fatalf("the resumed run: %s %s %s", res.Status, res.Error, res.Value)
	}
	got := []string{fmt.Sprint(out[0]), fmt.Sprint(out[1])}
	if got[0] != "completed" {
		t.Fatalf("the resumed call %v; rows %s", got, eventsOf(k.rowsOf("exe")))
	}
	rows := k.rowsOf("exe")
	if got := eventsOf(rows); c.rows != "" && got != c.rows {
		t.Errorf("rows %s, want %s", got, c.rows)
	}
	if n := len(rowsNamed(rows, EvPrepared)); n != 1 {
		t.Errorf("%d prepared rows (%s): the resume did not go on in the same allocation", n, eventsOf(rows))
	}
	if n := len(k.orch.spawned()); n != c.spawns {
		t.Errorf("%d subagents spawned, want %d (rows %s)", n, c.spawns, eventsOf(rows))
	}
	if n := len(k.orch.sentInput()); n != c.inputs {
		t.Errorf("%d repair inputs sent, want %d (rows %s)", n, c.inputs, eventsOf(rows))
	}
	if n := len(rowsNamed(rows, EvCompleted)); n != 1 {
		t.Errorf("rows %s", eventsOf(rows))
	}
	if got := k.placer.establishedPaths(); countOf(got, "/w/n1/a1") != 1 {
		t.Errorf("the worktree was made twice: %v", got)
	}
	ts := rowsNamed(rows, EvThreadStarted)
	if len(ts) == 0 || got[1] != rowStr(ts[len(ts)-1].Data, "thread_id") {
		t.Errorf("the handle's thread %s is not the last bound subagent (%v)", got[1], ts)
	}
	// the step files of the call: none is still to be taken once it has ended
	steps, _ := k.bus.Steps()
	for _, st := range steps {
		if st.Call == rows[0].CallID && st.Action != StepClose {
			t.Errorf("a step of the ended call is still published: %+v", st)
		}
	}
	if n := len(c.tokens); n > 0 {
		want := c.tokens[n-1]
		if total := fmt.Sprint(out[2]); total != fmt.Sprint(want.Total) {
			t.Errorf("the resumed call's tokens total %s, want the rollout's %d", total, want.Total)
		}
		// the resumed process's figure is the run's (Config.SpentBefore, CRW-1278): the turns before the recovered one are in no terminal
		// row until the resumed call's, which carries them, so the process counts them once with it and the recovered turn adds only
		// its own; the journal's figure is the same
		if spent := RunSpent(k.jr.Events(), k.h.cfg.RunID).Total; res.Spent != want.Total || spent != want.Total {
			t.Errorf("the resumed process's spent figure %d, the journal's %d, want the rollout's %d once: the turns before the recovered one are counted again or not at all",
				res.Spent, spent, want.Total)
		}
	}
	return k, got
}

// A continuation after a run restart runs on a new subagent from a checkpoint packet - never a fork - and writes its own
// thread_started row, so the handle chain and the pair relay acceptance reads name the new subagent.
func TestSessionContinuationAfterRestart(t *testing.T) {
	k := newSessionKit(t)
	first := `const r = await agent.call('build node n1', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
return r.handle`
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer { return orchAnswer{edit: "P:" + st.Op} }
	res := k.h.run(first)
	handle := value[string](t, res)
	k.h.cfg.Resume = &ResumeOptions{}
	res = k.h.run(`const r = await agent.call('build node n1', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
const fix = await agent.call('fix what the review found', { resume: r, label: 'fix', schema: RECEIPT })
return [r.handle, fix.status, fix.meta.threadId]`)
	got := value[[]string](t, res)
	if got[0] != handle || got[1] != "completed" {
		t.Fatalf("results %v (handle %s)", got, handle)
	}
	rows := k.rowsOf("fix")
	if got := eventsOf(rows); got != "prepared,step_published,thread_started,turn_started,completed" {
		t.Fatalf("rows %s", got)
	}
	if len(flowRows(k.h.journal, EvCheckpointStarted)) != 1 {
		t.Errorf("no checkpoint_started row: the continuation did not take the checkpoint branch")
	}
	for _, e := range k.h.journal.Events() {
		if e.Event == EvForkRequested || e.Event == EvForkStarted {
			t.Errorf("a session continuation forked: %v", e)
		}
	}
	ts := rows[2]
	if rowStr(ts.Data, "thread_id") != got[2] || rowStr(ts.Data, "thread_id") == rowStr(rowsNamed(k.rowsOf("exe"), EvThreadStarted)[0].Data, "thread_id") {
		t.Errorf("the continuation's thread_started %v", ts.Data)
	}
	step := rows[1]
	packet, _ := os.ReadFile(rowStr(step.Data, "packet"))
	exeStep := rowsNamed(k.rowsOf("exe"), EvStepPublished)[0]
	if !strings.Contains(string(packet), rowStr(exeStep.Data, "packet")) || !strings.Contains(string(packet), "answer-of-") || !strings.Contains(string(packet), "fix what the review found") {
		t.Errorf("the continuation packet does not carry the original packet, its answer and the new input:\n%s", packet)
	}
	if info, _ := LookupCall(k.h.journal, rows[0].CallID); info.ThreadID != got[2] || info.Cwd != "/w/n1/a1" {
		t.Errorf("the handle of the continuation %+v", info)
	}
}

// A held session call goes on in place on the next resume: the subagent the held attempt was bound to is confirmed not running,
// and a new attempt in the same allocation takes the task from its packet with the reason it stopped.
func TestSessionHeldCallGoesOnInPlace(t *testing.T) {
	k := newSessionKit(t)
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer { return orchAnswer{fail: "usageLimitExceeded"} }
	body := `const r = await agent.call('build node n1', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
return [r.status, r.reason]`
	res := k.h.run(body)
	if got := value[[]string](t, res); got[0] != "held" {
		t.Fatalf("first run %v", got)
	}
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer { return orchAnswer{edit: "P:" + st.Op} }
	k.h.cfg.Resume = &ResumeOptions{}
	res = k.h.run(body)
	if got := value[[]string](t, res); got[0] != "completed" {
		t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
	}
	rows := k.rowsOf("exe")
	if n := len(rowsNamed(rows, EvPrepared)); n != 1 {
		t.Errorf("%d prepared rows: %s", n, eventsOf(rows))
	}
	steps := rowsNamed(rows, EvStepPublished)
	last := steps[len(steps)-1]
	if rowInt(last.Data, "attempt") != 2 {
		t.Errorf("the attempt that goes on %v", last.Data)
	}
	packet, _ := os.ReadFile(rowStr(last.Data, "packet"))
	if !strings.Contains(string(packet), "host_infra_usageLimitExceeded") {
		t.Errorf("the packet does not say why the previous attempt stopped:\n%s", packet)
	}
	if len(flowRows(k.h.journal, EvHoldCleared)) != 1 {
		t.Errorf("no hold_cleared row")
	}
}

// The App Server backend is not changed by the session path: a run without Session records backend app-server and its rows are
// the thread and turn rows of before.
func TestAppServerRunRecordsItsBackend(t *testing.T) {
	h := newHarness(t)
	res := h.run(`return await agent('hi', { agentType: 'explorer', label: 'x' })`)
	_ = value[any](t, res)
	if got := RecordedBackend(h.journal.Events(), "run-test"); got != BackendAppServer {
		t.Errorf("backend %q", got)
	}
	for _, e := range h.journal.Events() {
		if e.Kind == KindRun && e.Event == "started" && rowStr(e.Data, "backend") != BackendAppServer {
			t.Errorf("started row %v", e.Data)
		}
	}
	if got := eventsOf(h.callRows("x")); got != "prepared,thread_requested,thread_started,turn_requested,turn_started,completed" {
		t.Errorf("rows %s", got)
	}
}

var _ = errors.New

func countOf(list []string, v string) int {
	n := 0
	for _, x := range list {
		if x == v {
			n++
		}
	}
	return n
}
