package dag

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func allEvents(t *testing.T, r *Repo, after int64) []Event {
	t.Helper()
	page, err := r.Events(context.Background(), "plan", after, MaxPage)
	if err != nil {
		t.Fatal(err)
	}
	return page.Events
}

// Replaying the committed events restores the plan: from the empty plan, and from any snapshot with the cursor it was read at.
func TestReplayFromEmptyAndFromAnySnapshotReachesTheHead(t *testing.T) {
	r, _, _ := newRepo(t)
	putAll(t, r)
	ctx := context.Background()
	head, _, err := r.Snapshot(ctx, "plan", 0)
	if err != nil {
		t.Fatal(err)
	}
	empty := Snapshot{PlanID: "plan", ProjectKey: "P-TEST"}
	got, err := Replay(empty, allEvents(t, r, 0))
	if err != nil || !reflect.DeepEqual(got, head) {
		t.Fatalf("replay from the empty plan: %v\n got  %+v\n head %+v", err, got, head)
	}
	for k := int64(1); k <= 4; k++ {
		at, _, err := r.Snapshot(ctx, "plan", k)
		if err != nil {
			t.Fatal(err)
		}
		replayed, err := Replay(at, allEvents(t, r, k))
		if err != nil || !reflect.DeepEqual(replayed, head) {
			t.Fatalf("snapshot at revision %d plus the events after it: %v\n got  %+v\n head %+v", k, err, replayed, head)
		}
		// a snapshot plus no events is itself
		same, err := Replay(at, nil)
		if err != nil || !reflect.DeepEqual(same, at) {
			t.Fatalf("snapshot %d replayed with nothing: %v", k, err)
		}
	}
	if err := r.VerifyLog(ctx, "plan"); err != nil {
		t.Fatal(err)
	}
}

// The cursor is a revision number: a page resumes after it, and the last page's cursor is the head.
func TestCursorPagesTheLog(t *testing.T) {
	r, _, _ := newRepo(t)
	putAll(t, r)
	ctx := context.Background()
	var seen []int64
	cursor := int64(0)
	for pages := 0; pages < 10; pages++ {
		page, err := r.Events(ctx, "plan", cursor, 3)
		if err != nil {
			t.Fatal(err)
		}
		if page.Head != 4 || page.After != cursor {
			t.Fatalf("page %+v", page)
		}
		for _, ev := range page.Events {
			seen = append(seen, ev.RevisionNo)
		}
		if len(page.Events) == 0 {
			if page.Cursor != cursor {
				t.Fatalf("an empty page moved the cursor to %d", page.Cursor)
			}
			break
		}
		cursor = page.Cursor
	}
	if !reflect.DeepEqual(seen, []int64{1, 2, 3, 4}) || cursor != 4 {
		t.Fatalf("paged %v, cursor %d", seen, cursor)
	}
	if _, err := r.Events(ctx, "plan", 9, 1); reasonOf(err) != "unregistered_scope" {
		t.Fatalf("a cursor past the head: %v", err)
	}
	// the events carry what was asked, in the canonical spelling a replay applies
	events := allEvents(t, r, 0)
	if events[0].RequestID != "r1" || events[3].ParentRevisionNo != 3 || events[1].Changes[0].Op != OpUpdateNode || events[1].Changes[0].Node.Title != "Implementation A" {
		t.Fatalf("events %+v", events)
	}
}

// What a log cannot contain is refused: a gap, a repeat, a reordering, a change that does not apply, a digest that is not the one recorded.
func TestReplayRefusesWhatALogCannotContain(t *testing.T) {
	r, _, _ := newRepo(t)
	putAll(t, r)
	empty := Snapshot{PlanID: "plan", ProjectKey: "P-TEST"}
	events := allEvents(t, r, 0)
	mutate := func(f func(e []Event) []Event) []Event {
		return f(append([]Event(nil), events...))
	}
	for name, bad := range map[string][]Event{
		"a missing event":     mutate(func(e []Event) []Event { return append(e[:1], e[2:]...) }),
		"a repeated event":    mutate(func(e []Event) []Event { return append(e[:2], e[1:]...) }),
		"reordered events":    mutate(func(e []Event) []Event { e[1], e[2] = e[2], e[1]; return e }),
		"a log with no first": events[1:],
		"a wrong state digest": mutate(func(e []Event) []Event {
			e[2].StateDigest = strings.Repeat("0", 64)
			return e
		}),
		"a change that does not apply": mutate(func(e []Event) []Event {
			e[1].Changes = []Change{{Op: OpRetireNode, NodeID: "ghost"}}
			return e
		}),
	} {
		if _, err := Replay(empty, bad); err == nil {
			t.Errorf("%s: replayed", name)
		}
	}
}

