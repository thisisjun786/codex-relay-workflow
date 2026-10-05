package hook

import (
	"runtime"
	"slices"
	"strings"
	"testing"
)

// CRW-583: the shell write-destination reader names the real path of a multi-part Path call and of a string literal written with
// escapes. Every case is a command string; the oracle's own answer stays first in the result and the reading adds after it.

// shellWriteEscapeOracle is the oracle's reading of a one-segment command, which ShellWriteDestinations must begin with.
func shellWriteEscapeOracle(command string) []string { return shellVerbOracle(shellTokenize(command)) }

type shellWriteEscapeCase struct {
	command    string
	has, lacks []string
	same       bool // the reading adds nothing to the oracle's answer
}

func shellWriteEscapeRun(t *testing.T, cases []shellWriteEscapeCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.command, func(t *testing.T) {
			got, oracle := ShellWriteDestinations(c.command), shellWriteEscapeOracle(c.command)
			if len(got) < len(oracle) || !slices.Equal(got[:len(oracle)], oracle) {
				t.Fatalf("the oracle's answer %q is not first in %q", oracle, got)
			}
			if c.same && !slices.Equal(got, oracle) {
				t.Errorf("got %q, want the oracle's answer %q alone", got, oracle)
			}
			for _, want := range c.has {
				if !slices.Contains(got, want) {
					t.Errorf("%q lacks %q", got, want)
				}
			}
			for _, not := range c.lacks {
				if slices.Contains(got, not) {
					t.Errorf("%q holds %q", got, not)
				}
			}
		})
	}
}

// A Path call names posixpath.join of its literal parts: an absolute part discards the ones before it, nothing else is normalized,
// and a part that is not a literal makes the call name nothing.
func TestShellWriteEscapePathJoin(t *testing.T) {
	path := func(call string) string { return "python3 -c \"from pathlib import Path; " + call + "\"" }
	shellWriteEscapeRun(t, []shellWriteEscapeCase{
		{command: path("Path('/tmp/b', '/m/a').write_text('x')"), has: []string{"/m/a"}, lacks: []string{"/tmp/b"}},
		{command: path("Path('/m', 'a').write_text('x')"), has: []string{"/m/a"}, lacks: []string{"/m"}},
		{command: path("Path('/m', 'a').write_bytes(b'x')"), has: []string{"/m/a"}},
		{command: path("Path('/m/a', '/tmp/b',).write_text('x')"), has: []string{"/tmp/b"}, lacks: []string{"/m/a"}},
		{command: path("Path('/w', '/m', 'a').write_text('x')"), has: []string{"/m/a"}, lacks: []string{"/w", "/w/m/a"}},
		{command: path("Path('/m', 'a', 'b').write_text('x')"), has: []string{"/m/a/b"}},
		{command: path("Path('/m/', 'a').write_text('x')"), has: []string{"/m/a"}},
		{command: path("Path('/m', 'a/', 'b').write_text('x')"), has: []string{"/m/a/b"}},
		{command: path("Path('/m', '').write_text('x')"), has: []string{"/m/"}},
		{command: path("Path('', 'a').write_text('x')"), has: []string{"a"}},
		{command: path("Path('\\x2fm', 'a').write_text('x')"), has: []string{"/m/a"}},
		{command: path("Path('/m', name).write_text('x')"), same: true},
		{command: path("Path(base, '/m/a').write_text('x')"), same: true},
		{command: path("Path('/m', *parts).write_text('x')"), same: true},
		{command: path("Path('/m', 'a').read_text()"), same: true},
		{command: path("Path('/m/n.md').write_text('x')"), has: []string{"/m/n.md"}, same: true},
	})
}

