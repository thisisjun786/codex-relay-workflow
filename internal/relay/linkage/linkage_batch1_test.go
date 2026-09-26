package linkage

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Test26_LNK1: a binding is role + scope + task, carries the real identifiers and no title/name.
func Test26_LNK1_a_binding_is_the_role_scope_and_task(t *testing.T) {
	w := newWorld(t)
	record := w.step(w.r.BindScopeAs(w.ctx, "parent", project, parentEP(parent), "active")).(contract.OrderedObject)
	w.step(registry.BindingID("parent", "project", project, parent), nil)
	w.sameAsPython("lnk1_binding_identity")
	for _, f := range record {
		for _, forbidden := range []string{"title", "latest", "name"} {
			if f.Key != "_bindings" && strings.Contains(strings.ToLower(f.Key), forbidden) {
				t.Errorf("binding carries %s", f.Key)
			}
		}
	}
}

// Test26_LNK2: identical replays converge, sequentially and concurrently.
func Test26_LNK2_identical_replays_converge(t *testing.T) {
	w := newWorld(t)
	w.step(w.r.BindScopeAs(w.ctx, "parent", project, parentEP(parent), "active"))
	w.step(w.r.BindScopeAs(w.ctx, "parent", project, parentEP(parent), "active"))
	w.rows("SELECT binding_id FROM scope_bindings")
	s := w.supervision()
	s.project, s.parent = otherProject, parentEP(otherParent)
	w.step(w.supervise(s))
	w.step(w.supervise(s))
	w.rows("SELECT link_id FROM scope_links")
	w.sameAsPython("lnk2_replays_converge")

	// Two identical concurrent binds on independent stores: no error, one row.
	c := newWorld(t)
	results := contend(t, c.path, func(r *registry.Registry) (contract.OrderedObject, error) {
		return r.BindScopeAs(context.Background(), "parent", project, registry.Endpoint{TaskID: parent, HostID: host, Cwd: ns("/parent")}, "active")
	}, func(r *registry.Registry) (contract.OrderedObject, error) {
		return r.BindScopeAs(context.Background(), "parent", project, registry.Endpoint{TaskID: parent, HostID: host, Cwd: ns("/parent")}, "active")
	})
	for _, res := range results {
		if res.err != nil {
			t.Fatalf("an identical replay raised instead of converging: %v", res.err)
		}
	}
	if field(results[0].value, "bindingId") != field(results[1].value, "bindingId") {
		t.Fatal("the two binds disagree about the binding")
	}
	if rows := c.rowsQuiet("SELECT 1 FROM scope_bindings WHERE scope_key = ?", project); len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
}

type outcome struct {
	value contract.OrderedObject
	err   error
}

// contend runs each claim on its own Store over path, released together by a barrier (a
// WaitGroup the goroutines all wait on), and returns every outcome. Nothing waits on a clock.
func contend(t *testing.T, path string, claims ...func(*registry.Registry) (contract.OrderedObject, error)) []outcome {
	t.Helper()
	out := make([]outcome, len(claims))
	var ready, done sync.WaitGroup
	ready.Add(len(claims))
	release := make(chan struct{})
	for i, claim := range claims {
		s, err := store.Open(context.Background(), path, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		r := &registry.Registry{Store: s, Now: func() string { return fakeISO }, Policy: registry.ResolveRolePolicy(map[string]string{})}
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Done()
			<-release
			out[i].value, out[i].err = claim(r)
		}()
	}
	ready.Wait()
	close(release)
	done.Wait()
	return out
}

func (w *world) rowsQuiet(query string, args ...any) []store.Row {
	w.t.Helper()
	rows, err := w.s.All(w.ctx, query, args...)
	if err != nil {
		w.t.Fatal(err)
	}
	return rows
}

