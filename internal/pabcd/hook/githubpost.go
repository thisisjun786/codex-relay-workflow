package hook

// CRW-783: CRW's own protection, no CXC oracle. The PreToolUse guard over the shell tools stops a GitHub
// posting command whose text sits inside the command or holds an outer-shell expansion, and scans the text
// bound for GitHub (an inline title, or the file of --body-file, -F body=@F or --input) for secrets. The
// rules are inline-github-body, expansion-in-github-text, secret-in-github-text and unreadable-github-post,
// and the deny reason names the rule and the place, never the value.

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
	githubPostDepth        = 8       // how deep a shell -c or eval program is read, so the work stays linear
	githubPostMaxFileBytes = 1 << 20 // the most the guard reads of a file a post names (1 MiB)
	// GitHubPostMaxStdinBytes is this row's own stdin bound (1 MiB): over it the guard refuses rather than
	// passing an input it could not judge.
	GitHubPostMaxStdinBytes = 1 << 20
)

// GitHubPostAnswer is the component row's whole input policy: the guard's answer for the payload, or the deny
// envelope when the payload is over the bound. A read that fails reads as empty input.
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

// HandleGitHubPostGuard is the guard's PreToolUse leg: the deny envelope for a GitHub post these rules stop,
// else empty. Another tool or event, and a command unrelated to GitHub, pass.
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
	var site githubPostSite
	var denied bool
	if argv {
		site, denied = s.command(words, "")
	} else if text.Trim(command) != "" {
		site, denied = s.text(command)
	}
	if !denied {
		return ""
	}
	return githubPostDeny(site.rule, site.place)
}

// githubPostDeny is the deny envelope: the rule and the place, then one way forward. The text and the matched
// pattern stay out of it.
func githubPostDeny(rule, place string) string {
	reason := "GitHub post blocked (" + rule + ") at " + place +
		": write the text to a file, check it, and pass it with --body-file, -F body=@file or --input"
	return editAnswer("deny", reason, reason)
}

// githubPostSite is why and where a post is denied; githubPostWord is one word, text unquoted and raw as
// written; githubPostOption is one option with its value word; githubPostCommand is one command line with its
// quoted heredoc body.
type githubPostSite struct{ rule, place string }
type githubPostWord struct{ text, raw string }
type githubPostOption struct {
	name  string
	value githubPostWord
}
type githubPostCommand struct {
	text    string
	body    string
	hasBody bool
}

// githubPostScan is one payload's judgement: its working directory, the heredoc body of the line being read,
// and the depth left.
type githubPostScan struct {
	cwd, body string
	hasBody   bool
	depth     int
}

// githubPostInput is tool_input's command or cmd: an argv array becomes already-split words, and a string is
// shell text.
func githubPostInput(input map[string]any) ([]githubPostWord, string, bool) {
	for _, key := range [...]string{"command", "cmd"} {
		if items, ok := input[key].([]any); ok {
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
	}
	for _, key := range [...]string{"command", "cmd"} {
		if s, ok := input[key].(string); ok {
			return nil, s, false
		}
	}
	return nil, "", false
}

// text reads one shell text: every simple command it holds, each with the quoted heredoc body of its own
// line. A text it cannot split or read, naming a GitHub post, is denied as unreadable.
func (s *githubPostScan) text(command string) (githubPostSite, bool) {
	if _, balanced := githubPostStripped(command); !balanced {
		return githubPostUnread(command)
	}
	for _, line := range githubPostCommands(command) {
		s.body, s.hasBody = line.body, line.hasBody
		for _, segment := range githubPostSegments(line.text) {
			if site, denied := s.command(githubPostWords(segment), segment); denied {
				return site, true
			}
			// A gh post inside a command substitution is a text the guard cannot read.
			if githubPostExpands(segment, false, true) {
				return githubPostUnread(segment)
			}
		}
	}
	return githubPostSite{}, false
}

// githubPostUnread is the fail-closed judgement: a text the guard cannot read is denied when it names a
// GitHub post, and passed when it does not.
func githubPostUnread(command string) (githubPostSite, bool) {
	if githubPostMentions(command) {
		return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
	}
	return githubPostSite{}, false
}

// command judges one simple command: a gh posting command, a shell -c or eval program read one level deeper,
// or nothing.
func (s *githubPostScan) command(words []githubPostWord, segment string) (githubPostSite, bool) {
	head := githubPostStripPrefixes(words)
	if head >= len(words) {
		return githubPostSite{}, false
	}
	verb := shellVerbName(words[head].text)
	if verb == "gh" {
		return s.gh(words[head+1:])
	}
	if s.depth <= 0 {
		return githubPostSite{}, false
	}
	program, ok := githubPostProgram(verb, words[head:])
	if !ok {
		return githubPostSite{}, false
	}
	// A program holding a command substitution cannot be read at all; anything else is read one level down.
	if githubPostExpands(program, false, true) {
		return githubPostUnread(segment)
	}
	s.depth--
	return s.text(program)
}

// githubPostProgram is the command string a shell -c or eval would run, when this command starts one.
func githubPostProgram(verb string, words []githubPostWord) (string, bool) {
	rest := make([]string, len(words))
	for i, w := range words {
		rest[i] = w.text
	}
	if shellVerbIsShell(verb) {
		return shellVerbShellScript(rest[1:])
	}
	if verb == "eval" {
		return strings.Join(rest[1:], " "), true
	}
	return "", false
}

// gh judges a gh command: pr create|edit|comment|review, issue create|comment|edit, or api with a body or
// title field; gh pr view|list|checks|diff and gh api without one are not targets.
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
	sub, pr := args[group+1].text, args[group].text == "pr"
	if pr && sub != "create" && sub != "edit" && sub != "comment" && sub != "review" {
		return githubPostSite{}, false
	}
	if !pr && sub != "create" && sub != "comment" && sub != "edit" {
		return githubPostSite{}, false
	}
	return s.flags(args[group+2:])
}

