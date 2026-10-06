package hook

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-754: the shell write-destination reader reads a Python program passed as a string literal to exec, eval or compile as
// program text, so an open() inside names its path and the memory gate denies it, and a first argument that is not a literal
// makes the program unreadable. Every case is a command string or a program; the oracle's own answer stays first in the
// result and this reading adds after it.

// shellWriteExecWhatWant is the fail-closed what this issue adds (shellWriteExecUnreadableWhat).
const shellWriteExecWhatWant = "a Python program passed to exec, eval or compile"

// shellWriteExecRows are the issue's red-first commands, with the path each must name. Each is a real shell command: the
// python -c program is quoted the way the issue quotes it.
func shellWriteExecRows(mem string) []struct{ name, command string } {
	return []struct{ name, command string }{
		{"exec keyword argument", "python3 -c \"exec('open(file=\\\"" + mem + "/a\\\", mode=\\\"w\\\")')\""},
		{"eval positional", "python3 -c \"eval(\\\"open('" + mem + "/a','w')\\\")\""},
		{"compile three arguments", "python3 -c \"compile('open(file=\\\"" + mem + "/a\\\",mode=\\\"w\\\")','x','exec')\""},
		{"builtins.exec", "python3 -c \"builtins.exec('open(file=\\\"" + mem + "/a\\\", mode=\\\"w\\\")')\""},
		{"__builtins__.eval", "python3 -c \"__builtins__.eval(\\\"open('" + mem + "/a','w')\\\")\""},
	}
}

// TestShellWriteExecReads is the reader case: the issue's commands name their path, and the prefix, quote and argument
// variants do too. On dev each of these named nothing.
func TestShellWriteExecReads(t *testing.T) {
	const mem = "/h/memories"
	for _, row := range shellWriteExecRows(mem) {
		t.Run(row.name, func(t *testing.T) {
			if got := ShellWriteDestinations(row.command); !slices.Contains(got, mem+"/a") {
				t.Errorf("%q named %q, want %q", row.command, got, mem+"/a")
			}
		})
	}
	for _, c := range []struct{ name, program string }{
		{"exec single-quoted", "exec('open(\"/m/a\", \"w\")')"},
		{"exec double-quoted", "exec(\"open('/m/a', 'w')\")"},
		{"eval keyword", "eval('open(file=\"/m/a\", mode=\"w\")')"},
		{"compile", "compile('open(\"/m/a\", \"w\")', 'x', 'exec')"},
		{"builtins.exec", "builtins.exec('open(\"/m/a\", \"w\")')"},
		{"__builtins__.compile", "__builtins__.compile('open(\"/m/a\", \"w\")', 'x', 'exec')"},
		{"r prefix", "exec(r'open(\"/m/a\", \"w\")')"},
		{"b prefix", "exec(b'open(\"/m/a\", \"w\")')"},
		{"u prefix", "exec(u'open(\"/m/a\", \"w\")')"},
		{"U prefix", "exec(U'open(\"/m/a\", \"w\")')"},
		{"R prefix", "exec(R'open(\"/m/a\", \"w\")')"},
		{"B prefix", "exec(B'open(\"/m/a\", \"w\")')"},
		{"triple-quoted", "exec('''open(\"/m/a\", \"w\")''')"},
		{"triple-quoted double", "exec(\"\"\"open('/m/a', 'w')\"\"\")"},
		{"field-free f-string", "exec(f'open(\"/m/a\", \"w\")')"},
		{"escaped quotes", "exec('open(\\\"/m/a\\\", \\\"w\\\")')"},
		{"nested two deep", "exec('exec(\\'open(\"/m/a\", \"w\")\\')')"},
		{"blanks around the argument", "exec(  'open(\"/m/a\", \"w\")'  )"},
		{"trailing comma", "exec('open(\"/m/a\", \"w\")',)"},
		{"second argument is not the program", "exec('open(\"/m/a\", \"w\")', globals())"},
		{"exec after a statement", "x=1; exec('open(\"/m/a\", \"w\")')"},
		{"exec after a newline", "import os\nexec('open(\"/m/a\", \"w\")')"},
		{"builtins.exec after a statement", "print(1)\nbuiltins.exec('open(\"/m/a\", \"w\")')"},
		{"compile after a definition", "f = lambda: 0\n__builtins__.compile('open(\"/m/a\", \"w\")', 'x', 'exec')"},
		{"exec inside a list literal", "x = [exec('open(\"/m/a\", \"w\")')]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := shellVerbOpenWrites(c.program); !slices.Contains(got, "/m/a") {
				t.Errorf("%q named %q, want /m/a", c.program, got)
			}
		})
	}
}

