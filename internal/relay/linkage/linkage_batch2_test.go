package linkage

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

type handoverArgs struct {
	role, key, expect string
	endpoint          *registry.Endpoint
	acknowledged      []string
	evidence          string
}

// handover is gen_linkage.handover: defaults PARENT over PROJECT to OTHER_PARENT.
func (w *world) handover(a handoverArgs) any {
	if a.role == "" {
		a.role = "parent"
	}
	if a.key == "" {
		a.key = project
	}
	if a.expect == "" {
		a.expect = parent
	}
	if a.evidence == "" {
		a.evidence = "taking over"
	}
	ep := parentEP(otherParent)
	if a.endpoint != nil {
		ep = *a.endpoint
	}
	return w.step(w.r.Handover(w.ctx, a.role, a.key, a.expect, ep, a.acknowledged, a.evidence, "test"))
}

func ep(e registry.Endpoint) *registry.Endpoint { return &e }

// reg is gen_linkage.reg: registry.register with a dispatch id and its "-turn".
func (w *world) reg(in registry.Registration, dispatch string) string {
	if in.Parent.TaskID == "" {
		in.Parent = registry.Endpoint{TaskID: parent, HostID: host, Cwd: ns("/parent")}
	}
	if in.Child.TaskID == "" {
		in.Child = bare("01child-two")
	}
	if in.IssueKey == "" {
		in.IssueKey = issue
	}
	if in.AllowedRecipients == nil {
		in.AllowedRecipients = []string{parent}
	}
	in.ArtifactRoots = []string{root}
	in.DispatchRequestID, in.DispatchTurnID = dispatch, ns(dispatch+"-turn")
	x, err := w.r.Register(w.ctx, in)
	if err != nil {
		w.step(nil, err)
		return ""
	}
	w.step(x.ID, nil)
	return x.ID
}

func (w *world) resumeAt(rid string, generation int64) {
	_, err := w.r.Resume(w.ctx, rid, generation, []string{root}, []string{parent}, "test")
	w.step(nil, err)
}

func (w *world) statusOf(rid string) {
	x, err := w.r.Get(w.ctx, rid)
	w.step(x.Status, err)
}

func objects(list []contract.OrderedObject) []any {
	out := []any{}
	for _, o := range list {
		out = append(out, o)
	}
	return out
}

func (w *world) outstanding(task sql.NullString) []string {
	w.t.Helper()
	out, err := w.r.Outstanding(w.ctx, project, task)
	if err != nil {
		w.t.Fatal(err)
	}
	return out
}

func refusalDetail(step map[string]any) string {
	r, _ := step["refused"].(map[string]any)
	return text(r["detail"])
}

// Test26_LNK11: contested directives are kept, settled ones are not re-decided, the refusal
// is retained, and the same instruction after a handover is its own record.
func Test26_LNK11_contested_directives_are_kept(t *testing.T) {
	t.Run("contested", func(t *testing.T) {
		w := newWorld(t)
		execution := field(w.superviseDefault(), "linkId").(string)
		first := w.record(initiative, supervisorTask, execution, "d-one", "project", project).(contract.OrderedObject)
		second := w.record(initiative, supervisorTask, execution, "d-two", "project", project).(contract.OrderedObject)
		contested := func() {
			list, err := w.r.ContestedDirectives(w.ctx, "project", project)
			w.step(objects(list), err)
		}
		contested()
		firstID, secondID := text(field(first, "directiveId")), text(field(second, "directiveId"))
		w.step(w.r.SettleDirective(w.ctx, firstID, "chosen", "alice", sql.NullString{}))
		w.step(w.r.SettleDirective(w.ctx, secondID, "superseded", "alice", ns("the initiative deferred")))
		contested()
		w.step(w.r.SettleDirective(w.ctx, firstID, "superseded", "bob", sql.NullString{}))
		w.step(w.r.SettleDirective(w.ctx, firstID, "chosen", "a replaying caller", sql.NullString{}))
		list, err := w.r.Directives(w.ctx, "project", project)
		w.step(objects(list), err)
		w.step(w.r.Conflicts(w.ctx, "project", project))
		w.sameAsPython("lnk11_contested_directives")
	})
	t.Run("same_instruction_after_handover", func(t *testing.T) {
		w := newWorld(t)
		execution := field(w.superviseDefault(), "linkId").(string)
		w.record(initiative, supervisorTask, execution, "d-same", "project", project)
		w.handover(handoverArgs{role: "supervisor", key: initiative, expect: supervisorTask, endpoint: ep(supervisorEP(otherSupervisor)), evidence: "a new supervisor"})
		w.record(initiative, otherSupervisor, execution, "d-same", "project", project)
		w.sameAsPython("lnk11_same_instruction_after_handover")
	})
}

