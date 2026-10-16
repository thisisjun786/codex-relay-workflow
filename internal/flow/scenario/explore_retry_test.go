package scenario

import (
	"io/fs"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows/lib"
)

// The background explorers of a milestone (CRW-1466) under the retry of CRW-1571: an explorer whose answer is null_model is asked
// once more on a fresh thread; one that fails twice is named in the architect's packet as a missing exploration and the milestone
// goes on; the milestone halts only when every background explorer failed. A run in which no explorer fails makes the calls, packets
// and records it made before.

// haltResult is the value of a project lane a milestone halt ended (lib/lane.js#milestoneHalt).
type haltResult struct {
	State  string `json:"state"`
	Source string `json:"source"`
	Reason string `json:"reason"`
	Call   string `json:"call"`
}

// noVerified is FINDINGS less its verified member: valid JSON the schema refuses, so the call's repair turn is asked.
func noVerified(answer string) map[string]any {
	return map[string]any{"answer": answer, "evidence": []string{"README.md"}, "inferred": []string{}, "unknown": []string{}}
}

// bgScript scripts the explorers of one question: plan answers each turn of each thread whose first turn asked the question, given
// the thread's ordinal among those threads (0 the first call, 1 its retry) and the turn; nil is the role's default. threads are the
// thread ids of the question's explorers, in the order their first turns came.
type bgScript struct {
	mu      sync.Mutex
	of      map[string]int
	threads []string
}

// scriptBackground scripts the background explorers of a node.
func scriptBackground(l *Lab, node string, plan func(attempt int, c *Call) *fakehostPlan) *bgScript {
	return scriptExplorer(l, "^"+BackgroundLabel, "Background of node "+node+" (", plan)
}

// scriptExplorer scripts the explorers whose label matches the expression and whose first turn holds text (an open question's,
// research or evidence question's packet names it after QUESTION:).
func scriptExplorer(l *Lab, label, text string, plan func(attempt int, c *Call) *fakehostPlan) *bgScript {
	s := &bgScript{of: map[string]int{}}
	l.On(label, func(c *Call) *fakehostPlan {
		s.mu.Lock()
		attempt, ok := s.of[c.ThreadID]
		if !ok && c.N == 1 && strings.Contains(c.Text, text) {
			attempt = len(s.threads)
			s.of[c.ThreadID] = attempt
			s.threads = append(s.threads, c.ThreadID)
			ok = true
		}
		s.mu.Unlock()
		if !ok {
			return nil
		}
		return plan(attempt, c)
	})
	return s
}

func (s *bgScript) Threads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.threads)
}

// nullModelTurns answers the first turn of a thread with JSON the schema refuses and its repair turn with no JSON: the call is
// null_model (schema_invalid_after_repair), as the incident of CRW-1571's explorer was.
func nullModelTurns(c *Call) *fakehostPlan {
	p := Garbage()
	if c.N == 1 {
		p = Say(noVerified("the answer without what was verified"))
	}
	return &p
}

// architectText captures the packet of the architect's first turn.
func architectText(l *Lab) func() string {
	var mu sync.Mutex
	text := ""
	l.On("^architect$", func(c *Call) *fakehostPlan {
		mu.Lock()
		if c.N == 1 {
			text = c.Text
		}
		mu.Unlock()
		return nil
	})
	return func() string { mu.Lock(); defer mu.Unlock(); return text }
}

func backgroundCalls(l *Lab) []CallInfo {
	var out []CallInfo
	for _, c := range l.AllCalls() {
		if strings.HasPrefix(c.Label, BackgroundLabel) {
			out = append(out, c)
		}
	}
	return out
}

func hasLabel(calls []CallInfo, label string) bool {
	return slices.ContainsFunc(calls, func(c CallInfo) bool { return c.Label == label })
}

// backgroundRecord is the completed crw.exploreRecord row of the background questions.
func backgroundRecord(t *testing.T, l *Lab) engine.Event {
	t.Helper()
	for _, ev := range l.Rows(ops.OpExploreRecord) {
		if str(ev.Data, "kind") == ops.ExploreBackground {
			return ev
		}
	}
	t.Fatalf("no background exploreRecord row")
	return engine.Event{}
}

func recordedAnswer(t *testing.T, row engine.Event, id string) map[string]any {
	t.Helper()
	list, _ := row.Data["answers"].([]any)
	for _, item := range list {
		if m, _ := item.(map[string]any); str(m, "id") == id {
			return m
		}
	}
	t.Fatalf("the record has no answer %s: %v", id, row.Data["answers"])
	return nil
}

