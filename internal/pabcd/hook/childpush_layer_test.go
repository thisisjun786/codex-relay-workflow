package hook

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
)

// TestChildPushReasonNamesTheChild (CRW-1478): the child push guard names the 자식 (the relay parent) as the one that
// integrates, and carries no bare role word; the guard code is unchanged.
func TestChildPushReasonNamesTheChild(t *testing.T) {
	unread := gateDeny(t, ChildPushInputUnreadAnswer())
	judged := childPushJudge(childPush{}, "")
	for what, text := range map[string]string{"the unread-input deny": unread, "the no-own-branch reason": judged} {
		if hits := layername.Find(text); len(hits) != 0 {
			t.Errorf("%s: bare role words %v in %q", what, hits, text)
		}
	}
	for _, want := range []string{"[crw CHILD-PUSH-GUARD] ", "통합은 자식·통합기 몫 (integration is the 자식's and the integrator's)"} {
		if !strings.Contains(unread, want) {
			t.Errorf("the unread-input deny lacks %q: %q", want, unread)
		}
	}
	if !strings.Contains(judged, "the 자식's checkout") {
		t.Errorf("the no-own-branch reason does not name the 자식's checkout: %q", judged)
	}
}