// Test26_LNK12: handover confirmation is decided inside the write transaction.
func Test26_LNK12_handover_confirmation(t *testing.T) {
	t.Run("unconfirmed", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.handover(handoverArgs{})
		w.handover(handoverArgs{expect: "01somebody-else"})
		w.handover(handoverArgs{evidence: "   "})
		w.handover(handoverArgs{endpoint: ep(parentEP(parent)), acknowledged: []string{rid}, evidence: "handing over to myself"})
		w.ownerStep("project", project)
		w.step(w.r.Conflicts(w.ctx, "project", project))
		for _, e := range []registry.Endpoint{{TaskID: "", HostID: host}, {TaskID: otherParent, HostID: ""}} {
			w.handover(handoverArgs{endpoint: ep(e), evidence: "a blank replacement"})
		}
		w.handover(handoverArgs{key: "PROJ-NOBODY", endpoint: ep(parentEP(parent)), evidence: "handing over a scope that does not exist"})
		want := w.sameAsPython("lnk12_handover_unconfirmed")
		if !strings.Contains(refusalDetail(want[len(want)-1]), "no live owner") {
			t.Fatal("an unregistered scope is not told so")
		}
	})
	t.Run("second_stale_handover", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		w.handover(handoverArgs{evidence: "the first handover"})
		w.handover(handoverArgs{endpoint: ep(parentEP("01parent-three")), evidence: "a stale second handover"})
		w.rows("SELECT task_id FROM scope_bindings WHERE scope_key = ? AND role = ?  AND status IN ('active','paused')", project, "parent")
		w.sameAsPython("lnk12_second_stale_handover")
	})
	t.Run("self_handover", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		w.handover(handoverArgs{endpoint: ep(parentEP(parent)), evidence: "handing over to myself"})
		w.ownerStep("project", project)
		w.step(w.r.Conflicts(w.ctx, "project", project))
		w.sameAsPython("lnk12_self_handover_settled")
	})
}

// Test26_LNK13: a handover would strand the project's attached work.
func Test26_LNK13_a_handover_would_strand_attached_work(t *testing.T) {
	t.Run("would_strand", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.step(w.r.Outstanding(w.ctx, project, sql.NullString{}))
		w.step(w.r.Outstanding(w.ctx, project, ns("01nobody")))
		w.step(w.r.Attached(w.ctx, project, sql.NullString{}, sql.NullString{}))
		w.handover(handoverArgs{acknowledged: []string{rid}, evidence: "the outgoing parent listed its unfinished issues"})
		w.exec("UPDATE relationships SET status = 'active' WHERE relationship_id = ?", rid)
		w.handover(handoverArgs{acknowledged: w.outstanding(sql.NullString{}), evidence: "a settled-looking project"})
		w.ownerStep("project", project)
		want := w.sameAsPython("lnk13_would_strand")
		detail := refusalDetail(want[3])
		if !strings.Contains(detail, rid) || !strings.Contains(detail, "supersedes") || !strings.Contains(detail, "reopens") {
			t.Fatalf("detail %q", detail)
		}
	})
	t.Run("third_parent", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.reg(registry.Registration{Parent: bare("01parent-three"), AllowedRecipients: []string{"01parent-three"}, Supersedes: rid}, "dispatch-third")
		w.step(w.r.Attached(w.ctx, project, ns(parent), sql.NullString{}))
		w.handover(handoverArgs{acknowledged: w.outstanding(sql.NullString{}), evidence: "work parked on a third parent"})
		w.sameAsPython("lnk13_third_parent")
	})
}

