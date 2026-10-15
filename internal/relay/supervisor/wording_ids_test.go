package supervisor

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
)

// CRW-1476 (bundle n2-layer-words, plan D8): the relay's ids hash only the direction, the relation,
// the kind or purpose, the subject (and a fault's id and cycle), never the human text, so rewording
// the 부모 channel's text moves no id. The hex literals below were taken at dev 7f21f0069, before the
// rewording; this test is green before and after it.
func TestSupervisorIDsIgnoreWording(t *testing.T) {
	t.Parallel()
	const (
		wantObligation = "c9ad57a34d7ea4b1ad4771ccd229f2f2"
		wantMessage    = "9f04d2e8b5921fb7fd85a9e28b3a4024"
		wantRequest    = "sup-9f04d2e8b592-a1"
		wantNotice     = "f9e8daa8ee6162a1917426325e8ab82c"
		wantDirect     = "dcff122105bc7351bdef56bd7fda8570"
		wantReception  = wantMessage
	)
	check := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %q, want %q (an id moved with the wording)", what, got, want)
		}
	}

	// A staged report and its first attempt: obligation_id, message_id and request_id.
	f := fixture24(t)
	f.c.Socket = "/tmp/host.sock"
	f.c.Settings = &delivery.TaskSettings{}
	o, staged := f.staged(t)
	check("obligation_id", o.ID, wantObligation)
	id, _ := staged["messageId"].(string)
	check("message_id", id, wantMessage)
	result, err := f.c.Attempt(f.ctx, id, &sendHost{status: "idle"}, 1_700_000_000)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := result["requestId"].(string)
	check("request_id", request, wantRequest)

	// A fault notice's packet: its envelope's messageId.
	packet, err := composeNoticeForTest(nsNotice(), nsLive(), nsAt, "evidence line")
	if err != nil {
		t.Fatal(err)
	}
	notice, _ := packet["envelope"].(map[string]any)["messageId"].(string)
	check("notice message id", notice, wantNotice)

	// A direct message, and the reception check's own derivation of the staged report's id (the same
	// four fields, so the same id).
	check("direct message id", DirectMessageID(DirectMessage{Kind: KindFlowMilestoneReport, Relationship: "project:PRJ-1", Subject: "milestone-1"}), wantDirect)
	upward, err := reception.MessageID("parent_to_supervisor", "rel-1", "completion", "event-1")
	if err != nil {
		t.Fatal(err)
	}
	check("reception message id", upward, wantReception)
}
