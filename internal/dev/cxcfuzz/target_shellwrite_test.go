//go:build dev

package cxcfuzz

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
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

// c3 (CRW-908): the shared generator emits the Python literal spelling it can hold, and an f form
// writes the destination's braces doubled so the literal evaluates to the destination itself. A form
// that cannot hold the destination is not emitted at all (c7 d2): a raw form only where the
// destination has no quote to escape and does not end in a backslash, a b form only for an ASCII
// destination. Red first: the doubled-brace form was emitted around the destination, so it evaluated
// to a leading brace, and the plain forms were emitted for destinations holding a quote or a backslash.
func TestShellwritePythonLiteralForms(t *testing.T) {
	for _, c := range []struct {
		word string
		want []string
	}{
		{"/m/a", []string{
			"'/m/a'", "\"/m/a\"", "'''/m/a'''", "\"\"\"/m/a\"\"\"",
			"r'/m/a'", "b'/m/a'", "u'/m/a'", "f'/m/a'", "f'''/m/a'''", "f\"\"\"/m/a\"\"\"",
		}},
		{"{x}.md", []string{
			"'{x}.md'", "\"{x}.md\"", "'''{x}.md'''", "\"\"\"{x}.md\"\"\"",
			"r'{x}.md'", "b'{x}.md'", "u'{x}.md'", "f'{{x}}.md'", "f'''{{x}}.md'''", "f\"\"\"{{x}}.md\"\"\"",
		}},
	} {
		forms := shellWritePythonLiteralForms(c.word)
		if len(forms) != len(c.want) {
			t.Fatalf("%q: %d forms, want %d: %v", c.word, len(forms), len(c.want), forms)
		}
		for i, want := range c.want {
			if forms[i] != want {
				t.Errorf("%q: form %d is %q, want %q", c.word, i, forms[i], want)
			}
		}
	}
}

// c7 d2/d3/d4 (CRW-908 generation 2): every Python program the generator can emit names the destination
// in a literal that evaluates in Python 3 to exactly that destination, over the actual destination pools
// of BOTH targets. The check runs the real interpreter when python3 is on PATH and skips with a message
// otherwise. It walks every program shellWritePrograms returns, not only the literal helper's output, so a
// program assembled wrongly is caught; the earlier version evaluated the helper alone and missed the
// slash-escape and append branches (c7 d4).
func TestShellwriteLiteralFormsEvaluateInPython(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not on PATH, so the emitted literal forms cannot be evaluated here")
	}
	// The harness substitutes the ROOT placeholder in the decoded command after the program is built and
	// before any interpreter sees it, so a ROOT-prefixed form is evaluated with the case root in place of
	// the placeholder - the substitution the campaign makes - against the destination it then names.
	root := t.TempDir()
	for _, dest := range shellWriteDests() {
		want := strings.ReplaceAll(dest, rootPlaceholder, root)
		for _, command := range shellWritePrograms(dest) {
			if command.interpreter != "python3" {
				continue
			}
			t.Run(command.program, func(t *testing.T) {
				program := strings.ReplaceAll(command.program, rootPlaceholder, root)
				got, err := pythonProgramDests(python, program)
				if err != nil {
					t.Fatalf("the program %q for %q does not run in Python: %v", command.program, dest, err)
				}
				if len(got) == 0 {
					t.Fatalf("the program %q for %q names no path", command.program, dest)
				}
				found := false
				for _, named := range got {
					if named == want {
						found = true
					}
				}
				if !found {
					t.Fatalf("the program %q names %q, want it to name %q", command.program, got, want)
				}
			})
		}
	}
}

// shellWriteDests is every destination the two targets' pools can put in a program: the shellwrite
// fragments and the memorygate destinations, built by the generators themselves rather than copied by
// hand, so a destination added to either pool is covered (c7 d4).
func shellWriteDests() []string {
	dests := append([]string{}, shellWritePathFragments()...)
	// The memorygate destinations, taken from the generator's own pool so every alias form, home form and
	// link-chain path is included. The link-dependent destinations are added here because this list feeds
	// the interpreter check, which evaluates a form on its own and never builds the tree the link stands
	// in; the campaign's own pool adds them only in the branch that builds those links.
	dests = append(dests, memoryGateDests()...)
	dests = append(dests, memoryGateLinkDests()...)
	dests = append(dests, memoryGateChainDests()...)
	return dests
}

