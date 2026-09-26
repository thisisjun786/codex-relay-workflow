package linkage

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func (w *world) full(rid string) {
	x, err := w.r.Get(w.ctx, rid)
	if err != nil {
		w.step(nil, err)
		return
	}
	w.step(x.FullRecord(), nil)
}

// handedTo is gen_linkage.handed_to: move the assignment first, then the scope after it.
func (w *world) handedTo(incoming, supersedes, dispatch string) string {
	moved := w.reg(registry.Registration{Parent: parentEP(incoming), Child: registry.Endpoint{TaskID: child, HostID: host, Cwd: ns(root)},
		Supersedes: supersedes, ProjectKey: project}, dispatch)
	holder := text(field(w.owner("project", project), "taskId"))
	w.handover(handoverArgs{expect: holder, endpoint: ep(parentEP(incoming)), acknowledged: w.outstanding(sql.NullString{}), evidence: "the assignment moved first"})
	return moved
}

// Test26_LNK21: a returning tenure (A -> B -> A) and its refusals.
func Test26_LNK21_a_returning_tenure(t *testing.T) {
	back := registry.Endpoint{TaskID: child, HostID: host, Cwd: ns(root)}
	t.Run("handback", func(t *testing.T) {
		w := newWorld(t)
		original := w.scoped()
		away := w.handedTo(otherParent, original, "dispatch-away")
		again := w.handedTo(parent, away, "dispatch-back")
		w.full(again)
		w.ownerStep("project", project)
		w.ownerStep("issue", issue)
		w.step(w.r.Attachment(w.ctx, original))
		w.full(away)
		w.rows("SELECT relationship_id, supersedes, superseded_by FROM relationships ORDER BY relationship_id")
		w.sameAsPython("lnk21_handback")
		if again != original {
			t.Fatal("the returning tenure is not the same relationship")
		}
	})
	t.Run("handback_new_host", func(t *testing.T) {
		w := newWorld(t)
		original := w.scoped()
		away := w.handedTo(otherParent, original, "dispatch-away")
		moved := w.reg(registry.Registration{Parent: registry.Endpoint{TaskID: parent, HostID: "host-two", Cwd: ns("/moved"), CXCSession: ns("cxc-moved")},
			Child: back, Supersedes: away, ProjectKey: project}, "dispatch-moved")
		w.full(moved)
		w.sameAsPython("lnk21_handback_new_host")
	})
	t.Run("restores_retained_project", func(t *testing.T) {
		w := newWorld(t)
		original := w.scoped()
		w.setStatus(original, "cancelled")
		interim := w.reg(registry.Registration{Parent: parentEP(otherParent), Child: registry.Endpoint{TaskID: "01child-two", HostID: host, Cwd: ns(root)},
			AllowedRecipients: []string{otherParent}}, "dispatch-interim")
		w.step(w.r.Attachment(w.ctx, interim))
		w.reg(registry.Registration{Parent: parentEP(parent), Child: back, Supersedes: interim}, "dispatch-back")
		w.ownerStep("issue", issue)
		w.step(w.r.Attachment(w.ctx, original))
		w.step(w.r.Up(w.ctx, registry.UpSelector{Issue: ns(issue)}), nil)
		w.sameAsPython("lnk21_restores_retained_project")
	})
	t.Run("retired_identity", func(t *testing.T) {
		w := newWorld(t)
		original := w.scoped()
		w.setStatus(original, "archived")
		w.reg(registry.Registration{Parent: parentEP(parent), Child: back}, "dispatch-silent")
		w.statusOf(original)
		want := w.sameAsPython("lnk21_refusals")
		if !strings.Contains(refusalDetail(want[0]), "relationship-resume") {
			t.Fatal("the refusal does not name relationship-resume")
		}
	})
	t.Run("predecessor_released", func(t *testing.T) {
		w := newWorld(t)
		original := w.scoped()
		away := w.handedTo(otherParent, original, "dispatch-away")
		w.setStatus(away, "cancelled")
		w.reg(registry.Registration{Parent: parentEP(parent), Child: back, Supersedes: away, ProjectKey: project}, "dispatch-late")
		w.statusOf(original)
		w.sameAsPython("lnk21_predecessor_released")
	})
	t.Run("live_replay_other_host", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		w.register()
		w.reg(registry.Registration{Parent: registry.Endpoint{TaskID: parent, HostID: "host-two", Cwd: ns("/parent")}, Child: back}, "dispatch-elsewhere")
		w.sameAsPython("lnk21_live_replay_other_host")
	})
	t.Run("earlier_dispatch", func(t *testing.T) {
		w := newWorld(t)
		original := w.scoped()
		away := w.reg(registry.Registration{Parent: parentEP(otherParent), Child: back, Supersedes: original, ProjectKey: project}, "dispatch-away")
		x, err := w.r.Register(w.ctx, registry.Registration{Parent: parentEP(parent), Child: back, IssueKey: issue, ArtifactRoots: []string{root},
			AllowedRecipients: []string{parent}, DispatchRequestID: "dispatch-1", DispatchTurnID: ns("turn-replayed"), Supersedes: away, ProjectKey: project})
		w.step(x.ID, err)
		w.statusOf(original)
		w.sameAsPython("lnk21_earlier_dispatch")
	})
	t.Run("refused_tenure_contest", func(t *testing.T) {
		w := newWorld(t)
		original := w.scoped()
		away := w.reg(registry.Registration{Parent: parentEP(otherParent), Child: registry.Endpoint{TaskID: "01child-two", HostID: host, Cwd: ns(root)},
			Supersedes: original, ProjectKey: project}, "dispatch-away")
		w.must(w.r.BindScopeAs(w.ctx, "child", "REL-ELSEWHERE", bare(child), "active"))
		w.reg(registry.Registration{Parent: parentEP(parent), Child: back, Supersedes: away, ProjectKey: project}, "dispatch-back")
		w.step(w.r.Conflicts(w.ctx, "issue", issue))
		w.full(away)
		w.full(original)
		w.sameAsPython("lnk21_refused_tenure_contest")
	})
}