// libSwap is the embedded helper library with the text of one file between two markers (the start marker included, the end marker
// not) replaced: the library of a binary whose function there was another.
func libSwap(t *testing.T, base fs.FS, file, start, end, with string) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	err := fs.WalkDir(base, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(base, p)
		if err != nil {
			return err
		}
		if p == file {
			s := string(b)
			i := strings.Index(s, start)
			j := strings.Index(s, end)
			if i < 0 || j < i {
				t.Fatalf("%s has no %q before %q", file, start, end)
			}
			b = []byte(s[:i] + with + s[j:])
		}
		out[p] = &fstest.MapFile{Data: b}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// b0Lib is the helper library with lib/lane.js#explore and lib/packets.js#architectPacket as they were before CRW-1571.
func b0Lib(t *testing.T) fs.FS {
	t.Helper()
	l := libSwap(t, lib.FS, "lane.js", "export async function explore(", "// exploreEvidence is SP-EXPL-3", b0Explore)
	return libSwap(t, l, "packets.js", "export function architectPacket(", "// The executor contract", b0ArchitectPacket)
}

// TestBackgroundExplorerNullModelDoesNotEndTheMilestone is the repro of CRW-1571: of two nodes, B's background explorer answers JSON
// without verified and then no JSON on its repair turn, on every thread it is given, and A's completes. The milestone is not ended
// by the one exploration that failed: the architect is called and the run reaches the design.
func TestBackgroundExplorerNullModelDoesNotEndTheMilestone(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	scriptBackground(l, "B", func(_ int, c *Call) *fakehostPlan { return nullModelTurns(c) })
	res := l.Run()
	if res.Status != engine.RunCompleted {
		t.Fatalf("run %s: %s: %s", res.Status, res.ErrorKind, res.Error)
	}
	got := Value[haltResult](t, res)
	if got.State == "escalated" && got.Source == "explorer" {
		t.Errorf("the milestone halted on the explorer: %s (escalations %v)", res.Value, escalationReasons(l))
	}
	if slices.Contains(escalationReasons(l), "explorer_null_model") {
		t.Errorf("escalations %v name explorer_null_model", escalationReasons(l))
	}
	if !hasLabel(l.AllCalls(), "architect") {
		t.Errorf("no architect call: the run did not reach the design; calls %v", labelsOf(l.AllCalls()))
	}
}

// TestBackgroundExplorerIsAskedAgainOnAFreshThread: B's explorer is null_model on its first thread and completes on the retry. The
// retry is the first call's label with :retry, on another thread, and the architect's packet carries its answer among the
// BACKGROUND FINDINGS, with no missing exploration.
func TestBackgroundExplorerIsAskedAgainOnAFreshThread(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	arch := architectText(l)
	s := scriptBackground(l, "B", func(attempt int, c *Call) *fakehostPlan {
		if attempt == 0 {
			return nullModelTurns(c)
		}
		p := Say(map[string]any{"answer": "the retry's answer of B", "evidence": []string{"b/"}, "verified": []string{"b/"}, "inferred": []string{}, "unknown": []string{}})
		return &p
	})
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q", got.State)
	}
	threads := s.Threads()
	if len(threads) != 2 || threads[0] == threads[1] {
		t.Fatalf("B's explorer threads %v, want two distinct", threads)
	}
	bg := backgroundCalls(l)
	if len(bg) != 3 {
		t.Fatalf("background calls %v, want A's, B's and B's retry", labelsOf(bg))
	}
	retried := 0
	for _, c := range bg {
		if strings.HasSuffix(c.Label, ":retry") {
			retried++
			if !hasLabel(bg, strings.TrimSuffix(c.Label, ":retry")) {
				t.Errorf("the retry %s has no first call of its label among %v", c.Label, labelsOf(bg))
			}
		}
	}
	if retried != 1 {
		t.Errorf("background calls %v, want one :retry", labelsOf(bg))
	}
	text := arch()
	if !strings.Contains(text, "the retry's answer of B") {
		t.Errorf("the architect packet does not carry the retry's answer:\n%s", text)
	}
	if strings.Contains(text, "MISSING EXPLORATIONS") {
		t.Errorf("the architect packet names a missing exploration:\n%s", text)
	}
	rec := backgroundRecord(t, l)
	if a := recordedAnswer(t, rec, "bg-B"); str(a, "answer") != "the retry's answer of B" || str(a, "call") == "" {
		t.Errorf("the record of bg-B %v, want the retry's answer and call", a)
	}
}