// A Python string literal is read as Python reads it: its escapes are decoded unless it is raw, and the decoded path follows the
// oracle's raw one.
func TestShellWriteEscapePythonPaths(t *testing.T) {
	shellWriteEscapeRun(t, []shellWriteEscapeCase{
		{command: "python3 -c 'open(\"\\x2fm/a\",\"w\")'", has: []string{"\\x2fm/a", "/m/a"}},
		{command: "python3 -c 'open(file=\"\\x2fm/a\",mode=\"w\")'", has: []string{"/m/a"}},
		{command: "python3 -c 'open(\"\\u002fm/a\",\"w\")'", has: []string{"/m/a"}},
		{command: "python3 -c 'open(\"/h/\\udcc3\\udca9/m\",\"w\")'", has: []string{"/h/\u00e9/m"}},
		{command: "python3 -c 'open(\"\\057m/a\",\"w\")'", has: []string{"/m/a"}},
		{command: "python3 -c 'open(b\"\\x2fm/a\",\"w\")'", has: []string{"/m/a"}},
		{command: "python3 -c 'open(\"/m/a\",\"\\x77\")'", has: []string{"/m/a"}},
		{command: "python3 -c 'open(\"/m/a\",\"\\x72\")'", lacks: []string{"/m/a"}},
		{command: "python3 -c 'open(r\"\\x2fm/a\",\"w\")'", has: []string{"\\x2fm/a"}, lacks: []string{"/m/a"}, same: true},
		{command: "python3 -c 'open(\"\\N{SOLIDUS}m/a\",\"w\")'", lacks: []string{"/m/a"}, same: true},
		{command: "python3 -c 'open(\"/m/a\\0\",\"w\")'", lacks: []string{"/m/a\x00"}, same: true},
		{command: "python3 -c 'open(\"\\x2m/a\",\"w\")'", same: true},
		{command: "python3 -c 'open(\"/m/\\d\",\"w\")'", has: []string{"/m/\\d"}, same: true},
		{command: "python3 -c 'from pathlib import Path; Path(\"\\x2fm/a\").write_text(\"x\")'", has: []string{"/m/a"}},
		{command: "python3 -c 'open(\"/m/\\\na\",\"w\")'", has: []string{"/m/a"}},
	})
}

// shellVerbLiteral is the value of a Python string literal; a literal whose escape Python rejects, or whose value holds NUL, is
// none.
func TestShellWriteEscapePythonLiteral(t *testing.T) {
	for _, c := range []struct {
		literal, want string
		ok            bool
	}{
		{"'\\x2fm/a'", "/m/a", true}, {"'\\u002fm/a'", "/m/a", true}, {"'\\U0000002fm/a'", "/m/a", true}, {"'\\057m/a'", "/m/a", true},
		{"'\\57m'", "/m", true}, {"'\\1010'", "A0", true}, {"'\\777'", "\u01ff", true}, {"b'\\777'", "\xff", true}, {"b'\\x2fm/a'", "/m/a", true},
		{"b'\\xc3\\xa9'", "\u00e9", true}, {"u'\\u00e9'", "\u00e9", true}, {"f'\\x2fm'", "/m", true},
		{"b'\\u002f'", "\\u002f", true}, {"b'\\U0000002f'", "\\U0000002f", true}, {"b'\\N{DASH}'", "\\N{DASH}", true},
		{"r'\\x2fm/a'", "\\x2fm/a", true}, {"rb'\\x2f'", "\\x2f", true}, {"bR'\\x2f'", "\\x2f", true}, {"R'\\n'", "\\n", true},
		{"'\\d/a'", "\\d/a", true}, {"'\\8'", "\\8", true}, {"'a\\\\b'", "a\\b", true},
		{"'a\\'b'", "a'b", true}, {"\"a\\\"b\"", "a\"b", true}, {"'a\\\"b'", "a\"b", true}, {"\"a\\'b\"", "a'b", true},
		{"'\\a\\b\\f\\n\\r\\t\\v'", "\a\b\f\n\r\t\v", true},
		{"'a\\\nb'", "ab", true}, {"'a\\\r\nb'", "ab", true}, {"'a\\\rb'", "ab", true}, {"r'a\\\nb'", "a\\\nb", true},
		{"'\\x2'", "", false}, {"'\\x'", "", false}, {"'\\xzz'", "", false}, {"'\\u12'", "", false}, {"'\\U0000002'", "", false},
		{"'\\U00110000'", "", false}, {"'\\N{SOLIDUS}'", "", false}, {"'\\N'", "", false}, {"b'\\x2'", "", false},
		{"'\\0'", "", false}, {"'\\x00'", "", false}, {"'\\u0000'", "", false}, {"'a\\000'", "", false}, {"b'\\400'", "", false},
		{"'a' 'b'", "", false}, {"'abc", "", false}, {"name", "", false},
		{"'/h/\\udcc3\\udca9/m'", "/h/\u00e9/m", true}, {"'\\U0000dcc3'", "\xc3", true}, {"'\\udc80'", "\x80", true}, {"b'\\udcc3'", "\\udcc3", true},
		{"'\\ud83d'", "", false}, {"'\\ud83d\\ude00'", "", false}, {"'\\udc7f'", "", false}, {"'\\udd00'", "", false}, {"'\\ud800'", "", false},
	} {
		got, ok := shellVerbLiteral([]rune(c.literal))
		if got != c.want || ok != c.ok {
			t.Errorf("%q: got %q, %v; want %q, %v", c.literal, got, ok, c.want, c.ok)
		}
	}
}

