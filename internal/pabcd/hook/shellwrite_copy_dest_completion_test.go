package hook

import (
	"slices"
	"strings"
	"testing"
)

// TestShellCopyDestinationCompletion is CRW-900 defects D1 to D4 (the completion of PR #837, found by its pre-merge and
// post-merge evaluations): an import in a one-line compound statement, an alias named like a module, a from-import alias
// that keeps its function's keyword rules, and an import binding that outranks the spelling rules for Path and open. Each
// shape must name the destination the call writes; each control must name nothing.
func TestShellCopyDestinationCompletion(t *testing.T) {
	for _, c := range []struct {
		name, script string
		want         []string
	}{
		{"D1 one-line suite from-import", "if True: from shutil import copy; copy(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"D1 one-line suite import alias", "if True: import shutil as s; s.copy(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"D1 one-line else suite", "if False: pass\nelse: import os as o; o.rename(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"D1 one-line def body", "def f(): import shutil; shutil.copy(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"D2 shutil bound to the name os", "import shutil as os; os.copy(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"D2 os bound to the name shutil", "import os as shutil; shutil.rename(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"D3 renames alias takes new=", "from os import renames as mv; mv(old=\"/w/a\", new=\"/m/n.md\")", []string{"/m/n.md"}},
		{"D3 rename alias named renames takes dst=", "from os import rename as renames; renames(src=\"/w/a\", dst=\"/m/n.md\")", []string{"/m/n.md"}},
		{"D4 copy imported under the name Path", "from shutil import copy as Path; Path(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"D4 rename imported under the name open", "from os import rename as open; open(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"D4 control: real pathlib read", "from pathlib import Path; Path(\"/m/n.md\").read_text()", []string{}},
		{"D4 control: a copy from memories", "from shutil import copy as c; c(\"/m/n.md\", \"/w/b\")", []string{"/w/b"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := shellVerbScriptWrites(c.script, true); !slices.Equal(got, c.want) {
				t.Fatalf("%q: got %q, want %q", c.script, got, c.want)
			}
		})
	}
}

// TestMemoryGateDeniesCompletedCopyShapes runs the D1 to D4 shapes through the memory gate with no grant: each is denied and
// the deny names the memories path the write reaches.
func TestMemoryGateDeniesCompletedCopyShapes(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, c := range []struct {
		name, command string
	}{
		{"D1 one-line suite", "python3 -c 'if True: from shutil import copy; copy(\"/w/a\", \"" + root + "/n.md\")'"},
		{"D2 module alias", "python3 -c 'import shutil as os; os.copy(\"/w/a\", \"" + root + "/n.md\")'"},
		{"D3 renames alias", "python3 -c 'from os import renames as mv; mv(old=\"/w/a\", new=\"" + root + "/n.md\")'"},
		{"D4 copy imported as Path", "python3 -c 'from shutil import copy as Path; Path(\"/w/a\", \"" + root + "/n.md\")'"},
		{"D4 rename imported as open", "python3 -c 'from os import rename as open; open(\"/w/a\", \"" + root + "/n.md\")'"},
	} {
		t.Run(c.name, func(t *testing.T) {
			reason := gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, c.command), env))
			if !strings.Contains(reason, "("+root+"/n.md)") {
				t.Fatalf("the deny does not name %s/n.md: %s", root, reason)
			}
		})
	}
}
