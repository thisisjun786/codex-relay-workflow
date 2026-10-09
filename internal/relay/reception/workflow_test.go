package reception

import (
	"strings"
	"testing"
)

// The loop arms a goalplan, so a policy that names it under another mode is refused. The workflow is
// the name the skills give a child's loop, crw-loop, and the name the recorded packets and the CXC
// hosts still carry, CXC Loop: the rename never loosens the check for either.
func TestPolicyNamingTheLoopMustSayLoopMode(t *testing.T) {
	for _, c := range []struct {
		name, workflow, mode, names string // names is the name the refusal quotes; empty when the policy passes
	}{
		{"crw-loop under loop", "crw-loop", "loop", ""},
		{"crw-loop under another mode", "crw-loop", "non_loop", "crw-loop"},
		{"spaced and capitalised", "CRW Loop with crw-pabcd", "non_loop", "crw-loop"},
		{"a longer word is another name", "crw-loopback", "non_loop", ""},
		{"a different workflow", "plan only", "non_loop", ""},
		{"the earlier product name under another mode", "CXC Loop", "non_loop", "CXC Loop"},
		{"the earlier product name, hyphenated", "cxc-loop with cxc-pabcd", "non_loop", "CXC Loop"},
		{"the earlier product name under loop", "CXC Loop", "loop", ""},
		{"a longer word is another name for the earlier one too", "cxc-loopback", "non_loop", ""},
	} {
		err := checkLoopWorkflowMode(O("workflow", c.workflow, "mode", c.mode))
		if (c.names != "") != (err != nil) || (err != nil && !strings.Contains(err.Error(), "names "+c.names+" ")) {
			t.Errorf("%s: workflow %q under mode %q: got %v, want a refusal naming %q (empty: none)", c.name, c.workflow, c.mode, err, c.names)
		}
	}
}
