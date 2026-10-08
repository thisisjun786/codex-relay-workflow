package hook

import (
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// TestShellCopyDestinationReader is CRW-900 criterion c1: the Python program reader names the destination of a copy,
// rename or link call - the second positional argument or dst= of shutil.copy/copy2/copyfile/copytree/move and
// os.rename/replace/renames/link/symlink, the argument b of Path(a).rename(b)/replace(b), and the receiver b of
// Path(b).symlink_to(a)/hardlink_to(a) - with the module read by its prefix, by a from-import bare name, or by an
// import alias. An argument that is no string literal is handled as the non-literal open() argument is: nothing named.
func TestShellCopyDestinationReader(t *testing.T) {
	for _, c := range []struct {
		name, script string
		want         []string
	}{
		{"shutil.copy", "import shutil; shutil.copy(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"shutil.copy2", "import shutil; shutil.copy2(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"shutil.copyfile", "import shutil; shutil.copyfile(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"shutil.copytree", "import shutil; shutil.copytree(\"/w/a\", \"/m/n\")", []string{"/m/n"}},
		{"shutil.move", "import shutil; shutil.move(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"os.rename", "import os; os.rename(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"os.replace", "import os; os.replace(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"os.renames", "import os; os.renames(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"os.link", "import os; os.link(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"os.symlink", "import os; os.symlink(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"the prefix without an import", "shutil.copy(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"dst= keyword", "import shutil; shutil.copy(\"/w/a\", dst=\"/m/n.md\")", []string{"/m/n.md"}},
		{"src= and dst=", "import shutil; shutil.copy(src=\"/w/a\", dst=\"/m/n.md\")", []string{"/m/n.md"}},
		{"os.rename keywords", "import os; os.rename(src=\"/w/a\", dst=\"/m/n.md\")", []string{"/m/n.md"}},
		{"from-import", "from shutil import copy; copy(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"from-import alias", "from shutil import copy as c; c(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"from os import rename", "from os import rename; rename(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"from os import rename as mv", "from os import rename as mv; mv(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"import alias", "import shutil as s; s.copy(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"import os alias", "import os as o; o.rename(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"from-import star", "from shutil import *; copy(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"parenthesised import", "from shutil import (\n    copy,\n)\ncopy(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"parenthesised import alias", "from os import (\n    rename as mv,\n)\nmv(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"parenthesised import, several names", "from shutil import (\n    disk_usage,\n    copy,\n)\ncopy(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"backslash continued import", "from shutil import copy, \\\n    copyfile\ncopy(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"parenthesised import of another function", "from shutil import (\n    disk_usage,\n)\ndisk_usage(\"/m/n.md\")", []string{}},
		{"two calls", "import shutil; import os; shutil.copy(\"/w/a\", \"/m/a.md\"); os.rename(\"/w/b\", \"/m/b.md\")", []string{"/m/a.md", "/m/b.md"}},
		{"Path.rename", "from pathlib import Path; Path(\"/w/a\").rename(\"/m/n.md\")", []string{"/m/n.md"}},
		{"Path.replace", "from pathlib import Path; Path(\"/w/a\").replace(\"/m/n.md\")", []string{"/m/n.md"}},
		{"Path.rename target=", "from pathlib import Path; Path(\"/w/a\").rename(target=\"/m/n.md\")", []string{"/m/n.md"}},
		{"Path.symlink_to", "from pathlib import Path; Path(\"/m/l\").symlink_to(\"/w/a\")", []string{"/m/l"}},
		{"Path.hardlink_to", "from pathlib import Path; Path(\"/m/l\").hardlink_to(\"/w/a\")", []string{"/m/l"}},
		{"receiver with a name part", "from pathlib import Path; Path(\"/m\", name).symlink_to(\"/w/a\")", []string{"/m"}},
		{"non-literal receiver", "from pathlib import Path; Path(get_dst()).symlink_to(\"/w/a\")", []string{}},
		{"chained receiver", "from pathlib import Path; Path(\"/w/a\").joinpath(\"x\").rename(\"/m/n.md\")", []string{}},
		{"Path.rename spaced", "from pathlib import Path; Path(\"/w/a\") . rename ( \"/m/n.md\" )", []string{"/m/n.md"}},
		{"triple quoted destination", "import shutil; shutil.copy(\"/w/a\", '''/m/n.md''')", []string{"/m/n.md"}},
		{"escaped destination", "import os; os.rename(\"/w/a\", \"\\x2fm/n.md\")", []string{"\\x2fm/n.md", "/m/n.md"}},
		{"exec program", "exec(\"import shutil; shutil.copy('/w/a', '/m/n.md')\")", []string{"/m/n.md"}},
		{"copy out of the source", "import shutil; shutil.copy(\"/m/n.md\", \"/w/b\")", []string{"/w/b"}},
		{"rename outside", "import os; os.rename(\"/w/a\", \"/w/b\")", []string{"/w/b"}},
		{"non-literal destination", "import shutil; shutil.copy(\"/w/a\", dst)", []string{}},
		{"joined destination", "import shutil; shutil.copy(\"/w/a\", \"/m/\" + name)", []string{}},
		{"no destination", "import shutil; shutil.copy(\"/w/a\")", []string{}},
		{"no arguments", "import shutil; shutil.copy()", []string{}},
		{"bare name without the from-import", "copy(\"/w/a\", \"/m/n.md\")", []string{}},
		{"an unrelated alias", "import foo as s; s.copy(\"/w/a\", \"/m/n.md\")", []string{}},
		{"an unrelated module", "import shutilx; shutilx.copy(\"/w/a\", \"/m/n.md\")", []string{}},
		{"another shutil function", "import shutil; shutil.disk_usage(\"/m/n.md\")", []string{}},
		{"another os function", "import os; os.remove(\"/m/n.md\")", []string{}},
		{"a commented import", "# import shutil as s; s.copy(\"/w/a\", \"/m/n.md\")", []string{}},
		{"an import inside a string", "x = \"import shutil as s\"; s.copy(\"/w/a\", \"/m/n.md\")", []string{}},
		{"a def header", "from shutil import copy; def copy(src, dst=\"/m/x\"): return src", []string{}},
		{"a read only Path", "from pathlib import Path; Path(\"/m/n.md\").read_text()", []string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := shellVerbScriptWrites(c.script, true); !slices.Equal(got, c.want) {
				t.Fatalf("%q: got %q, want %q", c.script, got, c.want)
			}
		})
	}
}

// The oracle reading is untouched: scriptWriteDestinations reads none of these, so hard=false names nothing for them.
func TestShellCopyDestinationOracleReadingUnchanged(t *testing.T) {
	for _, script := range []string{
		"import shutil; shutil.copy(\"/w/a\", \"/m/n.md\")",
		"import os; os.rename(\"/w/a\", \"/m/n.md\")",
		"from pathlib import Path; Path(\"/w/a\").rename(\"/m/n.md\")",
		"from pathlib import Path; Path(\"/m/l\").symlink_to(\"/w/a\")",
	} {
		if got := shellVerbScriptWrites(script, false); len(got) != 0 {
			t.Errorf("%q: the oracle reading named %q", script, got)
		}
	}
}

// The shell verb step reaches the same destinations, so a python3 -c program names them at the command boundary too.
func TestShellCopyDestinationThroughTheCommand(t *testing.T) {
	for _, c := range []struct {
		command string
		want    []string
	}{
		{"python3 -c 'import shutil; shutil.copy(\"/w/a\", \"/m/n.md\")'", []string{"/m/n.md"}},
		{"python3 -c 'import os; os.rename(\"/w/a\", \"/m/n.md\")'", []string{"/m/n.md"}},
		{"python3 -c 'from pathlib import Path; Path(\"/m/l\").symlink_to(\"/w/a\")'", []string{"/m/l"}},
		{"python -c 'import shutil as s; s.move(\"/w/a\", \"/m/n.md\")'", []string{"/m/n.md"}},
	} {
		t.Run(c.command, func(t *testing.T) {
			if got := shellWriteDestsTest(c.command); !slices.Equal(got, c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestMemoryGateDeniesCopyAndRenameIntoMemories is CRW-900 criterion c2, red first: with no grant, every destination the
// issue lists is denied by the memory gate, and the deny names the path the write reaches.
func TestMemoryGateDeniesCopyAndRenameIntoMemories(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, c := range []struct {
		name, command, target string
	}{
		{"shutil.copy", "python3 -c 'import shutil; shutil.copy(\"/w/a\", \"" + root + "/n.md\")'", root + "/n.md"},
		{"shutil.copy2", "python3 -c 'import shutil; shutil.copy2(\"/w/a\", \"" + root + "/n.md\")'", root + "/n.md"},
		{"shutil.copyfile", "python3 -c 'import shutil; shutil.copyfile(\"/w/a\", \"" + root + "/n.md\")'", root + "/n.md"},
		{"shutil.copytree", "python3 -c 'import shutil; shutil.copytree(\"/w/a\", \"" + root + "/d\")'", root + "/d"},
		{"shutil.move", "python3 -c 'import shutil; shutil.move(\"/w/a\", \"" + root + "/n.md\")'", root + "/n.md"},
		{"os.rename", "python3 -c 'import os; os.rename(\"/w/a\", \"" + root + "/n.md\")'", root + "/n.md"},
		{"os.replace", "python3 -c 'import os; os.replace(\"/w/a\", \"" + root + "/n.md\")'", root + "/n.md"},
		{"os.renames", "python3 -c 'import os; os.renames(\"/w/a\", \"" + root + "/n.md\")'", root + "/n.md"},
		{"os.link", "python3 -c 'import os; os.link(\"/w/a\", \"" + root + "/n.md\")'", root + "/n.md"},
		{"os.symlink", "python3 -c 'import os; os.symlink(\"/w/a\", \"" + root + "/n.md\")'", root + "/n.md"},
		{"from shutil import copy", "python3 -c 'from shutil import copy; copy(\"/w/a\", \"" + root + "/n.md\")'", root + "/n.md"},
		{"import shutil as s", "python3 -c 'import shutil as s; s.copy(\"/w/a\", \"" + root + "/n.md\")'", root + "/n.md"},
		{"dst= keyword", "python3 -c 'import shutil; shutil.copy(src, dst=\"" + root + "/n.md\")'", root + "/n.md"},
		{"parenthesised import", "python3 -c 'from shutil import (\n    copy,\n)\ncopy(\"/w/a\", \"" + root + "/n.md\")'", root + "/n.md"},
		{"backslash continued import", "python3 -c 'from shutil import copy, \\\n    copyfile\ncopy(\"/w/a\", \"" + root + "/n.md\")'", root + "/n.md"},
		{"os.renames new= keyword", "python3 -c 'import os; os.renames(old=\"/w/a\", new=\"" + root + "/n.md\")'", root + "/n.md"},
		{"f-string field with an outer alias", "python3 -c 'import shutil as s; f\"{s.copy(\\\"/w/a\\\", \\\"" + root + "/n.md\\\")}\"'", root + "/n.md"},
		{"exec with an outer alias", "python3 -c 'import shutil as s; exec(\"s.copy(\\\"/w/a\\\", \\\"" + root + "/n.md\\\")\")'", root + "/n.md"},
		{"Path.rename", "python3 -c 'from pathlib import Path; Path(\"/w/a\").rename(\"" + root + "/n.md\")'", root + "/n.md"},
		{"Path.replace", "python3 -c 'from pathlib import Path; Path(\"/w/a\").replace(\"" + root + "/n.md\")'", root + "/n.md"},
		{"Path.symlink_to", "python3 -c 'from pathlib import Path; Path(\"" + root + "/l\").symlink_to(\"/w/a\")'", root + "/l"},
		{"Path.hardlink_to", "python3 -c 'from pathlib import Path; Path(\"" + root + "/l\").hardlink_to(\"/w/a\")'", root + "/l"},
	} {
		t.Run(c.name, func(t *testing.T) {
			reason := gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, c.command), env))
			if !strings.Contains(reason, "("+c.target+")") {
				t.Fatalf("the deny does not name %s: %s", c.target, reason)
			}
		})
	}
}

// The allowed controls of the same criterion: a copy out of memories, a rename outside it, and a copy in a session that
// holds the grant all pass.
func TestMemoryGateAllowsCopyAndRenameOutsideMemories(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, c := range []struct{ name, command string }{
		{"copy out of memories", "python3 -c 'import shutil; shutil.copy(\"" + root + "/n.md\", \"/w/b\")'"},
		{"rename outside", "python3 -c 'import os; os.rename(\"/w/a\", \"/w/b\")'"},
		{"a sibling that only looks like the root", "python3 -c 'import shutil; shutil.copy(\"/w/a\", \"" + root + "-backup/n.md\")'"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := HandleMemoryWriteGate(gateBash(t, cwd, c.command), env); got != "" {
				t.Fatalf("%q must pass, answered %q", c.command, got)
			}
		})
	}
}

// The same copy passes in a session whose grant is not yet spent.
func TestMemoryGateAllowsCopyWithAGrant(t *testing.T) {
	cwd, root, env := gateScene(t)
	gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
	command := "python3 -c 'import shutil; shutil.copy(\"/w/a\", \"" + root + "/n.md\")'"
	if got := HandleMemoryWriteGate(gateBash(t, cwd, command), env); got != "" {
		t.Fatalf("the granted copy: %q", got)
	}
	if state.ReadState(cwd, gateSession).MemoryWriteGrant {
		t.Error("the grant is not spent")
	}
}

// TestShellCopyDestinationReviewFixes pins the shapes this pull request's reviews found: the destination keyword of
// os.renames is new, an alias any import in the program bound counts for a call made before a later rebinding, and an
// enclosing program's imports reach an expression read recursively (an f-string replacement field, an exec program).
func TestShellCopyDestinationReviewFixes(t *testing.T) {
	for _, c := range []struct {
		name, script string
		want         []string
	}{
		{"os.renames new=", "import os; os.renames(old=\"/w/a\", new=\"/m/n.md\")", []string{"/m/n.md"}},
		{"os.renames new= and old positional", "import os; os.renames(\"/w/a\", new=\"/m/n.md\")", []string{"/m/n.md"}},
		{"alias before a later rebinding", "import shutil as s; s.copy(\"/w/a\", \"/m/n.md\"); import json as s", []string{"/m/n.md"}},
		{"alias after a rebinding", "import json as s; import shutil as s; s.copy(\"/w/a\", \"/m/n.md\")", []string{"/m/n.md"}},
		{"f-string field with an outer alias", "import shutil as s; f\"{s.copy('/w/a', '/m/n.md')}\"", []string{"/m/n.md"}},
		{"exec with an outer alias", "import shutil as s; exec(\"s.copy('/w/a', '/m/n.md')\")", []string{"/m/n.md"}},
		{"exec with an outer from-import", "from os import rename; exec(\"rename('/w/a', '/m/n.md')\")", []string{"/m/n.md"}},
		{"an unrelated alias is still no copy", "import json as s; s.copy(\"/w/a\", \"/m/n.md\")", []string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := shellVerbScriptWrites(c.script, true); !slices.Equal(got, c.want) {
				t.Fatalf("%q: got %q, want %q", c.script, got, c.want)
			}
		})
	}
}

// The pathlib methods this issue added stay in the Python reading: a Node program keeps the reading it had before
// CRW-900, so a JavaScript identifier named Path is not a filesystem rename.
func TestShellCopyPathMethodsStayPythonOnly(t *testing.T) {
	for _, script := range []string{
		"Path(\"/w/a\").rename(\"/m/n.md\")",
		"Path(\"/m/l\").symlink_to(\"/w/a\")",
		"Path(\"/m/l\").hardlink_to(\"/w/a\")",
	} {
		if got := shellVerbScriptWritesIn(script, true, false); len(got) != 0 {
			t.Errorf("%q: a Node program named %q", script, got)
		}
	}
}
