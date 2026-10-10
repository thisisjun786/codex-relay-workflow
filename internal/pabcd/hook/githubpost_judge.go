package hook

// The GitHub post guard reads a command through the shared command reader (internal/pabcd/shellir). Every
// gh invocation the reader shows is judged by its argument words; a script file that a shell runs is read
// and judged the same way; an interpreter program that names a post is refused; a text the reader cannot
// read is refused. An argument word the reader cannot evaluate is passed to the rules as
// githubPostUnknownMark, so a rule that would read its value refuses it.

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// githubPostUnknownMark stands for an argument word whose value the reader cannot evaluate.
const githubPostUnknownMark = "\x00unknown"

// githubPostNoDir is the base a relative path resolves from when the working directory is unknown; no
// path under it lies under a trusted root and no script under it can be opened.
const githubPostNoDir = "\x00dir"

// githubPostMaxScriptDepth bounds script files that run script files.
const githubPostMaxScriptDepth = 4

// HandleGitHubPostGuard is the guard's PreToolUse leg: the deny envelope for a GitHub post these rules
// stop, else empty. Another tool or event, and a command unrelated to GitHub, pass.
func HandleGitHubPostGuard(raw string) string {
	p := editObject(raw)
	if p["hook_event_name"] != "PreToolUse" {
		return ""
	}
	tool, _ := p["tool_name"].(string)
	if !memoryGateShellTool(tool) {
		return ""
	}
	input, _ := p["tool_input"].(map[string]any)
	if input == nil {
		return ""
	}
	cwd, _ := p["cwd"].(string)
	cwd = shellirPayloadCwd(cwd)
	var site githubPostSite
	var denied bool
	if words, ok := githubPostArgv(input); ok {
		site, denied = githubPostJudgeArgv(words, cwd)
	} else if command, ok := githubPostCommand(input); ok && text.Trim(command) != "" {
		site, denied = githubPostJudgeText(command, cwd)
	}
	if !denied {
		return ""
	}
	return githubPostDenyPayload(site, p)
}

// githubPostJudgeArgv judges an argv array as the text that runs it: each word is quoted, so the reader
// sees the words exactly as given and no expansion happens.
func githubPostJudgeArgv(words []string, cwd string) (githubPostSite, bool) {
	parts := make([]string, 0, len(words))
	for _, w := range words {
		parts = append(parts, "'"+strings.ReplaceAll(w, "'", "'\\''")+"'")
	}
	return githubPostJudgeText(strings.Join(parts, " "), cwd)
}

// githubPostJudgeText is the rule for one shell text.
func githubPostJudgeText(command, cwd string) (githubPostSite, bool) {
	return githubPostJudgeTextDepth(command, cwd, 0, nil, false)
}

// githubPostJudgeTextDepth judges a text that a script file holds; outer is the writes of the texts that run it, which also
// happen before the script's own commands. cdpath is whether CDPATH may be set where the script runs (shellir.Exec.Cdpath): the body
// is read with it set, so a cd to a bare name in it is not a directory the guard knows.
func githubPostJudgeTextDepth(command, cwd string, depth int, outer *githubPostWrites, cdpath bool) (githubPostSite, bool) {
	site, denied := githubPostJudgeTextOnly(command, cwd, depth, outer, cdpath)
	if denied && !site.judged {
		// Whether a refusal is about a GitHub post is a fact of the text it was found in: the innermost text that refuses sets it,
		// and the text that runs that text does not overwrite it.
		site.judged, site.mentions = true, githubPostSpellsPost(command)
	}
	return site, denied
}

// githubPostEnv is the part of the hook's own environment, which the command it judges inherits, that the guard reads:
// PYTHONPYCACHEPREFIX, which makes a Python module run load compiled code from outside the module inventory (CRW-1178). Every other
// variable is unset for the guard, as before.
func githubPostEnv(name string) (string, bool) {
	if name != "PYTHONPYCACHEPREFIX" {
		return "", false
	}
	return os.LookupEnv(name)
}

func githubPostJudgeTextOnly(command, cwd string, depth int, outer *githubPostWrites, cdpath bool) (githubPostSite, bool) {
	res, err := shellir.AnalyzeScriptEnv(command, cwd, cdpath, githubPostEnv)
	if err != nil {
		return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, line: 0, reason: unreadableCause(err), analysis: true}, true
	}
	layoutLine, layout := 0, true
	if depth > 0 {
		layoutLine, layout = githubPostScriptLayout(command)
	}
	return githubPostJudgeExecs(res.Execs, depth, outer, layout, layoutLine)
}

