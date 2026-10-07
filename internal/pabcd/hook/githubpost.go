package hook

// CRW-783: CRW's own protection, no CXC oracle. The PreToolUse guard over the shell tools allows a
// GitHub post only when the command is one simple command of literal words in the one form the guard
// reads whole (the closed rule), and refuses every other text that names a post. The deny reason names
// only the rule and the place, never a value.
//
// The program word is read the way the shell builds it (quote removal and backslash removal), compared
// by its last path element and in ASCII lower case (a case-insensitive file system runs gh for GH), so
// /usr/bin/gh, ./gh, g''h, 'g'h and GH are the same word as gh; the form checks, the mention test, the
// unknown-gh check and the read-and-record exception all call that one function. A program word that
// still holds an expansion after that (a variable, a substitution, a backtick, a glob) names a command
// the guard cannot judge, so a text that names a post through it is refused rather than passed.
//
// A text that is not one simple command is read through its canonical words: every quote character and
// every backslash removed from the whole text, the ASCII letters lowered, then split on whitespace. The
// canonical words name a post when a pr or issue word is followed by a word outside the read list, when
// an api word sits with a body or method flag, or when a release word sits with a text flag, whether or
// not gh appears; that is what refuses a program word built in quote pieces or one quoting level down.

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

const (
	githubPostWhereCommand = "command"
	// githubPostWhereTitle is the place a title value carries a secret: the value is in the command.
	githubPostWhereTitle = "title"

	githubPostRuleInline = "inline-github-body"
	githubPostRuleExpand = "expansion-in-github-text"
	githubPostRuleSecret = "secret-in-github-text"
	githubPostRuleUnread = "unreadable-github-post"

	githubPostMaxFileBytes = 1 << 20 // the most the guard reads of a body file (1 MiB)
	// GitHubPostMaxStdinBytes is this row's own stdin bound (1 MiB): over it the guard refuses rather
	// than passing an input it could not judge.
	GitHubPostMaxStdinBytes = 1 << 20
)

// GitHubPostAnswer is the component row's whole input policy: the guard's answer for the payload, or the
// deny envelope when the payload is over the bound. A read that fails reads as empty input.
func GitHubPostAnswer(in io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(in, GitHubPostMaxStdinBytes+1))
	if err != nil {
		return ""
	}
	if len(b) > GitHubPostMaxStdinBytes {
		return githubPostDeny(githubPostRuleUnread, githubPostWhereCommand)
	}
	return HandleGitHubPostGuard(string(b))
}

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
	return githubPostDeny(site.rule, site.place)
}

// githubPostDeny is the deny envelope: the rule, the place, and one way forward.
func githubPostDeny(rule, place string) string {
	reason := "GitHub post blocked (" + rule + ") at " + place +
		": write the text to a file, check it, and pass it with --body-file, -F body=@file or --input"
	return editAnswer("deny", reason, reason)
}

// githubPostSite is why and where a post is denied.
type githubPostSite struct{ rule, place string }

// githubPostArgv is tool_input's command or cmd as an already-split argv array.
func githubPostArgv(input map[string]any) ([]string, bool) {
	for _, key := range [...]string{"command", "cmd"} {
		items, ok := input[key].([]any)
		if !ok {
			continue
		}
		words := make([]string, 0, len(items))
		for _, item := range items {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			words = append(words, s)
		}
		return words, true
	}
	return nil, false
}

// githubPostCommand is tool_input's command or cmd as shell text.
func githubPostCommand(input map[string]any) (string, bool) {
	for _, key := range [...]string{"command", "cmd"} {
		if shell, ok := input[key].(string); ok {
			return shell, true
		}
	}
	return "", false
}

// githubPostJudgeText is the whole rule for one shell text: a text that is one simple command is judged
// as that command, and any other text that names a post is refused.
func githubPostJudgeText(command, cwd string) (githubPostSite, bool) {
	if words, simple := githubPostSimple(command); simple {
		return githubPostJudgeWords(words, cwd)
	}
	return githubPostUnread(command, cwd)
}

// githubPostJudgeArgv judges an already-split argv array: it is a simple command when every word is
// literal, and otherwise the text it spells is refused when it names a post.
func githubPostJudgeArgv(words []string, cwd string) (githubPostSite, bool) {
	for _, w := range words {
		if !githubPostLiteralWord(w) {
			return githubPostUnread(strings.Join(words, " "), cwd)
		}
	}
	return githubPostJudgeWords(words, cwd)
}

// githubPostJudgeWords is the rule for one simple command of literal words: the one allowed post form,
// the read-and-record exception, a gh command the guard cannot place, or a text that names a post.
func githubPostJudgeWords(words []string, cwd string) (githubPostSite, bool) {
	if site, denied, handled := githubPostForm(words, cwd); handled {
		return site, denied
	}
	if site, denied := githubPostScriptCommand(words, cwd, true); denied {
		return site, true
	}
	if githubPostQuiet(words) {
		return githubPostSite{}, false
	}
	if site, denied := githubPostUnknownGh(words); denied {
		return site, true
	}
	// A command word that still holds an expansion cannot be judged, and a command whose text names a
	// post in any spelling is refused: the mention test reads the canonical words, so the program is
	// normalised and lowered first and a path, quote pieces or a mixed case are all seen.
	if githubPostMentions(strings.Join(words, " ")) {
		return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
	}
	return githubPostSite{}, false
}

