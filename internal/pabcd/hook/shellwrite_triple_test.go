package hook

import (
	"slices"
	"testing"
)

// CRW-678: the shell write-destination reader reads the physical line breaks of a triple-quoted body as Python's source
// decoding does (a CRLF and a lone CR both read as LF), and the two scanners that produce a call's argument spans
// (shellVerbOpenWrites, shellVerbWithoutComments) skip a triple-quoted literal as one string region. Every case is a command
// string; the oracle's own answer stays first in the result.

// TestShellWriteTripleScanNewlines is the CR and CRLF case: Python's source decoding turns either into LF before the literal
// is evaluated, so a destination written with either names the path Python writes, raw or not. A single-quoted literal cannot
// hold a physical line break, so its handling is unchanged and the existing cases cover it.
func TestShellWriteTripleScanNewlines(t *testing.T) {
	py := func(program string) string { return "python3 -c '" + program + "'" }
	shellWriteEscapeRun(t, []shellWriteEscapeCase{
		{command: py(`open("""/m/a` + "\r\n" + `b""","w")`), has: []string{"/m/a\nb"}, lacks: []string{"/m/a\r\nb"}},
		{command: py(`open("""/m/a` + "\r" + `b""","w")`), has: []string{"/m/a\nb"}, lacks: []string{"/m/a\rb"}},
		{command: py(`open(r"""/tmp/a` + "\r\n" + `b""","w")`), has: []string{"/tmp/a\nb"}, lacks: []string{"/tmp/a\r\nb"}},
		{command: py(`open(r"""/tmp/a` + "\r" + `/n.md""","w")`), has: []string{"/tmp/a\n/n.md"}, lacks: []string{"/tmp/a\r/n.md"}},
		// the escape decoding and the field-free f-string brace fold still run on the normalised body
		{command: py(`open(f"""/m/a{{b}}` + "\r" + `x""","w")`), has: []string{"/m/a{b}\nx"}, lacks: []string{"/m/a{b}\rx"}},
	})
}

// TestShellWriteTripleScanRegions is the scanner case: a comma, a closing bracket, a comment marker or another quote inside a
// triple-quoted literal is part of the string and does not end the argument or the literal, and a backslash keeps the character
// after it from closing the region.
func TestShellWriteTripleScanRegions(t *testing.T) {
	py := func(program string) string { return "python3 -c '" + program + "'" }
	shellWriteEscapeRun(t, []shellWriteEscapeCase{
		{command: py(`open("""/m/a,b""", "w")`), has: []string{"/m/a,b"}},
		{command: `python3 -c "open('''/m/a)b''', 'w')"`, has: []string{"/m/a)b"}},
		{command: py(`open("""/m/a#b""", "w")`), has: []string{"/m/a#b"}},
		{command: `python3 -c "open(\"\"\"/m/a'b\"\"\", \"w\")"`, has: []string{"/m/a'b"}},
		{command: py(`open("""/review/memories/"x""","w")`), has: []string{`/review/memories/"x`}},
	})
}

// TestShellWriteTripleScanUnits is the same reading at the unit boundary: shellVerbOpenWrites returns the destination of a
// triple-quoted open() call once, from the decoded reading alone.
func TestShellWriteTripleScanUnits(t *testing.T) {
	for _, c := range []struct {
		script string
		want   []string
	}{
		{`open("""/m/a,b""","w")`, []string{"/m/a,b"}},
		{`open('''/m/a)b''','w')`, []string{"/m/a)b"}},
		{`open("""/m/a#b""","w")`, []string{"/m/a#b"}},
		{`open("""/m/a'b""","w")`, []string{"/m/a'b"}},
		{`open("""/review/memories/"x""","w")`, []string{`/review/memories/"x`}},
		{`open("""/m/a\"""b""","w")`, []string{`/m/a"""b`}},
		{`open(rb"""/m/a""","w")`, []string{"/m/a"}},
		{`open("""/m/a` + "\r\n" + `b""","w")`, []string{"/m/a\nb"}},
		// A comment after the literal still ends at the newline, and a # inside the body is not a comment.
		{"open(\"\"\"/m/a\"\"\",\"w\") # open(\"/m/x\",\"w\")", []string{"/m/a"}},
	} {
		if got := shellVerbOpenWrites(c.script); !slices.Equal(got, c.want) {
			t.Errorf("%q: got %q, want %q", c.script, got, c.want)
		}
	}
}

// TestShellWriteTripleScanNodeKeepsTheDestination is the guard for the regression the Codex Code Review found on this change:
// the triple-quoted region rule is Python's, and a JavaScript program is not Python source. A template literal holding three
// quotes must not open a region that swallows the rest of the program, which would lose the decoded destination of a write the
// reader named before the rule existed. node -e drives the same scriptWriteDestinations path as python -c. An odd run of quotes
// in a template literal still desyncs the Node walk, exactly as it did before this change: the scanner reads no template literal,
// so that is the pre-existing hardened-path limit, not a regression.
func TestShellWriteTripleScanNodeKeepsTheDestination(t *testing.T) {
	js := func(program string) string { return "node -e '" + program + "'" }
	shellWriteEscapeRun(t, []shellWriteEscapeCase{
		{command: js("const x = " + "\x60" + `""""` + "\x60" + `; fs.open("\x2freview/memories/a", "w", cb)`), has: []string{"/review/memories/a"}},
		{command: js("const x = " + "\x60" + `""""` + "\x60" + `; fs.writeFileSync("\x2freview/memories/c", "x")`), has: []string{"/review/memories/c"}},
	})
}

// TestShellWriteTripleScanUnterminated pins the fail-closed answer the independent audit asked for: a program whose
// triple-quoted literal is never closed is not valid Python (a SyntaxError), so no write runs, and the reader names nothing
// rather than guessing a destination. An odd run of quotes that desyncs the walk is the same case: the region runs to the end
// of the text and no later call is read. Naming nothing is the safe answer for a security gate.
func TestShellWriteTripleScanUnterminated(t *testing.T) {
	for _, script := range []string{
		`open("""/m/a,b""`,
		`open("""/m/a""`,
		`open("""/m/a`,
		`open('''/m/a`,
		`x = """; open("/m/a","w")`,
	} {
		if got := shellVerbOpenWrites(script); len(got) != 0 {
			t.Errorf("%q: got %q, want no destination (fail-closed)", script, got)
		}
	}
}