// Test26_LNK14: a handover of a settled scope succeeds; the escape route is reachable; a child
// is not handed over through linkage.
func Test26_LNK14_a_settled_handover_succeeds(t *testing.T) {
	t.Run("settled", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		w.handover(handoverArgs{evidence: "the project has no unfinished issues left"})
		w.step(w.r.Binding(w.ctx, registry.BindingID("parent", "project", project, parent)))
		w.step(w.r.Link(w.ctx, registry.LinkID("execution", "initiative", initiative, "project", project)))
		w.ownerStep("project", project)
		w.sameAsPython("lnk14_settled_handover")
	})
	t.Run("escape_route", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.handover(handoverArgs{acknowledged: []string{rid}, evidence: "before the work moved"})
		w.reg(registry.Registration{Parent: registry.Endpoint{TaskID: otherParent, HostID: host, Cwd: ns("/parent")},
			AllowedRecipients: []string{otherParent}, Supersedes: rid}, "dispatch-moved")
		w.step(w.r.Attached(w.ctx, project, ns(parent), sql.NullString{}))
		w.step(w.r.Outstanding(w.ctx, project, sql.NullString{}))
		w.handover(handoverArgs{acknowledged: w.outstanding(sql.NullString{}), evidence: "the work moved first"})
		w.ownerStep("issue", issue)
		w.sameAsPython("lnk14_escape_route")
	})
	t.Run("child", func(t *testing.T) {
		w := newWorld(t)
		w.scoped()
		w.handover(handoverArgs{role: "child", key: issue, expect: child, endpoint: ep(bare("01child-two")), evidence: "trying to move a child sideways"})
		w.ownerStep("issue", issue)
		w.sameAsPython("lnk14_child_not_handed_over")
	})
}

// Test26_LNK15: host and endpoint on replay and reactivation.
func Test26_LNK15_hosts_and_endpoints_on_replay(t *testing.T) {
	t.Run("replay_other_host", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		s := w.supervision()
		s.parent = registry.Endpoint{TaskID: parent, HostID: "some-other-host", Cwd: ns("/parent")}
		w.step(w.supervise(s))
		w.ownerStep("project", project)
		w.step(w.r.Conflicts(w.ctx, "project", project))
		w.sameAsPython("lnk15_replay_other_host")
	})
	t.Run("take_back_from_new_host", func(t *testing.T) {
		w := newWorld(t)
		w.step(w.r.BindScopeAs(w.ctx, "parent", project, registry.Endpoint{TaskID: parent, HostID: host, Cwd: ns("/first"), CXCSession: ns("cxc-first")}, "active"))
		w.handover(handoverArgs{evidence: "handed away"})
		w.handover(handoverArgs{expect: otherParent, endpoint: ep(registry.Endpoint{TaskID: parent, HostID: "host-two", Cwd: ns("/new-host"), CXCSession: ns("cxc-second")}),
			evidence: "taken back, from a different machine"})
		w.ownerStep("project", project)
		w.step(w.r.BindScopeAs(w.ctx, "parent", project, registry.Endpoint{TaskID: parent, HostID: "host-three"}, "active"))
		w.sameAsPython("lnk15_take_back_from_new_host")
	})
	t.Run("restore_keeps_status", func(t *testing.T) {
		w := newWorld(t)
		w.step(w.r.BindScopeAs(w.ctx, "child", issue, bare(child), "archived"))
		w.step(w.r.BindScopeAs(w.ctx, "child", issue, bare(child), "paused"))
		w.sameAsPython("lnk15_restore_keeps_status")
	})
	t.Run("cli_handover_keeps_cxc_session", func(t *testing.T) {
		w := newWorld(t)
		w.must(w.r.BindScopeAs(w.ctx, "parent", project, registry.Endpoint{TaskID: parent, HostID: host, Cwd: ns("/parent"), CXCSession: ns("cxc-first")}, "active"))
		code, stdout, stderr := runCLI(t, w, "linkage-handover", "--role", "parent", "--scope", project, "--expect-task", parent,
			"--task", otherParent, "--host", host, "--cwd", "/replacement", "--cxc-session", "cxc-replacement",
			"--evidence", "the project changed hands", "--actor", "test")
		if code != 0 {
			t.Fatalf("exit %d: %s %s", code, stdout, stderr)
		}
		owner := w.owner("project", project)
		if field(owner, "taskId") != otherParent || field(owner, "cwd") != "/replacement" ||
			field(field(owner, "_bindings").(contract.OrderedObject), "cxcSession") != "cxc-replacement" {
			t.Fatalf("owner %v", owner)
		}
	})
}

