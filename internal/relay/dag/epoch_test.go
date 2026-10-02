package dag

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// The fence (epoch.go): what a write that names an epoch is refused for, judged on the real store through the real Put.

func parentBinding(t *testing.T, s *store.Store, id, task, project, status string) {
	t.Helper()
	if err := storeseed.InsertScopeBinding(context.Background(), s, store.ScopeBindingsRow{BindingID: id, Role: "parent", ScopeKind: "project", ScopeKey: project, TaskID: task, HostID: "host",
		Status: status, Revision: 1, CreatedAt: "2026-10-02T00:00:00.000000+00:00", UpdatedAt: "2026-10-02T00:00:00.000000+00:00"}); err != nil {
		t.Fatal(err)
	}
}

func claimRow(t *testing.T, s *store.Store, plan string, epoch int64, binding, task, nonce string) {
	t.Helper()
	if _, err := s.DB.Exec("INSERT INTO dag_coordinator_claims (plan_id, epoch, binding_id, binding_revision, task_id, session_nonce, claimed_at) VALUES (?,?,?,?,?,?,?)",
		plan, epoch, binding, 1, task, nonce, "2026-10-02T00:00:00.000000+00:00"); err != nil {
		t.Fatal(err)
	}
}

// epochDoc is a revision document of the shared test plan shape, written by task as a session that holds epoch.
func epochDoc(plan, request string, parent int, task string, epoch int64, project string, changes ...doc) doc {
	d := revDoc(plan, request, parent, changes...)
	d["author_task_id"], d["project_key"] = task, project
	if epoch != 0 {
		d["coordinator_epoch"] = epoch
	}
	return d
}

func isStale(err error) bool {
	var refused *store.RefusedError
	return errors.As(err, &refused) && refused.Reason == "stale_coordinator_epoch"
}

// A write is judged against the newest claim, and a refusal writes nothing at all: not a revision, not a header, not a row of any other table of the zone.
func TestTheFenceRules(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, s *store.Store)
		task  string
		epoch int64
		stale bool
	}{
		{name: "a plan nobody claimed takes a write that names no epoch", task: "t1"},
		{name: "a plan nobody claimed refuses a write that names an epoch", task: "t1", epoch: 1, stale: true},
		{name: "the newest claim, by the actor", task: "t1", epoch: 1, setup: func(t *testing.T, s *store.Store) {
			parentBinding(t, s, "b1", "t1", "P-TEST", "active")
			claimRow(t, s, "plan", 1, "b1", "t1", "n1")
		}},
		{name: "a claim exists and the write names no epoch", task: "t1", stale: true, setup: func(t *testing.T, s *store.Store) {
			parentBinding(t, s, "b1", "t1", "P-TEST", "active")
			claimRow(t, s, "plan", 1, "b1", "t1", "n1")
		}},
		{name: "an older session of the same task", task: "t1", epoch: 1, stale: true, setup: func(t *testing.T, s *store.Store) {
			parentBinding(t, s, "b1", "t1", "P-TEST", "active")
			claimRow(t, s, "plan", 1, "b1", "t1", "n1")
			claimRow(t, s, "plan", 2, "b1", "t1", "n2")
		}},
		{name: "the newest session of the same task", task: "t1", epoch: 2, setup: func(t *testing.T, s *store.Store) {
			parentBinding(t, s, "b1", "t1", "P-TEST", "active")
			claimRow(t, s, "plan", 1, "b1", "t1", "n1")
			claimRow(t, s, "plan", 2, "b1", "t1", "n2")
		}},
		{name: "another task writes under the newest epoch", task: "t2", epoch: 2, stale: true, setup: func(t *testing.T, s *store.Store) {
			parentBinding(t, s, "b1", "t1", "P-TEST", "active")
			parentBinding(t, s, "b2", "t2", "P-OTHER", "active")
			claimRow(t, s, "plan", 1, "b1", "t1", "n1")
			claimRow(t, s, "plan", 2, "b2", "t2", "n2")
		}},
		{name: "the claiming binding was replaced", task: "t1", epoch: 1, stale: true, setup: func(t *testing.T, s *store.Store) {
			parentBinding(t, s, "b1", "t1", "P-TEST", "active")
			claimRow(t, s, "plan", 1, "b1", "t1", "n1")
			if _, err := s.DB.Exec("UPDATE scope_bindings SET status = 'archived', superseded_by = 'b2' WHERE binding_id = 'b1'"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a paused binding is not a stale epoch", task: "t1", epoch: 1, setup: func(t *testing.T, s *store.Store) {
			parentBinding(t, s, "b1", "t1", "P-TEST", "paused")
			claimRow(t, s, "plan", 1, "b1", "t1", "n1")
		}},
		{name: "a claim made under the binding of another project", task: "t1", epoch: 1, stale: true, setup: func(t *testing.T, s *store.Store) {
			parentBinding(t, s, "b1", "t1", "P-OTHER", "active")
			claimRow(t, s, "plan", 1, "b1", "t1", "n1")
		}},
		{name: "the claiming binding is not a parent binding", task: "t1", epoch: 1, stale: true, setup: func(t *testing.T, s *store.Store) {
			parentBinding(t, s, "b1", "t1", "P-TEST", "active")
			if _, err := s.DB.Exec("UPDATE scope_bindings SET role = 'child' WHERE binding_id = 'b1'"); err != nil {
				t.Fatal(err)
			}
			claimRow(t, s, "plan", 1, "b1", "t1", "n1")
		}},
	}
	for _, c := range cases {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s (plan exists: %v)", c.name, existing), func(t *testing.T) {
				r, s, _ := newRepo(t)
				ctx := context.Background()
				if existing {
					if _, err := r.Put(ctx, decode(t, revDoc("plan", "r1", 0, addNode("a", NodeNonPR)))); err != nil {
						t.Fatal(err)
					}
				}
				if c.setup != nil {
					c.setup(t, s)
				}
				before := zoneRows(t, s.DB)
				parent := 0
				if existing {
					parent = 1
				}
				res, err := r.Put(ctx, decode(t, epochDoc("plan", "r2", parent, c.task, c.epoch, "P-TEST", addNode("b", NodeNonPR))))
				if c.stale {
					if !isStale(err) {
						t.Fatalf("want stale_coordinator_epoch, got %v (%+v)", err, res)
					}
					if after := zoneRows(t, s.DB); !reflect.DeepEqual(before, after) {
						t.Fatalf("a refused write changed the zone:\nbefore %v\nafter  %v", before, after)
					}
					return
				}
				if err != nil {
					t.Fatalf("the write was refused: %v", err)
				}
				if res.CoordinatorEpoch != c.epoch {
					t.Fatalf("the revision carries epoch %d, want %d", res.CoordinatorEpoch, c.epoch)
				}
				var recorded int64
				if err := s.DB.QueryRow("SELECT coordinator_epoch FROM dag_plan_revisions WHERE plan_id = 'plan' AND revision_no = ?", res.RevisionNo).Scan(&recorded); err != nil || recorded != c.epoch {
					t.Fatalf("the stored revision has epoch %d (%v), want %d", recorded, err, c.epoch)
				}
			})
		}
	}
}

