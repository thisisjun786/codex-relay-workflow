package hook

// CRW-783: CRW's own protection, no CXC oracle. The PreToolUse guard over the shell tools stops a GitHub
// posting command whose text sits inside the command or holds an outer-shell expansion, and scans the
// GitHub-bound text for secrets, naming only the rule and the place in its reason.

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
	// GitHubPostMaxStdinBytes is this row's own stdin bound (1 MiB): over it the guard refuses rather than passing an input it could not judge.
	GitHubPostMaxStdinBytes = 1 << 20
)

// GitHubPostAnswer is the component row's whole input policy: the guard's answer for the payload, or the deny envelope when the payload is over the bound. A read that fails reads as empty input.
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

// HandleGitHubPostGuard is the guard's PreToolUse leg: the deny envelope for a GitHub post these rules stop, else empty. Another tool or event, and a command unrelated to GitHub, pass.
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

// githubPostDeny is the deny envelope: the rule, the place, and one way forward.
func githubPostDeny(rule, place string) string {
	reason := "GitHub post blocked (" + rule + ") at " + place +
		": write the text to a file, check it, and pass it with --body-file, -F body=@file or --input"
	return editAnswer("deny", reason, reason)
}

// githubPostSite is why and where a post is denied; githubPostWord is one word, unquoted and as written.
type githubPostSite struct{ rule, place string }
type githubPostWord struct{ text, raw string }
type githubPostOption struct {
	name  string
	value githubPostWord
}
type githubPostLine struct {
	text   string
	bodies []string
}

// githubPostScan is one payload's judgement: its working directory, the bodies and writes of the line it reads.
type githubPostScan struct {
	cwd    string
	bodies []string
	writes []string
	depth  int
}

// githubPostInput is tool_input's command or cmd: an argv array is already split, a string is shell text.
func githubPostInput(input map[string]any) ([]githubPostWord, string, bool) {
	var value any
	for _, key := range [...]string{"command", "cmd"} {
		if value != nil {
			break
		}
		value = input[key]
	}
	if items, ok := value.([]any); ok {
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
	text, _ := value.(string)
	return nil, text, false
}

// text reads one shell text: every simple command it holds, with its line's bodies and writes.
func (s *githubPostScan) text(command string) (githubPostSite, bool) {
	lines, balanced := githubPostLines(command)
	if !balanced {
		return githubPostUnread(command)
	}
	for _, line := range lines {
		s.bodies, s.writes = line.bodies, ShellWriteDestinations(line.text)
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

// githubPostUnread is the fail-closed judgement: an unreadable text naming a post is denied, else passed.
func githubPostUnread(command string) (githubPostSite, bool) {
	if githubPostMentions(command) {
		return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
	}
	return githubPostSite{}, false
}

// command judges one simple command: a gh post, a shell -c or eval program read deeper, or nothing.
func (s *githubPostScan) command(words []githubPostWord, segment string) (githubPostSite, bool) {
	rest := githubPostStripPrefixes(words)
	if len(rest) == 0 {
		return githubPostSite{}, false
	}
	verb := shellVerbName(rest[0].text)
	if verb == "gh" {
		return s.gh(rest[1:])
	}
	if s.depth <= 0 {
		return githubPostSite{}, false
	}
	program, ok := githubPostProgram(verb, rest)
	if !ok {
		return githubPostSite{}, false
	}
	if githubPostExpands(program, false, true) {
		return githubPostUnread(segment)
	}
	deeper := *s
	deeper.depth--
	return deeper.text(program)
}

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

// gh judges a gh command: pr create|edit|comment|review, issue create|comment|edit, or api with a field.
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
	if sub == "new" {
		sub = "create"
	}
	if pr && sub != "create" && sub != "edit" && sub != "comment" && sub != "review" {
		return githubPostSite{}, false
	}
	if !pr && sub != "create" && sub != "comment" && sub != "edit" {
		return githubPostSite{}, false
	}
	return s.flags(args[group+2:])
}

// flags judges a gh pr or gh issue command: -b and --body are inline, --body-file and -F name a file.
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
		case "--fill", "--fill-first", "--fill-verbose", "--template":
			site, no = githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
		}
		if no && !denied {
			first, denied = site, true
		}
	}
	return first, denied
}