// pythonProgramDests is the destination literal each Python write in the program names, read back by the
// real interpreter: Python parses the program with its own ast module and evaluates the literal with eval
// in an empty namespace, so the value is exactly what Python gives that literal - an f literal, a raw
// literal and a bytes literal alike. Nothing is executed and no file is touched. The argument that names
// the destination is the one the reader reads: the first for open, the last for Path (the earlier parts
// are a fixed prefix), and the second for os.rename and shutil.copy/copyfile. A program Python cannot
// parse is an error, not an empty list, so a syntax error is reported rather than read as agreement.
func pythonProgramDests(python, program string) ([]string, error) {
	const driver = `import ast, json, sys` + "\n" +
		`tree = ast.parse(sys.argv[1])` + "\n" +
		`seen = []` + "\n" +
		`def value(node):` + "\n" +
		`    v = eval(compile(ast.Expression(node), "<literal>", "eval"), {"__builtins__": {}})` + "\n" +
		`    return v.decode("utf-8", "surrogateescape") if isinstance(v, bytes) else v` + "\n" +
		`def name_of(node):` + "\n" +
		`    if isinstance(node, ast.Name):` + "\n" +
		`        return node.id` + "\n" +
		`    if isinstance(node, ast.Attribute):` + "\n" +
		`        return node.attr` + "\n" +
		`    return ""` + "\n" +
		`for node in ast.walk(tree):` + "\n" +
		`    if not isinstance(node, ast.Call):` + "\n" +
		`        continue` + "\n" +
		`    name = name_of(node.func)` + "\n" +
		`    if name == "open" and node.args:` + "\n" +
		`        seen.append(value(node.args[0]))` + "\n" +
		`    elif name == "Path" and node.args:` + "\n" +
		`        seen.append(value(node.args[-1]))` + "\n" +
		`    elif name in ("rename", "copy", "copyfile") and len(node.args) > 1:` + "\n" +
		`        seen.append(value(node.args[1]))` + "\n" +
		`sys.stdout.buffer.write(json.dumps(seen).encode())`
	out, err := exec.Command(python, "-c", driver, program).Output()
	if err != nil {
		return nil, err
	}
	var dests []string
	if err := json.Unmarshal(out, &dests); err != nil {
		return nil, err
	}
	return dests, nil
}

// pythonLiteralValue is what Python 3 evaluates a literal to, run through the real interpreter. The
// value is written as bytes so a destination that is not valid UTF-8 survives the pipe.
func pythonLiteralValue(python, literal string) (string, error) {
	const script = `import sys` + "\n" +
		`v = eval(sys.argv[1])` + "\n" +
		`sys.stdout.buffer.write(v if isinstance(v, bytes) else v.encode("utf-8", "surrogateescape"))`
	out, err := exec.Command(python, "-c", script, literal).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// c3 (CRW-908): the ROOT placeholder keeps its braces. The harness substitutes that token in the
// decoded command after the program is built, so doubling the placeholder's own braces would leave a
// literal placeholder in the path: the substitution would find nothing to replace, the interpreter
// would name a relative path, and the campaign would agree without ever asking the gate about the
// protected root. Red first: the brace doubling was applied to the whole destination, placeholder
// included.
func TestShellwriteFStringFormsKeepTheRootPlaceholder(t *testing.T) {
	for _, c := range []struct {
		word string
		want []string
	}{
		{rootPlaceholder + "/m/a", []string{"f'" + rootPlaceholder + "/m/a'", "f'''" + rootPlaceholder + "/m/a'''", "f\"\"\"" + rootPlaceholder + "/m/a\"\"\""}},
		{rootPlaceholder + "/codex-home/memories/{x}.md", []string{
			"f'" + rootPlaceholder + "/codex-home/memories/{{x}}.md'",
			"f'''" + rootPlaceholder + "/codex-home/memories/{{x}}.md'''",
			"f\"\"\"" + rootPlaceholder + "/codex-home/memories/{{x}}.md\"\"\"",
		}},
	} {
		var fForms []string
		for _, form := range shellWritePythonLiteralForms(c.word) {
			if strings.HasPrefix(form, "f") {
				fForms = append(fForms, form)
			}
		}
		if len(fForms) != len(c.want) {
			t.Fatalf("%q: %d f forms, want %d: %v", c.word, len(fForms), len(c.want), fForms)
		}
		for i, want := range c.want {
			if fForms[i] != want {
				t.Errorf("%q: f form %d is %q, want %q", c.word, i, fForms[i], want)
			}
		}
		if !strings.Contains(fForms[0], rootPlaceholder) {
			t.Errorf("%q: the f form %q lost the placeholder the harness substitutes", c.word, fForms[0])
		}
	}
}

// c3 (CRW-908): every f form evaluates to the destination, and the generator emits three of them for
// every destination. The plain f form is not emitted for a brace-holding destination: a single brace
// there opens a replacement field, which is not the destination at all. The value is taken from the real
// interpreter (c7 d3): a Go helper modelling Python's literal rules cannot prove them, so this test runs
// python3 and skips with a message where it is not on PATH.
func TestShellwriteFStringFormsEvaluateToTheDestination(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not on PATH, so the emitted f forms cannot be evaluated here")
	}
	root := t.TempDir()
	for _, dest := range []string{"/m/a", rootPlaceholder + "/codex-home/memories/{x}.md", "{x}.md", "/m/a}b", "/m/{a}b", rootPlaceholder + "/codex-home/memories/{a}{b}.md"} {
		// The harness substitutes the placeholder in the decoded command after the program is built and
		// before any interpreter sees it, so the form is evaluated with the case root in place of the
		// placeholder - the substitution the campaign makes.
		want := strings.ReplaceAll(dest, rootPlaceholder, root)
		seen := 0
		for _, form := range shellWritePythonLiteralForms(dest) {
			if !strings.HasPrefix(form, "f") {
				continue
			}
			seen++
			got, err := pythonLiteralValue(python, strings.ReplaceAll(form, rootPlaceholder, root))
			if err != nil {
				t.Fatalf("the form %q is not an f literal Python can evaluate: %v", form, err)
			}
			if got != want {
				t.Fatalf("the form %q evaluates to %q, want %q", form, got, want)
			}
		}
		if seen != 3 {
			t.Fatalf("the destination %q produced %d f forms, want 3", dest, seen)
		}
	}
}

