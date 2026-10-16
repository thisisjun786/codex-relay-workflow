package cli

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
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
// tip the signal reads is not the head the DESIGN ran at. A-1's build and fix RECEIPTs each report a plan mismatch (its first unit
// check asks for a fix with one P2 finding), and the second makes the milestone re-plan while B-1 has not started. The re-plan's DESIGN raises
// the first DESIGN's open question again and one the first did not; an explorer's answer reads one head, so both go to explorers in its
// audit round. The re-plan's audit and explorer calls pin the signal's tip, and the critic's packet names the head, the range from the DESIGN's base and the heads landed (C-1); the first DESIGN's audit
// and explorer calls pin the design head its phase sampled and the critic's packet has no HEAD line; B-1 is released after the re-plan.
func TestReplanCriticAndExplorerCallsCarryTheSignalHead(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "C-1", Milestone: "M1"}, labIssue{Key: "A-1", Milestone: "M1", After: []string{"C-1"}}, labIssue{Key: "B-1", Milestone: "M1", After: []string{"A-1"}})
	nodeA, nodeB, nodeC := l.nodeOf["A-1"], l.nodeOf["B-1"], l.nodeOf["C-1"]
	first := ops.OpenQuestion{ID: "q1", Question: "which file holds the base text?", Scope: "repo", FilesNeeded: []string{"README.md"}}
	again := ops.OpenQuestion{ID: "q2", Question: "which node's file did the landed range add?", Scope: "repo", FilesNeeded: []string{"README.md"}}
	l.designEdit = func(d *ops.Design) { d.OpenQuestions = []ops.OpenQuestion{first} }
	// the number of effect-bearing relay calls made when the re-plan's architect turn ran
	replanAt := -1
	// A-1's first unit check asks for a fix with one P2 finding
	l.UnitChecks("A-1", ops.UnitCheckUncommitted, labCheck{Verdict: reviewadapter.VerdictNeedsChanges, Findings: []reviewadapter.Finding{{Severity: "P2", Impact: "core_function",
		File: "node-A-1.txt", Line: 1, Text: "fix this"}}})
	l.turn = func(c labCall, r fakehost.TurnRequest) (fakehost.TurnPlan, bool) {
		switch {
		case strings.HasPrefix(c.Label, "replan:"):
			l.mu.Lock()
			replanAt = len(l.effects)
			l.mu.Unlock()
			d := l.design()
			d.OpenQuestions = append(d.OpenQuestions, again)
			// the re-plan rewrites the started A-1's tests and done condition: the critic must see what A-1 was released with
			for i := range d.Issues {
				if d.Issues[i].Node == nodeA {
					d.Issues[i].TestsFirst = []string{"a test A-1 was not released with"}
					d.Issues[i].DoneCondition = "a done condition A-1 was not released with"
				}
			}
			return labSay(d), true
		case strings.HasPrefix(c.Label, "build:"+nodeA+":") || strings.HasPrefix(c.Label, "fix:"+nodeA+":"):
			return l.executorTurn(c, map[string]any{"plan_mismatch": "the plan assumed something else: " + c.Label}), true
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
	if len(replanExplore) != 2 || heads[replanExplore[0]] != tip || heads[replanExplore[1]] != tip {
		t.Errorf("the re-plan's explorer calls %v (want two, at head %s); heads %v", replanExplore, tip, heads)
	}
	if len(firstExplore) != 1 || heads[firstExplore[0]] != base || heads["design-audit:r1"] != base {
		t.Errorf("the first DESIGN's explorer calls %v and audit pin heads %v, want one explorer and the design head %s", firstExplore, heads, base)
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
	// the started nodes (C-1, A-1) reach the critic as they were released, B-1 (unstarted) does not
	_, released, _ := strings.Cut(rp, "RELEASED PLANS")
	released, _, _ = strings.Cut(released, "\n\n")
	for _, n := range []string{nodeC, nodeA} {
		if !strings.Contains(released, "- "+n+" (") || !strings.Contains(released, `"approach":"write the file of `+n+`"`) ||
			!strings.Contains(released, `"tests_first":["the file of `+n+` exists"]`) || !strings.Contains(released, `"done_condition":"the file of `+n+` is committed"`) ||
			!strings.Contains(released, `"size_fit":"executor"`) {
			t.Errorf("the re-plan's critic packet lacks the whole released plan of %s:\n%s", n, rp)
		}
	}
	if strings.Contains(released, "- "+nodeB+" (") {
		t.Errorf("the re-plan's critic packet lists the unstarted %s as released:\n%s", nodeB, released)
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

// TestDesignPhaseReadsOneHead: the run samples one design head before the phase explore, and every background explorer and the
// architect's first call read it. A candidate lands on the integration target while the first background explorer runs: the second
// explorer and the architect still read the sampled head, the architect's checkout is made at it, the ledger's r0 records it as the
// head the DESIGN ran at, and the architect's packet names it with the background findings.
func TestDesignPhaseReadsOneHead(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1", After: []string{"A-1"}})
	before := l.git(l.repo, "rev-parse", "integration")
	landed := ""
	l.turn = func(c labCall, r fakehost.TurnRequest) (fakehost.TurnPlan, bool) {
		if !strings.HasPrefix(c.Label, "explore-bg:") {
			return fakehost.TurnPlan{}, false
		}
		l.mu.Lock()
		first := landed == ""
		if first {
			// another candidate lands while the phase runs
			tree := l.git(l.repo, "rev-parse", "integration^{tree}")
			landed = l.git(l.repo, "commit-tree", tree, "-p", "integration", "-m", "landed meanwhile")
			l.git(l.repo, "update-ref", "refs/heads/integration", landed)
		}
		l.mu.Unlock()
		return labSay(map[string]any{"answer": "README.md holds the base text", "evidence": []string{"README.md"}, "verified": []string{}, "inferred": []string{}, "unknown": []string{}}), true
	}
	spec := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	l.closed(l.runToEnd(spec.RunID))
	if landed == "" || landed == before {
		t.Fatalf("no candidate landed during the phase explore")
	}

	heads, starts := map[string]string{}, map[string]string{}
	baseTip := ""
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Event == engine.EvPrepared && e.Op == "agent" && e.Label != "" {
			heads[e.Label] = str(e.Data, "head")
			if alloc, ok := e.Data["allocation"].(map[string]any); ok {
				starts[e.Label] = str(alloc, "start_commit")
			}
		}
		if e.Kind == engine.KindCall && e.Op == ops.OpDesignLedger && e.Event == engine.EvCompleted && baseTip == "" {
			baseTip = str(e.Data, "base_tip")
		}
	}
	bg := 0
	for label, head := range heads {
		if strings.HasPrefix(label, "explore-bg:") {
			bg++
			if head != before || starts[label] != before {
				t.Errorf("the background explorer %s reads head %q at %q, want the sampled design head %s (a candidate landed at %s)", label, head, starts[label], before, landed)
			}
		}
	}
	if bg != 2 {
		t.Fatalf("%d background explorers, want 2: %v", bg, heads)
	}
	if heads["architect"] != before || starts["architect"] != before {
		t.Errorf("the architect reads head %q at %q, want the sampled design head %s", heads["architect"], starts["architect"], before)
	}
	if baseTip != before {
		t.Errorf("r0 records the DESIGN ran at %q, want %s", baseTip, before)
	}
	l.mu.Lock()
	arch := l.texts["architect"]
	l.mu.Unlock()
	if !strings.Contains(arch, "BACKGROUND FINDINGS (read at "+before+",") {
		t.Errorf("the architect packet does not name the head of the background findings:\n%s", arch)
	}
}

// TestFirstDesignAuditLoopReadsTheDesignHead: the whole audit loop of the first DESIGN reads the design head the phase sampled, as the
// background explorers and the architect do. Commits land on the integration target during the background phase and again while the
// open question's explorer runs; the open question's explorer, design-answers, the critic of every round (its null_model retry
// included) and design-fix all name the sampled head and run at it, r0 records it, and the critic's packet of the first DESIGN has no
// re-plan head block.
func TestFirstDesignAuditLoopReadsTheDesignHead(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	q := ops.OpenQuestion{ID: "q1", Question: "which file holds the base text?", Scope: "repo", FilesNeeded: []string{"README.md"}}
	l.designEdit = func(d *ops.Design) { d.OpenQuestions = []ops.OpenQuestion{q} }
	before := l.git(l.repo, "rev-parse", "integration")
	lands := 0
	land := func(msg string) {
		tree := l.git(l.repo, "rev-parse", "integration^{tree}")
		next := l.git(l.repo, "commit-tree", tree, "-p", "integration", "-m", msg)
		l.git(l.repo, "update-ref", "refs/heads/integration", next)
		lands++
	}
	l.turn = func(c labCall, r fakehost.TurnRequest) (fakehost.TurnPlan, bool) {
		l.mu.Lock()
		defer l.mu.Unlock()
		switch {
		case strings.HasPrefix(c.Label, "explore-bg:") && lands == 0:
			land("landed during the background phase")
		case strings.HasPrefix(c.Label, "explore:") && lands == 1:
			land("landed during the audit loop")
		case c.Label == "design-audit:r1":
			return labSay(map[string]any{"verdict": "needs_changes", "findings": []any{map[string]any{"ref": "D1", "severity": "P2", "finding": "state the order", "evidence": []string{}}}, "summary": "one thing"}), true
		case c.Label == "design-audit:r2":
			return fakehost.TurnPlan{Message: "not json"}, true
		}
		return fakehost.TurnPlan{}, false
	}
	spec := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	l.closed(l.runToEnd(spec.RunID))
	if lands != 2 {
		t.Fatalf("%d commits landed during the design phase, want 2", lands)
	}
	heads, starts := map[string]string{}, map[string]string{}
	baseTip := ""
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Event == engine.EvPrepared && e.Op == "agent" && e.Label != "" {
			heads[e.Label] = str(e.Data, "head")
			if alloc, ok := e.Data["allocation"].(map[string]any); ok {
				starts[e.Label] = str(alloc, "start_commit")
			}
		}
		if e.Kind == engine.KindCall && e.Op == ops.OpDesignLedger && e.Event == engine.EvCompleted && baseTip == "" {
			baseTip = str(e.Data, "base_tip")
		}
	}
	seen := map[string]bool{}
	for label, head := range heads {
		for _, prefix := range []string{"architect", "explore:", "design-answers:", "design-audit:", "design-fix:"} {
			if !strings.HasPrefix(label, prefix) {
				continue
			}
			seen[prefix] = true
			if head != before || starts[label] != before {
				t.Errorf("%s names head %q and runs at %q, want the sampled design head %s", label, head, starts[label], before)
			}
		}
	}
	for _, label := range []string{"design-audit:r1", "design-fix:r1", "design-audit:r2", "design-audit:r2:retry", "design-answers:r1"} {
		if _, ok := heads[label]; !ok {
			t.Errorf("no call %s: %v", label, heads)
		}
	}
	if len(seen) != 5 {
		t.Errorf("the calls of the first DESIGN's loop: %v", heads)
	}
	if baseTip != before {
		t.Errorf("r0 records the DESIGN ran at %q, want %s", baseTip, before)
	}
	l.mu.Lock()
	r1 := l.texts["design-audit:r1"]
	l.mu.Unlock()
	if strings.Contains(r1, "HEAD: ") || strings.Contains(r1, "LANDED HEADS") || strings.Contains(r1, "RELEASED PLANS") || strings.Contains(r1, "UNSTARTED NODES") {
		t.Errorf("the first DESIGN's critic packet carries re-plan sections:\n%s", r1)
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

// ---- the unit check of the node lane (CRW-1465) ----

// unitCheckRun starts a project lane run of M1 on the lab and runs it to its end; it answers the run's value.
func unitCheckRun(t *testing.T, l *laneLab) (Result, map[string]any) {
	t.Helper()
	spec := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	res := l.runToEnd(spec.RunID)
	var v map[string]any
	if res.Status != engine.RunCompleted || json.Unmarshal(res.Value, &v) != nil {
		t.Fatalf("the run: %s %s %s\n%s", res.Status, res.ErrorKind, res.Error, res.Value)
	}
	return res, v
}

// laneOfValue is the lane result of a node in a run's value.
func laneOfValue(t *testing.T, v map[string]any, node string) map[string]any {
	t.Helper()
	lanes, _ := v["lanes"].([]any)
	for _, ln := range lanes {
		if m, ok := ln.(map[string]any); ok && m["node"] == node {
			return m
		}
	}
	t.Fatalf("no lane of %s in %v", node, v)
	return nil
}

// labCalls are the agent calls of the lab's last process, as their prepared rows name them (label, role, the call they continue).
func labCalls(l *laneLab) []struct{ ID, Label, Role, ResumeOf string } {
	var out []struct{ ID, Label, Role, ResumeOf string }
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Op == "agent" && e.Event == engine.EvPrepared {
			out = append(out, struct{ ID, Label, Role, ResumeOf string }{e.CallID, e.Label, str(e.Data, "role"), str(e.Data, "resume_of")})
		}
	}
	return out
}

// TestNodeLaneRunsTheUnitCheckBeforeTheCommitAndMakesNoReviewerCall (CRW-1465): the node lane checks the executor's uncommitted work
// with crw.unitCheck before the run commits it, then commits, verifies, accepts and integrates the head; no reviewer is called for the
// node and no crw.review row is written. The relay's acceptance rests on the crw.unitCheck row (its call and the checker's model).
func TestNodeLaneRunsTheUnitCheckBeforeTheCommitAndMakesNoReviewerCall(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	node := l.nodeOf["A-1"]
	// a reviewer of the node, were one called, would block the head
	l.turn = func(c labCall, r fakehost.TurnRequest) (fakehost.TurnPlan, bool) {
		if c.Role == "reviewer" {
			return labSay(labVerdict(false, labDefect("d1", "P1", "a reviewer was called"))), true
		}
		return fakehost.TurnPlan{}, false
	}
	res, _ := unitCheckRun(t, l)
	l.closed(res)
	at := map[string]int{}
	check := ""
	for i, e := range l.events() {
		if e.Kind != engine.KindCall || e.Event != engine.EvCompleted || str(e.Data, "node") != node {
			continue
		}
		switch e.Op {
		case ops.OpUnitCheck, ops.OpCommit, ops.OpVerify, ops.OpAccept, ops.OpIntegrate:
			if _, seen := at[e.Op]; !seen {
				at[e.Op] = i
			}
			if e.Op == ops.OpUnitCheck {
				check = e.CallID
				if str(e.Data, "role") != "unit-check" || str(e.Data, "mode") != ops.UnitCheckUncommitted || str(e.Data, "call") == "" {
					t.Errorf("the unit-check row: %v", e.Data)
				}
			}
		case ops.OpReview:
			t.Errorf("a crw.review row of %s: %v", node, e.Data)
		}
	}
	order := []string{ops.OpUnitCheck, ops.OpCommit, ops.OpVerify, ops.OpAccept, ops.OpIntegrate}
	for i, op := range order {
		if _, ok := at[op]; !ok {
			t.Fatalf("no completed %s row of %s: %v", op, node, at)
		}
		if i > 0 && at[order[i-1]] >= at[op] {
			t.Errorf("%s (row %d) is not before %s (row %d)", order[i-1], at[order[i-1]], op, at[op])
		}
	}
	for _, c := range labCalls(l) {
		if c.Role == "reviewer" || strings.HasPrefix(c.Label, "review:") {
			t.Errorf("a reviewer call %s (%s)", c.Label, c.Role)
		}
	}
	reqs := l.uc.requests()
	if len(reqs) != 1 || reqs[0].Node != node || reqs[0].Mode != ops.UnitCheckUncommitted {
		t.Errorf("the unit checks asked: %+v", reqs)
	}
	var call, model string
	if err := l.w.st.DB.QueryRow("SELECT reviewer_call_id, reviewer_model FROM dag_agent_acceptances WHERE node_id = ?", node).Scan(&call, &model); err != nil {
		t.Fatal(err)
	}
	if call != check || model != labCheckModel {
		t.Errorf("the acceptance rests on %s (%s), want the unit check %s (%s)", call, model, check, labCheckModel)
	}
}

// TestUnitCheckFindingGoesBackToTheSameExecutorThread (CRW-1465): a unit check with a finding sends the finding back to the executor on its
// own thread (fix:<node>:<tier>:r<round>, a continuation of the build), and the fix packet names the finding by its id, file and text.
// The fix turn answers needs_research: the lane answers it through the research branch on the same thread, and the next unit check of
// the continuation's work is verified, so the node lands with one commit.
func TestUnitCheckFindingGoesBackToTheSameExecutorThread(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	node := l.nodeOf["A-1"]
	f := reviewadapter.Finding{Severity: "P2", Impact: "core_function", File: "node-A-1.txt", Line: 1, Text: "the file misses the second line"}
	l.UnitChecks("A-1", ops.UnitCheckUncommitted, labCheck{Verdict: reviewadapter.VerdictNeedsChanges, Findings: []reviewadapter.Finding{f}})
	l.turn = func(c labCall, r fakehost.TurnRequest) (fakehost.TurnPlan, bool) {
		if strings.HasPrefix(c.Label, "fix:"+node+":") {
			return l.executorTurn(c, map[string]any{"outcome": "needs_research", "research_question": "which line does the check want?"}), true
		}
		return fakehost.TurnPlan{}, false
	}
	res, _ := unitCheckRun(t, l)
	l.closed(res)
	byLabel := map[string]struct{ ID, Label, Role, ResumeOf string }{}
	var labels []string
	for _, c := range labCalls(l) {
		byLabel[c.Label] = c
		labels = append(labels, c.Label)
	}
	build, fix, research := byLabel["build:"+node+":executor"], byLabel["fix:"+node+":executor:r1"], byLabel["research:"+node+":executor:r2"]
	if build.ID == "" || fix.ID == "" || research.ID == "" {
		t.Fatalf("the calls %v: want the build, the fix of round 1 and the research continuation of round 2", labels)
	}
	if fix.ResumeOf != build.ID || research.ResumeOf != fix.ID {
		t.Errorf("the fix continues %q and the research %q, want the executor's own thread: %s then %s", fix.ResumeOf, research.ResumeOf, build.ID, fix.ID)
	}
	explorers := 0
	for _, c := range labCalls(l) {
		if c.Role == "explorer" && strings.HasPrefix(c.Label, "explore:") && strings.Contains(c.Label, node) {
			explorers++
		}
	}
	if explorers != 1 {
		t.Errorf("%d explorers of the node's research question, want 1: %v", explorers, labels)
	}
	l.mu.Lock()
	text := l.texts[fix.Label]
	l.mu.Unlock()
	id := reviewadapter.FindingID(f.File, f.Text)
	for _, want := range []string{"UNIT CHECK (needs_changes", "[P2] " + id + " node-A-1.txt:1 the file misses the second line", "needs_research"} {
		if !strings.Contains(text, want) {
			t.Errorf("the fix packet lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "REVIEW (") || strings.Contains(text, "CRITERIA NOT MET") {
		t.Errorf("the fix packet of a unit check renders a reviewer's verdict:\n%s", text)
	}
	if n := len(l.uc.requests()); n != 2 {
		t.Errorf("%d unit checks, want the build's and the research continuation's", n)
	}
	commits := 0
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Op == ops.OpCommit && e.Event == engine.EvCompleted && str(e.Data, "node") == node {
			commits++
		}
	}
	if commits != 1 {
		t.Errorf("%d commits of the node, want one after the verified check", commits)
	}
}

// TestUnitCheckStopsAfterTwoRoundTripsAndCarriesTheRest (CRW-1465): a unit check with findings gets at most two fix round trips on a tier.
// Three checks each find a different blocking finding: the executor fixes twice, the third check is final and gets no fix, and the node
// escalates (ask land, unit_check_carried) with the carried finding ids; nothing is committed after it and nothing is accepted. When the
// third check is verified with a P3 finding only, the node lands and the finding is carried: in the lane result and as a defect of the
// premerge record of the acceptance.
func TestUnitCheckStopsAfterTwoRoundTripsAndCarriesTheRest(t *testing.T) {
	finding := func(sev, text string) reviewadapter.Finding {
		impact := "core_function"
		if sev == "P3" {
			impact = "minor_separable"
		}
		return reviewadapter.Finding{Severity: sev, Impact: impact, File: "node-A-1.txt", Line: 1, Text: text}
	}
	nc := func(f reviewadapter.Finding) labCheck {
		return labCheck{Verdict: reviewadapter.VerdictNeedsChanges, Findings: []reviewadapter.Finding{f}}
	}
	f1, f2 := finding("P2", "the first thing is wrong"), finding("P2", "a second thing is wrong")
	fixesOf := func(l *laneLab, node string) []string {
		var out []string
		for _, c := range labCalls(l) {
			if strings.HasPrefix(c.Label, "fix:"+node+":") {
				out = append(out, c.Label)
			}
		}
		return out
	}
	finals := func(l *laneLab, node string) []bool {
		var out []bool
		for _, e := range l.events() {
			if e.Kind == engine.KindCall && e.Op == ops.OpUnitCheck && e.Event == engine.EvCompleted && str(e.Data, "node") == node {
				b, _ := e.Data["final"].(bool)
				out = append(out, b)
			}
		}
		return out
	}

	t.Run("a blocking finding after two round trips escalates", func(t *testing.T) {
		l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
		node := l.nodeOf["A-1"]
		f3 := finding("P1", "a third thing is wrong")
		l.UnitChecks("A-1", ops.UnitCheckUncommitted, nc(f1), nc(f2), nc(f3))
		_, v := unitCheckRun(t, l)
		if ln := laneOfValue(t, v, node); ln["state"] != "escalated" || ln["reason"] != "unit_check_carried" {
			t.Fatalf("the lane of %s: %v", node, ln)
		}
		if fixes := fixesOf(l, node); strings.Join(fixes, ",") != "fix:"+node+":executor:r1,fix:"+node+":executor:r2" {
			t.Errorf("the fix calls %v, want two", fixes)
		}
		if n := len(l.uc.requests()); n != 3 {
			t.Errorf("%d unit checks, want 3", n)
		}
		if got := finals(l, node); fmt.Sprint(got) != "[false false true]" {
			t.Errorf("the final flags of the checks %v, want only the third final", got)
		}
		var esc []engine.Event
		for _, e := range l.events() {
			if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation && str(e.Data, "node") == node {
				esc = append(esc, e)
			}
		}
		id3 := reviewadapter.FindingID(f3.File, f3.Text)
		if len(esc) != 1 || str(esc[0].Data, "reason") != "unit_check_carried" || str(esc[0].Data, "ask") != "land" || !strings.Contains(str(esc[0].Data, "detail"), id3+" node-A-1.txt "+f3.Text) {
			t.Fatalf("the escalations of %s: %+v", node, esc)
		}
		ev, _ := esc[0].Data["evidence"].(map[string]any)
		if carried, _ := ev["carried"].([]any); len(carried) != 1 || carried[0] != id3 {
			t.Errorf("the escalation carries %v, want [%s]", ev, id3)
		}
		for _, e := range l.events() {
			if e.Kind == engine.KindCall && (e.Op == ops.OpCommit || e.Op == ops.OpAccept) && e.Event == engine.EvCompleted && str(e.Data, "node") == node {
				t.Errorf("a %s row after the carried blocking finding: %v", e.Op, e.Data)
			}
		}
		if n := l.count("SELECT COUNT(*) FROM dag_agent_acceptances WHERE node_id = ?", node); n != 0 {
			t.Errorf("%d acceptances of %s", n, node)
		}
	})

	t.Run("a P3 finding after two round trips lands carried", func(t *testing.T) {
		l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
		node := l.nodeOf["A-1"]
		f3 := finding("P3", "a name could be clearer")
		l.UnitChecks("A-1", ops.UnitCheckUncommitted, nc(f1), nc(f2), labCheck{Verdict: reviewadapter.VerdictVerified, Findings: []reviewadapter.Finding{f3}})
		res, v := unitCheckRun(t, l)
		l.closed(res)
		id3 := reviewadapter.FindingID(f3.File, f3.Text)
		ln := laneOfValue(t, v, node)
		if carried, _ := ln["carried"].([]any); ln["state"] != "landed" || len(carried) != 1 || carried[0] != id3 {
			t.Fatalf("the lane of %s: %v, want landed with %s carried", node, ln, id3)
		}
		if fixes := fixesOf(l, node); len(fixes) != 2 {
			t.Errorf("the fix calls %v, want two", fixes)
		}
		if got := finals(l, node); fmt.Sprint(got) != "[false false true]" {
			t.Errorf("the final flags of the checks %v", got)
		}
		var pm string
		if err := l.w.st.DB.QueryRow("SELECT premerge_json FROM dag_agent_acceptances WHERE node_id = ?", node).Scan(&pm); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(pm, id3) {
			t.Errorf("the premerge record does not carry %s:\n%s", id3, pm)
		}
	})
}

// ---- the crosscheck of the milestone close (CRW-1473) ----

// labRows are the completed rows of an op of the lab's last process, in order, with their index in the journal.
func labRows(l *laneLab, op string) ([]engine.Event, []int) {
	var out []engine.Event
	var at []int
	for i, e := range l.events() {
		if e.Kind == engine.KindCall && e.Op == op && e.Event == engine.EvCompleted {
			out = append(out, e)
			at = append(at, i)
		}
	}
	return out, at
}

// bodyLine is the value of the line of a report body that starts with prefix ("" when there is none).
func bodyLine(body, prefix string) string {
	for _, line := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(line, prefix); ok {
			return v
		}
	}
	return ""
}

// TestCloseDoesNotCloseWhenTheCrosscheckHasAP1Finding (CRW-1473): the crosscheck's pass A finds a P1 core_function finding in the
// integrated range of M1. The milestone does not close: no crw.milestoneClose and no crw.reportUp row, no reviewer-final call, and the run
// ends escalated crosscheck_pending with ask crosscheck_pending (the 자식 hands it to the 부모). The verdict V is handed on as the op
// recorded it: the run's value carries it as crosscheck, JSON-equal to the crw.crosscheck row's verdict and to the escalation's
// evidence.crosscheck, and the fix packet in the value names the finding by its id and the route of the fix (a correction node landed
// through land-head, then the verdict reported up), not a second crosscheck run.
func TestCloseDoesNotCloseWhenTheCrosscheckHasAP1Finding(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	f := reviewadapter.Finding{Severity: "P1", Impact: "core_function", File: "node-A-1.txt", Line: 1, Text: "the file breaks the criterion of the milestone"}
	id := reviewadapter.FindingID(f.File, f.Text)
	l.Crosschecks("M1", 1, 0, "A", 1, reviewadapter.Result{State: reviewadapter.StateOK, Verdict: reviewadapter.VerdictNeedsChanges, Findings: []reviewadapter.Finding{f}})
	_, v := unitCheckRun(t, l)
	if v["state"] != "escalated" || v["source"] != "crosscheck" || v["reason"] != "crosscheck_pending" || v["ask"] != "crosscheck_pending" {
		t.Fatalf("the run's value %v, want escalated crosscheck_pending with ask crosscheck_pending", v)
	}
	rows, _ := labRows(l, ops.OpCrosscheck)
	if len(rows) != 1 || str(rows[0].Data, "state") != ops.CrosscheckStateVerdict {
		t.Fatalf("crosscheck rows %+v", rows)
	}
	recorded := rows[0].Data["verdict"]
	if rv, _ := recorded.(map[string]any); rv["verdict"] != reviewadapter.VerdictNeedsChanges {
		t.Fatalf("the crosscheck row's verdict %v", recorded)
	}
	if !jsonEqualCLI(t, v["crosscheck"], recorded) {
		t.Errorf("the run's value carries the crosscheck %v, want the row's verdict %v", v["crosscheck"], recorded)
	}
	var esc map[string]any
	for _, e := range l.events() {
		if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation && str(e.Data, "reason") == "crosscheck_pending" {
			esc = e.Data
		}
	}
	if esc == nil || str(esc, "ask") != "crosscheck_pending" {
		t.Fatalf("no escalation crosscheck_pending asking crosscheck_pending: %v", esc)
	}
	evidence, _ := esc["evidence"].(map[string]any)
	if !jsonEqualCLI(t, evidence["crosscheck"], recorded) {
		t.Errorf("the escalation's evidence.crosscheck %v, want the row's verdict %v", evidence["crosscheck"], recorded)
	}
	fix, _ := v["fix"].(string)
	for _, want := range []string{id, "crw flow start land-head", "crw flow report-up", "부모"} {
		if !strings.Contains(fix, want) {
			t.Errorf("the fix packet lacks %q:\n%s", want, fix)
		}
	}
	if strings.Contains(fix, "start crosscheck") {
		t.Errorf("the fix packet starts a second crosscheck run:\n%s", fix)
	}
	for _, op := range []string{ops.OpMilestoneClose, ops.OpReportUp, ops.OpReviewerFinal} {
		if got, _ := labRows(l, op); len(got) != 0 {
			t.Errorf("%d %s rows: the milestone closed over a crosscheck that needs changes", len(got), op)
		}
	}
	for _, c := range labCalls(l) {
		if c.Role == "reviewer-final" || strings.HasPrefix(c.Label, "reviewer-final:") || strings.HasPrefix(c.Label, "reflection:") {
			t.Errorf("the close made the call %s (%s)", c.Label, c.Role)
		}
	}
}

// TestReportBodyCarriesTheCrosscheckAndReflectionOfTheClose (CRW-1473): a clean close of M1 reports up the crosscheck it closed over and
// the reflection of the architect. The body the shipped Reporter stores (report.MilestoneBody of the ops.Report the op built) holds a
// Crosscheck: line whose V is verified at the gate's head, and Reflection: ALIGNED.
func TestReportBodyCarriesTheCrosscheckAndReflectionOfTheClose(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	res, v := unitCheckRun(t, l)
	l.closed(res)
	if v["crosscheck"] != reviewadapter.VerdictVerified || v["reflection"] != "ALIGNED" {
		t.Errorf("the run's value %v", v)
	}
	gates, _ := labRows(l, ops.OpGate)
	if len(gates) == 0 {
		t.Fatal("no gate row")
	}
	head := str(gates[len(gates)-1].Data, "head")
	l.mu.Lock()
	bodies := append([]string(nil), l.reportBodies...)
	l.mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("%d report bodies", len(bodies))
	}
	var cc map[string]any
	if err := json.Unmarshal([]byte(bodyLine(bodies[0], "Crosscheck: ")), &cc); err != nil {
		t.Fatalf("the body's Crosscheck: line is no V: %v\n%s", err, bodies[0])
	}
	if cc["verdict"] != reviewadapter.VerdictVerified || cc["head"] != head || head == "" || cc["guide_rev"] != ops.CrosscheckGuideRev {
		t.Errorf("the body's crosscheck %v, want a verified V at the gate head %s", cc, head)
	}
	if got := bodyLine(bodies[0], "Reflection: "); got != "ALIGNED" {
		t.Errorf("the body's Reflection: %q\n%s", got, bodies[0])
	}
}

// TestReplannedCloseRunsEveryFinalStepFirst (CRW-1473, CRW-1447): a close whose reflection is MISALIGNED is due for a re-plan, and the
// closed milestone is audited and reported before the re-plan: the crw.audit and crw.reportUp rows come before the replan:close call,
// and the report body says Reflection: MISALIGNED.
func TestReplannedCloseRunsEveryFinalStepFirst(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	misaligned(l)
	_, v := unitCheckRun(t, l)
	replanAt := -1
	for i, e := range l.events() {
		if e.Kind == engine.KindCall && e.Op == "agent" && e.Event == engine.EvPrepared && e.Label == "replan:close" {
			replanAt = i
			break
		}
	}
	if replanAt < 0 {
		t.Fatalf("no replan:close call; the run's value %v", v)
	}
	for _, op := range []string{ops.OpAudit, ops.OpReportUp} {
		_, at := labRows(l, op)
		if len(at) != 1 || at[0] > replanAt {
			t.Errorf("the %s rows at %v, want one before the replan:close call at %d", op, at, replanAt)
		}
	}
	l.mu.Lock()
	bodies := append([]string(nil), l.reportBodies...)
	l.mu.Unlock()
	if len(bodies) != 1 || bodyLine(bodies[0], "Reflection: ") != "MISALIGNED" {
		t.Errorf("the report bodies %q, want one that says Reflection: MISALIGNED", bodies)
	}
}

// misaligned has the architect's reflection of the close answer MISALIGNED, so the close is due for a re-plan.
func misaligned(l *laneLab) {
	l.turn = func(c labCall, r fakehost.TurnRequest) (fakehost.TurnPlan, bool) {
		if c.Role == "architect" && strings.HasPrefix(c.Label, "reflection:") {
			return labSay(map[string]any{"verdict": "MISALIGNED", "misaligned": []any{map[string]any{"decision_id": "D1", "observed": "the range departs from D1",
				"affected_nodes": []string{l.nodeOf["A-1"]}, "evidence": []string{"README.md"}}}}), true
		}
		return fakehost.TurnPlan{}, false
	}
}

// TestEveryCloseAuditsAndReportsAndArchivesNothing (CRW-1447): every close of the project lane, one that closes and one that is due for a
// re-plan, writes one crw.audit and one crw.reportUp row, in that order, and no crw.archive row; the report it sends up carries the
// audit of the milestone, and the run's value of the closed milestone holds the audit and no archive member.
func TestEveryCloseAuditsAndReportsAndArchivesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*laneLab)
		state string
	}{
		{"a close", func(*laneLab) {}, "closed"},
		{"a close due for a re-plan", misaligned, "replanned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
			tc.setup(l)
			_, v := unitCheckRun(t, l)
			if v["state"] != tc.state {
				t.Fatalf("the run's value %v, want state %s", v, tc.state)
			}
			if _, ok := v["archived"]; ok {
				t.Errorf("the run's value has an archive member: %v", v)
			}
			if a, ok := v["audit"].(map[string]any); tc.state == "closed" && (!ok || a["ok"] != float64(1)) {
				t.Errorf("the run's value has no audit of the one acceptance: %v", v)
			}
			audits, auditAt := labRows(l, ops.OpAudit)
			_, reportAt := labRows(l, ops.OpReportUp)
			if len(auditAt) != 1 || len(reportAt) != 1 || auditAt[0] > reportAt[0] {
				t.Errorf("the crw.audit rows at %v and the crw.reportUp rows at %v, want one of each, the audit first", auditAt, reportAt)
			}
			for _, e := range l.events() {
				if e.Kind == engine.KindCall && e.Op == ops.OpArchive {
					t.Errorf("a crw.archive row: %s %v", e.Event, e.Data)
				}
			}
			l.mu.Lock()
			bodies := append([]string(nil), l.reportBodies...)
			l.mu.Unlock()
			if len(bodies) != 1 {
				t.Fatalf("%d report bodies", len(bodies))
			}
			var sent map[string]any
			if err := json.Unmarshal([]byte(bodyLine(bodies[0], "Audit: ")), &sent); err != nil || len(audits) != 1 || sent["milestone"] != "M1" || sent["ok"] != float64(1) {
				t.Errorf("the report's audit %v (%v), want the audit of M1 with its one acceptance\n%s", sent, err, bodies[0])
			}
		})
	}
}