// TestShellWriteExecUnchanged is the invariant case: text that is only data names nothing, another callee is left to the
// dynamic routes this issue records as pending, and a Node program is not Python source.
func TestShellWriteExecUnchanged(t *testing.T) {
	for _, c := range []struct{ name, program string }{
		{"a string holding a call is data", "print('exec(x)')"},
		{"a string holding a write is data", "x = \"open(file='/m/a', mode='w')\""},
		{"a data string holding a whole program", "x = 'exec(\\'open(\"/m/a\", \"w\")\\')'"},
		{"a longer name is no callee", "myexec('open(\"/m/a\", \"w\")')"},
		{"an attribute is no callee", "re.compile('open(\"/m/a\", \"w\")')"},
		{"another module builtins", "a.builtins.exec('open(\"/m/a\", \"w\")')"},
		{"getattr is out of scope", "getattr(builtins, 'exec')('open(\"/m/a\", \"w\")')"},
		{"an f-string with a field is no literal", "exec(f'{x}')"},
		{"exec takes no keyword argument", "exec(code='open(\"/m/a\", \"w\")')"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := shellVerbOpenWrites(c.program); len(got) != 0 {
				t.Errorf("%q named %q", c.program, got)
			}
		})
	}
	// A Node program is not Python source, so an exec call there is not a Python exec.
	if got := shellVerbOpenWritesIn("exec('open(\"/m/a\",\"w\")')", false); len(got) != 0 {
		t.Errorf("a Node program was walked as Python: %q", got)
	}
	// A comment is cut before the walk, so a program named in one is no program.
	if got := shellVerbOpenWrites("exec(x) # exec('open(\"/m/a\",\"w\")')"); len(got) != 0 {
		t.Errorf("a program in a comment named %q", got)
	}
}

// TestShellWriteExecUnreadable is the fail-closed case: a first argument that is not a literal reports the reason, the
// depth guard reports it too, and a write the walk did reach is still named beside it. The reader is lexical, not a Python
// evaluator, so an exec call in dead code is read the same way (a cost of the fail-closed branch, not an oversight).
func TestShellWriteExecUnreadable(t *testing.T) {
	for _, c := range []struct{ name, program string }{
		{"a name", "exec(src)"},
		{"a call", "exec(open('x.py').read())"},
		{"a concatenation", "exec('a' 'b')"},
		{"a joined name", "exec('open(\"' + p + '\", \"w\")')"},
		{"an f-string field", "exec(f'{src}')"},
		{"no argument", "exec()"},
		{"a name in dead code", "exec(src) if 0 else None"},
		{"a name beside a write", "exec(src); open(\"/m/a\", \"w\")"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := shellWriteFStringUnreadableProgram(c.program)
			if !ok || got != shellWriteExecWhatWant {
				t.Errorf("%q: got %q, %v; want %q, true", c.program, got, ok, shellWriteExecWhatWant)
			}
		})
	}
	// The same program reached through a shell command reports the same reason.
	if got, ok := shellWriteFStringUnreadable("python3 -c 'exec(src)'"); !ok || got != shellWriteExecWhatWant {
		t.Errorf("a shell command: got %q, %v; want %q, true", got, ok, shellWriteExecWhatWant)
	}
	// The depth limit is the scan guard: a walk already past it reports the reason and reads nothing. (A source program
	// nested that deep cannot be written compactly, because every level escapes the quotes of the level below it, so the
	// guard is asserted where it is decided.)
	if dests, what := shellWriteExecScan([]rune("pass"), true, shellWriteExecMaxDepth+1); what != shellWriteExecUnreadableWhat || len(dests) != 0 {
		t.Errorf("past the depth limit: %q, %v; want %q, []", dests, what, shellWriteExecUnreadableWhat)
	}
	// A program the walk cannot read still leaves the destinations it did read.
	if got := shellVerbOpenWrites("exec(src); open(\"/m/a\", \"w\")"); !slices.Contains(got, "/m/a") {
		t.Errorf("a write beside an unreadable program named %q, want /m/a", got)
	}
	// A well-formed program reports nothing.
	for _, program := range []string{
		"exec('open(\"/m/a\", \"w\")')",
		"print('exec(x)')",
		"open('/m/a', 'w')",
	} {
		if got, ok := shellWriteFStringUnreadableProgram(program); ok {
			t.Errorf("%q reported unreadable %q", program, got)
		}
	}
}

