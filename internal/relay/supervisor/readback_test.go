package supervisor

import (
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

func delivered24(t *testing.T) (*stageFixture, *sendHost, string, map[string]any) {
	t.Helper()
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, result := f.staged(t)
	id := result["messageId"].(string)
	h := &sendHost{status: "idle"}
	sent, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	if err != nil {
		t.Fatal(err)
	}
	return f, h, id, sent
}
func Test24_SCH_11_SeparateQueues(t *testing.T) {
	f := fixture24(t)
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	if id == f.event {
		t.Fatal("one id for distinct queues")
	}
	var count int
	if err := f.s.DB.QueryRowContext(f.ctx, "SELECT count(*) FROM deliveries WHERE event_id=?", id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("supervisor id in deliveries: %d %v", count, err)
	}
}
func Test24_SCH_12_EligibleOldestFirst(t *testing.T) {
	f := fixture24(t)
	_, stage := f.staged(t)
	rows, err := f.c.Eligible(f.ctx, 1_700_000_001, 4)
	if err != nil || len(rows) != 1 || rows[0].MessageID != stage["messageId"] {
		t.Fatalf("eligible %v %v", rows, err)
	}
}
func Test24_SCH_13_SendReadRoundtrip(t *testing.T) {
	f, h, id, send := delivered24(t)
	answer, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", h, 1_700_000_002)
	if err != nil {
		t.Fatal(err)
	}
	if answer["verified"] != "host_read" || answer["delivered"].(map[string]any)["found"] != true || !strings.HasPrefix(answer["delivered"].(map[string]any)["token"].(string), send["requestId"].(string)+".") {
		t.Fatalf("readback %v", answer)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil || row.State != "read" {
		t.Fatalf("message %v %v", row, err)
	}
}
func Test24_SCH_14_DeliveredBytesCannotContainProof(t *testing.T) {
	_, h, id, _ := delivered24(t)
	bytes := h.sends[0]
	if !strings.Contains(bytes, id) || strings.Contains(bytes, "turn-supervisor-1") || strings.Contains(bytes, Proof(id, "turn-supervisor-1")) {
		t.Fatalf("proof leaked to message %q", bytes)
	}
}
func Test24_SCH_15_InvalidProofAndPreSendRefused(t *testing.T) {
	f := fixture24(t)
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	_, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", &sendHost{status: "idle"}, 1_700_000_001)
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Reason != "not_claimable" {
		t.Fatalf("pre-send: %v", err)
	}
	f.c.Settings = &delivery.TaskSettings{}
	h := &sendHost{status: "idle"}
	if _, err = f.c.Attempt(f.ctx, id, h, 1_700_000_000); err != nil {
		t.Fatal(err)
	}
	_, err = f.c.ReadBack(f.ctx, id, "turn-supervisor-1", "wrong", "", h, 1_700_000_002)
	if !errors.As(err, &refusal) || refusal.Reason != "ack_proof_mismatch" {
		t.Fatalf("wrong proof: %v", err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil || row.State != "dispatched" {
		t.Fatalf("state %v %v", row, err)
	}
}
func Test24_SCH_16_UnverifiedTurnsAndTranscript(t *testing.T) {
	for _, tc := range []struct {
		name, turn           string
		hostless, unreadable bool
		want                 string
	}{{"missing", "not-a-turn", false, false, "turn_not_found"}, {"no-host", "turn-supervisor-1", true, false, "unverified_turn"}, {"empty-transcript", "turn-supervisor-1", false, false, "transcript_unconfirmed"}, {"unreadable-transcript", "turn-supervisor-1", false, true, "transcript_unconfirmed"}} {
		t.Run(tc.name, func(t *testing.T) {
			f, h, id, _ := delivered24(t)
			if tc.unreadable {
				h.failTranscript = true
			} else {
				h.items = map[string]string{}
			}
			var adapter SendAdapter = h
			if tc.hostless {
				adapter = nil
			}
			answer, err := f.c.ReadBack(f.ctx, id, tc.turn, Proof(id, tc.turn), "", adapter, 1_700_000_002)
			if err != nil || answer["verified"] != tc.want {
				t.Fatalf("readback %v %v", answer, err)
			}
			row, err := f.c.Get(f.ctx, id)
			if err != nil || row.State != "dispatched" {
				t.Fatalf("message %v %v", row, err)
			}
			readback, err := f.s.SupervisorReadback(f.ctx, id)
			if err != nil || readback.Verified != tc.want {
				t.Fatalf("stored %v %v", readback, err)
			}
		})
	}
}
func Test24_SCH_17_TurnOrigin(t *testing.T) {
	f, h, id, _ := delivered24(t)
	answer, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", h, 1_700_000_002)
	if err != nil || answer["turnOrigin"] != "relay_opened" {
		t.Fatalf("origin %v %v", answer, err)
	}
}
func Test24_SCH_18_SettledReadbackWins(t *testing.T) {
	f, h, id, _ := delivered24(t)
	first, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", h, 1_700_000_002)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.c.ReadBack(f.ctx, id, "turn-any-other", "invalid", "", nil, 1_700_000_003)
	if err != nil || second["recorded"] != false || second["verified"] != "host_read" || second["readTurnId"] != first["readTurnId"] {
		t.Fatalf("settled %v %v", second, err)
	}
	var count int
	if err := f.s.DB.QueryRowContext(f.ctx, "SELECT count(*) FROM supervisor_readbacks WHERE message_id=?", id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows %d %v", count, err)
	}
}
func Test24_SCH_20_ShowWholeRecord(t *testing.T) {
	f, h, id, _ := delivered24(t)
	if _, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", h, 1_700_000_002); err != nil {
		t.Fatal(err)
	}
	shown, err := f.c.Show(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if shown["state"] != "read" || shown["recipient"] != "supervisor" || len(shown["attempts"].([]any)) != 1 || shown["readback"].(map[string]any)["verified"] != "host_read" || shown["packet"].(map[string]any)["version"] != packetVersion || !strings.Contains(shown["limits"].(string), "Linear record") {
		t.Fatalf("shown %v", shown)
	}
}
func Test24_SCH_19_ReadDoesNotDischargeObligation(t *testing.T) {
	f, h, id, _ := delivered24(t)
	if _, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", h, 1_700_000_002); err != nil {
		t.Fatal(err)
	}
	o := f.obligation(t)
	can, reason, err := f.c.Reportable(f.ctx, o)
	if err != nil || can || reason != "already_reported_under_this_obligation" {
		t.Fatalf("reportable %v %s %v", can, reason, err)
	}
}