// TestTwiceFailedBackgroundExplorerIsNamedInTheArchitectPacket: B's explorer is null_model on both threads. The architect's packet
// names bg-B and both calls under MISSING EXPLORATIONS; the record of bg-B has no answer and no call, as before CRW-1571, and the
// record's arguments are the ones of a library that never marks a missing exploration (the missing member does not reach
// crw.exploreRecord).
func TestTwiceFailedBackgroundExplorerIsNamedInTheArchitectPacket(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	arch := architectText(l)
	scriptBackground(l, "B", func(_ int, c *Call) *fakehostPlan { return nullModelTurns(c) })
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q", got.State)
	}
	text := arch()
	var ids []string
	for _, r := range backgroundCalls(l) {
		if first := strings.TrimSuffix(r.Label, ":retry"); first != r.Label {
			for _, c := range backgroundCalls(l) {
				if c.Label == first {
					ids = append(ids, c.ID, r.ID)
				}
			}
		}
	}
	if len(ids) != 2 {
		t.Fatalf("background calls %v, want B's call and its retry", labelsOf(backgroundCalls(l)))
	}
	_, missing, ok := strings.Cut(text, "MISSING EXPLORATIONS")
	if !ok {
		t.Fatalf("the architect packet names no missing exploration:\n%s", text)
	}
	want := "- bg-B: Background of node B (CRW-B): issue B."
	if !strings.Contains(missing, want) || !strings.Contains(missing, "(calls "+ids[0]+", "+ids[1]+": null_model schema_invalid_after_repair)") {
		t.Errorf("the missing section does not name bg-B with %v:\n%s", ids, missing)
	}
	if strings.Contains(missing, "bg-A") {
		t.Errorf("the missing section names bg-A:\n%s", missing)
	}
	findings, _, _ := strings.Cut(text, "MISSING EXPLORATIONS")
	if !strings.Contains(findings, "- bg-A: ") || strings.Contains(findings, "- bg-B: ") {
		t.Errorf("BACKGROUND FINDINGS should list bg-A only:\n%s", findings)
	}
	rec := backgroundRecord(t, l)
	if a := recordedAnswer(t, rec, "bg-B"); str(a, "answer") != "" || str(a, "call") != "" {
		t.Errorf("the record of bg-B %v, want no answer and no call", a)
	}
	// a run recorded with a library whose explore never attaches the missing member resumes over this one with its background record
	// replayed (the same arguments) and the architect live (its packet now names the missing exploration)
	pl := NewLab(t, twoNodes()...)
	pl.Lib = libSwap(t, lib.FS, "lane.js", markMissing, markMissingEnd, "")
	plainText := architectText(pl)
	scriptBackground(pl, "B", func(_ int, c *Call) *fakehostPlan { return nullModelTurns(c) })
	if got := mustRun(t, pl); got.State != "closed" {
		t.Fatalf("state %q", got.State)
	}
	if strings.Contains(plainText(), "MISSING EXPLORATIONS") {
		t.Fatalf("the library without the missing member still names it:\n%s", plainText())
	}
	before := pl.LastSeq()
	pl.Lib = nil
	pl.NewProcess("proc-2")
	pl.Resume()
	for _, ev := range rowsAfter(pl, before) {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvCompleted && ev.Op == ops.OpExploreRecord && str(ev.Data, "kind") == ops.ExploreBackground {
			t.Errorf("the background record ran again on the resume: its arguments carry the missing member: %v", ev.Data)
		}
	}
	if live := labelsOf(pl.LiveCalls(before)); len(live) == 0 || live[0] != "architect" {
		t.Errorf("live calls %v on the resume, want the architect's first", live)
	}
}

// markMissing and markMissingEnd bound the statement of lib/lane.js#explore that adds the missing member to an answer:
// TestTwiceFailedBackgroundExplorerIsNamedInTheArchitectPacket removes it to run a library that never marks one.
const (
	markMissing    = "if (miss) withMissing.missing = miss"
	markMissingEnd = "\n    return withMissing"
)

