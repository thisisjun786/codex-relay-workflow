package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `crw doctor flow-bound` shows the check the spawn hook makes of the flow-bound directory: absent and safe are fine, anything the
// hook would refuse is reported with its reason and exit 1.
func TestDoctorFlowBoundShowsTheHooksDirectoryCheck(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".local", "state", "crw", "flow-bound")
	run := func(args ...string) (int, string, string) {
		var out, errOut strings.Builder
		code := flowBoundCLI(args, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	if code, out, _ := run(); code != 0 || !strings.Contains(out, `"state": "absent"`) || !strings.Contains(out, dir) {
		t.Fatalf("absent: %d %s", code, out)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run("--json"); code != 0 || !strings.Contains(out, `"state": "safe"`) {
		t.Fatalf("safe: %d %s", code, out)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run(); code != 1 || !strings.Contains(out, `"state": "unsafe_dir"`) || !strings.Contains(out, "mode") {
		t.Fatalf("unsafe: %d %s", code, out)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(home, dir); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run(); code != 1 || !strings.Contains(out, `"state": "unsafe_dir"`) {
		t.Fatalf("symlink: %d %s", code, out)
	}
	if code, _, errOut := run("bogus"); code != usageExit || !strings.Contains(errOut, `unknown argument "bogus"`) {
		t.Fatalf("argument: %d %s", code, errOut)
	}
	t.Setenv("HOME", "")
	if code, out, _ := run(); code != 1 || !strings.Contains(out, `"state": "unknown"`) {
		t.Fatalf("no home: %d %s", code, out)
	}
}
