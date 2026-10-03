package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func text(value string) sql.NullString { return sql.NullString{String: value, Valid: true} }

func binding(id, task, key string, revision int64, status string) ScopeBindingsRow {
	return ScopeBindingsRow{BindingID: id, Role: "parent", ScopeKind: "project", ScopeKey: key, TaskID: task, HostID: "host", Status: status, Revision: revision, CreatedAt: "t", UpdatedAt: "t"}
}

func TestScopeOwners_lists_every_live_owner_newest_first_and_owner_takes_the_newest(t *testing.T) {
	t.Parallel()
	// Given: a store without the owner guard holding two live owners, one archived and one superseded.
	s := recordStore(t)
	ctx := context.Background()
	_, err := s.DB.ExecContext(ctx, "DROP INDEX scope_bindings_one_live_owner")
	must(t, err)
	must(t, s.InsertScopeBinding(ctx, binding("bnd-a", "task-a", "P", 1, "active")))
	must(t, s.InsertScopeBinding(ctx, binding("bnd-b", "task-b", "P", 2, "paused")))
	must(t, s.InsertScopeBinding(ctx, binding("bnd-c", "task-c", "P", 3, "archived")))
	must(t, s.InsertScopeBinding(ctx, binding("bnd-d", "task-d", "P", 4, "active")))
	must(t, s.ArchiveScopeBinding(ctx, "bnd-d", "active", "bnd-x", "t2"))
	// When: the readers ask who owns the scope.
	owners, err := s.ScopeOwners(ctx, "project", "P")
	must(t, err)
	owner, err := s.ScopeOwner(ctx, "project", "P")
	must(t, err)
	tasks, err := s.LiveOwnerTasks(ctx, "project", "P", "parent")
	must(t, err)
	// Then: live means active or paused and not superseded; newest revision first.
	if len(owners) != 2 || owners[0].BindingID != "bnd-b" || owners[1].BindingID != "bnd-a" || owner.BindingID != "bnd-b" {
		t.Fatalf("owners=%v owner=%v", owners, owner)
	}
	if len(tasks) != 2 || tasks[0] != "task-a" || tasks[1] != "task-b" {
		t.Fatalf("live owner tasks are ordered by task id: %v", tasks)
	}
}

func link(id, kind, upperKey, lowerKey string, revision int64) ScopeLinksRow {
	return ScopeLinksRow{LinkID: id, LinkKind: kind, UpperKind: "project", UpperKey: upperKey, UpperTaskID: "tu", LowerKind: "project", LowerKey: lowerKey, LowerTaskID: "tl", Status: "active", Revision: revision, CreatedAt: "t", UpdatedAt: "t"}
}

func TestExecutionLinks_read_only_live_execution_edges(t *testing.T) {
	t.Parallel()
	// Given: an execution edge and a reference edge out of one scope into two.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.InsertScopeLink(ctx, link("lnk-1", "execution", "I", "B", 1)))
	must(t, s.InsertScopeLink(ctx, link("lnk-2", "reference", "I", "A", 1)))
	// When: the execution edges below I and above B are read.
	below, err := s.ExecutionLinksBelow(ctx, "project", "I")
	must(t, err)
	above, err := s.ExecutionLinksAbove(ctx, "project", "B")
	must(t, err)
	// Then: only the execution edge appears.
	if len(below) != 1 || below[0].LinkID != "lnk-1" || len(above) != 1 || above[0].LinkID != "lnk-1" {
		t.Fatalf("below=%v above=%v", below, above)
	}
}

