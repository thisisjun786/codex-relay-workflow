package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

func TestPreflightPendingWakeDoesNotProveTurn(t *testing.T) {
	w := newWorld(t)
	if _, err := w.st.DB.ExecContext(ctx, `INSERT INTO delivery_wakes (event_id, woken_at, original_deadline) VALUES ('pending', 1, 2)`); err != nil {
		t.Fatal(err)
	}
	r := &Reader{St: w.st, State: w.state}
	facts := StoreProber{}.Facts(ctx, PreflightRequest{Reader: r, Thread: "owner", Now: time.Now()})
	if got := facts[4]; got.Level != LevelWarn || got.Pass {
		t.Fatalf("a pending wake has no observed new turn; want warn, got %+v", got)
	}
}

func wakeFixture() (delivery.WakeAttempt, *delivery.WakeExpectation) {
	a := delivery.WakeAttempt{
		RequestID: "wake-request", RecipientTaskID: "probe-recipient", RecipientStatusBefore: "notLoaded",
		TurnID: "new-turn", NewTurn: true, DeliveryState: delivery.Dispatched, SendAttempted: "yes", Unattended: true,
		ObservedAt: "2026-01-01T00:00:00Z", RequiredSettings: json.RawMessage(`{"approvalPolicy":"never","model":"model-a","effort":"high"}`),
		Identity: delivery.WakeIdentity{
			OperatingScope: "host-user", AppServerInstance: "server-instance", Socket: "socket-a", ServiceInstance: "service-instance",
			ApprovalPolicy: "never", RolePolicy: "policy-digest", Role: "parent", PairSource: "role",
			TransportRevision: "transport-revision", Disposition: "session_step_waiting",
		},
	}
	return a, &delivery.WakeExpectation{Identity: a.Identity, RequiredSettings: a.RequiredSettings}
}

func TestPreflightRecordedWakeSurvivesConsumptionAndReopen(t *testing.T) {
	for _, status := range []string{"idle", "notLoaded"} {
		t.Run(status, func(t *testing.T) {
			w := newWorld(t)
			a, want := wakeFixture()
			a.RecipientStatusBefore, want.RequiredStatus = status, status
			// Directly exercise the delivery recording seam; the supervisor sender
			// wiring is deliberately deferred until its concurrent change lands.
			if err := delivery.RecordWakeAttempt(ctx, w.st, a); err != nil {
				t.Fatal(err)
			}
			if _, err := w.st.DB.ExecContext(ctx, `INSERT INTO delivery_wakes (event_id,woken_at,original_deadline,spent_at) VALUES ('wake-request',1,2,1); DELETE FROM delivery_wakes WHERE spent_at IS NOT NULL`); err != nil {
				t.Fatal(err)
			}
			w.close()
			r, err := OpenReader(ctx, w.state, "")
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			facts := StoreProber{}.Facts(ctx, PreflightRequest{Reader: r, Thread: "owner", Now: time.Now(), Wake: want})
			if got := facts[4]; !got.Pass || got.Level != LevelPass || !strings.Contains(got.Reading, "new-turn") || !strings.Contains(got.Reading, "wake-request") {
				t.Fatalf("the retained wake must prove fact 5: %+v", got)
			}
		})
	}
}

func TestPreflightWakeNeedsMatchingEvidence(t *testing.T) {
	cases := []struct {
		name string
		edit func(*delivery.WakeAttempt, *delivery.WakeExpectation)
	}{
		{"idle does not prove eviction", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.RecipientStatusBefore = "idle" }},
		{"busy steer", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.RecipientStatusBefore = "active" }},
		{"no new turn", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.NewTurn = false }},
		{"missing turn", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.TurnID = "" }},
		{"uncertain", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.DeliveryState = delivery.HeldUncertain }},
		{"not sent", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.SendAttempted = "no" }},
		{"manual send", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.Unattended = false }},
		{"future observation", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.ObservedAt = "2999-01-01T00:00:00Z" }},
		{"operating scope", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.Identity.OperatingScope = "other" }},
		{"server restart", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.Identity.AppServerInstance = "other" }},
		{"socket", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.Identity.Socket = "other" }},
		{"service restart", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.Identity.ServiceInstance = "other" }},
		{"approval policy", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.Identity.ApprovalPolicy = "on-request" }},
		{"role policy", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.Identity.RolePolicy = "other" }},
		{"role", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.Identity.Role = "child" }},
		{"pair origin", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.Identity.PairSource = "exception:other" }},
		{"transport revision", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.Identity.TransportRevision = "other" }},
		{"disposition", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.Identity.Disposition = "completion" }},
		{"missing identity", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.Identity.ServiceInstance = "" }},
		{"unknown current identity", func(_ *delivery.WakeAttempt, w *delivery.WakeExpectation) { w.Identity.ServiceInstance = "" }},
		{"changed settings", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) {
			a.RequiredSettings = json.RawMessage(`{"approvalPolicy":"never","model":"other"}`)
		}},
		{"missing settings", func(a *delivery.WakeAttempt, _ *delivery.WakeExpectation) { a.RequiredSettings = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			a, want := wakeFixture()
			tc.edit(&a, want)
			if err := delivery.RecordWakeAttempt(ctx, w.st, a); err != nil {
				t.Fatal(err)
			}
			facts := StoreProber{}.Facts(ctx, PreflightRequest{Reader: &Reader{St: w.st}, Thread: "owner", Now: time.Now(), Wake: want})
			if got := facts[4]; got.Level != LevelWarn || got.Pass || got.Reason == "" {
				t.Fatalf("unmeasured wake must warn: %+v", got)
			}
		})
	}
}

func TestPreflightUnreadableWakeEvidenceFails(t *testing.T) {
	w := newWorld(t)
	_, want := wakeFixture()
	if _, err := w.st.DB.ExecContext(ctx, `INSERT INTO journal (at,kind,subject,detail) VALUES ('now',?,'request','broken json')`, delivery.WakeObserved); err != nil {
		t.Fatal(err)
	}
	facts := StoreProber{}.Facts(ctx, PreflightRequest{Reader: &Reader{St: w.st}, Thread: "owner", Now: time.Now(), Wake: want})
	if got := facts[4]; got.Level != LevelFail || got.Pass {
		t.Fatalf("unreadable evidence must fail: %+v", got)
	}
}
