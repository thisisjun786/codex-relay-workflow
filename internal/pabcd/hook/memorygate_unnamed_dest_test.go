package hook

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-951: the memory write gate's Python program reader names only string-literal destinations, so a program that
// computes its destination (os.path.join, Path(...) / name, a chained receiver, a function value, getattr, __import__)
// writes into the memories root without a grant. The third fail-closed check of the shell branch refuses it, beside
// CRW-741's f-string field and CRW-726's unreadable program, with the target
// "(a program the gate cannot read: a write whose destination it cannot name)". The oracle's scriptWriteDestinations
// names the same literal destinations only, so it allows the same programs: this is a security fix (port: fixed), and
// the over-blocking it accepts is recorded in docs/port-cxc/known-defects/CRW-951.md.

// unnamedDestWant is the target the third check reports, written out so the red run compiles against dev.
const unnamedDestWant = "(a program the gate cannot read: a write whose destination it cannot name)"

// unnamedDestWhat is the what inside that target.
const unnamedDestWhat = "a write whose destination it cannot name"

// unnamedDestPrograms are the writes the issue body lists as allowed without a grant on dev: every destination is the
// memories root's n.md, and the reader names none of them. Each is a python3 -c program as the issue writes it.
func unnamedDestPrograms(root string) []struct{ name, command string } {
	m := root + "/n.md"
	py := func(program string) string { return "python3 -c " + shellWriteUnnamedQuote(program) }
	return []struct{ name, command string }{
		{"open(Path(m), w)", py("from pathlib import Path; open(Path(\"" + m + "\"), \"w\")")},
		{"open(os.path.join(root, n.md), w)", py("import os; open(os.path.join(\"" + root + "\", \"n.md\"), \"w\")")},
		{"f = open; f(m, w)", py("f = open; f(\"" + m + "\", \"w\")")},
		{"Path(root).joinpath(n.md).write_text", py("from pathlib import Path; Path(\"" + root + "\").joinpath(\"n.md\").write_text(\"x\")")},
		{"(Path(root) / n.md).write_text", py("from pathlib import Path; (Path(\"" + root + "\") / \"n.md\").write_text(\"x\")")},
		{"Path(/w/a).absolute().rename(m)", py("from pathlib import Path; Path(\"/w/a\").absolute().rename(\"" + m + "\")")},
		{"Path(/w/a).rename(Path(m))", py("from pathlib import Path; Path(\"/w/a\").rename(Path(\"" + m + "\"))")},
		{"f = shutil.copy; f(/w/a, m)", py("import shutil; f = shutil.copy; f(\"/w/a\", \"" + m + "\")")},
		{"getattr(shutil, copy)(/w/a, m)", py("import shutil; getattr(shutil, \"copy\")(\"/w/a\", \"" + m + "\")")},
		{"__import__(shutil).copy(/w/a, m)", py("__import__(\"shutil\").copy(\"/w/a\", \"" + m + "\")")},
		{"shutil.copy(/w/a, os.path.join(root, n.md))", py("import shutil, os; shutil.copy(\"/w/a\", os.path.join(\"" + root + "\", \"n.md\"))")},
	}
}

// shellWriteUnnamedQuote wraps a program so a real shell hands it to the interpreter unchanged. A program without a
// single quote takes the plain single-quoted form; one holding a single quote takes the double-quoted form with the
// characters the shell still reads inside double quotes escaped.
func shellWriteUnnamedQuote(program string) string {
	if !strings.ContainsRune(program, '\'') {
		return "'" + program + "'"
	}
	r := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "$", "\\$", "`", "\\`")
	return "\"" + r.Replace(program) + "\""
}

// TestMemoryGateUnnamedDestinationClassify is c1 and c2's classification: every program of the issue body is an attempt
// of its own with the new target, which the red run of this test shows as a pass on the fixed tree alone.
func TestMemoryGateUnnamedDestinationClassify(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, c := range unnamedDestPrograms(root) {
		t.Run(c.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": c.command}, cwd, env)
			if got.Surface != "shell" || got.Target != unnamedDestWant {
				t.Errorf("%q: %+v, want the shell surface and %s", c.command, got, unnamedDestWant)
			}
		})
	}
}