// TestAllBackgroundExplorersFailingHaltsAsBefore: both background explorers are null_model on both threads: the milestone halts as
// it did before CRW-1571 (escalated, source explorer, explorer_null_model), after four explorer calls and no architect call.
func TestAllBackgroundExplorersFailingHaltsAsBefore(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	scriptBackground(l, "A", func(_ int, c *Call) *fakehostPlan { return nullModelTurns(c) })
	scriptBackground(l, "B", func(_ int, c *Call) *fakehostPlan { return nullModelTurns(c) })
	res := l.Run()
	if res.Status != engine.RunCompleted {
		t.Fatalf("run %s: %s: %s", res.Status, res.ErrorKind, res.Error)
	}
	got := Value[haltResult](t, res)
	if got.State != "escalated" || got.Source != "explorer" || got.Reason != "schema_invalid_after_repair" || got.Call == "" {
		t.Errorf("value %s, want escalated by the explorer", res.Value)
	}
	if reasons := escalationReasons(l); !slices.Equal(reasons, []string{"explorer_null_model"}) {
		t.Errorf("escalations %v", reasons)
	}
	bg := backgroundCalls(l)
	if len(bg) != 4 {
		t.Errorf("background calls %v, want two and their retries", labelsOf(bg))
	}
	if len(bg) == 0 || !strings.HasSuffix(bg[len(bg)-1].Label, ":retry") || got.Call != bg[len(bg)-1].ID {
		t.Errorf("the halt names call %s, want the last retry of %v", got.Call, labelsOf(bg))
	}
	if hasLabel(l.AllCalls(), "architect") {
		t.Errorf("the architect was called: %v", labelsOf(l.AllCalls()))
	}
}

// TestHeldExplorerIsNotRetried: a background explorer the host failed is held, not asked again: the milestone is held by the
// explorer as before CRW-1571, and no call is labelled :retry.
func TestHeldExplorerIsNotRetried(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	scriptBackground(l, "B", func(int, *Call) *fakehostPlan { p := Fail("serverOverloaded"); return &p })
	res := l.Run()
	if res.Status != engine.RunCompleted {
		t.Fatalf("run %s: %s: %s", res.Status, res.ErrorKind, res.Error)
	}
	if got := Value[haltResult](t, res); got.State != "held" || got.Source != "explorer" {
		t.Errorf("value %s, want held by the explorer", res.Value)
	}
	if rows := l.Rows("crw.hold"); len(rows) != 1 || str(rows[0].Data, "node") != "milestone:M1" || str(rows[0].Data, "source") != "explorer" {
		t.Errorf("hold rows %+v", rows)
	}
	for _, c := range l.AllCalls() {
		if strings.HasSuffix(c.Label, ":retry") {
			t.Errorf("a call was retried: %v", labelsOf(l.AllCalls()))
		}
	}
	if hasLabel(l.AllCalls(), "architect") {
		t.Errorf("the architect was called: %v", labelsOf(l.AllCalls()))
	}
}

// TestNoFailedExplorerKeepsTheCalls: with every explorer completing, a run recorded with explore and architectPacket as they were
// before CRW-1571 resumes over this library with nothing live: every agent call (its packet, schema, head and joins: its input
// digest), crw.exploreRoute and the background crw.exploreRecord (its arguments) replay, so the calls, packets and records are the
// ones that library made.
func TestNoFailedExplorerKeepsTheCalls(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	l.Lib = b0Lib(t)
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q", got.State)
	}
	before := l.LastSeq()
	labels := labelsOf(l.AllCalls())
	l.Lib = nil
	l.NewProcess("proc-2")
	res := l.Resume()
	if res.Status != engine.RunCompleted {
		t.Fatalf("resume %s: %s: %s", res.Status, res.ErrorKind, res.Error)
	}
	if got := Value[laneResult](t, res); got.State != "closed" {
		t.Errorf("resumed state %q", got.State)
	}
	if live := l.LiveCalls(before); len(live) != 0 {
		t.Errorf("live calls %v on the resume, want none", labelsOf(live))
	}
	for _, ev := range rowsAfter(l, before) {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvCompleted && (ev.Op == ops.OpExploreRoute || ev.Op == ops.OpExploreRecord) {
			t.Errorf("%s ran again on the resume: %v", ev.Op, ev.Data)
		}
	}
	for _, label := range labels {
		if strings.HasSuffix(label, ":retry") {
			t.Errorf("a call was retried: %v", labels)
		}
	}
}

func labelsOf(calls []CallInfo) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.Label
	}
	return out
}

