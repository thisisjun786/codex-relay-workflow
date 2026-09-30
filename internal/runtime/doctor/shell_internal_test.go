package doctor

import (
	"strings"
	"testing"
)

var testExpander = Expander{Vars: map[string]string{"HOME": "/h", "CODEX_HOME": "/c", "PLUGIN_ROOT": "/p"}}

// parsed is each simple command as its words' values joined by |, commands by " / ", and each
// refused construct as "source: what".
func parsed(program string) (string, []string) {
	argvs, refused := parse(program, testExpander)
	var commands, constructs []string
	for _, argv := range argvs {
		var words []string
		for _, w := range argv {
			words = append(words, w.Value)
		}
		commands = append(commands, strings.Join(words, "|"))
	}
	for _, r := range refused {
		constructs = append(constructs, r[0]+": "+r[1])
	}
	return strings.Join(commands, " / "), constructs
}

// The grammar the scan judges: simple commands joined by ;, &, &&, ||, |, |& and newlines, with
// literal words and redirections to literal words. The native Stop command and the bridge
// launcher are inside it.
func TestTheGrammarReadsSimpleCommandsListsAndPipelines(t *testing.T) {
	for program, want := range map[string]string{
		`"$HOME/.local/share/crw-runtime/current/bin/crw" hook --plugin-launch; exit 0`: `/h/.local/share/crw-runtime/current/bin/crw|hook|--plugin-launch / exit|0`,
		"#!/bin/sh\n# comment\nexec \"$HOME/b\" --plugin-launch \"$@\"\n":               `exec|/h/b|--plugin-launch|`,
		`a && b || c | d |& e & f`:          `a / b / c / d / e / f`,
		"a\nb; ! c":                         `a / b / c`,
		`x >/dev/null 2>&1 <in >>"$HOME/l"`: `x`,
		`a\ b "c\"d" 'e'f \*`:               `a b|c"d|ef|*`,
	} {
		got, refused := parsed(program)
		if got != want || len(refused) != 0 {
			t.Errorf("%q\n got %s %v\nwant %s", program, got, refused, want)
		}
	}
}

// Everything else is refused with its source, never interpreted: a function (finding 1), an
// assignment in any form (finding 2), a compound command, a here-document or here-string, a
// command or process substitution, a redirection to a word that is not literal, and a program
// the parser rejects. The simple commands around a refused construct are still returned.
func TestTheGrammarRefusesEveryOtherConstruct(t *testing.T) {
	for _, c := range []struct{ program, commands, refused string }{
		{`relay() { command relay hook; }; relay`, `relay`, `FuncDecl`},
		{`function g { x; }`, ``, `FuncDecl`},
		{`PATH=/v relay hook`, ``, `an assignment`},
		{`PATH=/v; relay hook`, `relay|hook`, `an assignment`},
		{`export PATH=/v; relay hook`, `relay|hook`, `DeclClause`},
		{`if true; then relay; fi`, ``, `IfClause`},
		{`for x in a; do relay; done`, ``, `ForClause`},
		{`while true; do relay; done`, ``, `WhileClause`},
		{`case x in x) relay ;; esac`, ``, `CaseClause`},
		{`(relay)`, ``, `Subshell`},
		{`{ relay; }`, ``, `Block`},
		{`[[ -x /r ]] && relay`, `relay`, `TestClause`},
		{`(( i++ ))`, ``, `ArithmCmd`},
		{`time relay`, ``, `TimeClause`},
		{`coproc relay`, ``, `CoprocClause`},
		{"sh <<EOF\nrelay\nEOF", `sh`, `a here-document`},
		{`bash <<< relay`, `bash`, `a here-document`},
		{`echo "$(relay)"`, `echo|`, `a command or process substitution`},
		{"echo `relay`", `echo|`, `a command or process substitution`},
		{`diff <(relay) x`, `diff||x`, `a command or process substitution`},
		{`relay > "$LOG"`, `relay`, `a redirection to $LOG`},
		{`echo 'unterminated`, ``, `cannot parse`},
	} {
		got, refused := parsed(c.program)
		if got != c.commands || len(refused) != 1 || !strings.Contains(refused[0], c.refused) {
			t.Errorf("%q\n got %s %v\nwant %s and %q", c.program, got, refused, c.commands, c.refused)
		}
	}
}

