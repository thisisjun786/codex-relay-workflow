package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// CRW-767: the merge train's three tables (the decision in docs/port/decisions.md section 79). A
// train and a member are written once and an event is appended, the zone's triggers abort an UPDATE
// and a DELETE, and the train's state is the newest event's kind rather than a column. The
// statements ship in the same shape as dag_base_refreshes, so these tests write the tables directly,
// as an operator's sqlite3 would, and the constraints answer for a writer that skips the store's own
// insert.

// mergeTrainRow is one merge_trains row as SQL, every column named so a reordered statement cannot
// pass by accident.
func mergeTrainRow(id string) string {
	return fmt.Sprintf("INSERT INTO merge_trains (train_id, target_key, repository, base_ref, base_sha,"+
		" leader_task_id, created_at) VALUES ('%s','repo|dev','o/r','dev','%s','task-leader','2026-10-06T00:00:00Z')", id, zoneDigest)
}

// The two terminal outcomes the successor commands will write: a train that lands every member closes
// with done, and one whose base a landing outside the lane moved closes with abandoned. Both are just
// another appended event, so the state follows the newest one and no earlier row moves. The writer
// refuses the same things the DDL does, so the command layer never has to pre-check them.
func TestDAGMergeTrainTerminalStatesAndWriterRefusals(t *testing.T) {
	t.Parallel()
	s := recordStore(t)
	ctx := context.Background()
	train := MergeTrainRow{TrainID: "train-2", TargetKey: "o/r|dev", Repository: "o/r", BaseRef: "dev",
		BaseSHA: zoneDigest, LeaderTaskID: "task-leader", CreatedAt: "2026-10-06T00:00:00Z"}
	if err := RecordMergeTrain(ctx, s, train); err != nil {
		t.Fatal(err)
	}
	// the writer refuses a kind outside the five, an empty actor and a repeated sequence number
	for _, bad := range []MergeTrainEventRow{
		{TrainID: train.TrainID, Seq: 1, Kind: "started", Actor: "task-leader", DetailJSON: "{}", RecordedAt: "2026-10-06T00:00:01Z"},
		{TrainID: train.TrainID, Seq: 1, Kind: MergeTrainOpened, Actor: "", DetailJSON: "{}", RecordedAt: "2026-10-06T00:00:01Z"},
		{TrainID: train.TrainID, Seq: 1, Kind: MergeTrainOpened, Actor: "task-leader", DetailJSON: "", RecordedAt: "2026-10-06T00:00:01Z"},
		{TrainID: "train-none", Seq: 1, Kind: MergeTrainOpened, Actor: "task-leader", DetailJSON: "{}", RecordedAt: "2026-10-06T00:00:01Z"},
	} {
		if err := RecordMergeTrainEvent(ctx, s, bad); err == nil {
			t.Fatalf("the writer accepted %+v", bad)
		}
	}
	// the writer refuses a member at a sequence number the train already holds, and one of another train
	member := MergeTrainMemberRow{TrainID: train.TrainID, Seq: 1, TurnID: "turn-1", PRNumber: 701,
		RelationshipID: "rel-1", MemberHead: zoneDigest}
	if err := RecordMergeTrainMember(ctx, s, member); err != nil {
		t.Fatal(err)
	}
	if err := RecordMergeTrainMember(ctx, s, member); err == nil {
		t.Fatal("the writer accepted a second member at one sequence number")
	}
	// an abandoned train: the state is abandoned and every earlier event still reads back unchanged
	opened := MergeTrainEventRow{TrainID: train.TrainID, Seq: 1, Kind: MergeTrainOpened, Actor: "task-leader", DetailJSON: "{}", RecordedAt: "2026-10-06T00:00:01Z"}
	verified := MergeTrainEventRow{TrainID: train.TrainID, Seq: 2, Kind: MergeTrainVerified, Actor: "task-leader", DetailJSON: `{"seq":1,"run_id":37414778712}`, RecordedAt: "2026-10-06T00:00:02Z"}
	abandoned := MergeTrainEventRow{TrainID: train.TrainID, Seq: 3, Kind: MergeTrainAbandoned, Actor: "task-leader", DetailJSON: `{"reason":"base moved out of lane"}`, RecordedAt: "2026-10-06T00:00:03Z"}
	for _, event := range []MergeTrainEventRow{opened, verified, abandoned} {
		if err := RecordMergeTrainEvent(ctx, s, event); err != nil {
			t.Fatal(err)
		}
	}
	if state, found, err := MergeTrainState(ctx, s, train.TrainID); err != nil || !found || state != MergeTrainAbandoned {
		t.Fatalf("state = %q found=%v (%v), want %q", state, found, err, MergeTrainAbandoned)
	}
	events, err := MergeTrainEvents(ctx, s, train.TrainID)
	if err != nil || !reflect.DeepEqual(events, []MergeTrainEventRow{opened, verified, abandoned}) {
		t.Fatalf("events = %+v (%v)", events, err)
	}
	// the two terminal kinds are distinct constants, so a reader can tell them apart
	if MergeTrainDone == MergeTrainAbandoned {
		t.Fatal("done and abandoned are the same kind")
	}
}

