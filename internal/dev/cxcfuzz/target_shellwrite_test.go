//go:build dev

package cxcfuzz

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// The target is registered under its name, with a Node shim and a comparator.
func TestShellwriteTargetIsRegistered(t *testing.T) {
	target, ok := Lookup("shellwrite")
	if !ok {
		t.Fatalf("shellwrite is not registered; registered: %v", Names())
	}
	if target.Oracle.Command != "node" || !strings.HasSuffix(target.Oracle.Shim, filepath.Join("testdata", "shellwrite", "shim.mjs")) {
		t.Fatalf("oracle %+v", target.Oracle)
	}
	if _, err := os.Stat(target.Oracle.Shim); err != nil {
		t.Fatalf("the shim is missing: %v", err)
	}
}

// Before the target is registered the command ends unknown target: this is the red-first case the
// issue names. The name is looked up in the table rather than through the command, so the test
// still holds after the entry exists (Lookup of a name that is not there).
func TestShellwriteUnknownTargetEndsTwo(t *testing.T) {
	if _, ok := Lookup("shellwrite-not-a-target"); ok {
		t.Fatal("a made-up target name is registered")
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"shellwrite-not-a-target", "--cases", "1"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "unknown target") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

// The generator produces a command object the Go side and the shim both read, over the whole
// range of sizes, and it never emits a destination outside the placeholder or the fixed pool.
func TestShellwriteGeneratorShape(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		input := shellWriteGenerate(rng, rng.Int())
		value, found := field(input, "command")
		command, ok := value.(string)
		if !found || !ok {
			t.Fatalf("input %s has no command string", canonical(input))
		}
		if strings.ContainsAny(command, "\x00") {
			t.Fatalf("command holds NUL: %q", command)
		}
		// The input must survive the JSON round trip the campaign makes, or the Go side and the
		// shim would read different commands.
		decoded, err := decode(canonical(input))
		if err != nil {
			t.Fatalf("the input is not JSON: %v", err)
		}
		again, found := field(decoded, "command")
		if !found || again != command {
			t.Fatalf("the command changed in the round trip: %q -> %v", command, again)
		}
	}
}

