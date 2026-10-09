//go:build dev

package cxcfuzz

import (
	"math/rand"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// rootPlaceholder is the literal an input writes where an absolute path under the case root
// belongs. The harness builds the same relative tree under two different roots, one per side, so an
// absolute destination cannot be written into the input; each side substitutes its own root for
// this text before the call, and the harness rewrites each side's root back to it in the answers
// (ReplaceRoot).
const rootPlaceholder = "${ROOT}"

// shellwriteTarget is the shell write destination reader (CRW-703): the ported
// hook.ShellWriteDestinations against the oracle's shellWriteDestinations at CXC v0.2.40
// (plugins/codexclaw/components/pabcd-state/dist/shell-write-destinations.js:25-36). The answer is a
// destination set: order and duplicates are ignored, a destination the oracle names and the port
// does not is a miss (a write the memory gate would let through), one only the port names is
// extra, and a difference on both sides at once is differ.
func shellwriteTarget() Target {
	return Target{
		Name:     "shellwrite",
		Generate: shellWriteGenerate,
		Go:       shellWriteGo,
		Oracle:   Oracle{Command: "node", Shim: shimPath("shellwrite"), Root: DefaultOracleRoot},
		Compare:  shellWriteCompare,
		Reading:  shellWriteReading,
	}
}

// shellWriteGo is the Go side: hook.ShellWriteDestinations, as a JSON array of strings. The case's
// own root takes the place of the input's ROOT placeholder in the decoded input's string values,
// exactly as the shim substitutes its own before it reads the command.
func shellWriteGo(input any, env Env) (any, error) {
	command := ""
	if value, found := field(substituteRootValue(input, env.Root), "command"); found {
		command, _ = value.(string)
	}
	return shellWriteStrings(hook.ShellWriteDestinations(command)), nil
}

func shellWriteStrings(dests []string) []any {
	out := make([]any, 0, len(dests))
	for _, dest := range dests {
		out = append(out, dest)
	}
	return out
}

// shellWriteCompare classifies the two destination sets.
func shellWriteCompare(goOut, oracleOut any) Verdict {
	goSet, goList := shellWriteSet(goOut)
	oracleSet, oracleList := shellWriteSet(oracleOut)
	// A side that answered something other than a destination array - a worker failure, an error
	// reply - is not an empty set: reading it as one would let a failure agree with a reader that
	// names nothing.
	if !goList || !oracleList {
		return Verdict{Kind: Differ, Detail: "the oracle answered " + shellWriteClip(canonical(oracleOut)) + "; the port " + shellWriteClip(canonical(goOut))}
	}
	missing, extra := shellWriteOnly(oracleSet, goSet), shellWriteOnly(goSet, oracleSet)
	switch {
	case len(missing) > 0 && len(extra) > 0:
		return Verdict{Kind: Differ, Detail: "the oracle names " + strings.Join(missing, ", ") + "; Go names " + strings.Join(extra, ", ")}
	case len(missing) > 0:
		return Verdict{Kind: Miss, Detail: "the oracle names " + strings.Join(missing, ", ")}
	case len(extra) > 0:
		return Verdict{Kind: Extra, Detail: "Go names " + strings.Join(extra, ", ")}
	default:
		return Verdict{Kind: Same}
	}
}

// shellWriteSet reads a destination array; the second answer says whether the value was one.
func shellWriteSet(value any) (map[string]bool, bool) {
	out := map[string]bool{}
	items, ok := value.([]any)
	if !ok {
		return out, false
	}
	for _, item := range items {
		if text, ok := item.(string); ok {
			out[text] = true
		}
	}
	return out, true
}

// shellWriteClip bounds a detail so a long answer does not fill a divergence file.
func shellWriteClip(text string) string {
	if len(text) > 200 {
		return text[:200]
	}
	return text
}

// shellWriteOnly is the sorted members of one that the other lacks.
func shellWriteOnly(one, other map[string]bool) []string {
	out := []string{}
	for key := range one {
		if !other[key] {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// shellWritePathFragments are the destinations the shellwrite generator mixes with shell
// punctuation: a small set carrying the characters a lexer trips on.
// It is a function rather than a package-level value, because this package does no work at
// program start.
func shellWritePathFragments() []string {
	return []string{
		"/m/a", "/m/a.md", "/m/n", "~/x", "~/x.md", "rel", "rel.md", "./rel", "../rel", "/w/x.md",
		"/m/a b", "/m/a'b", `/m/a"b`, "/m/a#b", "/m/a,b", "/m/a)b", "/m/a;b", "/m/a|b", "/m/a$b", "/m/a\\b",
		"/m/a\nb", "/m/a\rb", "/m/a\r\nb", "/m/-x", "/m/a",
	}
}

// shellWriteGenerate is one command: one to three segments, each a wrapper over a redirect, a
// writing verb, an interpreter program, a heredoc, a quoted operand, a read-only use or a nested
// shell. Every path comes from the pool the caller names, so the memory gate target can reuse the
// whole grammar with destinations under its own protected root.
func shellWriteGenerate(rng *rand.Rand, size int) any {
	return pyjson.Object{{Key: "command", Value: shellWriteCommandWith(rng, shellWritePathFragments(), size)}}
}

func shellWriteCommandWith(rng *rand.Rand, paths []string, size int) string {
	segments := 1 + size%3
	if segments < 1 {
		segments = 1
	}
	parts := make([]string, 0, segments)
	for i := 0; i < segments; i++ {
		parts = append(parts, shellWriteSegmentWith(rng, paths))
	}
	separators := []string{"; ", " && ", " || ", " | ", " & ", "\n"}
	return strings.Join(parts, separators[rng.Intn(len(separators))])
}

func shellWriteSegmentWith(rng *rand.Rand, paths []string) string {
	switch rng.Intn(12) {
	case 0, 1, 2, 3:
		return shellWriteWrapper(rng) + shellWriteReader(rng) + shellWriteRedirect(rng) + shellWritePath(rng, paths)
	case 4, 5:
		return shellWriteWrapper(rng) + shellWriteVerb(rng, paths)
	case 6:
		return shellWriteWrapper(rng) + shellWriteProgram(rng, paths)
	case 7:
		return shellWriteHeredoc(rng, paths)
	case 8:
		return shellWriteWrapper(rng) + "echo " + shellWriteQuoted(rng, shellWritePath(rng, paths))
	case 9:
		return shellWriteWrapper(rng) + "rg " + shellWritePath(rng, paths) + " 2>/dev/null"
	case 10:
		return shellWriteNested(rng, paths)
	default:
		return shellWriteWrapper(rng) + "true " + shellWriteComment(rng, paths)
	}
}

// shellWriteWrapper is the command a write runs under. Each one hides the real verb from a reader
// that only looks at the first word.
func shellWriteWrapper(rng *rand.Rand) string {
	wrappers := []string{"", "", "env A=1 ", "sudo ", "command ", "exec ", "nohup ", "timeout 5 ",
		"builtin ", "env -i ", "sudo -u root ", "timeout 5 sudo ", "env A=1 B=2 sudo "}
	return wrappers[rng.Intn(len(wrappers))]
}

// shellWriteReader is the word before the redirect: an ordinary command, or nothing at all so the
// redirect is the segment's first token.
func shellWriteReader(rng *rand.Rand) string {
	readers := []string{"echo hi ", "printf x ", "echo hi", "cat ", "true ", "echo 'a>b' ", "echo \"a > b\" ", "", "echo "}
	return readers[rng.Intn(len(readers))]
}

func shellWriteRedirect(rng *rand.Rand) string {
	redirects := []string{"> ", ">>", ">|", "&> ", "1> ", "2> ", ">", ">> ", "0<> ", "&>> "}
	return redirects[rng.Intn(len(redirects))]
}

func shellWritePath(rng *rand.Rand, paths []string) string {
	return paths[rng.Intn(len(paths))]
}

// shellWriteQuoted wraps a word in one of the quoting forms a shell accepts. A word that already
// holds the closing quote is deliberately left unterminated: the oracle has a recorded answer for
// that, and the target must reproduce it.
func shellWriteQuoted(rng *rand.Rand, word string) string {
	switch rng.Intn(5) {
	case 0:
		return "'" + word + "'"
	case 1:
		return "\"" + word + "\""
	case 2:
		return "$'" + word + "'"
	case 3:
		return "\\" + word
	default:
		return word
	}
}

// shellWriteVerb is a command whose own arguments name the file it writes.
func shellWriteVerb(rng *rand.Rand, paths []string) string {
	dest := shellWritePath(rng, paths)
	verbs := []string{
		"tee " + dest,
		"tee -a " + dest,
		"tee -a " + dest + " /w/other",
		"sed -i 's/a/b/' " + dest,
		"sed --in-place=.bak 's/a/b/' " + dest,
		"sed -i.bak -e 's/a/b/' " + dest,
		"cp /w/a.md " + dest,
		"mv /w/a.md " + dest,
		"cp -t " + dest + " /w/a.md",
		"install -m 644 /w/a.md " + dest,
		"dd of=" + dest + " if=/w/a.md",
		"perl -i -pe 's/a/b/' " + dest,
		"ruby -i -pe 's/a/b/' " + dest,
		"perl -pie 's/a/b/' " + dest,
	}
	return verbs[rng.Intn(len(verbs))]
}

// shellWriteProgram is a python or node one-liner whose program text names the write. It carries
// every Python spelling the issue required: open/Path writes, os.rename and shutil.copy/copyfile
// with the destination second, each Python literal quoting form, and a destination whose slash is
// written as one of the single-character escapes. The program it emits is one of
// shellWritePrograms, which the literal-form test walks in full.
func shellWriteProgram(rng *rand.Rand, paths []string) string {
	commands := shellWritePrograms(shellWritePath(rng, paths))
	chosen := commands[rng.Intn(len(commands))]
	return chosen.interpreter + " " + chosen.flag + chosen.sep + shellWriteShellQuote(chosen.program)
}

// shellWriteCommand is one program the generator can emit and the interpreter that runs it. The
// interpreter, its flag and the separator between the flag and the program are kept apart from the
// program so a test can hand the program to the real interpreter without re-reading the shell quoting.
// The separator is part of the form: `-c` and `-e` take the program joined to the flag, while `--eval`
// takes it as a separate word, and a reader of the command reads one or the other.
type shellWriteCommand struct {
	interpreter string
	flag        string
	sep         string
	program     string
}

// shellWritePrograms is every program the generator can emit for a destination, in a fixed order.
// Every Python program it returns names the destination in a literal that evaluates to that
// destination; the literal-form test evaluates each one with the real interpreter.
func shellWritePrograms(dest string) []shellWriteCommand {
	// Every Python literal that embeds the destination is written so it evaluates to the destination:
	// a quote or a backslash in the pool would otherwise make the literal a syntax error or decode the
	// backslash to another character, and the program would not name the path the case chose.
	quoted := shellWritePythonEscaped(dest, "'", false)
	appendProgram := "open('" + quoted + "','a')"
	if raw, ok := shellWritePythonRaw(dest); ok {
		appendProgram = "open(" + raw + ",'a')"
	}
	// Every program goes through shellWriteShellQuote: a destination from the pool may hold a double
	// quote, a dollar, a backslash or a backtick, and a fixed double-quoted argument would let the
	// shell rewrite the program before the interpreter ever saw it.
	commands := []shellWriteCommand{
		// The separated and attached spellings of the interpreter flag are both emitted, exactly as the
		// generator emitted them before: a reader that reads only one of the two would miss the other.
		{"python3", "-c", " ", "open('" + quoted + "','w').write('x')"},
		{"python3", "-c", " ", "from pathlib import Path; Path('" + quoted + "').write_text('x')"},
		{"python3", "-c", " ", "from pathlib import Path; Path('/m','" + quoted + "').write_bytes(b'x')"},
		{"python3", "-c", " ", appendProgram},
		{"py", "-c", " ", "open('" + quoted + "','w')"},
		{"python3", "-c", "", "open('" + quoted + "','w')"},
		{"node", "-e", " ", "require('fs').writeFileSync('" + dest + "','x')"},
		{"node", "--eval", " ", "require('fs').createWriteStream('" + dest + "')"},
		{"node", "-e", "", "require('fs').appendFileSync('" + dest + "','x')"},
		{"node", "-e", " ", "require('fs').open('" + dest + "','w',()=>{})"},
	}
	// The destination is the second argument of os.rename and shutil.copy/copyfile, so a reader
	// that only reads the first argument of a call names the source and misses the write.
	for _, literal := range shellWritePythonLiteralForms(dest) {
		commands = append(commands,
			shellWriteCommand{"python3", "-c", " ", "import os; os.rename(\"/w/old.md\", " + literal + ")"},
			shellWriteCommand{"python3", "-c", " ", "import shutil; shutil.copy(\"/w/old.md\", " + literal + ")"},
			shellWriteCommand{"python3", "-c", " ", "import shutil; shutil.copyfile(\"/w/old.md\", " + literal + ")"},
		)
	}
	// The slash of a destination written as an escape: a reader that does not decode the escape
	// names no path, or names the raw text. The escape IS the literal body: its leading backslash is
	// the escape Python decodes to the destination's slash, so it is embedded as written. Escaping it
	// again would double that backslash and Python would read the escape's letters as the path.
	for _, escaped := range shellWritePythonEscapeForms(dest) {
		commands = append(commands, shellWriteCommand{"python3", "-c", " ", "open(\"" + escaped + "\",\"w\")"})
	}
	return commands
}

// shellWriteShellQuote wraps a program so a real shell hands it to the interpreter unchanged. The
// generator emits a command a reader lexes, but a form the shell cannot pass through is not the
// write form the issue asked for: a single-quoted argument holding an unescaped single quote ends
// at that quote, and the interpreter then receives a truncated program. A program without a single
// quote takes the plain single-quoted form; otherwise it takes a double-quoted form with the four
// characters the shell still reads inside double quotes escaped.
//
// The ROOT placeholder is never escaped. The program is split around each occurrence and only the
// rest of each part is escaped, so the placeholder reaches the harness's substitution byte for byte
// and the destination the program names after substitution is the path the case chose: escaping the
// placeholder first turned \${ROOT} into \<the case root>, and a reader then named a path with a
// stray backslash in front of it. The placeholder carries no character a shell reads inside double
// quotes, and a campaign root is an os.MkdirTemp path with no shell-special character either; a root
// that did hold one is covered by the value-substitution unit test CRW-857 added (the harness
// substitutes the decoded input value, never the serialized text).
func shellWriteShellQuote(program string) string {
	if !strings.Contains(program, "'") {
		return "'" + program + "'"
	}
	parts := strings.Split(program, rootPlaceholder)
	for i, part := range parts {
		parts[i] = shellWriteDoubleQuoteEscape(part)
	}
	return "\"" + strings.Join(parts, rootPlaceholder) + "\""
}

// shellWriteDoubleQuoteEscape escapes the four characters a shell still reads inside a double-quoted
// word, so the text it returns is passed through unchanged.
func shellWriteDoubleQuoteEscape(text string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(text)
}

// shellWritePythonLiteralForms is every quoting form the generator uses for a Python string literal:
// the four quote characters Python accepts, and the r, b, u and f prefixes. Every form it returns
// evaluates in Python 3 to exactly the destination it was built for, so a reader that walks the
// destination out of a generated program names the path the case chose. A form that cannot hold the
// destination is not returned: a raw form only where the destination has no quote it would have to
// escape and does not end in a backslash, and a b form only for an ASCII destination, because a bytes
// literal cannot hold a non-ASCII character. The f forms keep the brace rule: a brace inside the
// destination is written doubled, so the literal evaluates to the destination instead of opening a
// replacement field, while the braces of an exact ROOT placeholder are left alone for the harness's
// substitution.
func shellWritePythonLiteralForms(word string) []string {
	forms := []string{
		"'" + shellWritePythonEscaped(word, "'", false) + "'",
		"\"" + shellWritePythonEscaped(word, "\"", false) + "\"",
		"'''" + shellWritePythonEscaped(word, "'", false) + "'''",
		"\"\"\"" + shellWritePythonEscaped(word, "\"", false) + "\"\"\"",
	}
	if raw, ok := shellWritePythonRaw(word); ok {
		forms = append(forms, raw)
	}
	if shellWritePythonASCII(word) {
		forms = append(forms, "b'"+shellWritePythonEscaped(word, "'", false)+"'")
	}
	return append(forms,
		"u'"+shellWritePythonEscaped(word, "'", false)+"'",
		"f'"+shellWritePythonEscaped(word, "'", true)+"'",
		"f'''"+shellWritePythonEscaped(word, "'", true)+"'''",
		"f\"\"\""+shellWritePythonEscaped(word, "\"", true)+"\"\"\"",
	)
}

// shellWritePythonEscaped writes a destination so a non-raw Python literal delimited by quote holds it
// unchanged: a backslash is doubled, the delimiter is escaped, and a line break is written as its
// escape, because Python reads a physical line break inside a literal as a newline and normalizes a
// carriage return. braces additionally doubles every brace that is not part of an exact ROOT
// placeholder, which is what an f literal needs to evaluate to the destination. The placeholder is
// split out and rejoined untouched, because the harness substitutes that token in the decoded command
// after the program is built.
func shellWritePythonEscaped(word, quote string, braces bool) string {
	parts := strings.Split(word, rootPlaceholder)
	for i, part := range parts {
		var out strings.Builder
		for _, r := range part {
			switch {
			case r == '\\':
				out.WriteString(`\\`)
			case r == '\n':
				out.WriteString(`\n`)
			case r == '\r':
				out.WriteString(`\r`)
			case braces && r == '{':
				out.WriteString("{{")
			case braces && r == '}':
				out.WriteString("}}")
			case string(r) == quote:
				out.WriteByte('\\')
				out.WriteRune(r)
			default:
				out.WriteRune(r)
			}
		}
		parts[i] = out.String()
	}
	return strings.Join(parts, rootPlaceholder)
}

// shellWritePythonRaw is the raw form of a destination, and whether the destination can be held in it
// unchanged. A raw literal processes no escape, so it holds a backslash as written; it cannot hold the
// delimiter it would have to escape, cannot span a line, and cannot end in a backslash, which would
// escape its own closing quote.
func shellWritePythonRaw(word string) (string, bool) {
	if strings.ContainsAny(word, "'\n\r") || strings.HasSuffix(word, `\`) {
		return "", false
	}
	return "r'" + word + "'", true
}

// shellWritePythonASCII reports whether a destination holds only ASCII characters, which is what a
// bytes literal can carry: Python refuses a non-ASCII character in a b literal. A destination still
// holding the ROOT placeholder answers no, because the harness substitutes the case root into it after
// the program is built and a root is not promised to be ASCII - a non-ASCII root inside a b literal is
// a source error, so the bytes form is simply not emitted for such a destination.
func shellWritePythonASCII(word string) bool {
	if strings.Contains(word, rootPlaceholder) {
		return false
	}
	for _, r := range word {
		if r > 0x7f {
			return false
		}
	}
	return true
}

// shellWritePythonEscapeForms writes the first slash of a destination as each single-character
// escape Python accepts for it. A destination with no slash has no such form, and the returned
// slice is nil then.
func shellWritePythonEscapeForms(word string) []string {
	at := strings.IndexByte(word, '/')
	if at < 0 {
		return nil
	}
	// The parts around the slash are ordinary literal text, so they are escaped the way any literal
	// escapes them - a quote the form's delimiter would close, or a backslash Python would decode -
	// while the escape itself is written as its own backslash sequence. The caller wraps the body in
	// double quotes, which is the delimiter these parts are escaped for.
	head := shellWritePythonEscaped(word[:at], "\"", false)
	tail := shellWritePythonEscaped(word[at+1:], "\"", false)
	return []string{
		head + `\x2f` + tail,
		head + `\u002f` + tail,
		head + `\057` + tail,
		head + `\N{SOLIDUS}` + tail,
	}
}

// shellWriteHeredoc is a heredoc whose body names a path that must not count as a destination, and
// whose operator line still carries the redirect that must.
func shellWriteHeredoc(rng *rand.Rand, paths []string) string {
	delims := []string{"EOF", "'EOF'", "\"EOF\"", "EOF-X", "-EOF", "'A B'", "A"}
	delim := delims[rng.Intn(len(delims))]
	operator := "cat <<" + delim
	if rng.Intn(4) == 0 {
		operator = "cat <<" + delim + " < /w/in"
	}
	// The terminating line is the delimiter word with its quoting and its `-` taken off: the shell
	// removes both before it compares the line, so a quoted delimiter must close with the bare word.
	terminator := strings.Trim(strings.TrimPrefix(delim, "-"), "'\"")
	return operator + " > " + shellWritePath(rng, paths) + "\n" + shellWritePath(rng, paths) + "\n" + terminator + "\necho x > " + shellWritePath(rng, paths)
}

// shellWriteNested hides the write one level down: in a shell -c, an eval, an xargs, a subshell or
// brace group, or a command substitution written with $( ) or with backticks.
func shellWriteNested(rng *rand.Rand, paths []string) string {
	inner := "echo hi > " + shellWritePath(rng, paths)
	forms := []string{
		"bash -c '" + inner + "'",
		"sh -c \"" + inner + "\"",
		"zsh -c '" + inner + "'",
		"eval " + inner,
		"eval eval " + inner,
		"su -c '" + inner + "'",
		"echo " + shellWritePath(rng, paths) + " | xargs -I{} tee {}",
		"xargs -n1 tee " + shellWritePath(rng, paths),
		"(" + inner + ")",
		"{ " + inner + "; }",
		"x=$(" + inner + ")",
		"y=`" + inner + "`",
	}
	return forms[rng.Intn(len(forms))]
}

// shellWriteComment ends a segment with a comment, whose text must be read as text.
func shellWriteComment(rng *rand.Rand, paths []string) string {
	comments := []string{"# " + shellWritePath(rng, paths), "#word " + shellWritePath(rng, paths), "\\ # " + shellWritePath(rng, paths)}
	return comments[rng.Intn(len(comments))]
}

// shellWriteReading is the c2g measure for this target: the input is unreadable when the shared reader cannot read its command,
// and the Go side refused it when it named the unknown destination (a destination holding NUL), the only answer ShellWriteDestinations
// gives for a command it cannot read.
func shellWriteReading(input any, env Env, goOut any) (unreadable, refused bool) {
	command := ""
	if value, found := field(substituteRootValue(input, env.Root), "command"); found {
		command, _ = value.(string)
	}
	if hook.ShellCommandReadable(command, "", nil) {
		return false, false
	}
	items, _ := goOut.([]any)
	for _, item := range items {
		if text, ok := item.(string); ok && strings.Contains(text, "\x00") {
			return true, true
		}
	}
	return true, false
}