// mergeTrainMemberRow is one merge_train_members row as SQL.
func mergeTrainMemberRow(train string, seq int, turn string) string {
	return fmt.Sprintf("INSERT INTO merge_train_members (train_id, seq, turn_id, pr_number, relationship_id,"+
		" member_head) VALUES ('%s',%d,'%s',%d,'rel-1','%s')", train, seq, turn, 700+seq, zoneDigest)
}

// mergeTrainEventRow is one merge_train_events row as SQL.
func mergeTrainEventRow(train string, seq int, kind string) string {
	return fmt.Sprintf("INSERT INTO merge_train_events (train_id, seq, kind, actor, detail_json, recorded_at)"+
		" VALUES ('%s',%d,'%s','task-leader','{}','2026-10-06T00:00:00Z')", train, seq, kind)
}

// CRW-768 decision 9: merge_trains is a SQLite rowid table, so TEXT PRIMARY KEY does not mean NOT
// NULL and the shipped CHECK (train_id <> '') lets a NULL through. Two NULL rows were inserted into
// one table before the trigger (the CRW-767 post-merge finding P1), and a NULL row cannot be found
// by the string id every train command looks a train up by. The guard is appended as one BEFORE
// INSERT trigger; no shipped statement is edited and an existing row is never deleted or rewritten.

// mergeTrainsCreate is the shipped CRW-767 CREATE TABLE, repeated here as a 767-era store holds it,
// so this file can build a zone that predates the trigger without opening the current one.
const mergeTrainsCreate = "CREATE TABLE IF NOT EXISTS merge_trains (\n" +
	"    train_id       TEXT PRIMARY KEY CHECK (train_id <> ''),\n" +
	"    target_key     TEXT NOT NULL CHECK (target_key <> ''),\n" +
	"    repository     TEXT NOT NULL CHECK (repository <> ''),\n" +
	"    base_ref       TEXT NOT NULL CHECK (base_ref <> ''),\n" +
	"    base_sha       TEXT NOT NULL CHECK (base_sha <> ''),\n" +
	"    leader_task_id TEXT NOT NULL CHECK (leader_task_id <> ''),\n" +
	"    created_at     TEXT NOT NULL\n" +
	")"

// A NULL train_id is refused once the trigger is in the zone, an empty id by the existing CHECK, a
// valid id inserts and its repeat is the PRIMARY KEY's refusal. Every one of these is the DDL's own
// answer, so the command layer never pre-checks them.
func TestDAGZoneMergeTrainsRefuseANullTrainID(t *testing.T) {
	t.Parallel()
	db := zoneOpenedDB(t)
	zoneRefuses(t, db, "a NULL train id", "INSERT INTO merge_trains (train_id, target_key, repository, base_ref, base_sha, leader_task_id, created_at)"+
		" VALUES (NULL,'o/r|dev','o/r','dev','"+zoneDigest+"','task-leader','2026-10-06T00:00:00Z')")
	zoneRefuses(t, db, "a second NULL train id", "INSERT INTO merge_trains (train_id, target_key, repository, base_ref, base_sha, leader_task_id, created_at)"+
		" VALUES (NULL,'o/r|dev','o/r','dev','"+zoneDigest+"','task-leader','2026-10-06T00:00:00Z')")
	zoneRefuses(t, db, "an empty train id", mergeTrainRow(""))
	zoneMustExec(t, db, mergeTrainRow("train-1"))
	zoneRefuses(t, db, "a second row of one train", mergeTrainRow("train-1"))
	// the trigger is a BEFORE INSERT guard and never touches a row that already exists: no DELETE or
	// UPDATE of merge_trains can run anyway (the append-only triggers), so the count is what is read.
	var trains int
	if err := db.QueryRow("SELECT count(*) FROM merge_trains").Scan(&trains); err != nil || trains != 1 {
		t.Fatalf("trains = %d (%v), want 1", trains, err)
	}
}

