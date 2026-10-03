package faults

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// parkRecorder is the supervisor channel as the deliverer meets it when a notice's message is staged and
// the send does not happen: the message stays queued, and the deliverer must park it through the channel.
type parkRecorder struct {
	t       *testing.T
	l       *Ledger
	parked  []string
	reasons []string
}

func (c *parkRecorder) Resolve(context.Context, string) (map[string]any, error) {
	return map[string]any{"sender": "parent", "recipient": "supervisor", "projectKey": "P"}, nil
}

func (c *parkRecorder) StageNotice(ctx context.Context, _ map[string]any) (map[string]any, error) {
	seedNoticeMessage(c.t, ctx, c.l.Store, "m1", "supervisor", "queued", "2023-11-14T22:13:20.000000+00:00", "obligation_kind='fault_notification'", "obligation_id='n1'")
	return map[string]any{"messageId": "m1"}, nil
}

func (c *parkRecorder) Attempt(context.Context, string, float64, string) error {
	return errors.New("the recipient is busy")
}
func (c *parkRecorder) Recover(context.Context, string, float64) error { return nil }
func (c *parkRecorder) Measure(context.Context, string) error          { return nil }
func (c *parkRecorder) Park(_ context.Context, id, reason string) error {
	c.parked, c.reasons = append(c.parked, id), append(c.reasons, reason)
	return nil
}

func noticeToParkWorld(t *testing.T, state, owner string) (*Ledger, context.Context, *parkRecorder) {
	t.Helper()
	l, ctx := testLedger(t)
	for _, statement := range []string{
		"INSERT INTO fault_ledger (fault_id, product, fault_class, component, severity, signature, scope, scope_key, state, first_seen_at, last_seen_at, updated_at) VALUES ('abc123','crw','report_omitted','c','broken','{}','{\"projectKey\": \"P\"}','P','open','t','t','t')",
		"INSERT INTO fault_notifications (notification_id, fault_id, product, kind, cycle, state, owner, created_at, updated_at) VALUES ('n1','abc123','crw','blocking',1,'" + state + "'," + owner + ",'t','t')",
	} {
		if _, err := l.Store.Q(ctx).ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return l, ctx, &parkRecorder{t: t, l: l}
}

func TestDeliverer_parks_through_the_channel_a_message_it_could_not_send(t *testing.T) {
	// Given: a pending notification about a fault no relationship anchors (so no contact reading is asked), whose
	// message the channel stages and then cannot send.
	l, ctx, channel := noticeToParkWorld(t, "pending", "NULL")
	d := &NoticeDeliverer{Ledger: l, Channel: channel, Owner: "relay-daemon"}
	// When: the deliverer takes one pass.
	answer, err := d.Tick(ctx, 100000, 1)
	// Then: it settled the reservation as returned and asked the channel to park the staged message, naming why.
	if err != nil {
		t.Fatal(err)
	}
	if answer.Returned != 1 || answer.Delivered != 0 {
		t.Fatalf("answer %+v", answer)
	}
	if len(channel.parked) != 1 || channel.parked[0] != "m1" || !strings.HasPrefix(channel.reasons[0], "nothing was sent: its message m1 is queued") {
		t.Fatalf("parked %v for %q", channel.parked, channel.reasons)
	}
}

func TestDeliverer_parks_through_the_channel_a_message_an_uncertain_notice_never_sent(t *testing.T) {
	// Given: an uncertain notification of the daemon's whose message is still queued with no attempt.
	l, ctx, channel := noticeToParkWorld(t, "uncertain", "'relay-daemon'")
	seedNoticeMessage(t, ctx, l.Store, "m1", "supervisor", "queued", "2023-11-14T22:13:20.000000+00:00", "obligation_kind='fault_notification'", "obligation_id='n1'")
	d := &NoticeDeliverer{Ledger: l, Channel: channel, Owner: "relay-daemon"}
	// When: the deliverer reconciles it.
	answer, err := d.Tick(ctx, 100000, 0)
	// Then: the notification is settled as returned and the message is parked through the channel.
	if err != nil {
		t.Fatal(err)
	}
	if answer.Returned != 1 || len(channel.parked) != 1 || channel.parked[0] != "m1" {
		t.Fatalf("answer %+v, parked %v", answer, channel.parked)
	}
}