// Test26_LNK16: blank endpoints never reach a binding.
func Test26_LNK16_blank_endpoints_never_reach_a_binding(t *testing.T) {
	t.Run("supervision_and_peer", func(t *testing.T) {
		w := newWorld(t)
		for _, pair := range [][2]registry.Endpoint{{supervisorEP(""), parentEP(parent)}, {{TaskID: supervisorTask}, parentEP(parent)},
			{supervisorEP(supervisorTask), parentEP("")}, {supervisorEP(supervisorTask), {TaskID: parent}}} {
			s := w.supervision()
			s.supervisor, s.parent = pair[0], pair[1]
			w.step(w.supervise(s))
		}
		w.rows("SELECT 1 FROM scope_bindings")
		w.superviseDefault()
		s := w.supervision()
		s.project, s.parent = otherProject, parentEP(otherParent)
		w.must(w.supervise(s))
		w.step(w.r.RegisterPeer(w.ctx, project, registry.Endpoint{HostID: host}, otherProject, parentEP(otherParent)))
		w.sameAsPython("lnk16_blank_endpoints")
	})
	t.Run("blank_child_host", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		rid := w.register()
		w.exec("UPDATE relationships SET child_host_id = '' WHERE relationship_id = ?", rid)
		w.step(w.r.AttachIssue(w.ctx, rid, project))
		w.ownerStep("issue", issue)
		want := w.sameAsPython("lnk16_blank_child_host")
		if !strings.Contains(refusalDetail(want[0]), "host id") {
			t.Fatal("the refusal does not name the host id")
		}
	})
}

// Test26_LNK17: the assignment lifecycle moves the issue scope with it.
func Test26_LNK17_assignment_lifecycle_and_the_issue_scope(t *testing.T) {
	t.Run("pause_and_cancel", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.setStatus(rid, "paused")
		w.ownerStep("issue", issue)
		w.step(w.r.Down(w.ctx, "project", project), nil)
		w.setStatus(rid, "cancelled")
		w.ownerStep("issue", issue)
		w.sameAsPython("lnk17_pause_and_cancel")
	})
	t.Run("archived_then_resume", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.setStatus(rid, "archived")
		w.ownerStep("issue", issue)
		_, err := w.r.SetStatus(w.ctx, rid, "paused", "test")
		w.step(nil, err)
		w.statusOf(rid)
		w.resumeAt(rid, 7)
		w.ownerStep("issue", issue)
		w.statusOf(rid)
		w.resumeAt(rid, 1)
		w.ownerStep("issue", issue)
		w.step(w.r.Attachment(w.ctx, rid))
		w.sameAsPython("lnk17_archived_then_resume")
	})
}