// b0Explore is lib/lane.js#explore as it was before CRW-1571 (a531e0503): from its line `export async function explore(` up to the
// comment of exploreEvidence.
const b0Explore = `export async function explore(kind, questions, ctx, opts) {
  const plan = await crw.exploreRoute(kind, ctx, questions)
  const spawn = plan.routes.filter((r) => r.route === 'spawn')
  const ends = (opts && opts.endConditions) || []
  const endsFor = spawn.length && ends.length ? spawn[0].digest : null
  const found = {}
  let failed = null
  let tokens = 0
  // a background explorer (CRW-1466) is labelled explore-bg:<milestone>:<digest12>, so a reader of the explore: labels (an open
  // question's, a research or evidence question's) never takes one for its own; every other kind keeps its label
  const prefix = kind === 'background' ? 'explore-bg:' : 'explore:'
  // the head the explorers read: the context's (it is part of the route, and of the reuse key of a re-plan's or an evidence question),
  // else opts.head, a head the calls are placed at that the route does not key on (the first DESIGN's design head)
  const callHead = ctx.head || (opts && opts.head) || undefined
  for (const wave of chunks(spawn, EXPLORER_CONCURRENCY)) {
    // the explorer given the end conditions answers FINDINGS_END; every other one is the call it was before CRW-1382 (its packet and
    // its schema), so a run recorded then replays it
    const calls = await parallel(wave.map((r) => () => r.digest === endsFor
      ? agent.call(explorerPacket(kind, r, ctx, ends), {
        agentType: 'explorer', node: ctx.node || undefined, head: callHead, schema: FINDINGS_END, label: prefix + (ctx.node || ctx.milestone) + ':' + r.digest.slice(0, 12),
      })
      : agent.call(explorerPacket(kind, r, ctx), {
        agentType: 'explorer', node: ctx.node || undefined, head: callHead, schema: FINDINGS, label: prefix + (ctx.node || ctx.milestone) + ':' + r.digest.slice(0, 12),
      })), { keys: wave.map((r) => r.digest.slice(0, 12)), label: 'explore' })
    wave.forEach((r, i) => {
      const call = calls[i]
      tokens += (call && call.meta && call.meta.tokens && call.meta.tokens.total) || 0
      if (!call || call.status !== 'completed') {
        if (!failed) failed = call ? { callId: call.callId, status: call.status, reason: call.reason } : { callId: null, status: 'failed', reason: 'explorer_thrown' }
        return
      }
      found[r.digest] = { answer: call.output.answer, evidence: call.output.evidence, call: call.callId, findings: call.output }
    })
  }
  const answers = plan.routes.map((r) => {
    let answer = r.answer || ''
    let call = ''
    let ev = []
    let findings = null
    if (r.route === 'spawn' && found[r.digest]) {
      answer = found[r.digest].answer
      call = found[r.digest].call
      ev = found[r.digest].evidence
      findings = found[r.digest].findings
    }
    if (r.route === 'reuse' && r.same_as >= 0) {
      const sib = plan.routes[r.same_as]
      if (sib && found[sib.digest]) answer = found[sib.digest].answer
      else if (sib) answer = sib.answer || ''
    }
    const out = { digest: r.digest, id: r.id, question: r.question, route: r.route, reason: r.reason || '', scope: r.scope || '', authority: r.authority || '', answer, call, evidence: ev, findings }
    // the optional origin only with a value (CRW-1381)
    if (r.origin) out.origin = r.origin
    if (r.route === 'spawn' && r.digest === endsFor) out.end_conditions_asked = ends
    return out
  })
  await crw.exploreRecord(kind, ctx, answers)
  return {
    kind,
    answers,
    spawned: spawn.length,
    inline: plan.routes.filter((r) => r.route === 'inline'),
    orchestrator: plan.routes.filter((r) => r.route === 'orchestrator'),
    escalate: plan.routes.filter((r) => r.route === 'escalate'),
    // the questions the run asked that no answer reached and these questions do not raise again: the next ask carries them (CRW-1381)
    carried: plan.carried || [],
    failed,
    tokens,
  }
}

`

