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
