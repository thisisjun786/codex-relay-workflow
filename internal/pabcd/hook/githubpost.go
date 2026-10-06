package hook

// CRW-783: CRW's own protection, with no CXC oracle. The PreToolUse guard over the shell tools stops a
// GitHub posting command whose text sits inside the command or holds an outer-shell expansion, and
// scans the text bound for GitHub (an inline title, or the file of --body-file, -F body=@F or --input)
// for secrets. The deny reason names the rule and the place, never the value.
//
// Four rules: inline-github-body (a body may never sit in the command), expansion-in-github-text (text
// bound for GitHub holding an outer-shell expansion outside single quotes), secret-in-github-text (a
// secret pattern in that text) and unreadable-github-post (a post the guard cannot read, denied because
// the guard fails closed). A command unrelated to GitHub behaves as it did.

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

const (
	githubPostWhereCommand = "command"
	githubPostRuleInline   = "inline-github-body"
	githubPostRuleExpand   = "expansion-in-github-text"
	githubPostRuleSecret   = "secret-in-github-text"
	githubPostRuleUnread   = "unreadable-github-post"

	// githubPostDepth bounds how deep a shell -c or eval program is read, so the work stays linear.
	githubPostDepth = 8

	// githubPostMaxFileBytes is the most the guard reads of a file a post names (1 MiB).
	githubPostMaxFileBytes = 1024 * 1024

	// GitHubPostMaxStdinBytes is this row's own stdin bound (1 MiB), the bound a PreToolUse gate of this
	// runtime reads its payload within. The row reads its input itself; over the bound the guard refuses
	// rather than passing an input it could not judge.
	GitHubPostMaxStdinBytes = 1024 * 1024
)

// GitHubPostAnswer is the component row's whole input policy: the guard's answer for the payload, or the
// deny envelope when the payload is over the bound. A read that fails is read as empty input, as the
// other component ingresses read it.
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
// stop, else empty. A call that is not one PreToolUse object, or a tool that is not one of the shell
// tools, passes, as does a command unrelated to GitHub.
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
	s := githubPostScan{cwd: cwd, depth: githubPostDepth}
	words, command, argv := githubPostInput(input)
	if argv {
		site, denied := s.command(words, "")
		if !denied {
			return ""
		}
		return githubPostDeny(site.rule, site.place)
	}
	if text.Trim(command) == "" {
		return ""
	}
	site, denied := s.text(command)
	if !denied {
		return ""
	}
	return githubPostDeny(site.rule, site.place)
}

// githubPostDeny is the deny envelope: the rule and the place, then one way forward. The text, the
// matched pattern and its context stay out of it, because this reason reaches a public thread.
func githubPostDeny(rule, place string) string {
	reason := "GitHub post blocked (" + rule + ") at " + place +
		": write the text to a file, check it, and pass it with --body-file, -F body=@file or --input"
	return editAnswer("deny", reason, reason)
}

// githubPostSite is why and where a GitHub post is denied: the rule and the place the reason names.
type githubPostSite struct{ rule, place string }

// githubPostScan is one payload's judgement: its working directory, the quoted heredoc body of the
// command being read, and how deep a program may still be read.
type githubPostScan struct {
	cwd, body string
	hasBody   bool
	depth     int
}

// githubPostWord is one word of a simple command: text is the word with its quoting removed, raw the
// word as written, so a caller can still ask what the outer shell would expand.
type githubPostWord struct{ text, raw string }

// githubPostInput is tool_input's command or cmd: an argv array becomes words that are already split, so
// no shell expands them and a word has no written form for the expansion rule to read; a string is shell
// text. The array is looked for first, as the host that sends one sends no text beside it.
func githubPostInput(input map[string]any) ([]githubPostWord, string, bool) {
	for _, key := range [...]string{"command", "cmd"} {
		items, ok := input[key].([]any)
		if !ok {
			continue
		}
		words := make([]githubPostWord, 0, len(items))
		for _, item := range items {
			s, ok := item.(string)
			if !ok {
				return nil, "", false
			}
			words = append(words, githubPostWord{text: s})
		}
		return words, "", true
	}
	for _, key := range [...]string{"command", "cmd"} {
		if s, ok := input[key].(string); ok {
			return nil, s, false
		}
	}
	return nil, "", false
}