// githubPostJudgeExecs judges the executions of a text; layoutLine is the line of the statement that breaks the layout of a script
// (layout false). A refusal inside a script carries the line of the execution (or statement) that is refused, which the script
// judge reports as script-file:line.
func githubPostJudgeExecs(execs []shellir.Exec, depth int, outer *githubPostWrites, layout bool, layoutLine int) (site githubPostSite, denied bool) {
	failLine := 0
	defer func() {
		if denied && site.line == 0 {
			site.line = failLine
		}
	}()
	// The closed rule: a post is judged only as one simple command. A post that sits behind a wrapper,
	// a shell, a list, a pipe or a substitution is refused.
	simple := len(execs) == 1 && githubPostPlainContext(execs[0].Ctx)
	// A script file that holds a post is read by the line rule: every command line of it is a post in form A or a command of
	// exception B, each a plain line of its own. One line that is neither refuses the script.
	lines := depth > 0 && githubPostHasPost(execs)
	if lines {
		// layout is whether the text is, statement by statement, plain simple commands each on a line of its own (no assignment,
		// redirection, list, declaration, compound command or word with an expansion): what the execution records cannot show.
		if !layout {
			failLine = layoutLine
			return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, post: true}, true
		}
		for _, e := range execs {
			if !githubPostScriptLine(e) {
				failLine = e.Line
				return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, post: true}, true
			}
		}
	}
	var written *githubPostTextWrites
	var i int
	textWrites := func() *githubPostTextWrites {
		if written == nil {
			written = githubPostWritesOf(execs, outer)
		}
		return written
	}
	// writes is the view of the writes made before execution i runs: the text's own records, and the script bodies run before it.
	writes := func() *githubPostWrites { return textWrites().at(i) }
	for i = range execs {
		e := execs[i]
		failLine = e.Line
		if e.Kind == shellir.KindScriptFile {
			if textWrites().stale(i, e.Script.Value, e.Dir) {
				return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, line: 0, reason: githubPostScriptReason("script file is written before it runs", e.Script.Value)}, true
			}
			if site, denied := githubPostJudgeScript(e, depth, writes().as(githubPostBodyKey(e.Script.Value, e.Dir))); denied {
				return site, true
			}
			continue
		}
		if e.Inline != nil && githubPostInlineNamesPost(e.Inline.Source.Value) {
			return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, post: true}, true
		}
		// A runner's arguments are judged whatever runs it: an installed tmux, or ./tmux, a binary of any size the reader does not read.
		if githubPostRunnerNamesPost(e) {
			return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, post: true}, true
		}
		direct := githubPostDirectPath(e)
		// A file run by path (./gh among them) that the text, or a script body it runs before, writes is not the file read here.
		if direct && githubPostDirectStale(textWrites(), i, e) {
			return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, line: 0, reason: githubPostScriptReason("script file is written before it runs", e.Program.Value)}, true
		}
		if direct && githubPostProgram(e.Name) != "gh" {
			if site, denied := githubPostJudgeDirect(e, depth, writes().as(githubPostBodyKey(e.Program.Value, e.Dir))); denied {
				return site, true
			}
			continue
		}
		if githubPostProgram(e.Name) != "gh" {
			continue
		}
		words := githubPostWordsOf(e)
		base := e.Dir.Path
		if !e.Dir.Known {
			base = githubPostNoDir
		}
		var reads []string
		if site, denied := githubPostJudgeWords(words, githubPostDir{path: base, reads: &reads}); denied {
			site.post = githubPostPostSub(words)
			return site, true
		}
		// A post of a script that passed the line rule is the whole command of its line, so what it reads is what the guard read
		// unless something the script, or the text that runs it, writes reaches that file.
		if !simple && !lines && githubPostPostSub(words) {
			return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, post: true}, true
		}
		if depth > 0 && len(reads) > 0 && writes().readsReach(reads) {
			return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, line: 0}, true
		}
		// ./gh is a file in the directory, which may be a script and not the installed gh: the file is read as well.
		if direct {
			if site, denied := githubPostJudgeDirect(e, depth, writes().as(githubPostBodyKey(e.Program.Value, e.Dir))); denied {
				return site, true
			}
		}
	}
	return githubPostSite{}, false
}

// githubPostWordsOf is the words of a gh execution: gh, then each argument, an argument the reader cannot evaluate as
// githubPostUnknownMark.
func githubPostWordsOf(e shellir.Exec) []string {
	words := []string{"gh"}
	for _, a := range e.Args {
		if a.Known {
			words = append(words, a.Value)
		} else {
			words = append(words, githubPostUnknownMark)
		}
	}
	return words
}

// githubPostHasPost is whether a text runs a gh command that posts (or calls the API): the mention that makes a script file a
// target of the line rule.
func githubPostHasPost(execs []shellir.Exec) bool {
	for _, e := range execs {
		if e.Kind == shellir.KindCommand && githubPostProgram(e.Name) == "gh" && githubPostPostSub(githubPostWordsOf(e)) {
			return true
		}
	}
	return false
}

// githubPostScriptLine is whether one execution of a script that holds a post is a line the rule allows: a plain command of its
// own line that is a gh post (its form is judged by the caller) or a command of exception B. Exception B is a program named by a
// bare word (or by an absolute path in a system bin directory) and run on literal words only: rg without --pre, --pre-glob or
// --hostname-bin, grep, egrep, fgrep, cat, head, tail, wc, ls, echo, printf without an option, and git log, show, diff, grep and
// status with no option before the subcommand and no option that writes a file or runs a program (git commit runs the hooks of the
// repository, so it is not a reading subcommand). Every other line (a gh read,
// another git subcommand, cd, an assignment, a script or an inline program) refuses the script.
func githubPostScriptLine(e shellir.Exec) bool {
	if e.Kind != shellir.KindCommand || e.Inline != nil || !githubPostPlainContext(e.Ctx) || !e.Program.Known ||
		len(e.Assigns) > 0 || len(e.Redirs) > 0 {
		return false
	}
	if githubPostProgram(e.Name) == "gh" {
		return githubPostPostSub(githubPostWordsOf(e))
	}
	if !githubPostInstalledName(e) {
		return false // a relative path names a file, not the installed program
	}
	args := make([]string, 0, len(e.Args))
	for _, a := range e.Args {
		if !a.Known {
			return false
		}
		args = append(args, a.Value)
	}
	switch e.Name {
	case "grep", "egrep", "fgrep", "cat", "head", "tail", "wc", "ls", "echo":
		return true
	case "printf":
		// printf -v assigns a variable (PATH among them), and zsh printf %n stores a count in the variable it names: only a
		// format of plain conversions is read.
		return len(args) == 0 || githubPostPrintfFormat(args[0])
	case "rg":
		for _, a := range args {
			switch {
			case a == "--pre" || a == "--pre-glob" || a == "--hostname-bin",
				strings.HasPrefix(a, "--pre=") || strings.HasPrefix(a, "--pre-glob=") || strings.HasPrefix(a, "--hostname-bin="):
				return false
			}
		}
		return true
	case "git":
		return githubPostGitReadLine(args)
	}
	return false
}

// githubPostInstalledName is whether a command is named by a bare word or by an absolute path in a system bin directory, so that
// it is the installed program and not a file of the working directory.
func githubPostInstalledName(e shellir.Exec) bool {
	if e.Program.Value == e.Name {
		return true
	}
	dir, base := filepath.Split(e.Program.Value)
	switch dir {
	case "/bin/", "/usr/bin/", "/usr/local/bin/", "/opt/homebrew/bin/":
		return base == e.Name
	}
	return false
}