// Test26_LNK18: resume revalidates who holds the scope now and keeps its contest.
func Test26_LNK18_resume_revalidates_the_scope(t *testing.T) {
	t.Run("reassigned_issue", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.setStatus(rid, "cancelled")
		w.must(w.r.BindScopeAs(w.ctx, "child", issue, bare("01child-two"), "active"))
		w.resumeAt(rid, 1)
		w.rows("SELECT task_id FROM scope_bindings WHERE scope_key = ? AND role = ?  AND status IN ('active','paused')", issue, "child")
		w.statusOf(rid)
		w.step(w.r.Conflicts(w.ctx, "issue", issue))
		w.sameAsPython("lnk18_reassigned_issue")
	})
	t.Run("project_changed_hands", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.setStatus(rid, "archived")
		w.handover(handoverArgs{evidence: "the archived assignment left nothing behind"})
		w.resumeAt(rid, 1)
		w.statusOf(rid)
		w.ownerStep("issue", issue)
		w.step(w.r.Conflicts(w.ctx, "project", project))
		w.sameAsPython("lnk18_project_changed_hands")
	})
	t.Run("project_without_parent", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.setStatus(rid, "archived")
		w.exec("UPDATE scope_bindings SET status = 'archived'  WHERE scope_kind = ? AND scope_key = ? AND role = ?", "project", project, "parent")
		w.resumeAt(rid, 1)
		w.statusOf(rid)
		w.sameAsPython("lnk18_project_without_parent")
	})
	t.Run("reclaimed_elsewhere", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.setStatus(rid, "archived")
		w.must(w.r.BindScopeAs(w.ctx, "child", issue, registry.Endpoint{TaskID: child, HostID: "host-two"}, "active"))
		w.resumeAt(rid, 1)
		w.statusOf(rid)
		w.ownerStep("issue", issue)
		w.step(w.r.Conflicts(w.ctx, "issue", issue))
		w.sameAsPython("lnk18_reclaimed_elsewhere")
	})
	t.Run("refused_resume_contest", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.setStatus(rid, "archived")
		w.must(w.r.BindScopeAs(w.ctx, "child", issue, bare("01child-two"), "active"))
		w.resumeAt(rid, 1)
		w.step(w.r.Conflicts(w.ctx, "issue", issue))
		w.statusOf(rid)
		w.sameAsPython("lnk18_refused_resume_contest")
	})
}

// Test26_LNK19: registration and supersession guards on the lower level.
func Test26_LNK19_lower_level_registration_guards(t *testing.T) {
	t.Run("other_issue_successor", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.reg(registry.Registration{Child: bare("01child-far"), IssueKey: "REL-FAR", Supersedes: rid}, "dispatch-far")
		w.statusOf(rid)
		w.step(w.r.Attachment(w.ctx, rid))
		w.sameAsPython("lnk19_other_issue_successor")
	})
	t.Run("borrowed_predecessor", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.reg(registry.Registration{Parent: parentEP(otherParent), Child: bare("01child-far"), IssueKey: "REL-FAR",
			AllowedRecipients: []string{otherParent}, Supersedes: rid, ProjectKey: project}, "dispatch-far")
		w.statusOf(rid)
		w.sameAsPython("lnk19_borrowed_predecessor")
	})
	t.Run("dead_predecessor", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.setStatus(rid, "cancelled")
		w.reg(registry.Registration{Parent: parentEP(otherParent), Child: registry.Endpoint{TaskID: "01child-two", HostID: host, Cwd: ns(root)},
			AllowedRecipients: []string{otherParent}, Supersedes: rid, ProjectKey: project}, "dispatch-dead")
		w.ownerStep("project", project)
		w.sameAsPython("lnk19_dead_predecessor")
	})
	t.Run("project_history_only", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		s := w.supervision()
		s.project, s.parent = otherProject, parentEP(otherParent)
		w.must(w.supervise(s))
		first := w.register()
		w.must(w.r.AttachIssue(w.ctx, first, project))
		w.setStatus(first, "cancelled")
		unrelated := w.reg(registry.Registration{Parent: bare(otherParent), Child: bare("01child-far"), AllowedRecipients: []string{otherParent}}, "dispatch-unrelated")
		w.step(w.r.AttachIssue(w.ctx, unrelated, project))
		w.sameAsPython("lnk19_project_history_only")
	})
	t.Run("same_child_second_parent", func(t *testing.T) {
		w := newWorld(t)
		w.scoped()
		w.reg(registry.Registration{Parent: parentEP(otherParent), Child: registry.Endpoint{TaskID: child, HostID: host, Cwd: ns(root)},
			AllowedRecipients: []string{otherParent}}, "dispatch-second-parent")
		w.rows("SELECT relationship_id FROM relationships WHERE issue_key = ?  AND status IN ('active','paused') AND superseded_by IS NULL", issue)
		w.sameAsPython("lnk19_same_child_second_parent")
	})
	t.Run("reclaimed_issue_successor", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.setStatus(rid, "cancelled")
		w.must(w.r.BindScopeAs(w.ctx, "child", issue, bare(child), "active"))
		w.reg(registry.Registration{Parent: parentEP(parent), Child: registry.Endpoint{TaskID: "01child-two", HostID: host, Cwd: ns(root)},
			Supersedes: rid, ProjectKey: project}, "dispatch-successor")
		w.ownerStep("issue", issue)
		w.sameAsPython("lnk19_reclaimed_issue_successor")
	})
}