// TestMemoryGateUnnamedDestinationDeniesAndSpends is c2's envelope case: each program is denied without a grant with a
// reason naming the new target, and with one the write passes and the grant is consumed.
func TestMemoryGateUnnamedDestinationDeniesAndSpends(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, c := range unnamedDestPrograms(root) {
		t.Run(c.name, func(t *testing.T) {
			payload := gateBash(t, cwd, c.command)
			reason := gateDeny(t, HandleMemoryWriteGate(payload, env))
			if !strings.Contains(reason, unnamedDestWhat) {
				t.Errorf("the reason does not name the unnamed destination: %s", reason)
			}
			gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
			if out := HandleMemoryWriteGate(payload, env); out != "" {
				t.Errorf("with a grant the write must pass: %s", out)
			}
			if state.ReadState(cwd, gateSession).MemoryWriteGrant {
				t.Error("the grant was not spent")
			}
		})
	}
}

// TestMemoryGateUnnamedDestinationControls is c2's control set: the same computed shapes writing under /w with no
// protected-area literal pass, a read-only open with a computed destination under the root passes, and a literal
// destination keeps the reading it had. The protected-area condition is what the check turns on.
func TestMemoryGateUnnamedDestinationControls(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, c := range []struct{ name, command string }{
		{"computed open under /w", "python3 -c 'from pathlib import Path; open(Path(\"/w/n.md\"), \"w\")'"},
		{"computed join under /w", "python3 -c 'import os; open(os.path.join(\"/w\", \"n.md\"), \"w\")'"},
		{"open through a variable under /w", "python3 -c 'f = open; f(\"/w/n.md\", \"w\")'"},
		{"chained Path under /w", "python3 -c 'from pathlib import Path; Path(\"/w\").joinpath(\"n.md\").write_text(\"x\")'"},
		{"chained rename under /w", "python3 -c 'from pathlib import Path; Path(\"/w/a\").absolute().rename(\"/w/n.md\")'"},
		{"copy through a variable under /w", "python3 -c 'import shutil; f = shutil.copy; f(\"/w/a\", \"/w/n.md\")'"},
		{"getattr under /w", "python3 -c 'import shutil; getattr(shutil, \"copy\")(\"/w/a\", \"/w/n.md\")'"},
		{"import under /w", "python3 -c '__import__(\"shutil\").copy(\"/w/a\", \"/w/n.md\")'"},
		{"read-only open of a computed path under the root", "python3 -c 'import os; open(os.path.join(\"" + root + "\", \"n.md\"))'"},
		{"read-only Path under the root", "python3 -c 'from pathlib import Path; Path(\"" + root + "\", \"n.md\").read_text()'"},
		{"a destination outside the root", "python3 -c 'import os; open(os.path.join(\"/w\", \"n.md\"), \"w\")'"},
		{"a read mode open of a computed path under the root", "python3 -c 'import os; open(os.path.join(\"" + root + "\", \"n.md\"), \"r\")'"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := memoryGateClassify("Bash", map[string]any{"command": c.command}, cwd, env); got.Surface != "" {
				t.Errorf("%q must pass: %+v", c.command, got)
			}
		})
	}
	// A literal destination is named by the ordinary check, not by this one, so its target is the path.
	for _, c := range []struct{ name, command, target string }{
		{"literal open", "python3 -c 'open(\"" + root + "/n.md\", \"w\")'", root + "/n.md"},
		{"literal copy", "python3 -c 'import shutil; shutil.copy(\"/w/a\", \"" + root + "/n.md\")'", root + "/n.md"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": c.command}, cwd, env)
			if got.Surface != "shell" || got.Target != c.target {
				t.Errorf("%q: %+v, want the named destination %s", c.command, got, c.target)
			}
		})
	}
}

