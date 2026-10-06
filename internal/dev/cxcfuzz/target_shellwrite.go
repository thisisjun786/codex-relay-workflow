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
// with the destination second, each Python literal quoting form (including the f form around a
// doubled brace), and a destination whose slash is written as one of the single-character escapes.
func shellWriteProgram(rng *rand.Rand, paths []string) string {
	dest := shellWritePath(rng, paths)
	programs := []string{
		"python3 -c \"open('" + dest + "','w').write('x')\"",
		"python3 -c \"from pathlib import Path; Path('" + dest + "').write_text('x')\"",
		"python3 -c \"from pathlib import Path; Path('/m','" + dest + "').write_bytes(b'x')\"",
		"python3 -c \"open(r'" + dest + "','a')\"",
		"py -c \"open('" + dest + "','w')\"",
		"python3 -c\"open('" + dest + "','w')\"",
		"node -e \"require('fs').writeFileSync('" + dest + "','x')\"",
		"node --eval \"require('fs').createWriteStream('" + dest + "')\"",
		"node -e\"require('fs').appendFileSync('" + dest + "','x')\"",
		"node -e \"require('fs').open('" + dest + "','w',()=>{})\"",
	}
	// The destination is the second argument of os.rename and shutil.copy/copyfile, so a reader
	// that only reads the first argument of a call names the source and misses the write.
	for _, literal := range shellWritePythonLiteralForms(dest) {
		programs = append(programs,
			"python3 -c 'import os; os.rename(\"/w/old.md\", "+literal+")'",
			"python3 -c 'import shutil; shutil.copy(\"/w/old.md\", "+literal+")'",
			"python3 -c 'import shutil; shutil.copyfile(\"/w/old.md\", "+literal+")'",
		)
	}
	// The slash of a destination written as an escape: a reader that does not decode the escape
	// names no path, or names the raw text.
	for _, escaped := range shellWritePythonEscapeForms(dest) {
		programs = append(programs, "python3 -c 'open(\""+escaped+"\",\"w\")'")
	}
	return programs[rng.Intn(len(programs))]
}

// shellWritePythonLiteralForms is every quoting form the generator uses for a Python string
// literal: the four quote characters Python accepts, and the r, b, u and f prefixes. An f literal
// is sometimes wrapped in a doubled brace, because a brace inside an f-string is an expression
// unless it is doubled, and a reader that walks replacement fields must still find the path.
func shellWritePythonLiteralForms(word string) []string {
	return []string{
		"'" + word + "'",
		"\"" + word + "\"",
		"'''" + word + "'''",
		"\"\"\"" + word + "\"\"\"",
		"r'" + word + "'",
		"b'" + word + "'",
		"u'" + word + "'",
		"f'" + word + "'",
		"f'{{" + word + "}}'",
		"f\"\"\"{{" + word + "}}\"\"\"",
	}
}

// shellWritePythonEscapeForms writes the first slash of a destination as each single-character
// escape Python accepts for it. A destination with no slash has no such form, and the returned
// slice is nil then.
func shellWritePythonEscapeForms(word string) []string {
	at := strings.IndexByte(word, '/')
	if at < 0 {
		return nil
	}
	head, tail := word[:at], word[at+1:]
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
