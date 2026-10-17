package workflows

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/roles"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
)

// packetText runs a script that imports from crw:lib/packets and returns the packet text it returns.
func packetText(t *testing.T, name, imports, expr string) string {
	t.Helper()
	src := "import { " + imports + " } from 'crw:lib/packets'\nexport const meta = { name: '" + name + "' }\n" +
		"const ms = { id: 'M1', plan: 'P', title: 'the milestone', nodes: [" +
		"{ id: 'n1', issue: 'CRW-1', title: 'first', after: [], regions: ['a.go'], criteria: [] }, " +
		"{ id: 'n2', issue: 'CRW-2', title: 'second', after: ['n1'], regions: ['b.go'], criteria: [] }, " +
		"{ id: 'n3', issue: 'CRW-3', title: 'third', after: ['n1'], regions: ['c.go'], criteria: [] }] }\n" +
		"const cur = { revision: 1, base_tip: 'base0', landed: [{ node: 'n1', head: 'h1' }], design: { decisions: [{ id: 'D1', change: 'split the store' }] } }\n" +
		"const due = { reasons: ['plan_mismatch'], plan_mismatch: 2, plan_mismatch_calls: ['c1', 'c2'], slice_change: { changed: [], total: 3 } }\n" +
		"return " + expr + "\n"
	prog, err := script.Load(script.Source{Name: name + ".js", Text: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Run(context.Background(), prog, engine.Config{Backend: engine.NewFake(), Policy: engine.FakePolicy()})
	if err != nil || res.Status != engine.RunCompleted {
		t.Fatalf("%v %+v", err, res)
	}
	var packet string
	if err := json.Unmarshal(res.Value, &packet); err != nil {
		t.Fatalf("the packet is not a string: %v (%s)", err, res.Value)
	}
	return packet
}

// unstartedOf is the list of the UNSTARTED NODES section of a packet: the lines after its heading up to the blank line that ends it.
func unstartedOf(packet string) string {
	_, rest, ok := strings.Cut(packet, "UNSTARTED NODES")
	if !ok {
		return ""
	}
	_, rest, _ = strings.Cut(rest, ":\n")
	body, _, _ := strings.Cut(rest, "\n\n")
	return body
}

// TestReplanPacketAsksForTheWholeDesign (CRW-1462): the re-plan turn asks the architect for the whole DESIGN, not a delta: every
// decision and every issue of every node, the started ones as they were released, with the ids kept. The packet names the nodes
// that have not started (UNSTARTED NODES), the ones the new revision releases; at the close none is left.
func TestReplanPacketAsksForTheWholeDesign(t *testing.T) {
	signal := packetText(t, "replan-packet", "replanPacket", "replanPacket(ms, cur, due, 'tip1', null, ['n2', 'n3'])")
	for _, want := range []string{
		"the whole DESIGN",
		"every decision",
		"every issue of every node",
		"UNSTARTED NODES",
		"- n2 (CRW-2): second",
		"- n3 (CRW-3): third",
		"Keep the ids",
	} {
		if !strings.Contains(signal, want) {
			t.Errorf("the re-plan packet lacks %q:\n%s", want, signal)
		}
	}
	for _, gone := range []string{"delta of the plan", "delta"} {
		if strings.Contains(signal, gone) {
			t.Errorf("the re-plan packet still says %q:\n%s", gone, signal)
		}
	}
	if strings.Contains(signal, "- n1 (CRW-1)") {
		t.Errorf("the re-plan packet lists the started node n1 as unstarted:\n%s", signal)
	}
	closing := packetText(t, "replan-close-packet", "replanPacket", "replanPacket(ms, cur, due, 'tip1', [], [])")
	if unstartedOf(closing) != "- none" {
		t.Errorf("the re-plan packet of the close does not say no node is unstarted:\n%s", closing)
	}
	if !strings.Contains(closing, "the whole DESIGN") {
		t.Errorf("the re-plan packet of the close does not ask for the whole DESIGN:\n%s", closing)
	}
}

// TestCriticReplanPacketListsUnstartedNodes (CRW-1462): the critic of a re-plan reads the nodes that have not started, and an
// unstarted node with no issue in the DESIGN is a P1 finding. The first DESIGN's critic packet (no context) has no such section.
func TestCriticReplanPacketListsUnstartedNodes(t *testing.T) {
	cand := "{ design: { decisions: [], issues: [{ node: 'n2' }] }, answers: [] }"
	replan := packetText(t, "critic-replan-packet", "designAuditPacket",
		"designAuditPacket(ms, "+cand+", 1, [], { head: 'tip1', base_tip: 'base0', landed: [{ node: 'n1', head: 'h1' }], unstarted: ['n2', 'n3'] })")
	for _, want := range []string{
		"UNSTARTED NODES",
		"- n2 (CRW-2): second",
		"- n3 (CRW-3): third",
		"an unstarted node with no issue in the DESIGN is a P1 finding",
		// a design-fix answer of the re-plan becomes the revision when approved, so it is held to the whole DESIGN as well
		"the architect's design-fix answers included",
		"a started node whose issue is missing",
	} {
		if !strings.Contains(replan, want) {
			t.Errorf("the critic packet of a re-plan lacks %q:\n%s", want, replan)
		}
	}
	closing := packetText(t, "critic-close-packet", "designAuditPacket",
		"designAuditPacket(ms, "+cand+", 1, [], { head: 'tip1', base_tip: 'base0', landed: [], unstarted: [] })")
	if unstartedOf(closing) != "- none" {
		t.Errorf("the critic packet of the close does not say no node is unstarted:\n%s", closing)
	}
	first := packetText(t, "critic-first-packet", "designAuditPacket", "designAuditPacket(ms, "+cand+", 1, [], undefined)")
	if strings.Contains(first, "UNSTARTED NODES") || strings.Contains(first, "unstarted node") {
		t.Errorf("the critic packet of the first DESIGN names unstarted nodes:\n%s", first)
	}
}

// TestCriticPacketPriorFindingsRendersEveryDisposition (CRW-1452): from the second round on, the critic's PRIOR FINDINGS lists each
// finding of the earlier rounds by id with the architect's disposition of it: folded or rebutted with its reason, not reported when
// the answer named no disposition of it, null when the round's answer had no dispositions at all; a disposition that names no
// finding of its round is listed on its own line. The first round has no such section, and a second round with no prior round
// says none.
func TestCriticPacketPriorFindingsRendersEveryDisposition(t *testing.T) {
	cand := "{ design: { decisions: [], issues: [] }, answers: [] }"
	prior := "[" +
		"{ round: 1, findings: auditFindings(1, [" +
		"{ ref: 'a.go', severity: 'P1', finding: 'fold me' }, " +
		"{ ref: 'b.go', severity: 'P2', finding: 'rebut me' }, " +
		"{ ref: 'c.go', severity: 'P3', finding: 'forget me' }]), " +
		"dispositions: [" +
		"{ finding: 'r1.1', disposition: 'folded', reason: 'done in D2' }, " +
		"{ finding: 'r1.2', disposition: 'rebutted', reason: 'outside the milestone' }, " +
		"{ finding: 'r1.9', disposition: 'folded', reason: 'a mistyped id' }] }, " +
		"{ round: 2, findings: auditFindings(2, [{ ref: 'd.go', severity: 'P1', finding: 'still open' }]), dispositions: null }]"
	packet := packetText(t, "critic-prior-packet", "designAuditPacket, auditFindings", "designAuditPacket(ms, "+cand+", 3, "+prior+", undefined)")
	got, ok := priorFindingsOf(packet)
	if !ok {
		t.Fatalf("the critic packet of round 3 has no PRIOR FINDINGS:\n%s", packet)
	}
	want := []string{
		"- r1.1 [P1] a.go: fold me -> folded: done in D2",
		"- r1.2 [P2] b.go: rebut me -> rebutted: outside the milestone",
		"- r1.3 [P3] c.go: forget me -> not reported",
		"- r1.9 (no finding of round 1 has this id) -> folded: a mistyped id",
		"- r2.1 [P1] d.go: still open -> null (no dispositions reported)",
	}
	if !slices.Equal(got, want) {
		t.Errorf("PRIOR FINDINGS of round 3:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	empty := packetText(t, "critic-no-prior-packet", "designAuditPacket", "designAuditPacket(ms, "+cand+", 2, [], undefined)")
	if got, _ := priorFindingsOf(empty); !slices.Equal(got, []string{"- none"}) {
		t.Errorf("the critic packet of round 2 with no prior round lists %q, want none:\n%s", got, empty)
	}
	first := packetText(t, "critic-round-one-packet", "designAuditPacket, auditFindings", "designAuditPacket(ms, "+cand+", 1, "+prior+", undefined)")
	if strings.Contains(first, "PRIOR FINDINGS") {
		t.Errorf("the critic packet of round 1 has PRIOR FINDINGS:\n%s", first)
	}
}

// priorFindingsOf is the list of the PRIOR FINDINGS section of a critic packet: the "- " lines after its heading.
func priorFindingsOf(packet string) ([]string, bool) {
	_, rest, ok := strings.Cut(packet, "PRIOR FINDINGS")
	if !ok {
		return nil, false
	}
	_, rest, _ = strings.Cut(rest, ":\n")
	var out []string
	for _, line := range strings.Split(rest, "\n") {
		if !strings.HasPrefix(line, "- ") {
			break
		}
		out = append(out, line)
	}
	return out, true
}

// TestCriticReplanPacketListsReleasedPlans: the critic of a re-plan is asked whether the started nodes keep their issues as they were
// released, so its packet carries them: RELEASED PLANS lists each started node with the revision it was released under, its approach,
// edit regions and decision refs and the decisions those refs name, and a started node whose issue in the DESIGN differs from it is
// a P1 finding. The first DESIGN's critic packet has no such section, and a re-plan with no started node says none.
func TestCriticReplanPacketListsReleasedPlans(t *testing.T) {
	cand := "{ design: { decisions: [], issues: [] }, answers: [] }"
	released := "[{ node: 'n1', revision: 'r0', design_digest: 'dg0', plan: { issue: 'CRW-1', node: 'n1', size_fit: 'executor', size_reason: '', issue_size_decision: 'ok', " +
		"edit_regions: ['a.go', 'a_test.go'], approach: 'split the store first', tests_first: ['TestSplit'], done_condition: 'go test ./a', decision_refs: ['D1'], order_after: [], group: null }, " +
		"decisions: [{ id: 'D1', evidence: ['a.go:1'], change: 'split the store', alternatives: ['keep it'], assumptions: ['one writer'] }] }]"
	replan := packetText(t, "critic-released-packet", "designAuditPacket",
		"designAuditPacket(ms, "+cand+", 1, [], { head: 'tip1', base_tip: 'base0', landed: [{ node: 'n1', head: 'h1' }], unstarted: ['n2', 'n3'], released: "+released+" })")
	for _, want := range []string{
		"RELEASED PLANS",
		"- n1 (CRW-1), released under r0\n  plan: {\"approach\":\"split the store first\",\"decision_refs\":[\"D1\"],\"done_condition\":\"go test ./a\",\"edit_regions\":[\"a.go\",\"a_test.go\"],\"group\":null,\"issue\":\"CRW-1\",\"issue_size_decision\":\"ok\",\"node\":\"n1\",\"order_after\":[],\"size_fit\":\"executor\",\"size_reason\":\"\",\"tests_first\":[\"TestSplit\"]}",
		"\n  decisions: [{\"alternatives\":[\"keep it\"],\"assumptions\":[\"one writer\"],\"change\":\"split the store\",\"evidence\":[\"a.go:1\"],\"id\":\"D1\"}]",
		"a started node whose issue in the DESIGN differs from its plan in RELEASED PLANS in any field is a P1 finding that names the node, and so is a decision a released plan names that the DESIGN drops or changes in any field",
	} {
		if !strings.Contains(replan, want) {
			t.Errorf("the critic packet of a re-plan lacks %q:\n%s", want, replan)
		}
	}
	// a node sized for a split with no sub-tasks (a legacy row), a child or a bundle split started too: it is listed with how it
	// started; a split with sub-tasks is released to an executor and listed as released (CRW-1459)
	// a bundle split says a bundle_split_request escalation was raised for it and names the escalation id (crw.releasedPlans
	// escalation_id, CRW-1437), and before it is recorded only that its lane escalates it so. It makes no claim about whether the
	// request reached anyone, nor names a recipient, whatever an entry carries: asked or request members (which an earlier binary of this branch wrote)
	// change nothing, since the result of crw.releasedPlans is replayed on a resume
	const raised = "- n2 (CRW-2), sized for a bundle split under r0 (no executor started; a bundle_split_request escalation was raised for it: esc-7)\n  plan: {"
	for _, row := range []struct{ name, kind, asked, want string }{
		{"split", "split", "", "- n2 (CRW-2), sized for a split under r0 with no sub-tasks (a legacy split row: no executor started; its lane escalated it as size_split)\n  plan: {"},
		{"child", "child", "", "- n2 (CRW-2), sized for a child under r0 (no executor started; its lane escalated it as size_split)\n  plan: {"},
		{"bundle_split-raised", "bundle_split", "escalation_id: 'esc-7', ", raised},
		{"bundle_split-asked", "bundle_split", "asked: true, escalation_id: 'esc-7', ", raised},
		{"bundle_split-unasked", "bundle_split", "asked: false, escalation_id: 'esc-7', ", raised},
		{"bundle_split-queued", "bundle_split", "request: 'queued', escalation_id: 'esc-7', ", raised},
		{"bundle_split-delivered", "bundle_split", "asked: true, request: 'delivered', escalation_id: 'esc-7', ", raised},
		{"bundle_split", "bundle_split", "", "- n2 (CRW-2), sized for a bundle split under r0 (no executor started; its lane escalates it as size_split with ask bundle_split_request)\n  plan: {"},
		{"bundle_split-unraised-asked", "bundle_split", "asked: true, request: 'delivered', ", "- n2 (CRW-2), sized for a bundle split under r0 (no executor started; its lane escalates it as size_split with ask bundle_split_request)\n  plan: {"},
		{"released", "released", "", "- n2 (CRW-2), released under r0\n  plan: {"},
	} {
		sized := packetText(t, "critic-released-"+row.name+"-packet", "designAuditPacket",
			"designAuditPacket(ms, "+cand+", 1, [], { head: 'tip1', base_tip: 'base0', landed: [], unstarted: [], released: [{ node: 'n2', kind: '"+row.kind+"', "+row.asked+"revision: 'r0', design_digest: 'dg0', plan: { issue: 'CRW-2', size_fit: 'split' }, decisions: [] }] })")
		if !strings.Contains(sized, row.want) {
			t.Errorf("a released entry %s:\n%s", row.name, sized)
		}
		_, entry, _ := strings.Cut(sized, "- n2 (CRW-2), ")
		entry, _, _ = strings.Cut(entry, "\n  plan: {")
		// nor who the request went to: a run whose plan names no project tells the 자식's own thread, not the 부모
		for _, claim := range []string{"asked", "deliver", "queued", "reached", "sent", "told", "부모", "자식", "parent", "orchestrator", "thread"} {
			if strings.Contains(entry, claim) {
				t.Errorf("a released entry %s makes a delivery or recipient claim (%q): %s", row.name, claim, entry)
			}
		}
	}
	// an entry of the flat form an earlier lane binary recorded is rendered, not thrown on
	flat := packetText(t, "critic-released-flat-packet", "designAuditPacket",
		"designAuditPacket(ms, "+cand+", 1, [], { head: 'tip1', base_tip: 'base0', landed: [], unstarted: [], released: [{ node: 'n1', issue: 'CRW-1', revision: 'r0', approach: 'a', edit_regions: [], decision_refs: [], decisions: [] }] })")
	if !strings.Contains(flat, "- n1 (CRW-1), released under r0\n  plan: {") || !strings.Contains(flat, `"approach":"a"`) {
		t.Errorf("a flat released entry:\n%s", flat)
	}
	none := packetText(t, "critic-released-none-packet", "designAuditPacket",
		"designAuditPacket(ms, "+cand+", 1, [], { head: 'tip1', base_tip: 'base0', landed: [], unstarted: ['n1', 'n2', 'n3'], released: [] })")
	if _, rest, ok := strings.Cut(none, "RELEASED PLANS"); !ok || !strings.Contains(strings.SplitN(rest, "\n\n", 2)[0], "\n- none") {
		t.Errorf("the critic packet of a re-plan with no started node does not say none:\n%s", none)
	}
	first := packetText(t, "critic-first-released-packet", "designAuditPacket", "designAuditPacket(ms, "+cand+", 1, [], undefined)")
	if strings.Contains(first, "RELEASED PLANS") {
		t.Errorf("the first DESIGN's critic packet lists released plans:\n%s", first)
	}
}

// TestArchitectPacketNamesTheHeadOfTheBackgroundFindings: the background explorers and the architect read one design head the run
// sampled before the phase explore, and the architect's packet names it with the findings, so the DESIGN's basis can be checked.
// A background explorer's packet names it too. Without a head the packets are as they were.
func TestArchitectPacketNamesTheHeadOfTheBackgroundFindings(t *testing.T) {
	bg := "[{ id: 'bg-n1', question: 'background of n1', answer: 'a.go holds it', findings: null }]"
	at := packetText(t, "architect-head-packet", "architectPacket", "architectPacket(ms, "+bg+", 'head0')")
	if !strings.Contains(at, "BACKGROUND FINDINGS (read at head0, the commit your directory is a checkout of;") {
		t.Errorf("the architect packet does not name the head of its background findings:\n%s", at)
	}
	without := packetText(t, "architect-nohead-packet", "architectPacket", "architectPacket(ms, "+bg+")")
	if strings.Contains(without, "read at") {
		t.Errorf("the architect packet without a head names one:\n%s", without)
	}
	exp := packetText(t, "explorer-head-packet", "explorerPacket", "explorerPacket('background', { question: 'background of n1', files_needed: ['a.go'] }, { milestone: 'M1', head: 'head0' })")
	if !strings.Contains(exp, "READ AT: head0") {
		t.Errorf("the background explorer packet does not name its head:\n%s", exp)
	}
}

// architectBackground is a background of two nodes as crw.explore returns it: bg-n1 answered, bg-n2 with no answer.
const architectBackground = "[{ id: 'bg-n1', question: 'background of n1', answer: 'a.go holds it', findings: { answer: 'a.go holds it', evidence: ['a.go'] } }, " +
	"{ id: 'bg-n2', question: 'background of n2', answer: '', findings: null }]"

// TestArchitectPacketWithoutAMissingExplorationIsUnchanged (CRW-1571): with no missing exploration the architect's packet has the
// bytes it had before CRW-1571.
func TestArchitectPacketWithoutAMissingExplorationIsUnchanged(t *testing.T) {
	got := packetText(t, "architect-golden-packet", "architectPacket", "architectPacket(ms, "+architectBackground+", 'head0')")
	want := "ROLE: architect. Plan milestone M1 of plan P (the milestone).\n\n" +
		"Answer with DESIGN: the decisions, one plan for each issue node below (its size_fit, the sub_tasks of a split node, edit regions, approach, tests first and done condition), the open questions you cannot answer from what you read, and the risks.\n\n" +
		"NODES:\nn1 (CRW-1): first\n  regions: a.go\n  criteria: none\nn2 (CRW-2): second after n1\n  regions: b.go\n  criteria: none\nn3 (CRW-3): third after n1\n  regions: c.go\n  criteria: none\n\n" +
		"BACKGROUND FINDINGS (read at head0, the commit your directory is a checkout of; an explorer read the repository for each node before you; build on them, and ask as an open question only what they and your own reading leave open):\n" +
		"- bg-n1: background of n1\n  answer: a.go holds it\n  findings: {\"answer\":\"a.go holds it\",\"evidence\":[\"a.go\"]}\n- bg-n2: background of n2"
	if got != want {
		t.Errorf("architect packet\n%q\nwant\n%q", got, want)
	}
}

// TestArchitectPacketNamesAMissingExploration (CRW-1571): a background answer whose explorer was asked twice and did not answer
// (crw.explore's missing member) is not listed among the BACKGROUND FINDINGS; a MISSING EXPLORATIONS section after them names it
// with both calls and the status and reason of the last.
func TestArchitectPacketNamesAMissingExploration(t *testing.T) {
	bg := "[{ id: 'bg-n1', question: 'background of n1', answer: 'a.go holds it', findings: null }, " +
		"{ id: 'bg-n2', question: 'background of n2', answer: '', call: '', findings: null, missing: { calls: ['c_1', 'c_2'], status: 'null_model', reason: 'schema_invalid_after_repair' } }]"
	got := packetText(t, "architect-missing-packet", "architectPacket", "architectPacket(ms, "+bg+", 'head0')")
	findings, missing, ok := strings.Cut(got, "\n\nMISSING EXPLORATIONS")
	if !ok {
		t.Fatalf("no MISSING EXPLORATIONS section:\n%s", got)
	}
	if !strings.HasSuffix(findings, "\n- bg-n1: background of n1\n  answer: a.go holds it") {
		t.Errorf("BACKGROUND FINDINGS should end with bg-n1 alone:\n%s", findings)
	}
	want := " (the explorer of each was asked twice and did not answer; read what the node rests on yourself, and ask as an open question what you cannot settle):\n" +
		"- bg-n2: background of n2 (calls c_1, c_2: null_model schema_invalid_after_repair)"
	if missing != want {
		t.Errorf("missing section\n%q\nwant\n%q", missing, want)
	}
}

// openAnswers are the answers of a round's open questions as crw.explore returns them with nothing missing: one an explorer answered,
// one asked of the orchestrator.
const openAnswers = "{ id: 'found', question: 'is the flag documented?', route: 'spawn', reason: '', authority: '', answer: 'yes, README.md says so', call: 'c_9', findings: { answer: 'yes, README.md says so' } }, " +
	"{ id: 'policy', question: 'does the project ship the flag in 1.0?', route: 'orchestrator', reason: 'out_of_authority', authority: 'authority:policy', answer: '', call: '', findings: null }"

// lostAnswer is an open question whose explorer was asked twice and did not answer (crw.explore's missing member, CRW-1571).
const lostAnswer = "{ id: 'lost', question: 'where is the retry policy kept?', route: 'spawn', reason: '', authority: '', answer: '', call: '', findings: null, missing: { calls: ['c_1', 'c_2'], status: 'null_model', reason: 'schema_invalid_after_repair' } }"

// TestOpenQuestionPacketsWithoutAMissingExplorationAreUnchanged (CRW-1571): with no missing exploration the critic's answers
// (answerLines) and the design-answers packet have the bytes they had before CRW-1571.
func TestOpenQuestionPacketsWithoutAMissingExplorationAreUnchanged(t *testing.T) {
	cand := "{ design: { decisions: [] }, answers: [" + openAnswers + "] }"
	audit := packetText(t, "critic-answers-golden-packet", "designAuditPacket", "designAuditPacket(ms, "+cand+", 1, [], undefined)")
	_, got, ok := strings.Cut(audit, "\n\nANSWERS TO THE OPEN QUESTIONS")
	if !ok {
		t.Fatalf("no answers section:\n%s", audit)
	}
	want := " (each with its route and what it got):\n" +
		"- found: is the flag documented?\n  route: an explorer answered it (call c_9)\n  answer: yes, README.md says so\n  findings: {\"answer\":\"yes, README.md says so\"}\n" +
		"- policy: does the project ship the flag in 1.0?\n  route: asked of the orchestrator (out_of_authority, authority:policy): no answer in this audit; the run raised it in the design_question escalation of this round, the milestone goes on and the plan stands on the assumption the DESIGN states\n\n" +
		"An open question of the DESIGN with no entry in ANSWERS TO THE OPEN QUESTIONS (one the architect raised when the answers came back to its thread, which no route has taken yet) is a P1 finding that names the question, so the next round routes it."
	if got != want {
		t.Errorf("answers section\n%q\nwant\n%q", got, want)
	}
	answers := packetText(t, "design-answers-golden-packet", "designAnswersPacket", "designAnswersPacket(["+openAnswers+"], undefined)")
	wantAnswers := "ROLE: architect. The open questions of your DESIGN got the answers below. Answer with the DESIGN again, with what they say folded into its decisions and plans, before the critic audits it; keep the ids of the decisions and the issues, and give dispositions null (no audit finding is before you). An open question an answer settles leaves open_questions; a question you raise anew in this answer reaches the critic with no answer, which is a P1 finding, and is routed in the next audit round.\n\n" +
		"ANSWERS:\n- found: is the flag documented?\n  answer: yes, README.md says so\n  findings: {\"answer\":\"yes, README.md says so\"}"
	if answers != wantAnswers {
		t.Errorf("design-answers packet\n%q\nwant\n%q", answers, wantAnswers)
	}
	replan := packetText(t, "design-answers-replan-golden-packet", "designAnswersPacket", "designAnswersPacket(["+openAnswers+"], { head: 'tip1', base_tip: 'base0', landed: [] })")
	if strings.Contains(replan, "NOT ANSWERED") || !strings.HasPrefix(replan, wantAnswers+"\n\nYour directory P is a read-only checkout") {
		t.Errorf("design-answers packet of a re-plan:\n%s", replan)
	}
}

// TestDesignAnswersPacketNamesTheQuestionsNotAnswered (CRW-1571): a question whose explorer was asked twice and did not answer is
// not among the ANSWERS; a NOT ANSWERED section right after them names it with both calls and the status and reason of the last.
func TestDesignAnswersPacketNamesTheQuestionsNotAnswered(t *testing.T) {
	got := packetText(t, "design-answers-missing-packet", "designAnswersPacket", "designAnswersPacket(["+openAnswers+", "+lostAnswer+"], { head: 'tip1', base_tip: 'base0', landed: [] })")
	listed, rest, ok := strings.Cut(got, "\n\nNOT ANSWERED")
	if !ok {
		t.Fatalf("no NOT ANSWERED section:\n%s", got)
	}
	if !strings.HasSuffix(listed, "ANSWERS:\n- found: is the flag documented?\n  answer: yes, README.md says so\n  findings: {\"answer\":\"yes, README.md says so\"}") {
		t.Errorf("ANSWERS should list the answered question alone:\n%s", listed)
	}
	want := " (the explorer of each was asked twice and did not answer; the question stays open):\n" +
		"- lost: where is the retry policy kept? (calls c_1, c_2: null_model schema_invalid_after_repair)\n\nYour directory P is a read-only checkout"
	if !strings.HasPrefix(rest, want) {
		t.Errorf("NOT ANSWERED section\n%q\nwant the prefix\n%q", rest, want)
	}
}

// TestOpenQuestionMissingWithNoAnswerGoesToTheCritic (CRW-1571): when no question of the round got an answer and one is missing,
// the round has no design-answers turn (answeredQuestions is empty, project-lane.js unchanged) and the critic's packet names the
// missing one: its route line says its explorer was asked twice and did not answer, never that an explorer answered it, and the
// other questions' lines are as before. (In a run the round then halts unless an explorer it spawned completed - D1.4 - so the
// line reaches the critic beside an answered question; TestOpenQuestionExplorerThatFailsTwiceIsNotAnswered runs that.)
func TestOpenQuestionMissingWithNoAnswerGoesToTheCritic(t *testing.T) {
	policy := "{ id: 'policy', question: 'does the project ship the flag in 1.0?', route: 'orchestrator', reason: 'out_of_authority', authority: 'authority:policy', answer: '', call: '', findings: null }"
	answers := "[" + lostAnswer + ", " + policy + "]"
	got := packetText(t, "critic-missing-packet", "answeredQuestions, designAuditPacket",
		"String(answeredQuestions("+answers+").length) + '|' + designAuditPacket(ms, { design: { decisions: [] }, answers: "+answers+" }, 1, [], undefined)")
	count, audit, _ := strings.Cut(got, "|")
	if count != "0" {
		t.Errorf("%s answered questions, want none (no design-answers turn)", count)
	}
	_, section, ok := strings.Cut(audit, "ANSWERS TO THE OPEN QUESTIONS (each with its route and what it got):\n")
	if !ok {
		t.Fatalf("no answers section:\n%s", audit)
	}
	want := "- lost: where is the retry policy kept?\n  route: an explorer was asked twice and did not answer (calls c_1, c_2: null_model schema_invalid_after_repair)\n" +
		"- policy: does the project ship the flag in 1.0?\n  route: asked of the orchestrator (out_of_authority, authority:policy): no answer"
	if !strings.HasPrefix(section, want) {
		t.Errorf("answers section\n%q\nwant the prefix\n%q", section, want)
	}
	if strings.Contains(section, "an explorer answered it") {
		t.Errorf("the missing question reads as answered:\n%s", section)
	}
	// a repeat in the round of the missing question (its reuse carries the missing member too) reads the same
	repeat := "{ id: 'again', question: 'where is the retry policy kept?', route: 'reuse', reason: 'same_question_in_round', authority: '', origin: 'spawn', answer: '', call: '', findings: null, missing: { calls: ['c_1', 'c_2'], status: 'null_model', reason: 'schema_invalid_after_repair' } }"
	reused := packetText(t, "critic-missing-reuse-packet", "designAuditPacket", "designAuditPacket(ms, { design: { decisions: [] }, answers: ["+lostAnswer+", "+repeat+"] }, 1, [], undefined)")
	if !strings.Contains(reused, "- again: where is the retry policy kept?\n  route: an explorer was asked twice and did not answer (calls c_1, c_2: null_model schema_invalid_after_repair)") {
		t.Errorf("the repeat of the missing question:\n%s", reused)
	}
}

// TestArchitectPacketAsksForTheSubTasksOfASplit (S-1, CRW-1435): the architect's first packet asks, with each issue's size_fit, for
// the sub_tasks of a split node, and the size fit section of the architect overlay names every size_fit value and every member of a
// sub-task the DESIGN schema has, and no other size: a value or a member the schema gains or drops moves the overlay with it.
func TestArchitectPacketAsksForTheSubTasksOfASplit(t *testing.T) {
	at := packetText(t, "architect-split-packet", "architectPacket", "architectPacket(ms, [], 'head0')")
	if !strings.Contains(at, "(its size_fit, the sub_tasks of a split node, edit regions, approach, tests first and done condition)") {
		t.Errorf("the architect packet does not ask for the sub_tasks of a split node:\n%s", at)
	}
	text, ok := ops.SchemaText(ops.SchemaDesign)
	if !ok {
		t.Fatal("no DESIGN schema")
	}
	var schema struct {
		Properties struct {
			Issues struct {
				Items struct {
					Properties struct {
						SizeFit struct {
							Enum []string `json:"enum"`
						} `json:"size_fit"`
						SubTasks struct {
							Items struct {
								Required []string `json:"required"`
							} `json:"items"`
						} `json:"sub_tasks"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"issues"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(text), &schema); err != nil {
		t.Fatal(err)
	}
	fits := schema.Properties.Issues.Items.Properties.SizeFit.Enum
	members := schema.Properties.Issues.Items.Properties.SubTasks.Items.Required
	if len(fits) == 0 || len(members) == 0 {
		t.Fatalf("the DESIGN schema gives no size_fit values (%v) or no sub-task members (%v)", fits, members)
	}
	overlay, err := roles.Overlay("architect", roles.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, size, ok := strings.Cut(overlay, "\n## Size fit\n")
	if !ok {
		t.Fatal("the architect overlay has no \"## Size fit\" section")
	}
	size, _, _ = strings.Cut(size, "\n## ")
	for _, word := range slices.Concat(fits, members) {
		if !strings.Contains(size, "`"+word+"`") {
			t.Errorf("the size fit section of the architect overlay does not name %q of the DESIGN schema", word)
		}
	}
	// A size the overlay offers is one of the schema's: each "- `value`:" item of its list is a size_fit value.
	for _, line := range strings.Split(size, "\n") {
		if v, ok := strings.CutPrefix(line, "- `"); ok {
			v, _, _ = strings.Cut(v, "`")
			if !slices.Contains(fits, v) {
				t.Errorf("the size fit section offers %q, which the DESIGN schema does not take (%v)", v, fits)
			}
		}
	}
}

// packetPlan is the plan of an issue node as crw.issuePlan gives it to the executor's packet.
const packetPlan = "{ issue: 'CRW-1', title: 'first', revision: 'r0', design_digest: 'sha256:d0', approach: 'guard the store', regions: ['a.go'], " +
	"tests_first: ['TestGuard'], done_condition: 'go test ./a', criteria: ['the guard holds'], decisions: [{ id: 'D1', change: 'split the store' }], answers: [] }"

// TestPacketWithoutLastStateIsUnchanged (CRW-1469): the executor's packet of a node with no last state (the first build, or a fresh
// thread of a node that has had no unit check, verification or receipt yet) is the packet it was, byte for byte: no last argument, an
// undefined one and the empty state of the node lane give the same text, with and without a checkpoint, and none of the LAST
// sections is rendered. A receipt with no summary is no last receipt.
func TestPacketWithoutLastStateIsUnchanged(t *testing.T) {
	const body = "ROLE: executor. Node n1 (CRW-1: first).\n\n" +
		"DESIGN REVISION: r0 sha256:d0\n\n" +
		"APPROACH:\nguard the store\n\n" +
		"EDIT REGIONS:\n- a.go\n\n" +
		"TESTS FIRST:\n- TestGuard\n\n" +
		"DONE CONDITION:\ngo test ./a\n\n" +
		"CRITERIA:\n- c1: the guard holds\n\n" +
		"DECISIONS:\n- D1: split the store"
	const checkpoint = "\n\nCHECKPOINT (a fresh tier takes over; the worktree starts at this head):\n- head: h1\n- tree: t1"
	const tail = "\n\nAnswer with RECEIPT when you are done. Edit files only under the absolute path P of your worktree."
	for _, row := range []struct{ name, cp, last, want string }{
		{"no-last", "null", "", body + tail},
		{"undefined-last", "null", ", undefined", body + tail},
		{"empty-last", "null", ", { findings: [], ci: null, receipt: null }", body + tail},
		{"receipt-without-summary", "null", ", { findings: [], ci: null, receipt: { summary: null } }", body + tail},
		{"checkpoint-no-last", "{ head: 'h1', tree: 't1' }", "", body + checkpoint + tail},
		{"checkpoint-empty-last", "{ head: 'h1', tree: 't1' }", ", { findings: [], ci: null, receipt: null }", body + checkpoint + tail},
		// a plan with no sub-tasks has this packet whatever progress the lane passes (CRW-1459)
		{"split-progress-no-sub-tasks", "null", ", { findings: [], ci: null, receipt: null, split: { done: 0, failed: {} } }", body + tail},
	} {
		got := packetText(t, "executor-packet-"+row.name, "packet", "packet('n1', "+packetPlan+", "+row.cp+row.last+")")
		if got != row.want {
			t.Errorf("%s: the packet\n%q\nwant\n%q", row.name, got, row.want)
		}
	}
}

// TestPacketRendersTheLastState (CRW-1469): a fresh thread's packet carries the node's last state: the findings of the last unit check
// as the fix packet names them, the last verification with its command and failures (or that it passed), and the summary of the last
// receipt, each in its own section, before the checkpoint.
func TestPacketRendersTheLastState(t *testing.T) {
	last := "{ findings: [{ id: 'f_1', severity: 'P2', impact: 'core_function', file: 'a.go', line: 3, text: 'the guard is missing' }], " +
		"ci: { pass: false, command: 'crw-dev ci local', failures: ['validate/tests: failed'], summary: 'validate/tests: failed' }, receipt: { summary: 'wrote the guard' } }"
	got := packetText(t, "executor-packet-last", "packet", "packet('n1', "+packetPlan+", { head: 'h1', tree: 't1' }, "+last+")")
	for _, want := range []string{
		"\n\nLAST FINDINGS (",
		"\n- [P2] f_1 a.go:3 the guard is missing",
		"\n\nLAST VERIFICATION FAILED (crw-dev ci local):\n- validate/tests: failed",
		"\n\nLAST RECEIPT (",
		"): wrote the guard",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the packet lacks %q:\n%s", want, got)
		}
	}
	if i, j := strings.Index(got, "LAST FINDINGS"), strings.Index(got, "CHECKPOINT"); i < 0 || j < 0 || i > j {
		t.Errorf("the last state does not come before the checkpoint:\n%s", got)
	}
	passed := packetText(t, "executor-packet-last-pass", "packet", "packet('n1', "+packetPlan+", null, { findings: [], ci: { pass: true, command: 'crw-dev ci local', failures: [], summary: 'pass' }, receipt: null })")
	if !strings.Contains(passed, "\n\nLAST VERIFICATION PASSED (crw-dev ci local)") || strings.Contains(passed, "LAST FINDINGS") || strings.Contains(passed, "LAST RECEIPT") {
		t.Errorf("the packet of a passed last verification:\n%s", passed)
	}
}

// TestFixPacketTextIsKept (CRW-1469): the fix packet builds its unit-check, verification and receipt sections from the helpers the
// executor's packet shares, with explicit scope and factual-research instructions (CRW-1637).
func TestFixPacketTextIsKept(t *testing.T) {
	uc := packetText(t, "fix-packet-uc", "fixPacket", "fixPacket('A', { verdict: 'needs_changes', findings: [{ id: 'f_1', severity: 'P2', file: 'a/x.go', line: 7, text: 'the loop never ends' }] }, null, null, { summary: 'wrote the loop' })")
	if want := "ROLE: executor. Node A: fix what the run found, in the same worktree, and answer with RECEIPT again.\n\n" +
		"UNIT CHECK (needs_changes; fix every finding):\n- [P2] f_1 a/x.go:7 the loop never ends\n\n" +
		"If a fix requires a file outside your assigned edit regions, answer blocked_scope with the required files and why they are needed in gap. An explorer cannot grant editing permission.\n\n" +
		"Use needs_research only for a factual question that can be answered by reading the repository; put that question in research_question.\n\n" +
		"YOUR LAST RECEIPT: wrote the loop"; uc != want {
		t.Errorf("the unit-check fix packet\n%q\nwant\n%q", uc, want)
	}
	ci := packetText(t, "fix-packet-ci", "fixPacket", "fixPacket('A', null, { pass: false, command: 'crw-dev ci local', failures: [], summary: 'validate/tests: failed' }, null, { summary: null })")
	if want := "ROLE: executor. Node A: fix what the run found, in the same worktree, and answer with RECEIPT again.\n\n" +
		"VERIFICATION FAILED (crw-dev ci local):\n- validate/tests: failed\n\n" +
		"If a fix requires a file outside your assigned edit regions, answer blocked_scope with the required files and why they are needed in gap. An explorer cannot grant editing permission.\n\n" +
		"Use needs_research only for a factual question that can be answered by reading the repository; put that question in research_question."; ci != want {
		t.Errorf("the verification fix packet\n%q\nwant\n%q", ci, want)
	}
}

func TestResearchPacketCarriesFixContext(t *testing.T) {
	packet := packetText(t, "research-context", "explorerPacket", `explorerPacket('research',
{question: 'Which context starts this new git query?', files_needed: null},
{node: 'A', regions: ['internal/allowed/'], findings: [{id: 'f_scope', severity: 'P2', file: 'internal/other.go', line: 17, text: 'the new git query starts from context.Background()'}],
source: {call: 'executor-question', head: 'snapshot-head', tree: 'pending-tree'}})`)
	for _, want := range []string{"f_scope", "internal/other.go:17", "the new git query starts from context.Background()", "internal/allowed/", "READ AT: snapshot-head", "pending-tree", "executor-question", "read-only", "uncommitted changes", "cannot authorize editing"} {
		if !strings.Contains(packet, want) {
			t.Errorf("research packet lacks %q: %s", want, packet)
		}
	}
	background := packetText(t, "background-context", "explorerPacket", `explorerPacket('background', {question:'background'}, {milestone:'M', head:'base', regions:['internal/allowed/']})`)
	if !strings.Contains(background, "READ AT: base") || strings.Contains(background, "NODE EDIT REGIONS") || strings.Contains(background, "FINDINGS FROM THE FIX ROUND") {
		t.Errorf("background packet changed: %s", background)
	}
	answer := packetText(t, "research-answer", "answerPacket", "answerPacket([])")
	if !strings.Contains(answer, "Research answers cannot widen your edit regions") || !strings.Contains(answer, "blocked_scope") {
		t.Errorf("answer grants no scope reminder: %s", answer)
	}
	overlay, err := roles.Prompt("executor", roles.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"blocked_scope", "required files", "factual question", "`gap`"} {
		if !strings.Contains(overlay, want) {
			t.Errorf("executor overlay lacks %q", want)
		}
	}
}

// TestFixPacketNamesTheFailedTestsOfTheVerification (CRW-1469, CRW-1488 item 16): a failed verification whose result names the failed
// tests of its record (failed_tests: by failed step, the tests, the packages and the last lines of its output) renders them in the fix
// packet after the failures, and in the LAST VERIFICATION of a fresh thread's packet; a step that names no test shows its output only.
// A failed verification with no failed_tests retains its verification section, followed by the scope guidance.
func TestFixPacketNamesTheFailedTestsOfTheVerification(t *testing.T) {
	const ci = "{ pass: false, command: 'crw-dev ci local', failures: ['go-product/tests: failed', 'validate/lint: failed'], summary: 'go-product/tests: failed; validate/lint: failed', " +
		"failed_tests: [{ job: 'go-product', step: 'tests', tests: ['TestGuard', 'TestStore/empty'], packages: ['example.com/m/store'], tail: '--- FAIL: TestGuard (0.01s)\\nFAIL\\texample.com/m/store\\t0.1s' }, " +
		"{ job: 'validate', step: 'lint', tests: [], packages: [], tail: 'a.go:3: undefined: x' }] }"
	const steps = "FAILED TESTS (each failed step of the verification record: its tests, its packages and the last lines of its output):\n" +
		"- go-product/tests: TestGuard, TestStore/empty (packages example.com/m/store)\n" +
		"    --- FAIL: TestGuard (0.01s)\n    FAIL\texample.com/m/store\t0.1s\n" +
		"- validate/lint: no test named\n" +
		"    a.go:3: undefined: x"
	fix := packetText(t, "fix-packet-failed-tests", "fixPacket", "fixPacket('A', null, "+ci+", null, null)")
	if want := "ROLE: executor. Node A: fix what the run found, in the same worktree, and answer with RECEIPT again.\n\n" +
		"VERIFICATION FAILED (crw-dev ci local):\n- go-product/tests: failed\n- validate/lint: failed\n\n" + steps + "\n\n" +
		"If a fix requires a file outside your assigned edit regions, answer blocked_scope with the required files and why they are needed in gap. An explorer cannot grant editing permission.\n\n" +
		"Use needs_research only for a factual question that can be answered by reading the repository; put that question in research_question."; fix != want {
		t.Errorf("the fix packet\n%q\nwant\n%q", fix, want)
	}
	fresh := packetText(t, "executor-packet-failed-tests", "packet", "packet('n1', "+packetPlan+", null, { findings: [], ci: "+ci+", receipt: null })")
	if !strings.Contains(fresh, "\n\nLAST VERIFICATION FAILED (crw-dev ci local):\n- go-product/tests: failed\n- validate/lint: failed\n\n"+steps+"\n\n") {
		t.Errorf("the fresh thread's packet lacks the failed tests:\n%s", fresh)
	}
	for _, none := range []string{"", ", failed_tests: []"} {
		old := packetText(t, "fix-packet-no-failed-tests", "fixPacket", "fixPacket('A', null, { pass: false, command: 'crw-dev ci local', failures: ['validate/tests: failed'], summary: 'validate/tests: failed'"+none+" }, null, null)")
		if want := "ROLE: executor. Node A: fix what the run found, in the same worktree, and answer with RECEIPT again.\n\n" +
			"VERIFICATION FAILED (crw-dev ci local):\n- validate/tests: failed\n\n" +
			"If a fix requires a file outside your assigned edit regions, answer blocked_scope with the required files and why they are needed in gap. An explorer cannot grant editing permission.\n\n" +
			"Use needs_research only for a factual question that can be answered by reading the repository; put that question in research_question."; old != want {
			t.Errorf("a verification without failed tests (%q): the fix packet\n%q\nwant\n%q", none, old, want)
		}
	}
}

// TestCrosscheckRuleIsOneTableForPacketOpAndClose (CRW-1473, CRW-1480 item 2): the crosscheck has one close rule, and the packet, the op
// and the close say the same one. Both passes' packets hold ops.CrosscheckRule word for word (and the lens of their own pass), and for
// each severity P0-P3 crossed with every impact and none, ops.CrosscheckVerdictOf rules needs_changes exactly when premerge.Blocks and
// carries the finding otherwise. The close part is TestCloseFollowsTheCrosscheckRule (ops/finalgate_test.go): a close over that one
// finding closes exactly when V is verified.
func TestCrosscheckRuleIsOneTableForPacketOpAndClose(t *testing.T) {
	const g = "{ milestone: 'M1', head: 'h9', base: 'b0', range: 'b0..h9', decisions: [{ id: 'D1', nodes: ['n1'], change: 'split the store' }] }"
	for pass, lens := range map[string]string{"A": "PASS A (coverage)", "B": "PASS B (bug hunt)"} {
		packet := packetText(t, "crosscheck-packet-"+pass, "crosscheckPacket", "crosscheckPacket(ms, "+g+", '"+pass+"', {})")
		for _, want := range []string{ops.CrosscheckRule, lens, "Review the changes between b0 and h9", "HEAD: h9", "D1 (nodes n1): split the store",
			"verdict of this pass and its own lens", "criteria and evidence before accepting the author's framing",
			"conflicting evidence or an unresolved assumption", "smallest check", "Never average verdicts or treat agreement as proof",
			"Do not invent another pass's conclusion", "no findings remains an empty list", "no extra passes or helper calls"} {
			if !strings.Contains(packet, want) {
				t.Errorf("the crosscheck packet of pass %s lacks %q:\n%s", pass, want, packet)
			}
		}
	}
	for _, sev := range []string{"P0", "P1", "P2", "P3"} {
		for _, impact := range append(slices.Clone(premerge.Impacts), "") {
			f := reviewadapter.Finding{ID: "f1", Severity: sev, Impact: impact, File: "a.go", Line: 1, Text: "t"}
			verdict, carried := ops.CrosscheckVerdictOf([]reviewadapter.Finding{f})
			blocks := premerge.Blocks(sev, impact)
			if (verdict == reviewadapter.VerdictNeedsChanges) != blocks || (len(carried) == 1) == blocks {
				t.Errorf("%s/%q: CrosscheckVerdictOf %s carried %v, premerge.Blocks %v", sev, impact, verdict, carried, blocks)
			}
		}
	}
}

// TestCrosscheckFixPacketNamesTheRouteOfTheFix (CRW-1473): the fix packet of a crosscheck that needs changes names the blocking
// findings by id (not the carried ones), the range as a git diff command over their files, the criteria, and the route of part A: a
// correction issue of the milestone, a new executor subagent on a branch from the integration tip, the land through land-head, and
// the report of the verdict up to the 부모.
func TestCrosscheckFixPacketNamesTheRouteOfTheFix(t *testing.T) {
	const v = "{ verdict: 'needs_changes', round: 1, head: 'h9', base: 'b0', carried: ['f2'], findings: [" +
		"{ id: 'f1', severity: 'P1', impact: 'core_function', file: 'a.go', line: 3, text: 'the store loses a row' }, " +
		"{ id: 'f2', severity: 'P3', impact: 'minor_separable', file: 'b.go', line: 1, text: 'a nit' }] }"
	packet := packetText(t, "crosscheck-fix-packet", "crosscheckFixPacket", "crosscheckFixPacket(ms, "+v+")")
	for _, want := range []string{"f1 [P1/core_function] a.go:3 the store loses a row", "git diff b0..h9 -- a.go", "correction issue of milestone M1 naming these finding ids: f1",
		"a new executor subagent", "crw flow start land-head", "crw flow report-up", "부모"} {
		if !strings.Contains(packet, want) {
			t.Errorf("the fix packet lacks %q:\n%s", want, packet)
		}
	}
	for _, gone := range []string{"f2", "b.go", "start crosscheck"} {
		if strings.Contains(packet, gone) {
			t.Errorf("the fix packet names %q:\n%s", gone, packet)
		}
	}
}

// TestLandFixPacketNamesTheFindingsRangeAndInRunRepair (CRW-1490): the fix packet of a land-head run whose unit check needs changes
// is the executor's turn in the run's repair: it names every finding of the check by its severity, id, file, line and text, the range
// of the land as a git diff command, the node's edit regions and criteria, and the receipt of the executor's last turn; and it says
// the run commits, verifies and checks again - the executor commits nothing and starts no land of its own.
func TestLandFixPacketNamesTheFindingsRangeAndInRunRepair(t *testing.T) {
	const n = "{ id: 'n1', issue: 'CRW-1', title: 'first', criteria: ['the store keeps every row'], regions: ['a.go'] }"
	const findings = "[{ id: 'f1', severity: 'high', file: 'a.go', line: 3, text: 'the store loses a row' }, { id: 'f2', severity: 'low', file: 'b.go', line: 1, text: 'a nit' }]"
	packet := packetText(t, "land-fix-packet", "landFixPacket", "landFixPacket("+n+", "+findings+", { commit: 'h9', base: 'b0' }, { summary: 'previous fix' })")
	for _, want := range []string{
		"LAND FIX of node n1 (CRW-1: first)", "at h9 needs changes",
		"- [high] f1 a.go:3 the store loses a row", "- [low] f2 b.go:1 a nit",
		"RANGE: git diff b0..h9",
		"EDIT REGIONS:\n- a.go",
		"CRITERIA OF THE NODE:\n- c1: the store keeps every row",
		"previous fix",
		"Do not commit or start another land run",
	} {
		if !strings.Contains(packet, want) {
			t.Errorf("the land fix packet lacks %q:\n%s", want, packet)
		}
	}
	for _, gone := range []string{"crw flow start land-head", "a new executor subagent", "the fixer subagent thread", "--handoff"} {
		if strings.Contains(packet, gone) {
			t.Errorf("the in-run fix packet names the old route %q:\n%s", gone, packet)
		}
	}
}

// splitPlan is the plan of a node sized for a split with two sub-tasks, as crw.issuePlan gives it (CRW-1459).
const splitPlan = "{ issue: 'CRW-1', node: 'n1', title: 'first', revision: 'r0', design_digest: 'sha256:d0', approach: 'guard the store', regions: ['a/'], " +
	"tests_first: ['TestGuard'], done_condition: 'go test ./a', criteria: ['the guard holds'], decisions: [{ id: 'D1', change: 'split the store' }], answers: [], size_fit: 'split', " +
	"sub_tasks: [{ id: 'store', summary: 'split the store', edit_regions: ['a/store.go'], done_condition: 'go test ./a -run Store', after: null }, " +
	"{ id: 'guard', summary: 'guard the store', edit_regions: ['a/guard.go', 'a/guard_test.go'], done_condition: 'go test ./a -run Guard', after: ['store'] }] }"

// TestPacketNamesTheSubTasksOfASplit (CRW-1459, S-3): the packet of a fresh executor thread of a split node lists its sub-tasks in
// order, each marked done or to do with its regions and done condition, and says to do the first one not done now, and only it; the
// sub-tasks that reported failed are named with their summary and gap. With every sub-task done it says to finish the node.
func TestPacketNamesTheSubTasksOfASplit(t *testing.T) {
	first := packetText(t, "executor-packet-split-first", "packet", "packet('n1', "+splitPlan+", null, { findings: [], ci: null, receipt: null, split: { done: 0, failed: {} } })")
	for _, want := range []string{
		"\n\nSUB-TASKS (this node is split: ",
		"\n- 1. store (to do): split the store\n  edit regions: a/store.go\n  done condition: go test ./a -run Store",
		"\n- 2. guard (to do): guard the store\n  edit regions: a/guard.go, a/guard_test.go\n  done condition: go test ./a -run Guard",
		"\n\nDo sub-task 1 (store) now, and only it: ",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("the first packet of a split lacks %q:\n%s", want, first)
		}
	}
	if strings.Contains(first, "REPORTED FAILED") {
		t.Errorf("the first packet names a failed sub-task:\n%s", first)
	}
	if noArg := packetText(t, "executor-packet-split-noarg", "packet", "packet('n1', "+splitPlan+", null)"); !strings.Contains(noArg, "Do sub-task 1 (store) now") {
		t.Errorf("a split packet with no progress does not start at the first sub-task")
	}
	again := packetText(t, "executor-packet-split-again", "packet", "packet('n1', "+splitPlan+", { head: 'h1', tree: 't1' }, { findings: [], ci: null, receipt: null, split: { done: 1, failed: { store: { summary: 'half of the store', gap: 'the index is missing' } } } })")
	for _, want := range []string{
		"\n- 1. store (done): split the store",
		"\n- 2. guard (to do): guard the store",
		"REPORTED FAILED (",
		"it is part of this node and is judged with it",
		"\n- 1. store: half of the store\n  gap: the index is missing",
		"\n\nDo sub-task 2 (guard) now, and only it: ",
	} {
		if !strings.Contains(again, want) {
			t.Errorf("the packet of a fresh thread at sub-task 2 lacks %q:\n%s", want, again)
		}
	}
	if i, j := strings.Index(again, "SUB-TASKS"), strings.Index(again, "CHECKPOINT"); i < 0 || j < 0 || i > j {
		t.Errorf("the sub-tasks do not come before the checkpoint:\n%s", again)
	}
	all := packetText(t, "executor-packet-split-all", "packet", "packet('n1', "+splitPlan+", { head: 'h1', tree: 't1' }, { findings: [], ci: null, receipt: null, split: { done: 2, failed: {} } })")
	if !strings.Contains(all, "- 2. guard (done)") || strings.Contains(all, "(to do)") || strings.Contains(all, "Do sub-task") || !strings.Contains(all, "Every sub-task is done and committed in your worktree") {
		t.Errorf("the packet with every sub-task done:\n%s", all)
	}
}

// TestSubTaskPacketIsTheNextSubTaskOnTheThread (CRW-1459, S-2): the next turn of a split node's thread names the sub-task by its place,
// id and summary, its regions and done condition, and the earlier sub-tasks that reported failed.
func TestSubTaskPacketIsTheNextSubTaskOnTheThread(t *testing.T) {
	got := packetText(t, "subtask-packet", "subTaskPacket", "subTaskPacket("+splitPlan+", 1, {})")
	want := "ROLE: executor. Node n1: sub-task 2 of 2 (guard): guard the store. The run committed the sub-tasks before it in your worktree; do this one now, and only it, in the same worktree, and answer with RECEIPT when it is done.\n\n" +
		"EDIT REGIONS:\n- a/guard.go\n- a/guard_test.go\n\n" +
		"DONE CONDITION:\ngo test ./a -run Guard"
	if got != want {
		t.Errorf("the sub-task packet\n%q\nwant\n%q", got, want)
	}
	failed := packetText(t, "subtask-packet-failed", "subTaskPacket", "subTaskPacket("+splitPlan+", 1, { store: { summary: 'half of the store', gap: 'the index is missing' } })")
	if !strings.HasPrefix(failed, want+"\n\nREPORTED FAILED (") || !strings.Contains(failed, "\n- 1. store: half of the store\n  gap: the index is missing") {
		t.Errorf("the sub-task packet after a failed sub-task:\n%s", failed)
	}
}

// TestAFailedSectionNamesOnlyTheSubTasksThatReportedFailed (CRW-1459): the failed receipts of a split node's sub-tasks are read by the
// sub-task's own id, never through the object's prototype: a sub-task whose id is a name every object inherits (constructor, a valid
// sub-task id) is not named as reported failed when no receipt of it failed, in the packet of a fresh thread and in the next sub-task's
// turn, and it is named when it did fail.
func TestAFailedSectionNamesOnlyTheSubTasksThatReportedFailed(t *testing.T) {
	plan := strings.ReplaceAll(splitPlan, "'store'", "'constructor'")
	fresh := packetText(t, "executor-packet-split-constructor", "packet", "packet('n1', "+plan+", { head: 'h1', tree: 't1' }, { findings: [], ci: null, receipt: null, split: { done: 1, failed: {} } })")
	next := packetText(t, "subtask-packet-constructor", "subTaskPacket", "subTaskPacket("+plan+", 1, {})")
	for name, text := range map[string]string{"the packet of a fresh thread": fresh, "the next sub-task's turn": next} {
		if strings.Contains(text, "REPORTED FAILED") || strings.Contains(text, "constructor: no summary") {
			t.Errorf("%s names a sub-task that did not fail:\n%s", name, text)
		}
	}
	failed := packetText(t, "subtask-packet-constructor-failed", "subTaskPacket", "subTaskPacket("+plan+", 1, { constructor: { summary: 'half of the store', gap: 'the index is missing' } })")
	if !strings.Contains(failed, "\n- 1. constructor: half of the store\n  gap: the index is missing") {
		t.Errorf("the turn after a failed sub-task constructor:\n%s", failed)
	}
}