// The JS write calls and template literals name the JS-decoded path after the oracle's raw text, when the two differ.
func TestShellWriteEscapeJSPaths(t *testing.T) {
	js := func(call string) string { return "node -e 'const fs = require(\"fs\"); " + call + "'" }
	shellWriteEscapeRun(t, []shellWriteEscapeCase{
		{command: js("fs.writeFileSync(\"\\x2fm/a\",\"x\")"), has: []string{"\\x2fm/a", "/m/a"}},
		{command: js("fs.writeFileSync(\"\\u{2f}m/a\",\"x\")"), has: []string{"/m/a"}},
		{command: js("fs.writeFileSync(\"/h/\\u{d83d}\\u{de00}/m\",\"x\")"), has: []string{"/h/\U0001f600/m"}},
		{command: js("fs.writeFileSync(\"\\u002fm/a\",\"x\")"), has: []string{"/m/a"}},
		{command: js("fs.writeFileSync(\"\\57m/a\",\"x\")"), has: []string{"/m/a"}},
		{command: js("fs.writeFileSync(`\\x2fm/a`,\"x\")"), has: []string{"/m/a"}},
		{command: js("fs.appendFileSync(\"\\x2fm/a\",\"x\")"), has: []string{"/m/a"}},
		{command: js("fs.writeFile(\"\\x2fm/a\",\"x\",cb)"), has: []string{"/m/a"}},
		{command: js("fs.createWriteStream(\"\\x2fm/a\")"), has: []string{"/m/a"}},
		{command: js("fs.writeFileSync(\"/m/\\a\",\"x\")"), has: []string{"/m/a"}},
		{command: js("fs.writeFileSync(`/m/\\${x}`,\"x\")"), has: []string{"/m/${x}"}},
		{command: js("fs.writeFileSync(`\\x2fm/${x}`,\"x\")"), has: []string{"\\x2fm/${x}"}, lacks: []string{"/m/${x}"}},
		{command: js("fs.writeFileSync(`/m/\\\\${x}`,\"x\")"), has: []string{"/m/\\\\${x}"}},
		{command: js("fs.writeFileSync(\"\\x2m/a\",\"x\")"), same: true},
		{command: js("fs.writeFileSync(\"/m/a\",\"x\")"), has: []string{"/m/a"}, same: true},
		{command: js("fs.writeFileSync(\"\\x2fm/\\\na\",\"x\")"), has: []string{"/m/a"}},
		{command: js("fs.writeFileSync(\"\\x2fm/a\\\"b\",\"x\")"), has: []string{"/m/a\"b"}},
		{command: js("fs.writeFileSync(`\\x2fm/a\nb`,\"x\")"), has: []string{"/m/a\nb"}},
		{command: js("fs.writeFileSync(`\\x2fm/a\r\nb`,\"x\")"), has: []string{"/m/a\nb"}},
		{command: js("fs.writeFileSync(`\\x2fm/a\rb`,\"x\")"), has: []string{"/m/a\nb"}},
		{command: js("fs.writeFileSync(`\\x2fm/a\\rb`,\"x\")"), has: []string{"/m/a\rb"}},
		{command: js("fs.writeFileSync(\"\\x2fm/a\nb\",\"x\")"), lacks: []string{"/m/a\nb"}},
	})
}

