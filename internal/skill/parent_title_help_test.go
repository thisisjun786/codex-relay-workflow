package skill

import (
	"strings"
	"testing"
)

// CRW-1344: the work is tracked in a GitHub repository, so crw skill parent-title says in its help where the product family
// comes from: the family:NAME label of the milestone's issues in the tracking repository
// (crw-plan/references/integrations.md#set-the-app-presentation-and-record). It names no other tracker and no label group.
func TestParentTitleHelpNamesTheFamilyLabelOfTheTrackingRepository(t *testing.T) {
	for _, args := range [][]string{{"parent-title", "--help"}, {"parent-title", "-h"}} {
		code, help, errOut := call(args, "")
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, errOut)
		}
		for _, want := range []string{"family:NAME", "tracking repository", "milestone"} {
			if !strings.Contains(help, want) {
				t.Errorf("%v: the help does not say %q:\n%s", args, want, help)
			}
		}
		for _, banned := range []string{"linear", "label group"} {
			if strings.Contains(strings.ToLower(help), banned) {
				t.Errorf("%v: the help still says %q:\n%s", args, banned, help)
			}
		}
	}
}
