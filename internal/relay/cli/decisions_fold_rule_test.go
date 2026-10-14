package cli_test

import (
	"strings"
	"testing"
)

// CRW-1390 (Argon V5 P3 of bd2d68b6, decisions.go foldWarning): a raise held under no management seat that does not fold into the open
// record of a management seat says which rule answers it - the store seat's, for a user-grade or delegated-management-grade question
// with no seat above its project, or the raiser's own, for a parent-class question - instead of naming an empty string as a seat.
func TestDecisionFoldWarningOfARaiseWithNoSeatNamesTheRuleThatAnswersIt(t *testing.T) {
	question := "Open the release window for the shared module?"
	for _, c := range []struct {
		name, authority, rule string
	}{
		{"store seat", "user", "store seat"},
		{"parent class", "parent=" + authOwnerC, "raiser"},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := authWorld(t)
			first, _ := authRaise(t, state, "P-A", question, "user")
			second, raised := authRaise(t, state, "P-C", question, c.authority)
			if second == first || raised["merged"] != false {
				t.Fatalf("the raise with no seat folded into seat %s's record: %v", authSeatA, raised)
			}
			warning, _ := raised["foldWarning"].(map[string]any)
			detail, _ := warning["detail"].(string)
			if warning["reason"] != "seat_differs" || !strings.Contains(detail, first) {
				t.Fatalf("the fold warning %v", raised["foldWarning"])
			}
			if strings.Contains(detail, "('')") || strings.Contains(detail, `("")`) {
				t.Fatalf("the fold warning names an empty seat: %q", detail)
			}
			if !strings.Contains(detail, c.rule) || !strings.Contains(detail, authSeatA) {
				t.Fatalf("the fold warning %q names neither the %s rule that answers this raise nor the seat %s that holds the open record", detail, c.rule, authSeatA)
			}
			rows := authJournal(t, state)
			if len(rows) != 1 || !strings.Contains(rows[0], c.rule) {
				t.Fatalf("decision_authority_warning rows %q", rows)
			}
		})
	}
}

// CRW-1406 (Argon V5 P3 of 2741763e, decisions.go decisionSeatsText): the other way round - the open record is held under no management
// seat (raised from P-C, which no seat stands above) and the same question is raised from P-A, under management seat mgmt-a. The raise
// does not fold; the warning names the open record's seat by the rule that answers it, not as an empty seat, and this raise's seat.
func TestDecisionFoldWarningOfARaiseUnderASeatNamesTheOpenRecordWithNoSeatByItsRule(t *testing.T) {
	question := "Open the release window for the shared module?"
	state := authWorld(t)
	first, _ := authRaise(t, state, "P-C", question, "user")
	if got := authRef(t, state, first); got != nil {
		t.Fatalf("the record raised from P-C holds the ref %v, want none", got)
	}
	second, raised := authRaise(t, state, "P-A", question, "user")
	if second == first || raised["merged"] != false {
		t.Fatalf("the raise under seat %s folded into the record with no seat: %v", authSeatA, raised)
	}
	if got := authRef(t, state, second); got != authSeatA {
		t.Fatalf("the second record's ref is %v, want %s", got, authSeatA)
	}
	warning, _ := raised["foldWarning"].(map[string]any)
	detail, _ := warning["detail"].(string)
	if warning["reason"] != "seat_differs" || warning["seat"] != authSeatA || !strings.Contains(detail, first) {
		t.Fatalf("the fold warning %v", raised["foldWarning"])
	}
	if strings.Contains(detail, "('')") || strings.Contains(detail, `("")`) || strings.Contains(detail, "management seat ''") {
		t.Fatalf("the fold warning names an empty seat: %q", detail)
	}
	held := "under no management seat (the store seat's rule, or the raiser's own for a parent-class question)"
	this := "this raise is held under management seat '" + authSeatA + "', whose owner answers it"
	if !strings.Contains(detail, held) || !strings.Contains(detail, this) {
		t.Fatalf("the fold warning %q names neither the rule that answers the open record (%q) nor this raise's seat (%q)", detail, held, this)
	}
	rows := authJournal(t, state)
	if len(rows) != 1 || !strings.Contains(rows[0], second) || !strings.Contains(rows[0], held) {
		t.Fatalf("decision_authority_warning rows %q", rows)
	}
}