// Test26_LNK3: a link id is stable when its lower owner changes.
func Test26_LNK3_a_link_id_does_not_change_when_its_owner_does(t *testing.T) {
	w := newWorld(t)
	lid := registry.LinkID("execution", "initiative", initiative, "project", project)
	w.step(lid, nil)
	w.step(w.supervise(w.supervision()))
	w.step(w.r.Handover(w.ctx, "parent", project, parent, parentEP(otherParent), nil, "the outgoing parent handed over its project", "test"))
	link := w.step(w.r.Link(w.ctx, lid)).(contract.OrderedObject)
	w.sameAsPython("lnk3_link_id_stable_across_handover")
	if lower := field(link, "lower").(contract.OrderedObject); field(lower, "taskId") != otherParent {
		t.Fatalf("lower task %v", field(lower, "taskId"))
	}
}

// Test26_LNK4: one live owner per scope; the contest is retained once; concurrency settles
// as one owner; an archived binding is revalidated, not restored.
func Test26_LNK4_one_live_owner_per_scope(t *testing.T) {
	w := newWorld(t)
	w.step(w.r.BindScopeAs(w.ctx, "parent", project, parentEP(parent), "active"))
	for range 3 {
		w.step(w.r.BindScopeAs(w.ctx, "parent", project, parentEP(otherParent), "active"))
	}
	conflicts := w.conflicts()
	w.rows("SELECT task_id FROM scope_bindings WHERE scope_key = ?", project)
	w.sameAsPython("lnk4_one_owner_per_scope")
	if len(conflicts) != 1 || conflicts[0].Get("reason") != "duplicate_scope_owner" ||
		conflicts[0].Get("incumbent") != parent || conflicts[0].Get("challenger") != otherParent {
		t.Fatalf("conflicts %v", conflicts)
	}

	a := newWorld(t)
	a.step(a.r.BindScopeAs(a.ctx, "parent", project, parentEP(parent), "active"))
	a.exec("UPDATE scope_bindings SET status = 'archived' WHERE scope_key = ?", project)
	a.step(a.r.BindScopeAs(a.ctx, "parent", project, parentEP(otherParent), "active"))
	a.step(a.r.BindScopeAs(a.ctx, "parent", project, parentEP(parent), "active"))
	a.ownerStep("project", project)
	a.sameAsPython("lnk4_archived_is_revalidated")

	c := newWorld(t)
	claim := func(task string) func(*registry.Registry) (contract.OrderedObject, error) {
		return func(r *registry.Registry) (contract.OrderedObject, error) {
			return r.BindScopeAs(context.Background(), "parent", project, registry.Endpoint{TaskID: task, HostID: host, Cwd: ns("/parent")}, "active")
		}
	}
	results := contend(t, c.path, claim(parent), claim(otherParent))
	wins, refusals := 0, 0
	for _, res := range results {
		switch {
		case res.err == nil:
			wins++
		case reasonOf(res.err) == "duplicate_scope_owner":
			refusals++
		default:
			t.Fatalf("unexpected: %v", res.err)
		}
	}
	live := c.rowsQuiet("SELECT task_id FROM scope_bindings WHERE scope_key = ?  AND status IN ('active','paused')", project)
	if wins != 1 || refusals != 1 || len(live) != 1 {
		t.Fatalf("wins %d refusals %d live %d", wins, refusals, len(live))
	}
}