// githubPostUnread is the fail-closed judgement for a text that is not one simple command: rule 2's
// canonical words are the gate, so a text that names a post is refused whatever its quoting depth, its
// case or its number of commands, and a text that names no post is not a target.
func githubPostUnread(command, cwd string) (githubPostSite, bool) {
	// A text that spells an inline body is the inline-body rule, wherever the body sits.
	if githubPostInlineShape(command) {
		return githubPostSite{githubPostRuleInline, githubPostWhereCommand}, true
	}
	// A text that holds an outer-shell expansion and mentions a gh post in any spelling keeps the
	// generation-6 refusal: the expansion may build the post the guard cannot read.
	if githubPostExpands(command) && githubPostMentionsBroad(command) {
		return githubPostSite{githubPostRuleExpand, githubPostWhereCommand}, true
	}
	// The file rule reads each command of a list through the same decomposition the write reader uses, so a
	// wrapper prefix or a list separator no longer hides a shell that runs a posting script file.
	if site, denied := githubPostScriptList(command, cwd); denied {
		return site, true
	}
	if !githubPostCanonicalNamesPost(githubPostCanonicalWords(command)) {
		// A command word that still holds an expansion after quote removal cannot be judged, and the rest
		// of its words name a post verb: rule 1's program identity refuses that command word.
		if words := githubPostSplitWords(command); len(words) > 0 &&
			githubPostHoldsExpansion(githubPostNormal(words[0])) && githubPostNamesPost(words[1:]) {
			return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
		}
		return githubPostSite{}, false
	}
	return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
}

// githubPostSimple is rule 1: the whole text, trimmed, is one simple command whose every word is
// literal. A newline, ;, &, |, grouping, a substitution, a redirection, an assignment word before the
// program or a non-literal word makes it something else.
func githubPostSimple(command string) ([]string, bool) {
	s := text.Trim(command)
	if s == "" || strings.ContainsAny(s, "\n;&|") {
		return nil, false
	}
	words, ok := githubPostWords(s)
	if !ok || len(words) == 0 {
		return nil, false
	}
	if githubPostAssignmentWord(words[0]) {
		return nil, false
	}
	return words, true
}

// githubPostWords splits one simple command into its words, keeping each word as written, and reports
// false when a word or the text as a whole is not literal: an unquoted $, backtick, backslash, *, ?, [,
// ], ~, {, }, (, ), < or >, or a quote that never closes.
func githubPostWords(s string) ([]string, bool) {
	words, cur := []string{}, strings.Builder{}
	started := false
	flush := func() {
		if started {
			words = append(words, cur.String())
			cur.Reset()
			started = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			flush()
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			started = true
			cur.WriteString(s[i : i+1+j+1])
			i += 1 + j
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				// A double-quoted word is literal only without $, a backtick or a backslash.
				if s[j] == '$' || s[j] == 0x60 || s[j] == '\\' {
					return nil, false
				}
				j++
			}
			if j >= len(s) {
				return nil, false
			}
			started = true
			cur.WriteString(s[i : j+1])
			i = j
		case c == '$' || c == 0x60 || c == '\\' || c == '*' || c == '?' || c == '[' || c == ']' ||
			c == '~' || c == '{' || c == '}' || c == '(' || c == ')' || c == '<' || c == '>':
			return nil, false
		default:
			started = true
			cur.WriteByte(c)
		}
	}
	flush()
	return words, true
}

// githubPostNormal is one word as the shell reads it: quote removal and backslash removal, as rule 1's
// program identity asks. It is tolerant, so a caller can normalise a word it cannot otherwise judge.
func githubPostNormal(word string) string {
	out := strings.Builder{}
	for i := 0; i < len(word); i++ {
		switch c := word[i]; {
		case c == '\'':
			j := strings.IndexByte(word[i+1:], '\'')
			if j < 0 {
				return out.String()
			}
			out.WriteString(word[i+1 : i+1+j])
			i += 1 + j
		case c == '"':
			j := i + 1
			for j < len(word) && word[j] != '"' {
				if word[j] == '\\' && j+1 < len(word) {
					j++
				}
				out.WriteByte(word[j])
				j++
			}
			if j >= len(word) {
				return out.String()
			}
			i = j
		case c == '\\' && i+1 < len(word):
			out.WriteByte(word[i+1])
			i++
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// githubPostProgram is the program word of a command: the word as the shell reads it, compared by its
// last path element and in ASCII lower case. The form checks, the mention test, the unknown-gh check
// and the read-and-record exception share this one function, so a program spelled as a path
// (/usr/bin/gh, ./gh), in quote pieces (g”h, g""h, 'g'h) or in another case (GH, Gh) is the same word.
func githubPostProgram(word string) string {
	normal := githubPostLower(githubPostNormal(word))
	if i := strings.LastIndexByte(normal, '/'); i >= 0 {
		return normal[i+1:]
	}
	return normal
}

// githubPostLower is the ASCII lower case of a text, the case rule 1 compares a program or subcommand
// word in, because a case-insensitive file system runs gh for GH.
func githubPostLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// githubPostCanonicalWords is rule 2's canonical words: every quote character and every backslash removed
// from the whole text, the ASCII letters lowered, then split on whitespace. It is what the guard reads
// when a text is not one simple command, so a program word built in quote pieces, holding a backslash or
// sitting one quoting level down is read as the word the shell builds.
func githubPostCanonicalWords(s string) []string {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '\'', '"', '\\':
			return -1
		}
		return r
	}, s)
	return strings.Fields(githubPostLower(cleaned))
}

// githubPostCanonicalNamesPost is rule 2's post test on the canonical words: a pr or issue word whose
// next word is not one of the read subcommands, an api word with a body or method flag, or a release
// word with a text flag. It names a post whether or not gh appears, so a program word the guard cannot
// place is refused.
func githubPostCanonicalNamesPost(words []string) bool {
	for i, w := range words {
		switch w {
		case "pr", "issue":
			if i+1 >= len(words) || !githubPostCanonicalRead(words[i+1]) {
				return true
			}
		case "api":
			for _, flag := range words {
				if githubPostCanonicalAPIFlag(flag) {
					return true
				}
			}
		case "release":
			if i+1 >= len(words) || !githubPostCanonicalReleaseRead(words[i+1]) {
				return true
			}
		}
	}
	return false
}