// b0ArchitectPacket is lib/packets.js#architectPacket as it was before CRW-1571 (a531e0503): from its line `export function
// architectPacket(` up to the comment of the executor contract.
const b0ArchitectPacket = `export function architectPacket(ms, background, head) {
  const nodes = ms.nodes.map((n) => n.id + ' (' + n.issue + '): ' + n.title + (n.after.length ? ' after ' + n.after.join(', ') : '') + (n.group ? ' [group ' + n.group + ']' : '') +
    '\n  regions: ' + (n.regions.length ? n.regions.join(', ') : 'undeclared') + '\n  criteria: ' + (n.criteria.length ? n.criteria.join(' | ') : 'none'))
  const parts = [
    'ROLE: architect. Plan milestone ' + ms.id + ' of plan ' + ms.plan + ' (' + ms.title + ').',
    'Answer with DESIGN: the decisions, one plan for each issue node below (its size_fit, the sub_tasks of a split node, edit regions, approach, tests first and done condition), the open questions you cannot answer from what you read, and the risks.',
    section('NODES', nodes),
  ]
  if (background && background.length) {
    parts.push(section('BACKGROUND FINDINGS (' + (head ? 'read at ' + head + ', the commit your directory is a checkout of; ' : '') + 'an explorer read the repository for each node before you; build on them, and ask as an open question only what they and your own reading leave open)',
      background.map((a) => {
        const lines = [(a.id ? a.id + ': ' : '') + a.question]
        if (a.answer) lines.push('  answer: ' + a.answer)
        if (a.findings) lines.push('  findings: ' + canon(a.findings))
        return '- ' + lines.join('\n')
      })))
  }
  return parts.join('\n\n')
}

`

// The open questions, research and evidence questions under the rule of CRW-1571: an explorer whose answer is null_model is asked
// once more on a fresh thread; a question whose explorer fails twice is named as not answered (design-answers' NOT ANSWERED, the
// critic's route line) and the call goes on; the call fails as before only when every explorer it spawned failed.

const (
	lostQuestion  = "where is the retry policy kept?"
	foundQuestion = "is the flag documented?"
)

// explorerCalls are the calls labelled explore:<owner>: whose first turn asked the question, in the order they were made.
func explorerCalls(l *Lab, owner, question string) []CallInfo {
	var out []CallInfo
	for _, c := range l.AllCalls() {
		if strings.HasPrefix(c.Label, "explore:"+owner+":") && strings.Contains(textOfCall(l, c.ID), "QUESTION: "+question) {
			out = append(out, c)
		}
	}
	return out
}

// firstAndRetry checks that calls are one call and its retry (the first one's label with :retry) and returns their ids.
func firstAndRetry(t *testing.T, calls []CallInfo) (string, string) {
	t.Helper()
	if len(calls) != 2 || calls[1].Label != calls[0].Label+":retry" {
		t.Fatalf("explorer calls %v, want one and its retry", labelsOf(calls))
	}
	return calls[0].ID, calls[1].ID
}

func answerOf(text string) func(int, *Call) *fakehostPlan {
	return func(int, *Call) *fakehostPlan {
		p := Say(map[string]any{"answer": text, "evidence": []string{"README.md"}, "verified": []string{"README.md"}, "inferred": []string{}, "unknown": []string{}})
		return &p
	}
}

func openQuestionsLab(t *testing.T, qs ...ops.OpenQuestion) *Lab {
	l := NewLab(t, twoNodes()...)
	d := l.designFor()
	d.OpenQuestions = qs
	l.Design = &d
	return l
}

var (
	lostOpen   = ops.OpenQuestion{ID: "lost", Question: lostQuestion, Scope: "code", FilesNeeded: []string{"a/retry.go"}}
	foundOpen  = ops.OpenQuestion{ID: "found", Question: foundQuestion, Scope: "docs", FilesNeeded: []string{"README.md"}}
	policyOpen = ops.OpenQuestion{ID: "policy", Question: "does the project ship the flag in 1.0?", Scope: "authority:policy: the release scope", FilesNeeded: []string{}}
)