// contestTheProject gives PROJECT a second live parent, the way a store with no guard index can.
func (w *world) contestTheProject() {
	w.exec("DROP INDEX scope_bindings_one_live_owner")
	w.exec("INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key,"+
		" task_id, host_id, cwd, cxc_session, status, revision, supersedes,"+
		" superseded_by, handover_note, created_at, updated_at)"+
		" VALUES (?,?,?,?,?,?,NULL,NULL,'active',2,NULL,NULL,NULL,?,?)",
		registry.BindingID("parent", "project", project, otherParent), "parent", "project", project, otherParent, host,
		"2026-09-19T00:00:00Z", "2026-09-19T00:00:00Z")
}

func (w *world) duplicateOwners() {
	w.exec("DROP INDEX scope_bindings_one_live_owner")
	for i, task := range []string{"01owner-one", "01owner-two"} {
		w.exec("INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key,"+
			" task_id, host_id, cwd, cxc_session, status, revision, supersedes,"+
			" superseded_by, handover_note, created_at, updated_at)"+
			" VALUES (?,?,?,?,?,?,NULL,NULL,'active',?,NULL,NULL,NULL,?,?)",
			registry.BindingID("parent", "project", project, task), "parent", "project", project, task, host, i+1,
			"2026-09-19T00:00:00Z", "2026-09-19T00:00:00Z")
	}
}

