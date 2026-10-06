package hook

// CRW-783: CRW's own protection, no CXC oracle. The PreToolUse guard over the shell tools allows a
// GitHub post only when the command is one simple command of literal words in the one form the guard
// reads whole (the closed rule), and refuses every other text that names a post. The deny reason names
// only the rule and the place, never a value.

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
	return githubPostUnread(command)
}

// githubPostJudgeArgv judges an already-split argv array: it is a simple command when every word is
// literal, and otherwise the text it spells is refused when it names a post.
func githubPostJudgeArgv(words []string, cwd string) (githubPostSite, bool) {
	for _, w := range words {
		if !githubPostLiteralWord(w) {
			return githubPostUnread(strings.Join(words, " "))
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
	if githubPostQuiet(words) {
		return githubPostSite{}, false
	}
	if site, denied := githubPostUnknownGh(words); denied {
		return site, true
	}
	// A command word that still holds an expansion cannot be judged, and a word list that names a post
	// in any spelling is refused: the program is normalised first, so a path or quote pieces are seen.
	if githubPostMentionsWords(words) {
		return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
	}
	return githubPostSite{}, false
}

// githubPostUnread is the fail-closed judgement for a text that is not one simple command: a text naming
// a post is refused, and one whose text holds an outer-shell expansion says so.
func githubPostUnread(command string) (githubPostSite, bool) {
	// A command word that still holds an expansion after quote removal cannot be judged, so a command
	// whose program is such a word and whose rest names a post verb is refused.
	if words := githubPostSplitTolerant(command); len(words) > 0 &&
		githubPostHoldsExpansion(githubPostNormal(words[0])) && githubPostNamesPost(words[1:]) {
		return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
	}
	if !githubPostMentions(command) && !githubPostMentionsWords(githubPostSplitTolerant(command)) {
		return githubPostSite{}, false
	}
	// A text that spells an inline body is the inline-body rule, wherever the body sits.
	if githubPostInlineShape(command) {
		return githubPostSite{githubPostRuleInline, githubPostWhereCommand}, true
	}
	if githubPostExpands(command) {
		return githubPostSite{githubPostRuleExpand, githubPostWhereCommand}, true
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
// false when a word or the text as a whole is not literal: an unquoted $, backtick, *, ?, [, ], ~, {, },
// (, ), < or >, or a quote that never closes. A backslash escapes the character after it, so the pair is
// literal too.
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
				// A double-quoted word is literal only without $ or a backtick; a backslash escapes the
				// character after it, which the shell removes.
				if s[j] == '$' || s[j] == 0x60 {
					return nil, false
				}
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				j++
			}
			if j >= len(s) {
				return nil, false
			}
			started = true
			cur.WriteString(s[i : j+1])
			i = j
		case c == '\\' && i+1 < len(s):
			started = true
			cur.WriteString(s[i : i+2])
			i++
		case c == '$' || c == 0x60 || c == '*' || c == '?' || c == '[' || c == ']' ||
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
// last path element. The form checks and the mention test share this one function, so a program spelled
// as a path (/usr/bin/gh, ./gh) or in quote pieces (g”h, g""h, 'g'h) is the same word either way.
func githubPostProgram(word string) string {
	normal := githubPostNormal(word)
	if i := strings.LastIndexByte(normal, '/'); i >= 0 {
		return normal[i+1:]
	}
	return normal
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
	if len(words) < 2 || githubPostProgram(words[0]) != "gh" || !githubPostGhCommand(githubPostNormal(words[1])) {
		return githubPostSite{}, false, false
	}
	sub := githubPostNormal(words[1])
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
		switch githubPostNormal(words[2]) {
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
		// A release's text arguments post the text; anything else is not a post shape.
		for _, w := range words[2:] {
			if name, _, _ := strings.Cut(w, "="); name == "--notes" || name == "--notes-file" || name == "--title" {
				return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true, true
			}
		}
		return githubPostSite{}, false, true
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
			if v == "-" {
				return githubPostSite{githubPostRuleInline, githubPostWhereCommand}, true, true
			}
			if fileSet {
				return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true, true
			}
			file, fileSet = v, true
		case strings.HasPrefix(w, "--body-file="):
			v := strings.TrimPrefix(w, "--body-file=")
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
			if fileSet {
				return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true, true
			}
			file, fileSet = v, true
		case w == "-F" || w == "--field":
			v, ok := githubPostNext(args, &i)
			if !ok {
				return githubPostSite{}, false, false
			}
			name, value, found := strings.Cut(v, "=")
			if !found || name != "body" {
				return githubPostSite{}, false, false
			}
			if !strings.HasPrefix(value, "@") {
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
		if len(words) < 2 || strings.HasPrefix(words[1], "-") {
			return false
		}
		for _, sub := range [...]string{"log", "show", "diff", "grep", "status", "commit"} {
			if words[1] == sub {
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
	if len(words) < 2 || githubPostProgram(words[0]) != "gh" || githubPostGhCommand(githubPostNormal(words[1])) {
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

// githubPostMentions is the raw-text test: the word gh with pr, issue or api anywhere. It is the
// fail-closed test for a text the guard cannot take as one simple command.
func githubPostMentions(s string) bool {
	return githubPostWord(s, "gh") && (githubPostWord(s, "pr") || githubPostWord(s, "issue") || githubPostWord(s, "api"))
}

// githubPostMentionsWords is rule 2's mention test on a word list: the words hold gh, by the normalised
// program of the first word (so a path or quote pieces are seen) or as a word anywhere, and a posting
// subcommand, or api with a field flag, or release with a text flag.
func githubPostMentionsWords(words []string) bool {
	if len(words) == 0 {
		return false
	}
	joined := strings.Join(words, " ")
	if !githubPostWord(joined, "gh") && githubPostProgram(words[0]) != "gh" {
		return false
	}
	if githubPostWord(joined, "pr") || githubPostWord(joined, "issue") {
		for _, verb := range [...]string{"comment", "create", "edit", "review"} {
			if githubPostWord(joined, verb) {
				return true
			}
		}
	}
	if githubPostWord(joined, "api") {
		for _, flag := range [...]string{"-X", "--method", "-f", "-F", "--field", "--raw-field", "--input", "mutation"} {
			if strings.Contains(joined, flag) {
				return true
			}
		}
	}
	if githubPostWord(joined, "release") {
		for _, flag := range [...]string{"--notes", "--notes-file", "--title"} {
			if strings.Contains(joined, flag) {
				return true
			}
		}
	}
	return false
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

// githubPostSplitTolerant is githubPostSplitWords, kept for the call sites that name it.
func githubPostSplitTolerant(s string) []string {
	return githubPostSplitWords(s)
}

// api with a field flag.
func githubPostMentionsPost(s string) bool {
	if githubPostWord(s, "gh") && (githubPostWord(s, "pr") || githubPostWord(s, "issue")) {
		for _, w := range [...]string{"comment", "create", "edit", "review"} {
			if githubPostWord(s, w) {
				return true
			}
		}
	}
	if githubPostWord(s, "api") {
		for _, f := range [...]string{"-X", "--method", "-f", "-F", "--field", "--raw-field", "--input", "mutation"} {
			if strings.Contains(s, f) {
				return true
			}
		}
	}
	return false
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
	path := name
	if !filepath.IsAbs(path) {
		base := cwd
		if base == "" {
			if wd, err := os.Getwd(); err == nil {
				base = wd
			}
		}
		path = filepath.Join(base, path)
	}
	path = filepath.Clean(path)
	if !githubPostUnderRoots(path) {
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
	b, err := io.ReadAll(io.LimitReader(file, githubPostMaxFileBytes+1))
	if err != nil || len(b) > githubPostMaxFileBytes {
		return "", false
	}
	return string(b), true
}

// githubPostUnderRoots is whether a cleaned path lies under a temporary root the guard trusts: the
// environment's TMPDIR when it is absolute, /tmp, or /var/tmp.
func githubPostUnderRoots(path string) bool {
	roots := []string{"/tmp", "/var/tmp"}
	if t := os.Getenv("TMPDIR"); filepath.IsAbs(t) {
		roots = append(roots, t)
	}
	for _, root := range roots {
		root = filepath.Clean(root)
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
