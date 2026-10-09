package hook

import (
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-741: the shell write-destination reader reads the replacement fields of a Python f-string, so an open() call
// inside one names its path and the memory gate stops it, and it fails closed when a field cannot be read. Every case
// is a command string or a program; the oracle's own answer stays first in the result and this reading adds after it.
// Heredoc and standard-input programs are stripped before the reader runs (shellwrite.go stripHeredocBodies) and are
// out of scope here (CRW-765).

// shellWriteFStringRows are the five python -c commands of the issue table, with the path each names.
func shellWriteFStringRows(mem string) []struct{ name, command string } {
	return []struct{ name, command string }{
		{"triple-quoted keyword", "python3 -c \"f''' '{open(file='" + mem + "/a', mode='w')} '''\""},
		{"single-quoted keyword", "python3 -c \"f'{open(file=\\\"" + mem + "/a\\\", mode=\\\"w\\\")}'\""},
		{"double-quote triple keyword", "python3 -c 'f\"\"\"{open(file=\"" + mem + "/a\", mode=\"w\")}\"\"\"'"},
		{"PEP 701 same-quote nesting", "python3 -c \"f'{open(file='" + mem + "/a', mode='w')}'\""},
		{"field nested in a format spec", "python3 -c \"x=1; f'{x:{open(file=\\\"" + mem + "/a\\\", mode=\\\"w\\\")}}'\""},
	}
}

// TestShellWriteFStringFields is the reader case: the five -c rows of the issue table, a conversion field, an inner
// f-string, a spec field and the prefix and quote variants each name their path. On dev each named nothing.
func TestShellWriteFStringFields(t *testing.T) {
	const mem = "/h/memories"
	for _, row := range shellWriteFStringRows(mem) {
		t.Run(row.name, func(t *testing.T) {
			if got := shellWriteDestsTest(row.command); !slices.Contains(got, mem+"/a") {
				t.Errorf("%q named %q, want %q", row.command, got, mem+"/a")
			}
		})
	}
	for _, c := range []struct{ name, program string }{
		{"single-quoted field", `f'{open("/m/a", "w")}'`},
		{"double-quoted field", `f"{open('/m/a', 'w')}"`},
		{"keyword file and mode", `f'{open(file="/m/a", mode="w")}'`},
		{"rf prefix", `rf'{open("/m/a", "w")}'`},
		{"fr prefix", `fr'{open("/m/a", "w")}'`},
		{"Rf prefix", `Rf'{open("/m/a", "w")}'`},
		{"fR prefix", `fR'{open("/m/a", "w")}'`},
		{"F prefix", `F'{open("/m/a", "w")}'`},
		{"triple-quoted field", `f'''{open("/m/a", "w")}'''`},
		{"conversion field", `f'{open("/m/a", "w")!r}'`},
		{"conversion beside a later field", `f'{x!r}{open("/m/a", "w")}'`},
		{"not-equal is no conversion", `f'{a!=b} {open("/m/a", "w")}'`},
		{"inner f-string", `f'{f"{open("/m/a", "w")}"}'`},
		{"field in a format spec", `f'{x:{open("/m/a", "w")}}'`},
		{"field in a list in the expression", `f'{[open("/m/a", "w")]}'`},
		{"doubled braces are literal", `f'{{}} {open("/m/a", "w")}'`},
		{"literal text around a field", `f'text {open("/m/a", "w")} more'`},
		{"Path call in a field", `f'{Path("/m/a").write_text("x")}'`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := shellVerbOpenWrites(c.program); !slices.Contains(got, "/m/a") {
				t.Errorf("%q named %q, want /m/a", c.program, got)
			}
		})
	}
	// A field-free f-string still names nothing (the reading before this change).
	if got := shellVerbOpenWrites(`f'{{open}}'`); len(got) != 0 {
		t.Errorf("doubled braces named %q", got)
	}
}

// TestShellWriteFStringUnchanged is the invariant case: a literal without an f in its prefix, a Node program and a
// directly visible destination read exactly as before.
func TestShellWriteFStringUnchanged(t *testing.T) {
	for _, c := range []struct{ name, program string }{
		{"f-less literal is data", `x='''open(file="/m/a", mode="w")'''`},
		{"f-less literal with a quote", `x="open(file='/m/a', mode='w')"`},
		{"doubled braces name nothing", `print(f'{{open}}')`},
		{"a name ending in f is no prefix", `buf='open("/m/a", "w")'`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := shellVerbOpenWrites(c.program); len(got) != 0 {
				t.Errorf("%q named %q", c.program, got)
			}
		})
	}
	// A Node program is not Python source: the same text that is an f-string in Python is not walked when python is false.
	if got := shellVerbOpenWritesIn("const x = `f\"{open('/m/a','w')}\"`", false); len(got) != 0 {
		t.Errorf("a Node program was walked as Python: %q", got)
	}
	// A directly visible destination still reads as before.
	if got := shellWriteDestsTest(`python3 -c "open(file='/m/a', mode='w')"`); !slices.Contains(got, "/m/a") {
		t.Errorf("a direct open(file=) named %q", got)
	}
}

