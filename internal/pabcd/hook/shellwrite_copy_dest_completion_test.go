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
		{"D1 def body import alias on its own lines", "def f():\n    import shutil as s\n    s.copy(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"D1 indented block from-import alias", "if True:\n    from shutil import copy as c\n    c(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"D1 def body from-import", "def f(): from shutil import copy as c; c(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"D4 variant: from-import of os.rename under an alias", "from os import rename as mv; mv(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"D2 binding first: json bound to shutil names no shutil copy", "import json as shutil; shutil.copy(\"/w/a\", \"/m/n.md\")", []string{}},
		{"D2 control: json bound to os is not read as os.rename", "import json as os; os.rename(\"/w/a\", \"/m/n.md\")", []string{}},
		{"D2 control: shutil bound to os has no rename", "import shutil as os; os.rename(\"/w/a\", \"/m/n.md\")", []string{}},
		{"D2 control: os bound to shutil has no copy", "import os as shutil; shutil.copy(\"/w/a\", \"/m/n.md\")", []string{}},
		{"D2 allowed: os bound to shutil renames outside memories", "import os as shutil; shutil.rename(\"/w/a\", \"/w/b\")", []string{"/w/b"}},
		{"D4 list import: copy bound beside move", "from shutil import copy, move; copy(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"D4 list import: aliased names", "from shutil import copy as c, move as m; m(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
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
		{"D1 def body import alias", "python3 -c 'def f(): import shutil as s; s.copy(\"/w/a\", \"" + root + "/n.md\")'"},
		{"D1 one-line from-import alias", "python3 -c 'if True: from shutil import copy as c; c(\"/w/a\", \"" + root + "/n.md\")'"},
		{"D2 os alias for rename into memories", "python3 -c 'def f(): import os as shutil; shutil.rename(\"/w/a\", \"" + root + "/n.md\")'"},
		{"D4 copy imported under its own name", "python3 -c 'from shutil import copy as c; c(\"/w/a\", \"" + root + "/n.md\")'"},
	} {
		t.Run(c.name, func(t *testing.T) {
			reason := gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, c.command), env))
			if !strings.Contains(reason, "("+root+"/n.md)") {
				t.Fatalf("the deny does not name %s/n.md: %s", root, reason)
			}
		})
	}
}