// c2 (CRW-908): the ROOT placeholder is left unescaped, so after the harness substitutes the case root
// the destination of a double-quoted program is byte for byte the chosen path. Red first: the
// placeholder's dollar was escaped before the substitution, so the destination the interpreter was
// handed began with a backslash and the reader named a different path.
func TestShellwriteShellQuoteLeavesTheRootPlaceholderUnescaped(t *testing.T) {
	root := t.TempDir()
	for _, dest := range []string{
		rootPlaceholder + "/m/n.md",
		rootPlaceholder + "/codex-home/memories/a b.md",
		rootPlaceholder + "/codex-home/memories/a'b.md",
	} {
		program := "open('" + dest + "','w').write('x')"
		quoted := shellWriteShellQuote(program)
		if want := "\"" + program + "\""; quoted != want {
			t.Fatalf("the quoted program is %q, want %q", quoted, want)
		}
	}
	// End to end: once the root is substituted, the reader the port ships names the chosen destination.
	dest := rootPlaceholder + "/m/n.md"
	command := "python3 -c " + shellWriteShellQuote("open('"+dest+"','w').write('x')")
	got := hook.ShellWriteDestinations(strings.ReplaceAll(command, rootPlaceholder, root))
	if len(got) != 1 || got[0] != root+"/m/n.md" {
		t.Fatalf("the reader names %v, want [%s]", got, root+"/m/n.md")
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

// c2 (CRW-857): EVERY python/node program the generator emits carries the shell quoting, so the
// class the issue named reaches the campaigns as a command a shell could run rather than as
// truncated or rewritten text. The check is on the command the shell would parse, not on the
// program text alone: the argument after the interpreter flag must be a well-formed quoted word.
func TestShellwriteGeneratorQuotesItsPrograms(t *testing.T) {
	rng := rand.New(rand.NewSource(17))
	for i := 0; i < 5000; i++ {
		program := shellWriteProgram(rng, shellWritePathFragments())
		for _, prefix := range []string{"python3 -c", "py -c", "node -e", "node --eval"} {
			if !strings.HasPrefix(program, prefix) {
				continue
			}
			// The flag and the argument may be separated by a space or not; both are emitted.
			quoted := strings.TrimSpace(strings.TrimPrefix(program, prefix))
			if !strings.HasPrefix(quoted, "'") && !strings.HasPrefix(quoted, "\"") {
				t.Fatalf("the %s program is not shell-quoted: %q", prefix, program)
			}
			if strings.HasPrefix(quoted, "'") && strings.Count(quoted, "'") != 2 {
				t.Fatalf("a single-quoted program holds a stray single quote: %q", program)
			}
		}
	}
}
