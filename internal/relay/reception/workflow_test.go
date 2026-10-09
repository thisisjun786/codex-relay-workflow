package reception

import (
	"strings"
	"testing"
)

// The loop arms a goalplan, so a policy that names it under another mode is refused. The workflow is
// the name the skills give a child's loop, crw-loop; the earlier product name is no alias for it.
func TestPolicyNamingTheLoopMustSayLoopMode(t *testing.T) {
	for _, c := range []struct {
		name, workflow, mode string
		refused              bool
	}{
		{"crw-loop under loop", "crw-loop", "loop", false},
		{"crw-loop under another mode", "crw-loop", "non_loop", true},
		{"spaced and capitalised", "CRW Loop with crw-pabcd", "non_loop", true},
		{"a longer word is another name", "crw-loopback", "non_loop", false},
		{"a different workflow", "plan only", "non_loop", false},
		{"the earlier product name names no loop", "CXC Loop", "non_loop", false},
	} {
		err := checkLoopWorkflowMode(O("workflow", c.workflow, "mode", c.mode))
		if c.refused != (err != nil) || (err != nil && !strings.Contains(err.Error(), "names crw-loop")) {
			t.Errorf("%s: workflow %q under mode %q: got %v, want refused=%v naming crw-loop", c.name, c.workflow, c.mode, err, c.refused)
		}
	}
}