// TestMemoryGateUnnamedDestinationOtherSources is c1's other two program sources: a program passed to exec as a string
// literal, and the body of a here-document the reader reads.
func TestMemoryGateUnnamedDestinationOtherSources(t *testing.T) {
	cwd, root, env := gateScene(t)
	m := root + "/n.md"
	py := func(program string) string { return "python3 -c " + shellWriteUnnamedQuote(program) }
	inner := py("exec(\"open(Path(\\\"" + m + "\\\"), \\\"w\\\")\")")
	for _, c := range []struct{ name, command string }{
		{"exec string", py("exec(\"import shutil; shutil.copy(chr(47), os.path.join('" + root + "', 'n.md'))\")")},
		{"exec string inside a shell -c", "bash -c " + shellWriteUnnamedQuote(inner)},
		{"here-document program", "python3 <<'EOF'\nimport shutil\nshutil.copy(\"/w/a\", os.path.join(\"" + root + "\", \"n.md\"))\nEOF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": c.command}, cwd, env)
			if got.Surface != "shell" || got.Target != unnamedDestWant {
				t.Errorf("%q: %+v, want the shell surface and %s", c.command, got, unnamedDestWant)
			}
		})
	}
}

// TestMemoryGateUnnamedDestinationStaysOnPythonPrograms: a command that runs no Python program is not this check's case,
// a Python program with no unnamed write is not either, and the check sits behind the protected-root guard.
func TestMemoryGateUnnamedDestinationStaysOnPythonPrograms(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, command := range []string{
		"echo hi > " + root + "/n.md",
		"node -e 'require(\"fs\").writeFileSync(\"" + root + "/n.md\", \"x\")'",
		"perl -i -pe 's/a/b/' " + root + "/MEMORY.md",
	} {
		if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); got.Target == unnamedDestWant {
			t.Errorf("%q must not be this check's case: %+v", command, got)
		}
	}
	// A Python program that names the protected area and holds no unnamed write passes this check: the ordinary
	// destination check decides it.
	for _, command := range []string{
		"python3 -c 'print(\"" + root + "/n.md\")'",
		"python3 -c 'import shutil; shutil.copy(\"/w/a\", \"" + root + "/n.md\")'",
	} {
		if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); got.Target == unnamedDestWant {
			t.Errorf("%q must not be this check's case: %+v", command, got)
		}
	}
	if _, ok := (memoryGateEnv{homeOK: false}).root(); ok {
		t.Error("a gate env with no resolvable home must have no protected root")
	}
}

// TestMemoryGateUnnamedDestinationReviewFixes pins the shapes this pull request's Devin and Codex reviews found: a
// Path(...) call whose argument is not a literal names only its directory, a write method on a receiver the reader never
// named is a write whose destination it cannot name, an exec program keeps its own imports, a program the reader cannot
// read fails closed, a def header and an assignment bind a name rather than calling it, and a * or ** argument leaves
// the mode and the path unread.
func TestMemoryGateUnnamedDestinationReviewFixes(t *testing.T) {
	cwd, _, env := gateScene(t)
	py := func(program string) string { return "python3 -c " + shellWriteUnnamedQuote(program) }
	for _, c := range []struct{ name, command string }{
		// A Path(...) call whose argument is not a literal names its directory, not a destination.
		{"Path prefix with a non-literal part", py("from pathlib import Path; Path('/w', 'memories').joinpath('n.md').write_text('x')")},
		// A write method on a receiver the reader never named.
		{"variable receiver", py("from pathlib import Path; p = Path('/w') / 'memories' / 'n.md'; p.write_text('x')")},
		{"variable receiver from the environment", py("import os; from pathlib import Path; p = Path(os.environ['CODEX_HOME']) / 'memories' / 'n.md'; p.write_text('x')")},
		// An exec program keeps its own imports beside the enclosing program's.
		{"exec program with its own import", py("exec(\"import shutil as s, os; s.copy('/w/a', os.path.join(os.environ['CODEX_HOME'], 'memories', 'n.md'))\")")},
		// A program the reader cannot read fails closed.
		{"expanded here-document", "python3 <<PY\nimport os\nopen(os.path.join('$CODEX_HOME', 'memories', 'n.md'), 'w')\nPY"},
		{"redirection before the interpreter", "0<<'PY' python3\nimport os\nopen(os.path.join(os.environ['CODEX_HOME'], 'memories', 'n.md'), 'w')\nPY"},
		// A * or ** argument leaves the mode and the path unread.
		{"unpacked open arguments", py("import os; opts = {'file': os.path.join(os.environ['CODEX_HOME'], 'memories', 'n.md'), 'mode': 'w'}; open(**opts)")},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": c.command}, cwd, env)
			if got.Surface != "shell" || got.Target != unnamedDestWant {
				t.Errorf("%q: %+v, want the shell surface and %s", c.command, got, unnamedDestWant)
			}
		})
	}
	// The controls the reviews asked for: a def header and an assignment bind a name rather than calling it, and an
	// interpreter that runs a script operand does not read its here-document as the program.
	for _, c := range []struct{ name, command string }{
		{"def header", py("def open(file, mode='w'): pass; print('memories')")},
		{"assignment target", py("open = print; print('memories')")},
		{"script operand with a here-document", "python3 consume.py <<'EOF'\nopen(Path('memories'), 'w')\nEOF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := memoryGateClassify("Bash", map[string]any{"command": c.command}, cwd, env); got.Surface != "" {
				t.Errorf("%q must pass: %+v", c.command, got)
			}
		})
	}
}