// TestShellWriteExecGate is the gate case: the issue's commands are denied as the shell surface with the path as the target,
// an unreadable program is an attempt of its own with the fail-closed reason, and no protected root leaves the gate unchanged.
func TestShellWriteExecGate(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, row := range shellWriteExecRows(root) {
		t.Run(row.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": row.command}, cwd, env)
			if got.Surface != "shell" || got.Target != root+"/a" {
				t.Errorf("%+v, want the shell surface and %s", got, root+"/a")
			}
		})
	}
	unreadable := "python3 -c 'exec(src)'"
	want := "(a program the gate cannot read: " + shellWriteExecWhatWant + ")"
	if got := memoryGateClassify("Bash", map[string]any{"command": unreadable}, cwd, env); got.Surface != "shell" || got.Target != want {
		t.Errorf("unreadable: %+v, want the shell surface and %s", got, want)
	}
	// The unreadable branch sits behind the protected-root guard, so a session with no root makes no attempt.
	if _, ok := (memoryGateEnv{homeOK: false}).root(); ok {
		t.Error("a gate env with no resolvable home must have no protected root")
	}
	// A command that names no protected path is no attempt, readable or not.
	for _, command := range []string{
		"python3 -c \"print('exec(x)')\"",
		"python3 -c \"x='''open(file='" + root + "/a', mode='w')'''\"",
	} {
		if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); got.Surface != "" {
			t.Errorf("%q must pass: %+v", command, got)
		}
	}
	if got := memoryGateClassify("Bash", map[string]any{"command": "echo hi > /w/out.txt"}, cwd, env); got.Surface != "" {
		t.Errorf("a destination outside the root must pass: %+v", got)
	}
}