// text reads one shell text: every simple command it holds, each with the quoted heredoc body of its own
// line. Text the guard cannot split, or a program it cannot read that mentions a GitHub post, is denied
// as unreadable.
func (s *githubPostScan) text(command string) (githubPostSite, bool) {
	if _, balanced := githubPostStripped(command); !balanced {
		if githubPostMentions(command) {
			return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
		}
		return githubPostSite{}, false
	}
	for _, line := range githubPostCommands(command) {
		s.body, s.hasBody = line.body, line.hasBody
		for _, segment := range githubPostSegments(line.text) {
			if site, denied := s.command(githubPostWords(segment), segment); denied {
				return site, true
			}
			// A gh post inside a command substitution is a text the guard cannot read, so a segment that
			// holds one and names a post is denied rather than passed unjudged.
			if githubPostExpands(segment, false, true) && githubPostMentions(segment) {
				return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
			}
		}
	}
	return githubPostSite{}, false
}

// command judges one simple command: a gh posting command, a shell -c or eval program read one level
// deeper, or nothing.
func (s *githubPostScan) command(words []githubPostWord, segment string) (githubPostSite, bool) {
	spelled := make([]string, len(words))
	for i, w := range words {
		spelled[i] = w.text
	}
	head := githubPostStripPrefixes(words)
	if head >= len(words) {
		return githubPostSite{}, false
	}
	if shellVerbName(words[head].text) == "gh" {
		return s.gh(words[head+1:])
	}
	if s.depth <= 0 {
		return githubPostSite{}, false
	}
	program, ok := githubPostProgram(shellVerbName(words[head].text), spelled[head:])
	if !ok {
		return githubPostSite{}, false
	}
	// A program holding a command substitution cannot be read at all, so a text that mentions gh with
	// pr, issue or api is denied rather than passed unjudged; anything else is read one level down.
	if githubPostExpands(program, false, true) {
		if githubPostMentions(segment) {
			return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
		}
		return githubPostSite{}, false
	}
	s.depth--
	return s.text(program)
}

// githubPostProgram is the command string a shell -c or eval would run, when this command starts one.
func githubPostProgram(verb string, rest []string) (string, bool) {
	if shellVerbIsShell(verb) {
		return shellVerbShellScript(rest[1:])
	}
	if verb == "eval" {
		return strings.Join(rest[1:], " "), true
	}
	return "", false
}

// gh judges a gh command: pr create|edit|comment|review, issue create|comment|edit, or api with a body
// or title field. gh pr view|list|checks|diff and gh api without such a field are not targets. The group
// word is looked for rather than taken as the first argument, because gh takes global flags before it
// (gh --repo o/r pr create).
func (s *githubPostScan) gh(args []githubPostWord) (githubPostSite, bool) {
	group := -1
	for i, a := range args {
		if a.text == "pr" || a.text == "issue" || a.text == "api" {
			group = i
			break
		}
	}
	if group < 0 {
		return githubPostSite{}, false
	}
	if args[group].text == "api" {
		return s.api(args[group+1:])
	}
	if group+1 >= len(args) {
		return githubPostSite{}, false
	}
	sub := args[group+1].text
	if args[group].text == "pr" {
		if sub != "create" && sub != "edit" && sub != "comment" && sub != "review" {
			return githubPostSite{}, false
		}
	} else if sub != "create" && sub != "comment" && sub != "edit" {
		return githubPostSite{}, false
	}
	return s.flags(args[group+2:])
}

// flags judges a gh pr or gh issue command: -b and --body carry the body inside the command,
// --body-file and -F name a file, and -t and --title may stay inside the command unless they hold an
// outer-shell expansion or a secret.
func (s *githubPostScan) flags(args []githubPostWord) (githubPostSite, bool) {
	var first githubPostSite
	denied := false
	record := func(site githubPostSite, no bool) {
		if no && !denied {
			first, denied = site, true
		}
	}
	for _, o := range githubPostOptions(args, "btF") {
		switch o.name {
		case "-b", "--body":
			record(s.inline(o.value, true))
		case "-t", "--title":
			record(s.inline(o.value, false))
		case "-F", "--body-file":
			record(s.file(o.value.text))
		}
	}
	return first, denied
}