// TestMemoryGateUnnamedDestinationMethodReceivers pins the two shapes this check's own review of the receiver rule
// found: a method named open (p.open("w")) writes to its receiver, which this reader names for no open() method, while
// an ordinary string's own replace names no file and must not be refused.
func TestMemoryGateUnnamedDestinationMethodReceivers(t *testing.T) {
	cwd, root, env := gateScene(t)
	py := func(program string) string { return "python3 -c " + shellWriteUnnamedQuote(program) }
	for _, c := range []struct{ name, command string }{
		{"variable receiver open", py("from pathlib import Path; p = Path('/w'); p.open('w'); print('memories')")},
		{"literal receiver open", py("from pathlib import Path; Path('/w/n.md').open('w'); print('memories')")},
		{"literal receiver open at the root", py("from pathlib import Path; Path('" + root + "/n.md').open('w')")},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": c.command}, cwd, env)
			if got.Surface != "shell" || got.Target != unnamedDestWant {
				t.Errorf("%q: %+v, want the shell surface and %s", c.command, got, unnamedDestWant)
			}
		})
	}
	for _, c := range []struct{ name, command string }{
		{"string replace literal receiver", py("x = 'memories'.replace('a','b'); print(x)")},
		{"string replace variable receiver", py("s = 'memories'; x = s.replace('a','b'); print(x)")},
		{"string method that names no file", py("s = 'memories'; print(s.upper())")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := memoryGateClassify("Bash", map[string]any{"command": c.command}, cwd, env); got.Surface != "" {
				t.Errorf("%q must pass: %+v", c.command, got)
			}
		})
	}
}

