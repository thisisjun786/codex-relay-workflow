package install_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
)

// A selected runtime a host cannot launch - a compatibility link gone, bin/crw not executable,
// bin/crw gone - is not answered as already installed. Its directory is named for the archive's
// digest, so it cannot be built again beside itself; the refusal names the problems and the
// commands that restore it in place, and once they have run (here through sh, as an operator
// runs them) the same install answers alreadyInstalled.
func TestADamagedRuntimeIsNotInstalled(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("no tar to extract crw with")
	}
	h := newHost(t)
	first := archive(t, "0.9.0", "")
	env := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	crw := filepath.Join(env, "bin", "crw")
	for label, c := range map[string]struct {
		damage func() error
		repair string
	}{
		"a link gone":           {func() error { return os.Remove(filepath.Join(env, "bin", "codex-thread-bridge")) }, "ln -sfn crw"},
		"crw not executable":    {func() error { return os.Chmod(crw, 0o644) }, "chmod 755"},
		"crw gone":              {func() error { return os.Remove(crw) }, "tar -xzf"},
		"crw and a link broken": {func() error { _ = os.Remove(filepath.Join(env, "bin", "codex-session-relay")); return os.Remove(crw) }, "tar -xzf"},
	} {
		if err := c.damage(); err != nil {
			t.Fatal(err)
		}
		refused, code := install.Install(context.Background(), h.options(), "install", install.Source{From: first})
		repair := golden.List(at(refused, "repair"))
		if code != install.Refused || at(refused, "alreadyInstalled") == true || len(golden.List(at(refused, "launchable", "problems"))) == 0 || !strings.Contains(golden.Canon(repair), c.repair) {
			t.Fatalf("%s: exit %d\n%s", label, code, golden.Canon(refused))
		}
		for _, command := range repair {
			if out, err := exec.Command("sh", "-c", command.(string)).CombinedOutput(); err != nil {
				t.Fatalf("%s: the repair %q failed: %v %s", label, command, err, out)
			}
		}
		if again, code := install.Install(context.Background(), h.options(), "install", install.Source{From: first}); code != install.OK || at(again, "alreadyInstalled") != true {
			t.Fatalf("%s: after the repair: exit %d\n%s", label, code, golden.Canon(again))
		}
	}
}