// TestCrosscheckRerunsAVoidPassOnceAndGivesNoVerdictWhenBothAreVoid (CRW-1504): a pass that answered but read no repository (not_read)
// is void. When exactly one pass of the round is void and the other is ok, only the void pass runs once more, on a fresh checkout
// (attempt 2), and the first attempt stays in V.passes as voided; a rerun that reads the repository gives the verdict. When both passes
// are void, the round gives no verdict at once (two checks, no rerun), and so does a rerun that is void again: the run ends escalated
// crosscheck_no_verdict with ask crosscheck_no_verdict (the 자식 hands it to the 부모) and the milestone does not close.
func TestCrosscheckRerunsAVoidPassOnceAndGivesNoVerdictWhenBothAreVoid(t *testing.T) {
	void := reviewadapter.Result{State: reviewadapter.StateNotRead, Verdict: reviewadapter.VerdictVerified, Detail: "read 2304 tokens and 1 commands in 20 s (min 3 commands, 45 s)"}
	ok := reviewadapter.Result{State: reviewadapter.StateOK, Verdict: reviewadapter.VerdictVerified}
	keys := func(l *laneLab) []string {
		var out []string
		for _, c := range l.cc.requests() {
			out = append(out, c.Key)
		}
		return out
	}
	crosscheckRow := func(t *testing.T, l *laneLab) engine.Event {
		t.Helper()
		rows, _ := labRows(l, ops.OpCrosscheck)
		if len(rows) != 1 {
			t.Fatalf("%d crosscheck rows", len(rows))
		}
		return rows[0]
	}
	passesOf := func(t *testing.T, row engine.Event) []ops.CrosscheckPass {
		t.Helper()
		raw, _ := json.Marshal(row.Data["passes"])
		var out []ops.CrosscheckPass
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("the row's passes %s: %v", raw, err)
		}
		return out
	}
	noVerdict := func(t *testing.T, l *laneLab, v map[string]any) {
		t.Helper()
		if v["state"] != "escalated" || v["source"] != "crosscheck" || v["reason"] != "crosscheck_no_verdict" || v["ask"] != "crosscheck_no_verdict" {
			t.Fatalf("the run's value %v, want escalated crosscheck_no_verdict with ask crosscheck_no_verdict", v)
		}
		row := crosscheckRow(t, l)
		if str(row.Data, "state") != ops.CrosscheckStateNoVerdict || str(row.Data, "reason") != "crosscheck_no_verdict" || row.Data["verdict"] != nil {
			t.Errorf("the crosscheck row %v, want no_verdict crosscheck_no_verdict with no V", row.Data)
		}
		var esc map[string]any
		for _, e := range l.events() {
			if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation && str(e.Data, "reason") == "crosscheck_no_verdict" {
				esc = e.Data
			}
		}
		if esc == nil || str(esc, "ask") != "crosscheck_no_verdict" {
			t.Errorf("no escalation crosscheck_no_verdict asking crosscheck_no_verdict: %v", esc)
		}
		for _, op := range []string{ops.OpMilestoneClose, ops.OpReportUp} {
			if got, _ := labRows(l, op); len(got) != 0 {
				t.Errorf("%d %s rows: the milestone closed over a crosscheck that gave no verdict", len(got), op)
			}
		}
	}

	t.Run("one void pass reruns once and the rerun gives the verdict", func(t *testing.T) {
		l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
		l.Crosschecks("M1", 1, 0, "A", 1, void)
		l.Crosschecks("M1", 1, 0, "B", 1, ok)
		l.Crosschecks("M1", 1, 0, "A", 2, ok)
		res, v := unitCheckRun(t, l)
		l.closed(res)
		if v["crosscheck"] != reviewadapter.VerdictVerified {
			t.Errorf("the run's value %v", v)
		}
		if got := keys(l); len(got) != 3 || !slices.Contains(got, "M1/r1/e0/A1") || !slices.Contains(got, "M1/r1/e0/B1") || got[2] != "M1/r1/e0/A2" {
			t.Fatalf("the checks %v, want A1 and B1, then A2", got)
		}
		dirs := map[string]bool{}
		for _, c := range l.cc.requests() {
			if !c.Existed || dirs[c.Request.Worktree] {
				t.Errorf("check %s ran in %q (existed %v): each attempt has a fresh checkout of its own", c.Key, c.Request.Worktree, c.Existed)
			}
			dirs[c.Request.Worktree] = true
		}
		row := crosscheckRow(t, l)
		rv, _ := row.Data["verdict"].(map[string]any)
		if str(row.Data, "state") != ops.CrosscheckStateVerdict || rv["verdict"] != reviewadapter.VerdictVerified {
			t.Fatalf("the crosscheck row %v", row.Data)
		}
		raw, _ := json.Marshal(rv["passes"])
		var vpasses []ops.CrosscheckPass
		_ = json.Unmarshal(raw, &vpasses)
		for name, passes := range map[string][]ops.CrosscheckPass{"the row": passesOf(t, row), "V": vpasses} {
			if len(passes) != 3 {
				t.Fatalf("%s's passes %+v", name, passes)
			}
			want := []struct {
				pass    string
				attempt int
				state   string
				voided  bool
			}{{"A", 1, string(reviewadapter.StateNotRead), true}, {"B", 1, string(reviewadapter.StateOK), false}, {"A", 2, string(reviewadapter.StateOK), false}}
			for i, w := range want {
				p := passes[i]
				if p.Pass != w.pass || p.Attempt != w.attempt || p.State != w.state || p.Voided != w.voided {
					t.Errorf("%s's pass %d: %+v, want %+v", name, i, p, w)
				}
			}
		}
	})

	t.Run("both void passes give no verdict at once", func(t *testing.T) {
		l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
		l.Crosschecks("M1", 1, 0, "A", 1, void)
		l.Crosschecks("M1", 1, 0, "B", 1, void)
		_, v := unitCheckRun(t, l)
		if got := keys(l); len(got) != 2 {
			t.Errorf("the checks %v, want exactly A1 and B1", got)
		}
		noVerdict(t, l, v)
		passes := passesOf(t, crosscheckRow(t, l))
		if len(passes) != 2 || !passes[0].Voided || !passes[1].Voided {
			t.Errorf("the row's passes %+v, want both attempts voided", passes)
		}
	})

	t.Run("a rerun that is void again gives no verdict", func(t *testing.T) {
		l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
		l.Crosschecks("M1", 1, 0, "A", 1, void)
		l.Crosschecks("M1", 1, 0, "B", 1, ok)
		l.Crosschecks("M1", 1, 0, "A", 2, void)
		_, v := unitCheckRun(t, l)
		if got := keys(l); len(got) != 3 || got[2] != "M1/r1/e0/A2" {
			t.Errorf("the checks %v, want A1 and B1, then A2", got)
		}
		noVerdict(t, l, v)
		passes := passesOf(t, crosscheckRow(t, l))
		if len(passes) != 3 || !passes[0].Voided || passes[1].Voided || !passes[2].Voided || passes[2].Attempt != 2 {
			t.Errorf("the row's passes %+v, want A1 and A2 voided beside B1", passes)
		}
	})
}

// jsonEqualCLI says whether two values are the same JSON.
func jsonEqualCLI(t *testing.T, a, b any) bool {
	t.Helper()
	ra, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var x, y any
	_ = json.Unmarshal(ra, &x)
	_ = json.Unmarshal(rb, &y)
	na, _ := json.Marshal(x)
	nb, _ := json.Marshal(y)
	return a != nil && b != nil && string(na) == string(nb)
}