// TestOpenQuestionExplorerThatFailsTwiceIsNotAnswered: of two open questions, one's explorer is null_model on both threads and the
// other's answers. The milestone does not halt: design-answers runs with the answered one under ANSWERS and the other under NOT
// ANSWERED with both calls, the critic's packet says its explorer was asked twice and did not answer, and the open questions' cap
// counts two spawns (the retry is no spawn decision).
func TestOpenQuestionExplorerThatFailsTwiceIsNotAnswered(t *testing.T) {
	l := openQuestionsLab(t, lostOpen, foundOpen)
	scriptExplorer(l, "^explore:M1:", "QUESTION: "+lostQuestion, func(_ int, c *Call) *fakehostPlan { return nullModelTurns(c) })
	scriptExplorer(l, "^explore:M1:", "QUESTION: "+foundQuestion, answerOf("yes, README.md says so"))
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q; escalations %v", got.State, escalationReasons(l))
	}
	if slices.Contains(escalationReasons(l), "explorer_null_model") {
		t.Errorf("escalations %v name explorer_null_model", escalationReasons(l))
	}
	first, retry := firstAndRetry(t, explorerCalls(l, "M1", lostQuestion))
	if found := explorerCalls(l, "M1", foundQuestion); len(found) != 1 {
		t.Errorf("the answered question's explorer calls %v, want one", labelsOf(found))
	}
	calls := "(calls " + first + ", " + retry + ": null_model schema_invalid_after_repair)"
	answers := textOfCall(l, callNamed(t, l, "design-answers:r1").ID)
	listed, notAnswered, ok := strings.Cut(answers, "NOT ANSWERED (the explorer of each was asked twice and did not answer; the question stays open):\n")
	if !ok {
		t.Fatalf("design-answers names no question as not answered:\n%s", answers)
	}
	if want := "- lost: " + lostQuestion + " " + calls; !strings.HasPrefix(notAnswered, want) {
		t.Errorf("NOT ANSWERED does not start with %q:\n%s", want, notAnswered)
	}
	if !strings.Contains(listed, "- found: "+foundQuestion+"\n  answer: yes, README.md says so") || strings.Contains(listed, "- lost: ") {
		t.Errorf("ANSWERS should list the answered question alone:\n%s", listed)
	}
	audit := textOfCall(l, callNamed(t, l, "design-audit:r1").ID)
	for _, want := range []string{
		"lost: " + lostQuestion + "\n  route: an explorer was asked twice and did not answer " + calls,
		"found: " + foundQuestion + "\n  route: an explorer answered it (call ",
	} {
		if !strings.Contains(audit, want) {
			t.Errorf("the critic's packet lacks %q:\n%s", want, audit)
		}
	}
	if spawned := spawnRowsOf(l, ops.RuleExpl1, "spawn"); len(spawned) != 2 {
		t.Errorf("open question spawn decisions %+v, want two", spawned)
	}
}

// TestOpenQuestionsAllFailedHaltAsBefore: every explorer the open questions spawned is null_model on both threads: the milestone
// halts as before CRW-1571 (escalated, source explorer), naming the last retry, with no design-answers and no critic call. A question
// the round asks of the orchestrator is no exploration: it does not keep the milestone going (D1.4: the call fails when every
// explorer it spawned is missing), and it is still asked.
func TestOpenQuestionsAllFailedHaltAsBefore(t *testing.T) {
	l := openQuestionsLab(t, lostOpen, foundOpen, policyOpen)
	scriptExplorer(l, "^explore:M1:", "QUESTION: "+lostQuestion, func(_ int, c *Call) *fakehostPlan { return nullModelTurns(c) })
	scriptExplorer(l, "^explore:M1:", "QUESTION: "+foundQuestion, func(_ int, c *Call) *fakehostPlan { return nullModelTurns(c) })
	res := l.Run()
	if res.Status != engine.RunCompleted {
		t.Fatalf("run %s: %s: %s", res.Status, res.ErrorKind, res.Error)
	}
	got := Value[haltResult](t, res)
	if got.State != "escalated" || got.Source != "explorer" || got.Reason != "schema_invalid_after_repair" {
		t.Errorf("value %s, want escalated by the explorer", res.Value)
	}
	firstAndRetry(t, explorerCalls(l, "M1", lostQuestion))
	_, retry := firstAndRetry(t, explorerCalls(l, "M1", foundQuestion))
	if got.Call != retry {
		t.Errorf("the halt names call %s, want the last retry %s", got.Call, retry)
	}
	if reasons := escalationReasons(l); !slices.Contains(reasons, "explorer_null_model") || !slices.Contains(reasons, "design_question") {
		t.Errorf("escalations %v, want the design question and explorer_null_model", reasons)
	}
	for _, c := range l.AllCalls() {
		if strings.HasPrefix(c.Label, "design-answers:") || strings.HasPrefix(c.Label, "design-audit:") {
			t.Errorf("a %s call after every open-question explorer failed: %v", c.Label, labelsOf(l.AllCalls()))
		}
	}
}