// githubPostPrintfFormat is whether a printf format is text, escapes and plain conversions only: %% or % with flags, a decimal
// width and precision and one of the conversions s d i o u x X f F e E g G a A c b q. Any other directive (%n, which zsh reads as
// an assignment of the count printed so far to the variable it names, a positional %1$, a * width, %(...)T) refuses the line, and
// so does a first word that is an option (printf -v assigns a variable).
func githubPostPrintfFormat(f string) bool {
	if strings.HasPrefix(f, "-") {
		return false
	}
	return githubPostPrintfPlain.MatchString(f)
}

// githubPostPrintfPlain matches a format whose every % starts a plain conversion.
var githubPostPrintfPlain = regexp.MustCompile(`^(?:[^%]|%%|%[-+ #0]*[0-9]*(?:\.[0-9]*)?[sdiouxXfFeEgGaAcbq])*$`)

// githubPostGitValueOpts is, per subcommand of exception B, the short options that take a value: required ones (the rest of the
// bundle, else the next word) and optional ones (the rest of the bundle only), the way git's option parsers read them.
var githubPostGitValueOpts = map[string]struct{ required, optional string }{
	"log":    {"SGLnIO", "UlMCBX"},
	"show":   {"SGLnIO", "UlMCBX"},
	"diff":   {"SGLnIO", "UlMCBX"},
	"grep":   {"efABCm", "O"},
	"status": {"", "u"},
}

// githubPostGitFlags is the long options of exception B that take no value. A long option not in the list, written without =,
// may take the next word as its value (--src-prefix X), and that word may be the --; so a -- that follows one is not read as
// the end of the options.
var githubPostGitFlags = map[string]bool{
	"oneline": true, "stat": true, "numstat": true, "shortstat": true, "name-only": true, "name-status": true, "cached": true,
	"staged": true, "no-index": true, "patch": true, "quiet": true, "color": true, "no-color": true, "summary": true,
	"graph": true, "all": true, "abbrev-commit": true, "decorate": true, "follow": true, "merges": true, "no-merges": true,
	"first-parent": true, "reverse": true, "raw": true, "check": true, "exit-code": true, "porcelain": true, "short": true,
	"branch": true, "long": true, "cc": true, "no-ext-diff": true, "no-textconv": true, "count": true, "line-number": true,
	"ignore-case": true, "files-with-matches": true, "fixed-strings": true, "extended-regexp": true, "word-regexp": true,
	"invert-match": true, "untracked": true, "null": true, "no-patch": true, "no-color-moved": true, "full-index": true,
}

// githubPostGitReadLine is git of exception B: log, show, diff, grep or status as the first word (no option before the
// subcommand), without an option that writes a file (--output, in any abbreviation git accepts) or that runs a program on the files
// (-O, --open-files-in-pager). A short bundle is read option by option: an option that takes a value ends the bundle, so the O of
// -SOrder or -eOpen is a value, and the word after an option whose value is required (-e -O) is that value.
func githubPostGitReadLine(args []string) bool {
	if len(args) == 0 {
		return false
	}
	opts, ok := githubPostGitValueOpts[args[0]]
	if !ok {
		return false
	}
	ambiguous := false // the previous word is a long option that may have taken this word as its value
	for i := 1; i < len(args); i++ {
		a := args[i]
		prev := ambiguous
		ambiguous = false
		switch {
		case a == "--":
			// The words after -- are paths, unless this -- is the value of the option before it (--src-prefix -- --output=F).
			return !prev
		case strings.HasPrefix(a, "--"):
			name, _, hasValue := strings.Cut(strings.TrimPrefix(a, "--"), "=")
			if name != "" && (strings.HasPrefix("output", name) || strings.HasPrefix("open-files-in-pager", name)) {
				return false
			}
			ambiguous = !hasValue && !githubPostGitFlags[name]
		case len(a) > 1 && a[0] == '-':
			for j := 1; j < len(a); j++ {
				c := a[j]
				if c == 'O' {
					return false
				}
				if strings.IndexByte(opts.optional, c) >= 0 {
					break
				}
				if strings.IndexByte(opts.required, c) >= 0 {
					if j == len(a)-1 {
						i++ // the value is the next word
					}
					break
				}
			}
		}
	}
	return true
}

// githubPostScriptLayout is whether a script text is a sequence of plain simple commands, each on a line of its own: every
// statement is a call with no assignment before it, no redirection, no background, coprocess or negation, no trailing ;, and
// only words of literal text (no parameter, command, arithmetic or process expansion); a list, pipe, declaration (export,
// declare), loop, conditional, function, group or subshell is not a statement of that kind. The execution records of the reader
// cannot show the assignments of a declaration or the separators of a line, so the rule reads them in the text. badLine is the line
// of the first statement that is not of that kind.
func githubPostScriptLayout(src string) (badLine int, ok bool) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "")
	if err != nil {
		return 1, false
	}
	last := uint(0)
	for _, st := range file.Stmts {
		line := int(st.Pos().Line())
		call, ok := st.Cmd.(*syntax.CallExpr)
		if !ok || len(call.Assigns) > 0 || len(call.Args) == 0 || len(st.Redirs) > 0 ||
			st.Background || st.Coprocess || st.Negated || st.Semicolon.IsValid() {
			return line, false
		}
		if st.Pos().Line() <= last {
			return line, false
		}
		last = st.End().Line()
		for _, w := range call.Args {
			if !githubPostLiteralWord(w) {
				return line, false
			}
		}
	}
	return 0, true
}

