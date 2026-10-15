package registry

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1446 (bundle CRW-1422): the registry's refusals name the session layers. The relay role id
// `supervisor` is the 부모 and `parent` is the 자식 (plugins/crw/skills/crw-canon/references/hierarchy.md
// "The levels"), so a refusal a person reads writes 부모 or 자식 in running text, glosses a role id it
// shows as a value ('parent' (자식)), and never leaves a bare role word layername.Find counts. The role
// ids themselves (codes, fields, flag values) are unchanged.

// layerWordsPolicy declares every role, the 자식 and the issue child on one pair.
const layerWordsPolicy = `{"roles": {"supervisor": {"expectation": "record"}, "parent": {"model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh"}, "child": {"model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh"}}}`

// layerWordsPolicyWithout declares every role but one.
func layerWordsPolicyWithout(role string) string {
	roles := []string{}
	for _, r := range []string{"supervisor", "parent", "child"} {
		if r == role {
			continue
		}
		if r == "supervisor" {
			roles = append(roles, `"supervisor": {"expectation": "record"}`)
			continue
		}
		roles = append(roles, `"`+r+`": {"model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh"}`)
	}
	return `{"roles": {` + strings.Join(roles, ", ") + `}}`
}

func layerWordsSettings(t *testing.T) contract.OrderedObject {
	t.Helper()
	return mcpDecoded(t, `{"sandbox":{"type":"readOnly","networkAccess":false},"approvalPolicy":"never","cwd":"/w","runtimeWorkspaceRoots":["/w"],"model":"anthropic/claude-opus-5-5","reasoningEffort":"xhigh","environments":[{"environmentId":"local","cwd":"/w","runtimeWorkspaceRoots":["/w"]}]}`)
}

func layerWordsEndpoint(task string) Endpoint {
	return Endpoint{task, host, ns("/w"), ns("cxc-" + task)}
}

// layerWordsBind writes a live binding straight into the store, for the corrupt or contested states a
// refusal exists for and the writing paths would never make.
func layerWordsBind(t *testing.T, r *Registry, id, role, kind, key, task string) {
	t.Helper()
	if _, err := r.Store.DB.ExecContext(ctx(), "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, status, revision, created_at, updated_at)"+
		" VALUES (?, ?, ?, ?, ?, ?, 'active', 1, ?, ?)", id, role, kind, key, task, host, fakeISO, fakeISO); err != nil {
		t.Fatal(err)
	}
}

// layerWordsText asserts text holds every wanted piece and no bare role word.
func layerWordsText(t *testing.T, text string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("text does not hold %q:\n%s", w, text)
		}
	}
	if hits := layername.Find(text); len(hits) > 0 {
		t.Errorf("text names a role id where a person reads a layer (write 부모 / 자식 / supervisor (부모)): %v\n%s", hits, text)
	}
}

// layerWordsRefusal is the detail of a refusal err, failing when err is no refusal.
func layerWordsRefusal(t *testing.T, err error) string {
	t.Helper()
	var refused *store.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("want a refusal, got %v", err)
	}
	return refused.Detail
}