// api judges gh api, a target only through a body or title field: -f, --raw-field, -F, --field, --input.
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
			if i := strings.LastIndexByte(key, '['); i >= 0 {
				key = strings.TrimSuffix(key[i+1:], "]")
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

// githubPostOptions reads a gh command's options, in the long and short forms pflag accepts.
func githubPostOptions(args []githubPostWord, short string) []githubPostOption {
	out := []githubPostOption{}
	for i := 0; i < len(args); i++ {
		w, next := args[i], githubPostWord{}
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

// inline judges GitHub-bound text inside the command: an expansion, then an inline body, then a secret.
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

// file judges the text a post takes from a file, a name of - being the quoted heredoc of its own line.
func (s *githubPostScan) file(name string) (githubPostSite, bool) {
	if name == "-" {
		if len(s.bodies) == 0 {
			return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
		}
		for _, body := range s.bodies {
			if line, found := githubPostSecretLine(body); found {
				return githubPostSite{githubPostRuleSecret, "-:" + strconv.Itoa(line)}, true
			}
		}
		return githubPostSite{}, false
	}
	for _, w := range s.writes {
		if name == "" || w == name || filepath.Clean(w) == filepath.Clean(name) {
			return githubPostSite{githubPostRuleUnread, name}, true
		}
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

// githubPostReadFile reads the text a post names: a regular file of at most 1 MiB, ~ expanded.
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

// githubPostLines is a shell text as its command lines, each with the quoted heredocs it introduces, and
// whether every quote closes: a text the guard cannot split is judged by the fail-closed rule.
func githubPostLines(command string) ([]githubPostLine, bool) {
	rs := []rune(githubPostUncontinued(command))
	out, cur := []githubPostLine{}, strings.Builder{}
	for i := 0; i < len(rs); i++ {
		if c := rs[i]; c == '\'' || c == '"' {
			end, closed := githubPostQuotedEnd(rs, i)
			if !closed {
				return nil, false
			}
			cur.WriteString(string(rs[i:end]))
			i = end - 1
			continue
		}
		if rs[i] == '\n' {
			line, after := githubPostHeredocLine(rs, i, cur.String())
			out = append(out, line)
			cur.Reset()
			i = after
			continue
		}
		cur.WriteRune(rs[i])
	}
	return append(out, githubPostLine{text: cur.String()}), true
}

// githubPostHeredocLine is the line ending at the newline at nl, with every quoted heredoc body it has.
func githubPostHeredocLine(rs []rune, nl int, line string) (githubPostLine, int) {
	delims := []string{}
	for i := 0; i < nl; i++ {
		if c := rs[i]; c == '\\' {
			i++
		} else if c == '\'' || c == '"' {
			end, _ := githubPostQuotedEnd(rs, i)
			i = end - 1
		} else if c == '<' && i+1 < nl && rs[i+1] == '<' && (i+2 >= nl || rs[i+2] != '<') {
			if delim, ok := githubPostHeredocDelimiter(rs, i); ok {
				delims = append(delims, delim)
			}
		}
	}
	out, from := githubPostLine{text: line}, nl+1
	for _, delim := range delims {
		body, after, ok := githubPostHeredocBody(rs, from, delim)
		if !ok {
			break
		}
		out.bodies, from = append(out.bodies, body), after+1
	}
	return out, from - 1
}

// githubPostHeredocDelimiter is the quoted delimiter of the heredoc at i; githubPostHeredocBody its body.
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

// githubPostHeredocBody is the body of the quoted heredoc whose delimiter is delim.
func githubPostHeredocBody(rs []rune, from int, delim string) (string, int, bool) {
	body, j := strings.Builder{}, from
	for j <= len(rs) {
		end := j
		for end < len(rs) && rs[end] != '\n' {
			end++
		}
		if strings.TrimLeft(string(rs[j:end]), "\t") == delim {
			return body.String(), end, true
		}
		body.WriteString(string(rs[j:end]) + "\n")
		if end >= len(rs) {
			break
		}
		j = end + 1
	}
	return body.String(), len(rs), true
}

func githubPostSegments(command string) []string {
	out := []string{}
	for _, segment := range splitShellSegments(utf16.Encode([]rune(command))) {
		if s := shellString(segment); text.Trim(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// githubPostUncontinued removes the backslash-newline joins the shell drops before splitting words.
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

// githubPostQuotedEnd is the index past the quoted region at i, and whether the quote closes at all.
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

// githubPostWords splits one simple command into its words, keeping each word's written form too.
func githubPostWords(segment string) []githubPostWord {
	rs, out := []rune(segment), []githubPostWord{}
	raw, spelled, started := strings.Builder{}, strings.Builder{}, false
	flush := func() {
		if started {
			out = append(out, githubPostWord{spelled.String(), raw.String()})
			raw, spelled, started = strings.Builder{}, strings.Builder{}, false
		}
	}
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
		case c == '\'' || c == '"':
			end, _ := githubPostQuotedEnd(rs, i)
			started = true
			raw.WriteString(string(rs[i:end]))
			spelled.WriteString(githubPostUnquoted(rs[i:end]))
			i = end - 1
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

// githubPostUnquoted is a quoted region as the shell reads it: quotes off, escapes inside double quotes.
func githubPostUnquoted(quoted []rune) string {
	out := strings.Builder{}
	for i := 1; i < len(quoted)-1; i++ {
		c := quoted[i]
		if quoted[0] == '"' && c == '\\' && i+1 < len(quoted)-1 && githubPostEscapable(quoted[i+1]) {
			i++
			c = quoted[i]
		}
		out.WriteRune(c)
	}
	return out.String()
}

// githubPostEscapable is what a backslash escapes inside double quotes; every other backslash stands.
func githubPostEscapable(c rune) bool {
	return c == '"' || c == '\\' || c == '$' || c == 0x60 || c == '\n'
}

// githubPostExpands is whether the text as written holds an outer-shell expansion outside single quotes.
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
			if !substitution && (next == '{' || next == '_' || strings.ContainsRune("0123456789@*#?$!-", next) ||
				next >= 'a' && next <= 'z' || next >= 'A' && next <= 'Z') {
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

// githubPostStripPrefixes is the command a simple command starts with: grouping words, wrappers, assignments.
func githubPostStripPrefixes(words []githubPostWord) []githubPostWord {
	for len(words) > 0 {
		w := words[0].text
		name := shellVerbName(w)
		if trimmed := strings.Trim(w, "({!});"); trimmed != w {
			if trimmed == "" {
				words = words[1:]
				continue
			}
			words[0] = githubPostWord{text: trimmed, raw: words[0].raw}
			continue
		}
		if w == "" {
			words = words[1:]
			continue
		}
		if !githubPostWrapper(name) && !shellVerbAssignment(w) {
			return words
		}
		words = words[1:]
		for len(words) > 0 {
			word := words[0].text
			if githubPostWrapperValue(name, word) && !strings.Contains(word, "=") && len(words) > 1 {
				words = words[2:]
				continue
			}
			if !githubPostWrapperWord(word) {
				break
			}
			words = words[1:]
		}
		if len(words) > 0 && words[0].text == "--" {
			words = words[1:]
		}
	}
	return words
}

// githubPostWrapper is a command that runs the rest of the line, so the post behind it is read.
func githubPostWrapper(name string) bool {
	for _, w := range [...]string{"env", "sudo", "doas", "command", "builtin", "nohup", "setsid", "time",
		"nice", "ionice", "stdbuf", "timeout", "chrt", "taskset"} {
		if name == w {
			return true
		}
	}
	return false
}

// githubPostWrapperWord is a word belonging to the wrapper: an assignment, an option, or a number.
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

// githubPostWrapperValue is a wrapper option that takes the next word as its value.
func githubPostWrapperValue(name, option string) bool {
	for _, v := range strings.Fields(githubPostWrapperValues(name)) {
		if option == v || strings.HasPrefix(option, v+"=") {
			return true
		}
	}
	return false
}

// githubPostWrapperValues is a wrapper's value-taking options, as its own help lists them; a function, so
// no package-level table is initialized at program start.
func githubPostWrapperValues(name string) string {
	for _, row := range [...]string{
		"env -u --unset -C --chdir -S --split-string",
		"sudo -u --user -g --group -p --prompt -C --close-from -h --host -U --other-user -R --chroot -r --role -t --type -D --chdir",
		"doas -u --user -C --config",
		"timeout -s --signal -k --kill-after",
		"nice -n --adjustment",
		"ionice -c --class -n --classdata -p --pid",
		"stdbuf -i --input -o --output -e --error",
		"chrt -p --pid -T --sched-runtime -P --sched-period -D --sched-deadline",
		"taskset -p --pid",
	} {
		if rest, ok := strings.CutPrefix(row, name+" "); ok {
			return rest
		}
	}
	return ""
}

// githubPostMentions is whether the text names gh with pr, issue or api, the fail-closed rule's words.
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