// TestMemoryGateUnnamedDestinationRenameAndGetattr pins two shapes this check's own review found: a CRW-900 rename or
// link call whose receiver the reader never named (a variable Path), and getattr whose name argument is not a literal,
// so the reader cannot tell which function it reaches. An ordinary string's own replace takes two arguments and writes
// no file, so it stays allowed.
func TestMemoryGateUnnamedDestinationRenameAndGetattr(t *testing.T) {
	cwd, _, env := gateScene(t)
	py := func(program string) string { return "python3 -c " + shellWriteUnnamedQuote(program) }
	for _, c := range []struct{ name, command string }{
		{"variable receiver replace", py("from pathlib import Path; p = Path('/w/a'); p.replace('/w/b'); print('memories')")},
		{"variable receiver symlink_to", py("from pathlib import Path; p = Path('/w/l'); p.symlink_to('/w/a'); print('memories')")},
		{"computed getattr name", py("import shutil; getattr(shutil, 'co'+'py')('/w/a', '/w/b'); print('memories')")},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": c.command}, cwd, env)
			if got.Surface != "shell" || got.Target != unnamedDestWant {
				t.Errorf("%q: %+v, want the shell surface and %s", c.command, got, unnamedDestWant)
			}
		})
	}
	for _, c := range []struct{ name, command string }{
		{"string replace with two arguments", py("s = 'memories'; print(s.replace('a','b'))")},
		{"string replace with three arguments", py("s = 'memories'; print(s.replace('a','b',1))")},
		{"getattr of a name that is no write function", py("import shutil; getattr(shutil, 'disk_usage')('/w'); print('memories')")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := memoryGateClassify("Bash", map[string]any{"command": c.command}, cwd, env); got.Surface != "" {
				t.Errorf("%q must pass: %+v", c.command, got)
			}
		})
	}
}

// TestMemoryGateUnnamedDestinationMethodOpenMode pins the mode a method named open is called with: Path(...).open(mode)
// gives it as the only argument and module.open(path, mode) as the last one, so a read mode is no write and a write mode
// is. An ordinary module's read-only open must not be refused.
func TestMemoryGateUnnamedDestinationMethodOpenMode(t *testing.T) {
	cwd, _, env := gateScene(t)
	py := func(program string) string { return "python3 -c " + shellWriteUnnamedQuote(program) }
	for _, c := range []struct{ name, command string }{
		{"Path literal open write", py("from pathlib import Path; Path('memories/n.md').open('w')")},
		{"variable receiver open write", py("from pathlib import Path; p = Path('/w'); p.open('w'); print('memories')")},
		{"module open write", py("import tarfile; tarfile.open('memories/a.tar','w')")},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": c.command}, cwd, env)
			if got.Surface != "shell" || got.Target != unnamedDestWant {
				t.Errorf("%q: %+v, want the shell surface and %s", c.command, got, unnamedDestWant)
			}
		})
	}
	for _, c := range []struct{ name, command string }{
		{"module open read", py("import tarfile; tarfile.open('memories/a.tar','r')")},
		{"module open read by keyword", py("import tarfile; tarfile.open('memories/a.tar', mode='r')")},
		{"module open with no mode", py("import tarfile; tarfile.open('memories/a.tar')")},
		{"Path literal open read", py("from pathlib import Path; Path('memories/n.md').open('r')")},
		{"Path literal open with no mode", py("from pathlib import Path; Path('memories/n.md').open()")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := memoryGateClassify("Bash", map[string]any{"command": c.command}, cwd, env); got.Surface != "" {
				t.Errorf("%q must pass: %+v", c.command, got)
			}
		})
	}
}