// Test26_LNK5: one task, one role, one scope (every row of the property).
func Test26_LNK5_one_task_one_role_one_scope(t *testing.T) {
	t.Run("parent_as_supervisor", func(t *testing.T) {
		w := newWorld(t)
		w.step(w.r.BindScopeAs(w.ctx, "parent", project, parentEP(parent), "active"))
		w.step(w.r.BindScopeAs(w.ctx, "supervisor", initiative, supervisorEP(parent), "active"))
		w.sameAsPython("lnk5_parent_as_supervisor")
	})
	t.Run("child_as_supervisor", func(t *testing.T) {
		w := newWorld(t)
		w.scoped()
		w.step(w.r.BindScopeAs(w.ctx, "supervisor", otherInitiative, supervisorEP(child), "active"))
		w.sameAsPython("lnk5_child_as_supervisor")
	})
	t.Run("parent_as_another_supervisor", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		w.step(w.supervise(supervision{otherInitiative, otherProject, supervisorEP(parent), parentEP(otherParent), "execution"}))
		w.sameAsPython("lnk5_parent_as_another_supervisor")
	})
	t.Run("handover_to_supervisor", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		w.step(w.r.Handover(w.ctx, "parent", project, parent, supervisorEP(supervisorTask), nil, "the supervisor tried to take the project", "test"))
		w.ownerStep("project", project)
		w.sameAsPython("lnk5_handover_to_supervisor")
	})
	t.Run("cross_role_resume", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.setStatus(rid, "cancelled")
		w.step(w.r.BindScopeAs(w.ctx, "supervisor", "INIT-LATER", bare(child), "active"))
		w.step(w.resume(rid))
		w.conflicts()
		w.sameAsPython("lnk5_cross_role_resume")
	})
	t.Run("parent_second_project", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		s := w.supervision()
		s.project = otherProject
		w.step(w.supervise(s))
		w.ownerStep("project", otherProject)
		w.sameAsPython("lnk5_parent_second_project")
	})
	t.Run("child_second_issue", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		first := w.register()
		w.must(w.r.AttachIssue(w.ctx, first, project))
		second := w.must(w.registerRaw(registry.Registration{Parent: registry.Endpoint{TaskID: parent, HostID: host, Cwd: ns("/parent")},
			Child: bare(child), IssueKey: "REL-2", ArtifactRoots: []string{root}, AllowedRecipients: []string{parent},
			DispatchRequestID: "dispatch-2", DispatchTurnID: ns("turn-2")})).(string)
		w.step(w.r.AttachIssue(w.ctx, second, project))
		w.sameAsPython("lnk5_child_second_issue")
	})
	t.Run("child_in_another_scope_resume", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.setStatus(rid, "cancelled")
		w.step(w.r.BindScopeAs(w.ctx, "child", "REL-ELSEWHERE", bare(child), "active"))
		w.step(w.resume(rid))
		x := w.must(w.r.Get(w.ctx, rid)).(registry.Relationship)
		w.step(x.Status, nil)
		w.ownerStep("issue", issue)
		w.sameAsPython("lnk5_child_in_another_scope_resume")
	})
}

// Test26_LNK6: cycles and kinds.
func Test26_LNK6_cycles_and_kinds(t *testing.T) {
	t.Run("self_supervision", func(t *testing.T) {
		w := newWorld(t)
		s := w.supervision()
		s.supervisor = supervisorEP(parent)
		w.step(w.supervise(s))
		w.conflicts()
		w.sameAsPython("lnk6_self_supervision")
	})
	t.Run("child_supervises_its_project", func(t *testing.T) {
		w := newWorld(t)
		w.scoped()
		s := w.supervision()
		s.initiative, s.supervisor, s.kind = otherInitiative, supervisorEP(child), "reference"
		w.step(w.supervise(s))
		w.sameAsPython("lnk6_child_supervises_its_project")
	})
	t.Run("issue_parent_is_its_child", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		rid := w.must(w.registerRaw(registry.Registration{Parent: bare("01same-task"), Child: bare("01same-task"), IssueKey: "REL-SELF",
			ArtifactRoots: []string{root}, AllowedRecipients: []string{parent}, DispatchRequestID: "dispatch-self", DispatchTurnID: ns("turn-self")})).(string)
		w.step(w.r.AttachIssue(w.ctx, rid, project))
		w.sameAsPython("lnk6_issue_parent_is_its_child")
	})
	t.Run("peer_kind", func(t *testing.T) {
		w := newWorld(t)
		s := w.supervision()
		s.kind = "peer"
		w.step(w.supervise(s))
		w.sameAsPython("lnk6_peer_kind")
	})
}

