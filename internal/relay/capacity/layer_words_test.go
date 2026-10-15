package capacity

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// CRW-1446: the refusals and warnings capacity writes for a person name the layer, 부모 for the relay
// role id supervisor and 자식 for the relay role id parent, and carry no bare role word
// (layername.Find, the matcher the ci ratchet shares). The refusal codes and the field names stay.

// namesLayers fails the test unless text carries want and layername.Find counts no bare role word in it.
func namesLayers(t *testing.T, what, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Errorf("%s %q does not carry %q", what, text, want)
	}
	if hits := layername.Find(text); len(hits) != 0 {
		t.Errorf("%s %q names a role id where a layer is read: %v", what, text, hits)
	}
}

// refusedDetail is the detail of a refusal, or a fatal when err is no refusal with reason.
func refusedDetail(t *testing.T, err error, reason contract.RefusalReason) string {
	t.Helper()
	if got := reasonOf(err); got != string(reason) {
		t.Fatalf("reason %q, want %s (%v)", got, reason, err)
	}
	return storeDeclarerRefusalDetail(t, err)
}

func TestCapacityRefusalTextNamesLayers(t *testing.T) {
	t.Run("store declarer refusal", func(t *testing.T) {
		e := newEnv(t)
		_, err := e.cap.DeclareLimit(ctx(), Limit{ScopeKind: "store", ScopeKey: "store", Dimension: "runs",
			Unit: "runs", Ceiling: 40, DeclaredBy: supervisor, Source: "operator", Enforce: true})
		namesLayers(t, "refusal", refusedDetail(t, err, contract.RefusalScopeRoleMismatch),
			`task "task-supervisor" holds no live store-scope 부모 binding, and the store scope has no owner of its own to speak for it`)
	})

	t.Run("store declarer warning without a holder", func(t *testing.T) {
		e := newEnv(t)
		bindManagementSeat(t, e, "01loneManager")
		limit := storeSeatLimit()
		limit.DeclaredBy = "01loneManager"
		answer, err := e.cap.DeclareLimit(ctx(), limit)
		if err != nil {
			t.Fatal(err)
		}
		warning, _ := field(answer, "declarerWarning").(contract.OrderedObject)
		detail, _ := field(warning, "detail").(string)
		namesLayers(t, "warning", detail, "the store has no store-scope 부모 (no live store seat, and none was laid down for a management seat)")
	})

	t.Run("store declarer warning naming the holder", func(t *testing.T) {
		e := newEnv(t)
		bindStoreSeat(t, e)
		bindManagementSeat(t, e, storeSeatTask)
		bindManagementSeat(t, e, "01otherManager")
		limit := storeSeatLimit()
		limit.DeclaredBy = "01otherManager"
		answer, err := e.cap.DeclareLimit(ctx(), limit)
		if err != nil {
			t.Fatal(err)
		}
		warning, _ := field(answer, "declarerWarning").(contract.OrderedObject)
		detail, _ := field(warning, "detail").(string)
		namesLayers(t, "warning", detail, `the store-scope 부모 is "01management"`)
	})

	t.Run("a bound stated by a task that holds no seat names the seat's layer", func(t *testing.T) {
		e := newEnv(t)
		for _, c := range []struct{ kind, key, want string }{
			{"project", projectA, `task "task-stranger" is not the registered 자식 of project "PRJ-A", which is held by "task-alpha"`},
			{"initiative", "INIT-1", `task "task-stranger" is not the registered 부모 of initiative "INIT-1", which is held by "task-supervisor"`},
		} {
			_, err := e.cap.DeclareLimit(ctx(), Limit{ScopeKind: c.kind, ScopeKey: c.key, Dimension: "runs", Unit: "runs",
				Ceiling: 99, DeclaredBy: "task-stranger", Source: "operator", Enforce: true})
			namesLayers(t, c.kind+" refusal", refusedDetail(t, err, contract.RefusalScopeRoleMismatch), c.want)
		}
	})

	t.Run("a reservation names the project's 자식", func(t *testing.T) {
		e := newEnv(t)
		reserve := func(subject, task, project string) error {
			_, err := e.cap.Reserve(ctx(), Reservation{SubjectKind: assignment, SubjectKey: subject, ParentTask: task, Project: project, ReservedBy: task})
			return err
		}
		namesLayers(t, "refusal", refusedDetail(t, reserve("REL-2", beta, projectA), contract.RefusalScopeRoleMismatch),
			"task 'task-beta' is not the registered 자식 of project 'PRJ-A', which is held by 'task-alpha'")
		namesLayers(t, "refusal", refusedDetail(t, reserve("REL-3", alpha, "PRJ-NONE"), contract.RefusalUnregisteredScope),
			"project 'PRJ-NONE' has no registered 자식")
		// The guard index normally keeps a second live owner out; a store that holds one anyway is
		// what the duplicate_scope_owner refusal answers.
		if _, err := e.store.DB.ExecContext(ctx(), "DROP INDEX scope_bindings_one_live_owner"); err != nil {
			t.Fatal(err)
		}
		if err := storeseed.InsertScopeBinding(ctx(), e.store, store.ScopeBindingsRow{BindingID: "bind-second-parent", Role: "parent",
			ScopeKind: "project", ScopeKey: projectA, TaskID: "task-gamma", HostID: "host-g", Status: "active", Revision: 1,
			CreatedAt: e.clock.iso(), UpdatedAt: e.clock.iso()}); err != nil {
			t.Fatal(err)
		}
		namesLayers(t, "refusal", refusedDetail(t, reserve("REL-4", alpha, projectA), contract.RefusalDuplicateScopeOwner),
			"project 'PRJ-A' has more than one live 자식 ('task-alpha', 'task-gamma'), so there is no owner to reserve under")
	})

	t.Run("edit region refusals name the 자식", func(t *testing.T) {
		e := newRegionEnv(t)
		_, err := e.regions.Propose(ctx(), Proposal{Repository: repo, BaseRevision: rev, Path: "src/a.py", RegionKind: "file",
			RegionClass: "source", LeftProject: "PRJ-A", RightProject: "PRJ-B", PeerLinkID: e.pair, ProposerTaskID: zeta,
			ConstraintText: "keep the public signature", IssueKey: ns("CRW-1"), NextOwner: ns(beta)})
		namesLayers(t, "proposal refusal", refusedDetail(t, err, contract.RefusalScopeRoleMismatch),
			`task "task-zeta" is the registered 자식 of neither "PRJ-A" nor "PRJ-B", and a proposal pre-accepts its own side`)
		record := e.propose("src/a.py", proposeOpt{})
		_, err = e.regions.Settle(ctx(), id(record), zeta, "accepted", null(), null())
		namesLayers(t, "settlement refusal", refusedDetail(t, err, contract.RefusalScopeRoleMismatch),
			"task 'task-zeta' is the registered 자식 of neither 'PRJ-A' nor 'PRJ-B'")
		_, err = e.regions.RestateRevision(ctx(), repo, rev, "rev-2", zeta)
		namesLayers(t, "restatement refusal", refusedDetail(t, err, contract.RefusalScopeRoleMismatch),
			`task "task-zeta" is the registered 자식 of no project holding an agreement in "owner/repo"`)
	})

	t.Run("a carried agreement with no single 자식 says who takes it", func(t *testing.T) {
		e := newRegionEnv(t)
		original := e.byBeta()
		e.moved("rev-2")
		successor := e.reaffirm(original, alpha, "rev-2", null())
		if _, err := e.store.DB.ExecContext(ctx(), "UPDATE scope_bindings SET status = 'archived' WHERE scope_key = 'PRJ-B'"); err != nil {
			t.Fatal(err)
		}
		e.agreement(successor)
		reaffirmation, _ := e.ok(len(e.steps) - 1)["reaffirmation"].(map[string]any)
		awaiting, _ := reaffirmation["awaitingAcceptance"].(map[string]any)
		precondition, _ := awaiting["precondition"].(string)
		namesLayers(t, "precondition", precondition, `"PRJ-B" has no single registered 자식; the 자식 that takes it runs region-settle --agreement `)
	})
}
