package argparse

import (
	"strings"
	"testing"
)

// CRW-1378: decision-answer's --authority help says who answers a user-grade or delegated-management-grade decision as the command
// decides it (CRW-1371): the owner of the management seat the record's authority ref names, else the store seat's task.
func TestDecisionAnswerAuthorityHelpNamesTheManagementSeat(t *testing.T) {
	var help string
	for _, a := range Specs["decision-answer"].Actions {
		for _, flag := range a.Flags {
			if flag == "--authority" {
				help = a.Help
			}
		}
	}
	if !strings.Contains(help, "management seat") || !strings.Contains(help, "store-scope supervisor binding") || !strings.Contains(help, "with no such ref") {
		t.Fatalf("the --authority help is %q", help)
	}
}
