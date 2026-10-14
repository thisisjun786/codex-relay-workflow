package registry

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The management -> project edge (CRW-1314 lane D1c, design §1-2): the 1.0 path has two session
// layers, management and project owner, so a management session binding a project owner records
// the execution edge from its own seat to the project in the binding's transaction, the way a lead
// registration records the edge to the initiative (CRW-1333).

// bindProjectOwner is one linkage-bind of a project owner (role parent at the project) with
// --creator, or without it when creator is "".
func (s *storeSeat) bindProjectOwner(project, owner, creator string) (int, string) {
	s.t.Helper()
	argv := []string{"linkage-bind", "--role", "parent", "--scope", project, "--task", owner, "--host", "host"}
	if creator != "" {
		argv = append(argv, "--creator", creator)
	}
	return s.run(argv...)
}

// mustBindProjectOwner is bindProjectOwner with a successful answer decoded.
func (s *storeSeat) mustBindProjectOwner(project, owner, creator string) map[string]any {
	s.t.Helper()
	code, out := s.bindProjectOwner(project, owner, creator)
	if code != 0 {
		s.t.Fatalf("bind project owner %s of %s (creator %q): exit %d stdout %q", owner, project, creator, code, out)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(out), &answer); err != nil {
		s.t.Fatalf("binding answer is not JSON: %v (%q)", err, out)
	}
	return answer
}

// projectEdges are the live execution edges above project.
func (s *storeSeat) projectEdges(project string) []store.Row {
	s.t.Helper()
	return s.rows("SELECT link_id, upper_kind, upper_key, upper_task_id, lower_task_id, revision FROM scope_links WHERE lower_kind = 'project' AND lower_key = ?"+
		" AND link_kind = 'execution' AND status IN ('active','paused') AND superseded_by IS NULL", project)
}

// TestProjectOwnerBindingWritesTheManagementEdge: a management session binding a project owner
// writes the execution edge management -> project in the binding's transaction, answered under
// managementLink; a replay converges; --creator defaults to CODEX_THREAD_ID; an owner registering
// itself (no management seat) writes no edge; and the walk up from the project reaches the seat.
func TestProjectOwnerBindingWritesTheManagementEdge(t *testing.T) {
	s := newStoreSeat(t)
	t.Setenv("CODEX_THREAD_ID", "")
	s.mustBindManagement("01mgmtA", "01mgmtA")
	answer := s.mustBindProjectOwner("P-A", "01ownerA", "01mgmtA")
	link, _ := answer["managementLink"].(map[string]any)
	if answer["taskId"] != "01ownerA" || answer["scopeKind"] != "project" || answer["role"] != "parent" || link == nil {
		t.Fatalf("project owner binding answered %v, want the owner's binding and its management link", answer)
	}
	edges := s.projectEdges("P-A")
	if len(edges) != 1 || edges[0].Text("upper_kind") != managementKind || edges[0].Text("upper_key") != "01mgmtA" ||
		edges[0].Text("upper_task_id") != "01mgmtA" || edges[0].Text("lower_task_id") != "01ownerA" || edges[0].Text("link_id") != link["linkId"] {
		t.Fatalf("edges above P-A %v, want one from 01mgmtA's seat to 01ownerA", edges)
	}
	if _, skipped := answer["managementLinkSkipped"]; skipped {
		t.Fatalf("a clean binding carries a skip warning: %v", answer)
	}
	// The same binding again converges: one edge, no second revision.
	if code, out := s.bindProjectOwner("P-A", "01ownerA", "01mgmtA"); code != 0 {
		t.Fatalf("replay: exit %d %q", code, out)
	}
	if again := s.projectEdges("P-A"); len(again) != 1 || again[0].Text("revision") != edges[0].Text("revision") {
		t.Fatalf("a replayed binding moved the edge: %v", again)
	}
	// The walk up from the project names the management level directly above it.
	code, out := s.run("linkage-up", "--task", "01ownerA", "--scope", "P-A")
	if code != 0 {
		t.Fatalf("linkage-up: exit %d %q", code, out)
	}
	var up map[string]any
	if err := json.Unmarshal([]byte(out), &up); err != nil {
		t.Fatal(err)
	}
	levels, _ := up["levels"].([]any)
	if len(levels) != 2 {
		t.Fatalf("linkage-up levels %v, want the project and the management seat", up)
	}
	top, _ := levels[1].(map[string]any)
	owner, _ := top["owner"].(map[string]any)
	if top["scopeKind"] != managementKind || top["scopeKey"] != "01mgmtA" || owner["taskId"] != "01mgmtA" {
		t.Fatalf("the level above P-A is %v", top)
	}

	// The default creator is the caller's own thread.
	t.Setenv("CODEX_THREAD_ID", "01mgmtA")
	s.mustBindProjectOwner("P-A2", "01ownerA2", "")
	if edges := s.projectEdges("P-A2"); len(edges) != 1 || edges[0].Text("upper_task_id") != "01mgmtA" {
		t.Fatalf("edges above P-A2 %v, want the caller's seat", edges)
	}

	// An owner registering itself holds no management seat and writes no edge.
	t.Setenv("CODEX_THREAD_ID", "01ownerC")
	s.mustBindProjectOwner("P-C", "01ownerC", "")
	if edges := s.projectEdges("P-C"); len(edges) != 0 {
		t.Fatalf("a self-registered owner got edges %v", edges)
	}
}

