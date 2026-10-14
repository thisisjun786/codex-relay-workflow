package cli_test

import (
	"strings"
	"testing"
)

// CRW-1378 (CRW-1371 follow-ups): the fingerprint of a question does not carry its authority, so the same question raised from a
// project under another management seat would fold into the open record the first seat holds, and the second seat's owner could not
// answer it. A raise folds only into a record held under the same seat; under another it is a new record, warned and journaled
// (decision_authority_warning, seat_differs). A record without a seat (the store seat's) folds as it always did. And with no management
// edge above the project, a raiser's ref that names a known management seat is kept, warned and journaled (ref_without_edge).

func TestDecisionRaiseUnderAnotherSeatIsANewRecord(t *testing.T) {
	state := authWorld(t)
	question := "Open the release window for the shared module?"
	first, _ := authRaise(t, state, "P-A", question, "user")
	second, raised := authRaise(t, state, "P-B", question, "user")
	if second == first || raised["merged"] != false {
		t.Fatalf("the raise under seat %s folded into %s's record: %v", authSeatB, authSeatA, raised)
	}
	if got := authRef(t, state, second); got != authSeatB {
		t.Fatalf("the second record's ref is %v, want %s", got, authSeatB)
	}
	warning, _ := raised["foldWarning"].(map[string]any)
	if warning["reason"] != "seat_differs" || !strings.Contains(warning["detail"].(string), first) {
		t.Fatalf("the fold warning %v", raised["foldWarning"])
	}
	rows := authJournal(t, state)
	if len(rows) != 1 || !strings.Contains(rows[0], second) || !strings.Contains(rows[0], "seat_differs") {
		t.Fatalf("decision_authority_warning rows %q", rows)
	}
	// each seat's owner answers its own record
	authAccepted(t, state, second, authSeatB, authAnswer(t, state, second, authSeatB, "user"))
	authAccepted(t, state, first, authSeatA, authAnswer(t, state, first, authSeatA, "user"))

	// under the same seat, and with no seat at all, a repeated question folds as before
	again := authWorld(t)
	a1, _ := authRaise(t, again, "P-A", question, "user")
	a2, raised := authRaise(t, again, "P-A", question, "user")
	if a2 != a1 || raised["merged"] != true || raised["foldWarning"] != nil {
		t.Fatalf("the same seat's question did not fold: %v", raised)
	}
	c1, _ := authRaise(t, again, "P-C", "Which window does P-C take?", "user")
	c2, raised := authRaise(t, again, "P-C", "Which window does P-C take?", "user")
	if c2 != c1 || raised["merged"] != true {
		t.Fatalf("the store seat's question did not fold: %v", raised)
	}
	authAccepted(t, again, c1, "task-sup", authAnswer(t, again, c1, "task-sup", "user"))
}

func TestDecisionRaiseRefWithoutAnEdgeWarns(t *testing.T) {
	state := authWorld(t)
	decision, raised := authRaise(t, state, "P-C", "Which window does P-C take?", "user="+authSeatA)
	if got := authRef(t, state, decision); got != authSeatA {
		t.Fatalf("the ref is %v, want the raiser's %s kept", got, authSeatA)
	}
	warning, _ := raised["authorityWarning"].(map[string]any)
	if warning["reason"] != "ref_without_edge" || warning["given"] != authSeatA {
		t.Fatalf("the warning %v", raised["authorityWarning"])
	}
	if rows := authJournal(t, state); len(rows) != 1 || !strings.Contains(rows[0], decision) || !strings.Contains(rows[0], "ref_without_edge") {
		t.Fatalf("decision_authority_warning rows %q", rows)
	}
	// the seat it names answers it, as the ref says
	authAccepted(t, state, decision, authSeatA, authAnswer(t, state, decision, authSeatA, "user"))
}

// CRW-1378 d3 (a characterization of the recovery docs/relay/linkage.md describes, not a behaviour change): a record whose management
// seat has no live owner is answered again once a new thread binds the seat, and a question withdrawn and raised again names the seat
// above the project as it stands then.
func TestDecisionOfAnOwnerlessSeatIsRecoveredAsLinkageSays(t *testing.T) {
	state := authWorld(t)
	decision, _ := authRaise(t, state, "P-A", "Ship P-A before the review?", "delegated-management")
	other, _ := authRaise(t, state, "P-A", "Which window does P-A take?", "delegated-management")
	review818Exec(t, state, "UPDATE scope_bindings SET status = 'archived' WHERE scope_kind = 'management' AND scope_key = ?", authSeatA)
	review818RefusedFor(t, authAnswer(t, state, decision, authSeatA, "delegated-management"), "authority_unverified", authSeatA)
	authRelay(t, state, "linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", authSeatA, "--task", authSeatNew, "--host", "host")
	authAccepted(t, state, decision, authSeatNew, authAnswer(t, state, decision, authSeatNew, "delegated-management"))
	authRelay(t, state, "decision-withdraw", "--decision", other, "--reason", "raised again under the seat that stands now")
	again, _ := authRaise(t, state, "P-A", "Which window does P-A take?", "delegated-management")
	if again == other || authRef(t, state, again) != authSeatA {
		t.Fatalf("the question raised again is %s with ref %v", again, authRef(t, state, again))
	}
	authAccepted(t, state, again, authSeatNew, authAnswer(t, state, again, authSeatNew, "delegated-management"))
}
