package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func TestWakeAttemptAppendsHostEvidenceAndJoinsTransaction(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "socket")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	clock := &FakeClock{T: 1000}
	host := newFakeHost(clock)
	host.addThread("owner")
	life := Observe(ctx, host, "owner", nil, true)
	if !life.MaySend() || life.RuntimeStatus != "idle" {
		t.Fatalf("fake host before send: %+v", life)
	}
	raw := `{"approvalPolicy":"never","sandbox":{"type":"readOnly"},"cwd":"/work","runtimeWorkspaceRoots":["/work"],"model":"model-a","reasoningEffort":"high","environments":[]}`
	settings := &TaskSettings{Data: loadsObj(raw)}
	receipt, err := host.SendMessage(ctx, "request", "owner", "wake", settings)
	if err != nil {
		t.Fatal(err)
	}
	facts := Classify(receipt)
	turn, _ := facts.TurnID.(string)
	if turn == "" || len(host.threads["owner"].turns) != 1 {
		t.Fatal("host did not open a new turn")
	}
	a := WakeAttempt{
		RequestID: "request", RecipientTaskID: "owner", RecipientStatusBefore: life.RuntimeStatus.(string),
		TurnID: turn, NewTurn: true, DeliveryMethod: "turn/start", DeliveryState: facts.DeliveryState, SendAttempted: facts.SendAttempted,
		Unattended: true, RequiredSettings: json.RawMessage(dumps(settings.Data)), ObservedAt: clock.ISO(),
		Identity: WakeIdentity{OperatingScope: "scope", AppServerInstance: "server", Socket: "socket", ServiceInstance: "service",
			ApprovalPolicy: "never", RolePolicy: "policy", Role: "parent", PairSource: "role", TransportRevision: "revision", Disposition: "session_step_waiting"},
	}
	want := &WakeExpectation{Identity: a.Identity, RequiredSettings: json.RawMessage(raw), RequiredStatus: "idle"}
	if err := RecordWakeAttempt(ctx, s, a); err != nil {
		t.Fatal(err)
	}
	var first string
	if err := s.DB.QueryRowContext(ctx, `SELECT detail FROM journal WHERE kind=?`, WakeObserved).Scan(&first); err != nil {
		t.Fatal(err)
	}
	// A later answer to the same request must not overwrite the first observation.
	a.DeliveryState, a.NewTurn = HeldUncertain, false
	if err := RecordWakeAttempt(ctx, s, a); err != nil {
		t.Fatal(err)
	}
	var count int
	var retained string
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM journal WHERE kind=?`, WakeObserved).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT detail FROM journal WHERE kind=? ORDER BY seq LIMIT 1`, WakeObserved).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if count != 2 || retained != first {
		t.Fatalf("wake observations were overwritten: count=%d", count)
	}
	evidence, reason, err := FindWakeEvidence(ctx, s, want, time.Now())
	if err != nil || evidence == nil || evidence.TurnID != turn {
		t.Fatalf("host wake lost: %+v %s %v", evidence, reason, err)
	}
	rollback := errors.New("settlement failed")
	err = s.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		if err := RecordWakeAttempt(tx, s, a); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("transaction: %v", err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM journal WHERE kind=?`, WakeObserved).Scan(&count); err != nil || count != 2 {
		t.Fatalf("rolled-back attempt leaked: count=%d err=%v", count, err)
	}
	// Moving the JSON to another store does not make it evidence for that store.
	other, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "socket")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := journal(ctx, other, WakeObserved, a.RequestID, first, a.ObservedAt); err != nil {
		t.Fatal(err)
	}
	if got, _, err := FindWakeEvidence(ctx, other, want, time.Now()); err != nil || got != nil {
		t.Fatalf("foreign store evidence: %+v %v", got, err)
	}
}
