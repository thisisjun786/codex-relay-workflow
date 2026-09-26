package managed

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func fixtureReservation(t *testing.T) (Reservation, Identity) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return Reservation{Store: s, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }}, Identity{RequestID: "req-1", IssueKey: "REL-1", Fingerprint: "fp-1", Version: Schema, Workspace: "/tmp/work", MarkerRoot: "/tmp/markers", SocketIdentity: "/tmp/socket", CreateRequestID: "create-1", DispatchRequestID: "business-1"}
}
func reasonIs(t *testing.T, err error, reason string) {
	t.Helper()
	var refused *store.RefusedError
	if !errors.As(err, &refused) || refused.Reason != reason {
		t.Fatalf("want %s, got %v", reason, err)
	}
}
func Test27_MRS_1_ReservationReplayAndCheckpoint(t *testing.T) {
	r, id := fixtureReservation(t)
	ctx := context.Background()
	first, err := r.Reserve(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != "reserved" || first.Revision != 0 || first.ChildTaskID.Valid || first.StandbyTurnID.Valid {
		t.Fatal(first)
	}
	again, err := r.Reserve(ctx, id)
	if err != nil || again.CreatedAt != first.CreatedAt || again.Revision != 0 {
		t.Fatal(again, err)
	}
	id.Fingerprint = "different"
	_, err = r.Reserve(ctx, id)
	reasonIs(t, err, "relationship_conflict")
	id.Fingerprint = "fp-1"
	armed, err := r.Arm(ctx, id.RequestID, id.Fingerprint, 0)
	if err != nil || armed.State != "create_armed" || armed.Revision != 1 {
		t.Fatal(armed, err)
	}
	_, err = r.Receipt(ctx, id.RequestID, id.Fingerprint, map[string]any{"status": "accepted", "threadId": "child", "turnId": "standby"})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := r.Reserve(ctx, id)
	if err != nil || replayed.ChildTaskID.String != "child" || replayed.StandbyTurnID.String != "standby" {
		t.Fatal(replayed, err)
	}
}
func Test27_MRS_2_OnePendingRequestPerIssue(t *testing.T) {
	r, id := fixtureReservation(t)
	ctx := context.Background()
	if _, err := r.Reserve(ctx, id); err != nil {
		t.Fatal(err)
	}
	id.RequestID = "req-2"
	id.Fingerprint = "fp-2"
	_, err := r.Reserve(ctx, id)
	reasonIs(t, err, "duplicate_assignment")
	row, err := r.Store.PendingManagedStart(ctx, id.IssueKey)
	if err != nil || row.RequestID != "req-1" {
		t.Fatal(row, err)
	}
	_, err = (&registry.Registry{Store: r.Store, Now: r.Now}).Register(ctx, registry.Registration{Parent: registry.Endpoint{TaskID: "parent", HostID: "host"}, Child: registry.Endpoint{TaskID: "other", HostID: "host"}, IssueKey: id.IssueKey, ArtifactRoots: []string{"/work"}, AllowedRecipients: []string{"parent"}, DispatchRequestID: "raw"})
	reasonIs(t, err, "duplicate_assignment")
}
func Test27_MRS_2_ActiveAndPausedOwnersExcludeReservation(t *testing.T) {
	r, id := fixtureReservation(t)
	ctx := context.Background()
	reg := &registry.Registry{Store: r.Store, Now: r.Now}
	record, err := reg.Register(ctx, registry.Registration{Parent: registry.Endpoint{TaskID: "parent", HostID: "host"}, Child: registry.Endpoint{TaskID: "child", HostID: "host"}, IssueKey: id.IssueKey, ArtifactRoots: []string{"/work"}, AllowedRecipients: []string{"parent"}, DispatchRequestID: "live"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Reserve(ctx, id)
	reasonIs(t, err, "duplicate_assignment")
	if _, err = reg.SetStatus(ctx, record.ID, "paused", "operator"); err != nil {
		t.Fatal(err)
	}
	_, err = r.Reserve(ctx, id)
	reasonIs(t, err, "duplicate_assignment")
}

func Test27_MRS_2_ResumeCannotPassPendingReservation(t *testing.T) {
	r, id := fixtureReservation(t)
	ctx := context.Background()
	if _, err := r.Reserve(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.DB.ExecContext(ctx, "UPDATE managed_start_requests SET state='released',revision=1,release_reason='make-room' WHERE request_id=?", id.RequestID); err != nil {
		t.Fatal(err)
	}
	reg := &registry.Registry{Store: r.Store, Now: r.Now}
	record, err := reg.Register(ctx, registry.Registration{Parent: registry.Endpoint{TaskID: "parent", HostID: "host"}, Child: registry.Endpoint{TaskID: "child", HostID: "host"}, IssueKey: id.IssueKey, ArtifactRoots: []string{"/work"}, AllowedRecipients: []string{"parent"}, DispatchRequestID: "live"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.SetStatus(ctx, record.ID, "paused", "operator"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.DB.ExecContext(ctx, "UPDATE managed_start_requests SET state='reserved',revision=0,release_reason=NULL WHERE request_id=?", id.RequestID); err != nil {
		t.Fatal(err)
	}
	_, err = reg.Resume(ctx, record.ID, 1, []string{"/work"}, []string{"parent"}, "operator")
	reasonIs(t, err, "duplicate_assignment")
	var status string
	if err := r.Store.DB.QueryRowContext(ctx, "SELECT status FROM relationships WHERE relationship_id=?", record.ID).Scan(&status); err != nil || status != "paused" {
		t.Fatalf("status=%s: %v", status, err)
	}
}

func Test27_MRS_2_PendingIndexIsDatabaseConstraint(t *testing.T) {
	r, id := fixtureReservation(t)
	ctx := context.Background()
	if _, err := r.Reserve(ctx, id); err != nil {
		t.Fatal(err)
	}
	_, err := r.Store.DB.ExecContext(ctx, `INSERT INTO managed_start_requests(request_id,issue_key,request_fingerprint,fingerprint_version,workspace,marker_root,socket_identity,create_request_id,dispatch_request_id,state,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,'reserved',0,?,?)`, "other", id.IssueKey, "other", id.Version, id.Workspace, id.MarkerRoot, id.SocketIdentity, "other-create", "other-dispatch", r.Now(), r.Now())
	if err == nil {
		t.Fatal("partial unique index admitted a second pending request")
	}
	var count int
	if err := r.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM managed_start_requests WHERE issue_key=? AND state IN ('reserved','create_armed')", id.IssueKey).Scan(&count); err != nil || count != 1 {
		t.Fatalf("pending count %d: %v", count, err)
	}
}

func Test27_MRS_2_TwoConnectionsReserveOneIssue(t *testing.T) {
	r, id := fixtureReservation(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	start := make(chan struct{})
	outcomes := make(chan error, 2)
	for _, name := range []string{"a", "b"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			s, err := store.Open(ctx, r.Store.Path, "")
			if err != nil {
				outcomes <- err
				return
			}
			defer s.Close()
			in := id
			in.RequestID = name
			<-start
			_, err = (Reservation{Store: s, Now: r.Now}).Reserve(ctx, in)
			outcomes <- err
		}(name)
	}
	close(start)
	wg.Wait()
	close(outcomes)
	wins, losses := 0, 0
	for err := range outcomes {
		if err == nil {
			wins++
		} else {
			var refused *store.RefusedError
			if !errors.As(err, &refused) || refused.Reason != "duplicate_assignment" {
				t.Fatalf("unexpected loser: %v", err)
			}
			losses++
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("wins=%d losses=%d", wins, losses)
	}
	var count int
	if err := r.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM managed_start_requests WHERE issue_key=? AND state IN ('reserved','create_armed')", id.IssueKey).Scan(&count); err != nil || count != 1 {
		t.Fatalf("pending count %d: %v", count, err)
	}
}

func Test27_MRS_4_ArmAndReleaseRaceHasOneWinner(t *testing.T) {
	r, id := fixtureReservation(t)
	ctx := context.Background()
	if _, err := r.Reserve(ctx, id); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, action := range []string{"arm", "release"} {
		wg.Add(1)
		go func(action string) {
			defer wg.Done()
			s, err := store.Open(ctx, r.Store.Path, "")
			if err != nil {
				results <- err
				return
			}
			defer s.Close()
			reservation := Reservation{Store: s, Now: r.Now}
			<-start
			if action == "arm" {
				_, err = reservation.Arm(ctx, id.RequestID, id.Fingerprint, 0)
			} else {
				_, err = reservation.Release(ctx, id.RequestID, id.Fingerprint, 0, "operator")
			}
			results <- err
		}(action)
	}
	close(start)
	wg.Wait()
	close(results)
	winners, losers := 0, 0
	for err := range results {
		if err == nil {
			winners++
		} else {
			var refused *store.RefusedError
			if !errors.As(err, &refused) || refused.Reason != "relationship_conflict" {
				t.Fatalf("unexpected losing result: %v", err)
			}
			losers++
		}
	}
	row, err := r.get(ctx, id.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if winners != 1 || losers != 1 || row.Revision != 1 || (row.State != "released" && row.State != "create_armed") {
		t.Fatalf("winners=%d losers=%d row=%+v", winners, losers, row)
	}
}

func Test27_MRS_3_OnlyAcceptedReceiptPublishesChild(t *testing.T) {
	r, id := fixtureReservation(t)
	ctx := context.Background()
	if _, err := r.Reserve(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Arm(ctx, id.RequestID, id.Fingerprint, 0); err != nil {
		t.Fatal(err)
	}
	for _, receipt := range []map[string]any{{"status": "outcome_unknown", "threadId": "child"}, {"status": "accepted", "threadId": "child"}} {
		row, err := r.Receipt(ctx, id.RequestID, id.Fingerprint, receipt)
		if err != nil || row.ChildTaskID.Valid || row.StandbyTurnID.Valid {
			t.Fatal(row, err)
		}
	}
	row, err := r.Receipt(ctx, id.RequestID, id.Fingerprint, map[string]any{"status": "accepted", "threadId": "child", "turnId": "standby"})
	if err != nil || row.ChildTaskID.String != "child" || row.StandbyTurnID.String != "standby" {
		t.Fatal(row, err)
	}
	_, err = r.Receipt(ctx, id.RequestID, id.Fingerprint, map[string]any{"status": "accepted", "threadId": "other", "turnId": "standby"})
	reasonIs(t, err, "relationship_conflict")
	input := registry.Registration{Parent: registry.Endpoint{TaskID: "parent", HostID: "host"}, Child: registry.Endpoint{TaskID: "child", HostID: "host"}, IssueKey: id.IssueKey, ArtifactRoots: []string{"/work"}, AllowedRecipients: []string{"parent"}, DispatchRequestID: id.DispatchRequestID, DispatchTurnID: sql.NullString{String: "standby", Valid: true}, ManagedRequestID: id.RequestID}
	input.Child.TaskID = "other"
	_, err = (&registry.Registry{Store: r.Store, Now: r.Now}).Register(ctx, input)
	reasonIs(t, err, "relationship_conflict")
	input.Child.TaskID = "child"
	record, err := (&registry.Registry{Store: r.Store, Now: r.Now}).Register(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	attached, err := r.Reserve(ctx, id)
	if err != nil || attached.State != "attached" || attached.RelationshipID.String != record.ID || attached.ExecutionGeneration.Int64 != 1 {
		t.Fatal(attached, err)
	}
	_, err = r.Receipt(ctx, id.RequestID, id.Fingerprint, map[string]any{"status": "accepted", "threadId": "child", "turnId": "standby"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&registry.Registry{Store: r.Store, Now: r.Now}).Register(ctx, input)
	if err != nil {
		t.Fatalf("attached replay: %v", err)
	}
}
func Test27_MRS_4_ReleaseAndArmSameRevision(t *testing.T) {
	r, id := fixtureReservation(t)
	ctx := context.Background()
	if _, err := r.Reserve(ctx, id); err != nil {
		t.Fatal(err)
	}
	row, err := r.Release(ctx, id.RequestID, id.Fingerprint, 0, "cancelled")
	if err != nil || row.State != "released" || row.Revision != 1 || row.ReleaseReason.String != "cancelled" {
		t.Fatal(row, err)
	}
	_, err = r.Arm(ctx, id.RequestID, id.Fingerprint, 0)
	reasonIs(t, err, "relationship_conflict")
	_, err = r.Reserve(ctx, id)
	reasonIs(t, err, "relationship_conflict")
	second := id
	second.RequestID = "req-2"
	second.IssueKey = "REL-2"
	if _, err := r.Reserve(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Arm(ctx, second.RequestID, second.Fingerprint, 0); err != nil {
		t.Fatal(err)
	}
	_, err = r.Release(ctx, second.RequestID, second.Fingerprint, 1, "after-arm")
	reasonIs(t, err, "relationship_conflict")
	retained, err := r.get(ctx, second.RequestID)
	if err != nil || retained.State != "create_armed" {
		t.Fatal(retained, err)
	}
}