func TestConflicts_converge_on_one_row_per_contest(t *testing.T) {
	t.Parallel()
	// Given: the same contest recorded twice in each conflict table, then a different one.
	s := recordStore(t)
	ctx := context.Background()
	for _, at := range []string{"t1", "t2"} {
		must(t, s.RecordLinkageConflict(ctx, LinkageConflictsRow{At: at, ScopeKind: "project", ScopeKey: "P", Reason: "duplicate_scope_owner", Incumbent: "a", Challenger: "b", Detail: text("detail " + at)}))
		must(t, s.RecordCoordinationConflict(ctx, CoordinationConflictsRow{At: at, Domain: "merge_target", Subject: "repo|main", Reason: "merge_turn_not_held", Incumbent: "a", Challenger: "b", Detail: text("detail " + at)}))
	}
	must(t, s.RecordCoordinationConflict(ctx, CoordinationConflictsRow{At: "t3", Domain: "merge_target", Subject: "repo|main", Reason: "merge_turn_not_held", Incumbent: "a", Challenger: "c"}))
	// When: the contests are read.
	linkage, err := s.LinkageConflicts(ctx, "project", "P")
	must(t, err)
	coordination, err := s.CoordinationConflicts(ctx, "merge_target", "repo|main")
	must(t, err)
	// Then: a retried loser is one row carrying the latest time and detail; rows keep id order.
	if len(linkage) != 1 || linkage[0].At != "t2" || linkage[0].Detail.String != "detail t2" {
		t.Fatalf("linkage=%v", linkage)
	}
	if len(coordination) != 2 || coordination[0].At != "t2" || coordination[1].Challenger != "c" || coordination[0].ID > coordination[1].ID {
		t.Fatalf("coordination=%v", coordination)
	}
}

func TestRelationshipScope_keeps_the_first_project_recorded(t *testing.T) {
	t.Parallel()
	// Given: a relationship attached to P1, and relationships to order.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.RecordRelationshipScope(ctx, "rel-1", "P1", "t1"))
	// When: it is attached again to P2.
	must(t, s.RecordRelationshipScope(ctx, "rel-1", "P2", "t2"))
	row, err := s.RelationshipScope(ctx, "rel-1")
	must(t, err)
	// Then: ON CONFLICT DO NOTHING keeps P1 and its time.
	if row.ProjectKey != "P1" || row.RecordedAt != "t1" {
		t.Fatalf("row=%+v", row)
	}
}

func TestScopedRelationships_lists_a_projects_relationships_oldest_first(t *testing.T) {
	t.Parallel()
	// Given: two relationships attached to P, created in the opposite order to their ids.
	s := recordStore(t)
	ctx := context.Background()
	for _, r := range []struct{ id, created string }{{"rel-a", "t2"}, {"rel-b", "t1"}} {
		_, err := s.DB.ExecContext(ctx, `INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, r.id, "I-"+r.id, "active", "p", "h", "c-"+r.id, "h", 1, "[]", "[]", r.created, r.created)
		must(t, err)
		must(t, s.RecordRelationshipScope(ctx, r.id, "P", r.created))
	}
	// When/Then: they are listed by creation.
	ids, err := s.ScopedRelationships(ctx, "P")
	if err != nil || len(ids) != 2 || ids[0] != "rel-b" {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
}

func TestDomainWrites_join_the_open_transaction_and_roll_back_with_it(t *testing.T) {
	t.Parallel()
	// Given: a transaction that writes a binding and reads it back before failing.
	s := recordStore(t)
	ctx := context.Background()
	failure := errors.New("refused after the write")
	var seenInside int
	err := s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		must(t, s.InsertScopeBinding(ctx, binding("bnd-a", "task-a", "P", 1, "active")))
		owners, err := s.ScopeOwners(ctx, "project", "P")
		seenInside = len(owners)
		if err != nil {
			return err
		}
		return failure
	})
	// When: the transaction has rolled back.
	owners, readErr := s.ScopeOwners(ctx, "project", "P")
	must(t, readErr)
	// Then: the body read its own uncommitted write, and nothing survived the rollback.
	if !errors.Is(err, failure) || seenInside != 1 || len(owners) != 0 {
		t.Fatalf("err=%v inside=%d after=%d", err, seenInside, len(owners))
	}
}