// TestProjectOwnerBindingSecondManagementTakesOnlyByHandover: a project has one upward execution
// edge, so a second management session binding the same owner is refused link_conflict and commits
// only its conflict row; binding another owner is refused by today's duplicate-owner rule; the
// handover of the seat moves the edge with its record, and a project owner handover moves the
// edge's lower end.
func TestProjectOwnerBindingSecondManagementTakesOnlyByHandover(t *testing.T) {
	s := newStoreSeat(t)
	t.Setenv("CODEX_THREAD_ID", "")
	s.mustBindManagement("01mgmtA", "01mgmtA")
	s.mustBindManagement("01mgmtB", "01mgmtB")
	s.mustBindProjectOwner("P-A", "01ownerA", "01mgmtA")
	edges := s.projectEdges("P-A")

	s.mustRefuse("link_conflict", "linkage-bind", "--role", "parent", "--scope", "P-A", "--task", "01ownerA", "--host", "host", "--creator", "01mgmtB")
	if conflicts := s.rows("SELECT reason, incumbent, challenger FROM linkage_conflicts WHERE scope_kind = 'project' AND scope_key = 'P-A'"); len(conflicts) != 1 ||
		conflicts[0].Text("reason") != "link_conflict" || conflicts[0].Text("incumbent") != "01mgmtA" || conflicts[0].Text("challenger") != "01mgmtB" {
		t.Fatalf("conflict record %v", conflicts)
	}
	if fromB := s.rows("SELECT link_id FROM scope_links WHERE upper_kind = 'management' AND upper_key = '01mgmtB'"); len(fromB) != 0 {
		t.Fatalf("the refused binding left edges %v", fromB)
	}
	// Another owner is today's duplicate-owner refusal, whoever the creator is.
	code, out := s.bindProjectOwner("P-A", "01ownerB", "01mgmtB")
	if code != 2 || s.refusedReason(out) != "duplicate_scope_owner" {
		t.Fatalf("a second owner: exit %d %q, want duplicate_scope_owner", code, out)
	}
	if now := s.projectEdges("P-A"); len(now) != 1 || now[0].Text("upper_key") != "01mgmtA" || now[0].Text("revision") != edges[0].Text("revision") {
		t.Fatalf("edges above P-A %v after the refusals", now)
	}

	// The seat's handover moves the edge to the new owner, with the handover's record.
	s.mustHandover("supervisor", "01mgmtA", managementKind, "01mgmtA", "01mgmtNew")
	moved := s.projectEdges("P-A")
	if len(moved) != 1 || moved[0].Text("upper_key") != "01mgmtA" || moved[0].Text("upper_task_id") != "01mgmtNew" || moved[0].Text("link_id") != edges[0].Text("link_id") {
		t.Fatalf("after the seat handover the edge is %v, want the same link with upper_task_id 01mgmtNew", moved)
	}
	if journal := s.rows("SELECT subject FROM journal WHERE kind = 'scope_handover'"); len(journal) != 1 {
		t.Fatalf("scope_handover journal %v", journal)
	}
	// The project owner's own handover moves the edge's lower end.
	s.mustHandover("parent", "P-A", "", "01ownerA", "01ownerNew")
	if after := s.projectEdges("P-A"); len(after) != 1 || after[0].Text("lower_task_id") != "01ownerNew" || after[0].Text("upper_task_id") != "01mgmtNew" {
		t.Fatalf("after the owner handover the edge is %v", after)
	}
}