// flags judges a gh pr or gh issue command: -b and --body carry the body inside the command, --body-file and
// -F name a file, and -t and --title may stay inline unless they hold an expansion or a secret. Every option is read.
func (s *githubPostScan) flags(args []githubPostWord) (githubPostSite, bool) {
	first, denied := githubPostSite{}, false
	for _, o := range githubPostOptions(args, "btF") {
		site, no := githubPostSite{}, false
		switch o.name {
		case "-b", "--body":
			site, no = s.inline(o.value, true)
		case "-t", "--title":
			site, no = s.inline(o.value, false)
		case "-F", "--body-file":
			site, no = s.file(o.value.text)
		}
		if no && !denied {
			first, denied = site, true
		}
	}
	return first, denied
}

// api judges gh api, which is a target only through a body or title field: -f and --raw-field carry text, -F
// and --field carry text or @file, and --input names a file.
func (s *githubPostScan) api(args []githubPostWord) (githubPostSite, bool) {
	first, denied := githubPostSite{}, false
	for _, o := range githubPostOptions(args, "fF") {
		site, no := githubPostSite{}, false
		switch {
		case o.name == "--input":
			site, no = s.file(o.value.text)
		case o.name == "-f" || o.name == "--raw-field" || o.name == "-F" || o.name == "--field":
			key, value, ok := strings.Cut(o.value.text, "=")
			if !ok {
				continue
			}
			switch {
			case key == "body" && (o.name == "-F" || o.name == "--field") && strings.HasPrefix(value, "@"):
				site, no = s.file(strings.TrimPrefix(value, "@"))
			case key == "body":
				site, no = s.inline(o.value, true)
			case key == "title":
				site, no = s.inline(o.value, false)
			}
		}
		if no && !denied {
			first, denied = site, true
		}
	}
	return first, denied
}

// githubPostOptions reads a gh command's options: a long option with its value after an equals sign or in the
// next word, and a short option in the same two forms. A value-taking shorthand ends its bundle, as pflag
// reads it, and a -- ends the options.
func githubPostOptions(args []githubPostWord, short string) []githubPostOption {
	out := []githubPostOption{}
	for i := 0; i < len(args); i++ {
		w := args[i]
		next := githubPostWord{}
		if i+1 < len(args) {
			next = args[i+1]
		}
		switch {
		case w.text == "--":
			return out
		case strings.HasPrefix(w.text, "--"):
			name, value, attached := strings.Cut(w.text, "=")
			if !attached {
				out = append(out, githubPostOption{name, next})
				continue
			}
			out = append(out, githubPostOption{name, githubPostWord{text: value, raw: w.raw}})
		case len(w.text) > 1 && w.text[0] == '-':
			letter, value := "", ""
			for j := 1; j < len(w.text) && letter == ""; j++ {
				if c := w.text[j]; strings.IndexByte(short, c) >= 0 {
					letter, value = string(c), w.text[j+1:]
				}
			}
			if letter == "" {
				continue
			}
			if value == "" {
				out = append(out, githubPostOption{"-" + letter, next})
				continue
			}
			out = append(out, githubPostOption{"-" + letter, githubPostWord{text: strings.TrimPrefix(value, "="), raw: w.raw}})
		}
	}
	return out
}

// inline judges text bound for GitHub inside the command: an expansion first, then an inline body, then a
// secret pattern in a title.
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

// file judges the text a post takes from a file: readable (a regular file of at most 1 MiB, under the
// payload's working directory) and holding no secret pattern. A name of - is standard input, read from the
// quoted heredoc of the same line and denied otherwise.
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