// A word is literal only after the scan's own expansions (~ and ~/, $HOME, $CODEX_HOME,
// ${PLUGIN_ROOT}); any other expansion, and an unquoted glob or brace pattern (finding 3), is
// Missing. A word that is exactly one positional parameter is marked as one.
func TestAWordIsLiteralOnlyAfterTheScansExpansions(t *testing.T) {
	var got []string
	for _, program := range []string{
		`x ~/b ~ "$HOME/y" $CODEX_HOME/z ${PLUGIN_ROOT}/w '$HOME' '*' \?`,
		`$OTHER ~u/x ~"/x" ${HOME:-x} $'a' $"a" ven* a?b a[b] {a,b} x{`,
		`"$@" $1 "${2}" $* $0 "$@x" $#`,
	} {
		argvs, _ := parse(program, testExpander)
		for _, w := range argvs[0] {
			got = append(got, w.Written+"="+w.Value+"|"+w.Missing+map[bool]string{true: "|positional"}[w.Positional])
		}
	}
	want := []string{
		"x=x|", "~/b=/h/b|", "~=/h|", "$HOME/y=/h/y|", "$CODEX_HOME/z=/c/z|", "${PLUGIN_ROOT}/w=/p/w|", "$HOME=$HOME|", "*=*|", "?=?|",
		"$OTHER=|$OTHER", "~u/x=|~u/x", "~/x=/x|~", "${HOME:-x}=|${HOME:-x}", "$'a'=|$'a'", `$"a"=|$"a"`, "ven*=|ven*", "a?b=|a?b", "a[b]=a|[b]", "{a,b}=|{a,b}", "x{=|x{",
		"$@=|$@|positional", "$1=|$1|positional", "${2}=|${2}|positional", "$*=|$*|positional", "$0=|$0|positional", "$@x=x|$@", "$#=|$#",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
}

// A program nested deeper than maxDepth sh -c strings is not followed.
func TestAProgramNestedTooDeepIsUnreadable(t *testing.T) {
	program := "relay"
	for i := 0; i <= maxDepth; i++ {
		program = "sh -c '" + strings.ReplaceAll(program, "'", `'\''`) + "'"
	}
	var reports []string
	argvJudge{c: Classifier{Expand: Expander{Path: "/usr/bin:/bin"}}, report: func(word string, e Executable) {
		reports = append(reports, e.Kind+": "+e.Detail)
	}}.program(program)
	if len(reports) != 1 || !strings.Contains(reports[0], "nested more than") {
		t.Fatalf("reports %v", reports)
	}
}

// failures is every report of a recovered panic judging program with a judge that finds
// commands on /usr/bin:/bin.
func failures(program string) []string {
	var failed []string
	judge := argvJudge{c: Classifier{Expand: Expander{Vars: testExpander.Vars, Path: "/usr/bin:/bin"}}, report: func(word string, e Executable) {
		if strings.Contains(e.Detail, "reading of it failed") {
			failed = append(failed, word+": "+e.Detail)
		}
	}}
	readStopCommand(judge, program)
	return failed
}

// emptyWords are the texts finding 4131865048 names, and their neighbours: an empty quoted word
// has no parts, so nothing may index them.
var emptyWords = []string{`""`, `''`, `"$@"`, `"" x`, `x "" y`, `$''`, `$""`, ``, `;`, ` `, "\n", `sh -c ""`, `sh -c ''`, `exec ""`, `env ""`, `true "" ''`, `"""$@"`, `"$@"""`, `> ""`, `""()`, `a=""`}

// Parsing and judging an empty word, an empty program or a lone separator neither panics nor
// needs the recovery below.
func TestTheGrammarReadsEmptyWordsWithoutPanicking(t *testing.T) {
	for _, program := range emptyWords {
		parse(program, testExpander) // no recovery here: a panic fails the test
		if failed := failures(program); len(failed) != 0 {
			t.Errorf("%q: %v", program, failed)
		}
	}
}

// A failure of the scan's own reading of one program is recovered around that program alone
// and reported unreadable with its text; the next program is still read.
func TestAFailureReadingOneProgramIsUnreadableNotTheEndOfTheScan(t *testing.T) {
	var reports []string
	judge := argvJudge{c: Classifier{Expand: Expander{Path: "/usr/bin:/bin"}}, report: func(word string, e Executable) {
		reports = append(reports, word+": "+e.Kind+": "+e.Detail)
	}, seen: func(argv []shellWord, _ Executable) {
		if argv[0].Value == "env" {
			panic("boom")
		}
	}}
	judge.program("exit 0; env")
	judge.program("crw-no-such-command -m relay")
	if len(reports) != 2 || !strings.Contains(reports[0], "exit 0; env: unreadable: this scan's reading of it failed (boom)") || !strings.HasPrefix(reports[1], "crw-no-such-command: unreadable") {
		t.Fatalf("reports %v", reports)
	}
}

// FuzzTheStopReaderNeverFails runs the seed corpus in go test; go test -fuzz explores further.
// Neither the grammar nor the Stop reader may panic, recovered or not.
func FuzzTheStopReaderNeverFails(f *testing.F) {
	for _, seed := range append(emptyWords, `"$HOME/.local/share/crw-runtime/current/bin/crw" hook --plugin-launch; exit 0`,
		"python3 -c \"\nimport os\n\" \"${PLUGIN_ROOT}/wiring/crw_stop_hook.py\"", `f() { x; }; f`, `a[b] {a,b} ~u "${x:-$(y)}"`) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, program string) {
		parse(program, testExpander)
		if failed := failures(program); len(failed) != 0 {
			t.Fatalf("%q: %v", program, failed)
		}
	})
}
