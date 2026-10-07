package supervisor

import (
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// CRW-943. CRW-905 made a fault notice unclaimable while a delivery to its recipient waits out a
// busy backoff, but left the notice's queued row counted as "ahead": oldestAhead (send.go) and the
// head selection the daemon pass runs (autoHeads, autosend.go) both still saw it as a row that goes
// first, so an ordinary supervisor message staged behind it waited too and the daemon picked the
// notice. The daemon also turned the new refusal into an independent 60-second backoff, so releasing
// the head did not restore the notice's claimability.
//
// These tests pin the decided behaviour on temporary stores: the order judgement asks the same
// predicate the claim asks, so a notice the line holds is not an older row that goes first (I-216
// unchanged for every other message), and that one refusal leaves the notice's eligibility exactly
// as it is while every other refusal keeps its backoff.
//
// The line is a real deliveries row - the one busyHeadSQL names - so these tests ask the shared
// predicate rather than a second reading of it.

// orderNow is the instant every case below judges at: the notice fixture's own clock.
const orderNow = nsNow

// orderLater stages a message after the notice fixture stages its own, so the notice is the older
// of the two.
const orderLater = "2023-11-14T22:13:30.000000+00:00"

// orderMessage stages the fixture's own report obligation - an ordinary supervisor message to the
// same recipient the notice goes to - and moves it to stagedAt, which is how these cases fix the
// order between the two facts. It is staged through the channel rather than seeded, so it carries
// the packet an attempt re-derives and is judged on the order rather than on a malformed proposal.
// It returns the row's id and that packet.
//
// The fixture's obligation stages once, so a case that needs a second ordinary message calls
// orderMessageAgain with the packet this one returned.
func orderMessage(t *testing.T, w *noticeYieldWorld, stagedAt string) (id, packet string) {
	t.Helper()
	id = stagedID(t, w.stageFixture)
	w.exec(t, "UPDATE supervisor_messages SET staged_at=?,updated_at=? WHERE message_id=?", stagedAt, stagedAt, id)
	return id, w.message(t, id).Packet
}

// orderMessageAgain seeds another ordinary supervisor message, under an id of its own and staged at
// stagedAt, carrying the packet of the one the fixture's obligation stages. One obligation is one
// message, so the second ordinary message a case needs is seeded rather than staged a second time.
func orderMessageAgain(t *testing.T, w *noticeYieldWorld, id, packet, stagedAt string) string {
	t.Helper()
	w.exec(t, "INSERT INTO supervisor_messages(message_id,obligation_id,obligation_kind,relationship_id,project_key,purpose,kind,sender_task_id,recipient_task_id,subject,packet,state,staged_at,updated_at,event_id,submission_no) VALUES (?,'obl-"+id+"','report','rel-1','PRJ-1','completion','notification','parent','supervisor',?,?,'queued',?,?,?,1)",
		id, "s-"+id, packet, stagedAt, stagedAt, w.event)
	return id
}

// orderHeads is the head selection the daemon pass runs, by message id.
func orderHeads(t *testing.T, w *noticeYieldWorld, now float64) []string {
	t.Helper()
	rows, err := w.c.autoHeads(w.ctx, now, 100, "", "")
	if err != nil {
		t.Fatalf("autoHeads: %v", err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.MessageID)
	}
	return ids
}

func orderHas(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// orderRefused reports whether err is the not_claimable answer the order and yield refusals carry.
func orderRefused(err error) bool {
	var refusal Refusal
	return errors.As(err, &refusal) && refusal.Reason == "not_claimable"
}

// orderNoticeThenMessage stages a fault notice and, after it, an ordinary supervisor message to the
// same recipient: the two facts evaluation case 1 is about. The notice must be the older one, which
// the fixture's own staging instant and orderLater guarantee; the check below says so rather than
// letting a changed fixture turn the case into something else.
func orderNoticeThenMessage(t *testing.T, w *noticeYieldWorld) (notice, message string) {
	t.Helper()
	notice = w.stagedNotice(t)
	message, _ = orderMessage(t, w, orderLater)
	if w.message(t, notice).StagedAt >= w.message(t, message).StagedAt {
		t.Fatalf("the notice is not the older message: %s vs %s", w.message(t, notice).StagedAt, w.message(t, message).StagedAt)
	}
	return notice, message
}

// TestNoticeYieldOrder_a_supervisor_message_behind_a_yielding_notice_is_claimed is evaluation case
// 1 asked of the claim: notice N staged first, ordinary message S staged after it, both to one
// recipient whose line a busy-backoff delivery holds until now+120. N cannot be claimed, so it is
// not an older row that goes first, and S goes.
func TestNoticeYieldOrder_a_supervisor_message_behind_a_yielding_notice_is_claimed(t *testing.T) {
	t.Parallel()
	w := newNoticeYieldWorld(t)
	notice, message := orderNoticeThenMessage(t, w)
	w.line(t, "supervisor", orderNow+120)
	// When: the ordinary message is attempted.
	record, err := w.c.Attempt(w.ctx, message, w.host, orderNow)
	// Then: it is claimed and sent.
	if err != nil || record == nil || record["deliveryState"] != "dispatched" {
		t.Fatalf("record %v err %v", record, err)
	}
	// And the notice is left exactly where it was: the yield still refuses it.
	if row := w.message(t, notice); row.State != "queued" || row.AttemptCount != 0 || row.NextEligibleAt.Valid {
		t.Fatalf("notice %+v", row)
	}
	if _, err := w.c.Attempt(w.ctx, notice, w.host, orderNow); !orderRefused(err) {
		t.Fatalf("notice attempt %v", err)
	}
}

// TestNoticeYieldOrder_the_daemon_sends_the_message_behind_a_yielding_notice is the same case asked
// of the daemon pass: with no project opened the pass only picks its heads and sends, so what it
// sends is the head selection's answer.
func TestNoticeYieldOrder_the_daemon_sends_the_message_behind_a_yielding_notice(t *testing.T) {
	t.Parallel()
	w := newNoticeYieldWorld(t)
	notice, message := orderNoticeThenMessage(t, w)
	w.line(t, "supervisor", orderNow+120)
	// Given: the head selection, and the pass over it.
	if heads := orderHeads(t, w, orderNow); !orderHas(heads, message) || orderHas(heads, notice) {
		t.Fatalf("heads %v, want %s and not %s", heads, message, notice)
	}
	answer, err := w.c.AutoSend(w.ctx, w.host, orderNow, 0, 1, "", "", "")
	// Then: the ordinary message went out and the notice is untouched.
	if err != nil {
		t.Fatal(err)
	}
	if row := w.message(t, message); row.State != "dispatched" {
		t.Fatalf("ordinary message %+v, pass %+v", row, answer)
	}
	if row := w.message(t, notice); row.State != "queued" || row.AttemptCount != 0 || row.NextEligibleAt.Valid {
		t.Fatalf("notice %+v, pass %+v", row, answer)
	}
}

// TestNoticeYieldOrder_a_yielding_notice_is_claimable_when_its_head_ends is evaluation case 2: the
// holding delivery's backoff ends at now+10, so at now the line is still held. The pass must not
// push the notice's eligibility out, and at now+20, with the line free, the notice is claimed.
func TestNoticeYieldOrder_a_yielding_notice_is_claimable_when_its_head_ends(t *testing.T) {
	t.Parallel()
	w := newNoticeYieldWorld(t)
	notice := w.stagedNotice(t)
	w.line(t, "supervisor", orderNow+10)
	// When: the daemon pass runs while the line is still held.
	if _, err := w.c.AutoSend(w.ctx, w.host, orderNow, 0, 1, "", "", ""); err != nil {
		t.Fatal(err)
	}
	// Then: the notice was not attempted and its eligibility was not pushed past the head's own end.
	row := w.message(t, notice)
	if row.NextEligibleAt.Valid {
		t.Fatalf("the pass backed the yielding notice off to %v", row.NextEligibleAt.Float64)
	}
	if row.State != "queued" || row.AttemptCount != 0 {
		t.Fatalf("the pass moved the yielding notice: %+v", row)
	}
	// And at now+20, with the head's backoff over, the notice is claimed and sent.
	if _, err := w.c.Attempt(w.ctx, notice, w.host, orderNow+20); err != nil {
		t.Fatalf("notice after the head ended: %v", err)
	}
	if row := w.message(t, notice); row.State != "dispatched" || row.AttemptCount != 1 {
		t.Fatalf("notice %+v", row)
	}
}

// TestNoticeYieldOrder_the_daemon_leaves_the_line_yield_alone pins the daemon's own deferral, which
// the head selection cannot cover on its own: a notice that is refused because its recipient's line
// is held is waiting, not failing, and backing it off would delay it past the head's release, while
// every other refusal keeps the channel's backoff.
func TestNoticeYieldOrder_the_daemon_leaves_the_line_yield_alone(t *testing.T) {
	t.Parallel()
	w := newNoticeYieldWorld(t)
	notice := w.stagedNotice(t)
	w.line(t, "supervisor", orderNow+120)
	// Given: the refusal a yielding notice really gets, taken from an attempt of its own.
	_, yieldErr := w.c.Attempt(w.ctx, notice, w.host, orderNow)
	if !orderRefused(yieldErr) {
		t.Fatalf("notice attempt %v", yieldErr)
	}
	// When: the daemon defers that failure.
	before := journalCount(t, w.stageFixture)
	if err := w.c.deferAutoFault(w.ctx, notice, orderNow, yieldErr); err != nil {
		t.Fatal(err)
	}
	// Then: the notice is left exactly as it was and nothing is journaled.
	row := w.message(t, notice)
	if row.NextEligibleAt.Valid || row.State != "queued" || row.AttemptCount != 0 {
		t.Fatalf("the line yield moved the notice: %+v", row)
	}
	if journalCount(t, w.stageFixture) != before {
		t.Fatalf("the line yield journaled a fault: %d -> %d", before, journalCount(t, w.stageFixture))
	}
	// And: another failure of the same notice still backs it off by the channel's recheck interval.
	if err := w.c.deferAutoFault(w.ctx, notice, orderNow, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	want := orderNow + delivery.DefaultPolicy().LifecycleRecheck
	if row := w.message(t, notice); !row.NextEligibleAt.Valid || row.NextEligibleAt.Float64 != want {
		t.Fatalf("another failure did not back the notice off: %+v, want %v", row, want)
	}
}

// TestNoticeYieldOrder_a_notice_whose_line_is_free_still_goes_first pins how narrow the change is:
// with nothing holding the line, the older notice is still ahead of the younger message and the
// pass still picks the notice (I-216's oldest-of-the-claimable rule).
func TestNoticeYieldOrder_a_notice_whose_line_is_free_still_goes_first(t *testing.T) {
	t.Parallel()
	w := newNoticeYieldWorld(t)
	notice, message := orderNoticeThenMessage(t, w)
	// Given: no delivery holding the line at all.
	if heads := orderHeads(t, w, orderNow); !orderHas(heads, notice) || orderHas(heads, message) {
		t.Fatalf("heads %v, want %s and not %s", heads, notice, message)
	}
	// And the younger message is refused, naming the notice.
	_, err := w.c.Attempt(w.ctx, message, w.host, orderNow)
	if !orderRefused(err) {
		t.Fatalf("ordinary message attempt %v", err)
	}
}

// TestNoticeYieldOrder_a_held_line_does_not_excuse_an_ordinary_message_behind_another pins that the
// exclusion is the notice channel's alone: an ordinary message behind another ordinary message
// waits however the recipient's line stands.
func TestNoticeYieldOrder_a_held_line_does_not_excuse_an_ordinary_message_behind_another(t *testing.T) {
	t.Parallel()
	w := newNoticeYieldWorld(t)
	_, packet := orderMessage(t, w, nsAt)
	younger := orderMessageAgain(t, w, "order-younger", packet, orderLater)
	w.line(t, "supervisor", orderNow+120)
	// When: the younger ordinary message is attempted.
	if _, err := w.c.Attempt(w.ctx, younger, w.host, orderNow); !orderRefused(err) {
		t.Fatalf("younger ordinary message attempt %v", err)
	}
}

// TestNoticeYieldOrder_a_notice_in_flight_still_blocks_the_message_behind_it pins the boundary the
// exclusion must not cross: a notice whose send is already under way goes ahead whatever the line
// does, so nothing behind it takes its place (I-216's in-flight clause).
func TestNoticeYieldOrder_a_notice_in_flight_still_blocks_the_message_behind_it(t *testing.T) {
	t.Parallel()
	w := newNoticeYieldWorld(t)
	notice, message := orderNoticeThenMessage(t, w)
	w.line(t, "supervisor", orderNow+120)
	w.exec(t, "UPDATE supervisor_messages SET state='sending',lease_owner='other',lease_until=? WHERE message_id=?", orderNow+300, notice)
	// When: the younger message is attempted.
	if _, err := w.c.Attempt(w.ctx, message, w.host, orderNow); !orderRefused(err) {
		t.Fatalf("ordinary message attempt %v", err)
	}
	// And the notice in flight is still that recipient's head: it is not claimable, but it is not
	// yielded out of the way either, so the younger message is refused for it rather than sent.
	if row := w.message(t, notice); row.State != "sending" {
		t.Fatalf("the in-flight notice moved: %+v", row)
	}
}