// TestMemoryGateUnnamedDestinationPremergeFixes pins the shapes the pre-merge evaluation found: a variable receiver's
// open() whose mode is computed (the reader cannot read the mode, so the call fails closed), a write method taken off
// its receiver as a value, an interpreter option that takes its value as a separate word before a here-document, and a
// here-document inside a nested shell program. The controls pin the two read-only programs the same reading must leave
// allowed: a string's own replace on a call or parenthesized receiver, and an unquoted here-document that expands a
// variable but writes nothing.
func TestMemoryGateUnnamedDestinationPremergeFixes(t *testing.T) {
	cwd, root, env := gateScene(t)
	m := root + "/n.md"
	py := func(program string) string { return "python3 -c " + shellWriteUnnamedQuote(program) }
	for _, c := range []struct{ name, command string }{
		// A variable receiver may hold a Path or a module, so the reader reads the mode from both positions; a mode
		// that is no literal is one it cannot name.
		{"variable receiver with a computed mode", py("from pathlib import Path; p = Path(\"" + m + "\"); mode = \"w\"; p.open(mode).write(\"x\")")},
		{"variable receiver with a computed keyword mode", py("from pathlib import Path; p = Path(\"" + m + "\"); p.open(mode=open(\"/w/mode\").read())")},
		// A write method taken off its receiver as a value writes through whatever name it is bound to.
		{"write method as a value", py("from pathlib import Path; p = Path(\"" + m + "\"); f = p.write_text; f(\"x\")")},
		{"write method of a literal receiver as a value", py("from pathlib import Path; f = Path(\"" + m + "\").write_bytes; f(b\"x\")")},
		{"rename method as a value", py("from pathlib import Path; p = Path(\"/w/a\"); f = p.rename; f(\"" + m + "\")")},
		// An interpreter option whose value is a separate word does not make the interpreter a script runner.
		{"interpreter option before a here-document", "python3 -W ignore <<'PY'\nimport os\nopen(os.path.join(\"" + root + "\", \"n.md\"), \"w\")\nPY"},
		{"interpreter -X option before a here-document", "python3 -X utf8 <<'PY'\nimport os\nopen(os.path.join(\"" + root + "\", \"n.md\"), \"w\")\nPY"},
		// A here-document inside a nested shell program is read too.
		{"here-document inside a shell -c", "bash -c " + shellWriteUnnamedQuote("python3 <<'PY'\nimport os\nopen(os.path.join('"+root+"', 'n.md'), 'w')\nPY")},
		{"unquoted here-document inside a shell -c", "bash -c " + shellWriteUnnamedQuote("python3 <<PY\nimport os\nopen(os.path.join('"+root+"', 'n.md'), 'w')\nPY")},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": c.command}, cwd, env)
			if got.Surface != "shell" || got.Target != unnamedDestWant {
				t.Errorf("%q: %+v, want the shell surface and %s", c.command, got, unnamedDestWant)
			}
		})
	}
	for _, c := range []struct{ name, command string }{
		// A string's own replace takes two arguments and writes no file, whatever the receiver is.
		{"string replace on a call receiver", py("print(str('memories').replace('m', 'M'))")},
		{"string replace on a parenthesized receiver", py("print(('memories').replace('m', 'M'))")},
		{"string replace on a variable receiver", py("s = 'memories'; print(s.replace('m', 'M'))")},
		// An unquoted here-document that expands a variable but writes nothing is read as the program it is.
		{"expanded here-document with no write", "python3 <<PY\nprint(\"$USER\")\nPY"},
		{"expanded here-document that only reads", "python3 <<PY\nopen(\"$HOME/notes.md\").read()\nPY"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := memoryGateClassify("Bash", map[string]any{"command": c.command}, cwd, env); got.Surface != "" {
				t.Errorf("%q must pass: %+v", c.command, got)
			}
		})
	}
}

// TestMemoryGateUnnamedDestinationControlShapes is the matching /w control set the pre-merge evaluation asked for: the
// computed shapes of the issue body that write under /w with no protected-area literal must stay allowed, so the
// protected-area condition alone decides.
func TestMemoryGateUnnamedDestinationControlShapes(t *testing.T) {
	cwd, _, env := gateScene(t)
	py := func(program string) string { return "python3 -c " + shellWriteUnnamedQuote(program) }
	for _, c := range []struct{ name, command string }{
		{"chained Path under /w", py("from pathlib import Path; (Path('/w') / 'n.md').write_text('x')")},
		{"Path rename with a Path argument under /w", py("from pathlib import Path; Path('/w/a').rename(Path('/w/n.md'))")},
		{"copy to a computed destination under /w", py("import shutil, os; shutil.copy('/w/a', os.path.join('/w', 'n.md'))")},
		{"variable receiver with a computed mode under /w", py("from pathlib import Path; p = Path('/w/n.md'); mode = 'w'; p.open(mode)")},
		{"write method as a value under /w", py("from pathlib import Path; p = Path('/w/n.md'); f = p.write_text; f('x')")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := memoryGateClassify("Bash", map[string]any{"command": c.command}, cwd, env); got.Surface != "" {
				t.Errorf("%q must pass: %+v", c.command, got)
			}
		})
	}
}
