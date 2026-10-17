package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// twoMilestones is the plan of the two-milestone fixture: M1 holds A-1 and A-2 (A-2 after A-1), M2 holds B-1 after A-2.
func twoMilestones() []labIssue {
	return []labIssue{{Key: "A-1", Milestone: "M1"}, {Key: "A-2", Milestone: "M1", After: []string{"A-1"}}, {Key: "B-1", Milestone: "M2", After: []string{"A-2"}}}
}

// A changed milestone requires a fresh DESIGN, but the integrated issue and its
// referenced decision must survive even when the architect rewrites both.
func TestProjectLaneChangedMilestoneKeepsIntegratedDesign(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1", After: []string{"A-1"}})
	node := l.nodeOf["A-1"]
	src := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	l.afterRow = func(e engine.Event) {
		if e.Kind == engine.KindCall && e.Event == engine.EvCompleted && e.Op == ops.OpIntegrate && str(e.Data, "node") == node {
			l.mu.Lock()
			cur := l.cur
			l.mu.Unlock()
			if f, ok := cur.Journal.(interface{ Flush() error }); ok {
				if err := f.Flush(); err != nil {
					t.Fatal(err)
				}
			}
			l.kill()
		}
	}
	if _, killed := l.process(src.RunID, false); !killed {
		t.Fatal("source did not stop after first integration")
	}
	l.afterRow = nil
	before := journalBytes(t, l, src.RunID)
	raw := l.handoffBytes(func(in *compile.Input) { in.Projects[0].Milestones[0].Title += " revised" })
	file := filepath.Join(t.TempDir(), "revised.json")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	l.designEdit = func(d *ops.Design) {
		for i := range d.Issues {
			if d.Issues[i].Node == node {
				d.Issues[i].Approach += "!"
			}
		}
		d.Decisions[0].Change += "!"
	}
	next := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", file)
	l.closed(l.runToEnd(next.RunID))
	reuses := reuseIntegratedRows(t, l)
	if len(reuses) != 2 {
		t.Fatalf("reuse decisions: %+v", reuses)
	}
	for _, r := range reuses {
		if r["node"] == node && (r["reason"] != "integrated_reused" || r["source_run"] != src.RunID) {
			t.Fatalf("integrated node not reused: %+v", r)
		}
	}
	for _, c := range labCalls(l) {
		if strings.HasPrefix(c.Label, "build:"+node+":") {
			t.Errorf("integrated node executor started: %s", c.Label)
		}
	}
	for _, e := range l.events() {
		if e.Op == ops.OpEscalate && str(e.Data, "reason") == "design_question" {
			t.Errorf("unexpected integration question: %+v", e)
		}
	}
	decisions := designReuseRows(t, l)
	if len(decisions) != 1 || !strings.Contains(fmt.Sprint(decisions[0]["attempts"]), "milestone_digest_changed") {
		t.Fatalf("fresh design reason: %+v", decisions)
	}
	if !slices.Equal(before, journalBytes(t, l, src.RunID)) {
		t.Fatal("source journal changed")
	}
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
				"evidence": []string{"README.md:1"}}}, "summary": "one P1 finding", "root": nil, "first_check": nil}), true
		case "design-audit:r2":
			return labSay(map[string]any{"verdict": "approved", "findings": []any{}, "summary": "the finding is folded", "root": nil, "first_check": nil}), true
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
			// the re-plan rewrites the unstarted B-1's tests and done condition and keeps the started A-1 as it was released (a
			// revision that changes a started node is refused by crw.designLedger, CRW-1472): the critic must see what A-1 was
			// released with, and not the plan of B-1
			for i := range d.Issues {
				if d.Issues[i].Node == nodeB {
					d.Issues[i].TestsFirst = []string{"a test of B-1 after the re-plan"}
					d.Issues[i].DoneCondition = "a done condition of B-1 after the re-plan"
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
// architect's first call read it. A candidate lands after the first background placement is recorded and before the second is
// planned: both explorers and the architect still read the sampled head, the architect's checkout is made at it, the ledger's r0
// records it as the head the DESIGN ran at, and the architect's packet names it with the background findings.
func TestDesignPhaseReadsOneHead(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1", After: []string{"A-1"}})
	before := l.git(l.repo, "rev-parse", "integration")
	landed := ""
	preparedBoth := make(chan struct{})
	prepared := 0
	l.afterRow = func(e engine.Event) {
		if e.Kind != engine.KindCall || e.Op != "agent" || e.Event != engine.EvPrepared || !strings.HasPrefix(e.Label, "explore-bg:") {
			return
		}
		prepared++
		if prepared == 1 {
			// prepared is written synchronously, before this call's worktree is made and before the next call is planned.
			// Moving the ref from a model turn let both placements precede the move and could overlap Git worktree setup.
			tree := l.git(l.repo, "rev-parse", before+"^{tree}")
			landed = l.git(l.repo, "commit-tree", tree, "-p", before, "-m", "landed meanwhile")
			l.git(l.repo, "update-ref", "refs/heads/integration", landed, before)
		}
		if prepared == 2 {
			defer close(preparedBoth)
			if tip := l.git(l.repo, "rev-parse", "integration"); tip != landed || tip == before {
				t.Errorf("the second background placement saw integration at %q, want the landed head %q (before %s)", tip, landed, before)
			}
		}
	}
	l.turn = func(c labCall, r fakehost.TurnRequest) (fakehost.TurnPlan, bool) {
		if !strings.HasPrefix(c.Label, "explore-bg:") {
			return fakehost.TurnPlan{}, false
		}
		// Exercise the schedule where both calls are prepared before either model answers.
		<-preparedBoth
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
			return labSay(map[string]any{"verdict": "needs_changes", "findings": []any{map[string]any{"ref": "D1", "severity": "P2", "finding": "state the order", "evidence": []string{}}}, "summary": "one thing", "root": nil, "first_check": nil}), true
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

// closeFix is the turn hook of a close repair (CRW-1490/DV-1490-2): the executor call close-fix:<ms> appends its correction to the
// file of the node in its worktree and answers a ready RECEIPT. The synthetic node milestone:<ms> has no file of the lab's issue
// map, so the file is named by the issue it repairs; label is the close-fix label of the milestone.
func closeFix(l *laneLab, issue string) func(c labCall, r fakehost.TurnRequest) (fakehost.TurnPlan, bool) {
	return func(c labCall, r fakehost.TurnRequest) (fakehost.TurnPlan, bool) {
		if c.Label != "close-fix:M1" {
			return fakehost.TurnPlan{}, false
		}
		if c.Cwd != "" {
			path := filepath.Join(c.Cwd, "node-"+issue+".txt")
			prior, _ := os.ReadFile(path)
			if err := os.WriteFile(path, append(prior, []byte("close correction\n")...), 0o644); err != nil {
				l.t.Errorf("the close fix cannot write %s: %v", path, err)
			}
		}
		return labSay(map[string]any{"outcome": "ready_for_review", "files": []string{"node-" + issue + ".txt"}, "verification": []any{},
			"summary": "fixed the milestone finding", "gap": nil, "research_question": nil, "plan_mismatch": nil}), true
	}
}

// closeFixCalls counts the prepared executor calls labelled close-fix:M1 (the in-run repair of the close).
func closeFixCalls(l *laneLab) int {
	n := 0
	for _, c := range labCalls(l) {
		if c.Label == "close-fix:M1" {
			n++
		}
	}
	return n
}

// syntheticCommits are the completed crw.commit rows of the synthetic close node milestone:M1.
func syntheticCommits(l *laneLab) []engine.Event {
	var out []engine.Event
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Op == ops.OpCommit && e.Event == engine.EvCompleted && str(e.Data, "node") == "milestone:M1" {
			out = append(out, e)
		}
	}
	return out
}

// ccVerdict is the verdict word of a completed crw.crosscheck row.
func ccVerdict(e engine.Event) string {
	v, _ := e.Data["verdict"].(map[string]any)
	s, _ := v["verdict"].(string)
	return s
}

// TestCloseRepairsAndRechecksBeforeClosing (CRW-1490/DV-1490-2): a crosscheck round of M1 that needs changes gets one in-run
// correction: an executor call close-fix:M1 under the synthetic node milestone:M1 starts at the failed gate's head, its work is
// committed by crw.commit, the final gate re-judges the candidate head and the SAME crosscheck re-runs as round 2 on it. A verified
// round 2 closes the milestone at the candidate head: the tip the close recorded and the integration branch's tip are the fixed
// commit, which descends from the head the crosscheck failed. No reviewer call and no second round of node unit checks run.
func TestCloseRepairsAndRechecksBeforeClosing(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	node := l.nodeOf["A-1"]
	f := reviewadapter.Finding{Severity: "P1", Impact: "core_function", File: "node-A-1.txt", Line: 1, Text: "the file breaks the criterion of the milestone"}
	id := reviewadapter.FindingID(f.File, f.Text)
	l.Crosschecks("M1", 1, 0, "A", 1, reviewadapter.Result{State: reviewadapter.StateOK, Verdict: reviewadapter.VerdictNeedsChanges, Findings: []reviewadapter.Finding{f}})
	l.turn = closeFix(l, "A-1")
	res, _ := unitCheckRun(t, l)
	lv := l.closed(res)
	if n := closeFixCalls(l); n != 1 {
		t.Fatalf("%d close-fix executor calls, want 1 (the close escalated without an in-run correction)", n)
	}
	for _, c := range labCalls(l) {
		if c.Role == "reviewer-final" || strings.HasPrefix(c.Label, "reviewer-final:") {
			t.Errorf("the close made the call %s (%s): no new check stage", c.Label, c.Role)
		}
	}
	rows, at := labRows(l, ops.OpCrosscheck)
	if len(rows) != 2 {
		t.Fatalf("%d crosscheck rows, want round 1 needs_changes and round 2 verified: %+v", len(rows), rows)
	}
	if ccVerdict(rows[0]) != reviewadapter.VerdictNeedsChanges || rows[0].Data["round"] != float64(1) {
		t.Errorf("the first crosscheck: %v", rows[0].Data)
	}
	if ccVerdict(rows[1]) != reviewadapter.VerdictVerified || rows[1].Data["round"] != float64(2) || rows[1].Data["evidence_round"] != float64(0) {
		t.Errorf("the second crosscheck: %v", rows[1].Data)
	}
	first, fixed := str(rows[0].Data, "head"), str(rows[1].Data, "head")
	if first == "" || fixed == "" || first == fixed {
		t.Fatalf("the crosscheck heads %q -> %q, want a different fixed head", first, fixed)
	}
	if str(rows[0].Data, "base") == "" || str(rows[0].Data, "base") != str(rows[1].Data, "base") {
		t.Errorf("the rounds checked different bases: %q vs %q", str(rows[0].Data, "base"), str(rows[1].Data, "base"))
	}
	// the second round's passes ran against the candidate head, and their prompts carry the failed round's finding
	seen := map[string]bool{}
	for _, r := range l.cc.requests() {
		seen[r.Key] = true
		if strings.HasPrefix(r.Key, "M1/r2/") {
			if !strings.Contains(r.Request.Prompt, id) || !strings.Contains(r.Request.Prompt, f.Text) {
				t.Errorf("the round-2 prompt %s does not carry the finding %s (%q):\n%s", r.Key, id, f.Text, r.Request.Prompt)
			}
			if !strings.HasSuffix(filepath.Base(r.Request.Worktree), fixed[:12]) {
				t.Errorf("the round-2 pass %s ran in %s, not a checkout of %s", r.Key, r.Request.Worktree, fixed)
			}
		}
	}
	for _, key := range []string{"M1/r2/e0/A1", "M1/r2/e0/B1"} {
		if !seen[key] {
			t.Errorf("no %s pass of the recheck", key)
		}
	}
	// the close published the candidate: the tip it recorded and the integration branch are the fixed head, which keeps the failed head
	if lv.Tip != fixed {
		t.Errorf("the close's tip %q, want the fixed head %q", lv.Tip, fixed)
	}
	if tip := l.git(l.repo, "rev-parse", l.target); tip != fixed {
		t.Errorf("the integration tip %q, want the fixed head %q", tip, fixed)
	}
	// the publication is a verified move the official push path accepts (CRW-1490)
	if proven, err := (&dagsched.Scheduler{Store: l.w.st}).VerifiedMoveOnto(context.Background(), l.target, fixed); err != nil || !proven {
		t.Fatalf("the repaired close is not a verified move for the official push path: %v %v", proven, err)
	}
	// the record of it: one intent naming the verified head and a real verification digest, and one ref_moved of the same batch
	stages, err := store.IntegrationStagesOfPlan(context.Background(), l.w.st, l.plan)
	if err != nil {
		t.Fatal(err)
	}
	batchOf, digest := "", ""
	intentSeen, movedSeen := false, false
	for _, s := range stages {
		var intent map[string]string
		if s.Stage == "intent" && json.Unmarshal([]byte(s.Detail), &intent) == nil && intent["verified_head"] == fixed {
			if intent["integration_ref"] != l.target || intent["verification_digest"] == "" || intent["milestone"] != "M1" || intent["base"] != first {
				t.Errorf("the repair's intent %v", intent)
			}
			intentSeen, batchOf, digest = true, s.BatchID, intent["verification_digest"]
		}
		if s.Stage == "ref_moved" && s.Detail == fixed {
			movedSeen = true
			if s.BatchID != batchOf {
				t.Errorf("the ref_moved row's batch %q is not the intent's %q", s.BatchID, batchOf)
			}
		}
	}
	if !intentSeen || !movedSeen {
		t.Fatalf("the repair's stage rows %+v, want an intent of the verified head and a ref_moved of the same batch", stages)
	}
	// a replay of the same publication is idempotent: the target stays and no second stage row lands. The replaying relay is the
	// same run, as a resumed process builds it: same run id and nonce, the handoff that run pinned.
	closes, _ := labRows(l, ops.OpMilestoneClose)
	if len(closes) != 1 {
		t.Fatalf("%d close rows, want the one repaired close", len(closes))
	}
	rl := l.relayFor(closes[0].RunID+"-replay", l.handoffBytes(nil), 1)
	rl.RunID, rl.Nonce = closes[0].RunID, closes[0].RunID
	pinned, err := os.ReadFile(filepath.Join(RunDir(l.w.state, rl.RunID), HandoffFile))
	if err != nil {
		t.Fatal(err)
	}
	rl.Handoff = HandoffRef{Path: filepath.Join(RunDir(l.w.state, rl.RunID), HandoffFile), SHA256: sha256Hex(pinned)}
	rep, err := rl.IntegrateMilestoneRepair(context.Background(), ops.MilestoneRepairRequest{Plan: l.plan, Milestone: "M1", Base: first, Head: fixed, VerificationDigest: digest})
	if err != nil || rep.Tip != fixed || rep.Moved {
		t.Fatalf("a replay of the repair's publication: %+v %v", rep, err)
	}
	if again, err := store.IntegrationStagesOfPlan(context.Background(), l.w.st, l.plan); err != nil || len(again) != len(stages) {
		t.Fatalf("the replay wrote %d stage rows, want the same %d: %v", len(again), len(stages), err)
	}
	l.git(l.repo, "merge-base", "--is-ancestor", first, fixed)
	l.git(l.repo, "merge-base", "--is-ancestor", fixed, l.git(l.repo, "rev-parse", l.target))
	if content := l.git(l.repo, "show", fixed+":node-A-1.txt"); !strings.Contains(content, "close correction") || !strings.HasPrefix(content, l.git(l.repo, "show", first+":node-A-1.txt")) {
		t.Errorf("the fixed head's node-A-1.txt does not hold the original work plus the correction: %q", content)
	}
	// no second stage of per-node checks ran, and the candidate was committed once under the synthetic node
	if checks := l.unitCheckRowsOf(node); len(checks) != 1 {
		t.Errorf("%d unit checks of %s, want its one pre-commit check: %+v", len(checks), node, checks)
	}
	if commits := syntheticCommits(l); len(commits) != 1 || str(commits[0].Data, "commit") != fixed {
		t.Errorf("the synthetic commits %+v, want the one commit %s of milestone:M1", commits, fixed)
	}
	// the candidate's gate ran before its recheck, and the close after it
	gates, gateAt := labRows(l, "crw.finalGate")
	candGate := -1
	for i, g := range gates {
		if str(g.Data, "head") == fixed {
			candGate = i
		}
	}
	if candGate < 0 || gateAt[candGate] > at[1] {
		t.Errorf("no final gate of the candidate before the second crosscheck: gates %+v at %v, crosschecks at %v", gates, gateAt, at)
	}
	if closes, closeAt := labRows(l, ops.OpMilestoneClose); len(closes) != 1 || closeAt[0] < at[1] {
		t.Errorf("the milestone closed before the recheck: closes %+v at %v", closes, closeAt)
	}
}

// TestCloseLeavesSecondNeedsChangesPending (CRW-1473, DV-1490-2): the crosscheck's pass A finds a P1 core_function finding in the
// integrated range of M1; the close repairs it once in the run (one close-fix executor call), and the same crosscheck re-runs as
// round 2 on the candidate. A second needs_changes exhausts the run's one correction: no crw.milestoneClose and no crw.reportUp row,
// no reviewer-final call, and the run ends escalated crosscheck_pending with ask crosscheck_pending (the 자식 hands it to the 부모),
// its V the LAST crosscheck row's verdict and its fix packet naming the latest finding. The integration target stays at the head the
// failed check judged: a candidate that still needs changes is never published.
func TestCloseLeavesSecondNeedsChangesPending(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	f := reviewadapter.Finding{Severity: "P1", Impact: "core_function", File: "node-A-1.txt", Line: 1, Text: "the file breaks the criterion of the milestone"}
	f2 := reviewadapter.Finding{Severity: "P1", Impact: "core_function", File: "node-A-1.txt", Line: 2, Text: "the correction still breaks the criterion"}
	needs := func(f reviewadapter.Finding) reviewadapter.Result {
		return reviewadapter.Result{State: reviewadapter.StateOK, Verdict: reviewadapter.VerdictNeedsChanges, Findings: []reviewadapter.Finding{f}}
	}
	l.Crosschecks("M1", 1, 0, "A", 1, needs(f))
	l.Crosschecks("M1", 2, 0, "A", 1, needs(f2))
	l.turn = closeFix(l, "A-1")
	_, v := unitCheckRun(t, l)
	if v["state"] != "escalated" || v["source"] != "crosscheck" || v["reason"] != "crosscheck_pending" || v["ask"] != "crosscheck_pending" {
		t.Fatalf("the run's value %v, want escalated crosscheck_pending with ask crosscheck_pending", v)
	}
	if n := closeFixCalls(l); n != 1 {
		t.Errorf("%d close-fix executor calls, want the one correction the run gets", n)
	}
	rows, _ := labRows(l, ops.OpCrosscheck)
	if len(rows) != 2 || str(rows[0].Data, "state") != ops.CrosscheckStateVerdict || str(rows[1].Data, "state") != ops.CrosscheckStateVerdict {
		t.Fatalf("crosscheck rows %+v, want two judged rounds", rows)
	}
	recorded := rows[1].Data["verdict"]
	if rv, _ := recorded.(map[string]any); rv["verdict"] != reviewadapter.VerdictNeedsChanges {
		t.Fatalf("the last crosscheck row's verdict %v", recorded)
	}
	if !jsonEqualCLI(t, v["crosscheck"], recorded) {
		t.Errorf("the run's value carries the crosscheck %v, want the LAST row's verdict %v", v["crosscheck"], recorded)
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
		t.Errorf("the escalation's evidence.crosscheck %v, want the last row's verdict %v", evidence["crosscheck"], recorded)
	}
	fix, _ := v["fix"].(string)
	for _, want := range []string{reviewadapter.FindingID(f2.File, f2.Text), "crw flow start land-head", "crw flow report-up", "부모"} {
		if !strings.Contains(fix, want) {
			t.Errorf("the fix packet lacks %q:\n%s", want, fix)
		}
	}
	// a candidate the second recheck rejected was never published, so it is no verified move for the push path (CRW-1490)
	if commits := syntheticCommits(l); len(commits) != 1 {
		t.Errorf("the synthetic commits %+v, want the one repair", commits)
	} else if proven, err := (&dagsched.Scheduler{Store: l.w.st}).VerifiedMoveOnto(context.Background(), l.target, str(commits[0].Data, "commit")); err != nil || proven {
		t.Errorf("the rejected candidate is a verified move for the push path: %v %v", proven, err)
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
	if tip := l.git(l.repo, "rev-parse", l.target); tip != str(rows[0].Data, "head") {
		t.Errorf("the integration tip %q moved off the judged head %q: a candidate that still needs changes was published", tip, str(rows[0].Data, "head"))
	}
}

// TestCloseEvidenceMissingAsksAndRechecks (CRW-1490/DV-1490-2): a crosscheck finding that is only missing evidence is not a
// correction: the close asks its evidence question of an explorer (crw.explore kind evidence at the failed head) and re-runs the
// SAME crosscheck as round 2 of evidence round 1 on the SAME head, with the answers in the passes' prompts. No executor call and no
// commit of the synthetic node run. Verified rechecks close; a second needs_changes is the existing crosscheck_pending.
func TestCloseEvidenceMissingAsksAndRechecks(t *testing.T) {
	missing := reviewadapter.Finding{Severity: "P2", Impact: "criterion_unmet", File: "node-A-1.txt", Line: 1, Text: "the verification evidence is missing",
		Kind: reviewadapter.KindEvidenceMissing, EvidenceQuestion: &reviewadapter.EvidenceQuestion{Question: "Which command proves the milestone?", FilesNeeded: []string{"node-A-1.txt"}}}
	needs := func(f reviewadapter.Finding) reviewadapter.Result {
		return reviewadapter.Result{State: reviewadapter.StateOK, Verdict: reviewadapter.VerdictNeedsChanges, Findings: []reviewadapter.Finding{f}}
	}
	setup := func(t *testing.T) *laneLab {
		l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
		l.Crosschecks("M1", 1, 0, "A", 1, needs(missing))
		l.turn = func(c labCall, r fakehost.TurnRequest) (fakehost.TurnPlan, bool) {
			if c.Role == "explorer" && (strings.HasPrefix(c.Label, "explore:milestone:M1") || strings.HasPrefix(c.Label, "explore:M1")) {
				return labSay(map[string]any{"answer": "the milestone command passed", "evidence": []string{"go test ./node: PASS"}, "verified": []string{"go test ./node: PASS"},
					"inferred": []string{}, "unknown": []string{}}), true
			}
			return fakehost.TurnPlan{}, false
		}
		return l
	}
	assertEvidenceClose := func(l *laneLab, res Result) {
		if n := closeFixCalls(l); n != 0 {
			t.Errorf("%d close-fix executor calls, want 0 for an evidence-only verdict", n)
		}
		if commits := syntheticCommits(l); len(commits) != 0 {
			t.Errorf("%d commits of the synthetic node, want 0", len(commits))
		}
		rows, _ := labRows(l, ops.OpCrosscheck)
		if len(rows) != 2 {
			t.Fatalf("%d crosscheck rows, want the failed round and the evidence recheck: %+v", len(rows), rows)
		}
		if str(rows[1].Data, "head") != str(rows[0].Data, "head") {
			t.Errorf("the evidence recheck judged another head: %q -> %q", str(rows[0].Data, "head"), str(rows[1].Data, "head"))
		}
		if rows[1].Data["round"] != float64(2) || rows[1].Data["evidence_round"] != float64(1) {
			t.Errorf("the recheck is not round 2 of evidence round 1: %v", rows[1].Data)
		}
		routes := 0
		for _, e := range l.events() {
			if e.Kind == engine.KindCall && e.Op == ops.OpExploreRoute && e.Event == engine.EvCompleted && str(e.Data, "kind") == "evidence" {
				routes++
			}
		}
		if routes != 1 {
			t.Errorf("%d evidence routes, want the one for the milestone's questions", routes)
		}
		for _, r := range l.cc.requests() {
			if strings.HasPrefix(r.Key, "M1/r2/") {
				for _, want := range []string{"Which command proves the milestone?", "the milestone command passed", "go test ./node: PASS"} {
					if !strings.Contains(r.Request.Prompt, want) {
						t.Errorf("the recheck prompt %s lacks %q:\n%s", r.Key, want, r.Request.Prompt)
					}
				}
			}
		}
	}
	t.Run("verified after the evidence", func(t *testing.T) {
		l := setup(t)
		res, _ := unitCheckRun(t, l)
		l.closed(res)
		assertEvidenceClose(l, res)
	})
	t.Run("a second needs_changes is pending", func(t *testing.T) {
		l := setup(t)
		l.Crosschecks("M1", 2, 1, "A", 1, needs(missing))
		res, v := unitCheckRun(t, l)
		assertEvidenceClose(l, res)
		if v["state"] != "escalated" || v["reason"] != "crosscheck_pending" {
			t.Fatalf("the run's value %v, want escalated crosscheck_pending", v)
		}
		for _, op := range []string{ops.OpMilestoneClose, ops.OpReportUp} {
			if got, _ := labRows(l, op); len(got) != 0 {
				t.Errorf("%d %s rows over a second needs_changes", len(got), op)
			}
		}
	})
}

// TestCloseRepairDoesNotOverwriteATargetThatMoved (DV-1490-2): the repair is published by a fenced compare-and-swap from the failed
// head: when the integration target moves between the recheck and the publication, the candidate is not forced on - the run ends
// unsettled close_head_moved, nothing closes or is reported, and the target keeps the head that landed meanwhile.
func TestCloseRepairDoesNotOverwriteATargetThatMoved(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	f := reviewadapter.Finding{Severity: "P1", Impact: "core_function", File: "node-A-1.txt", Line: 1, Text: "the file breaks the criterion of the milestone"}
	l.Crosschecks("M1", 1, 0, "A", 1, reviewadapter.Result{State: reviewadapter.StateOK, Verdict: reviewadapter.VerdictNeedsChanges, Findings: []reviewadapter.Finding{f}})
	l.turn = closeFix(l, "A-1")
	var moved string
	var once sync.Once
	l.cc.onCheck = func(key string) {
		// another candidate lands while the recheck's passes run
		if !strings.HasPrefix(key, "M1/r2/") {
			return
		}
		once.Do(func() {
			tree := l.git(l.repo, "rev-parse", l.target+"^{tree}")
			moved = l.git(l.repo, "commit-tree", tree, "-p", l.target, "-m", "landed meanwhile")
			l.git(l.repo, "update-ref", "refs/heads/"+l.target, moved)
		})
	}
	res, v := unitCheckRun(t, l)
	if moved == "" {
		t.Fatal("the recheck never ran")
	}
	if v["state"] != "escalated" && v["state"] != "stop" {
		if v["state"] == "closed" {
			t.Fatalf("the run closed over a target that moved: %s", res.Value)
		}
	}
	if got := fmt.Sprint(v["reason"]); got != "close_head_moved" {
		t.Fatalf("the run's reason %q, want close_head_moved: %s", got, res.Value)
	}
	if closes, _ := labRows(l, ops.OpMilestoneClose); len(closes) != 1 || closes[0].Data["moved"] != true {
		t.Errorf("the close rows over a moved target: %+v, want the one close that answered moved", closes)
	}
	if got, _ := labRows(l, ops.OpReportUp); len(got) != 0 {
		t.Errorf("%d crw.reportUp rows over a moved target", len(got))
	}
	if tip := l.git(l.repo, "rev-parse", l.target); tip != moved {
		t.Errorf("the integration tip %q, want the head that landed meanwhile %q (the repair did not overwrite it)", tip, moved)
	}
}

// TestCloseRepairDoesNotPublishAfterFailedVerification (DV-1490-2): the candidate the close's repair committed is gated before it is
// rechecked: the final gate's verification of the candidate fails, so no second crosscheck runs, the milestone does not close and the
// integration target stays at the head the failed check judged.
func TestCloseRepairDoesNotPublishAfterFailedVerification(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	f := reviewadapter.Finding{Severity: "P1", Impact: "core_function", File: "node-A-1.txt", Line: 1, Text: "the file breaks the criterion of the milestone"}
	l.Crosschecks("M1", 1, 0, "A", 1, reviewadapter.Result{State: reviewadapter.StateOK, Verdict: reviewadapter.VerdictNeedsChanges, Findings: []reviewadapter.Finding{f}})
	// the lab's verifier fails the second gate head it is asked about (the repair candidate); the gate verifies are the
	// commanded ones, so node checks are untouched
	gateHeads := map[string]bool{}
	l.verifyFail = func(req ops.VerifyRequest) bool {
		if req.Command == "" || gateHeads[req.Head] {
			return false
		}
		if len(gateHeads) > 0 {
			return true
		}
		gateHeads[req.Head] = true
		return false
	}
	l.turn = closeFix(l, "A-1")
	res, v := unitCheckRun(t, l)
	if v["state"] == "closed" {
		t.Fatalf("the run closed over a candidate its gate failed: %s", res.Value)
	}
	if n := closeFixCalls(l); n != 1 {
		t.Errorf("%d close-fix executor calls, want 1", n)
	}
	rows, _ := labRows(l, ops.OpCrosscheck)
	if len(rows) != 1 {
		t.Errorf("%d crosscheck rows, want only the failed round: %+v", len(rows), rows)
	}
	for _, op := range []string{ops.OpMilestoneClose, ops.OpReportUp} {
		if got, _ := labRows(l, op); len(got) != 0 {
			t.Errorf("%d %s rows over an ungated candidate", len(got), op)
		}
	}
	if len(rows) == 1 {
		if tip := l.git(l.repo, "rev-parse", l.target); tip != str(rows[0].Data, "head") {
			t.Errorf("the integration tip %q moved off the judged head %q", tip, str(rows[0].Data, "head"))
		}
	}
	// a candidate that failed the gate was never published, so it is no verified move for the push path (CRW-1490)
	if commits := syntheticCommits(l); len(commits) != 1 {
		t.Errorf("the synthetic commits %+v, want the one repair", commits)
	} else if proven, err := (&dagsched.Scheduler{Store: l.w.st}).VerifiedMoveOnto(context.Background(), l.target, str(commits[0].Data, "commit")); err != nil || proven {
		t.Errorf("the unverified candidate is a verified move for the push path: %v %v", proven, err)
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

// ---- the design reuse of another run (CRW-1329) ----

// stopAfterDesign kills the next process of the lab the moment its journal records a completed row of op (state "", or that
// state): the source run of a design-reuse test stops after its recorded DESIGN - an approved ledger, or a rejected audit - and
// before any release, so its journal ends at the point the reuse reads.
func stopAfterDesign(l *laneLab, op, state string) {
	l.afterRow = func(e engine.Event) {
		if e.Kind != engine.KindCall || e.Op != op || e.Event != engine.EvCompleted || (state != "" && str(e.Data, "state") != state) {
			return
		}
		l.mu.Lock()
		cur := l.cur
		l.mu.Unlock()
		if f, ok := cur.Journal.(interface{ Flush() error }); ok {
			if err := f.Flush(); err != nil {
				l.t.Fatal(err)
			}
		}
		l.kill()
	}
}

// designedSource runs a project lane for M1 only to the moment op ("" for the design ledger) records a completed row of the state,
// and answers the run that wrote it; the journal was flushed, so the rows another run reads stand in the store.
func designedSource(t *testing.T, l *laneLab, op, state string) string {
	t.Helper()
	spec := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	if _, killed := l.process(spec.RunID, false); !killed {
		t.Fatalf("the source run was not killed after %s", op)
	}
	l.afterRow = nil
	return spec.RunID
}

// designReuseRows are the completed crw.designReuse rows of the current process's journal, their results decoded.
func designReuseRows(t *testing.T, l *laneLab) []map[string]any {
	t.Helper()
	var out []map[string]any
	rows, _ := labRows(l, "crw.designReuse")
	for _, row := range rows {
		var res map[string]any
		if err := json.Unmarshal([]byte(str(row.Data, "result")), &res); err != nil {
			t.Fatalf("the designReuse row's result: %v", err)
		}
		out = append(out, res)
	}
	return out
}

// TestProjectLaneReusesApprovedDesignFromAnotherRun (CRW-1329): a second project-lane run of the same milestone over the same node
// slices and the same milestone digest reuses the approved DESIGN of an earlier run: no background explorer, architect, audit or
// design-fix call of its own, the ledger records the new run's revision 0 over the original design with the original run's
// provenance, and the release of the node stands on the imported design - the run executes and closes on it.
func TestProjectLaneReusesApprovedDesignFromAnotherRun(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	node := l.nodeOf["A-1"]
	l.designEdit = func(d *ops.Design) { d.Issues[0].Approach = "reuse the approved store guard" }
	stopAfterDesign(l, ops.OpDesignLedger, "")
	srcRun := designedSource(t, l, "", "")

	// the source run persisted an approved audit, its r0 ledger, and no release
	srcEvents, hasRun, err := journal.Load(context.Background(), l.w.st, srcRun)
	if err != nil || !hasRun {
		t.Fatalf("the source run's journal: %v %v", hasRun, err)
	}
	var ledger engine.Event
	approved := false
	for _, e := range srcEvents {
		if e.Kind != engine.KindCall || e.Event != engine.EvCompleted {
			continue
		}
		switch e.Op {
		case ops.OpDesignAudit:
			approved = approved || str(e.Data, "state") == "approved"
		case ops.OpDesignLedger:
			ledger = e
		case ops.OpRelease:
			t.Errorf("the source run released %v: it was stopped after its DESIGN", e.Data)
		}
	}
	if ledger.Data == nil || !approved {
		t.Fatalf("the source run's journal holds ledger %v and approved audit %v", ledger.Data == nil, approved)
	}
	srcDigest, srcBase := str(ledger.Data, "digest"), str(ledger.Data, "base_tip")
	if srcDigest == "" || srcBase == "" {
		t.Fatalf("the source ledger's data %v, want its digest and base tip", ledger.Data)
	}

	// the second run of the same milestone and plan reuses it: no design phase of its own
	l.designEdit = nil
	spec := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	res := l.runToEnd(spec.RunID)
	l.closed(res)

	// neither background explorer nor architect nor audit nor design-fix call was made; a reflection's architect resume is the
	// close's own and may stand
	for _, c := range labCalls(l) {
		if c.Label == "architect" || strings.HasPrefix(c.Label, "explore-bg:") || strings.HasPrefix(c.Label, "design-audit") || strings.HasPrefix(c.Label, "design-fix") {
			t.Errorf("the reusing run made the design call %s (%s)", c.Label, c.Role)
		}
	}
	// one designReuse row: reused, of the source run and its revision
	reuses := designReuseRows(t, l)
	if len(reuses) != 1 || reuses[0]["reused"] != true {
		t.Fatalf("the designReuse rows %+v, want the one that reused", reuses)
	}
	if reuses[0]["source_run"] != srcRun || reuses[0]["source_revision"] != "r0" || reuses[0]["digest"] != srcDigest || reuses[0]["base_tip"] != srcBase {
		t.Errorf("the reuse %+v does not name the source run %s, r0, the digest %s and base %s", reuses[0], srcRun, srcDigest, srcBase)
	}
	// the ledger records the run's r0 over the original design and names its source; no foreign engine handle is imported
	leds, _ := labRows(l, ops.OpDesignLedger)
	if len(leds) != 1 || str(leds[0].Data, "revision") != "r0" {
		t.Fatalf("the ledger rows %+v, want the run's r0", leds)
	}
	led := leds[0]
	if str(led.Data, "digest") != srcDigest || str(led.Data, "source_run") != srcRun || str(led.Data, "source_revision") != "r0" ||
		str(led.Data, "source_audit_call") == "" || str(led.Data, "reuse_call") == "" || str(led.Data, "architect_handle") != "" {
		t.Errorf("the imported ledger %+v", led.Data)
	}
	if d, _ := led.Data["design"].(map[string]any); d == nil || !strings.Contains(fmt.Sprint(d["issues"]), "reuse the approved store guard") {
		t.Errorf("the imported design %v does not carry the source's payload", led.Data["design"])
	}
	// the release stands on the imported design: the executor's packet carries the approach of the source's DESIGN, and the run
	// lands and closes the node on it
	rels, _ := labRows(l, ops.OpRelease)
	if len(rels) != 1 || str(rels[0].Data, "design_digest") != srcDigest || str(rels[0].Data, "source_run") != srcRun || str(rels[0].Data, "source_revision") != "r0" {
		t.Errorf("the release rows %+v, want the one release under the imported design's digest and provenance", rels)
	}
	if text := l.texts["build:"+node+":executor"]; !strings.Contains(text, "reuse the approved store guard") {
		t.Errorf("the executor's packet does not carry the reused design's approach:\n%s", text)
	}
	// the close's reflection runs on a fresh LOCAL architect thread - never the source run's handle - and its packet carries the
	// reused design's provenance and payload, so the reflection reads what the run actually stood on
	reflections := 0
	for _, c := range labCalls(l) {
		if c.Label == "reflection:M1" {
			reflections++
			if c.ResumeOf != "" {
				t.Errorf("the reflection resumed the foreign thread %s", c.ResumeOf)
			}
		}
	}
	if reflections != 1 {
		t.Errorf("the run made %d reflection calls, want the one", reflections)
	}
	if text := l.texts["reflection:M1"]; !strings.Contains(text, srcRun) || !strings.Contains(text, "REUSED APPROVED DESIGN") || !strings.Contains(text, "reuse the approved store guard") {
		t.Errorf("the reflection's packet does not carry the reused design and its provenance:\n%s", text)
	}
	refls, _ := labRows(l, "crw.reflection")
	if len(refls) != 1 || str(refls[0].Data, "state") != "verdict" || str(refls[0].Data, "alignment") != "ALIGNED" {
		t.Errorf("the reflection rows %+v, want the one ALIGNED verdict", refls)
	}
	// a resume of the finished run replays the recorded decision and import: no new decision, ledger or release rows are
	// written, and no design-phase call is made
	if code, killed := l.process(spec.RunID, true); code != 0 || killed {
		t.Fatalf("the resume of the reused DESIGN: exit %d killed %v", code, killed)
	}
	resumed, ok := ReadResult(l.w.state, spec.RunID)
	if !ok {
		t.Fatal("the resumed run left no result")
	}
	l.closed(resumed)
	loaded, hasRun, err := journal.Load(context.Background(), l.w.st, spec.RunID)
	if err != nil || !hasRun {
		t.Fatalf("the resumed run's journal: %v %v", hasRun, err)
	}
	counts := map[string]int{}
	for _, e := range loaded {
		if e.Kind == engine.KindCall && e.Event == engine.EvCompleted {
			counts[e.Op]++
		}
		if e.Kind == engine.KindCall && e.Event == engine.EvPrepared && (e.Label == "architect" || strings.HasPrefix(e.Label, "explore-bg:") || strings.HasPrefix(e.Label, "design-audit") || strings.HasPrefix(e.Label, "design-fix")) {
			t.Errorf("the resumed run made the design call %s", e.Label)
		}
	}
	if counts["crw.designReuse"] != 1 || counts[ops.OpDesignLedger] != 1 || counts[ops.OpRelease] != 1 {
		t.Errorf("the resumed journal's rows %v, want the one decision, ledger and release", counts)
	}
}

// TestProjectLaneRejectsChangedDesignReuse (CRW-1329): a milestone whose digest changed (its title), or one whose node slices
// changed (an issue's criteria, a new plan revision), is not the one the earlier run designed: the run says why the reuse was
// rejected - the slice change checked first - then designs and audits its own DESIGN as before.
func TestProjectLaneRejectsChangedDesignReuse(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(in *compile.Input)
		replan bool
		reason string
	}{
		{"a changed milestone digest", func(in *compile.Input) {
			in.Projects[0].Milestones[0].Title = "milestone M1, renamed"
		}, false, "milestone_digest_changed"},
		{"a changed node slice", func(in *compile.Input) {
			in.Projects[0].Issues[0].Criteria = []string{"the work of A-1 is different now"}
		}, true, "node_slices_changed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
			stopAfterDesign(l, ops.OpDesignLedger, "")
			srcRun := designedSource(t, l, "", "")
			raw := l.handoffBytes(c.mutate)
			if c.replan {
				edited, err := DecodeHandoff(raw)
				if err != nil {
					t.Fatal(err)
				}
				res, err := compile.Compile(edited, compile.Options{Bases: map[string]dag.State{l.plan: dag.StateFromSnapshot(l.snapshot(0))}})
				if err != nil {
					t.Fatal(err)
				}
				if res.Plans[0].Change == nil {
					t.Fatal("the criteria change made no new plan revision")
				}
				if _, err := (&dag.Repo{Store: l.w.st}).Put(context.Background(), *res.Plans[0].Change); err != nil {
					t.Fatal(err)
				}
			}
			file := filepath.Join(t.TempDir(), "handoff.json")
			if err := os.WriteFile(file, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			spec := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", file)
			res := l.runToEnd(spec.RunID)
			l.closed(res)

			reuses := designReuseRows(t, l)
			if len(reuses) != 1 || reuses[0]["reused"] != false {
				t.Fatalf("the designReuse rows %+v, want the one that refused the reuse", reuses)
			}
			attempts, _ := reuses[0]["attempts"].([]any)
			found := false
			for _, a := range attempts {
				am, _ := a.(map[string]any)
				if am["source_run"] == srcRun && am["source_revision"] == "r0" && am["reason"] == c.reason {
					found = true
				}
			}
			if !found {
				t.Errorf("the reuse's attempts %+v name no rejection of %s r0 with reason %q", attempts, srcRun, c.reason)
			}
			// the run designed and audited its own DESIGN
			architect, audit := false, false
			for _, c2 := range labCalls(l) {
				architect = architect || c2.Label == "architect"
				audit = audit || strings.HasPrefix(c2.Label, "design-audit")
			}
			if !architect || !audit {
				t.Errorf("the rejecting run made architect %v and audit %v of its own", architect, audit)
			}
			// the fresh DESIGN was audited locally (the same payload a deterministic architect may emit is audited again, not
			// trusted by digest) and its ledger names no source
			audits, _ := labRows(l, ops.OpDesignAudit)
			if len(audits) != 1 || str(audits[0].Data, "state") != "approved" {
				t.Errorf("the local audits %+v, want the one approved audit of the fresh design", audits)
			}
			leds, _ := labRows(l, ops.OpDesignLedger)
			if len(leds) != 1 || str(leds[0].Data, "source_run") != "" || str(leds[0].Data, "digest") == "" {
				t.Errorf("the ledger of the fresh design %+v", leds)
			}
		})
	}
}

// TestProjectLaneDoesNotReuseUnapprovedDesign (CRW-1329): an earlier run that was stopped with a needs_changes audit and no ledger
// has no approved DESIGN to lend: the reuse is refused, naming that run and why, and the run designs and audits its own.
func TestProjectLaneDoesNotReuseUnapprovedDesign(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	l.turn = func(c labCall, r fakehost.TurnRequest) (fakehost.TurnPlan, bool) {
		if c.Role == "critic" && strings.HasPrefix(c.Label, "design-audit") {
			return labSay(map[string]any{"verdict": "needs_changes", "findings": []any{map[string]any{
				"ref": l.nodeOf["A-1"], "severity": "P1", "finding": "the design does not hold", "evidence": []string{}}},
				"summary": "the DESIGN needs changes", "root": nil, "first_check": nil}), true
		}
		return fakehost.TurnPlan{}, false
	}
	stopAfterDesign(l, ops.OpDesignAudit, "needs_changes")
	srcRun := designedSource(t, l, "", "")
	srcEvents, _, err := journal.Load(context.Background(), l.w.st, srcRun)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range srcEvents {
		if e.Kind == engine.KindCall && e.Event == engine.EvCompleted && (e.Op == ops.OpDesignLedger || e.Op == ops.OpRelease) {
			t.Fatalf("the source run recorded %s: a run without an approved design has nothing to lend", e.Op)
		}
	}
	l.turn = nil
	spec := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	res := l.runToEnd(spec.RunID)
	l.closed(res)

	reuses := designReuseRows(t, l)
	if len(reuses) != 1 || reuses[0]["reused"] != false {
		t.Fatalf("the designReuse rows %+v, want the one that refused the reuse", reuses)
	}
	found := false
	attempts, _ := reuses[0]["attempts"].([]any)
	for _, a := range attempts {
		am, _ := a.(map[string]any)
		if am["source_run"] == srcRun && (am["reason"] == "design_not_approved" || am["reason"] == "design_missing" || am["reason"] == "no_approved_design") {
			found = true
		}
	}
	if !found {
		t.Errorf("the reuse %+v names no rejection of %s for want of an approved design", reuses[0], srcRun)
	}
	architect, audit := false, false
	for _, c := range labCalls(l) {
		architect = architect || c.Label == "architect"
		audit = audit || strings.HasPrefix(c.Label, "design-audit")
	}
	if !architect || !audit {
		t.Errorf("the run made architect %v and audit %v of its own", architect, audit)
	}
}

// TestDesignHistoryFiltersRunAndRepository (CRW-1329): the source search names only the runs of the same plan that the same
// repository recorded, newest first - the current run never, a run of another repository's spec never, another plan's never.
func TestDesignHistoryFiltersRunAndRepository(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	stopAfterDesign(l, ops.OpDesignLedger, "")
	srcRun := designedSource(t, l, "", "")
	ctx := context.Background()

	// another repository: the run is a source in name only, with a reason and no events
	hist, err := designHistory(ctx, l.w.st, l.w.state, t.TempDir(), "run-current", l.plan)
	if err != nil || len(hist) == 0 {
		t.Fatalf("the history of another repository: %v %v", hist, err)
	}
	found := false
	for _, h := range hist {
		if h.Run == srcRun {
			found = true
			if h.Reason != "repository_mismatch" || h.Events != nil {
				t.Errorf("the mismatched source %+v, want repository_mismatch with no events", h)
			}
		}
	}
	if !found {
		t.Errorf("the history %+v does not name the source run at all", hist)
	}
	// the run itself is never a source
	hist, err = designHistory(ctx, l.w.st, l.w.state, l.repo, srcRun, l.plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hist {
		if h.Run == srcRun {
			t.Errorf("the run is its own source: %+v", h)
		}
	}
	// another plan has no sources
	hist, err = designHistory(ctx, l.w.st, l.w.state, l.repo, "run-current", "other-plan")
	if err != nil || len(hist) != 0 {
		t.Errorf("another plan's history %+v %v, want empty", hist, err)
	}
}

// ---- the close rerun over integrated nodes (CRW-1597) ----

// reuseIntegratedRows are the completed crw.reuseIntegrated rows of the current process's journal, their results decoded.
func reuseIntegratedRows(t *testing.T, l *laneLab) []map[string]any {
	t.Helper()
	var out []map[string]any
	rows, _ := labRows(l, "crw.reuseIntegrated")
	for _, row := range rows {
		var res map[string]any
		if err := json.Unmarshal([]byte(str(row.Data, "result")), &res); err != nil {
			t.Fatalf("the reuseIntegrated row's result: %v", err)
		}
		out = append(out, res)
	}
	return out
}

// journalBytes is the run's stored journal marshaled, for a byte equality check after another run.
func journalBytes(t *testing.T, l *laneLab, run string) []byte {
	t.Helper()
	ev, has, err := journal.Load(context.Background(), l.w.st, run)
	if err != nil || !has {
		t.Fatalf("the journal of run %s: %v %v", run, has, err)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// gateFailed asserts the run ended completed with the milestone escalated on the final gate's failed test evidence.
func gateFailed(t *testing.T, l *laneLab, res Result) {
	t.Helper()
	var v map[string]any
	if res.Status != engine.RunCompleted || json.Unmarshal(res.Value, &v) != nil {
		t.Fatalf("the run: %s %s %s\n%s", res.Status, res.ErrorKind, res.Error, res.Value)
	}
	if v["state"] != "escalated" || v["source"] != "final_gate" || v["reason"] != "test_failed" {
		t.Fatalf("the run's value %v, want escalated on the final gate's failed test evidence", v)
	}
	esc := false
	for _, e := range l.events() {
		if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation && str(e.Data, "reason") == "final_gate_test_failed" {
			esc = true
		}
	}
	if !esc {
		t.Errorf("no escalation recorded with reason final_gate_test_failed")
	}
}

// sourceLandings reads the completed crw.accept and crw.integrate rows of a run's stored journal, by node.
func sourceLandings(t *testing.T, l *laneLab, run string) (accepted, integrated map[string]engine.Event) {
	t.Helper()
	ev, has, err := journal.Load(context.Background(), l.w.st, run)
	if err != nil || !has {
		t.Fatalf("the journal of run %s: %v %v", run, has, err)
	}
	accepted, integrated = map[string]engine.Event{}, map[string]engine.Event{}
	for _, e := range ev {
		if e.Kind != engine.KindCall || e.Event != engine.EvCompleted {
			continue
		}
		switch e.Op {
		case ops.OpAccept:
			accepted[str(e.Data, "node")] = e
		case ops.OpIntegrate:
			integrated[str(e.Data, "node")] = e
		}
	}
	return accepted, integrated
}

// TestProjectLaneRerunsCloseWithoutRebuildingIntegratedNodes (CRW-1597): a milestone whose close failed its final gate is retried
// by a new run of the same plan: the nodes the relay still reads as integrated are not built again, the run goes straight to the
// close and makes its own gate evidence - which passes, or fails again as the first run's did. Neither run touches the target
// outside its own integrations, and the original journal is left as it was.
func TestProjectLaneRerunsCloseWithoutRebuildingIntegratedNodes(t *testing.T) {
	for _, mode := range []string{"passes", "fails_again"} {
		t.Run(mode, func(t *testing.T) {
			l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1", After: []string{"A-1"}})
			nodes := []string{l.nodeOf["A-1"], l.nodeOf["A-2"]}
			// the final gate's verifications (node "") fail; the nodes' own verifications pass
			l.verifyFail = func(req ops.VerifyRequest) bool { return req.Node == "" }
			spec := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
			gateFailed(t, l, l.runToEnd(spec.RunID))

			// the source journal: exactly two integrations, a failing node-empty crw.verify, no milestone close or report
			srcJSON := journalBytes(t, l, spec.RunID)
			accepted, integrated := sourceLandings(t, l, spec.RunID)
			if len(integrated) != 2 {
				t.Fatalf("the source run made %d integrations, want 2", len(integrated))
			}
			failedVerify, closes := false, 0
			srcEvents, has, lerr := journal.Load(context.Background(), l.w.st, spec.RunID)
			if lerr != nil || !has {
				t.Fatalf("the source run's journal: %v %v", has, lerr)
			}
			for _, e := range srcEvents {
				if e.Kind != engine.KindCall || e.Event != engine.EvCompleted {
					continue
				}
				switch e.Op {
				case ops.OpVerify:
					failedVerify = failedVerify || (str(e.Data, "node") == "" && !func() bool { b, _ := e.Data["ok"].(bool); return b }())
				case ops.OpMilestoneClose, ops.OpReportUp:
					closes++
				}
			}
			if !failedVerify || closes != 0 {
				t.Fatalf("the source journal: failing gate verification %v, closes %d, want a failed gate and no close", failedVerify, closes)
			}
			savedVerifications, savedTip := l.verifications, l.git(l.repo, "rev-parse", l.target)
			if mode == "passes" {
				l.verifyFail = nil
			}

			// the second run of the same plan: every node the relay reads as integrated is inherited, none is built
			spec2 := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
			res2 := l.runToEnd(spec2.RunID)
			if mode == "passes" {
				l.closed(res2)
			} else {
				gateFailed(t, l, res2)
				if n := len(mustRows(l, ops.OpCrosscheck)) + len(mustRows(l, ops.OpMilestoneClose)) + len(mustRows(l, ops.OpReportUp)); n != 0 {
					t.Errorf("the failed rerun made %d crosscheck/close/report rows", n)
				}
			}
			if tip := l.git(l.repo, "rev-parse", l.target); tip != savedTip {
				t.Errorf("the integration tip moved to %s, want %s", tip, savedTip)
			}
			if l.verifications != savedVerifications {
				t.Errorf("%d merged-tree verifications, want %d (no integration batch ran)", l.verifications, savedVerifications)
			}
			for _, c := range labCalls(l) {
				if c.Role == "executor" || c.Role == "executor-t1" {
					t.Errorf("the rerun started the executor call %s (%s)", c.Label, c.Role)
				}
				if strings.HasPrefix(c.Label, "build:") || strings.HasPrefix(c.Label, "rebuild:") {
					t.Errorf("the rerun made the build call %s", c.Label)
				}
			}
			for _, e := range l.events() {
				if e.Kind != engine.KindCall || e.Event != engine.EvCompleted {
					continue
				}
				switch e.Op {
				case ops.OpRelease, ops.OpCommit, ops.OpAccept, ops.OpIntegrate:
					t.Errorf("the rerun wrote a %s row: %v", e.Op, e.Data)
				case ops.OpVerify:
					if str(e.Data, "node") != "" {
						t.Errorf("the rerun verified a node's head: %v", e.Data)
					}
				}
			}
			reuses := reuseIntegratedRows(t, l)
			if len(reuses) != 2 {
				t.Fatalf("the reuseIntegrated rows %+v, want two", reuses)
			}
			for _, ru := range reuses {
				node := str(ru, "node")
				acc, ing := accepted[node], integrated[node]
				if ru["reused"] != true || ru["source_run"] != spec.RunID ||
					ru["acceptance_id"] != str(acc.Data, "acceptance_id") || ru["head"] != str(acc.Data, "head") ||
					str(ru, "source_accept_call") == "" || str(ru, "source_integrate_call") == "" || str(ru, "batch_commit") == "" ||
					ru["batch_commit"] != str(ing.Data, "batch_commit") {
					t.Errorf("the reuse of %s %+v does not carry the source run %s, its acceptance %s and integration %s",
						node, ru, spec.RunID, acc.Data, ing.Data)
				}
			}
			if b := journalBytes(t, l, spec.RunID); !slices.Equal(b, srcJSON) {
				t.Errorf("the source run's journal changed during the rerun")
			}
			if mode != "passes" {
				return
			}
			// the close audits every acceptance the milestone rests on, the inherited ones read from the original journal
			audits, _ := labRows(l, "crw.audit")
			if len(audits) != 1 {
				t.Fatalf("the audit rows %+v, want the one", audits)
			}
			var outcome ops.AuditOutcome
			if err := json.Unmarshal([]byte(str(audits[0].Data, "result")), &outcome); err != nil {
				t.Fatalf("the audit's result: %v", err)
			}
			if len(outcome.Results) != 2 {
				t.Fatalf("the audit judged %d acceptances, want the two of the milestone", len(outcome.Results))
			}
			got := map[string]bool{}
			for _, res := range outcome.Results {
				acc := accepted[res.Node]
				if res.AcceptanceID != str(acc.Data, "acceptance_id") || got[res.AcceptanceID] {
					t.Errorf("the audit result of %s names acceptance %s, want each original once", res.Node, res.AcceptanceID)
				}
				got[res.AcceptanceID] = true
			}
			for _, node := range nodes {
				if !got[str(accepted[node].Data, "acceptance_id")] {
					t.Errorf("the audit results %v miss the original acceptance of %s", got, node)
				}
			}
			// the reflection's currentDesign and the close's re-plan numbers read the inherited landings: both nodes' heads
			designs, _ := labRows(l, "crw.currentDesign")
			if len(designs) == 0 {
				t.Fatalf("no crw.currentDesign row")
			}
			var cd ops.CurrentDesign
			if err := json.Unmarshal([]byte(str(designs[len(designs)-1].Data, "result")), &cd); err != nil {
				t.Fatalf("the currentDesign result: %v", err)
			}
			for _, node := range nodes {
				found := false
				for _, ld := range cd.Landed {
					if ld.Node == node && ld.Head == str(accepted[node].Data, "head") {
						found = true
					}
				}
				if !found {
					t.Errorf("the currentDesign landed %+v names no landing of %s at %s", cd.Landed, node, str(accepted[node].Data, "head"))
				}
			}
			// a resume of the closed rerun replays the recorded reuse: no new verification, build or reuse rows, target unchanged
			beforeVerifications := l.verifications
			if code, killed := l.process(spec2.RunID, true); code != 0 || killed {
				t.Fatalf("the resume of the closed rerun: exit %d killed %v", code, killed)
			}
			l.closed(mustResult(t, l, spec2.RunID))
			loaded, has, err := journal.Load(context.Background(), l.w.st, spec2.RunID)
			if err != nil || !has {
				t.Fatalf("the resumed journal: %v %v", has, err)
			}
			counts := map[string]int{}
			for _, e := range loaded {
				if e.Kind == engine.KindCall && e.Event == engine.EvCompleted {
					counts[e.Op]++
				}
			}
			if counts["crw.reuseIntegrated"] != 2 || counts[ops.OpVerify] != 1 || counts[ops.OpRelease] != 0 {
				t.Errorf("the resumed journal's rows %v, want the two reuse decisions and the one gate verification", counts)
			}
			if l.verifications != beforeVerifications {
				t.Errorf("the resume ran %d merged-tree verifications", l.verifications-beforeVerifications)
			}
			if tip := l.git(l.repo, "rev-parse", l.target); tip != savedTip {
				t.Errorf("the resume moved the integration tip to %s", tip)
			}
		})
	}
}

// mustRows is labRows' row list alone.
func mustRows(l *laneLab, op string) []engine.Event {
	rows, _ := labRows(l, op)
	return rows
}

// mustResult is the stored result of a run.
func mustResult(t *testing.T, l *laneLab, run string) Result {
	t.Helper()
	res, ok := ReadResult(l.w.state, run)
	if !ok {
		t.Fatalf("run %s left no result", run)
	}
	return res
}

// TestProjectLaneReusesIntegratedPredecessorOnly (CRW-1597): a run killed after the integration of A-1 but before A-2 started is
// continued by a new run of the same plan: A-1 is inherited landed and only A-2 is released and built - a pending successor builds
// after its inherited predecessor without releasing it.
func TestProjectLaneReusesIntegratedPredecessorOnly(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1", After: []string{"A-1"}})
	node1, node2 := l.nodeOf["A-1"], l.nodeOf["A-2"]
	spec := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	l.afterRow = func(e engine.Event) {
		if e.Kind != engine.KindCall || e.Op != ops.OpIntegrate || e.Event != engine.EvCompleted || str(e.Data, "node") != node1 {
			return
		}
		l.mu.Lock()
		cur := l.cur
		l.mu.Unlock()
		if f, ok := cur.Journal.(interface{ Flush() error }); ok {
			if err := f.Flush(); err != nil {
				l.t.Fatal(err)
			}
		}
		l.kill()
	}
	if _, killed := l.process(spec.RunID, false); !killed {
		t.Fatalf("the source run was not killed after the integration of %s", node1)
	}
	l.afterRow = nil
	accepted, integrated := sourceLandings(t, l, spec.RunID)
	if len(integrated) != 1 || integrated[node1].CallID == "" || len(accepted) != 1 || accepted[node1].CallID == "" {
		t.Fatalf("the source journal: %d acceptances, %d integrations, want the one of %s", len(accepted), len(integrated), node1)
	}
	srcJSON := journalBytes(t, l, spec.RunID)

	spec2 := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	v2 := l.closed(l.runToEnd(spec2.RunID))
	for _, ln := range v2.Lanes {
		if ln.State != "landed" {
			t.Fatalf("the lane of %s: %+v, want landed", ln.Node, ln)
		}
	}
	// A-1 was inherited: no release, no build, no calls of it at all - only its reuse decision row; A-2 got its one release and its executor build
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Event == engine.EvCompleted && str(e.Data, "node") == node1 && e.Op != "crw.reuseIntegrated" {
			t.Errorf("the rerun wrote a %s row of %s: %v", e.Op, node1, e.Data)
		}
		if e.Kind == engine.KindCall && e.Event == engine.EvPrepared && str(e.Data, "node") == node1 {
			t.Errorf("the rerun made the call %s of %s", e.Label, node1)
		}
	}
	builds, releases, integrations := 0, 0, 0
	for _, c := range labCalls(l) {
		if c.Label == "build:"+node2+":executor" {
			builds++
		}
	}
	for _, e := range l.events() {
		if e.Kind != engine.KindCall || e.Event != engine.EvCompleted {
			continue
		}
		switch e.Op {
		case ops.OpRelease:
			if str(e.Data, "node") == node2 {
				releases++
			}
		case ops.OpIntegrate:
			if str(e.Data, "node") == node2 {
				integrations++
			}
		}
	}
	if builds != 1 || releases != 1 || integrations != 1 {
		t.Errorf("%s: %d builds, %d releases, %d integrations, want one of each", node2, builds, releases, integrations)
	}
	reuses := reuseIntegratedRows(t, l)
	if len(reuses) != 2 {
		t.Fatalf("the reuseIntegrated rows %+v, want two", reuses)
	}
	seen := map[string]bool{}
	for _, ru := range reuses {
		node := str(ru, "node")
		seen[node] = true
		want := node == node1
		if r, _ := ru["reused"].(bool); r != want {
			t.Errorf("the reuse of %s: %+v, want reused %v", node, ru, want)
		}
		if node == node1 && (ru["source_run"] != spec.RunID || ru["acceptance_id"] != str(accepted[node1].Data, "acceptance_id") || ru["head"] != str(accepted[node1].Data, "head")) {
			t.Errorf("the reuse of %s %+v does not carry the source run's landing", node1, ru)
		}
	}
	if !seen[node1] || !seen[node2] {
		t.Errorf("the reuse rows %v name both nodes", seen)
	}
	// the close's audit covers the inherited acceptance of the source run and the fresh one of this run
	freshAccept := ""
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Op == ops.OpAccept && e.Event == engine.EvCompleted && str(e.Data, "node") == node2 {
			freshAccept = str(e.Data, "acceptance_id")
		}
	}
	audits, _ := labRows(l, "crw.audit")
	if len(audits) != 1 {
		t.Fatalf("the audit rows %+v, want the one", audits)
	}
	var outcome ops.AuditOutcome
	if err := json.Unmarshal([]byte(str(audits[0].Data, "result")), &outcome); err != nil {
		t.Fatalf("the audit's result: %v", err)
	}
	got := map[string]bool{}
	for _, res := range outcome.Results {
		got[res.AcceptanceID] = true
	}
	if len(outcome.Results) != 2 || !got[str(accepted[node1].Data, "acceptance_id")] || !got[freshAccept] {
		t.Errorf("the audit judged %+v, want the inherited acceptance %s and the fresh %s", got, str(accepted[node1].Data, "acceptance_id"), freshAccept)
	}
	if b := journalBytes(t, l, spec.RunID); !slices.Equal(b, srcJSON) {
		t.Errorf("the source run's journal changed during the rerun")
	}
}

// TestProjectLaneRefusesReuseWhenIntegratedHeadIsLost (CRW-1597): when the target no longer holds the head the source run
// integrated, a new run of the same plan does not rebuild the node: the reuse is refused integration_reuse_unproven and the run
// ends on the refusal without a release, an acceptance or an integration of its own.
func TestProjectLaneRefusesReuseWhenIntegratedHeadIsLost(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	l.verifyFail = func(req ops.VerifyRequest) bool { return req.Node == "" }
	spec := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	gateFailed(t, l, l.runToEnd(spec.RunID))
	base := ""
	srcEvents, has, err := journal.Load(context.Background(), l.w.st, spec.RunID)
	if err != nil || !has {
		t.Fatalf("the source run's journal: %v %v", has, err)
	}
	for _, e := range srcEvents {
		if e.Kind == engine.KindCall && e.Op == ops.OpDesignLedger && e.Event == engine.EvCompleted {
			base = str(e.Data, "base_tip")
		}
	}
	if base == "" || base == l.git(l.repo, "rev-parse", l.target) {
		t.Fatalf("the pre-integration base %q, want the commit the target held before the run", base)
	}
	// the target loses the integrated head
	l.git(l.repo, "update-ref", "refs/heads/"+l.target, base)

	spec2 := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	res := l.runToEnd(spec2.RunID)
	if res.Status == engine.RunCompleted || !strings.Contains(res.Error, "integration_reuse_unproven") {
		t.Fatalf("the rerun's result %s %s %q, want the integration_reuse_unproven refusal", res.Status, res.ErrorKind, res.Error)
	}
	for _, e := range l.events() {
		if e.Kind != engine.KindCall || e.Event != engine.EvCompleted {
			continue
		}
		switch e.Op {
		case ops.OpRelease, ops.OpAccept, ops.OpIntegrate:
			t.Errorf("the refused rerun wrote a %s row: %v", e.Op, e.Data)
		}
	}
	for _, c := range labCalls(l) {
		if c.Role == "executor" || c.Role == "executor-t1" || strings.HasPrefix(c.Label, "build:") {
			t.Errorf("the refused rerun started the executor call %s (%s)", c.Label, c.Role)
		}
	}
}

// TestProjectLaneCloseRerunKeepsMilestoneReviewBase (CRW-1597): the source run integrates C-1, re-plans after A-1's unit check,
// and closes stopped at the final gate: its latest ledger's base_tip sits after C-1. The rerun inherits every landing; its own
// gate, crosscheck and close must still judge the milestone's whole original range (from the first ledger's base_tip), not the
// narrowed range of the newest design revision.
func TestProjectLaneCloseRerunKeepsMilestoneReviewBase(t *testing.T) {
	l := newLaneLab(t,
		labIssue{Key: "C-1", Milestone: "M1"},
		labIssue{Key: "A-1", Milestone: "M1", After: []string{"C-1"}},
		labIssue{Key: "B-1", Milestone: "M1", After: []string{"A-1"}})
	nodeA := l.nodeOf["A-1"]
	l.UnitChecks("A-1", ops.UnitCheckUncommitted, labCheck{Verdict: reviewadapter.VerdictNeedsChanges,
		Findings: []reviewadapter.Finding{{Severity: "P2", Impact: "core_function", File: "node-A-1.txt", Line: 1, Text: "fix this"}}})
	l.turn = func(c labCall, r fakehost.TurnRequest) (fakehost.TurnPlan, bool) {
		if strings.HasPrefix(c.Label, "build:"+nodeA+":") || strings.HasPrefix(c.Label, "fix:"+nodeA+":") {
			return l.executorTurn(c, map[string]any{"plan_mismatch": "the plan assumed something else: " + c.Label}), true
		}
		return fakehost.TurnPlan{}, false
	}
	l.verifyFail = func(req ops.VerifyRequest) bool { return req.Node == "" }
	spec := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	gateFailed(t, l, l.runToEnd(spec.RunID))
	leds, _ := labRows(l, ops.OpDesignLedger)
	if len(leds) < 2 {
		t.Fatalf("the source run made %d design ledger rows, want the original and the re-plan's", len(leds))
	}
	originalBase, latestBase := str(leds[0].Data, "base_tip"), str(leds[len(leds)-1].Data, "base_tip")
	if originalBase == "" || originalBase == latestBase {
		t.Fatalf("the source run's ledger bases %q then %q, want the re-plan's base different from the original", originalBase, latestBase)
	}
	srcJSON := journalBytes(t, l, spec.RunID)
	savedTip := l.git(l.repo, "rev-parse", l.target)

	l.verifyFail = nil
	spec2 := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	l.closed(l.runToEnd(spec2.RunID))
	if tip := l.git(l.repo, "rev-parse", l.target); tip != savedTip {
		t.Fatalf("the integration tip moved to %s, want %s", tip, savedTip)
	}
	for _, c := range labCalls(l) {
		if c.Role == "executor" || c.Role == "executor-t1" || strings.HasPrefix(c.Label, "build:") || strings.HasPrefix(c.Label, "rebuild:") {
			t.Errorf("the rerun started the executor call %s (%s)", c.Label, c.Role)
		}
	}
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Event == engine.EvCompleted {
			switch e.Op {
			case ops.OpRelease, ops.OpCommit, ops.OpAccept, ops.OpIntegrate:
				t.Errorf("the rerun wrote a %s row: %v", e.Op, e.Data)
			}
		}
	}
	// every gate row of the rerun judges the milestone's original range
	gates, _ := labRows(l, ops.OpGate)
	if len(gates) == 0 {
		t.Fatalf("the rerun made no final gate")
	}
	for _, g := range gates {
		if str(g.Data, "base") != originalBase || str(g.Data, "range") != originalBase+".."+savedTip {
			t.Errorf("the gate judged base %s range %s, want %s .. %s", str(g.Data, "base"), str(g.Data, "range"), originalBase, savedTip)
		}
	}
	checks, _ := labRows(l, ops.OpCrosscheck)
	if len(checks) != 1 {
		t.Fatalf("the crosscheck rows %+v, want the one", checks)
	}
	var cc ops.CrosscheckOutcome
	if err := json.Unmarshal([]byte(str(checks[0].Data, "result")), &cc); err != nil {
		t.Fatalf("the crosscheck's result: %v", err)
	}
	if cc.Verdict == nil || cc.Verdict.Base != originalBase || cc.Verdict.Head != savedTip {
		t.Errorf("the crosscheck's verdict %+v, want the original base %s at %s", cc.Verdict, originalBase, savedTip)
	}
	closes, _ := labRows(l, ops.OpMilestoneClose)
	if len(closes) != 1 {
		t.Fatalf("the milestone close rows %+v, want the one", closes)
	}
	var closeOut ops.CloseResultOutcome
	if err := json.Unmarshal([]byte(str(closes[0].Data, "result")), &closeOut); err != nil {
		t.Fatalf("the close's result: %v", err)
	}
	if closeOut.Base != originalBase || closeOut.Tip != savedTip {
		t.Errorf("the close recorded base %s tip %s, want %s .. %s", closeOut.Base, closeOut.Tip, originalBase, savedTip)
	}
	for _, ru := range reuseIntegratedRows(t, l) {
		if ru["reused"] != true || str(ru, "review_base") != originalBase {
			t.Errorf("the reuse %+v, want reused with the original review base %s", ru, originalBase)
		}
	}
	if b := journalBytes(t, l, spec.RunID); !slices.Equal(b, srcJSON) {
		t.Errorf("the source run's journal changed during the rerun")
	}
}

// A fresh architect answer cannot change a decision an integrated issue names.
// The workflow restores it before audit; the build's refusal of an actually changed
// ledger is covered by TestReuseIntegratedStillRefusesChangedDesign in ops.
func TestProjectLaneRestoresChangedIntegratedDecision(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	l.verifyFail = func(req ops.VerifyRequest) bool { return req.Node == "" }
	spec := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", l.file)
	gateFailed(t, l, l.runToEnd(spec.RunID))
	savedTip := l.git(l.repo, "rev-parse", l.target)

	l.verifyFail = nil
	raw := l.handoffBytes(func(in *compile.Input) { in.Projects[0].Milestones[0].Title = "renamed M1" })
	handoff2 := filepath.Join(t.TempDir(), "handoff.json")
	if err := os.WriteFile(handoff2, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	l.designEdit = func(d *ops.Design) { d.Decisions[0].Change = "Use a different storage format" }
	spec2 := l.start("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1"}`, l.plan), "--handoff", handoff2)
	res := l.runToEnd(spec2.RunID)
	l.closed(res)
	leds, _ := labRows(l, ops.OpDesignLedger)
	if len(leds) != 1 {
		t.Fatalf("expected one restored ledger, got %d", len(leds))
	}
	rawDesign, _ := json.Marshal(leds[0].Data["design"])
	var design ops.Design
	if err := json.Unmarshal(rawDesign, &design); err != nil || len(design.Decisions) != 1 || design.Decisions[0].Change != "each node writes its own file" {
		t.Fatalf("original decision not restored: %s (%v)", rawDesign, err)
	}
	for _, ru := range reuseIntegratedRows(t, l) {
		if ru["reused"] != true || ru["source_run"] != spec.RunID {
			t.Errorf("the restored-decision rerun did not reuse its landing: %+v", ru)
		}
	}
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Event == engine.EvCompleted {
			switch e.Op {
			case ops.OpRelease, ops.OpAccept, ops.OpIntegrate:
				t.Errorf("the rerun wrote a new %s row for an inherited landing: %v", e.Op, e.Data)
			}
		}
	}
	for _, c := range labCalls(l) {
		if c.Role == "executor" || c.Role == "executor-t1" || strings.HasPrefix(c.Label, "build:") {
			t.Errorf("the rerun started an executor for an inherited landing: %s (%s)", c.Label, c.Role)
		}
	}
	if tip := l.git(l.repo, "rev-parse", l.target); tip != savedTip {
		t.Errorf("the integration tip moved to %s, want %s", tip, savedTip)
	}
}
