//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The delivery authority (CRW-1102): crw-dev holds the one definition of when a push is allowed (an applicable user grant, the
// repository's standing grant, or a scope packet carrying one), and crw-pabcd, crw-run and crw-plan point at it. The delivery
// path of the repository is chosen before a PR stack is. These checks pin the wording the definition rests on; the behaviour
// itself is read from the scenario review recorded in the issue handoff, because a text test shows only that the text is there.

func deliveryAuthorityLine(t *testing.T, rel, marker string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, marker) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: %d lines carry %q, want exactly one", rel, len(found), marker)
	}
	return found[0]
}

func TestDeliveryAuthority_CrwDevHoldsTheDefinition(t *testing.T) {
	line := deliveryAuthorityLine(t, "plugins/crw/skills/crw-dev/SKILL.md", "(DEV-GIT-PUSH-01, ESCALATE)")
	for _, want := range []string{
		"the one definition of when a push is allowed",
		"explicit approval for it in the current session",
		"a standing grant of the repository",
		"a scope packet that carries one of the two",
		"evidence that a grant applies, never a new consent",
		"outranks every grant",
		"force-push, a tag, a push to an integration branch",
		"a release and a deployment keep their own authorization",
		"no grant for another",
		"Where no grant applies, do not push",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("DEV-GIT-PUSH-01 lacks %q", want)
		}
	}
	for _, old := range []string{"never `git push` without the user's explicit approval", "Push requires explicit user approval"} {
		if strings.Contains(line, old) {
			t.Errorf("DEV-GIT-PUSH-01 keeps the session-approval-only wording %q", old)
		}
	}
}

func TestDeliveryAuthority_StackFollowsTheDeliveryPath(t *testing.T) {
	line := deliveryAuthorityLine(t, "plugins/crw/skills/crw-dev/SKILL.md", "(DEV-STACK-01, DEFAULT)")
	for _, want := range []string{
		"Choose the repository's delivery path first",
		"verified task branch that the integrator fast-forwards onto `dev`",
		"opens no pull request, so it is never stacked",
		"An external contribution or an in-flight pull request keeps the pull-request path",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("DEV-STACK-01 lacks %q", want)
		}
	}
}

func TestDeliveryAuthority_OtherSkillsPointNotRestate(t *testing.T) {
	line := deliveryAuthorityLine(t, "plugins/crw/skills/crw-pabcd/SKILL.md", "3. **B — Build**")
	if !strings.Contains(line, "Push only as `crw-dev` DEV-GIT-PUSH-01 defines") {
		t.Errorf("PABCD B does not point at the crw-dev definition: %s", line)
	}
	for _, rel := range []string{
		"plugins/crw/skills/crw-pabcd/SKILL.md",
		"plugins/crw/skills/crw-pabcd/references/phase-plan.md",
		"plugins/crw/skills/crw-plan/references/integrations.md",
		"plugins/crw/skills/crw-run/references/task-packet.md",
	} {
		data, err := os.ReadFile(filepath.Join(repoRoot(), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		for i, l := range strings.Split(string(data), "\n") {
			for _, old := range []string{"without explicit user approval", "without the user's explicit approval in the current session", "say never to push without approval", "pushing remains gated by DEV-GIT-PUSH-01"} {
				if strings.Contains(l, old) {
					t.Errorf("%s:%d restates the session-approval rule (%q)", rel, i+1, old)
				}
			}
		}
	}
}

// The packet surfaces (launch packet, required fields, restoration block, S25) carry the grant and point at crw-dev; none of them
// names a publication scope as the approval DEV-GIT-PUSH-01 requires, or claims to own the rule (CRW-1102).
func TestDeliveryAuthority_PacketSurfacesDoNotRedefineTheApproval(t *testing.T) {
	forbidden := []string{
		"explicit push approval",
		"approval `DEV-GIT-PUSH-01` (`crw-dev`) requires",
		"approval `DEV-GIT-PUSH-01` requires",
		"push approval `DEV-GIT-PUSH-01`",
		"owns the rule; this line checks that the packet carries it",
	}
	for _, rel := range []string{
		"plugins/crw/skills/crw-run/references/task-packet.md",
		"plugins/crw/skills/crw-run/references/dispatch-verification.md",
		"plugins/crw/skills/crw-plan/references/integrations.md",
	} {
		data, err := os.ReadFile(filepath.Join(repoRoot(), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(string(data)), " ")
		for _, old := range forbidden {
			if strings.Contains(text, old) {
				t.Errorf("%s keeps a packet-side definition of the push approval (%q)", rel, old)
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(repoRoot(), "plugins/crw/skills/crw-run/references/task-packet.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(data)), " ")
	for _, want := range []string{
		"`crw-dev` `DEV-GIT-PUSH-01` defines when a push is allowed; this packet is evidence that the standing grant",
		"names the grant and `crw-dev` owns the rule",
		"still carries the standing grant that `DEV-GIT-PUSH-01` (`crw-dev`) accepts",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("task-packet.md lacks %q", want)
		}
	}
}