// api judges gh api, which is a target only through a body or title field: -f and --raw-field carry
// text, -F and --field carry text or @file, and --input names a file.
func (s *githubPostScan) api(args []githubPostWord) (githubPostSite, bool) {
	var first githubPostSite
	denied := false
	record := func(site githubPostSite, no bool) {
		if no && !denied {
			first, denied = site, true
		}
	}
	for _, o := range githubPostOptions(args, "fF") {
		switch {
		case o.name == "--input":
			record(s.file(o.value.text))
		case o.name == "-f" || o.name == "--raw-field" || o.name == "-F" || o.name == "--field":
			key, value, ok := strings.Cut(o.value.text, "=")
			if !ok {
				continue
			}
			switch {
			case key == "body" && (o.name == "-F" || o.name == "--field") && strings.HasPrefix(value, "@"):
				record(s.file(strings.TrimPrefix(value, "@")))
			case key == "body":
				record(s.inline(o.value, true))
			case key == "title":
				record(s.inline(o.value, false))
			}
		}
	}
	return first, denied
}

// githubPostOption is one option of a gh command: the name as written and the word carrying its value,
// which is the empty word when the option ends the command.
type githubPostOption struct {
	name  string
	value githubPostWord
}

// githubPostOptions reads a gh command's options: a long option with its value after an equals sign or
// in the next word, and a short option in the same two forms. A value-taking shorthand ends its bundle,
// as pflag reads it (-bplain is -b with "plain", -dbplain is -d then -b "plain"), and a leading equals
// sign of an attached value is dropped. short names the value-taking shorthands to read.
func githubPostOptions(args []githubPostWord, short string) []githubPostOption {
	out := []githubPostOption{}
	for i := 0; i < len(args); i++ {
		w := args[i]
		switch {
		case w.text == "--":
			// Everything after the separator is an operand, which gh reads as a positional argument.
			return out
		case strings.HasPrefix(w.text, "--"):
			name, value, attached := strings.Cut(w.text, "=")
			if !attached {
				out = append(out, githubPostOption{name, githubPostNext(args, i)})
				continue
			}
			out = append(out, githubPostOption{name, githubPostWord{text: value, raw: w.raw}})
		case len(w.text) > 1 && w.text[0] == '-':
			letter, value, ok := githubPostShortFlag(w, short)
			if !ok {
				continue
			}
			if value == "" {
				out = append(out, githubPostOption{"-" + letter, githubPostNext(args, i)})
				continue
			}
			out = append(out, githubPostOption{"-" + letter, githubPostWord{text: strings.TrimPrefix(value, "="), raw: w.raw}})
		}
	}
	return out
}

// githubPostShortFlag is the letter and the attached value of a short option: the first letter of the
// bundle that takes a value, with everything after it as its value (empty when the option stands alone).
func githubPostShortFlag(w githubPostWord, short string) (string, string, bool) {
	for i := 1; i < len(w.text); i++ {
		if c := w.text[i]; strings.IndexByte(short, c) >= 0 {
			return string(c), w.text[i+1:], true
		}
	}
	return "", "", false
}

// githubPostNext is the word after the option at i, or an empty word when the option ends the command.
func githubPostNext(args []githubPostWord, i int) githubPostWord {
	if i+1 < len(args) {
		return args[i+1]
	}
	return githubPostWord{}
}

// inline judges text bound for GitHub that sits inside the command: an outer-shell expansion is denied
// first, then an inline body, then a secret pattern in a title that may stay inline.
func (s *githubPostScan) inline(w githubPostWord, body bool) (githubPostSite, bool) {
	if githubPostExpands(w.raw, true, false) {
		return githubPostSite{githubPostRuleExpand, githubPostWhereCommand}, true
	}
	if body {
		return githubPostSite{githubPostRuleInline, githubPostWhereCommand}, true
	}
	if _, found := githubPostSecretLine(w.text); found {
		return githubPostSite{githubPostRuleSecret, githubPostWhereCommand}, true
	}
	return githubPostSite{}, false
}

// file judges the text a post takes from a file: the file must be readable (a regular file of at most
// 1 MiB, relative to the payload's working directory) and hold no secret pattern. A name of "-" is
// standard input, which the guard reads from a quoted heredoc of the same command and denies otherwise,
// because what would be posted is then out of its sight.
func (s *githubPostScan) file(name string) (githubPostSite, bool) {
	if name == "" {
		return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
	}
	if name == "-" {
		if !s.hasBody {
			return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
		}
		if line, found := githubPostSecretLine(s.body); found {
			return githubPostSite{githubPostRuleSecret, "-:" + strconv.Itoa(line)}, true
		}
		return githubPostSite{}, false
	}
	content, ok := githubPostReadFile(name, s.cwd)
	if !ok {
		return githubPostSite{githubPostRuleUnread, name}, true
	}
	if line, found := githubPostSecretLine(content); found {
		return githubPostSite{githubPostRuleSecret, name + ":" + strconv.Itoa(line)}, true
	}
	return githubPostSite{}, false
}

