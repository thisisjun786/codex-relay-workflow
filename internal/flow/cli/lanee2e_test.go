package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

// twoMilestones is the plan of the two-milestone fixture: M1 holds A-1 and A-2 (A-2 after A-1), M2 holds B-1 after A-2.
func twoMilestones() []labIssue {
	return []labIssue{{Key: "A-1", Milestone: "M1"}, {Key: "A-2", Milestone: "M1", After: []string{"A-1"}}, {Key: "B-1", Milestone: "M2", After: []string{"A-2"}}}
}

// runMilestones starts a project lane run for each milestone in turn and runs it to its end (resuming it after every kill); it answers the
// runs.
func runMilestones(l *laneLab, milestones ...string) []string {
	var runs []string
	for _, ms := range milestones {
		spec := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":%q}`, l.plan, ms), "--handoff", l.file)
		l.closed(l.runToEnd(spec.RunID))
		runs = append(runs, spec.RunID)
	}
	return runs
}

// TestProjectLaneTwoMilestonesOnTempStore (CRW-1277, the flow project's finish line): on a fake host, a temporary store and temporary git,
// `crw flow` runs of the project lane drive a two-milestone plan to integrated heads through the relay's scheduler: every issue node is
// released to an agent of the run, accepted on the agent path with its own acceptance and integrated onto the target branch, and each
// milestone closes. A process killed right after any effect-bearing call of the relay, before the lane recorded the answer, is resumed and
// the plan ends the same: no node is released, accepted or merged twice.
func TestProjectLaneTwoMilestonesOnTempStore(t *testing.T) {
	clean := newLaneLab(t, twoMilestones()...)
	runMilestones(clean, "M1", "M2")
	clean.effectsOnce("A-1", "A-2", "B-1")
	if strings.Join(clean.reports, ",") != "plan-1/M1,plan-1/M2" {
		t.Errorf("the milestone reports sent up: %v", clean.reports)
	}
	writes := clean.writes
	if writes < 9 {
		t.Fatalf("the clean run made %d effect-bearing relay calls: %v", writes, clean.effects)
	}
	t.Logf("the clean run made %d effect-bearing relay calls: %s", writes, strings.Join(clean.effects, " "))
	if t.Failed() || testing.Short() {
		return
	}
	for k := 1; k <= writes; k++ {
		t.Run(fmt.Sprintf("kill after call %d", k), func(t *testing.T) {
			l := newLaneLab(t, twoMilestones()...)
			l.killAt = k
			runMilestones(l, "M1", "M2")
			// the call the process was killed after was asked again by the resumed process, which the relay answered as a replay
			asked := 0
			for _, e := range l.effects {
				if e == l.effects[k-1] {
					asked++
				}
			}
			if l.processes != 3 || asked != 2 {
				t.Fatalf("killed after %s: %d processes, the call asked %d times: %v", l.effects[k-1], l.processes, asked, l.effects)
			}
			l.effectsOnce("A-1", "A-2", "B-1")
		})
	}
	// a process killed after an integration batch moved the branch and before the run observed the landing: the resumed process asks
	// again, the relay merges nothing again, and the observation is made
	t.Run("kill between the batch and its observation", func(t *testing.T) {
		l := newLaneLab(t, twoMilestones()...)
		l.killBetween = l.nodeOf["A-2"]
		runMilestones(l, "M1", "M2")
		if l.processes != 3 {
			t.Fatalf("%d processes: %v", l.processes, l.effects)
		}
		l.effectsOnce("A-1", "A-2", "B-1")
		if n := l.count("SELECT COUNT(*) FROM dag_integration_observations WHERE acceptance_id IN (SELECT acceptance_id FROM dag_agent_acceptances WHERE node_id = ?)", l.nodeOf["A-2"]); n == 0 {
			t.Errorf("the landing of A-2 was never observed")
		}
	})
}