// githubPostCanonicalRead is the read subcommand list of rule 2: a pr or issue word followed by one of
// these is a read, whatever text sits around it.
func githubPostCanonicalRead(w string) bool {
	for _, name := range [...]string{"list", "view", "status", "checks", "diff"} {
		if w == name {
			return true
		}
	}
	return false
}

// githubPostCanonicalAPIFlag is a gh api body or method flag of rule 2, written attached or as its own
// word (the canonical words are already in lower case, so -F and -X read as -f and -x).
func githubPostCanonicalAPIFlag(w string) bool {
	switch {
	case w == "-x" || w == "--method" || w == "-f" || w == "--field" || w == "--raw-field" ||
		w == "--input" || w == "mutation":
		return true
	case strings.HasPrefix(w, "--method=") || strings.HasPrefix(w, "--field=") ||
		strings.HasPrefix(w, "--raw-field=") || strings.HasPrefix(w, "--input="):
		return true
	case strings.HasPrefix(w, "-f") || strings.HasPrefix(w, "-x"):
		return len(w) > 2 // an attached value, as -fbody=plain or -xget
	}
	return false
}

// githubPostCanonicalReleaseRead is the release read list of rule 2: a release word followed by one of
// these is a read, whatever text sits around it. Every other release subcommand may carry text, so a
// release word followed by anything else (or by nothing) names a post.
func githubPostCanonicalReleaseRead(w string) bool {
	for _, name := range [...]string{"list", "view", "download"} {
		if w == name {
			return true
		}
	}
	return false
}