// Test26_LNK20: supersession moves the lower level; late writes from a dead relationship never
// take it back.
func Test26_LNK20_supersession_moves_the_lower_level(t *testing.T) {
	t.Run("replacement_inherits", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		next := w.reg(registry.Registration{Supersedes: rid}, "dispatch-successor")
		w.step(w.r.Attachment(w.ctx, next))
		w.setStatus(rid, "cancelled")
		w.ownerStep("issue", issue)
		w.step(w.r.Link(w.ctx, registry.LinkID("execution", "project", project, "issue", issue)))
		w.sameAsPython("lnk20_replacement_inherits")
	})
	t.Run("same_child_again", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		first := w.register()
		w.must(w.r.AttachIssue(w.ctx, first, project))
		w.setStatus(first, "archived")
		w.handover(handoverArgs{evidence: "the archived assignment left nothing behind"})
		again := w.reg(registry.Registration{Parent: registry.Endpoint{TaskID: otherParent, HostID: host, Cwd: ns("/parent")}, Child: bare(child),
			AllowedRecipients: []string{otherParent}, ProjectKey: project}, "dispatch-again")
		w.setStatus(first, "cancelled")
		w.ownerStep("issue", issue)
		w.step(w.r.Attachment(w.ctx, again))
		w.sameAsPython("lnk20_same_child_again")
	})
	t.Run("direct_rebind_survives", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.setStatus(rid, "archived")
		w.step(w.r.BindScopeAs(w.ctx, "child", issue, bare(child), "active"))
		w.setStatus(rid, "cancelled")
		w.ownerStep("issue", issue)
		w.sameAsPython("lnk20_direct_rebind_survives")
	})
	t.Run("supersede_dead_leaves_reclaim", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.setStatus(rid, "cancelled")
		w.must(w.r.BindScopeAs(w.ctx, "child", issue, bare(child), "active"))
		if err := w.r.Supersede(w.ctx, rid, "rel-elsewhere"); err != nil {
			t.Fatal(err)
		}
		w.ownerStep("issue", issue)
		w.sameAsPython("lnk20_supersede_dead_leaves_reclaim")
	})
}

// Test26_LNK19_racer: a contest that only appears after the pre-check is still recorded, and
// the registration rolls back whole.
func Test26_LNK19_a_contest_after_the_pre_check_is_still_recorded(t *testing.T) {
	w := newWorld(t)
	w.superviseDefault()
	w.r.SetBeforeRegisterTx(func(in registry.Registration) {
		w.must(w.r.BindScopeAs(w.ctx, "child", in.IssueKey, bare("01child-racer"), "active"))
	})
	w.reg(registry.Registration{Parent: parentEP(parent), Child: registry.Endpoint{TaskID: child, HostID: host, Cwd: ns(root)}, ProjectKey: project}, "dispatch-raced")
	w.r.SetBeforeRegisterTx(nil)
	w.step(w.r.Conflicts(w.ctx, "issue", issue))
	w.rows("SELECT relationship_id FROM relationships WHERE issue_key = ?", issue)
	want := w.sameAsPython("lnk19_racer")
	if text(want[0]["refused"].(map[string]any)["reason"]) != "duplicate_scope_owner" || len(want[1]["ok"].([]any)) == 0 || len(want[2]["ok"].([]any)) != 0 {
		t.Fatalf("python oracle %v", want)
	}
}