const researchQuestion = "which package owns the retry policy?"

func researchLab(t *testing.T) *Lab {
	l := NewLab(t, oneNode()...)
	l.Sequence("^build:A:executor$", func(c *Call) fakehostPlan {
		return Say(ReceiptWith("needs_research", map[string]any{"research_question": researchQuestion}))
	})
	return l
}

// TestResearchExplorerRetriesThenHoldsTheNode: a research question whose explorer is null_model is asked again on a fresh thread and
// the same executor thread goes on with the retry's answer; when the retry is null_model too, the node is held by the explorer as it
// was before CRW-1571 (the question's explorer was the call's only one), naming the retry.
func TestResearchExplorerRetriesThenHoldsTheNode(t *testing.T) {
	t.Run("retry answers", func(t *testing.T) {
		l := researchLab(t)
		scriptExplorer(l, "^explore:A:", "QUESTION: "+researchQuestion, func(attempt int, c *Call) *fakehostPlan {
			if attempt == 0 {
				return nullModelTurns(c)
			}
			return answerOf("the retry policy lives in a/retry.go")(attempt, c)
		})
		if got := mustRun(t, l); got.State != "closed" {
			t.Fatalf("state %q: %+v", got.State, got)
		}
		firstAndRetry(t, explorerCalls(l, "A", researchQuestion))
		build, research := callNamed(t, l, "build:A:executor"), callNamed(t, l, "research:A:executor:r1")
		if research.Resume != build.ID {
			t.Errorf("the research turn continues %q, want the executor's call %s", research.Resume, build.ID)
		}
		if text := textOfCall(l, research.ID); !strings.Contains(text, "the retry policy lives in a/retry.go") {
			t.Errorf("the executor was not given the retry's answer:\n%s", text)
		}
		if rows := l.Rows("crw.hold"); len(rows) != 0 {
			t.Errorf("hold rows %+v", rows)
		}
	})
	t.Run("retry fails", func(t *testing.T) {
		l := researchLab(t)
		scriptExplorer(l, "^explore:A:", "QUESTION: "+researchQuestion, func(_ int, c *Call) *fakehostPlan { return nullModelTurns(c) })
		got := mustRunIncomplete(t, l)
		if state, reason := laneOf(t, got, "A"); state != "held" || reason != "schema_invalid_after_repair" {
			t.Errorf("lane A: %s %s, want held schema_invalid_after_repair", state, reason)
		}
		_, retry := firstAndRetry(t, explorerCalls(l, "A", researchQuestion))
		if rows := l.Rows("crw.hold"); len(rows) != 1 || str(rows[0].Data, "node") != "A" || str(rows[0].Data, "source") != "explorer" || str(rows[0].Data, "call") != retry {
			t.Errorf("hold rows %+v, want node A held by the explorer's retry %s", rows, retry)
		}
		if slices.Contains(l.Labels(), "research:A:executor:r1") {
			t.Errorf("the executor went on with no answer: %v", l.Labels())
		}
	})
}

// TestEvidenceExplorerRetries: on the old reviewer path, an evidence question's explorer that is null_model is asked once more on a
// fresh thread (one :retry label), and the fix packet carries the retry's answer.
func TestEvidenceExplorerRetries(t *testing.T) {
	const question = "does the retry loop stop at the deadline?"
	l := NewLab(t, oneNode()...)
	reviewerLane(t, l)
	reviews(l, "A", Verdict(false, EvidenceDefect("e1", question, "retry.go")), Verdict(true))
	scriptExplorer(l, "^explore:A:", "QUESTION: "+question, func(attempt int, c *Call) *fakehostPlan {
		if attempt == 0 {
			return nullModelTurns(c)
		}
		return answerOf("the loop stops at the deadline in retry.go:44")(attempt, c)
	})
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q", got.State)
	}
	firstAndRetry(t, explorerCalls(l, "A", question))
	retried := 0
	for _, label := range l.Labels() {
		if strings.HasSuffix(label, ":retry") {
			retried++
		}
	}
	if retried != 1 {
		t.Errorf("calls %v, want one :retry", l.Labels())
	}
	if text := textOfCall(l, callNamed(t, l, "fix:A:executor:r1").ID); !strings.Contains(text, "the loop stops at the deadline in retry.go:44") {
		t.Errorf("the fix packet does not carry the retry's answer:\n%s", text)
	}
}