// githubPostReadFile reads the text a post names: a regular file of at most githubPostMaxFileBytes.
func githubPostReadFile(name, cwd string) (string, bool) {
	path := name
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		// A home-relative name is a name the guard can read, so it is expanded before it is refused.
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		base := cwd
		if base == "" {
			base, _ = os.Getwd()
		}
		path = filepath.Join(base, path)
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

// githubPostSegments is the simple commands of one shell text: the shellwrite reader's split of pipes, ;
// and && within a line, and a newline ends a command too. Heredoc bodies are already gone, so their text
// is never read as one. A backslash before a newline joins the two lines before the shell splits words,
// so it is removed here too (the shell keeps it only inside single quotes).
func githubPostSegments(command string) []string {
	command = githubPostUncontinued(command)
	out := []string{}
	for _, line := range githubPostLines(command) {
		for _, segment := range splitShellSegments(utf16.Encode([]rune(line))) {
			if s := shellString(segment); text.Trim(s) != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// githubPostLines cuts a shell text at its newlines outside quotes: one line is one command line, whose
// own heredoc body belongs to it alone.
func githubPostLines(command string) []string {
	rs := []rune(command)
	out, cur := []string{}, strings.Builder{}
	for i := 0; i < len(rs); i++ {
		switch c := rs[i]; {
		case c == '\'' || c == '"':
			end, _ := githubPostQuotedEnd(rs, i)
			cur.WriteString(string(rs[i:end]))
			i = end - 1
		case c == '\n':
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(c)
		}
	}
	return append(out, cur.String())
}

// githubPostCommand is one command line with the quoted heredoc body of that same line.
type githubPostCommand struct {
	text    string
	body    string
	hasBody bool
}

// githubPostCommands is a shell text as its command lines: a heredoc body belongs to the line that
// introduces it, and the body lines that follow are not commands of their own.
func githubPostCommands(command string) []githubPostCommand {
	lines, out := githubPostLines(githubPostUncontinued(command)), []githubPostCommand{}
	for i := 0; i < len(lines); i++ {
		c := githubPostCommand{text: lines[i]}
		if body, after, ok := githubPostHeredocBody(lines[i], lines, i); ok {
			c.body, c.hasBody = body, true
			i = after
		}
		out = append(out, c)
	}
	return out
}

// githubPostUncontinued removes the backslash-newline sequences the shell drops before word splitting.
func githubPostUncontinued(text string) string {
	if !strings.Contains(text, "\\\n") {
		return text
	}
	rs, out, mode := []rune(text), strings.Builder{}, rune(0)
	for i := 0; i < len(rs); i++ {
		switch c := rs[i]; {
		case mode == '\'':
			if c == '\'' {
				mode = 0
			}
			out.WriteRune(c)
		case c == '\\' && i+1 < len(rs) && rs[i+1] == '\n':
			i++
		case c == '\'' && mode == 0:
			mode = '\''
			out.WriteRune(c)
		default:
			out.WriteRune(c)
		}
	}
	return out.String()
}

// githubPostStripped is the shell text with its heredoc bodies replaced, as the shellwrite reader reads
// it, and whether every quote of it closes: text the guard cannot split is judged by the fail-closed
// rule, because the split it would make is not the shell's.
func githubPostStripped(command string) (string, bool) {
	stripped := shellString(stripHeredocBodies(utf16.Encode([]rune(command))))
	rs := []rune(stripped)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\\':
			i++
		case '\'', '"':
			end, closed := githubPostQuotedEnd(rs, i)
			if !closed {
				return stripped, false
			}
			i = end - 1
		}
	}
	return stripped, true
}

// githubPostQuotedEnd is the index just past the quoted region that starts at i, and whether the quote
// closes at all.
func githubPostQuotedEnd(rs []rune, i int) (int, bool) {
	quote := rs[i]
	for i++; i < len(rs); i++ {
		if quote == '"' && rs[i] == '\\' {
			i++
			continue
		}
		if rs[i] == quote {
			return i + 1, true
		}
	}
	return len(rs), false
}

// githubPostWords splits one simple command into its words: whitespace separates words outside quotes, a
// quoted region is part of its word, and a backslash escapes the next character outside single quotes.
func githubPostWords(segment string) []githubPostWord {
	rs, out := []rune(segment), []githubPostWord{}
	raw, spelled, mode, started := strings.Builder{}, strings.Builder{}, rune(0), false
	flush := func() {
		if started {
			out = append(out, githubPostWord{spelled.String(), raw.String()})
			raw, spelled, started = strings.Builder{}, strings.Builder{}, false
		}
	}
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case mode == '\'':
			raw.WriteRune(c)
			if c == '\'' {
				mode = 0
			} else {
				spelled.WriteRune(c)
			}
		case mode == '"':
			switch {
			case c == '\\' && i+1 < len(rs) && githubPostEscapable(rs[i+1]):
				raw.WriteString(string(rs[i : i+2]))
				spelled.WriteRune(rs[i+1])
				i++
			case c == '"':
				raw.WriteRune(c)
				mode = 0
			default:
				raw.WriteRune(c)
				spelled.WriteRune(c)
			}
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
		case c == '\'' || c == '"':
			started, mode = true, c
			raw.WriteRune(c)
		case c == '\\' && i+1 < len(rs):
			started = true
			raw.WriteString(string(rs[i : i+2]))
			spelled.WriteRune(rs[i+1])
			i++
		default:
			started = true
			raw.WriteRune(c)
			spelled.WriteRune(c)
		}
	}
	flush()
	return out
}

