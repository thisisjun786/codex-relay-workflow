package delivery

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1664: a failed automatic probe leaves a recipient-less preparation
// failure row, which is never accepted as wake proof.
func TestWakeProbeFailureEvidenceIsNotProof(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "socket")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	settings := json.RawMessage(`{"approvalPolicy":"never"}`)
	failed := WakeAttempt{
		RequestID: "probe-req", RecipientTaskID: "", Probe: true, Reason: "creation unsupported",
		DeliveryMethod: "turn/start", DeliveryState: WithheldPreSend, SendAttempted: "no",
		Unattended: true, RequiredSettings: settings, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := RecordWakeAttempt(ctx, s, failed); err != nil {
		t.Fatalf("probe preparation failure was not recorded: %v", err)
	}
	want := &WakeExpectation{
		Identity: WakeIdentity{OperatingScope: "scope", AppServerInstance: "server", Socket: "socket", ServiceInstance: "service",
			ApprovalPolicy: "never", RolePolicy: "policy", Role: "parent", PairSource: "role", TransportRevision: "rev", Disposition: "session_step_waiting"},
		RequiredSettings: settings,
	}
	if evidence, reason, err := FindWakeEvidence(ctx, s, want, time.Now()); err != nil || evidence != nil {
		t.Fatalf("probe failure claimed proof: %+v %q %v", evidence, reason, err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*WakeAttempt)
	}{
		{"not a probe", func(a *WakeAttempt) { a.Probe = false }},
		{"send attempted", func(a *WakeAttempt) { a.SendAttempted = "yes" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := failed
			tc.mutate(&bad)
			bad.RequestID = "probe-req-" + tc.name
			if err := RecordWakeAttempt(ctx, s, bad); err == nil {
				t.Fatal("recipient-less non-preparation row was retained")
			}
		})
	}
}
