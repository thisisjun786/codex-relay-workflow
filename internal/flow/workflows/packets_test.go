package workflows

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
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
	// a node sized for a split started too: it is listed with how it started
	split := packetText(t, "critic-released-split-packet", "designAuditPacket",
		"designAuditPacket(ms, "+cand+", 1, [], { head: 'tip1', base_tip: 'base0', landed: [], unstarted: [], released: [{ node: 'n2', kind: 'split', revision: 'r0', design_digest: 'dg0', plan: { issue: 'CRW-2', size_fit: 'split' }, decisions: [] }] })")
	if !strings.Contains(split, "- n2 (CRW-2), sized for a split under r0 (no executor started; its lane escalated it as size_split)\n  plan: {") {
		t.Errorf("a node sized for a split:\n%s", split)
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
// executor's packet shares, and its text is the one it was, so a resumed fix call has the input digest it was recorded with.
func TestFixPacketTextIsKept(t *testing.T) {
	uc := packetText(t, "fix-packet-uc", "fixPacket", "fixPacket('A', { verdict: 'needs_changes', findings: [{ id: 'f_1', severity: 'P2', file: 'a/x.go', line: 7, text: 'the loop never ends' }] }, null, null, { summary: 'wrote the loop' })")
	if want := "ROLE: executor. Node A: fix what the run found, in the same worktree, and answer with RECEIPT again.\n\n" +
		"UNIT CHECK (needs_changes; fix every finding):\n- [P2] f_1 a/x.go:7 the loop never ends\n\n" +
		"A finding you cannot fix without an answer: answer needs_research with the question, and the run answers it on this thread.\n\n" +
		"YOUR LAST RECEIPT: wrote the loop"; uc != want {
		t.Errorf("the unit-check fix packet\n%q\nwant\n%q", uc, want)
	}
	ci := packetText(t, "fix-packet-ci", "fixPacket", "fixPacket('A', null, { pass: false, command: 'crw-dev ci local', failures: [], summary: 'validate/tests: failed' }, null, { summary: null })")
	if want := "ROLE: executor. Node A: fix what the run found, in the same worktree, and answer with RECEIPT again.\n\n" +
		"VERIFICATION FAILED (crw-dev ci local):\n- validate/tests: failed"; ci != want {
		t.Errorf("the verification fix packet\n%q\nwant\n%q", ci, want)
	}
}