// githubPostEscapable is the character a backslash escapes inside double quotes, where the shell keeps
// every other backslash as itself.
func githubPostEscapable(c rune) bool {
	return c == '"' || c == '\\' || c == '$' || c == 0x60 || c == '\n'
}

// githubPostExpands is whether the text as written holds an outer-shell expansion the shell acts on
// before gh sees the text: a backtick, a command substitution, a brace or name expansion, an arithmetic
// expansion or a process substitution, and with newline true a newline too. A single-quoted region is
// literal to the shell and holds none of them. With substitution true only the two the guard cannot read
// at all count, a backtick and a command substitution.
func githubPostExpands(raw string, newline, substitution bool) bool {
	rs, mode := []rune(raw), rune(0)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case mode == '\'':
			// A single-quoted region is literal to the shell: nothing inside it expands.
			if c == '\'' {
				mode = 0
			}
		case c == '\\':
			if mode != '"' || i+1 < len(rs) && githubPostEscapable(rs[i+1]) {
				i++
			}
		case c == '\'':
			mode = '\''
		case c == '"':
			if mode == '"' {
				mode = 0
			} else {
				mode = '"'
			}
		case c == 0x60:
			return true
		case c == '$' && i+1 < len(rs):
			next := rs[i+1]
			if next == '(' {
				return true
			}
			if !substitution && (next == '{' || next == '_' || next >= 'a' && next <= 'z' || next >= 'A' && next <= 'Z') {
				return true
			}
		case c == '<' && !substitution && i+1 < len(rs) && rs[i+1] == '(':
			return true
		case c == '\n' && newline:
			return true
		}
	}
	return false
}

// githubPostStripPrefixes is the index of the command a simple command starts with: the wrappers and
// assignments a shell drops before it, so a post behind a runner prefix is still read. The shellwrite
// reader's own list is kept, and a wrapper that runs the rest of the line is added, together with the
// NAME=value assignments a command may carry. A wrapper's own option word is skipped with it.
func githubPostStripPrefixes(words []githubPostWord) int {
	head := 0
	for head < len(words) {
		name := shellVerbName(words[head].text)
		switch {
		case name == "env":
			head++
			for head < len(words) && shellVerbAssignment(words[head].text) {
				head++
			}
		case name == "sudo" || name == "doas" || name == "command" || name == "builtin" ||
			name == "nohup" || name == "setsid" || name == "time" || name == "nice" || name == "ionice" ||
			name == "stdbuf" || name == "timeout" || name == "chrt" || name == "taskset":
			head = githubPostSkipWrapper(words, head, name)
		case shellVerbAssignment(words[head].text):
			head++
		default:
			return head
		}
	}
	return head
}