// githubPostReadFile reads the text a post names: a regular file of at most githubPostMaxFileBytes, with a
// home-relative name expanded.
func githubPostReadFile(name, cwd string) (string, bool) {
	path := name
	if strings.HasPrefix(path, "~/") {
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

// githubPostCommands is a shell text as its command lines: a heredoc body belongs to the line introducing it
// and its body lines are not commands. githubPostSegments is one line's simple commands, split by the
// shellwrite reader.
func githubPostCommands(command string) []githubPostCommand {
	lines, out := githubPostLines(githubPostUncontinued(command)), []githubPostCommand{}
	for i := 0; i < len(lines); i++ {
		c := githubPostCommand{text: lines[i]}
		if body, after, ok := githubPostHeredoc(lines[i], lines, i); ok {
			c.body, c.hasBody = body, true
			i = after
		}
		out = append(out, c)
	}
	return out
}

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

func githubPostSegments(command string) []string {
	out := []string{}
	for _, segment := range splitShellSegments(utf16.Encode([]rune(githubPostUncontinued(command)))) {
		if s := shellString(segment); text.Trim(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// githubPostUncontinued removes the backslash-newline sequences the shell drops before word splitting (it
// keeps the backslash only inside single quotes).
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
		case c == '\'':
			mode = '\''
			out.WriteRune(c)
		default:
			out.WriteRune(c)
		}
	}
	return out.String()
}

// githubPostStripped is the shell text with its heredoc bodies replaced, as the shellwrite reader reads it,
// and whether every quote closes.
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

// githubPostQuotedEnd is the index just past the quoted region that starts at i, and whether the quote closes at all.
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
// quoted region is part of its word, and a backslash escapes what follows.
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

// githubPostEscapable is the character a backslash escapes inside double quotes, where the shell keeps every
// other backslash as itself.
func githubPostEscapable(c rune) bool {
	return c == '"' || c == '\\' || c == '$' || c == 0x60 || c == '\n'
}

// githubPostExpands is whether the text as written holds an outer-shell expansion the shell acts on before gh
// sees it: a backtick, a command substitution, a brace or name expansion, an arithmetic or process
// substitution, and with newline true a newline. A single-quoted region is literal to the shell; with.
func githubPostExpands(raw string, newline, substitution bool) bool {
	rs, mode := []rune(raw), rune(0)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case mode == '\'':
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
// assignments a shell drops before it, so a post behind a runner prefix is still read, its own options and
// numbers going with it.
func githubPostStripPrefixes(words []githubPostWord) int {
	head := 0
	for head < len(words) {
		name := shellVerbName(words[head].text)
		if !githubPostWrapper(name) && !shellVerbAssignment(words[head].text) {
			return head
		}
		head++
		for head < len(words) && githubPostWrapperWord(words[head].text) {
			head++
		}
		if head < len(words) && words[head].text == "--" {
			head++
		}
	}
	return head
}

// githubPostWrapper is a command that runs the rest of the line, so the post behind it is still read.
func githubPostWrapper(name string) bool {
	for _, w := range [...]string{"env", "sudo", "doas", "command", "builtin", "nohup", "setsid", "time",
		"nice", "ionice", "stdbuf", "timeout", "chrt", "taskset"} {
		if name == w {
			return true
		}
	}
	return false
}

// githubPostWrapperWord is a word belonging to the wrapper, not the command it runs: an assignment, an
// option, or a number an option or the wrapper takes.
func githubPostWrapperWord(word string) bool {
	if word == "" {
		return false
	}
	if word[0] == '-' {
		return word != "--"
	}
	if shellVerbAssignment(word) {
		return true
	}
	for _, c := range word {
		if c != '+' && c != '-' && c != '.' && c != ',' && c != 'x' && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// githubPostMentions is whether the text names gh with pr, issue or api: the words the fail-closed rule looks
// for, read as written.
func githubPostMentions(text string) bool {
	return githubPostWordPresent(text, "gh") && (githubPostWordPresent(text, "pr") ||
		githubPostWordPresent(text, "issue") || githubPostWordPresent(text, "api"))
}

func githubPostWordPresent(text, word string) bool {
	for i := 0; i+len(word) <= len(text); i++ {
		if text[i:i+len(word)] == word && !githubPostWordByte(text[i-1:i]) && !githubPostWordByte(text[i+len(word):i+len(word)+1]) {
			return true
		}
	}
	return false
}

func githubPostWordByte(rest string) bool {
	if rest == "" {
		return false
	}
	c := rest[0]
	return c == '_' || c == '-' || c == '.' || c == '/' ||
		c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

// githubPostHeredoc is the body of the first heredoc of the line whose delimiter is quoted, the form the
// shell does not expand (an unquoted delimiter is not read, because the shell expands that body), and the
// index of the line it ended on.
func githubPostHeredoc(line string, lines []string, at int) (string, int, bool) {
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
					return githubPostHeredocBody(lines, at+1, delim)
				}
			}
		}
	}
	return "", at, false
}

// githubPostHeredocDelimiter is the delimiter of the heredoc introduced by the << at index i when it is
// quoted; githubPostHeredocBody is its body, up to the delimiter line, with that line's index.
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

func githubPostHeredocBody(lines []string, from int, delim string) (string, int, bool) {
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
