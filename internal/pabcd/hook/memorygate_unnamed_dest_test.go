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