// githubPostLiteralWord is whether a word is made of literal text and quotes only.
func githubPostLiteralWord(w *syntax.Word) bool {
	for _, p := range w.Parts {
		switch x := p.(type) {
		case *syntax.Lit, *syntax.SglQuoted:
		case *syntax.DblQuoted:
			if x.Dollar {
				return false
			}
			for _, q := range x.Parts {
				if _, ok := q.(*syntax.Lit); !ok {
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

// githubPostPlainContext is whether a context is the top level of the text: no wrapper, shell, list,
// pipe, substitution, loop, function or subshell around the command.
func githubPostPlainContext(c shellir.Context) bool {
	return !c.Conditional && !c.Background && !c.Coprocess && !c.Subshell && !c.Pipeline &&
		!c.Loop && !c.FuncBody && !c.CmdSubst && !c.ProcSubst && c.Carrier == "" && c.Stdin == shellir.StdinNone
}

// githubPostJudgeScript reads the file a shell or a sed or awk program runs and judges its text.
func githubPostJudgeScript(e shellir.Exec, depth int, writes *githubPostWrites) (githubPostSite, bool) {
	unread := githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, line: 0}
	if depth >= githubPostMaxScriptDepth {
		unread.reason = "scripts run scripts too deeply"
		return unread, true
	}
	if !e.Script.Known {
		unread.reason = "script file name is not known"
		return unread, true
	}
	base := e.Dir.Path
	cwd := base
	if !e.Dir.Known {
		base = githubPostNoDir
		cwd = ""
	}
	body, ok := githubPostReadScript(e.Script.Value, base)
	if !ok {
		unread.reason = githubPostScriptReason("script file cannot be read", e.Script.Value)
		return unread, true
	}
	switch e.Name {
	case "sed", "awk", "gawk", "mawk", "nawk":
		if githubPostInlineNamesPost(body) {
			unread.post = true
			return unread, true
		}
		return githubPostSite{}, false
	}
	return githubPostLocate(githubPostJudgeTextDepth(body, cwd, depth+1, writes, e.Cdpath))(e.Script.Value)
}

// githubPostLocate turns a refusal found inside a script into script-file:line, once: a refusal that already has a place (an
// inner script's, or a file a post reads) keeps it.
func githubPostLocate(site githubPostSite, denied bool) func(script string) (githubPostSite, bool) {
	return func(script string) (githubPostSite, bool) {
		if denied && site.place == githubPostWhereCommand && site.line > 0 {
			site.place = script + ":" + strconv.Itoa(site.line)
		}
		site.line = 0
		return site, denied
	}
}

// githubPostInlineNamesPost is whether an interpreter's program text names a gh post. The program may run
// that post through a shell or a system call, so a text that names one is refused. The text is judged as the shell reads its
// words: quotes and backslashes are deleted first, so a name built from quoted pieces (g""h) is the name it spells.
func githubPostInlineNamesPost(src string) bool {
	lower := strings.ToLower(strings.NewReplacer("\"", "", "'", "", "\\", "").Replace(src))
	if !strings.Contains(lower, "gh") {
		return false
	}
	for _, verb := range []string{"comment", "create", "edit", "review", "api", "release", "pr", "issue"} {
		if strings.Contains(lower, verb) {
			return true
		}
	}
	return false
}

// githubPostWordSplit splits a text into shell words, roughly: at blanks, operators, substitutions, redirections and assignments.
var githubPostWordSplit = regexp.MustCompile("[\\s;&|()<>`$={},]+")

// githubPostSpellsPost is whether a text spells a gh command, for the wording of a refusal only (never a decision): a shell word,
// quotes and backslashes deleted first (so g""h is gh), that is gh or a path whose last component is gh (/usr/bin/gh, ./gh). The
// letters gh inside a word (high_priority, tests/ghost) or a file named after it (missing/gh.sh, tests/gh.py, the module tests.gh)
// are no gh command. The decision a text gets is made by the rules above; this says only whether the refusal is about a post.
func githubPostSpellsPost(src string) bool {
	lower := strings.ToLower(strings.NewReplacer("\"", "", "'", "", "\\", "").Replace(src))
	for _, word := range githubPostWordSplit.Split(lower, -1) {
		if word == "gh" || strings.HasSuffix(word, "/gh") {
			return true
		}
	}
	return false
}

// githubPostReadScript reads a script file of at most 1 MiB; a path that is not a regular file is refused.
func githubPostReadScript(name, cwd string) (string, bool) {
	file, ok := githubPostRegularFile(githubPostScriptPath(name, cwd))
	if !ok {
		return "", false
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, githubPostMaxFileBytes+1))
	if err != nil || len(b) > githubPostMaxFileBytes {
		return "", false
	}
	return string(b), true
}

// githubPostJudgeWords is the rule for one gh command whose words the reader gave.
func githubPostJudgeWords(words []string, cwd githubPostDir) (githubPostSite, bool) {
	expansion := githubPostSite{rule: githubPostRuleExpand, place: githubPostWhereCommand, line: 0}
	unread := githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, line: 0}
	site, denied, handled := githubPostForm(words, cwd)
	if handled && denied {
		return site, true
	}
	if !handled {
		if site, denied := githubPostUnknownGh(words); denied {
			return site, true
		}
		if githubPostPostSub(words) {
			if githubPostHasMark(words) {
				return expansion, true
			}
			return unread, true
		}
	}
	if githubPostHasMark(words) && githubPostBroadSub(words) {
		return expansion, true
	}
	return githubPostSite{}, false
}

// githubPostBroadSub is whether a gh command is a pr, issue or api command, whose words may carry text.
func githubPostBroadSub(words []string) bool {
	if len(words) < 2 {
		return false
	}
	switch githubPostProgram(words[1]) {
	case "pr", "issue", "api":
		return true
	}
	return false
}

// githubPostPostSub is whether a gh command is a post or an API call, whose words may carry text.
func githubPostPostSub(words []string) bool {
	if len(words) < 2 {
		return false
	}
	switch githubPostProgram(words[1]) {
	case "api":
		return true
	case "pr", "issue":
		if len(words) < 3 {
			return false
		}
		switch githubPostProgram(words[2]) {
		case "comment", "create", "edit", "review", "new":
			return true
		}
	case "release":
		if len(words) < 3 {
			return false
		}
		switch githubPostProgram(words[2]) {
		case "list", "view", "download":
			return false
		}
		return true
	}
	return false
}

func githubPostHasMark(words []string) bool {
	for _, w := range words {
		if strings.Contains(w, githubPostUnknownMark) {
			return true
		}
	}
	return false
}

// githubPostProgram is a program or subcommand word compared by its last path element in ASCII lower case,
// so /usr/bin/gh, ./gh and GH name the same program.
func githubPostProgram(word string) string {
	return strings.ToLower(filepath.Base(word))
}

// githubPostRunner reports a program that runs its arguments as a command the text does not show.
func githubPostRunner(name string) bool {
	switch name {
	case "ssh", "watch", "script", "tmux", "screen", "expect", "flock", "chroot", "unshare",
		"nsenter", "setpriv", "runuser", "taskset", "chrt", "strace", "ltrace":
		return true
	}
	return false
}

// githubPostRunnerNamesPost is whether a runner's arguments hold an unknown word or name a post.
func githubPostRunnerNamesPost(e shellir.Exec) bool {
	if !githubPostRunner(e.Name) {
		return false
	}
	parts := make([]string, 0, len(e.Args))
	for _, a := range e.Args {
		if a.Known {
			parts = append(parts, a.Value)
		} else {
			parts = append(parts, githubPostUnknownMark)
		}
	}
	joined := strings.Join(parts, " ")
	return strings.Contains(joined, githubPostUnknownMark) || githubPostInlineNamesPost(joined)
}

// githubPostDirectPath is whether an exec runs a file by a path with a slash (./post.sh, scripts/post.sh, ../gh, /tmp/post.sh): the
// file is read, whatever its directory. A binary (an installed gh, env, git) is not a script and is not judged; a script that is
// not a binary is judged as one the shell runs. The directory the command runs in is not needed for an absolute path.
func githubPostDirectPath(e shellir.Exec) bool {
	return e.Kind == shellir.KindCommand && e.Inline == nil && e.Program.Known && strings.Contains(e.Program.Value, "/")
}

// githubPostScriptKnown is whether the file a script name opens is known: its directory is, or the name is absolute.
func githubPostScriptKnown(name string, dir shellir.Dir) bool {
	return dir.Known || filepath.IsAbs(name)
}

// githubPostJudgeDirect reads the file a command runs by a path, as a script the shell runs: a file that is missing, not
// a regular file, or in a directory the reader does not know is unreadable. A binary (no #! line and a NUL in the first 4 KiB) is not
// a script and is not judged, whatever its size; a script over 1 MiB is unreadable. A script with a shell shebang, or none (the shell
// runs it), is judged as shell text; a script of another interpreter that names a post is refused (its lines cannot satisfy the line
// rule).
func githubPostJudgeDirect(e shellir.Exec, depth int, writes *githubPostWrites) (githubPostSite, bool) {
	unread := githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, line: 0}
	if depth >= githubPostMaxScriptDepth {
		unread.reason = "scripts run scripts too deeply"
		return unread, true
	}
	if !githubPostScriptKnown(e.Program.Value, e.Dir) {
		unread.reason = githubPostScriptReason("script file is in a directory that is not known", e.Program.Value)
		return unread, true
	}
	cwd := e.Dir.Path
	if !e.Dir.Known {
		cwd = ""
	}
	body, kind := githubPostReadDirect(e.Program.Value, cwd)
	switch kind {
	case githubPostFileUnreadable:
		unread.reason = githubPostScriptReason("script file cannot be read", e.Program.Value)
		return unread, true
	case githubPostFileBinary, githubPostFileAbsent:
		return githubPostSite{}, false
	case githubPostFileShell:
		return githubPostLocate(githubPostJudgeTextDepth(body, cwd, depth+1, writes, e.Cdpath))(e.Program.Value)
	}
	if githubPostScriptMentionsPost(body) {
		return unread, true
	}
	return githubPostSite{}, false
}

// githubPostScriptReason is the reader's reason for a script file it cannot read: what failed and the file's name as the command
// spells it, bounded.
func githubPostScriptReason(what, name string) string {
	return unreadableLabel(what + " (" + name + ")")
}

// githubPostFileKind is what a file run by path holds.
type githubPostFileKind int

const (
	githubPostFileUnreadable githubPostFileKind = iota // missing, not a regular file, or a script over 1 MiB
	githubPostFileBinary                               // no #! line and a NUL in the first 4 KiB
	githubPostFileShell                                // a #! line naming a shell, or none: the shell runs the text
	githubPostFileOther                                // a #! line naming another interpreter
	githubPostFileAbsent                               // an absolute path with no file: nothing runs, it is not a script
)

// githubPostDirectHead is how much of a file run by path decides whether it is a binary.
const githubPostDirectHead = 4096

// githubPostReadDirect opens a file run by a relative path once and reads its head: a file whose head has no #! line and a NUL is a
// binary and is read no further, so an executable of any size is not refused; any other file is a script, read whole up to 1 MiB.
func githubPostReadDirect(name, dir string) (string, githubPostFileKind) {
	path := githubPostScriptPath(name, dir)
	if filepath.IsAbs(name) {
		// An installed program is often a link (/usr/bin/python3, /etc/alternatives): the file it names is the one that runs.
		if r, err := filepath.EvalSymlinks(path); err == nil {
			path = r
		}
	}
	file, ok := githubPostRegularFile(path)
	if !ok {
		if filepath.IsAbs(name) {
			if _, err := os.Lstat(path); os.IsNotExist(err) {
				return "", githubPostFileAbsent
			}
		}
		return "", githubPostFileUnreadable
	}
	defer file.Close()
	head := make([]byte, githubPostDirectHead)
	n, err := io.ReadFull(file, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", githubPostFileUnreadable
	}
	head = head[:n]
	if !bytes.HasPrefix(head, []byte("#!")) && bytes.IndexByte(head, 0) >= 0 {
		return "", githubPostFileBinary
	}
	rest, err := io.ReadAll(io.LimitReader(file, int64(githubPostMaxFileBytes-n)+1))
	if err != nil || n+len(rest) > githubPostMaxFileBytes {
		return "", githubPostFileUnreadable
	}
	body := string(head) + string(rest)
	if !strings.HasPrefix(body, "#!") {
		return body, githubPostFileShell
	}
	line, _, _ := strings.Cut(body, "\n")
	words := strings.Fields(strings.TrimPrefix(line, "#!"))
	interp := ""
	if len(words) > 0 {
		interp = filepath.Base(words[0])
		rest := words[1:]
		if interp == "env" {
			interp, rest = "", nil
			for j, w := range words[1:] {
				if !strings.HasPrefix(w, "-") {
					interp, rest = filepath.Base(w), words[2+j:]
					break
				}
			}
		}
		if interp == "busybox" && len(rest) > 0 {
			// busybox runs the applet its first operand names: #!/bin/busybox ash is the shell ash.
			interp = rest[0]
		}
	}
	if githubPostShellName(interp) {
		return body, githubPostFileShell
	}
	return body, githubPostFileOther
}

// githubPostShellName is whether a program name is a POSIX or Korn family shell that reads a script file as shell text: the
// reader's own list, so a shell the pipe rule refuses is a shell here as well (hush, pdksh, oksh, posh, yash, rbash included).
func githubPostShellName(name string) bool { return shellir.IsShell(name) }

// githubPostScriptMentionsPost is whether the text of a script of another interpreter spells a gh command (gh pr, gh api, ...). It is
// narrower than githubPostInlineNamesPost, which a program on the command line is held to: a script file is long, and a bare gh
// and pr inside other words must not refuse every node shim in the directory.
func githubPostScriptMentionsPost(body string) bool {
	re := regexp.MustCompile(`(?:^|[^A-Za-z0-9_./-])gh\s+(?:pr|issue|api|release|repo|gist|workflow|run|label|project|extension|auth|alias|secret|ruleset|cache|codespace)\b`)
	return re.MatchString(body)
}

// githubPostWrites is the set of files the records of a text write, computed once per text: a text of thousands of script runs
// must not scan its records for each of them.
type githubPostWrites struct {
	files    map[string]bool
	existing map[int64][]os.FileInfo // the files a write reaches that exist already, by size: a hard link has another name and the same file
	trees    []string                // paths below which a write may land (a copied directory)
	unknown  bool                    // some write of the text's own records has a destination the reader cannot name
	// hooked is whether a program ran that configuration, not the text, may make run (git: a hook, a diff or textconv driver,
	// core.fsmonitor; rg: a --pre in its configuration): it may have written any file, so it reaches the files a post reads. It says
	// nothing of a script the text runs, which a program of the repository does not rewrite (see unknown).
	hooked bool
	// unknownBodies holds the scripts (githubPostBodyKey) whose bodies write a destination the reader cannot name, or whose writes
	// cannot be computed: an unknown write for every other script, not for the script itself (its own unknown write says nothing of
	// its own file, as a known write to it does).
	unknownBodies map[string]bool
	outer         *githubPostWrites // the writes of the text that runs this one: they happened before it
	// self is set on the view of the outer writes a script body is judged against: the script whose body it is, whose own unknown
	// writes are the body's and are counted in the body's own writes.
	self string
}

// as is the view of these writes that the body of the script key is judged against.
func (w *githubPostWrites) as(key string) *githubPostWrites {
	v := *w
	v.self = key
	return &v
}

// add records one destination by the name the kernel gives it and, when the file exists, by the file itself.
func (w *githubPostWrites) add(p string) {
	w.files[githubPostIdentity(p)] = true
	if fi, err := os.Stat(p); err == nil {
		w.existing[fi.Size()] = append(w.existing[fi.Size()], fi)
	}
}

// reaches is whether a write lands on the file the path names, whatever name the write used.
func (w *githubPostWrites) reaches(p string) bool {
	if w.files[githubPostIdentity(p)] {
		return true
	}
	fi, err := os.Stat(p)
	if err != nil {
		return false
	}
	for _, other := range w.existing[fi.Size()] {
		if os.SameFile(fi, other) {
			return true
		}
	}
	return false
}

// githubPostWritesOf collects the destinations of every record of a text, by the identity the kernel gives them (links resolved).
// A link made in the text (ln) can alias any file, so it counts as a write to a name unknown. A script file a shell runs or sources,
// and a shell script run by path, write what their bodies write, however deep: those writes happen before the scripts that run after
// them, in this text and in the texts around it (bash writer.sh; bash post.sh). A body whose writes cannot be computed (unreadable,
// over the limit, deeper than githubPostMaxScriptDepth) writes a file unknown. A binary or a script of another interpreter is a
// program like any installed one: its writes are not read.
//
// A body's writes happen when its script runs: they reach the executions after it (and the script itself, which a shell reads as it
// runs: githubPostTextWrites.stale), not the ones that ran before (bash lint.sh; bash refresh.sh, where refresh.sh rewrites lint.sh, ran the lint.sh read here).
// The text's own records are taken whole, wherever they sit. Where the text's order is not the order things run (githubPostInOrder:
// a loop runs its body again, a pipe or a background job runs alongside, a function body runs where it is called, a carried text that is not a shell's -c
// string may be run again, and two or more substitutions of one simple command are unordered), a body's writes reach every execution, and an execution there sees every body's writes.
func githubPostWritesOf(execs []shellir.Exec, outer *githubPostWrites) *githubPostTextWrites {
	t := &githubPostTextWrites{execs: execs, ordered: newGithubPostWrites(outer), bodies: make([]githubPostBody, len(execs))}
	t.ordered.collect(execs, 0, map[string]*githubPostWrites{}, func(i int, key string, o *githubPostWrites) {
		t.bodies[i] = githubPostBody{key: key, writes: o}
	})
	for i, e := range execs {
		if t.bodies[i].writes == nil && githubPostRunsConfigured(e) {
			// A program that configuration makes run code writes, from the point it runs, wherever the files are.
			t.bodies[i] = githubPostBody{writes: &githubPostWrites{hooked: true}}
		}
		if !githubPostInOrder(e.Ctx) {
			t.ordered.merge(t.bodies[i].key, t.bodies[i].writes)
			t.bodies[i] = githubPostBody{}
		}
	}
	return t
}

// githubPostRunsConfigured is whether a record runs a program whose code the repository or the environment configures: git (hooks,
// diff and textconv drivers, core.fsmonitor, core.pager) and rg (a --pre in its configuration file).
func githubPostRunsConfigured(e shellir.Exec) bool {
	return e.Kind == shellir.KindCommand && (e.Name == "git" || e.Name == "rg")
}

// githubPostTextWrites is the writes of one text, viewed from each of its executions in turn.
type githubPostTextWrites struct {
	execs []shellir.Exec
	// ordered holds the own records' writes, the writes of the bodies out of order, and the bodies of the executions up to next.
	ordered *githubPostWrites
	next    int
	full    *githubPostWrites // every write of the text, built when an execution out of order asks
	bodies  []githubPostBody  // by execution: the body that execution runs, until it is merged
}

// githubPostBody is the writes of the script body one execution runs (nil: none, or a program whose writes are not read).
type githubPostBody struct {
	key    string
	writes *githubPostWrites
}

// at is the view of the writes made before execution i runs: the writes around the body that execution runs, whose own writes happen
// in their order inside it. The views are asked in the order of the executions; one asked earlier must not be held across a later ask.
func (t *githubPostTextWrites) at(i int) *githubPostWrites {
	if !githubPostInOrder(t.execs[i].Ctx) {
		if t.full == nil {
			t.full = t.ordered.clone()
			for _, b := range t.bodies[t.next:] {
				t.full.merge(b.key, b.writes)
			}
		}
		return t.full
	}
	for ; t.next < i; t.next++ {
		b := t.bodies[t.next]
		t.ordered.merge(b.key, b.writes)
	}
	return t.ordered
}

// stale is whether the script execution i runs is not the file read before the command: the writes before it reach it, or its own
// body writes it (a shell reads a script as it runs it, so what the script appends to itself runs too).
func (t *githubPostTextWrites) stale(i int, script string, dir shellir.Dir) bool {
	return t.staleIn(i, script, dir, true)
}

// staleIn is stale; with strict false a write to a destination the reader cannot name does not count, only a write that names the
// file (or a tree it lies in).
func (t *githubPostTextWrites) staleIn(i int, script string, dir shellir.Dir, strict bool) bool {
	if t.at(i).rewritesIn(script, dir, strict) {
		return true
	}
	own := t.bodies[i].writes
	if own == nil || !githubPostScriptKnown(script, dir) {
		return false
	}
	if !dir.Known {
		dir.Path = ""
	}
	p := githubPostScriptPath(script, dir.Path)
	return own.reaches(p) || own.underTree(p)
}

// githubPostDirectStale is stale for a file run by path. An installed binary named by its absolute path (/usr/bin/git, a Go
// toolchain) is not a script, and only a write that names it makes it another file than the one that runs; a script, a missing
// relative file or a binary named relatively is not the file read when any write of the text may reach it.
func githubPostDirectStale(t *githubPostTextWrites, i int, e shellir.Exec) bool {
	if filepath.IsAbs(e.Program.Value) {
		cwd := e.Dir.Path
		if !e.Dir.Known {
			cwd = ""
		}
		if _, kind := githubPostReadDirect(e.Program.Value, cwd); kind == githubPostFileBinary || kind == githubPostFileAbsent {
			return t.staleIn(i, e.Program.Value, e.Dir, false)
		}
	}
	return t.stale(i, e.Program.Value, e.Dir)
}

// githubPostInOrder is whether an execution runs once, where the text puts it, after the executions before it and before the
// executions after it finish. A shell's -c string and a command substitution run once where the text puts them, so they are in order,
// except that the substitutions of a simple command are unordered when there are two or more (Context.Unsequenced).
func githubPostInOrder(c shellir.Context) bool {
	return !c.Loop && !c.FuncBody && !c.Background && !c.Coprocess && !c.Pipeline && !c.ProcSubst && !c.Repeat && !c.Unsequenced
}

// clone is a copy of these writes that later merges into either do not reach the other.
func (w *githubPostWrites) clone() *githubPostWrites {
	c := *w
	c.files = make(map[string]bool, len(w.files))
	for k, v := range w.files {
		c.files[k] = v
	}
	c.existing = make(map[int64][]os.FileInfo, len(w.existing))
	for k, v := range w.existing {
		c.existing[k] = append([]os.FileInfo(nil), v...)
	}
	c.unknownBodies = make(map[string]bool, len(w.unknownBodies))
	for k, v := range w.unknownBodies {
		c.unknownBodies[k] = v
	}
	c.trees = append([]string(nil), w.trees...)
	return &c
}

func newGithubPostWrites(outer *githubPostWrites) *githubPostWrites {
	return &githubPostWrites{files: map[string]bool{}, existing: map[int64][]os.FileInfo{}, unknownBodies: map[string]bool{}, outer: outer}
}

// githubPostBodyKey names a script a record runs: the file by the identity the kernel gives it, and the directory it runs in.
func githubPostBodyKey(script string, dir shellir.Dir) string {
	if !githubPostScriptKnown(script, dir) {
		return ""
	}
	if !dir.Known {
		dir.Path = ""
	}
	return githubPostIdentity(githubPostScriptPath(script, dir.Path)) + "\x00" + dir.Path
}

// collect adds the writes of the records of one text; depth counts the script bodies around it, memo holds the writes of each body
// already read (by script identity, directory and whether it runs by path or by a shell), so a text that runs one script many times
// reads it once. body, when set, receives the writes of the body each record runs (by record index) instead of their merge here.
func (w *githubPostWrites) collect(execs []shellir.Exec, depth int, memo map[string]*githubPostWrites, body func(int, string, *githubPostWrites)) {
	// A text read whole (a script body that another text runs) writes through a configured program wherever it sits; a text read in
	// order (body set) records it at the point the program runs (githubPostWritesOf).
	whole := body == nil
	if whole {
		body = func(_ int, key string, o *githubPostWrites) { w.merge(key, o) }
	}
	for i, o := range execs {
		if whole && githubPostRunsConfigured(o) {
			w.hooked = true
		}
		if o.Kind == shellir.KindScriptFile {
			switch o.Name {
			case "sed", "awk", "gawk", "mawk", "nawk":
				// a sed or awk program file is not shell text
			default:
				body(i, githubPostBodyKey(o.Script.Value, o.Dir), githubPostBodyWrites(o.Script, o.Dir, o.Cdpath, depth, memo, false))
			}
			continue
		}
		if githubPostDirectPath(o) {
			body(i, githubPostBodyKey(o.Program.Value, o.Dir), githubPostBodyWrites(o.Program, o.Dir, o.Cdpath, depth, memo, true))
		}
		if o.Name == "ln" {
			w.unknown = true
		}
		// A copy, move or link into a directory writes a file below it that no destination of the record names.
		copied, trees := shellir.CopiedPaths(o)
		for _, c := range copied {
			if p, ok := githubPostWritePath(c, o.Dir); ok {
				w.add(p)
			} else {
				w.unknown = true
			}
		}
		for _, t := range trees {
			if p, ok := githubPostWritePath(t, o.Dir); ok {
				w.trees = append(w.trees, githubPostIdentity(p))
			} else {
				w.unknown = true
			}
		}
		for _, d := range shellIRExecDests(o) {
			switch {
			case d == shellIRUnknownDest:
				w.unknown = true
			case d == "/dev/null":
			case filepath.IsAbs(d):
				w.add(d)
			case !o.Dir.Known:
				w.unknown = true
			default:
				w.add(githubPostScriptPath(d, o.Dir.Path))
			}
		}
	}
}

// githubPostBodyWrites is the writes of the body of a script a shell runs (direct: a file run by path, which may be a binary or a
// script of another interpreter, whose writes are not read). A body the reader cannot read writes a file unknown.
func githubPostBodyWrites(script shellir.Word, dir shellir.Dir, cdpath bool, depth int, memo map[string]*githubPostWrites, direct bool) *githubPostWrites {
	unknown := &githubPostWrites{unknown: true}
	if !script.Known || !githubPostScriptKnown(script.Value, dir) || depth >= githubPostMaxScriptDepth {
		return unknown
	}
	cwd := dir.Path
	if !dir.Known {
		cwd = ""
	}
	// The memo key holds how the file runs as well: run by path, a #! line of another program makes the file that program's input
	// (no writes read); run by a shell (bash f, source f), every line is shell text and the #! line a comment. One reading must not
	// stand for the other (./writer.sh; bash writer.sh; bash post.sh).
	key := githubPostBodyKey(script.Value, dir) + "\x00shell"
	if direct {
		key = githubPostBodyKey(script.Value, dir) + "\x00direct"
	}
	if cdpath {
		key += "\x00cdpath" // a cd to a bare name in the body is not a directory the guard knows
	}
	if got, ok := memo[key]; ok {
		return got
	}
	memo[key] = unknown // a script that runs itself is not read again
	var body string
	if direct {
		var kind githubPostFileKind
		body, kind = githubPostReadDirect(script.Value, cwd)
		switch kind {
		case githubPostFileBinary, githubPostFileOther, githubPostFileAbsent:
			memo[key] = nil
			return nil
		case githubPostFileUnreadable:
			return unknown
		}
	} else {
		var ok bool
		if body, ok = githubPostReadScript(script.Value, cwd); !ok {
			return unknown
		}
	}
	res, err := shellir.AnalyzeScript(body, cwd, cdpath)
	if err != nil {
		return unknown
	}
	got := newGithubPostWrites(nil)
	got.collect(res.Execs, depth+1, memo, nil)
	memo[key] = got
	return got
}

// merge adds the writes of the body of the script key to the writes of the text that runs it. An unknown write anywhere in the body
// (its own records, or a script it runs) is recorded under key: it counts for every other script the text runs.
func (w *githubPostWrites) merge(key string, o *githubPostWrites) {
	if o == nil {
		return
	}
	if o.unknown || len(o.unknownBodies) > 0 {
		w.unknownBodies[key] = true
	}
	if o.hooked {
		w.hooked = true
	}
	for f := range o.files {
		w.files[f] = true
	}
	for size, fis := range o.existing {
		w.existing[size] = append(w.existing[size], fis...)
	}
	w.trees = append(w.trees, o.trees...)
}

// githubPostIdentity is the name the kernel gives a path: its links resolved through the last component when it exists, through
// its directory when it does not. The path is resolved as written (failure class 5).
func githubPostIdentity(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir, base := filepath.Split(p)
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Join(r, base)
	}
	return filepath.Clean(p)
}