// A 767-era store (the shipped CREATE, no trigger) holding one NULL row upgrades with the row
// preserved: it is not deleted, not rewritten, and a new NULL insert is refused afterwards. The row
// is never read as a train because every reader addresses a train by a non-empty id.
func TestDAGZoneUpgradePreservesAnExistingNullTrainRow(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	db := zoneRawDB(t, path)
	if _, err := db.Exec(mergeTrainsCreate); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO merge_trains (train_id, target_key, repository, base_ref, base_sha, leader_task_id, created_at)" +
		" VALUES (NULL,'o/r|dev','o/r','dev','" + zoneDigest + "','task-leader','2026-10-06T00:00:00Z')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// opening the store runs the zone statements: the CREATE is skipped (IF NOT EXISTS) and the
	// trigger is appended, so the pre-existing row is left exactly as it was
	s, err := Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	upgraded := zoneRawDB(t, path)
	var preserved int
	if err := upgraded.QueryRow("SELECT count(*) FROM merge_trains WHERE train_id IS NULL").Scan(&preserved); err != nil || preserved != 1 {
		t.Fatalf("the NULL row after the upgrade = %d (%v), want 1 preserved", preserved, err)
	}
	zoneRefuses(t, upgraded, "a new NULL train id after the upgrade", "INSERT INTO merge_trains (train_id, target_key, repository, base_ref, base_sha, leader_task_id, created_at)"+
		" VALUES (NULL,'o/r|dev','o/r','dev','"+zoneDigest+"','task-leader','2026-10-06T00:00:01Z')")
	// no reader finds it: MergeTrain looks a train up by its non-empty id, and the writer never
	// writes one, so the row is invisible to open and show alike
	if _, found, err := MergeTrain(context.Background(), s, "train-1"); err != nil || found {
		t.Fatalf("the NULL row was read as a train: found=%v (%v)", found, err)
	}
}

// The three tables are append-only: an UPDATE and a DELETE abort on each, a train is written once,
// a member and an event are unique at their sequence number, the kind is one of the five the decision
// names, and a member or an event of a train that does not exist is the foreign key's refusal.
func TestDAGZoneMergeTrainTablesAreAppendOnly(t *testing.T) {
	t.Parallel()
	db := zoneOpenedDB(t)
	zoneMustExec(t, db, mergeTrainRow("train-1"))
	zoneRefuses(t, db, "a second row of one train", mergeTrainRow("train-1"))
	zoneRefuses(t, db, "a train with an empty id", mergeTrainRow(""))
	zoneMustExec(t, db, mergeTrainMemberRow("train-1", 1, "turn-1"), mergeTrainMemberRow("train-1", 2, "turn-2"))
	zoneRefuses(t, db, "a second member at one sequence number", mergeTrainMemberRow("train-1", 1, "turn-3"))
	zoneRefuses(t, db, "a member with sequence 0", mergeTrainMemberRow("train-1", 0, "turn-3"))
	zoneRefuses(t, db, "a member of a train that does not exist", mergeTrainMemberRow("train-none", 1, "turn-1"))
	zoneMustExec(t, db, mergeTrainEventRow("train-1", 1, "opened"), mergeTrainEventRow("train-1", 2, "verified"))
	zoneRefuses(t, db, "a second event at one sequence number", mergeTrainEventRow("train-1", 1, "landed"))
	zoneRefuses(t, db, "an event with sequence 0", mergeTrainEventRow("train-1", 0, "opened"))
	zoneRefuses(t, db, "an event of a train that does not exist", mergeTrainEventRow("train-none", 1, "opened"))
	zoneRefuses(t, db, "an event kind outside the five", mergeTrainEventRow("train-1", 3, "started"))
	zoneRefuses(t, db, "an event with an empty actor", "INSERT INTO merge_train_events VALUES ('train-1',4,'opened','','{}','2026-10-06T00:00:00Z')")
	zoneRefuses(t, db, "an event with an empty detail", "INSERT INTO merge_train_events VALUES ('train-1',5,'opened','task-a','','2026-10-06T00:00:00Z')")
	zoneRefuses(t, db, "an UPDATE of a train", "UPDATE merge_trains SET base_sha = 'h9' WHERE train_id = 'train-1'")
	zoneRefuses(t, db, "a DELETE of a train", "DELETE FROM merge_trains")
	zoneRefuses(t, db, "an UPDATE of a member", "UPDATE merge_train_members SET member_head = 'h9' WHERE train_id = 'train-1'")
	zoneRefuses(t, db, "a DELETE of a member", "DELETE FROM merge_train_members")
	zoneRefuses(t, db, "an UPDATE of an event", "UPDATE merge_train_events SET kind = 'done' WHERE train_id = 'train-1'")
	zoneRefuses(t, db, "a DELETE of an event", "DELETE FROM merge_train_events")
	var trains, members, events int
	if err := db.QueryRow("SELECT (SELECT count(*) FROM merge_trains), (SELECT count(*) FROM merge_train_members),"+
		" (SELECT count(*) FROM merge_train_events)").Scan(&trains, &members, &events); err != nil || trains != 1 || members != 2 || events != 2 {
		t.Fatalf("rows = %d trains, %d members, %d events (%v), want 1, 2, 2", trains, members, events, err)
	}
}