// TestShellWriteExecGateDeniesAndSpends is the envelope case: denied without a grant, and with one the write passes and the
// grant is consumed, for both a readable and an unreadable program.
func TestShellWriteExecGateDeniesAndSpends(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, c := range []struct{ name, command string }{
		{"readable program", shellWriteExecRows(root)[0].command},
		{"unreadable program", "python3 -c 'exec(src)'"},
	} {
		t.Run(c.name, func(t *testing.T) {
			payload := gateBash(t, cwd, c.command)
			reason := gateDeny(t, HandleMemoryWriteGate(payload, env))
			if !strings.Contains(reason, root+"/a") && !strings.Contains(reason, shellWriteExecWhatWant) {
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

// TestShellWriteExecReviewCases pins the two findings of this pull request's Devin review, both over-blocking in this
// issue's own new code: a definition of one of the three names is no call, and the shell-unescaped reading of a program this
// reader can read must not turn it into an unreadable one.
func TestShellWriteExecReviewCases(t *testing.T) {
	// A def or async def header binds a name; its parameter list runs nothing.
	for _, c := range []struct{ name, program string }{
		{"def compile", "def compile(source, filename, mode): return source"},
		{"def exec", "def exec(x): pass"},
		{"async def eval", "async def eval(x): pass"},
		{"def on the next line", "x = 1\ndef compile(source, filename, mode):\n    return source"},
		{"def with a blank before the paren", "def compile (source, filename, mode): return source"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, ok := shellWriteFStringUnreadableProgram(c.program); ok {
				t.Errorf("%q reported unreadable %q", c.program, got)
			}
		})
	}
	// A real call in the same program is still read.
	if got := shellVerbOpenWrites("def compile(x): return x\ncompile('open(\"/m/a\", \"w\")', 'f', 'exec')"); !slices.Contains(got, "/m/a") {
		t.Errorf("a call beside a definition named %q, want /m/a", got)
	}
	// The unescaped reading of a readable program must not report a reason.
	for _, command := range []string{
		"python3 -c 'exec(\"x = \\\"a\\\"\")'",
		"python3 -c 'exec(\"print(\\\"hi\\\")\")'",
	} {
		if got, ok := shellWriteFStringUnreadable(command); ok {
			t.Errorf("%q reported unreadable %q", command, got)
		}
	}
	// An exec whose first argument really is no literal is still unreadable, whatever the escaping.
	for _, command := range []string{
		"python3 -c 'exec(src)'",
		"python3 -c \"exec(src)\"",
		"python3 -c \"exec(open('x.py').read())\"",
	} {
		if got, ok := shellWriteFStringUnreadable(command); !ok || got != shellWriteExecWhatWant {
			t.Errorf("%q: got %q, %v; want %q, true", command, got, ok, shellWriteExecWhatWant)
		}
	}
}

// TestShellWriteExecCodexCases pins the four findings of the Codex review on this pull request: a parenthesized callee, the
// legal spacing around an attribute dot, a bytes program with a source-encoding declaration, and a lone carriage return.
func TestShellWriteExecCodexCases(t *testing.T) {
	// A parenthesized callee is still the built-in; a call's result or an index is not.
	for _, c := range []struct{ name, program string }{
		{"parenthesized builtins.exec", "import builtins; (builtins.exec)('open(\"/m/a\", \"w\")')"},
		{"doubly parenthesized", "import builtins; ((builtins.exec))('open(\"/m/a\", \"w\")')"},
		{"parenthesized exec", "(exec)('open(\"/m/a\", \"w\")')"},
		{"parenthesized builtins.eval", "(builtins.eval)('open(\"/m/a\", \"w\")')"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := shellVerbOpenWrites(c.program); !slices.Contains(got, "/m/a") {
				t.Errorf("%q named %q, want /m/a", c.program, got)
			}
		})
	}
	for _, c := range []struct{ name, program string }{
		{"a call result is no callee", "f()('open(\"/m/a\", \"w\")')"},
		{"an index is no callee", "a[0]('open(\"/m/a\", \"w\")')"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := shellVerbOpenWrites(c.program); len(got) != 0 {
				t.Errorf("%q named %q", c.program, got)
			}
		})
	}
	// Legal spacing around the attribute dot keeps it an attribute, so it is no built-in call.
	for _, program := range []string{"runner . exec(src)", "runner.exec(src)", "runner\t.\texec(src)"} {
		if got, ok := shellWriteFStringUnreadableProgram(program); ok {
			t.Errorf("%q reported unreadable %q; an attribute call is out of scope, not unreadable", program, got)
		}
	}
	// A bytes program declaring a source encoding is decoded under a codec this reader does not model, so it fails closed.
	bytes := "exec(b'# coding: unicode_escape\\nopen(\"/m/a\",\"w\")\\n')"
	if got, ok := shellWriteFStringUnreadableProgram(bytes); !ok || got != shellWriteExecWhatWant {
		t.Errorf("a bytes program with a coding declaration: got %q, %v; want %q, true", got, ok, shellWriteExecWhatWant)
	}
	// A bytes literal with no declaration still reads as the issue requires.
	if got := shellVerbOpenWrites("exec(b'open(\"/m/a\", \"w\")')"); !slices.Contains(got, "/m/a") {
		t.Errorf("a bytes literal with no declaration named %q, want /m/a", got)
	}
	// A lone carriage return is a line break to Python, so a comment ends there.
	if got := shellVerbOpenWrites("exec('# ignored\ropen(\"/m/a\",\"w\")')"); !slices.Contains(got, "/m/a") {
		t.Errorf("a lone CR comment named %q, want /m/a", got)
	}
	if got, ok := shellWriteFStringUnreadable("python3 -c \"exec('# ignored\ropen(\"/m/a\",mode=\"w\")\""); ok {
		t.Errorf("a lone CR comment reported unreadable %q", got)
	}
}

// TestShellWriteExecDepthStaysBounded is the bound case: a program nested far past the limit is refused at the limit
// instead of walked to the bottom. The chain grows by a constant amount per level (each level writes the quote and
// backslash characters of the level below as hex escapes), so a 200-level input is a few kilobytes and the reader
// must not spend work proportional to 200 levels of recursion.
func TestShellWriteExecDepthStaysBounded(t *testing.T) {
	escape := func(s string) string {
		s = strings.ReplaceAll(s, "\\", "\\x5c")
		return strings.ReplaceAll(s, "\"", "\\x22")
	}
	prog := "open(\"/m/a\",\"w\")"
	for i := 0; i < 200; i++ {
		prog = "exec(\"" + escape(prog) + "\")"
	}
	command := "python3 -c '" + prog + "'"
	if got, ok := shellWriteFStringUnreadable(command); !ok || got != shellWriteExecWhatWant {
		t.Errorf("a 200-level program: got %q, %v; want %q, true", got, ok, shellWriteExecWhatWant)
	}
	if got := ShellWriteDestinations(command); slices.Contains(got, "/m/a") {
		t.Errorf("a 200-level program named %q; the walk must stop at the depth limit", got)
	}
	// The work stays bounded as well as the answer: the walk stops at the limit, so 200 levels cost about as much as 32.
	// A reader that expanded every level would take far longer than this budget, which is generous enough not to be flaky
	// (the same wall-clock-budget convention the Stop-hook package uses).
	start := time.Now()
	ShellWriteDestinations(command)
	shellWriteFStringUnreadable(command)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("200 levels took %v; the walk is not bounded by the depth limit", elapsed)
	}
}