// githubPostSkipWrapper is the index just past a wrapper and its own arguments: the options of the
// wrapper, and the word that carries an option's value where the wrapper takes one.
func githubPostSkipWrapper(words []githubPostWord, head int, name string) int {
	value := func(word string) bool {
		switch name {
		case "timeout", "nice", "ionice", "chrt", "stdbuf":
			return word == ""
		}
		return false
	}
	i := head + 1
	for ; i < len(words); i++ {
		w := words[i].text
		if shellVerbAssignment(w) {
			continue
		}
		if w == "--" {
			return i + 1
		}
		if w == "" || w[0] != '-' {
			// The first operand of a wrapper that takes one is its argument; the command follows.
			switch {
			case name == "timeout":
				return i + 1
			case githubPostWrapperAdjustment(name) && githubPostNumeric(w):
				return i + 1
			}
			return i
		}
		if value(w) && i+1 < len(words) {
			return i + 2
		}
	}
	return i
}

// githubPostWrapperAdjustment is a wrapper whose operand is an adjustment it may also go without, so
// that the operand is skipped only when it is the adjustment and not the command itself.
func githubPostWrapperAdjustment(name string) bool {
	switch name {
	case "nice", "ionice", "chrt", "taskset":
		return true
	}
	return false
}

// githubPostNumeric is whether the word is a number or a range: an adjustment, not a command name.
func githubPostNumeric(word string) bool {
	if word == "" {
		return false
	}
	for _, c := range word {
		if c != '+' && c != '-' && c != '.' && c != ',' && c != 'x' && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// githubPostMentions is whether the text names gh with pr, issue or api: the words the fail-closed rule
// looks for in a program the guard cannot read. The text is read as written, so a name inside a quoted
// program still counts.
func githubPostMentions(text string) bool {
	return githubPostWordPresent(text, "gh") && (githubPostWordPresent(text, "pr") ||
		githubPostWordPresent(text, "issue") || githubPostWordPresent(text, "api"))
}

// githubPostWordPresent is whether the text holds the word with no word character beside it.
func githubPostWordPresent(text, word string) bool {
	for i := 0; i+len(word) <= len(text); i++ {
		if text[i:i+len(word)] != word {
			continue
		}
		if i > 0 && githubPostWordByte(text[i-1]) || i+len(word) < len(text) && githubPostWordByte(text[i+len(word)]) {
			continue
		}
		return true
	}
	return false
}

// githubPostWordByte is a byte that continues a shell word.
func githubPostWordByte(c byte) bool {
	return c == '_' || c == '-' || c == '.' || c == '/' ||
		c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

// githubPostHeredocBody is the body of the first heredoc of the text whose delimiter is quoted, the form
// the shell does not expand, and whether there is one. A heredoc whose delimiter is not quoted is not
// read: the shell expands its body, so the guard cannot know what would be posted. It answers with the
// index of the last line the body took, so the caller can continue after it.
func githubPostHeredocBody(line string, lines []string, at int) (string, int, bool) {
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\\':
			i++
		case '\'', '"':
			end, _ := githubPostQuotedEnd(rs, i)
			i = end - 1
		case '<':
			if i+1 < len(rs) && rs[i+1] == '<' && (i+2 >= len(rs) || rs[i+2] != '<') {
				if delim, ok := githubPostHeredocDelimiter(rs, i); ok {
					return githubPostHeredocBodyLines(lines, at+1, delim)
				}
			}
		}
	}
	return "", at, false
}

// githubPostHeredocDelimiter is the delimiter of the heredoc introduced by the << at index i, when that
// delimiter is quoted, the form the shell does not expand.
func githubPostHeredocDelimiter(rs []rune, i int) (string, bool) {
	j := i + 2
	if j < len(rs) && rs[j] == '-' {
		j++
	}
	for j < len(rs) && (rs[j] == ' ' || rs[j] == '\t') {
		j++
	}
	if j >= len(rs) || rs[j] != '\'' && rs[j] != '"' {
		return "", false
	}
	quote, start := rs[j], j+1
	for j = start; j < len(rs) && rs[j] != quote; j++ {
	}
	if j >= len(rs) || j == start {
		return "", false
	}
	return string(rs[start:j]), true
}

// githubPostHeredocBodyLines is the body of the heredoc whose quoted delimiter is delim: the lines after
// the introducing line, up to the line that is the delimiter, and the index of that line.
func githubPostHeredocBodyLines(lines []string, from int, delim string) (string, int, bool) {
	body := strings.Builder{}
	for i := from; i < len(lines); i++ {
		if strings.TrimLeft(lines[i], "\t") == delim {
			return body.String(), i, true
		}
		body.WriteString(lines[i])
		body.WriteString("\n")
	}
	return body.String(), len(lines), true
}