// TestProjectOwnerBindingUnderALegacyInitiativeKeepsIt: a project an initiative lead already
// supervises (the legacy configuration) keeps that edge when a management session binds its owner.
// The binding is not refused: it succeeds, writes no management edge, answers the warning under
// managementLinkSkipped and records it once as a management_link_skipped journal row.
func TestProjectOwnerBindingUnderALegacyInitiativeKeepsIt(t *testing.T) {
	s := newStoreSeat(t)
	t.Setenv("CODEX_THREAD_ID", "")
	s.mustBindManagement("01mgmtA", "01mgmtA")
	if code, out := s.run("linkage-supervise", "--initiative", "INI-L", "--project", "P-L", "--supervisor-task", "01leadL", "--supervisor-host", "host",
		"--parent-task", "01ownerL", "--parent-host", "host"); code != 0 {
		t.Fatalf("linkage-supervise: exit %d %q", code, out)
	}
	legacy := s.projectEdges("P-L")
	if len(legacy) != 1 || legacy[0].Text("upper_kind") != "initiative" {
		t.Fatalf("legacy edge %v", legacy)
	}
	answer := s.mustBindProjectOwner("P-L", "01ownerL", "01mgmtA")
	warning, _ := answer["managementLinkSkipped"].(map[string]any)
	if answer["taskId"] != "01ownerL" || warning == nil || warning["reason"] != "link_conflict" || warning["standing"] != legacy[0].Text("link_id") {
		t.Fatalf("binding answered %v, want the owner's binding with the skip warning", answer)
	}
	if _, linked := answer["managementLink"]; linked {
		t.Fatalf("a skipped edge is answered as written: %v", answer)
	}
	if now := s.projectEdges("P-L"); len(now) != 1 || now[0].Text("link_id") != legacy[0].Text("link_id") || now[0].Text("revision") != legacy[0].Text("revision") {
		t.Fatalf("edges above P-L %v, want the legacy edge untouched", now)
	}
	s.mustBindProjectOwner("P-L", "01ownerL", "01mgmtA")
	if skipped := s.rows("SELECT subject FROM journal WHERE kind = 'management_link_skipped'"); len(skipped) != 1 {
		t.Fatalf("management_link_skipped journal %v, want one row for the two bindings", skipped)
	}
	if conflicts := s.rows("SELECT reason FROM linkage_conflicts WHERE scope_kind = 'project' AND scope_key = 'P-L'"); len(conflicts) != 0 {
		t.Fatalf("a skip is no refusal, yet conflicts %v", conflicts)
	}
}

// TestLinkageSuperviseOverAManagementLinkedProjectNamesTheSeat: an initiative supervision over a
// project a management edge already stands above is refused duplicate_scope_owner (one upward
// execution edge), and the refusal names the management seat that holds the project, not an
// initiative of that name, and how to keep the legacy order (supervise before the owner binding).
func TestLinkageSuperviseOverAManagementLinkedProjectNamesTheSeat(t *testing.T) {
	s := newStoreSeat(t)
	t.Setenv("CODEX_THREAD_ID", "")
	s.mustBindManagement("01mgmtA", "01mgmtA")
	s.mustBindProjectOwner("P-A", "01ownerA", "01mgmtA")
	code, out := s.run("linkage-supervise", "--initiative", "INI-A", "--project", "P-A", "--supervisor-task", "01leadA", "--supervisor-host", "host",
		"--parent-task", "01ownerA", "--parent-host", "host")
	if code == 0 {
		t.Fatalf("linkage-supervise over a management-linked project succeeded: %q", out)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(out), &answer); err != nil {
		t.Fatalf("refusal is not JSON: %v (%q)", err, out)
	}
	raw, _ := json.Marshal(answer)
	text := string(raw)
	if !strings.Contains(text, "duplicate_scope_owner") || !strings.Contains(text, "management seat '01mgmtA'") ||
		strings.Contains(text, "from initiative '01mgmtA'") || !strings.Contains(text, "before the project owner's binding") {
		t.Fatalf("refusal %s does not name the management seat and the legacy order", text)
	}
	if edges := s.projectEdges("P-A"); len(edges) != 1 || edges[0].Text("upper_kind") != "management" {
		t.Fatalf("edges above P-A %v, want the management edge alone", edges)
	}
}
