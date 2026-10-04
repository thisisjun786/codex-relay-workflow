package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPabcdResetHelpAndStrictArguments(t *testing.T) {
	root := pabcdCLITestHome(t)
	p := filepath.Join(root, ".crw/sessions/keep.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"-h", "--help"} {
		code, out, errOut := pabcdCLITestRun([]string{"reset", arg}, "")
		if code != 0 || out != "usage: crw pabcd reset [-h] [--state | --generated | --goalplans | --all]\n" || errOut != "" {
			t.Fatalf("help: %d %q %q", code, out, errOut)
		}
	}
	for _, args := range [][]string{{"--bogus"}, {"--state", "--all"}, {"--state", "--state"}, {"state"}, {"--all=1"}} {
		code, out, errOut := pabcdCLITestRun(append([]string{"reset"}, args...), "")
		if code != 2 || out != "" || !strings.Contains(errOut, "reset:") {
			t.Fatalf("invalid %v: %d %q %q", args, code, out, errOut)
		}
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "keep" {
		t.Fatalf("help/invalid arguments deleted state: %q %v", b, err)
	}
}

func TestPabcdResetStreamsAndPhysicalWorkspace(t *testing.T) {
	root := pabcdCLITestHome(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PWD", alias)
	p := filepath.Join(root, ".crw/interview/plan.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := pabcdCLITestRun([]string{"reset", "--generated"}, "")
	if code != 0 || errOut != "" || out != "reset --generated: removed 1 path(s)\n  - "+filepath.Dir(p)+"\n" {
		t.Fatalf("reset: %d %q %q", code, out, errOut)
	}
	code, out, errOut = pabcdCLITestRun([]string{"reset", "--generated"}, "")
	if code != 0 || errOut != "" || out != "reset --generated: removed 0 path(s) (nothing to remove)\n" {
		t.Fatalf("repeat: %d %q %q", code, out, errOut)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, ".crw/sessions")); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = pabcdCLITestRun([]string{"reset"}, "")
	if code != 1 || out != "" || !strings.Contains(errOut, "crw cli failed:") {
		t.Fatalf("failure: %d %q %q", code, out, errOut)
	}
}
