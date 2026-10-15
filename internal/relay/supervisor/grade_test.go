package supervisor

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// One message has one grade at every step: the staging derives it from what the message is, and
// every later step from the stored row. These tie rowGrade to the three staging derivations through
// the rows each staging writes (obligation_kind and the packet kind), so they cannot drift.

// TestRowGradeAgreesWithEveryDirectKind: a direct row (obligation_kind the kind, kind the envelope's)
// has the kind's grade, and only a decision the user owes is GradeUser.
func TestRowGradeAgreesWithEveryDirectKind(t *testing.T) {
	user := map[string]bool{KindJunDecisionRequest: true, KindFlowJunDecisionRequest: true}
	for _, name := range DirectKinds() {
		k := directKinds[name]
		kind := "notification"
		if k.decision {
			kind = "decision"
		}
		row := store.SupervisorMessagesRow{ObligationKind: name, Kind: kind}
		if got, want := rowGrade(row), directGrade(k); got != want {
			t.Errorf("%s: rowGrade %v, directGrade %v", name, got, want)
		}
		if got := directGrade(k) == GradeUser; got != user[name] {
			t.Errorf("%s: user grade %v, want %v", name, got, user[name])
		}
	}
}

// TestRowGradeAgreesWithTheEventPath: an event row carries obligation_kind the obligation's kind and
// kind packetKind of it; decision_request is the user's and nothing else is.
func TestRowGradeAgreesWithTheEventPath(t *testing.T) {
	for _, kind := range []string{"completion", "blocked", "decision_request", "unreported"} {
		o := Obligation{Kind: kind}
		row := store.SupervisorMessagesRow{ObligationKind: kind, Kind: packetKind(kind)}
		if got, want := rowGrade(row), obligationGrade(o); got != want {
			t.Errorf("%s: rowGrade %v, obligationGrade %v", kind, got, want)
		}
		if got := obligationGrade(o) == GradeUser; got != (kind == "decision_request") {
			t.Errorf("%s: user grade %v", kind, got)
		}
	}
}

// TestRowGradeAgreesWithFaultNotices: a notice row carries obligation_kind fault_notification and the
// kind composeNotice gives it; a decision notice is the user's.
func TestRowGradeAgreesWithFaultNotices(t *testing.T) {
	for _, kind := range []string{"blocking", "decision", "resolved"} {
		notice := map[string]any{"kind": kind}
		packetKind := "notification"
		if kind == "decision" {
			packetKind = "decision"
		}
		row := store.SupervisorMessagesRow{ObligationKind: store.SupervisorNoticeObligationKind, Kind: packetKind}
		if got, want := rowGrade(row), noticeGrade(notice); got != want {
			t.Errorf("%s: rowGrade %v, noticeGrade %v", kind, got, want)
		}
		if got := noticeGrade(notice) == GradeUser; got != (kind == "decision") {
			t.Errorf("%s: user grade %v", kind, got)
		}
	}
}

// TestUserGradeResolvesThroughTheLaidDownSeatAndNotificationDoesNot: the resolve itself, at both
// grades, through the relationship and 'project:KEY', over a store seat laid down for a management
// seat; a live seat answers both.
func TestUserGradeResolvesThroughTheLaidDownSeatAndNotificationDoesNot(t *testing.T) {
	live := newStoreRecipientWorld(t)
	live.seat("bnd-store", storeRecipientSeatTask)
	successor := newStoreRecipientWorld(t)
	successor.seat("bnd-store", storeRecipientSeatTask)
	successor.takeOverStoreSeat("bnd-store", storeRecipientSeatTask)
	for _, relationship := range []string{storeRecipientRelationship, "project:" + storeRecipientProject} {
		for _, grade := range []Grade{GradeNotification, GradeUser} {
			got, err := live.channel().ResolveFor(live.ctx, relationship, grade)
			if err != nil || got.Recipient != storeRecipientSeatTask || got.RecipientScopeKind != "store" {
				t.Fatalf("live seat, %s grade %v: %+v (%v)", relationship, grade, got, err)
			}
		}
		got, err := successor.channel().ResolveFor(successor.ctx, relationship, GradeUser)
		if err != nil || got.Recipient != storeRecipientSeatTask || got.RecipientScopeKind != "store" {
			t.Fatalf("laid-down seat, %s user grade: %+v (%v), want the manager", relationship, got, err)
		}
		got, err = successor.channel().ResolveFor(successor.ctx, relationship, GradeNotification)
		if reason, _ := successor.refusal(err); reason != "unregistered_scope" {
			t.Fatalf("laid-down seat, %s notification: %+v refused %q, want unregistered_scope", relationship, got, reason)
		}
	}
}