// TestDesignAuditCarriesPriorFindingsAndDispositionsFromRoundTwo (CRW-1452): the design audit is the critic's call, and from its second
// round on the critic's packet carries the findings of the earlier rounds by id (r<round>.<index>) with the architect's disposition of
// each. The critic answers needs_changes in round 1 with one P1 finding; the design-fix packet names the finding by its id and asks for
// one disposition per id; the architect folds it, and the critic approves in round 2 over a packet that shows the finding, its id and
// the disposition with its reason. The first round's packet has no PRIOR FINDINGS.
func TestDesignAuditCarriesPriorFindingsAndDispositionsFromRoundTwo(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	const finding = "the done condition of A-1 names no file a test can read"
	const reason = "the done condition now names node-A-1.txt"
	l.turn = func(c labCall, r fakehost.TurnRequest) (fakehost.TurnPlan, bool) {
		switch c.Label {
		case "design-audit:r1":
			return labSay(map[string]any{"verdict": "needs_changes", "findings": []any{map[string]any{"ref": "issue A-1", "severity": "P1", "finding": finding,
				"evidence": []string{"README.md:1"}}}, "summary": "one P1 finding"}), true
		case "design-audit:r2":
			return labSay(map[string]any{"verdict": "approved", "findings": []any{}, "summary": "the finding is folded"}), true
		case "design-fix:r1":
			d := l.design()
			d.Dispositions = []ops.Disposition{{Finding: "r1.1", Disposition: "folded", Reason: reason}}
			return labSay(d), true
		}
		return fakehost.TurnPlan{}, false
	}
	spec := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	l.closed(l.runToEnd(spec.RunID))

	roles := map[string]string{}
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Event == engine.EvPrepared && strings.HasPrefix(e.Label, "design-") {
			roles[e.Label] = str(e.Data, "role")
		}
	}
	if roles["design-audit:r1"] != "critic" || roles["design-audit:r2"] != "critic" || roles["design-fix:r1"] != "architect" {
		t.Errorf("the roles of the design loop's calls: %v (want both audits the critic's, the fix the architect's)", roles)
	}
	l.mu.Lock()
	r1, fix, r2 := l.texts["design-audit:r1"], l.texts["design-fix:r1"], l.texts["design-audit:r2"]
	l.mu.Unlock()
	if r1 == "" || fix == "" || r2 == "" {
		t.Fatalf("the model was not given every call of the design loop: %v", roles)
	}
	if !strings.Contains(r1, "ROLE: critic.") || strings.Contains(r1, "PRIOR FINDINGS") {
		t.Errorf("the first round's critic packet:\n%s", r1)
	}
	if !strings.Contains(fix, "r1.1 [P1] issue A-1: "+finding) || !strings.Contains(fix, "dispositions") || !strings.Contains(fix, "r1.1") {
		t.Errorf("the design-fix packet does not name the finding by its id and ask for its disposition:\n%s", fix)
	}
	want := "- r1.1 [P1] issue A-1: " + finding + " -> folded: " + reason
	if !strings.Contains(r2, "ROLE: critic.") || !strings.Contains(r2, "PRIOR FINDINGS") || !strings.Contains(r2, want) {
		t.Errorf("the second round's critic packet lacks %q:\n%s", want, r2)
	}
}