// Test26_LNK22: writers refuse a contested project instead of picking an owner.
func Test26_LNK22_writers_refuse_a_contested_project(t *testing.T) {
	w := newWorld(t)
	w.superviseDefault()
	rid := w.register()
	s := w.supervision()
	s.project, s.parent = otherProject, parentEP("01parent-three")
	w.must(w.supervise(s))
	w.contestTheProject()
	w.step(w.r.AttachIssue(w.ctx, rid, project))
	w.step(w.r.Attachment(w.ctx, rid))
	w.step(w.r.RegisterPeer(w.ctx, project, parentEP(parent), otherProject, parentEP("01parent-three")))
	w.step(w.r.BindScopeAs(w.ctx, "parent", project, parentEP(parent), "active"))
	w.step(w.r.Conflicts(w.ctx, "project", project))
	w.sameAsPython("lnk22_contested_writers")
}

// Test26_LNK23: walk answer states, the unreadable one included.
func Test26_LNK23_walk_answer_states(t *testing.T) {
	w := newWorld(t)
	w.step(w.r.Down(w.ctx, "initiative", "INIT-NEVER-SEEN"), nil)
	w.superviseDefault()
	w.exec("INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id,"+
		" host_id, cwd, cxc_session, status, revision, supersedes, superseded_by,"+
		" handover_note, created_at, updated_at)"+
		" VALUES ('bnd-staged-second','parent','project',?,?,?,NULL,NULL,'active',1,"+
		"         NULL,NULL,NULL,?,?)", otherProject, parent, host, fakeISO, fakeISO)
	w.step(w.r.Up(w.ctx, registry.UpSelector{Task: ns(parent)}), nil)
	w.step(w.r.Up(w.ctx, registry.UpSelector{Task: ns(parent), Scope: ns(otherProject)}), nil)
	rid := w.register()
	w.must(w.r.AttachIssue(w.ctx, rid, project))
	w.step(w.r.Up(w.ctx, registry.UpSelector{Task: ns(parent), Issue: ns(issue)}), nil)
	w.step(w.r.Up(w.ctx, registry.UpSelector{Relationship: ns(rid)}), nil)
	// store.db.close(): every later read fails the way Python's closed connection does.
	if err := w.s.DB.Close(); err != nil {
		t.Fatal(err)
	}
	down := w.r.Down(w.ctx, "initiative", initiative)
	up := w.r.Up(w.ctx, registry.UpSelector{Task: ns(parent)})
	counterpart := w.r.Counterpart(w.ctx, parent, child, registry.CounterpartQuery{})
	for _, answer := range []contract.OrderedObject{down, up, counterpart} {
		if field(answer, "state") != "unreadable" || field(answer, "readable") != false || text(field(answer, "detail")) == "" {
			t.Fatalf("unreadable answer %v", answer)
		}
		// The fault class differs by driver (Python: ProgrammingError on a closed connection);
		// the shape outside detail is compared whole.
		w.step(dropDetail(answer), nil)
	}
	all, err := python()
	if err != nil {
		t.Fatal(err)
	}
	want := all["lnk23_walk_states"]
	for i := len(want) - 3; i < len(want); i++ {
		ok := want[i]["ok"].(map[string]any)
		delete(ok, "detail")
	}
	w.sameAsPython("lnk23_walk_states")
}

func dropDetail(o contract.OrderedObject) contract.OrderedObject {
	out := contract.OrderedObject{}
	for _, f := range o {
		if f.Key != "detail" {
			out = append(out, f)
		}
	}
	return out
}

func (w *world) strayLink(kind, init string, revision int, upperTask string) string {
	lid := registry.LinkID(kind, "initiative", init, "project", project)
	w.exec("INSERT INTO scope_links (link_id, link_kind, upper_kind, upper_key,"+
		" upper_task_id, lower_kind, lower_key, lower_task_id, status, revision,"+
		" superseded_by, created_at, updated_at)"+
		" VALUES (?,?,?,?,?,?,?,?,'active',?,NULL,?,?)",
		lid, kind, "initiative", init, upperTask, "project", project, parent, revision, "2026-09-19T00:00:00Z", "2026-09-19T00:00:00Z")
	return lid
}

