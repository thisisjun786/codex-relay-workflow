package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// gateRootScene is the layout the root fix is about: via is a link to real/sub, so "via/.." is real and not the directory
// that holds via, and a path that cleans "via/.." away names the sibling of the wrong directory.
type gateRootScene struct{ dir, cwd, via string }

func newGateRootScene(t *testing.T) gateRootScene {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"real/sub", "real/memories", "real/other", "real/.codex/memories", "work"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "real", "sub"), filepath.Join(dir, "via")); err != nil {
		t.Fatal(err)
	}
	return gateRootScene{dir: dir, cwd: filepath.Join(dir, "work"), via: dir + "/via"}
}

// gateRootCheck sends dest through the three write surfaces, each with no grant: a deny that names dest in the unchanged
// reason text, or silence.
func gateRootCheck(t *testing.T, cwd string, env host.LookupEnv, dest string, denied bool) {
	t.Helper()
	surfaces := map[string]map[string]any{
		"Write":       {"tool_name": "Write", "tool_input": map[string]any{"file_path": dest}},
		"shell":       {"tool_name": "Bash", "tool_input": map[string]any{"command": "echo hi > " + dest}},
		"apply_patch": {"tool_name": "apply_patch", "tool_input": map[string]any{"command": "*** Begin Patch\n*** Add File: " + dest + "\n+hi\n*** End Patch\n"}},
	}
	for name, over := range surfaces {
		out := HandleMemoryWriteGate(gatePayload(t, cwd, over), env)
		switch {
		case !denied && out != "":
			t.Errorf("%s to %s must pass, answered %q", name, dest, out)
		case denied && out == "":
			t.Errorf("%s to %s must be denied, answered nothing", name, dest)
		case denied:
			if reason := gateDeny(t, out); !strings.Contains(reason, "Blocked a write of a file under the Codex memories directory ("+dest+")") {
				t.Errorf("%s to %s: the reason text changed: %s", name, dest, reason)
			}
		}
	}
}

// With CODEX_HOME or HOME set to "<link>/..", an open of <root>/memories reaches real/memories, which the cleaned text names
// as dir/memories. Both places are the memories root.
func TestMemoryGateProtectsTheRootBehindALinkAndDotDot(t *testing.T) {
	s := newGateRootScene(t)
	for _, c := range []struct {
		name              string
		vars              map[string]string
		physical, lexical string
	}{
		{"CODEX_HOME", map[string]string{"HOME": s.dir + "/home", "CODEX_HOME": s.via + "/.."}, s.dir + "/real/memories", s.dir + "/memories"},
		{"HOME", map[string]string{"HOME": s.via + "/.."}, s.dir + "/real/.codex/memories", s.dir + "/.codex/memories"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := gateEnvOf(c.vars)
			alias := filepath.Join(s.cwd, "alias-"+c.name)
			if err := os.Symlink(c.physical, alias); err != nil { // a link in the workspace that leads into the physical root
				t.Fatal(err)
			}
			gateRootCheck(t, s.cwd, env, c.physical+"/n.md", true)
			gateRootCheck(t, s.cwd, env, alias+"/n.md", true)
			gateRootCheck(t, s.cwd, env, c.lexical+"/n.md", true) // the cleaned text stays protected
			gateRootCheck(t, s.cwd, env, s.dir+"/real/other/n.md", false)
			gateRootCheck(t, s.cwd, env, s.via+"/n.md", false) // real/sub is not widened into the root
		})
	}
}

// A relative CODEX_HOME is made absolute without cleaning its "..", and a plain relative one or an empty HOME keeps the text
// root it always had.
func TestMemoryGateRelativeRootKeepsItsTextAndFollowsItsLinks(t *testing.T) {
	s := newGateRootScene(t)
	t.Chdir(s.dir)
	linked := gateEnvOf(map[string]string{"HOME": s.dir + "/home", "CODEX_HOME": "via/.."})
	gateRootCheck(t, s.cwd, linked, s.dir+"/real/memories/n.md", true)
	gateRootCheck(t, s.cwd, linked, s.dir+"/memories/n.md", true)
	gateRootCheck(t, s.cwd, linked, s.dir+"/real/other/n.md", false)
	plain := gateEnvOf(map[string]string{"HOME": s.dir + "/home", "CODEX_HOME": "ch"})
	gateRootCheck(t, s.cwd, plain, s.dir+"/ch/memories/n.md", true)
	gateRootCheck(t, s.cwd, plain, s.dir+"/memories/n.md", false)
	emptyHome := gateEnvOf(map[string]string{"HOME": ""})
	gateRootCheck(t, s.cwd, emptyHome, s.dir+"/.codex/memories/n.md", true)
	gateRootCheck(t, s.cwd, emptyHome, s.dir+"/real/other/n.md", false)
}