// TestReplanCriticAndExplorerCallsCarryTheSignalHead (CRW-1458): a re-plan's critic and explorer calls are made at the head of the
// signal, as the re-plan's architect turn is. Three issues in one milestone, C-1 then A-1 then B-1: C-1 lands first, so the integration
// tip the signal reads is not the head the DESIGN ran at. A-1's build and fix RECEIPTs each report a plan mismatch (its first review
// asks for a fix with one P2 defect), and the second makes the milestone re-plan while B-1 has not started. The re-plan's DESIGN raises
// one open question the first did not, so an explorer runs in its audit round. The re-plan's audit and explorer calls pin the signal's
// tip, and the critic's packet names the head, the range from the DESIGN's base and the heads landed (C-1); the first DESIGN's audit
// and explorer calls pin no head and the critic's packet has no HEAD line; B-1 is released after the re-plan.
func TestReplanCriticAndExplorerCallsCarryTheSignalHead(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "C-1", Milestone: "M1"}, labIssue{Key: "A-1", Milestone: "M1", After: []string{"C-1"}}, labIssue{Key: "B-1", Milestone: "M1", After: []string{"A-1"}})
	nodeA, nodeB, nodeC := l.nodeOf["A-1"], l.nodeOf["B-1"], l.nodeOf["C-1"]
	first := ops.OpenQuestion{ID: "q1", Question: "which file holds the base text?", Scope: "repo", FilesNeeded: []string{"README.md"}}
	again := ops.OpenQuestion{ID: "q2", Question: "which node's file did the landed range add?", Scope: "repo", FilesNeeded: []string{"README.md"}}
	l.designEdit = func(d *ops.Design) { d.OpenQuestions = []ops.OpenQuestion{first} }
	// the number of effect-bearing relay calls made when the re-plan's architect turn ran
	replanAt := -1
	l.turn = func(c labCall, r fakehost.TurnRequest) (fakehost.TurnPlan, bool) {
		switch {
		case strings.HasPrefix(c.Label, "replan:"):
			l.mu.Lock()
			replanAt = len(l.effects)
			l.mu.Unlock()
			d := l.design()
			d.OpenQuestions = append(d.OpenQuestions, again)
			return labSay(d), true
		case strings.HasPrefix(c.Label, "build:"+nodeA+":") || strings.HasPrefix(c.Label, "fix:"+nodeA+":"):
			return l.executorTurn(c, map[string]any{"plan_mismatch": "the plan assumed something else: " + c.Label}), true
		case c.Label == "review:"+nodeA+":r1":
			return labSay(labVerdict(false, labDefect("d1", "P2", "fix this"))), true
		}
		return fakehost.TurnPlan{}, false
	}
	spec := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	l.closed(l.runToEnd(spec.RunID))

	var replans []string
	heads := map[string]string{}
	var firstExplore, replanExplore []string
	for _, e := range l.events() {
		if e.Kind != engine.KindCall || e.Event != engine.EvPrepared || e.Label == "" {
			continue
		}
		heads[e.Label] = str(e.Data, "head")
		if strings.HasPrefix(e.Label, "replan:") {
			replans = append(replans, e.Label)
		}
		if strings.HasPrefix(e.Label, "explore:") {
			if len(replans) == 0 {
				firstExplore = append(firstExplore, e.Label)
			} else {
				replanExplore = append(replanExplore, e.Label)
			}
		}
	}
	if len(replans) != 1 {
		t.Fatalf("the re-plan calls %v (want one); effects %v", replans, l.effects)
	}
	tag := strings.TrimPrefix(replans[0], "replan:")
	tip := heads[replans[0]]
	base := l.git(l.repo, "rev-parse", "main")
	if tip == "" || tip == base {
		t.Fatalf("the re-plan's head %q (the DESIGN's base %s): C-1 landed before the signal", tip, base)
	}
	audit := "design-audit:replan-" + tag + ":r1"
	if heads[audit] != tip {
		t.Errorf("the re-plan's audit call %s pins head %q, want the signal's tip %s; heads %v", audit, heads[audit], tip, heads)
	}
	for label, head := range heads {
		if strings.HasPrefix(label, "design-audit:replan-"+tag+":") && head != tip {
			t.Errorf("the re-plan's audit call %s pins head %q, want %s", label, head, tip)
		}
	}
	if len(replanExplore) != 1 || heads[replanExplore[0]] != tip {
		t.Errorf("the re-plan's explorer calls %v (want one, at head %s); heads %v", replanExplore, tip, heads)
	}
	if len(firstExplore) != 1 || heads[firstExplore[0]] != "" || heads["design-audit:r1"] != "" {
		t.Errorf("the first DESIGN's explorer calls %v and audit pin heads %v, want one explorer and no head", firstExplore, heads)
	}
	l.mu.Lock()
	r1, rp := l.texts["design-audit:r1"], l.texts[audit]
	at := replanAt
	effects := append([]string(nil), l.effects...)
	l.mu.Unlock()
	if r1 == "" || strings.Contains(r1, "HEAD: ") || strings.Contains(r1, "LANDED HEADS") {
		t.Errorf("the first DESIGN's critic packet names a head:\n%s", r1)
	}
	for _, want := range []string{"HEAD: " + tip, "RANGE: " + base + ".." + tip, "LANDED HEADS:\n- " + nodeC + ": ", "read-only checkout of the pinned head"} {
		if !strings.Contains(rp, want) {
			t.Errorf("the re-plan's critic packet lacks %q:\n%s", want, rp)
		}
	}
	release := -1
	for i, e := range effects {
		if e == "release:"+nodeB {
			release = i
		}
	}
	if at < 0 || release < at {
		t.Errorf("B-1 was released at effect %d, the re-plan ran after %d effects: %v", release, at, effects)
	}
}