// Test26_LNK7: issue attachment scoping.
func Test26_LNK7_issue_attachment_scoping(t *testing.T) {
	other := supervision{otherInitiative, otherProject, supervisorEP(otherSupervisor), parentEP(otherParent), "execution"}
	t.Run("foreign_parent", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		w.must(w.supervise(other))
		rid := w.register()
		w.step(w.r.AttachIssue(w.ctx, rid, otherProject))
		w.conflicts()
		w.sameAsPython("lnk7_foreign_parent")
	})
	t.Run("unregistered_project", func(t *testing.T) {
		w := newWorld(t)
		rid := w.register()
		w.step(w.r.AttachIssue(w.ctx, rid, project))
		w.sameAsPython("lnk7_unregistered_project")
	})
	t.Run("scoped_elsewhere", func(t *testing.T) {
		w := newWorld(t)
		rid := w.scoped()
		w.must(w.supervise(other))
		w.step(w.r.AttachIssue(w.ctx, rid, otherProject))
		w.sameAsPython("lnk7_scoped_elsewhere")
	})
	t.Run("two_projects", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		o := other
		o.initiative = "INIT-2"
		w.must(w.supervise(o))
		first := w.register()
		w.must(w.r.AttachIssue(w.ctx, first, project))
		second := w.must(w.registerRaw(registry.Registration{Parent: registry.Endpoint{TaskID: parent, HostID: host, Cwd: ns("/parent")},
			Child: bare("01child-two"), IssueKey: issue, ArtifactRoots: []string{root}, AllowedRecipients: []string{parent},
			DispatchRequestID: "dispatch-rival", DispatchTurnID: ns("turn-rival"), Supersedes: first})).(string)
		w.step(w.r.AttachIssue(w.ctx, second, otherProject))
		w.rows("SELECT link_id FROM scope_links WHERE lower_kind = ? AND lower_key = ?  AND status IN ('active','paused')", "issue", issue)
		want := w.sameAsPython("lnk7_two_projects")
		if !strings.Contains(text(want[0]["refused"].(map[string]any)["detail"]), otherProject) {
			t.Fatal("the refusal does not name the other project")
		}
	})
	t.Run("inactive", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		for _, status := range []string{"archived", "cancelled"} {
			rid := w.must(w.registerRaw(registry.Registration{Parent: parentEP(parent),
				Child: registry.Endpoint{TaskID: "01child-" + status, HostID: host, Cwd: ns(root)}, IssueKey: "REL-" + status,
				ArtifactRoots: []string{root}, AllowedRecipients: []string{parent}, DispatchRequestID: "dispatch-" + status,
				DispatchTurnID: ns("turn-" + status)})).(string)
			w.setStatus(rid, status)
			w.step(w.r.AttachIssue(w.ctx, rid, project))
		}
		w.sameAsPython("lnk7_inactive")
	})
}

// Test26_LNK8: attaching writes scope row + child binding + edge, converges, completes a
// partial attachment and keeps a paused assignment paused.
func Test26_LNK8_attaching_writes_converges_and_keeps_paused(t *testing.T) {
	t.Run("writes_and_converges", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		rid := w.register()
		w.step(w.r.AttachIssue(w.ctx, rid, project))
		w.step(w.r.AttachIssue(w.ctx, rid, project))
		w.rows("SELECT * FROM relationship_scope")
		w.rows("SELECT link_id FROM scope_links WHERE lower_kind = ?", "issue")
		w.sameAsPython("lnk8_attach_writes_and_converges")
	})
	t.Run("completes_partial", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		rid := w.register()
		w.exec("INSERT INTO relationship_scope (relationship_id, project_key, recorded_at) VALUES (?,?,?)", rid, project, fakeISO)
		w.ownerStep("issue", issue)
		w.step(w.r.AttachIssue(w.ctx, rid, project))
		w.step(w.r.Link(w.ctx, registry.LinkID("execution", "project", project, "issue", issue)))
		w.sameAsPython("lnk8_completes_partial")
	})
	t.Run("paused_stays_paused", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		rid := w.register()
		w.setStatus(rid, "paused")
		w.step(w.r.AttachIssue(w.ctx, rid, project))
		w.ownerStep("issue", issue)
		w.sameAsPython("lnk8_paused_stays_paused")
	})
}

