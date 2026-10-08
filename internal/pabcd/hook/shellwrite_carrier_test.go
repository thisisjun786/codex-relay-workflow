package hook

import "testing"

// TestMemoryGateRunTimeCarrierDestinations: a write whose destination xargs, find -exec or parallel supplies at run time is
// an unknown destination, so the memory gate asks for a grant; a read-only find and a literal copy stay as they were.
func TestMemoryGateRunTimeCarrierDestinations(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, cmd := range []string{
		"find " + root + " -type f -exec cp /tmp/x {} +",
		"find " + root + " -type f -execdir cp /tmp/x {} +",
		"find " + root + " -ok cp /tmp/x {} \\;",
		"printf '%s' " + root + "/a | xargs -I{} cp /tmp/x {}",
		"printf '%s' " + root + "/a | xargs cp /tmp/x",
		"parallel cp /tmp/x ::: " + root + "/a",
	} {
		if got := memoryGateClassify("Bash", map[string]any{"command": cmd}, cwd, env); got.Surface != "shell" {
			t.Errorf("%q: %+v, want an attempt (the destination is made at run time)", cmd, got)
		}
	}
	for _, cmd := range []string{"find /tmp -name x", "cp /tmp/x /tmp/y", "printf '%s' /tmp/x | xargs ls"} {
		if got := memoryGateClassify("Bash", map[string]any{"command": cmd}, cwd, env); got.Surface != "" {
			t.Errorf("%q: %+v, want no attempt", cmd, got)
		}
	}
}