// githubPostSplitWords is one command's words as the shell reads them, without the strict reader's
// refusal: quotes come off with their escapes and the rest is kept. It is used for the raw-text checks,
// so a word the strict reader would refuse is still seen as the word the shell builds.
func githubPostSplitWords(s string) []string {
	out, cur := []string{}, strings.Builder{}
	started := false
	flush := func() {
		if started {
			out = append(out, githubPostNormal(cur.String()))
			cur.Reset()
			started = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			flush()
		case c == '\'' || c == '"':
			started = true
			j := i + 1
			for j < len(s) && s[j] != c {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				j++
			}
			if j >= len(s) {
				j = len(s) - 1
			}
			cur.WriteString(s[i : j+1])
			i = j
		default:
			started = true
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// githubPostAssignmentWord is a NAME=value word, the assignment form rule 1 refuses before the program.
// githubPostLiteralWord is whether one word is literal on its own, for an argv array.
func githubPostLiteralWord(w string) bool {
	words, ok := githubPostWords(w)
	return ok && len(words) == 1
}

func githubPostAssignmentWord(w string) bool {
	name, _, found := strings.Cut(w, "=")
	if !found || name == "" || strings.HasPrefix(name, "-") {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// githubPostForm is form A: the one allowed gh post shape. handled is false when the command is not a
// gh post shape at all, so the caller keeps judging; a gh read and every other known gh command are
// handled and allowed, whatever words follow.
func githubPostForm(words []string, cwd string) (site githubPostSite, denied, handled bool) {
	if len(words) < 2 || githubPostProgram(words[0]) != "gh" || !githubPostGhCommand(githubPostProgram(words[1])) {
		return githubPostSite{}, false, false
	}
	sub := githubPostProgram(words[1])
	switch sub {
	case "alias":
		// An alias may expand to a post, so the guard never reads it.
		return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true, true
	case "api":
		return githubPostAPI(words[2:], cwd)
	case "pr", "issue":
		if len(words) < 3 {
			return githubPostSite{}, false, true // a gh pr or gh issue with no subcommand
		}
		switch githubPostProgram(words[2]) {
		case "comment", "create", "edit", "review", "new":
			// new is the built-in alias of create, so it is read as the post it is.
			return githubPostPost(words[3:], cwd)
		case "list", "view", "status", "checks", "diff":
			return githubPostSite{}, false, true // the read list, whatever words follow
		default:
			// Any other pr or issue subcommand may carry text (close --comment, merge --body or
			// --subject, and the rest), so it is refused.
			return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true, true
		}
	case "release":
		// The release read list, closed the way the pr and issue one is: only list, view and download
		// read, and every other release subcommand may carry text whatever flags follow (create and edit
		// take --notes, --notes-file and --title in their long and short forms, and the rest post text of
		// their own), so it is refused. gh release is never form A1 or A2.
		if len(words) < 3 {
			return githubPostSite{}, false, true // a gh release with no subcommand
		}
		switch githubPostProgram(words[2]) {
		case "list", "view", "download":
			return githubPostSite{}, false, true
		default:
			return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true, true
		}
	}
	return githubPostSite{}, false, true // a known gh command that is not a post shape
}

// githubPostPost is form A1: a gh pr or gh issue post, with only the words the rule allows.
func githubPostPost(args []string, cwd string) (githubPostSite, bool, bool) {
	file, fileSet, titles := "", false, []string{}
	for i := 0; i < len(args); i++ {
		w := args[i]
		switch {
		case !strings.HasPrefix(w, "-"):
			// a positional target: a number, a URL or a branch
		case w == "--body" || w == "-b" || strings.HasPrefix(w, "--body="):
			return githubPostSite{githubPostRuleInline, githubPostWhereCommand}, true, true
		case w == "--body-file" || w == "-F":
			v, ok := githubPostNext(args, &i)
			if !ok {
				return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true, true
			}
			v = githubPostNormal(v)
			if v == "-" {
				return githubPostSite{githubPostRuleInline, githubPostWhereCommand}, true, true
			}
			if fileSet {
				return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true, true
			}
			file, fileSet = v, true
		case strings.HasPrefix(w, "--body-file="):
			v := githubPostNormal(strings.TrimPrefix(w, "--body-file="))
			if v == "-" {
				return githubPostSite{githubPostRuleInline, githubPostWhereCommand}, true, true
			}
			if fileSet {
				return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true, true
			}
			file, fileSet = v, true
		case githubPostValueName(w) != "":
			name, value, attached := githubPostOptionValue(args, &i, w)
			if !attached {
				return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true, true
			}
			if name == "--title" || name == "-t" {
				titles = append(titles, value)
			}
		case githubPostSwitch(w):
		default:
			// Not the allowed form: the caller judges the command's own text.
			return githubPostSite{}, false, false
		}
	}
	for _, t := range titles {
		if _, found := githubPostSecretLine(t); found {
			return githubPostSite{githubPostRuleSecret, githubPostWhereTitle}, true, true
		}
	}
	if fileSet {
		content, ok := githubPostReadFile(file, cwd)
		if !ok {
			return githubPostSite{githubPostRuleUnread, file}, true, true
		}
		if line, found := githubPostSecretLine(content); found {
			return githubPostSite{githubPostRuleSecret, file + ":" + strconv.Itoa(line)}, true, true
		}
	}
	return githubPostSite{}, false, true
}

// githubPostScriptCommand is CRW-875's file rule for one command's tokens: the leading assignments and the
// wrapper commands with their options are dropped with the same reader the gh-post decomposition uses, and
// the program that remains is judged as a shell that takes its program from a file (D1) or as a path the
// shell runs directly (D2). A command whose wrapper runs nothing is not a target, and an allowed script
// leaves the command text to the closed rule, because the shell's own arguments may run a post the script
// passes on.
func githubPostScriptCommand(tokens []string, cwd string, quoted bool) (githubPostSite, bool) {
	// The words are read as the shell builds them (quote removal), so a wrapper or a shell in quote pieces
	// ('timeout' 30 bash post.sh, 'bash' post.sh) is the word the shell runs.
	normal := make([]string, len(tokens))
	for i, token := range tokens {
		if quoted {
			// The token still carries its quotes, as the strict word reader keeps them.
			normal[i] = githubPostNormal(token)
		} else {
			// The tokenizer already removed the quotes, so removing them again would strip quote
			// characters that are part of the file name (bash \"'post.sh'\" runs the file 'post.sh').
			normal[i] = token
		}
	}
	rest := shellVerbSkipWrappers(normal)
	for len(rest) > 0 && rest[0] == "--" {
		// A wrapper's own -- separator is an operand boundary the shell drops; the command runs the words
		// after it (timeout 30 -- bash post.sh runs bash post.sh).
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return githubPostSite{}, false
	}
	if site, denied, _ := githubPostShellScript(rest, cwd); denied {
		return site, true
	}
	return githubPostDirectScript(rest, cwd)
}

// githubPostScriptList applies the file rule to each command of a text that is not one simple command: the
// text is cut at a list separator, the leading assignments and the wrapper commands with their options are
// dropped, and the program that remains is judged. That is the same decomposition the write reader uses, so
// a wrapper prefix (timeout, sudo, env, nohup, nice, time, command, exec, a NAME=value word) or a list
// (cd . && bash post.sh, true; bash post.sh, bash post.sh &) no longer hides a posting script file.
func githubPostScriptList(command, cwd string) (githubPostSite, bool) {
	for _, part := range shellVerbSubsegments(command) {
		// A subshell or brace group closes with an unquoted ) or } glued to the last word; it is dropped
		// from the raw text, so a quoted operand keeps a literal close ('evil.sh)' names evil.sh)).
		part = strings.TrimRight(part, ")}")
		if site, denied := githubPostScriptCommand(shellTokenize(part), cwd, false); denied {
			return site, true
		}
	}
	return githubPostSite{}, false
}

// githubPostDirectScript is D2: a command word that is a path holding / and naming a regular file the shell
// reads as text (a #! line, or no NUL byte in its first 4 KiB) is a script, so the guard reads it and
// applies the generation-1 line rule. A binary (a NUL byte in the first 4 KiB) is not a target, and a text
// script the guard cannot read (absent, over 1 MiB, a read error) is refused as unreadable-github-post at
// the name.
func githubPostDirectScript(words []string, cwd string) (githubPostSite, bool) {
	name := words[0]
	if !strings.ContainsRune(name, '/') {
		return githubPostSite{}, false
	}
	switch githubPostProgram(name) {
	case "bash", "sh", "zsh", "dash", "ksh", "source", ".":
		// The shell rule owns a shell spelled as a path (/bin/bash script.sh); it already judged the script.
		return githubPostSite{}, false
	}
	content, binary, unreadable := githubPostReadScriptFile(name, cwd)
	if binary {
		// A regular file with a NUL byte in its first 4 KiB is a binary the shell does not run as a script.
		return githubPostSite{}, false
	}
	if unreadable {
		return githubPostSite{githubPostRuleUnread, name}, true
	}
	if !githubPostScriptNamesPost(content) {
		return githubPostSite{}, false
	}
	if line, found := githubPostScriptBadLine(content, cwd); found {
		return githubPostSite{githubPostRuleUnread, name + ":" + strconv.Itoa(line)}, true
	}
	return githubPostSite{}, false
}

// githubPostShellScript is CRW-875's rule: a shell that takes its program from a file makes the guard read
// that file. bash, sh, zsh, dash and ksh take their first operand as the program, and source and the dot
// builtin take a file. The file is judged by the closed rule, and a file the guard cannot read is refused
// at the file name.
//
// handled is true only for a refusal. A script the guard allows is not a verdict on the command: the
// command text still carries the shell's own arguments, which the script may run ("$@"), so the caller
// keeps judging it.
func githubPostShellScript(words []string, cwd string) (githubPostSite, bool, bool) {
	name, program, ok := githubPostShellProgramFile(words)
	if !ok {
		return githubPostSite{}, false, false
	}
	switch program {
	case "bash", "sh", "zsh", "dash", "ksh", "source", ".":
	default:
		return githubPostSite{}, false, false
	}
	if site, denied := githubPostJudgeScript(name, cwd); denied {
		return site, true, true
	}
	// source and the dot builtin search PATH for a name with no slash before the working directory, so a
	// file the working directory names may not be the one that runs. That file is judged too, and either
	// one failing refuses the command.
	if path, ok := githubPostSourcedPath(program, name, cwd); ok {
		if site, denied := githubPostJudgeScript(path, cwd); denied {
			return site, true, true
		}
	}
	return githubPostSite{}, false, false
}

// githubPostShellProgramFile is the program file operand of a shell command, the shell word itself, and
// whether the command takes its program from a file at all. -c takes a program string and -s takes it from
// standard input, so neither names a file; -- ends the options, so the word after it is the program
// whatever it looks like; an option written as its own word is skipped, and a value-taking one (-o, +o,
// -O, +O, --rcfile, --init-file) also consumes the word after it, so an option's value is never read as
// the program file. The shell word is the first word after the wrappers, so a source or dot command is
// recognised even when it is spelled as a path.
func githubPostShellProgramFile(words []string) (name, program string, ok bool) {
	if len(words) < 2 {
		return "", "", false
	}
	program = githubPostProgram(words[0])
	for i := 1; i < len(words); i++ {
		w := words[i]
		if w == "--" {
			if i+1 < len(words) {
				return words[i+1], program, true
			}
			return "", program, false
		}
		if githubPostShellProgramWord(w) {
			return "", program, false
		}
		if operator, consumesNext := githubPostRedirection(w); operator {
			// A redirection is not the program operand. A bare operator takes the word after it (the
			// redirection target, or the here-string), so both are skipped: "bash < post.sh" reads its
			// program from standard input, which is not a file the guard can read.
			if consumesNext {
				i++
			}
			continue
		}
		if strings.HasPrefix(w, "-") || strings.HasPrefix(w, "+") {
			if githubPostShellValueOption(w) {
				i++
			}
			continue
		}
		return w, program, true
	}
	return "", program, false
}

// githubPostRedirection is whether a word is a shell redirection operator, and whether it takes the word
// after it as its target. A leading file-descriptor number is skipped, so 2> and 2>> are operators too.
func githubPostRedirection(w string) (operator, consumesNext bool) {
	i := 0
	for i < len(w) && w[i] >= '0' && w[i] <= '9' {
		i++
	}
	rest := w[i:]
	if rest == "" || rest[0] != '<' && rest[0] != '>' {
		return false, false
	}
	switch rest {
	case "<", ">", ">>", "<<", "<<<", "<>", ">&", "<&", "&>", "&>>":
		return true, true
	}
	// An attached target (>out.txt) or a process substitution (<(cmd)) carries its own word.
	return true, false
}

// githubPostSourcedPath is the file a shell's source or dot builtin would read for a name with no slash: it
// searches PATH first and falls back to the name in the working directory. The guard reads this file as
// well as the working-directory one, so neither can hide a posting script from the other. PATH is the
// guard's own environment, because the payload carries none, so this is a superset of what the shell runs.
func githubPostSourcedPath(program, name, cwd string) (string, bool) {
	if program != "source" && program != "." {
		return "", false
	}
	if strings.ContainsRune(name, '/') {
		return "", false
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		if !filepath.IsAbs(dir) {
			// A relative PATH entry is relative to the payload's working directory, where the shell runs.
			dir = githubPostAbsPath(dir, cwd)
		}
		// The first regular file of that name is the one a shell reads; it need not be executable, as a
		// sourced file is read rather than run.
		candidate := filepath.Join(dir, name)
		if candidate == name {
			continue
		}
		if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() {
			return candidate, true
		}
	}
	return "", false
}

// githubPostShellProgramWord is whether a word is a single-dash option bundle that keeps the first operand
// from being the program file: c takes the program as its own word (bash -c, sh -ec) and s reads it from
// standard input (bash -s, bash -es). The bundle is read as the shell reads it, so a letter inside the
// attached value of o or O is not a flag (zsh -ocorrect post.sh has no c flag); a long option such as
// --norc is not a bundle at all.
func githubPostShellProgramWord(w string) bool {
	if len(w) < 2 || w[0] != '-' || w[1] == '-' {
		return false
	}
	for i := 1; i < len(w); i++ {
		switch w[i] {
		case 'c', 's':
			return true
		case 'o', 'O':
			// The rest of the word is this option's attached value, not more flags.
			return false
		}
	}
	return false
}

// githubPostShellValueOption is whether a shell option written as its own word takes the next word as its
// value: -o and -O name an option or a shopt option, in a bundle too (bash -eo errexit), and --rcfile and
// --init-file name a file. An option written attached (--rcfile=file) carries its own value, so it is not
// one, and a long option such as --norc is not one either.
func githubPostShellValueOption(w string) bool {
	if strings.ContainsRune(w, '=') {
		return false
	}
	if strings.HasPrefix(w, "--") {
		return w == "--rcfile" || w == "--init-file"
	}
	// A single-dash bundle: o and O take their value from the next word only when they end the bundle
	// (bash -o errexit, bash -eo errexit). With more of the bundle after them the value is attached
	// (zsh -oerrexit post.sh, zsh -Oextglob post.sh), so the next word is still the program file.
	if len(w) < 2 || w[0] != '-' && w[0] != '+' || w[1] == '-' {
		return false
	}
	return strings.ContainsAny(w[len(w)-1:], "oO")
}

// githubPostJudgeScript reads the program file and judges it: a file it cannot read is refused at the file
// name, and a file that names a post is refused at the offending line unless every executable line on its
// own is the one allowed post form or the read-and-record exception. A file the guard allows is not a
// verdict on the whole command: the caller keeps judging the command's own text, because a shell runs the
// arguments it is given and a script may run them too.
func githubPostJudgeScript(name, cwd string) (githubPostSite, bool) {
	content, ok := githubPostReadScript(name, cwd)
	if !ok {
		return githubPostSite{githubPostRuleUnread, name}, true
	}
	if !githubPostScriptNamesPost(content) {
		return githubPostSite{}, false
	}
	if line, found := githubPostScriptBadLine(content, cwd); found {
		return githubPostSite{githubPostRuleUnread, name + ":" + strconv.Itoa(line)}, true
	}
	return githubPostSite{}, false
}

// githubPostScriptNamesPost is whether a program file's text names a post: CRW-783's mention test (the
// word gh beside a pr or issue posting verb, or api, or release) or the canonical post test the rule
// already uses for a text that is not one simple command. The canonical test is what reads the built-in
// create aliases (gh pr new, gh issue new) and the release text arguments, so a script that posts
// through them is a target rather than a script that names no post.
func githubPostScriptNamesPost(content string) bool {
	return githubPostMentions(content) ||
		githubPostCanonicalNamesPost(githubPostCanonicalWords(content))
}

// githubPostScriptBadLine is the 1-based number of the first executable line that is neither the one
// allowed post form nor the read-and-record exception, and whether one does. A blank line and a comment
// line are skipped.
func githubPostScriptBadLine(content, cwd string) (int, bool) {
	for i, line := range strings.Split(content, "\n") {
		s := text.Trim(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		words, ok := githubPostSimple(s)
		if !ok {
			return i + 1, true
		}
		if _, denied, handled := githubPostForm(words, cwd); handled {
			if denied {
				return i + 1, true
			}
			continue
		}
		if githubPostQuiet(words) {
			continue
		}
		return i + 1, true
	}
	return 0, false
}

// githubPostAbsPath is the path a name denotes for the payload's working directory: an absolute name as
// written, a relative one joined with the payload cwd (or the guard's own when the payload carries none).
// The name is joined without cleaning, so a `..` in it is resolved by the kernel against the real
// directory tree rather than collapsed here: a link in the middle of the name is followed before the `..`
// applies, exactly as the shell would. Cleaning first would inspect a different file than the one opened.
func githubPostAbsPath(name, cwd string) string {
	if filepath.IsAbs(name) {
		return name
	}
	base := cwd
	if base == "" {
		if wd, err := os.Getwd(); err == nil {
			base = wd
		}
	}
	if base == "" {
		return name
	}
	return base + string(filepath.Separator) + name
}

// githubPostReadScriptFile reads a directly executed script (D2). The path is joined with the payload's
// working directory. The file is a script when it starts with #! or has no NUL byte in its first 4 KiB;
// only a file that does neither (a binary) is not a target, because a #! script runs under its interpreter
// whatever bytes follow the shebang line. Its whole text (at most 1 MiB) is returned. A name the guard
// cannot read as a text script - an absent file, a directory, a FIFO, a file over 1 MiB, a read error -
// reports unreadable, which the caller refuses at the name, exactly as generation 1 refuses a shell
// program file it cannot read.
func githubPostReadScriptFile(name, cwd string) (content string, binary, unreadable bool) {
	path := githubPostAbsPath(name, cwd)
	// A name that is not a regular file is refused before any open: a FIFO would wait for a writer, and an
	// absent file, a directory or a link to nothing is not a text script the guard can rule out.
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return "", false, true
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false, true
	}
	defer file.Close()
	// The open handle is the file that will be read, so its own stat is what the identity check compares
	// against the pathname: a file swapped in between the stat above and the open is refused.
	handle, err := file.Stat()
	if err != nil || !os.SameFile(st, handle) {
		return "", false, true
	}
	head := make([]byte, 4096)
	n, err := io.ReadFull(file, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", false, true
	}
	head = head[:n]
	if bytes.IndexByte(head, 0) >= 0 && !bytes.HasPrefix(head, []byte("#!")) {
		return "", true, false
	}
	rest, err := io.ReadAll(io.LimitReader(file, githubPostMaxFileBytes+1))
	if err != nil || int64(len(head))+int64(len(rest)) > githubPostMaxFileBytes {
		return "", false, true
	}
	return string(head) + string(rest), false, false
}

// githubPostReadScript reads the text of the shell program a command names: a literal path, a relative one
// joined with the payload's working directory, that is a regular file of at most 1 MiB and whose bytes the
// guard can read. Unlike a body file it is not confined to a temporary root: the guard reads it only to
// judge it, and the refusal names the file and never a value.
func githubPostReadScript(name, cwd string) (string, bool) {
	path := githubPostAbsPath(name, cwd)
	// A path that is not a regular file is refused before the open: opening a FIFO read-only waits for a
	// writer, which would hang the hook instead of denying the command. Stat follows a link without
	// blocking, so a script reached through a link is still read.
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	// The handle must be the file the path names now: another file swapped in between the check and the
	// open would otherwise be read past it.
	if now, err := os.Stat(path); err != nil || !os.SameFile(st, now) {
		return "", false
	}
	b, err := io.ReadAll(io.LimitReader(file, githubPostMaxFileBytes+1))
	if err != nil || len(b) > githubPostMaxFileBytes {
		return "", false
	}
	return string(b), true
}

// githubPostAPI is form A2: gh api, with only the words the rule allows. At most one of -F body=@F,
// --field body=@F or --input F carries the text; none of them is a read.
func githubPostAPI(args []string, cwd string) (githubPostSite, bool, bool) {
	file, fileSet, endpoint := "", false, false
	for i := 0; i < len(args); i++ {
		w := args[i]
		switch {
		case w == "-X" || w == "--method":
			v, ok := githubPostNext(args, &i)
			if !ok || !githubPostMethod(v) {
				return githubPostSite{}, false, false
			}
		case strings.HasPrefix(w, "--method="):
			if !githubPostMethod(strings.TrimPrefix(w, "--method=")) {
				return githubPostSite{}, false, false
			}
		case w == "-H" || w == "--header":
			if _, ok := githubPostNext(args, &i); !ok {
				return githubPostSite{}, false, false
			}
		case strings.HasPrefix(w, "--header="):
		case w == "--paginate" || w == "--silent" || w == "-i" || w == "--include":
		case w == "--jq" || w == "-q":
			if _, ok := githubPostNext(args, &i); !ok {
				return githubPostSite{}, false, false
			}
		case w == "--input" || strings.HasPrefix(w, "--input="):
			v := strings.TrimPrefix(w, "--input=")
			if w == "--input" {
				next, ok := githubPostNext(args, &i)
				if !ok {
					return githubPostSite{}, false, false
				}
				v = next
			}
			v = githubPostNormal(v)
			if v == "-" {
				// A standard-input body is refused explicitly, as --body-file - already was.
				return githubPostSite{githubPostRuleInline, githubPostWhereCommand}, true, true
			}
			if fileSet {
				return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true, true
			}
			file, fileSet = v, true
		case w == "-F" || w == "--field":
			v, ok := githubPostNext(args, &i)
			if !ok {
				return githubPostSite{}, false, false
			}
			v = githubPostNormal(v)
			name, value, found := strings.Cut(v, "=")
			if !found || name != "body" {
				return githubPostSite{}, false, false
			}
			if !strings.HasPrefix(value, "@") {
				return githubPostSite{githubPostRuleInline, githubPostWhereCommand}, true, true
			}
			if strings.TrimPrefix(value, "@") == "-" {
				// -F body=@- is a standard-input body: refused as an inline body, like --input -.
				return githubPostSite{githubPostRuleInline, githubPostWhereCommand}, true, true
			}
			if fileSet {
				return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true, true
			}
			file, fileSet = strings.TrimPrefix(value, "@"), true
		case w == "-f" || w == "--raw-field":
			v, ok := githubPostNext(args, &i)
			if !ok {
				return githubPostSite{}, false, false
			}
			v = githubPostNormal(v)
			if name, _, found := strings.Cut(v, "="); found && name == "body" {
				return githubPostSite{githubPostRuleInline, githubPostWhereCommand}, true, true
			}
			return githubPostSite{}, false, false
		case !strings.HasPrefix(w, "-"):
			if endpoint {
				return githubPostSite{}, false, false
			}
			endpoint = true
		default:
			return githubPostSite{}, false, false
		}
	}
	if fileSet {
		content, ok := githubPostReadFile(file, cwd)
		if !ok {
			return githubPostSite{githubPostRuleUnread, file}, true, true
		}
		if line, found := githubPostSecretLine(content); found {
			return githubPostSite{githubPostRuleSecret, file + ":" + strconv.Itoa(line)}, true, true
		}
	}
	return githubPostSite{}, false, true
}

// githubPostNext is the value of an option written as its own word, advancing past it.
func githubPostNext(args []string, i *int) (string, bool) {
	if *i+1 >= len(args) {
		return "", false
	}
	*i++
	return args[*i], true
}

// githubPostValueName is the canonical name of a value-taking option of form A1, empty when the word is
// not one.
func githubPostValueName(w string) string {
	name, _, _ := strings.Cut(w, "=")
	for _, v := range [...]string{"--title", "-t", "--base", "-B", "--head", "-H", "--repo", "-R",
		"--label", "--add-label", "--remove-label", "--assignee", "-a", "--reviewer", "--milestone", "-m"} {
		if name == v {
			return v
		}
	}
	return ""
}

// githubPostOptionValue is the value of an option of form A1, written attached or as its own word.
func githubPostOptionValue(args []string, i *int, w string) (name, value string, attached bool) {
	if n, v, cut := strings.Cut(w, "="); cut {
		return n, v, true
	}
	name = githubPostValueName(w)
	v, ok := githubPostNext(args, i)
	if !ok {
		return name, "", false
	}
	return name, v, true
}

// githubPostSwitch is a switch form A1 allows, one that carries no text.
func githubPostSwitch(w string) bool {
	for _, v := range [...]string{"--draft", "--approve", "--comment", "-c", "--request-changes", "-r"} {
		if w == v {
			return true
		}
	}
	return false
}

// githubPostMethod is the HTTP method form A2 allows.
func githubPostMethod(v string) bool {
	for _, m := range [...]string{"GET", "POST", "PATCH", "PUT", "DELETE"} {
		if v == m {
			return true
		}
	}
	return false
}

// githubPostQuiet is exception B: a simple command that reads or records, allowed whatever its words
// say, because a code search or a commit message may name a post. sed is not on the list: its e command
// runs a shell.
func githubPostQuiet(words []string) bool {
	if len(words) == 0 {
		return false
	}
	switch githubPostProgram(words[0]) {
	case "rg":
		for _, w := range words[1:] {
			if w == "--pre" || strings.HasPrefix(w, "--pre=") || w == "--pre-glob" || strings.HasPrefix(w, "--pre-glob=") {
				return false
			}
		}
		return true
	case "grep", "egrep", "fgrep", "cat", "head", "tail", "wc", "ls", "echo", "printf":
		return true
	case "git":
		// No option before its subcommand, and the subcommand only reads or records.
		if len(words) < 2 {
			return false
		}
		sub := githubPostProgram(words[1])
		if strings.HasPrefix(sub, "-") {
			return false
		}
		for _, name := range [...]string{"log", "show", "diff", "grep", "status", "commit"} {
			if sub == name {
				return true
			}
		}
		return false
	}
	return false
}

// githubPostUnknownGh is a simple gh command whose first word after gh is not a known gh command: an
// alias or a program the guard cannot place may expand to a post.
func githubPostUnknownGh(words []string) (githubPostSite, bool) {
	if len(words) < 2 || githubPostProgram(words[0]) != "gh" || githubPostGhCommand(githubPostProgram(words[1])) {
		return githubPostSite{}, false
	}
	return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
}

// githubPostGhCommand is the gh commands the guard knows, so a word outside the list is not a gh command
// the guard can place.
func githubPostGhCommand(w string) bool {
	for _, name := range [...]string{"alias", "api", "attestation", "auth", "browse", "cache", "codespace",
		"completion", "config", "extension", "gist", "gpg-key", "help", "issue", "label", "org", "pr",
		"project", "release", "repo", "ruleset", "run", "search", "secret", "ssh-key", "status", "variable",
		"workflow"} {
		if w == name {
			return true
		}
	}
	return false
}

// githubPostInlineShape is whether the text spells a body inside the command: -b, --body, --body=, or a
// standard-input body file, the shapes rule 2 names inline-github-body.
func githubPostInlineShape(s string) bool {
	for _, shape := range [...]string{"-b ", "--body ", "--body=", "--body-file -", "-F -"} {
		if strings.Contains(s, shape) {
			return true
		}
	}
	return false
}

// githubPostMentions is the mention test on any text: the word gh, by the normalised program of the first
// word or as a word anywhere, and then a post — pr or issue with one of the posting verbs, or api, or
// release. It reads the text as written and again through rule 2's canonical form, so a program word
// built in quote pieces, holding a backslash, one quoting level down or in another case is seen as well
// as one the shell builds by removing quotes. A read such as gh pr view is no mention.
func githubPostMentions(s string) bool {
	return githubPostMentionsIn(s, false) || githubPostMentionsIn(strings.Join(githubPostCanonicalWords(s), " "), false)
}

// githubPostMentionsBroad is the generation-6 mention test, kept for a text that holds an outer-shell
// expansion: the word gh with pr, issue or api anywhere, no posting verb required, because the expansion
// may build the post the guard cannot read.
func githubPostMentionsBroad(s string) bool {
	return githubPostMentionsIn(s, true) || githubPostMentionsIn(strings.Join(githubPostCanonicalWords(s), " "), true)
}

// githubPostMentionsIn is the mention test on one spelling of a text.
func githubPostMentionsIn(s string, broad bool) bool {
	words := strings.Fields(s)
	if len(words) == 0 {
		return false
	}
	if !githubPostWord(s, "gh") && githubPostProgram(words[0]) != "gh" {
		return false
	}
	if broad {
		return githubPostWord(s, "pr") || githubPostWord(s, "issue") || githubPostWord(s, "api")
	}
	if githubPostWord(s, "pr") || githubPostWord(s, "issue") {
		for _, verb := range [...]string{"comment", "create", "edit", "review"} {
			if githubPostWord(s, verb) {
				return true
			}
		}
	}
	return githubPostWord(s, "api") || githubPostWord(s, "release")
}

// githubPostNamesPost is whether the word list holds a post verb, for the unreadable-program rule.
func githubPostNamesPost(words []string) bool {
	for _, w := range words {
		switch githubPostProgram(w) {
		case "pr", "issue", "api", "comment", "create", "edit", "review", "mutation":
			return true
		}
	}
	return false
}

// githubPostHoldsExpansion is whether a normalised word still holds an expansion or a glob the shell
// would act on, so the guard cannot know the command it names.
func githubPostHoldsExpansion(word string) bool {
	return strings.ContainsAny(word, "$`*?[]~{}()<>")
}

// githubPostWord is whether the text holds the word with a boundary on either side.
func githubPostWord(s, word string) bool {
	for i := 0; i+len(word) <= len(s); i++ {
		if s[i:i+len(word)] != word {
			continue
		}
		if !githubPostWordByte(s, i-1) && !githubPostWordByte(s, i+len(word)) {
			return true
		}
	}
	return false
}

// githubPostWordByte is whether the byte of text at i continues a shell word; an index off either end is
// a boundary, so a word that starts or ends the text counts.
func githubPostWordByte(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	switch c := s[i]; {
	case c == '_' || c == '-' || c == '.' || c == '/' || c >= 0x80:
		return true
	case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9':
		return true
	}
	return false
}

// githubPostExpands is whether the text as written holds an outer-shell expansion the shell acts on
// before gh sees it: a backtick, a dollar, or a newline.
func githubPostExpands(s string) bool {
	return strings.ContainsAny(s, "\n$\x60")
}

// githubPostReadFile reads the text a post names: a literal path that lies under a temporary root, is a
// regular file of at most 1 MiB, and whose bytes the guard can read.
func githubPostReadFile(name, cwd string) (string, bool) {
	// The containment check reads the file the kernel would read, so the path is resolved through its
	// links before any cleaning; a link under a root that points outside it is refused.
	path, err := filepath.EvalSymlinks(githubPostAbsPath(name, cwd))
	if err != nil || !githubPostUnderRoots(path) {
		return "", false
	}
	// The resolved path holds no link of its own, so a link found at the open is one swapped in after the
	// check: the open refuses it rather than following it past the containment decision.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", false
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	// The handle must be the file the resolved path names now: a link or another file swapped in between
	// the resolution and the open would otherwise be read past the containment check.
	if now, err := os.Lstat(path); err != nil || !os.SameFile(st, now) {
		return "", false
	}
	b, err := io.ReadAll(io.LimitReader(file, githubPostMaxFileBytes+1))
	if err != nil || len(b) > githubPostMaxFileBytes {
		return "", false
	}
	return string(b), true
}

// githubPostUnderRoots is whether a resolved path lies under a temporary root the guard trusts: the
// environment's TMPDIR when it is absolute, and the two standard temporary directories. A root is
// resolved the way the path was, so a temporary directory reached through a link still contains its own
// files.
func githubPostUnderRoots(path string) bool {
	roots := []string{"/tmp", "/var/tmp"}
	if t := os.Getenv("TMPDIR"); filepath.IsAbs(t) {
		roots = append(roots, t)
	}
	for _, root := range roots {
		root = filepath.Clean(root)
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