// readsReach is whether a write of these writes, or of the texts around them, may land on one of the files a post read: a write
// to the file by any name or link, to a directory it lies in (a copied tree), or to a destination the reader cannot name.
func (w *githubPostWrites) readsReach(files []string) bool {
	for x := w; x != nil; x = x.outer {
		if x.unknown || x.hooked || len(x.unknownBodies) > 0 {
			return true
		}
		for _, p := range files {
			if x.reaches(p) || x.underTree(p) {
				return true
			}
		}
	}
	return false
}

// rewritesIn is whether the text itself writes the script file it runs (cp evil.sh post.sh && bash post.sh): the file the guard reads
// before the command is then not the file that runs. A write to a destination the reader cannot name is taken as a write to the
// script.
//
// The writes of the script bodies the text runs count as well (bash writer.sh; bash post.sh): a known write wherever it is, an unknown
// one for every script but the one whose body makes it. In a body judged inside another, the writes around it are walked with the
// script of each level excluded the same way. With strict false only a write that names the script (or a tree it lies in) counts.
func (w *githubPostWrites) rewritesIn(script string, dir shellir.Dir, strict bool) bool {
	if !githubPostScriptKnown(script, dir) {
		return false // the script is unreadable already
	}
	if !dir.Known {
		dir.Path = ""
	}
	p := githubPostScriptPath(script, dir.Path)
	self := githubPostBodyKey(script, dir)
	for x := w; x != nil; x = x.outer {
		if x != w {
			self = x.self
		}
		if x.reaches(p) || x.underTree(p) || strict && x.unknown {
			return true
		}
		for key := range x.unknownBodies {
			if strict && key != self {
				return true
			}
		}
	}
	return false
}

// underTree is whether the path lies at or below a path a copied directory may fill.
func (w *githubPostWrites) underTree(p string) bool {
	id := githubPostIdentity(p)
	for _, t := range w.trees {
		if id == t || strings.HasPrefix(id, strings.TrimSuffix(t, "/")+"/") {
			return true
		}
	}
	return false
}

// githubPostWritePath is the path a destination of a record names: as written when absolute, else joined to the record's
// directory; a relative one in a directory the reader does not know has no path.
func githubPostWritePath(d string, dir shellir.Dir) (string, bool) {
	switch {
	case filepath.IsAbs(d):
		return d, true
	case !dir.Known:
		return "", false
	}
	return githubPostScriptPath(d, dir.Path), true
}

// githubPostScriptPath is the path a shell opens for a script name in a directory: the name as written, joined to the directory
// with no lexical clean. A .. after a link steps up from the link's target, which the kernel resolves when the guard opens the
// path (failure class 5); cleaning first would read another file than the one that runs.
func githubPostScriptPath(name, cwd string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return strings.TrimSuffix(cwd, "/") + "/" + name
}