// The rows round-trip through the store, in the caller's transaction: a train, its members and its
// events read back value for value, the state is the newest event's kind, and a store that predates
// the tables answers absent rather than failing.
func TestDAGMergeTrainRoundTrips(t *testing.T) {
	t.Parallel()
	s := recordStore(t)
	ctx := context.Background()
	train := MergeTrainRow{TrainID: "train-1", TargetKey: "o/r|dev", Repository: "o/r", BaseRef: "dev",
		BaseSHA: zoneDigest, LeaderTaskID: "task-leader", CreatedAt: "2026-10-06T00:00:00Z"}
	if err := RecordMergeTrain(ctx, s, train); err != nil {
		t.Fatal(err)
	}
	read, found, err := MergeTrain(ctx, s, train.TrainID)
	if err != nil || !found || !reflect.DeepEqual(read, train) {
		t.Fatalf("read back %+v found=%v (%v), want %+v", read, found, err, train)
	}
	// a second train row is the primary key's refusal
	if err := RecordMergeTrain(ctx, s, train); err == nil {
		t.Fatal("a second row of one train was written")
	}
	member := MergeTrainMemberRow{TrainID: train.TrainID, Seq: 1, TurnID: "turn-1", PRNumber: 701,
		RelationshipID: "rel-1", MemberHead: zoneDigest}
	second := MergeTrainMemberRow{TrainID: train.TrainID, Seq: 2, TurnID: "turn-2", PRNumber: 702,
		RelationshipID: "rel-2", MemberHead: zoneDigest}
	for _, m := range []MergeTrainMemberRow{member, second} {
		if err := RecordMergeTrainMember(ctx, s, m); err != nil {
			t.Fatal(err)
		}
	}
	members, err := MergeTrainMembers(ctx, s, train.TrainID)
	if err != nil || !reflect.DeepEqual(members, []MergeTrainMemberRow{member, second}) {
		t.Fatalf("members = %+v (%v)", members, err)
	}
	readMember, found, err := MergeTrainMember(ctx, s, train.TrainID, 2)
	if err != nil || !found || !reflect.DeepEqual(readMember, second) {
		t.Fatalf("member 2 = %+v found=%v (%v)", readMember, found, err)
	}
	if _, found, err := MergeTrainMember(ctx, s, train.TrainID, 9); err != nil || found {
		t.Fatalf("a member the train does not hold: found=%v (%v)", found, err)
	}
	// the train opens, verifies and lands: three events, and the state is the newest event's kind
	if state, found, err := MergeTrainState(ctx, s, train.TrainID); err != nil || found || state != "" {
		t.Fatalf("a train with no event: state=%q found=%v (%v)", state, found, err)
	}
	log := []MergeTrainEventRow{
		{TrainID: train.TrainID, Seq: 1, Kind: MergeTrainOpened, Actor: "task-leader", DetailJSON: "{}", RecordedAt: "2026-10-06T00:00:01Z"},
		{TrainID: train.TrainID, Seq: 2, Kind: MergeTrainVerified, Actor: "task-leader", DetailJSON: `{"seq":1,"check_id":"c1"}`, RecordedAt: "2026-10-06T00:00:02Z"},
		{TrainID: train.TrainID, Seq: 3, Kind: MergeTrainLanded, Actor: "task-leader", DetailJSON: `{"seq":1,"landed_sha":"` + zoneDigest + `"}`, RecordedAt: "2026-10-06T00:00:03Z"},
	}
	for _, event := range log {
		if err := RecordMergeTrainEvent(ctx, s, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := RecordMergeTrainEvent(ctx, s, log[0]); err == nil {
		t.Fatal("a repeated event sequence number was written")
	}
	events, err := MergeTrainEvents(ctx, s, train.TrainID)
	if err != nil || !reflect.DeepEqual(events, log) {
		t.Fatalf("events = %+v (%v)", events, err)
	}
	readEvent, found, err := MergeTrainEvent(ctx, s, train.TrainID, 2)
	if err != nil || !found || !reflect.DeepEqual(readEvent, log[1]) {
		t.Fatalf("event 2 = %+v found=%v (%v)", readEvent, found, err)
	}
	if state, found, err := MergeTrainState(ctx, s, train.TrainID); err != nil || !found || state != MergeTrainLanded {
		t.Fatalf("state after landing = %q found=%v (%v), want %q", state, found, err, MergeTrainLanded)
	}
	// the close appends the last event and the state follows it, and no earlier row is rewritten
	closed := MergeTrainEventRow{TrainID: train.TrainID, Seq: 4, Kind: MergeTrainDone, Actor: "task-leader", DetailJSON: "{}", RecordedAt: "2026-10-06T00:00:04Z"}
	if err := RecordMergeTrainEvent(ctx, s, closed); err != nil {
		t.Fatal(err)
	}
	if state, found, err := MergeTrainState(ctx, s, train.TrainID); err != nil || !found || state != MergeTrainDone {
		t.Fatalf("state after the close = %q found=%v (%v), want %q", state, found, err, MergeTrainDone)
	}
	if read, found, err := MergeTrain(ctx, s, train.TrainID); err != nil || !found || !reflect.DeepEqual(read, train) {
		t.Fatalf("the train row changed: %+v found=%v (%v)", read, found, err)
	}
	// a member of a train that does not exist is the foreign key's refusal through the writer too
	missing := member
	missing.TrainID, missing.Seq, missing.TurnID = "train-none", 1, "turn-9"
	if err := RecordMergeTrainMember(ctx, s, missing); err == nil {
		t.Fatal("a member of a train that does not exist was written")
	}
}

// A store that predates the tables (the shape every existing store has) answers absent from the
// readers instead of failing: the tables arrive with the first write open, and a read-only reader of
// such a store must still answer.
func TestDAGMergeTrainReadersOnAStoreWithoutTheZone(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	s, err := OpenReadOnlyStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	if _, found, err := MergeTrain(ctx, s, "train-1"); err != nil || found {
		t.Fatalf("a store without the tables answered found=%v (%v)", found, err)
	}
	if rows, err := MergeTrainMembers(ctx, s, "train-1"); err != nil || len(rows) != 0 {
		t.Fatalf("a store without the tables answered %+v (%v)", rows, err)
	}
	if _, found, err := MergeTrainMember(ctx, s, "train-1", 1); err != nil || found {
		t.Fatalf("a store without the tables answered found=%v (%v)", found, err)
	}
	if rows, err := MergeTrainEvents(ctx, s, "train-1"); err != nil || len(rows) != 0 {
		t.Fatalf("a store without the tables answered %+v (%v)", rows, err)
	}
	if _, found, err := MergeTrainEvent(ctx, s, "train-1", 1); err != nil || found {
		t.Fatalf("a store without the tables answered found=%v (%v)", found, err)
	}
	if state, found, err := MergeTrainState(ctx, s, "train-1"); err != nil || found || state != "" {
		t.Fatalf("a store without the tables answered state=%q found=%v (%v)", state, found, err)
	}
}

// The writer joins the caller's transaction: a train written inside a transaction that is rolled back
// leaves no row, and one written inside a transaction that commits leaves exactly one.
func TestDAGMergeTrainWriterJoinsTheCallersTransaction(t *testing.T) {
	t.Parallel()
	s := recordStore(t)
	ctx := context.Background()
	row := MergeTrainRow{TrainID: "train-rollback", TargetKey: "o/r|dev", Repository: "o/r", BaseRef: "dev",
		BaseSHA: zoneDigest, LeaderTaskID: "task-leader", CreatedAt: "2026-10-06T00:00:00Z"}
	errRollback := errors.New("rolled back")
	if err := s.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		if err := RecordMergeTrain(txCtx, s, row); err != nil {
			return err
		}
		return errRollback
	}); !errors.Is(err, errRollback) {
		t.Fatalf("transaction = %v, want the rollback", err)
	}
	if _, found, err := MergeTrain(ctx, s, row.TrainID); err != nil || found {
		t.Fatalf("a rolled-back train was read: found=%v (%v)", found, err)
	}
	if err := s.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		return RecordMergeTrain(txCtx, s, row)
	}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := MergeTrain(ctx, s, row.TrainID); err != nil || !found {
		t.Fatalf("a committed train was not read: found=%v (%v)", found, err)
	}
}
