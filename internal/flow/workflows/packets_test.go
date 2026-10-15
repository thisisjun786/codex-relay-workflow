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
