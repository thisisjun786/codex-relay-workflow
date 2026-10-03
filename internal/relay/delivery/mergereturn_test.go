package delivery

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
)

// CRW-408 criterion c4: a return request reaches the holder through the real sender, waking
// its idle thread, and the notice reads as current exactly while the turn occupies the target.

func returnNotice(t *testing.T) (*fixture, *mergeturn.Service, string, string) {
	t.Helper()
	f, m, turn, _ := promotedGrant(t)
	answer, err := m.RequestReturn(f.ctx, turn, "01rival-task", "my candidate is ready and waiting")
	mustDo(t, err)
	queued, _ := answer["returnNotice"].(map[string]any)
	event, _ := queued["eventId"].(string)
	if event == "" || queued["state"] != "queued" {
		t.Fatal(answer["returnNotice"])
	}
	return f, m, turn, event
}

func Test408_c4_real_sender_wakes_the_idle_holder_with_the_return_request(t *testing.T) {
	t.Parallel()
	f, _, turn, event := returnNotice(t)
	before := len(f.host.threads[parent].turns)
	sent, err := f.delivery.Attempt(f.ctx, event, f.host, nil, "")
	mustDo(t, err)
	if pyjson.Text(sent.Get("deliveryState")) != Dispatched || len(f.host.threads[parent].turns) != before+1 || len(f.host.sends) != 1 || f.host.sends[0].thread != parent {
		t.Fatal(sent, f.host.sends, f.row(event))
	}
	text := f.host.threads[parent].items[len(f.host.threads[parent].items)-1][1]
	for _, fragment := range []string{"merge turn return requested", "requestedBy: 01rival-task", "my candidate is ready and waiting", "turnId: " + turn, "merge-turn-progress --turn " + turn, "merge-turn-release --turn " + turn, "holding limit"} {
		if !strings.Contains(text, fragment) {
			t.Fatal(fragment, text)
		}
	}
	if strings.Contains(text, "merge turn granted") || strings.Contains(text, "ack-proof") {
		t.Fatal(text)
	}
	state, err := f.delivery.SnapshotItem(f.ctx, event)
	mustDo(t, err)
	if pyjson.Text(state.Get("phase")) != "return_request_delivered" || pyjson.Text(state.Get("reported")) != "dispatched_return_request" {
		t.Fatal("a delivered return request waits for no grant acknowledgement:", state)
	}
	again, err := f.delivery.Attempt(f.ctx, event, f.host, nil, "")
	mustDo(t, err)
	if again != nil || len(f.host.sends) != 1 {
		t.Fatal("a delivered notice is not sent twice", again, f.host.sends)
	}
}

func Test408_c4_acknowledging_the_grant_does_not_suppress_the_return_request(t *testing.T) {
	t.Parallel()
	f, m, turn, event := returnNotice(t)
	record, err := m.Turn(f.ctx, turn)
	mustDo(t, err)
	grant := record["grant"].(map[string]any)["grantId"].(string)
	_, err = m.Acknowledge(f.ctx, turn, parent, grant, "read the grant")
	mustDo(t, err)
	sent, err := f.delivery.Attempt(f.ctx, event, f.host, nil, "")
	mustDo(t, err)
	if pyjson.Text(sent.Get("deliveryState")) != Dispatched || len(f.host.sends) != 1 {
		t.Fatal(sent, f.host.sends)
	}
}

func Test408_c4_a_return_request_for_a_closed_turn_is_suppressed_before_send(t *testing.T) {
	t.Parallel()
	f, m, turn, event := returnNotice(t)
	_, err := m.Release(f.ctx, turn, parent, "returned", "giving it back", "")
	mustDo(t, err)
	answer, err := f.delivery.Attempt(f.ctx, event, f.host, nil, "")
	mustDo(t, err)
	if pyjson.Text(answer.Get("sendAttempted")) != "no" || pyjson.Text(answer.Get("supersededReason")) != mergeturn.Closed || len(f.host.sends) != 0 {
		t.Fatal(answer, f.host.sends)
	}
}