// The fence runs before the replay check. A session that was replaced and sends its own committed request again is refused, not told "replayed";
// and the request id the replacement session sends under its own epoch is another request (plan_revision_conflict): it reads the log instead of resending.
func TestTheFenceComesBeforeTheReplayCheck(t *testing.T) {
	r, s, _ := newRepo(t)
	ctx := context.Background()
	parentBinding(t, s, "b1", "t1", "P-TEST", "active")
	claimRow(t, s, "plan", 1, "b1", "t1", "n1")
	first := decode(t, epochDoc("plan", "r1", 0, "t1", 1, "P-TEST", addNode("a", NodeNonPR)))
	if _, err := r.Put(ctx, first); err != nil {
		t.Fatal(err)
	}
	if res, err := r.Put(ctx, first); err != nil || !res.Replayed {
		t.Fatalf("the same session repeating its request: %+v, %v", res, err)
	}
	claimRow(t, s, "plan", 2, "b1", "t1", "n2")
	before := zoneRows(t, s.DB)
	if _, err := r.Put(ctx, first); !isStale(err) {
		t.Fatalf("a replaced session repeating its request must be refused, got %v", err)
	}
	if after := zoneRows(t, s.DB); !reflect.DeepEqual(before, after) {
		t.Fatalf("the refusal changed the zone")
	}
	_, err := r.Put(ctx, decode(t, epochDoc("plan", "r1", 0, "t1", 2, "P-TEST", addNode("a", NodeNonPR))))
	var refused *store.RefusedError
	if !errors.As(err, &refused) || refused.Reason != "plan_revision_conflict" {
		t.Fatalf("the same request id under another epoch is another request (plan_revision_conflict), got %v", err)
	}
}

// The check reads the claims of the transaction it runs in: a claim that lands after a session was judged holder refuses that session's next check.
func TestTheFenceReadsTheClaimsOfTheTransactionItIsIn(t *testing.T) {
	r, s, _ := newRepo(t)
	ctx := context.Background()
	parentBinding(t, s, "b1", "t1", "P-TEST", "active")
	if _, err := r.Put(ctx, decode(t, revDoc("plan", "r1", 0, addNode("a", NodeNonPR)))); err != nil {
		t.Fatal(err)
	}
	claimRow(t, s, "plan", 1, "b1", "t1", "n1")
	if err := s.Transaction(ctx, func(txCtx context.Context, conn *sql.Conn) error {
		return CheckCoordinatorEpoch(txCtx, conn, "plan", "", "t1", 1)
	}); err != nil {
		t.Fatalf("the holder is refused: %v", err)
	}
	claimRow(t, s, "plan", 2, "b1", "t1", "n2")
	err := s.Transaction(ctx, func(txCtx context.Context, conn *sql.Conn) error {
		return CheckCoordinatorEpoch(txCtx, conn, "plan", "", "t1", 1)
	})
	if !isStale(err) {
		t.Fatalf("a claim of the same store is not seen by the fence: %v", err)
	}
}
