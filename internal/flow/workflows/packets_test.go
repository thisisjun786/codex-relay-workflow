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
	for _, row := range []struct{ kind, want string }{
		{"split", "- n2 (CRW-2), sized for a split under r0 with no sub-tasks (a legacy split row: no executor started; its lane escalated it as size_split)\n  plan: {"},
		{"child", "- n2 (CRW-2), sized for a child under r0 (no executor started; its lane escalated it as size_split)\n  plan: {"},
		{"bundle_split", "- n2 (CRW-2), sized for a bundle split under r0 (no executor started; the parent was asked: bundle_split_request)\n  plan: {"},
		{"released", "- n2 (CRW-2), released under r0\n  plan: {"},
	} {
		sized := packetText(t, "critic-released-"+row.kind+"-packet", "designAuditPacket",
			"designAuditPacket(ms, "+cand+", 1, [], { head: 'tip1', base_tip: 'base0', landed: [], unstarted: [], released: [{ node: 'n2', kind: '"+row.kind+"', revision: 'r0', design_digest: 'dg0', plan: { issue: 'CRW-2', size_fit: 'split' }, decisions: [] }] })")
		if !strings.Contains(sized, row.want) {
			t.Errorf("a released entry of kind %s:\n%s", row.kind, sized)
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

// TestFixPacketNamesTheFailedTestsOfTheVerification (CRW-1469, CRW-1488 item 16): a failed verification whose result names the failed
// tests of its record (failed_tests: by failed step, the tests, the packages and the last lines of its output) renders them in the fix
// packet after the failures, and in the LAST VERIFICATION of a fresh thread's packet; a step that names no test shows its output only.
// A failed verification with no failed_tests, as every one recorded before them, renders the text it did.
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
		"VERIFICATION FAILED (crw-dev ci local):\n- go-product/tests: failed\n- validate/lint: failed\n\n" + steps; fix != want {
		t.Errorf("the fix packet\n%q\nwant\n%q", fix, want)
	}
	fresh := packetText(t, "executor-packet-failed-tests", "packet", "packet('n1', "+packetPlan+", null, { findings: [], ci: "+ci+", receipt: null })")
	if !strings.Contains(fresh, "\n\nLAST VERIFICATION FAILED (crw-dev ci local):\n- go-product/tests: failed\n- validate/lint: failed\n\n"+steps+"\n\n") {
		t.Errorf("the fresh thread's packet lacks the failed tests:\n%s", fresh)
	}
	for _, none := range []string{"", ", failed_tests: []"} {
		old := packetText(t, "fix-packet-no-failed-tests", "fixPacket", "fixPacket('A', null, { pass: false, command: 'crw-dev ci local', failures: ['validate/tests: failed'], summary: 'validate/tests: failed'"+none+" }, null, null)")
		if want := "ROLE: executor. Node A: fix what the run found, in the same worktree, and answer with RECEIPT again.\n\n" +
			"VERIFICATION FAILED (crw-dev ci local):\n- validate/tests: failed"; old != want {
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
		for _, want := range []string{ops.CrosscheckRule, lens, "Review the changes between b0 and h9", "HEAD: h9", "D1 (nodes n1): split the store"} {
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

// TestLandFixPacketNamesTheFindingsTheRangeAndTheNextRound (CRW-1490): the fix packet of a land-head run whose unit check needs changes
// names every finding of the check by its severity, id, file, line and text, the range of the land as a git diff command, the
// criteria of the node, and the route of the fix: a new executor subagent fixes the head, and a new land-head run of the node lands it
// with the next fix round, its args written out with the authors of the head, placeholders for the fixed head and the fixer's thread,
// and the handoff a start of land-head needs.
func TestLandFixPacketNamesTheFindingsTheRangeAndTheNextRound(t *testing.T) {
	const n = "{ id: 'n1', issue: 'CRW-1', title: 'first', criteria: ['the store keeps every row'] }"
	const findings = "[{ id: 'f1', severity: 'high', file: 'a.go', line: 3, text: 'the store loses a row' }, { id: 'f2', severity: 'low', file: 'b.go', line: 1, text: 'a nit' }]"
	packet := packetText(t, "land-fix-packet", "landFixPacket", "landFixPacket("+n+", "+findings+", { commit: 'h9', base: 'b0' }, { plan: 'P', milestone: 'M1', fix_round: 2, authors: ['t-author'] })")
	for _, want := range []string{
		"LAND FIX of node n1 (CRW-1: first)", "at h9 needs changes",
		"- [high] f1 a.go:3 the store loses a row", "- [low] f2 b.go:1 a nit",
		"RANGE: git diff b0..h9",
		"CRITERIA OF THE NODE:\n- c1: the store keeps every row",
		"a new executor subagent fixes this head",
		"`crw flow start land-head --args '" + `{"plan":"P","milestone":"M1","issue":"CRW-1","head":"<the fixed head>","authors":["t-author","<the fixer subagent thread>"],"fix_round":2}` +
			"' --handoff <the plan's handoff file>`",
	} {
		if !strings.Contains(packet, want) {
			t.Errorf("the land fix packet lacks %q:\n%s", want, packet)
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