// Test26_LNK24: walk contention words, the same in both directions.
func Test26_LNK24_walk_contention_words(t *testing.T) {
	t.Run("instruction_conflict", func(t *testing.T) {
		w := newWorld(t)
		execution := text(field(w.superviseDefault(), "linkId"))
		w.must(w.supervise(supervision{"INIT-2", project, supervisorEP(otherSupervisor), parentEP(parent), "reference"}))
		w.record(initiative, supervisorTask, execution, "d-one", "project", project)
		w.record(initiative, supervisorTask, execution, "d-two", "project", project)
		w.step(w.r.Down(w.ctx, "project", project), nil)
		w.step(w.r.Up(w.ctx, registry.UpSelector{Task: ns(parent)}), nil)
		w.sameAsPython("lnk24_instruction_conflict")
	})
	t.Run("cycle", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		w.exec("INSERT INTO scope_links (link_id, link_kind, upper_kind, upper_key,"+
			" upper_task_id, lower_kind, lower_key, lower_task_id, status, revision,"+
			" superseded_by, created_at, updated_at)"+
			" VALUES ('lnk-forced-cycle','execution','project',?,?,'initiative',?,?,"+
			"         'active',1,NULL,?,?)", project, parent, initiative, supervisorTask, fakeISO, fakeISO)
		w.step(w.r.Down(w.ctx, "initiative", initiative), nil)
		w.sameAsPython("lnk24_cycle")
	})
	t.Run("two_parents_of_issue", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		s := w.supervision()
		s.project, s.parent = otherProject, parentEP(otherParent)
		w.must(w.supervise(s))
		for _, p := range []string{project, otherProject} {
			w.exec("INSERT INTO scope_links (link_id, link_kind, upper_kind, upper_key,"+
				" upper_task_id, lower_kind, lower_key, lower_task_id, status, revision,"+
				" superseded_by, created_at, updated_at)"+
				" VALUES (?,'execution','project',?,?,'issue',?,?,'active',1,NULL,?,?)",
				registry.LinkID("execution", "project", p, "issue", issue), p, parent, issue, child, "2026-09-19T00:00:00Z", "2026-09-19T00:00:00Z")
		}
		w.step(w.r.Down(w.ctx, "initiative", initiative), nil)
		w.sameAsPython("lnk24_two_parents_of_issue")
	})
	t.Run("two_supervisions", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		w.strayLink("execution", otherInitiative, 2, otherSupervisor)
		w.step(w.r.Up(w.ctx, registry.UpSelector{Task: ns(parent)}), nil)
		w.sameAsPython("lnk24_two_supervisions")
	})
	t.Run("two_live_owners", func(t *testing.T) {
		w := newWorld(t)
		w.duplicateOwners()
		w.step(w.r.Down(w.ctx, "project", project), nil)
		w.step(w.r.Up(w.ctx, registry.UpSelector{Task: ns("01owner-two")}), nil)
		w.sameAsPython("lnk24_two_live_owners")
	})
	t.Run("handover_staging", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.reg(registry.Registration{Parent: parentEP(otherParent), Child: registry.Endpoint{TaskID: "01child-two", HostID: host, Cwd: ns(root)},
			AllowedRecipients: []string{otherParent}, Supersedes: rid, ProjectKey: project}, "dispatch-moved")
		w.step(w.r.Down(w.ctx, "initiative", initiative), nil)
		w.step(w.r.Up(w.ctx, registry.UpSelector{Issue: ns(issue)}), nil)
		w.sameAsPython("lnk24_handover_staging")
	})
}

func (w *world) counterpart(from, to string, q registry.CounterpartQuery) {
	w.step(w.r.Counterpart(w.ctx, from, to, q), nil)
}

