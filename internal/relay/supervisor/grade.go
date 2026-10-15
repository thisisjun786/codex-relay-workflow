package supervisor

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Grade is what a message is for the level that hears it (CRW-1486/S4): a decision the user owes
// (GradeUser), or anything else (GradeNotification: a completion, a block, a report, an escalation,
// and a decision the recipient itself owes). The grade decides one thing only: whether a store seat
// read through the management seat that took it over (CRW-1333) answers for a project with no
// management edge and no live store seat above it. A user-grade message is carried there, because
// the manager is where Jun's decisions go; every other message is not, so a parent that took the
// store seat over for its own project does not hear every other project's notifications. A live
// store seat answers for every grade.
//
// One message has one grade at every step of its life: it is derived from what the message is
// (obligationGrade at the event path's staging, directGrade at a direct staging, noticeGrade at a
// fault notice's staging), and from the stored row at every later step (rowGrade: claim, transport
// fence, reopen). grade_test.go ties rowGrade to the other three so they cannot drift.
type Grade int

const (
	// GradeNotification is every message that is not a decision the user owes.
	GradeNotification Grade = iota
	// GradeUser is a decision the user owes: answerOwedBy "user" in the envelope.
	GradeUser
)

// gradeOf is GradeUser when user is true.
func gradeOf(user bool) Grade {
	if user {
		return GradeUser
	}
	return GradeNotification
}

// obligationGrade is the grade of an event-path obligation: a decision_request is a decision only
// the user can make (the envelope's answerOwedBy "user"); every other kind owes no answer.
func obligationGrade(o Obligation) Grade { return gradeOf(o.Kind == "decision_request") }

// directGrade is the grade of a direct kind: a decision whose answer the user owes. A decision the
// recipient owes (lead_decision_request, flow_project_decision_request) is the recipient's own and
// is a notification here.
func directGrade(k directKind) Grade { return gradeOf(k.decision && k.owedBy == "user") }

// noticeGrade is the grade of a fault notice: a notice of kind decision asks the user for one.
func noticeGrade(notice map[string]any) Grade { return gradeOf(notice["kind"] == "decision") }

// userGradeObligationKinds are the obligation kinds whose decision-kind rows the user answers: the
// event path's decision_request, the two Jun decision direct kinds, and the fault notice.
var userGradeObligationKinds = map[string]bool{
	"decision_request":                   true,
	KindJunDecisionRequest:               true,
	KindFlowJunDecisionRequest:           true,
	store.SupervisorNoticeObligationKind: true,
}

// rowGrade is the grade of a stored message: a decision-kind row whose obligation the user answers.
func rowGrade(row store.SupervisorMessagesRow) Grade {
	return gradeOf(row.Kind == "decision" && userGradeObligationKinds[row.ObligationKind])
}

// heldThroughManagementSeat reports whether the store seat answers only through the management seat
// that took it over: registry.StoreScopeSupervisor names the management binding (scope kind
// management) when no live store-scope binding stands. A live store seat names itself.
func heldThroughManagementSeat(held []contract.OrderedObject) bool {
	if len(held) == 0 {
		return false
	}
	for _, one := range held {
		if one.Get("scopeKind") != scopeKindManagement {
			return false
		}
	}
	return true
}

// successorRefusal is the unregistered_scope a message other than a user decision reads when the only
// level above what (a project or an initiative) is a store seat laid down for a management seat.
func successorRefusal(held []contract.OrderedObject, what string) Refusal {
	holders := make([]any, len(held))
	for i, one := range held {
		holders[i] = one.Get("taskId")
	}
	return Refusal{"unregistered_scope", "no management seat stands above " + what + " and the store has no live seat: the store seat was laid down for the management seat of " +
		pythonRepr(holders) + ", which hears through it only a decision the user owes, so there is nobody to tell about anything else. The obligation stays standing until a management edge above it or a live store seat names someone"}
}