// TestShellWriteFStringUnreadable is the fail-closed case: a field the walk cannot finish reports the reason, and a
// well-formed program reports none.
func TestShellWriteFStringUnreadable(t *testing.T) {
	deep := "z"
	for i := 0; i < 40; i++ {
		deep = "{v:" + deep + "}"
	}
	for _, c := range []struct{ name, command string }{
		{"unclosed field at the program end", `python3 -c "f'{open(file='/m/a', mode='w')"`},
		{"unclosed field before the literal end", `python3 -c "f'{x"`},
		{"unpaired closing brace", `python3 -c "f'}x'"`},
		{"field nesting deeper than 32", "python3 -c \"f'" + deep + "'\""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := shellIRFStringUnreadable(c.command)
			if !ok || got != shellWriteFStringUnreadableWhat {
				t.Errorf("%q: got %q, %v; want %q, true", c.command, got, ok, shellWriteFStringUnreadableWhat)
			}
		})
	}
	for _, command := range []string{
		`python3 -c "f'{x}'"`,
		`python3 -c "f'{{open}}'"`,
		`python3 -c "x='''open(file='/m/a', mode='w')'''"`,
		`python3 -c "open(file='/m/a', mode='w')"`,
		`node -e 'fs.writeFileSync("/m/a","x")'`,
	} {
		if got, ok := shellIRFStringUnreadable(command); ok {
			t.Errorf("%q reported unreadable %q", command, got)
		}
	}
}

// TestShellWriteFStringGate is the gate case: the five -c rows are classified as the shell surface, an unreadable
// f-string is an attempt of its own with the fail-closed reason, and no protected root leaves the gate unchanged.
func TestShellWriteFStringGate(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, row := range shellWriteFStringRows(root) {
		t.Run(row.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": row.command}, cwd, env)
			if got.Surface != "shell" || got.Target != root+"/a" {
				t.Errorf("%+v, want the shell surface and %s", got, root+"/a")
			}
		})
	}
	unreadable := "python3 -c \"f'{open(file='" + root + "/a', mode='w')\""
	want := "(a program the gate cannot read: " + shellWriteFStringUnreadableWhat + ")"
	if got := memoryGateClassify("Bash", map[string]any{"command": unreadable}, cwd, env); got.Surface != "shell" || got.Target != want {
		t.Errorf("unreadable: %+v, want the shell surface and %s", got, want)
	}
	// The unreadable branch sits behind the protected-root guard, so a session with no root makes no attempt. host.Home
	// falls back to the account home, which no test can make fail, so the guard is asserted where it is decided, and a
	// destination outside the root is no attempt through the ordinary path.
	if _, ok := (memoryGateEnv{homeOK: false}).root(); ok {
		t.Error("a gate env with no resolvable home must have no protected root")
	}
	if got := memoryGateClassify("Bash", map[string]any{"command": "echo hi > /w/out.txt"}, cwd, env); got.Surface != "" {
		t.Errorf("a destination outside the root must pass: %+v", got)
	}
	for _, command := range []string{
		"python3 -c \"x='''open(file='" + root + "/a', mode='w')'''\"",
		"python3 -c \"print(f'{{open}}')\"",
	} {
		if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); got.Surface != "" {
			t.Errorf("%q must pass: %+v", command, got)
		}
	}
}

// TestShellWriteFStringGateDeniesAndSpends is the envelope case: denied without a grant, and with one the write passes
// and the grant is consumed, for both a readable and an unreadable f-string program.
func TestShellWriteFStringGateDeniesAndSpends(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, c := range []struct{ name, command string }{
		{"readable field", shellWriteFStringRows(root)[0].command},
		{"unreadable field", "python3 -c \"f'{open(file='" + root + "/a', mode='w')\""},
	} {
		t.Run(c.name, func(t *testing.T) {
			payload := gateBash(t, cwd, c.command)
			reason := gateDeny(t, HandleMemoryWriteGate(payload, env))
			if !strings.Contains(reason, root+"/a") && !strings.Contains(reason, shellWriteFStringUnreadableWhat) {
				t.Errorf("the reason names neither the path nor the reason: %s", reason)
			}
			gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
			if out := HandleMemoryWriteGate(payload, env); out != "" {
				t.Errorf("a grant must let it pass: %q", out)
			}
			if state.ReadState(cwd, gateSession).MemoryWriteGrant {
				t.Error("the grant was not consumed")
			}
		})
	}
}

// TestShellWriteFStringReviewCases pins the three findings of this pull request's reviews.
func TestShellWriteFStringReviewCases(t *testing.T) {
	// A comment inside a multi-line replacement field is not syntax: a quote or a } in it must not end the field
	// early, or the open() call after it goes unread (a security finding).
	commented := "python3 -c 'f\"\"\"{( # \"\nopen(file=\"/m/a\", mode=\"w\"),\n# \"\n\"x\")}\"\"\"'"
	if got := shellWriteDestsTest(commented); !slices.Contains(got, "/m/a") {
		t.Errorf("a comment in a field: %q named %q, want /m/a", commented, got)
	}
	if got, ok := shellIRFStringUnreadable(commented); ok {
		t.Errorf("a comment in a field reported unreadable %q", got)
	}
	// A backslash never escapes a brace in an f-string, so a valid raw f-string is not an unpaired brace (a
	// false-positive finding).
	for _, command := range []string{
		`python3 -c "x=1; print(rf'\{x}')"`,
		`python3 -c "x=1; print(rf'\{{{x}\}}')"`,
	} {
		if got, ok := shellIRFStringUnreadable(command); ok {
			t.Errorf("%q reported unreadable %q", command, got)
		}
	}
	// A Python program one level down a nested shell -c is read too (a P1 finding).
	inner := `python3 -c "f'{x"`
	outer := "bash -c \"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(inner) + "\""
	if got, ok := shellIRFStringUnreadable(outer); !ok || got != shellWriteFStringUnreadableWhat {
		t.Errorf("a nested shell: %q reported %q, %v; want %q, true", outer, got, ok, shellWriteFStringUnreadableWhat)
	}
}