// The unit reading: the oracle's own patterns answer the raw text alone, and the hardened reading adds the decoded path after it.
func TestShellWriteEscapeScriptWrites(t *testing.T) {
	script := `fs.writeFileSync("\x2fm/a","x")`
	if got := shellVerbScriptWrites(script, false); !slices.Equal(got, []string{`\x2fm/a`}) {
		t.Errorf("oracle reading: got %q", got)
	}
	if got := shellVerbScriptWrites(script, true); !slices.Equal(got, []string{`\x2fm/a`, "/m/a"}) {
		t.Errorf("hardened reading: got %q", got)
	}
	if got := shellVerbScriptWrites(`fs.writeFileSync("/m/a","x")`, true); !slices.Equal(got, []string{"/m/a"}) {
		t.Errorf("a value that equals the raw text is named once: got %q", got)
	}
}

// shellWriteEscapeJS decodes the body of a JavaScript literal as Node reads it (escapes, a template's raw line breaks and its
// placeholders); a malformed escape makes it no literal.
func TestShellWriteEscapeJSLiteral(t *testing.T) {
	for _, c := range []struct {
		body     string
		template bool
		want     string
		ok       bool
	}{
		{`\x2fm/a`, false, "/m/a", true}, {`\u002fm/a`, false, "/m/a", true}, {`\u{2f}m/a`, false, "/m/a", true},
		{`\u{0000002f}m/a`, false, "/m/a", true}, {`\u{1F600}`, false, "\U0001f600", true}, {`\ud83d\ude00`, false, "\U0001f600", true},
		{`\u{d83d}\u{de00}`, false, "\U0001f600", true}, {`\ud83d\u{de00}`, false, "\U0001f600", true}, {`\u{d83d}\ude00`, false, "\U0001f600", true}, {`\u{d800}`, false, "\ufffd", true},
		{`\ud800`, false, "\ufffd", true}, {`\0`, false, "\x00", true}, {`\01`, false, "\x01", true}, {`\08`, false, "\x008", true},
		{`\101`, false, "A", true}, {`\1010`, false, "A0", true}, {`\400`, false, " 0", true}, {`\477`, false, "'7", true},
		{`\57m`, false, "/m", true}, {`\8`, false, "8", true}, {`\q/m`, false, "q/m", true}, {`\$`, false, "$", true},
		{`\'\"\\`, false, "'\"\\", true}, {`\b\f\n\r\t\v`, false, "\b\f\n\r\t\v", true}, {`a\`, false, `a\`, true},
		{"a\\\nb", false, "ab", true}, {"a\\\r\nb", false, "ab", true}, {"a\\\rb", false, "ab", true}, {"a\\\u2028b", false, "ab", true},
		{`${x}`, false, `${x}`, true}, {`${x}`, true, "", false}, {`\${x}`, true, `${x}`, true}, {`\\${x}`, true, "", false},
		{"a\r\nb", true, "a\nb", true}, {"a\rb", true, "a\nb", true}, {`a\rb`, true, "a\rb", true},
		{`\x2`, false, "", false}, {`\x`, false, "", false}, {`\xzz`, false, "", false}, {`\u12`, false, "", false}, {`\u`, false, "", false},
		{`\u{}`, false, "", false}, {`\u{110000}`, false, "", false}, {`\u{2f`, false, "", false},
	} {
		if got, ok := shellWriteEscapeJS(c.body, c.template); got != c.want || ok != c.ok {
			t.Errorf("%q (template %v): got %q, %v; want %q, %v", c.body, c.template, got, ok, c.want, c.ok)
		}
	}
}

// The Path join costs time and memory linear in its arguments: a long run of literal parts, with or without a trailing slash, must not
// copy the accumulated path again for each one (the hook reads inputs of several MiB).
func TestShellWriteEscapePathJoinIsLinear(t *testing.T) {
	for _, part := range []string{"'a',", "'a/',"} {
		allocated := func(parts int) uint64 {
			script := "Path(" + strings.Repeat(part, parts) + "'/m/x').write_text(1)"
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			got := shellVerbOpenWrites(script)
			runtime.ReadMemStats(&after)
			if !slices.Equal(got, []string{"/m/x"}) {
				t.Fatalf("%d parts of %s: got %q", parts, part, got)
			}
			return after.TotalAlloc - before.TotalAlloc
		}
		small, large := allocated(8000), allocated(32000)
		if large > 8*small { // four times the input: about four times the allocation, sixteen when each part copies the path
			t.Errorf("%s: allocation grew from %d to %d bytes for four times the parts", part, small, large)
		}
	}
}