// The comparator is a destination set: order and duplicates do not matter, an oracle destination
// the port lacks is a miss, a port-only one is extra, and a difference on both sides is differ.
func TestShellwriteCompareClassifiesSets(t *testing.T) {
	for _, c := range []struct {
		name       string
		goSide     any
		oracleSide any
		want       Kind
	}{
		{"same order differs", []any{"b", "a"}, []any{"a", "b"}, Same},
		{"duplicates ignored", []any{"a", "a"}, []any{"a"}, Same},
		{"both empty", []any{}, []any{}, Same},
		{"oracle only is a miss", []any{}, []any{"/m/a"}, Miss},
		{"go only is extra", []any{"/m/a"}, []any{}, Extra},
		{"both ways is differ", []any{"/m/a"}, []any{"/m/b"}, Differ},
		{"a worker failure is not an empty set", pyjson.Object{{Key: "error", Value: "boom"}}, []any{}, Differ},
		{"a non-array answer is not an empty set", "boom", []any{}, Differ},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := shellWriteCompare(c.goSide, c.oracleSide).Kind; got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// The Go side answers hook.ShellWriteDestinations of the command, with the case's own root in the
// place of the input's ${ROOT}, and a non-nil array even when nothing is named.
func TestShellwriteGoAnswersTheReader(t *testing.T) {
	root := t.TempDir()
	env := RootEnv(root)
	input := pyjson.Object{{Key: "command", Value: "echo hi > " + rootPlaceholder + "/m/n.md"}}
	got, err := shellWriteGo(input, env)
	if err != nil {
		t.Fatal(err)
	}
	if canonical(got) != `["`+root+`/m/n.md"]` {
		t.Fatalf("got %s", canonical(got))
	}
	empty, err := shellWriteGo(pyjson.Object{{Key: "command", Value: "true"}}, env)
	if err != nil || canonical(empty) != "[]" {
		t.Fatalf("empty: %s, %v", canonical(empty), err)
	}
}

// The same input names each side's own root: two different roots substitute the placeholder
// independently, so the harness can compare one input against two trees. Pinned here because the
// whole target depends on it and a campaign only sees it indirectly.
func TestShellwriteRootPlaceholderNamesEachSidesRoot(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	input := pyjson.Object{{Key: "command", Value: "echo hi > " + rootPlaceholder + "/m/n.md; tee " + rootPlaceholder + "/m/t"}}
	for _, root := range []string{one, two} {
		got, err := shellWriteGo(input, RootEnv(root))
		if err != nil {
			t.Fatal(err)
		}
		text := canonical(got)
		if !strings.Contains(text, root+"/m/n.md") || !strings.Contains(text, root+"/m/t") {
			t.Fatalf("root %s: %s", root, text)
		}
		if strings.Contains(text, rootPlaceholder) {
			t.Fatalf("the placeholder reached the answer: %s", text)
		}
	}
}

// The seed cases replay through the Go side with no Node and no worker, and each one still holds
// its tag: an identical case agrees with the oracle's recorded answer, an intentionally-changed
// case agrees with the Go answer its record names, and an open case still prints the Go output it
// was pinned with.
func TestShellwriteSeedCasesReplay(t *testing.T) {
	target, ok := Lookup("shellwrite")
	if !ok {
		t.Fatal("shellwrite is not registered")
	}
	cases, err := LoadCases(filepath.Join("testdata", "shellwrite"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 8 {
		t.Fatalf("%d seed cases, want at least 8", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			if problem := CheckCase(target, c); problem != "" {
				t.Fatal(problem)
			}
		})
	}
}

// c2 (CRW-857): the shared generator emits every Python literal spelling the issue required, with
// the f form sometimes holding a doubled brace.
func TestShellwritePythonLiteralForms(t *testing.T) {
	forms := shellWritePythonLiteralForms("/m/a")
	for _, want := range []string{
		"'/m/a'", "\"/m/a\"", "'''/m/a'''", "\"\"\"/m/a\"\"\"",
		"r'/m/a'", "b'/m/a'", "u'/m/a'", "f'/m/a'", "f'{{/m/a}}'", "f\"\"\"{{/m/a}}\"\"\"",
	} {
		found := false
		for _, form := range forms {
			if form == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the literal forms do not include %q: %v", want, forms)
		}
	}
}

// c2 (CRW-857): each single-character escape Python accepts for a slash is available for a
// destination, and a destination with no slash yields none.
func TestShellwritePythonEscapeForms(t *testing.T) {
	forms := shellWritePythonEscapeForms("/m/a")
	for _, want := range []string{`\x2f` + "m/a", `\u002f` + "m/a", `\057` + "m/a", `\N{SOLIDUS}` + "m/a"} {
		found := false
		for _, form := range forms {
			if form == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the escape forms do not include %q: %v", want, forms)
		}
	}
	if got := shellWritePythonEscapeForms("rel"); got != nil {
		t.Errorf("a destination without a slash produced %v", got)
	}
}

// c2 (CRW-857): the program builder emits the new write forms. The seed is fixed, so the set it
// produces is the same on every run.
func TestShellwriteGeneratorEmitsTheRequiredPrograms(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	seen := map[string]bool{}
	for i := 0; i < 20000; i++ {
		seen[shellWriteProgram(rng, shellWritePathFragments())] = true
	}
	for _, want := range []struct{ name, has string }{
		{"os.rename", "os.rename("},
		{"shutil.copy", "shutil.copy("},
		{"shutil.copyfile", "shutil.copyfile("},
		{"triple single quote", "'''"},
		{"triple double quote", "\"\"\""},
		{"f prefix", "f'"},
		{"r prefix", "r'"},
		{"b prefix", "b'"},
		{"u prefix", "u'"},
		{"x2f escape", `\x2f`},
		{"u002f escape", `\u002f`},
		{"octal escape", `\057`},
		{"named escape", `\N{SOLIDUS}`},
	} {
		found := false
		for program := range seen {
			if strings.Contains(program, want.has) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the generator never emitted %s (%q)", want.name, want.has)
		}
	}
}

// c2 (CRW-857): the nested builder emits a subshell, a brace group, a command substitution and a
// backtick substitution around a real write.
func TestShellwriteGeneratorEmitsTheRequiredGroups(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	seen := map[string]bool{}
	for i := 0; i < 400; i++ {
		seen[shellWriteNested(rng, []string{"/m/a"})] = true
	}
	for _, want := range []struct{ name, has string }{
		{"subshell", "(echo hi > /m/a)"},
		{"brace group", "{ echo hi > /m/a; }"},
		{"command substitution", "x=$(echo hi > /m/a)"},
		{"backticks", "y=`echo hi > /m/a`"},
	} {
		if !seen[want.has] {
			t.Errorf("the generator never emitted the %s %q", want.name, want.has)
		}
	}
}

// c2 (CRW-857): a program the generator emits must be one a real shell hands to the interpreter
// unchanged. A single-quoted argument holding an unescaped single quote would end at that quote,
// so the interpreter would receive a truncated program and the form would never reach the write
// the issue asked for. The quoting is checked structurally, with no shell run: a single-quoted
// form holds no single quote at all, and a double-quoted one escapes every character the shell
// still reads inside double quotes.
func TestShellwriteShellQuoteSurvivesTheDestination(t *testing.T) {
	for _, dest := range []string{"/m/a", "/m/a'b", `/m/a"b`, "/m/a$b", "/m/a`b", `/m/a\b`, "/m/a b", "rel"} {
		for _, literal := range shellWritePythonLiteralForms(dest) {
			program := "import os; os.rename(\"/w/old.md\", " + literal + ")"
			quoted := shellWriteShellQuote(program)
			switch {
			case strings.HasPrefix(quoted, "'"):
				if !strings.HasSuffix(quoted, "'") {
					t.Fatalf("the single-quoted form is unterminated: %q", quoted)
				}
				if inner := quoted[1 : len(quoted)-1]; strings.Contains(inner, "'") {
					t.Fatalf("a single quote inside a single-quoted argument ends it early: %q", quoted)
				}
			case strings.HasPrefix(quoted, "\""):
				if !strings.HasSuffix(quoted, "\"") {
					t.Fatalf("the double-quoted form is unterminated: %q", quoted)
				}
				inner := quoted[1 : len(quoted)-1]
				for i := 0; i < len(inner); i++ {
					if inner[i] != '\\' {
						continue
					}
					if i+1 >= len(inner) {
						t.Fatalf("a trailing backslash escapes the closing quote: %q", quoted)
					}
					i++
				}
			default:
				t.Fatalf("the program is not quoted at all: %q", quoted)
			}
		}
	}
}

// c2 (CRW-857): the generated programs carry the shell quoting, so the class the issue named
// reaches the campaigns as a command a shell could run rather than as truncated text.
func TestShellwriteGeneratorQuotesItsPrograms(t *testing.T) {
	rng := rand.New(rand.NewSource(17))
	for i := 0; i < 5000; i++ {
		program := shellWriteProgram(rng, shellWritePathFragments())
		prefix := "python3 -c "
		if !strings.HasPrefix(program, prefix) {
			continue
		}
		quoted := program[len(prefix):]
		if !strings.HasPrefix(quoted, "'") && !strings.HasPrefix(quoted, "\"") {
			t.Fatalf("a python3 -c program is not shell-quoted: %q", program)
		}
		if strings.HasPrefix(quoted, "'") && strings.Count(quoted, "'") != 2 {
			t.Fatalf("a single-quoted program holds a stray single quote: %q", program)
		}
	}
}