func TestRegistryRefusalTextNamesLayers(t *testing.T) {
	t.Parallel()

	t.Run("registry.go a second assignment of the issue names its 자식", func(t *testing.T) {
		r := newRegistry(t)
		if _, err := r.Register(ctx(), fixture()); err != nil {
			t.Fatal(err)
		}
		second := fixture()
		second.Child = Endpoint{"child-2", host, ns(root), ns("cxc-child-2")}
		second.DispatchRequestID = "dispatch-2"
		_, err := r.Register(ctx(), second)
		layerWordsText(t, layerWordsRefusal(t, err), ", 자식 \""+parent+"\"); reuse that assignment")
	})

	t.Run("linkage.go an unregistered project has no registered 자식", func(t *testing.T) {
		r := newRegistry(t)
		layerWordsText(t, layerWordsRefusal(t, r.PrecheckScope(ctx(), parent, issue, "P1")),
			"project 'P1' has no registered 자식, so an issue cannot be attached to it yet")
	})

	t.Run("linkage.go an issue under another project's 자식", func(t *testing.T) {
		r := newRegistry(t)
		bindProject(t, r, "P1", "other-parent")
		layerWordsText(t, layerWordsRefusal(t, r.PrecheckScope(ctx(), parent, issue, "P1")),
			"issue '"+issue+"' is assigned under 자식 '"+parent+"', but project 'P1' is executed by 'other-parent'")
	})

	t.Run("linkage.go a task bound at another role names both layers", func(t *testing.T) {
		r := newRegistry(t)
		bindProject(t, r, "P1", "T1")
		_, err := r.BindScope(ctx(), "supervisor", "I1", layerWordsEndpoint("T1"))
		layerWordsText(t, layerWordsRefusal(t, err), "task 'T1' is already the 자식 of project 'P1', so it cannot also be a 부모")
	})

	t.Run("linkage.go a task bound at another level needs its own 자식", func(t *testing.T) {
		r := newRegistry(t)
		bindProject(t, r, "P1", "T1")
		_, err := r.BindScope(ctx(), "parent", "P2", layerWordsEndpoint("T1"))
		layerWordsText(t, layerWordsRefusal(t, err),
			"task 'T1' is already the 자식 of project 'P1'; one task is bound to one tracking level, so a second project needs its own 자식")
	})

	t.Run("linkage.go and rolepolicy.go an unknown role lists the roles typed", func(t *testing.T) {
		r := newRegistry(t)
		_, err := r.BindScope(ctx(), "bogus", "K1", layerWordsEndpoint("T1"))
		layerWordsText(t, layerWordsRefusal(t, err), "a role is one of child (issue child), parent (자식), supervisor (부모), not \"bogus\"")
		_, err = bindingScopeKind("bogus", "", "")
		layerWordsText(t, layerWordsRefusal(t, err), "a role is one of child (issue child), parent (자식), supervisor (부모), not \"bogus\"")
	})

	t.Run("rolepolicy.go a scope kind the role does not take", func(t *testing.T) {
		_, err := bindingScopeKind("child", "store", "store")
		layerWordsText(t, layerWordsRefusal(t, err),
			"--scope-kind 'store' is not a scope a 'child' (issue child) takes; the store seat is the 부모's and goes with --scope store, and a management seat is the 부모's and goes with --scope <seat id>")
	})

	t.Run("linkage.go a restore under a project whose 자식 changed", func(t *testing.T) {
		r := newRegistry(t)
		bindProject(t, r, "P1", parent)
		in := fixture()
		in.ProjectKey = "P1"
		rel, err := r.Register(ctx(), in)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.SetStatus(ctx(), rel.ID, "paused", "test"); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Store.DB.ExecContext(ctx(), "UPDATE scope_bindings SET task_id = 'other-parent' WHERE scope_kind = 'project' AND scope_key = 'P1'"); err != nil {
			t.Fatal(err)
		}
		_, err = r.Resume(ctx(), rel.ID, 1, []string{root}, []string{parent}, "test")
		layerWordsText(t, layerWordsRefusal(t, err),
			"the 자식 of project 'P1' is now 'other-parent', not '"+parent+"', so restoring '"+rel.ID+
				"' would reattach its issue under an owner the project no longer has; re-register the assignment under the current 자식 instead")
	})

	t.Run("linkage.go a restore of a child that has since taken a role", func(t *testing.T) {
		r := newRegistry(t)
		bindProject(t, r, "P1", parent)
		in := fixture()
		in.ProjectKey = "P1"
		rel, err := r.Register(ctx(), in)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.SetStatus(ctx(), rel.ID, "paused", "test"); err != nil {
			t.Fatal(err)
		}
		layerWordsBind(t, r, "child-as-parent", "parent", "project", "P2", child)
		_, err = r.Resume(ctx(), rel.ID, 1, []string{root}, []string{parent}, "test")
		layerWordsText(t, layerWordsRefusal(t, err), "task '"+child+"' has since become the parent (자식) of project 'P2', so it cannot be restored as the child of issue")
	})

	t.Run("record.go and cli.go a contested task shows its roles glossed", func(t *testing.T) {
		r := newRegistry(t)
		layerWordsBind(t, r, "b-parent", "parent", "project", "P1", "T1")
		layerWordsBind(t, r, "b-child", "child", "issue", "ISSUE-1", "T1")
		_, err := r.RecordSettings(ctx(), "T1", layerWordsSettings(t), "creation_result", "", Citation{})
		layerWordsText(t, layerWordsRefusal(t, err), "'T1' holds live bindings at ['child', 'parent'] (issue child, 자식); one task holds one role")
		_, _, err = CheckBoundRole(ctx(), r.Store, "T1", layerWordsSettings(t), r.Policy)
		layerWordsText(t, layerWordsRefusal(t, err), "'T1' holds live bindings at ['child', 'parent'] (issue child, 자식), and one task holds one role")
		err = r.refuseRoleDisagreement(ctx(), []settingsWrite{{task: "T1", values: layerWordsSettings(t), role: "child"}})
		layerWordsText(t, layerWordsRefusal(t, err), "\"T1\" holds live bindings at [\"child\",\"parent\"] (issue child, 자식); one task holds one role")
	})

	t.Run("record.go a later record states another role", func(t *testing.T) {
		r := newRegistry(t)
		if _, err := r.RecordSettings(ctx(), "T1", layerWordsSettings(t), "creation_result", "parent", Citation{}); err != nil {
			t.Fatal(err)
		}
		_, err := r.RecordSettings(ctx(), "T1", layerWordsSettings(t), "creation_result", "child", Citation{})
		layerWordsText(t, layerWordsRefusal(t, err), "'T1' was created citing role 'parent' (자식) and this record states 'child' (issue child).")
	})

	t.Run("record.go a bound task with no policy to read", func(t *testing.T) {
		r := newRegistry(t)
		bindProject(t, r, "P1", "T1")
		_, _, err := CheckBoundRole(ctx(), r.Store, "T1", layerWordsSettings(t), r.Policy)
		layerWordsText(t, layerWordsRefusal(t, err), "'T1' is bound as 'parent' (자식) and this process cannot read a role policy")
	})

	t.Run("record.go and rolepolicy.go a bound role the policy does not declare", func(t *testing.T) {
		x := newRoleRun(t)
		x.policy(layerWordsPolicyWithout("parent"))
		bindProject(t, x.r, "P1", "T1")
		_, _, err := CheckBoundRole(ctx(), x.r.Store, "T1", layerWordsSettings(t), x.r.Policy)
		layerWordsText(t, layerWordsRefusal(t, err), "'T1' is bound as 'parent' (자식): ", "declare role 'parent' (자식) in this host's execution policy")
	})

	t.Run("rolepolicy.go a task created citing one role and bound as another", func(t *testing.T) {
		want := "this task was created citing role 'parent' (자식) and is being bound as 'child' (issue child); "
		x := newRoleRun(t)
		x.policy(layerWordsPolicy)
		if _, err := x.r.BindScope(ctx(), "child", "ISSUE-1", layerWordsEndpoint("T1")); err != nil {
			t.Fatal(err)
		}
		_, err := x.r.RecordSettings(ctx(), "T1", layerWordsSettings(t), "creation_result", "parent", Citation{})
		layerWordsText(t, layerWordsRefusal(t, err), want)
		if _, err := x.r.RecordSettings(ctx(), "T2", layerWordsSettings(t), "creation_result", "parent", Citation{}); err != nil {
			t.Fatal(err)
		}
		_, err = x.r.BindScope(ctx(), "child", "ISSUE-2", layerWordsEndpoint("T2"))
		layerWordsText(t, layerWordsRefusal(t, err), want)
		// A cited role that is not a string is shown as the value it is, with no layer name.
		finding := CheckBinding(int64(7), "child", layerWordsSettings(t), x.r.Policy)
		layerWordsText(t, findingText(finding, "detail"), "created citing role 7 and is being bound as 'child' (issue child); ")
	})

	t.Run("rolepolicy.go the finding prose glosses the role", func(t *testing.T) {
		x := newRoleRun(t)
		x.policy(layerWordsPolicyWithout("supervisor"))
		finding := CheckBinding(nil, "supervisor", layerWordsSettings(t), x.r.Policy)
		layerWordsText(t, findingText(finding, "detail"), "this host's execution policy declares no role 'supervisor' (부모), so a task cannot be bound")
		if got, _ := finding.Lookup("boundRole"); got != "supervisor" {
			t.Errorf("boundRole is %v, want the role id", got)
		}
		x.policy(layerWordsPolicy)
		finding = CheckBinding(nil, "parent", pairSettings("anthropic/claude-sonnet-5-5", "xhigh"), x.r.Policy)
		layerWordsText(t, findingText(finding, "detail"), "this task's recorded pair is not the pair role 'parent' (자식) runs on")
		cited := layerWordsSettings(t).Set("citedException", "exc-1")
		finding = CheckRecord(cited, "supervisor", x.r.Policy)
		layerWordsText(t, findingText(finding, "detail"), "authorize that id for role 'supervisor' (부모) with this pair and directory")
		if got, _ := finding.Lookup("role"); got != "supervisor" {
			t.Errorf("role is %v, want the role id", got)
		}
		x.policy(layerWordsPolicyWithout("parent"))
		finding = CheckRecord(layerWordsSettings(t), "parent", x.r.Policy)
		layerWordsText(t, findingText(finding, "recovery"), "declare role 'parent' (자식) in this host's execution policy")
	})

	t.Run("linkage_write.go the supervision refusals name the edge and its 부모", func(t *testing.T) {
		r := newRegistry(t)
		_, err := r.RegisterSupervision(ctx(), "I1", "P1", layerWordsEndpoint("S1"), layerWordsEndpoint("T1"), "bogus")
		layerWordsText(t, layerWordsRefusal(t, err),
			"the link linkage-supervise registers is execution or reference, not \"bogus\"; a peer link is registered with register_peer")
		_, err = r.RegisterSupervision(ctx(), "I1", "P1", layerWordsEndpoint(""), layerWordsEndpoint("T1"), linkExec)
		layerWordsText(t, layerWordsRefusal(t, err), "the supervisor (부모)'s task id must be a non-empty string")
		_, err = r.RegisterSupervision(ctx(), "I1", "P1", layerWordsEndpoint("S1"), Endpoint{"T1", "", ns("/w"), ns("cxc-T1")}, linkExec)
		layerWordsText(t, layerWordsRefusal(t, err), "the parent (자식)'s host id must be a non-empty string")
		_, err = r.RegisterSupervision(ctx(), "I1", "P1", layerWordsEndpoint("T1"), layerWordsEndpoint("T1"), linkExec)
		layerWordsText(t, layerWordsRefusal(t, err), "task 'T1' cannot be both the 부모 and the 자식")
		_, err = r.RegisterSupervision(ctx(), "I1", "P1", layerWordsEndpoint("S1"), layerWordsEndpoint("T1"), linkReference)
		layerWordsText(t, layerWordsRefusal(t, err),
			"project 'P1' has no execution 부모 yet, so there is no outcome to reference. Register its execution edge first")
	})

	t.Run("linkage_write.go a 부모 that already owns a scope below the project", func(t *testing.T) {
		r := newRegistry(t)
		bindProject(t, r, "P1", "S1")
		_, err := r.RegisterSupervision(ctx(), "I1", "P1", layerWordsEndpoint("S1"), layerWordsEndpoint("T1"), linkExec)
		layerWordsText(t, layerWordsRefusal(t, err),
			"task 'S1' already owns a scope below project 'P1', so making it that project's 부모 would close a loop")
	})

	t.Run("linkage_write.go a project that already has its execution 부모", func(t *testing.T) {
		r := newRegistry(t)
		if _, err := r.RegisterSupervision(ctx(), "I1", "P1", layerWordsEndpoint("S1"), layerWordsEndpoint("T1"), linkExec); err != nil {
			t.Fatal(err)
		}
		lid := linkID(linkExec, scopeInitiative, "I1", scopeProject, "P1")
		_, err := r.RegisterSupervision(ctx(), "I2", "P1", layerWordsEndpoint("S2"), layerWordsEndpoint("T1"), linkExec)
		layerWordsText(t, layerWordsRefusal(t, err),
			"project 'P1' already has its execution 부모 under "+lid+" from initiative 'I1'; another initiative references it instead of taking a second execution edge")
		_, err = r.RegisterSupervision(ctx(), "I1", "P1", layerWordsEndpoint("S1"), layerWordsEndpoint("T1"), linkReference)
		layerWordsText(t, layerWordsRefusal(t, err),
			"initiative 'I1' already holds the execution edge over project 'P1' under "+lid+
				", so it cannot also reference it; a reference is how a DIFFERENT initiative reads this outcome")
		_, err = r.RegisterSupervision(ctx(), "I2", "P1", layerWordsEndpoint("S2"), layerWordsEndpoint("T9"), linkReference)
		layerWordsText(t, layerWordsRefusal(t, err),
			"project 'P1' is executed by 자식 'T1', so a reference naming 'T9' would clone its execution 자식")
	})

	t.Run("linkage_write.go a project whose execution 부모 is a management seat", func(t *testing.T) {
		r := newRegistry(t)
		lid := linkID(linkExec, scopeManagement, "seat-1", scopeProject, "P1")
		if _, err := r.Store.DB.ExecContext(ctx(), "INSERT INTO scope_links (link_id, link_kind, upper_kind, upper_key, upper_task_id, lower_kind, lower_key, lower_task_id, status, revision, superseded_by, created_at, updated_at)"+
			" VALUES (?, 'execution', 'management', 'seat-1', 'S1', 'project', 'P1', 'T1', 'active', 1, NULL, ?, ?)", lid, fakeISO, fakeISO); err != nil {
			t.Fatal(err)
		}
		_, err := r.RegisterSupervision(ctx(), "I1", "P1", layerWordsEndpoint("S2"), layerWordsEndpoint("T1"), linkExec)
		layerWordsText(t, layerWordsRefusal(t, err),
			"project 'P1' already has its execution 부모 under "+lid+" from management seat 'seat-1' (a management session bound its owner); "+
				"a project has one upward execution edge, so an initiative cannot take another. "+
				"A legacy initiative over the project is registered with linkage-supervise before the project owner's binding")
	})

	t.Run("linkage_write.go a project with two live execution edges", func(t *testing.T) {
		r := newRegistry(t)
		for _, initiative := range []string{"I1", "I2"} {
			lid := linkID(linkExec, scopeInitiative, initiative, scopeProject, "P1")
			if _, err := r.Store.DB.ExecContext(ctx(), "INSERT INTO scope_links (link_id, link_kind, upper_kind, upper_key, upper_task_id, lower_kind, lower_key, lower_task_id, status, revision, superseded_by, created_at, updated_at)"+
				" VALUES (?, 'execution', 'initiative', ?, 'S-'||?, 'project', 'P1', 'T1', 'active', 1, NULL, ?, ?)", lid, initiative, initiative, fakeISO, fakeISO); err != nil {
				t.Fatal(err)
			}
		}
		_, err := r.RegisterSupervision(ctx(), "I3", "P1", layerWordsEndpoint("S3"), layerWordsEndpoint("T1"), linkExec)
		layerWordsText(t, layerWordsRefusal(t, err),
			"project 'P1' already has more than one live execution edge (", "), so there is no single edge to register beside. "+
				"The reading paths report this as competing_parents; repair the store rather than adding to it")
	})

	t.Run("linkage_write.go a peer link joins two project 자식 sessions", func(t *testing.T) {
		r := newRegistry(t)
		_, err := r.RegisterPeer(ctx(), "P1", layerWordsEndpoint(""), "P2", layerWordsEndpoint("T2"))
		layerWordsText(t, layerWordsRefusal(t, err), "the left 자식's task id must be a non-empty string")
		_, err = r.RegisterPeer(ctx(), "P1", layerWordsEndpoint("T1"), "P2", Endpoint{"T2", "", ns("/w"), ns("cxc-T2")})
		layerWordsText(t, layerWordsRefusal(t, err), "the right 자식's host id must be a non-empty string")
		_, err = r.RegisterPeer(ctx(), "P1", layerWordsEndpoint("T1"), "P2", layerWordsEndpoint("T2"))
		layerWordsText(t, layerWordsRefusal(t, err),
			"task 'T1' is not the registered 자식 of project 'P1', which has no registered 자식; a peer link joins two project 자식 sessions")
		bindProject(t, r, "P1", "other-parent")
		_, err = r.RegisterPeer(ctx(), "P1", layerWordsEndpoint("T1"), "P2", layerWordsEndpoint("T2"))
		layerWordsText(t, layerWordsRefusal(t, err),
			"task 'T1' is not the registered 자식 of project 'P1', which is held by 'other-parent'; a peer link joins two project 자식 sessions")
	})

	t.Run("linkage_cli.go the project reading names the 자식", func(t *testing.T) {
		layerWordsText(t, projectReadingLimits, "integration and verification are the 자식's judgment")
		r := newRegistry(t)
		layerWordsBind(t, r, "owner-a", "parent", "project", "P1", "owner-a")
		if _, err := r.Store.DB.ExecContext(ctx(), "DROP INDEX scope_bindings_one_live_owner"); err != nil {
			t.Fatal(err)
		}
		layerWordsBind(t, r, "owner-b", "parent", "project", "P1", "owner-b")
		reading := fmt.Sprint(r.ProjectState(ctx(), "P1"))
		if !strings.Contains(reading, "which 자식 this reading is about is not decided here") {
			t.Errorf("the ambiguous reading does not name the 자식: %s", reading)
		}
	})
}