// A plan that does not agree with itself is the host's failure and never a partial plan, however it was damaged.
func TestReaderRecomputesDigestsAndNeverReturnsAPartialPlan(t *testing.T) {
	ctx := context.Background()
	for name, tamper := range map[string][]string{
		"a stored revision digest":  {"DROP TRIGGER dag_plan_revisions_no_update", "UPDATE dag_plan_revisions SET state_digest = '" + strings.Repeat("0", 64) + "' WHERE revision_no = 4"},
		"a stored slice digest":     {"DROP TRIGGER dag_nodes_retire_only", "UPDATE dag_nodes SET slice_digest = '" + strings.Repeat("1", 64) + "' WHERE node_id = 'join' AND retired_rev IS NULL"},
		"a node row that vanished":  {"DROP TRIGGER dag_nodes_no_delete", "DELETE FROM dag_nodes WHERE node_id = 'ship'"},
		"an edge row that vanished": {"DROP TRIGGER dag_edges_no_delete", "DELETE FROM dag_edges WHERE edge_id = 'e5'"},
	} {
		t.Run(name, func(t *testing.T) {
			r, s, _ := newRepo(t)
			putAll(t, r)
			for _, statement := range tamper {
				if _, err := s.DB.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err := r.Snapshot(ctx, "plan", 0)
			var corrupt *CorruptError
			if !errors.As(err, &corrupt) {
				t.Fatalf("a damaged plan was read as %v", err)
			}
		})
	}
	t.Run("a forged slice digest whose revision digest was recomputed to match", func(t *testing.T) {
		r, s, _ := newRepo(t)
		putAll(t, r)
		snap, _, err := r.Snapshot(ctx, "plan", 0)
		if err != nil {
			t.Fatal(err)
		}
		forged := strings.Repeat("7", 64)
		for i := range snap.Nodes {
			if snap.Nodes[i].NodeID == "join" {
				snap.Nodes[i].SliceDigest = forged
			}
		}
		recomputed := stateDigest(snap.PlanID, snap.ProjectKey, snap.Nodes, snap.Edges)
		for _, statement := range []string{"DROP TRIGGER dag_nodes_retire_only", "DROP TRIGGER dag_plan_revisions_no_update",
			"UPDATE dag_nodes SET slice_digest = '" + forged + "' WHERE node_id = 'join' AND retired_rev IS NULL",
			"UPDATE dag_plan_revisions SET state_digest = '" + recomputed + "' WHERE revision_no = 4"} {
			if _, err := s.DB.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
		// the stored digests agree with each other; the node's slice does not agree with the edges beside it
		var corrupt *CorruptError
		if _, _, err := r.Snapshot(ctx, "plan", 0); !errors.As(err, &corrupt) || !strings.Contains(err.Error(), "node join") {
			t.Fatalf("a forged slice digest was read as %v", err)
		}
	})
	t.Run("a stored change list that no longer replays", func(t *testing.T) {
		r, s, _ := newRepo(t)
		putAll(t, r)
		for _, statement := range []string{"DROP TRIGGER dag_plan_revisions_no_update",
			"UPDATE dag_plan_revisions SET change_json = '[{\"op\":\"retire_node\",\"node_id\":\"design\"}]' WHERE revision_no = 2"} {
			if _, err := s.DB.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
		// the head still reads: its rows and digest agree with each other; the log does not replay to them
		if _, _, err := r.Snapshot(ctx, "plan", 0); err != nil {
			t.Fatal(err)
		}
		var corrupt *CorruptError
		if err := r.VerifyLog(ctx, "plan"); !errors.As(err, &corrupt) {
			t.Fatalf("a log that does not replay verified as %v", err)
		}
	})
}

func TestAnAbsentPlanOrRevisionIsRefusedNotEmpty(t *testing.T) {
	r, _, _ := newRepo(t)
	ctx := context.Background()
	if _, _, err := r.Snapshot(ctx, "nothing", 0); reasonOf(err) != "unregistered_scope" || !strings.Contains(err.Error(), "no plan nothing is registered") {
		t.Fatalf("an unknown plan: %v", err)
	}
	mustPut(t, r, forkJoin("plan", "r1"))
	for _, rev := range []int64{2, 99} {
		if _, head, err := r.Snapshot(ctx, "plan", rev); reasonOf(err) != "unregistered_scope" || head != 1 || !strings.Contains(err.Error(), "its revisions are 1 to 1") {
			t.Fatalf("revision %d of a one-revision plan: %v", rev, err)
		}
	}
}

// A store that has no DAG zone (one no write open has reached) has no plans: the reader says so as it says it of an unknown plan.
func TestReaderOfAStoreWithoutTheZone(t *testing.T) {
	path := t.TempDir() + "/relay.sqlite3"
	testsupport.Create(t, path, "", "go")
	s, err := store.OpenReadOnlyStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := &Repo{Store: s}
	if _, _, err := r.Snapshot(context.Background(), "plan", 0); reasonOf(err) != "unregistered_scope" {
		t.Fatalf("a store without the zone: %v", err)
	}
	if _, err := r.Events(context.Background(), "plan", 0, 10); reasonOf(err) != "unregistered_scope" {
		t.Fatalf("its log: %v", err)
	}
}

// A reader sees one committed state however many revisions commit while it reads.
func TestReadIsOneSnapshot(t *testing.T) {
	r, _, path := newRepo(t)
	mustPut(t, r, forkJoin("plan", "r1"))
	writer := &Repo{Store: openStore(t, path)}
	ctx := context.Background()
	err := r.readTx(ctx, func(q Queryer) error {
		first, _, err := SnapshotAt(ctx, q, "plan", 0)
		if err != nil {
			return err
		}
		// a revision commits on another connection in the middle of the read
		if _, err := writer.Put(ctx, decode(t, revDoc("plan", "r2", 1, updateNode("impl-a", NodeImplementation, "late")))); err != nil {
			return err
		}
		second, _, err := SnapshotAt(ctx, q, "plan", 0)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(first, second) {
			t.Errorf("a read saw two states:\n %+v\n %+v", first, second)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if snap, head, err := r.Snapshot(ctx, "plan", 0); err != nil || head != 2 || snap.Revision != 2 {
		t.Fatalf("the next read: %v head %d", err, head)
	}
}