// Test26_LNK25: counterpart answers never borrow another link.
func Test26_LNK25_counterpart_never_borrows_a_link(t *testing.T) {
	t.Run("counterpart", func(t *testing.T) {
		w := newWorld(t)
		w.scoped()
		w.counterpart(parent, child, registry.CounterpartQuery{})
		w.counterpart(parent, child, registry.CounterpartQuery{QuotedRevision: sql.NullInt64{Int64: 99, Valid: true}})
		w.counterpart(parent, child, registry.CounterpartQuery{FromScope: ns("PROJ-NOT-MINE")})
		w.counterpart(parent, child, registry.CounterpartQuery{QuotedScope: ns("ISS-NOT-HELD")})
		w.counterpart(supervisorTask, parent, registry.CounterpartQuery{})
		w.sameAsPython("lnk25_counterpart")
	})
	t.Run("key_reused_at_another_level", func(t *testing.T) {
		w := newWorld(t)
		w.must(w.r.BindScopeAs(w.ctx, "supervisor", "SHARED-KEY", supervisorEP(supervisorTask), "active"))
		w.must(w.r.BindScopeAs(w.ctx, "child", "SHARED-KEY", bare("01child-far"), "active"))
		w.counterpart(supervisorTask, "01child-far", registry.CounterpartQuery{})
		w.sameAsPython("lnk25_key_reused_at_another_level")
	})
	t.Run("two_edges", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		w.strayLink("reference", initiative, 1, supervisorTask)
		w.counterpart(supervisorTask, parent, registry.CounterpartQuery{})
		w.sameAsPython("lnk25_two_edges")
	})
	t.Run("two_historical_scopes", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		w.handover(handoverArgs{evidence: "the first project moved on"})
		s := w.supervision()
		s.project = otherProject
		w.must(w.supervise(s))
		w.handover(handoverArgs{key: otherProject, endpoint: ep(parentEP("01parent-three")), evidence: "and so did the second"})
		w.counterpart(supervisorTask, parent, registry.CounterpartQuery{})
		w.counterpart(supervisorTask, parent, registry.CounterpartQuery{QuotedScope: ns(otherProject)})
		w.sameAsPython("lnk25_two_historical_scopes")
	})
}