// TestExplorersRunBeforeTheArchitectAndTheirFindingsReachItsPacket (CRW-1466): before the architect's first DESIGN, the milestone puts
// one background question for each of its nodes to an explorer (crw.explore of kind background, SP-EXPL-1, outside the open questions'
// cap), in a phase explore of its own. Each explorer call is labelled explore-bg:<milestone>:<digest12>, all of them run before the
// architect call in journal order, and the architect's packet carries BACKGROUND FINDINGS with each explorer's answer. The explorer's
// packet says it was asked in the background of the milestone's architect.
func TestExplorersRunBeforeTheArchitectAndTheirFindingsReachItsPacket(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1", After: []string{"A-1"}})
	answerOf := func(issue string) string { return "the background of " + issue + ": README.md holds its base text" }
	l.turn = func(c labCall, r fakehost.TurnRequest) (fakehost.TurnPlan, bool) {
		if !strings.HasPrefix(c.Label, "explore-bg:") {
			return fakehost.TurnPlan{}, false
		}
		for _, issue := range []string{"A-1", "A-2"} {
			if strings.Contains(r.Text, "("+issue+")") {
				return labSay(map[string]any{"answer": answerOf(issue), "evidence": []string{"README.md"}, "verified": []string{"README.md"}, "inferred": []string{}, "unknown": []string{}}), true
			}
		}
		return labSay(map[string]any{"answer": "no node named", "evidence": []string{}, "verified": []string{}, "inferred": []string{}, "unknown": []string{}}), true
	}
	spec := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	l.closed(l.runToEnd(spec.RunID))

	msID := ""
	phaseOf := map[string]string{}
	var order []string
	cur := ""
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Op == ops.OpMilestone && e.Event == engine.EvCompleted && msID == "" {
			var ms ops.Milestone
			if s, ok := e.Data["result"].(string); ok && json.Unmarshal([]byte(s), &ms) == nil {
				msID = ms.ID
			}
		}
		if e.Kind == engine.KindFlow && e.Event == "phase" {
			cur = str(e.Data, "title")
		}
		if e.Kind == engine.KindCall && e.Event == engine.EvPrepared && e.Op == "agent" && e.Label != "" {
			if _, seen := phaseOf[e.Label]; !seen {
				order = append(order, e.Label)
			}
			phaseOf[e.Label] = cur
		}
	}
	if msID == "" {
		t.Fatalf("the run read no milestone")
	}
	var bg []string
	architect := -1
	for i, label := range order {
		if strings.HasPrefix(label, "explore-bg:") {
			bg = append(bg, label)
			if !strings.HasPrefix(label, "explore-bg:"+msID+":") || phaseOf[label] != "explore" {
				t.Errorf("the background explorer call %s ran in phase %q (want explore-bg:%s:*, in phase explore)", label, phaseOf[label], msID)
			}
			if architect >= 0 {
				t.Errorf("the background explorer call %s came after the architect call: %v", label, order)
			}
		}
		if label == "architect" {
			architect = i
		}
	}
	if len(bg) != 2 || architect < 0 || phaseOf["architect"] != "design" {
		t.Fatalf("the background explorer calls %v (want one per node, 2) and the architect call at %d in phase %q: %v", bg, architect, phaseOf["architect"], order)
	}
	l.mu.Lock()
	arch := l.texts["architect"]
	exp := l.texts[bg[0]]
	l.mu.Unlock()
	if !strings.Contains(arch, "BACKGROUND FINDINGS") {
		t.Fatalf("the architect packet has no BACKGROUND FINDINGS:\n%s", arch)
	}
	for _, issue := range []string{"A-1", "A-2"} {
		if !strings.Contains(arch, answerOf(issue)) {
			t.Errorf("the architect packet lacks the background answer of %s:\n%s", issue, arch)
		}
	}
	for _, leak := range []string{"route:", "spawned_background", "scope_notation", "origin"} {
		if strings.Contains(arch, leak) {
			t.Errorf("the architect packet renders the routing metadata %q:\n%s", leak, arch)
		}
	}
	if !strings.Contains(exp, "ASKED BY: the run, in the background of the architect of milestone "+msID+" (background)") {
		t.Errorf("the background explorer's packet:\n%s", exp)
	}
}