// Test26_LNK9: a shared project.
func Test26_LNK9_a_shared_project(t *testing.T) {
	reference := supervision{otherInitiative, project, supervisorEP(otherSupervisor), parentEP(parent), "reference"}
	t.Run("second_initiative_references", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		w.step(w.supervise(reference))
		w.rows("SELECT task_id FROM scope_bindings WHERE scope_kind = ? AND scope_key = ?  AND role = ? AND status IN ('active','paused')", "project", project, "parent")
		w.sameAsPython("lnk9_second_initiative_references")
	})
	t.Run("second_execution_edge", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		s := reference
		s.kind = "execution"
		w.step(w.supervise(s))
		w.rows("SELECT link_id FROM scope_links WHERE lower_key = ? AND link_kind = 'execution'  AND status IN ('active','paused')", project)
		w.sameAsPython("lnk9_second_execution_edge")
	})
	t.Run("reference_naming_another_parent", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		s := reference
		s.parent = parentEP(otherParent)
		w.step(w.supervise(s))
		w.conflicts()
		w.sameAsPython("lnk9_reference_naming_another_parent")
	})
	t.Run("supervisor_cannot_reference", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		s := w.supervision()
		s.kind = "reference"
		w.step(w.supervise(s))
		w.rows("SELECT link_id FROM scope_links WHERE lower_key = ? AND status IN ('active','paused')", project)
		w.step(w.supervise(reference))
		w.sameAsPython("lnk9_supervisor_cannot_reference")
	})
	t.Run("reference_not_first", func(t *testing.T) {
		w := newWorld(t)
		w.step(w.supervise(supervision{"INIT-3", "PROJ-3", supervisorEP("01supervisor-three"), parentEP("01parent-three"), "reference"}))
		w.ownerStep("project", "PROJ-3")
		w.sameAsPython("lnk9_reference_not_first")
	})
}

func (w *world) record(origin, task, link, digest, kind, key string) any {
	return w.step(w.r.RecordDirective(w.ctx, kind, key, task, origin, link, digest, nullString()))
}

// Test26_LNK10: only the execution link's upper endpoint that owns its scope may instruct.
func Test26_LNK10_directive_authority(t *testing.T) {
	t.Run("authority", func(t *testing.T) {
		w := newWorld(t)
		execution := field(w.superviseDefault(), "linkId").(string)
		ref := field(w.must(w.supervise(supervision{otherInitiative, project, supervisorEP(otherSupervisor), parentEP(parent), "reference"})).(contract.OrderedObject), "linkId").(string)
		first := w.record(initiative, supervisorTask, execution, "d-one", "project", project).(contract.OrderedObject)
		w.record(otherInitiative, otherSupervisor, ref, "d-two", "project", project)
		w.record(initiative, otherSupervisor, execution, "d-three", "project", project)
		w.must(w.supervise(supervision{"INIT-3", "PROJ-3", supervisorEP("01supervisor-three"), parentEP("01parent-three"), "execution"}))
		other := registry.LinkID("execution", "initiative", "INIT-3", "project", "PROJ-3")
		w.record(initiative, supervisorTask, other, "d-four", "project", project)
		w.record(initiative, "01nobody", execution, "d-one", "project", project)
		w.record(initiative, supervisorTask, execution, "d-kind", "issue", project)
		w.conflicts()
		w.sameAsPython("lnk10_directive_authority")
		if field(first, "linkKind") != "execution" {
			t.Fatal("the first directive is not an execution one")
		}
	})
	t.Run("upper_endpoint", func(t *testing.T) {
		w := newWorld(t)
		w.superviseDefault()
		w.must(w.supervise(supervision{"INIT-2", otherProject, supervisorEP(otherSupervisor), parentEP(otherParent), "execution"}))
		other := registry.LinkID("execution", "initiative", "INIT-2", "project", otherProject)
		w.record(initiative, supervisorTask, other, "d-two", "project", project)
		w.sameAsPython("lnk10_upper_endpoint")
	})
}