// Test26_LNK26: the partial unique guard scope_bindings_one_live_owner.
func Test26_LNK26_the_one_live_owner_guard(t *testing.T) {
	t.Run("database_refuses", func(t *testing.T) {
		w := newWorld(t)
		w.must(w.r.BindScopeAs(w.ctx, "parent", project, parentEP(parent), "active"))
		_, err := w.s.DB.ExecContext(w.ctx, "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key,"+
			" task_id, host_id, cwd, cxc_session, status, revision, supersedes,"+
			" superseded_by, handover_note, created_at, updated_at)"+
			" VALUES ('bnd-forced','parent','project',?,?,?,NULL,NULL,'active',1,NULL,"+
			"         NULL,NULL,?,?)", project, otherParent, host, fakeISO, fakeISO)
		if err == nil {
			t.Fatal("the database accepted a second live owner")
		}
		w.step(store.PythonSQLiteError(err), nil)
		w.sameAsPython("lnk26_guard_index")
	})
	t.Run("unenforced", func(t *testing.T) {
		w := newWorld(t)
		code, stdout, _ := runCLI(t, w, "linkage-down", "--scope-kind", "project", "--scope", project)
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		w.step(decodeStdout(t, stdout), nil)
		w.duplicateOwners()
		reopened, err := store.Open(context.Background(), w.path, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		unenforced := []any{}
		for _, index := range reopened.UnenforcedIndexes {
			unenforced = append(unenforced, contract.OrderedObject{{Key: "index", Value: index.Index}, {Key: "detail", Value: index.Detail}})
		}
		w.step(unenforced, nil)
		r2 := &registry.Registry{Store: reopened, Now: w.r.Now, Policy: w.r.Policy}
		w.step(r2.Up(w.ctx, registry.UpSelector{Task: ns("01owner-one")}), nil)
		_, stdout, _ = runCLI(t, w, "linkage-down", "--scope-kind", "project", "--scope", project)
		w.step(decodeStdout(t, stdout), nil)
		w.sameAsPython("lnk26_unenforced")
	})
}

// Test26_LNK27: relay-owned ids keep 128 bits.
func Test26_LNK27_relay_owned_ids_keep_128_bits(t *testing.T) {
	w := newWorld(t)
	bid := registry.BindingID("parent", "project", project, parent)
	lid := registry.LinkID("execution", "initiative", initiative, "project", project)
	w.step(bid, nil)
	w.step(lid, nil)
	w.step(registry.LinkID("peer", "project", otherProject, "project", project), nil)
	w.step(registry.DirectiveID("project", project, initiative, "d-one", 1), nil)
	w.sameAsPython("lnk27_ids")
	if len(bid) != len("bnd-")+32 || len(lid) != len("lnk-")+32 {
		t.Fatalf("%s %s", bid, lid)
	}
}

// Test26_LNK28: CLI selectors of linkage-up.
func Test26_LNK28_linkage_up_selectors(t *testing.T) {
	w := newWorld(t)
	for _, argv := range [][]string{{"linkage-up"}, {"linkage-up", "--relationship", "rel-a", "--issue", issue}} {
		if code, _, stderr := runCLI(t, w, argv...); code != 2 || !strings.Contains(stderr, "usage:") {
			t.Fatalf("%v: exit %d %s", argv, code, stderr)
		}
	}
	code, stdout, _ := runCLI(t, w, "linkage-up", "--issue", issue, "--scope", project)
	answer := decodeStdout(t, stdout).(map[string]any)
	if code != contract.ExitRefused || answer["reason"] != "bad_invocation" || answer["ok"] != false {
		t.Fatalf("exit %d %v", code, answer)
	}
}

// Test26_LNK29: the assignment view of a contested project.
func Test26_LNK29_assignment_view_of_a_contested_project(t *testing.T) {
	w := newWorld(t)
	w.scoped()
	w.contestTheProject()
	answer, err := registry.NewAssignmentView(w.r).ForIssue(w.ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	picked := contract.OrderedObject{}
	for _, key := range []string{"projectKey", "projectParentTaskId", "scopeState", "projectParentCandidates", "parentOwnsProject"} {
		picked = append(picked, contract.Field{Key: key, Value: field(answer, key)})
	}
	w.step(picked, nil)
	w.sameAsPython("lnk29_contested_assignment_view")
}

// Test26_LNK_literal_reasons: every literal reason this port emits outside errors.RefusalReason
// lookups, and the bad-argument refusals, match Python's reason and detail.
func Test26_LNK_literal_reasons(t *testing.T) {
	w := newWorld(t)
	w.step(w.r.BindScopeAs(w.ctx, "parent", project, parentEP(parent), "bogus"))
	w.step(w.r.SettleDirective(w.ctx, "dir-x", "bogus", "a", sql.NullString{}))
	w.step(w.r.SettleDirective(w.ctx, "dir-x", "chosen", "a", sql.NullString{}))
	w.step(w.r.BindScopeAs(w.ctx, "bogus", project, parentEP(parent), "active"))
	w.step(w.r.BindScopeAs(w.ctx, "parent", "A|B", parentEP(parent), "active"))
	w.step(w.r.Handover(w.ctx, "parent", project, parent, parentEP(otherParent), nil, "", "t"))
	w.step(w.r.RegisterPeer(w.ctx, project, parentEP(parent), project, parentEP(otherParent)))
	w.step(w.r.AttachIssue(w.ctx, "rel-missing", project))
	want := w.sameAsPython("lnk_literal_reasons")
	var reasons []string
	for _, s := range want {
		reasons = append(reasons, text(s["refused"].(map[string]any)["reason"]))
	}
	if !slices.Contains(reasons, "link_not_active") {
		t.Fatal("link_not_active is not exercised")
	}
}
