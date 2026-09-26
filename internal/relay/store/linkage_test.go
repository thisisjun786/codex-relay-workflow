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

func TestRivalOwner_excludes_the_caller_and_the_owner_being_replaced(t *testing.T) {
	// Given: task-a owns the scope.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.InsertScopeBinding(ctx, binding("bnd-a", "task-a", "P", 1, "active")))
	// When/Then: task-b sees task-a as a rival, unless task-a is the one its handover replaces.
	rival, err := s.RivalOwner(ctx, "project", "P", "parent", "task-b", sql.NullString{})
	if err != nil || rival.TaskID != "task-a" || rival.Status != "active" {
		t.Fatalf("rival=%v err=%v", rival, err)
	}
	if _, err := s.RivalOwner(ctx, "project", "P", "parent", "task-b", text("task-a")); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("replaced owner counted as rival: %v", err)
	}
	if _, err := s.RivalOwner(ctx, "project", "P", "parent", "task-a", sql.NullString{}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("the caller counted as its own rival: %v", err)
	}
}

func TestBindingsForTask_lists_live_bindings_before_archived_ones(t *testing.T) {
	// Given: one task with an archived binding at a higher revision than its live one.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.InsertScopeBinding(ctx, binding("bnd-old", "task-a", "P1", 5, "archived")))
	must(t, s.InsertScopeBinding(ctx, binding("bnd-live", "task-a", "P2", 1, "active")))
	// When: the task's bindings are read.
	rows, err := s.BindingsForTask(ctx, "task-a")
	must(t, err)
	// Then: the live one comes first regardless of revision.
	if len(rows) != 2 || rows[0].BindingID != "bnd-live" {
		t.Fatalf("rows=%v", rows)
	}
}

func TestScopeBindingUpdates_move_status_and_endpoint_as_python_writes_them(t *testing.T) {
	// Given: an active binding.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.InsertScopeBinding(ctx, binding("bnd-a", "task-a", "P", 1, "active")))
	// When: its status is set to the value it already has, then it is reactivated paused elsewhere.
	must(t, s.SetScopeBindingStatus(ctx, "bnd-a", "active", "t-same"))
	unchanged, err := s.ScopeBinding(ctx, "bnd-a")
	must(t, err)
	must(t, s.ReactivateScopeBinding(ctx, "bnd-a", "paused", "t-back", "host-2", text("/cwd"), text("cxc")))
	moved, err := s.ScopeBinding(ctx, "bnd-a")
	must(t, err)
	// Then: an unchanged status is not rewritten, and a reactivation carries the new endpoint.
	if unchanged.UpdatedAt != "t" {
		t.Fatalf("same-status update rewrote updated_at: %q", unchanged.UpdatedAt)
	}
	if moved.Status != "paused" || moved.HostID != "host-2" || moved.CWD.String != "/cwd" || moved.CXCSession.String != "cxc" || moved.UpdatedAt != "t-back" {
		t.Fatalf("reactivated=%+v", moved)
	}
}

func link(id, kind, upperKey, lowerKey string, revision int64) ScopeLinksRow {
	return ScopeLinksRow{LinkID: id, LinkKind: kind, UpperKind: "project", UpperKey: upperKey, UpperTaskID: "tu", LowerKind: "project", LowerKey: lowerKey, LowerTaskID: "tl", Status: "active", Revision: revision, CreatedAt: "t", UpdatedAt: "t"}
}

func TestJoiningLinks_finds_live_links_in_either_direction(t *testing.T) {
	// Given: a peer link A->B, a reference B->A at a higher revision, and a cancelled link.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.InsertScopeLink(ctx, link("lnk-1", "peer", "A", "B", 1)))
	must(t, s.InsertScopeLink(ctx, link("lnk-2", "reference", "B", "A", 2)))
	must(t, s.InsertScopeLink(ctx, link("lnk-3", "execution", "A", "B", 3)))
	must(t, s.SetScopeLinkStatus(ctx, "lnk-3", "cancelled", "t2"))
	// When: the links joining A and B are read.
	rows, err := s.JoiningLinks(ctx, "project", "A", "project", "B")
	must(t, err)
	// Then: both live links, newest revision first, and never the cancelled one.
	if len(rows) != 2 || rows[0].LinkID != "lnk-2" || rows[1].LinkID != "lnk-1" {
		t.Fatalf("rows=%v", rows)
	}
}

func TestExecutionLinks_read_only_live_execution_edges(t *testing.T) {
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

func TestRepointScopeLink_reactivates_repoints_and_counts_a_revision(t *testing.T) {
	// Given: an archived edge superseded by another.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.InsertScopeLink(ctx, link("lnk-1", "execution", "P", "I", 1)))
	_, err := s.DB.ExecContext(ctx, "UPDATE scope_links SET status='archived', superseded_by='lnk-x' WHERE link_id='lnk-1'")
	must(t, err)
	// When: it is repointed at a new child.
	must(t, s.RepointScopeLink(ctx, "lnk-1", "active", "child-2", "parent-2", "t2"))
	row, err := s.ScopeLink(ctx, "lnk-1")
	must(t, err)
	// Then: live again, with the new tasks, one revision later, superseded_by cleared.
	if row.Status != "active" || row.LowerTaskID != "child-2" || row.UpperTaskID != "parent-2" || row.Revision != 2 || row.SupersededBy.Valid {
		t.Fatalf("row=%+v", row)
	}
}

func TestScopeDirectives_are_recorded_undecided_and_settled_once_read_in_order(t *testing.T) {
	// Given: two directives on one scope, recorded out of id order.
	s := recordStore(t)
	ctx := context.Background()
	for _, d := range []ScopeDirectivesRow{
		{DirectiveID: "dir-b", ScopeKind: "project", ScopeKey: "P", FromTaskID: "sup", FromScopeKey: "I", LinkID: "lnk", LinkKind: "execution", Digest: "d1", Revision: 1, RecordedAt: "t1"},
		{DirectiveID: "dir-a", ScopeKind: "project", ScopeKey: "P", FromTaskID: "sup", FromScopeKey: "I", LinkID: "lnk", LinkKind: "execution", Digest: "d2", Revision: 1, RecordedAt: "t2"},
	} {
		must(t, s.InsertScopeDirective(ctx, d))
	}
	// When: the first is settled.
	must(t, s.SettleScopeDirective(ctx, "dir-b", "chosen", "parent", "t3"))
	all, err := s.ScopeDirectives(ctx, "project", "P")
	must(t, err)
	open, err := s.UndecidedDirectives(ctx, "project", "P")
	must(t, err)
	// Then: listings keep recording order, and only the unsettled one is undecided.
	if len(all) != 2 || all[0].DirectiveID != "dir-b" || all[0].Disposition.String != "chosen" || all[0].DecidedBy.String != "parent" {
		t.Fatalf("all=%v", all)
	}
	if len(open) != 1 || open[0].DirectiveID != "dir-a" || open[0].Disposition.Valid {
		t.Fatalf("undecided=%v", open)
	}
}

func TestConflicts_converge_on_one_row_per_contest(t *testing.T) {
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