// TestArchitectContinuesOnItsThreadWithExplorerAnswersBeforeTheCritic (CRW-1466): the answers an explorer gave to the open questions
// of a DESIGN go back to the architect before the design audit. The first DESIGN raises one open question, an explorer answers it,
// and a design-answers:r1 call resumes the architect's thread (the same thread) with that answer, after the explorer and before
// design-audit:r1; its packet carries the question and the answer and no routing metadata, and the DESIGN the critic audits is the
// one that call answered with.
func TestArchitectContinuesOnItsThreadWithExplorerAnswersBeforeTheCritic(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	q := ops.OpenQuestion{ID: "q1", Question: "which file holds the base text?", Scope: "repo", FilesNeeded: []string{"README.md"}}
	l.designEdit = func(d *ops.Design) { d.OpenQuestions = []ops.OpenQuestion{q} }
	const answer = "README.md holds the base text, on its first line"
	const revised = "write the file of A-1 after reading the first line of README.md"
	threads := map[string]string{}
	l.turn = func(c labCall, r fakehost.TurnRequest) (fakehost.TurnPlan, bool) {
		l.mu.Lock()
		if c.Label != "" {
			threads[c.Label] = r.ThreadID
		}
		l.mu.Unlock()
		switch {
		case strings.HasPrefix(c.Label, "explore:"):
			return labSay(map[string]any{"answer": answer, "evidence": []string{"README.md:1"}, "verified": []string{"README.md"}, "inferred": []string{}, "unknown": []string{}}), true
		case c.Label == "design-answers:r1":
			d := l.design()
			d.Issues[0].Approach = revised
			return labSay(d), true
		}
		return fakehost.TurnPlan{}, false
	}
	spec := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	l.closed(l.runToEnd(spec.RunID))

	var order []string
	roles := map[string]string{}
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Event == engine.EvPrepared && e.Op == "agent" && e.Label != "" {
			if _, seen := roles[e.Label]; !seen {
				order = append(order, e.Label)
			}
			roles[e.Label] = str(e.Data, "role")
		}
	}
	at := func(want func(string) bool) int {
		for i, label := range order {
			if want(label) {
				return i
			}
		}
		return -1
	}
	explorer := at(func(s string) bool { return strings.HasPrefix(s, "explore:") })
	answers := at(func(s string) bool { return s == "design-answers:r1" })
	audit := at(func(s string) bool { return s == "design-audit:r1" })
	if explorer < 0 || answers < 0 || audit < 0 || !(explorer < answers && answers < audit) {
		t.Fatalf("the order of the design calls %v: want the open question's explorer, then design-answers:r1, then design-audit:r1", order)
	}
	if roles["design-answers:r1"] != "architect" {
		t.Errorf("design-answers:r1 is the %q's call, want the architect's", roles["design-answers:r1"])
	}
	l.mu.Lock()
	archThread, answersThread := threads["architect"], threads["design-answers:r1"]
	text, critic := l.texts["design-answers:r1"], l.texts["design-audit:r1"]
	l.mu.Unlock()
	if archThread == "" || answersThread != archThread {
		t.Errorf("design-answers:r1 ran on thread %q, want the architect's thread %q", answersThread, archThread)
	}
	for _, want := range []string{"q1: " + q.Question, answer} {
		if !strings.Contains(text, want) {
			t.Errorf("the design-answers packet lacks %q:\n%s", want, text)
		}
	}
	for _, leak := range []string{"route:", "origin", "an explorer answered it"} {
		if strings.Contains(text, leak) {
			t.Errorf("the design-answers packet renders the routing metadata %q:\n%s", leak, text)
		}
	}
	if !strings.Contains(critic, revised) {
		t.Errorf("the critic did not audit the DESIGN design-answers:r1 answered with:\n%s", critic)
	}
}
