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
	// GitHubPostMaxStdinBytes is this row's own stdin bound (1 MiB): over it the guard refuses rather than
	// passing an input it could not judge.
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

// githubPostSite is why and where a post is denied; githubPostWord is one word, unquoted and as written;
// githubPostOption is one option with its value word; githubPostLine is one line with its heredoc bodies.
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

// githubPostScan is one payload's judgement: its working directory, the bodies and writes of the line it
// reads, and the depth left.
type githubPostScan struct {
	cwd string
	// whole is the shell text being judged at this level. A command word the shell would expand names a
	// command the guard cannot read, and the text it runs is judged as unreadable against this text, not
	// against the segment alone (which may not hold the word gh the rule's fail-closed test needs).
	whole  string
	bodies []string
	writes []string
	depth  int
}

// githubPostInput is tool_input's command or cmd: an argv array is already split, a string is shell text.
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
		if shell, ok := input[key].(string); ok {
			return nil, shell, false
		}
	}
	return nil, "", false
}

// text reads one shell text: every simple command it holds, with its own line's bodies and writes. A text
// it cannot split or read, naming a GitHub post, is denied as unreadable.
func (s *githubPostScan) text(command string) (githubPostSite, bool) {
	lines, balanced := githubPostLines(command)
	if !balanced {
		return githubPostUnread(command)
	}
	s.whole = command
	for _, line := range lines {
		s.bodies, s.writes = line.bodies, ShellWriteDestinations(line.text)
		for _, segment := range githubPostSegments(line.text) {
			if site, denied := s.command(githubPostWords(segment), segment); denied {
				return site, true
			}
			// A gh post inside a command substitution is a text the guard cannot read.
			if githubPostExpands(segment, true) {
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

// command judges one simple command: a gh post, a program read deeper (a shell -c, su --command, a
// quoted heredoc that feeds a shell, env --split-string, eval), a command that names the post later in
// the same line (find's -exec), a program in a language the guard cannot read that names a post, or
// nothing. The depth is spent by the recursive read alone, so sibling programs each get their own
// allowance.
func (s *githubPostScan) command(words []githubPostWord, segment string) (githubPostSite, bool) {
	rest, split := githubPostStripPrefixes(words)
	if split != "" {
		return s.program(split, segment)
	}
	if len(rest) == 0 {
		return githubPostSite{}, false
	}
	// The shell decides which command a word holding an expansion runs, so the guard cannot read it: the
	// text is judged as unreadable against the whole command text, not the segment alone.
	if githubPostExpands(rest[0].raw, false) {
		return githubPostUnread(s.whole)
	}
	verb := shellVerbName(rest[0].text)
	if verb == "gh" {
		return s.gh(rest[1:])
	}
	if verb == "find" {
		for i, w := range rest {
			if (w.text == "-exec" || w.text == "-execdir") && i+1 < len(rest) {
				return s.command(rest[i+1:], segment)
			}
		}
		return githubPostSite{}, false
	}
	spelled := make([]string, len(rest))
	for i, w := range rest {
		spelled[i] = w.text
	}
	program, ok := githubPostProgram(verb, spelled[1:], s.bodies)
	if !ok {
		// A program in a language the guard cannot read, naming a post, is refused: the guard never
		// runs it through its own reader, so passing it would be a fail-open.
		if githubPostForeignProgram(verb, spelled[1:]) {
			return githubPostUnread(segment)
		}
		return githubPostSite{}, false
	}
	return s.program(program, segment)
}

// program reads a program text one level down, spending one level of the depth. A program the guard can
// no longer read, or one holding an expansion it cannot see the result of, is refused: it is never
// allowed because the depth or the reading ran out.
func (s *githubPostScan) program(program, segment string) (githubPostSite, bool) {
	if s.depth <= 0 {
		return githubPostUnread(program)
	}
	if githubPostExpands(program, true) {
		return githubPostUnread(segment)
	}
	deeper := *s
	deeper.depth--
	return deeper.text(program)
}

// githubPostProgram is the program text a simple command runs one level down: eval's words, a shell's
// -c operand or the quoted heredoc that feeds the shell on standard input, and su's --command operand.
func githubPostProgram(verb string, args []string, bodies []string) (string, bool) {
	switch {
	case verb == "eval":
		return strings.Join(args, " "), true
	case shellVerbIsShell(verb):
		if program, ok := shellVerbShellScript(args); ok {
			return program, true
		}
		// A shell with no -c reads its program from standard input, the quoted heredoc of its line.
		if len(bodies) > 0 {
			return bodies[0], true
		}
		return "", false
	case verb == "su":
		return githubPostSuCommand(args)
	}
	return "", false
}

// githubPostSuCommand is su's --command operand, the program the target user's shell runs. su's other
// value-taking options are skipped and its user operand is not the program.
func githubPostSuCommand(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			continue
		case a == "-c" || a == "--command" || a == "--session-command":
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		case strings.HasPrefix(a, "--command="):
			return strings.TrimPrefix(a, "--command="), true
		case strings.HasPrefix(a, "--session-command="):
			return strings.TrimPrefix(a, "--session-command="), true
		case len(a) > 1 && a[0] == '-' && a[1] != '-':
			// A short bundle: a value-taking letter ends it, and an attached value is the program.
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 'c':
					if j+1 < len(a) {
						return a[j+1:], true
					}
					if i+1 < len(args) {
						return args[i+1], true
					}
					return "", false
				case 's', 'g', 'G', 'w':
					i++ // this letter takes the next word as its value
					j = len(a)
				}
			}
		}
	}
	return "", false
}

// githubPostForeignProgram is a one-line program in a language the guard cannot read (python, node,
// perl, ruby, lua, php, awk and their kin) that names a post. Its text never goes through the shell
// reader, so the guard refuses it rather than allowing a post it did not judge.
func githubPostForeignProgram(verb string, args []string) bool {
	program, ok := githubPostForeignOperand(verb, args)
	return ok && githubPostMentions(program)
}

// githubPostForeignOperand is the program of a one-line interpreter: the option that carries it (python
// -c, node -e or -p, perl, ruby and lua -e, php -r), or awk's first operand.
func githubPostForeignOperand(verb string, args []string) (string, bool) {
	if githubPostForeignAwk(verb) {
		return githubPostAwkProgram(args)
	}
	short, long := githubPostForeignOption(verb)
	if short == "" {
		return "", false
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return "", false
		case a == "":
			continue
		case a[0] != '-' || a == "-":
			return "", false
		case strings.HasPrefix(a, "--"):
			name, value, attached := strings.Cut(a, "=")
			if !strings.Contains(" "+long+" ", " "+name+" ") {
				continue
			}
			if attached {
				return value, true
			}
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		default:
			for j := 1; j < len(a); j++ {
				if !strings.ContainsRune(short, rune(a[j])) {
					continue
				}
				if j+1 < len(a) {
					return a[j+1:], true
				}
				if i+1 < len(args) {
					return args[i+1], true
				}
				return "", false
			}
		}
	}
	return "", false
}

// githubPostForeignAwk is the awk family, whose program is its first operand rather than an option's value.
func githubPostForeignAwk(verb string) bool {
	for _, name := range [...]string{"awk", "gawk", "mawk", "nawk"} {
		if verb == name {
			return true
		}
	}
	return false
}

// githubPostAwkProgram is awk's program, the first operand. A program read from a file (-f, --file) is
// not the command text, so the guard reads nothing.
func githubPostAwkProgram(args []string) (string, bool) {
	for _, a := range args {
		switch {
		case a == "--":
			return "", false
		case a == "":
			continue
		case a == "-f" || a == "--file" || strings.HasPrefix(a, "--file=") || strings.HasPrefix(a, "-f") && len(a) > 2:
			return "", false
		case a[0] != '-' || a == "-":
			return a, true
		}
	}
	return "", false
}

// githubPostForeignOption is an interpreter's program options: the short letters and the long names.
func githubPostForeignOption(verb string) (string, string) {
	switch {
	case verb == "python" || verb == "python2" || verb == "python3" || shellVerbVersioned(verb):
		return "c", "command"
	case verb == "node" || verb == "nodejs":
		return "ep", "eval print"
	case verb == "perl" || verb == "ruby" || verb == "lua" || verb == "luajit":
		return "e", ""
	case verb == "php":
		return "r", ""
	}
	return "", ""
}

// gh judges a gh command: pr create|edit|comment|review, issue create|comment|edit, or api with a field;
// gh pr view|list|checks|diff and gh api without one are not targets, and new is read as create.
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

// flags judges a gh pr or gh issue command: -b and --body are inline, --body-file and -F name a file, and
// -t and --title may stay inline unless they hold an expansion or a secret. Every option is read. --fill
// and --template are denied: the text they post comes from the repository, not from the command.
func (s *githubPostScan) flags(args []githubPostWord) (githubPostSite, bool) {
	return githubPostFirst(githubPostOptions(args, "btF"), func(o githubPostOption) (githubPostSite, bool) {
		switch o.name {
		case "-b", "--body":
			return s.inline(o.value, true)
		case "-t", "--title":
			return s.inline(o.value, false)
		case "-F", "--body-file":
			return s.file(o.value)
		case "--fill", "--fill-first", "--fill-verbose", "--template":
			return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
		}
		return githubPostSite{}, false
	})
}

// api judges gh api, a target only through a body or title field: -f, --raw-field, -F, --field, --input. A
// nested key is read by both its top-level and its terminal field (body[text], input[body]).
func (s *githubPostScan) api(args []githubPostWord) (githubPostSite, bool) {
	return githubPostFirst(githubPostOptions(args, "fF"), func(o githubPostOption) (githubPostSite, bool) {
		if o.name == "--input" {
			return s.file(o.value)
		}
		if o.name != "-f" && o.name != "--raw-field" && o.name != "-F" && o.name != "--field" {
			return githubPostSite{}, false
		}
		key, value, ok := strings.Cut(o.value.text, "=")
		if !ok {
			return githubPostSite{}, false
		}
		file := o.name == "-F" || o.name == "--field"
		// A nested key is read by its top-level and its terminal field, the two GitHub reads.
		fields := []string{key}
		if i := strings.IndexByte(key, '['); i >= 0 {
			fields = append(fields, key[:i])
		}
		if i := strings.LastIndexByte(key, '['); i >= 0 {
			fields = append(fields, strings.TrimSuffix(key[i+1:], "]"))
		}
		for _, field := range fields {
			switch {
			case field == "body" && file && strings.HasPrefix(value, "@"):
				if site, no := s.file(githubPostWord{text: strings.TrimPrefix(value, "@")}); no {
					return site, true
				}
			case field == "body":
				if site, no := s.inline(o.value, true); no {
					return site, true
				}
			case field == "title":
				if site, no := s.inline(o.value, false); no {
					return site, true
				}
			}
		}
		return githubPostSite{}, false
	})
}

// githubPostFirst judges every option and answers the first denial, so a field after a clean one is judged.
func githubPostFirst(options []githubPostOption, judge func(githubPostOption) (githubPostSite, bool)) (githubPostSite, bool) {
	for _, o := range options {
		if site, denied := judge(o); denied {
			return site, true
		}
	}
	return githubPostSite{}, false
}

// githubPostOptions reads a gh command's options, in the long and short forms pflag accepts. A value-taking
// shorthand ends its bundle, as pflag reads it (-bplain is -b with "plain"), a -- ends the options, and a
// value taken from the next word is not read again as an option of its own.
func githubPostOptions(args []githubPostWord, short string) []githubPostOption {
	out := []githubPostOption{}
	for i := 0; i < len(args); i++ {
		w := args[i]
		switch {
		case w.text == "--":
			return out
		case strings.HasPrefix(w.text, "--"):
			if name, value, attached := strings.Cut(w.text, "="); attached {
				out = append(out, githubPostOption{name, githubPostWord{text: value, raw: w.raw}})
			} else if i+1 < len(args) {
				out, i = append(out, githubPostOption{name, args[i+1]}), i+1
			} else {
				out = append(out, githubPostOption{name: name})
			}
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
			switch {
			case value != "":
				out = append(out, githubPostOption{"-" + letter, githubPostWord{text: strings.TrimPrefix(value, "="), raw: w.raw}})
			case i+1 < len(args):
				out, i = append(out, githubPostOption{"-" + letter, args[i+1]}), i+1
			default:
				out = append(out, githubPostOption{name: "-" + letter})
			}
		}
	}
	return out
}

// inline judges GitHub-bound text inside the command: an expansion, then an inline body, then a secret.
func (s *githubPostScan) inline(w githubPostWord, body bool) (githubPostSite, bool) {
	if githubPostExpands(w.raw, false) {
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
// payload's working directory), not written by the same line, and holding no secret pattern. A name of -
// is standard input, read from the quoted heredocs of its own line and denied otherwise; a name the shell
// would expand names a file the guard cannot know, so it is denied too.
func (s *githubPostScan) file(w githubPostWord) (githubPostSite, bool) {
	name := w.text
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
	if name == "" || githubPostExpands(w.raw, false) {
		return githubPostSite{githubPostRuleUnread, name}, true
	}
	for _, write := range s.writes {
		if write == name || filepath.Clean(write) == filepath.Clean(name) {
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
		if base := cwd; base != "" {
			path = filepath.Join(base, path)
		} else if wd, err := os.Getwd(); err == nil {
			path = filepath.Join(wd, path)
		}
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
// whether every quote closes: a newline inside quotes is not a line end, a text the guard cannot split is
// judged by the fail-closed rule, and a heredoc body belongs to the line that introduces it.
func githubPostLines(command string) ([]githubPostLine, bool) {
	rs := []rune(githubPostUncontinued(command))
	out := []githubPostLine{}
	for i := 0; i < len(rs); {
		start, delims := i, []string{}
		for i < len(rs) && rs[i] != '\n' {
			switch c := rs[i]; {
			case c == '\\':
				i += 2
			case c == '\'' || c == '"':
				end, closed := githubPostQuotedEnd(rs, i)
				if !closed {
					return nil, false
				}
				i = end
			case c == '<' && i+2 < len(rs) && rs[i+1] == '<' && rs[i+2] == '<':
				// A here-string (<<<) is a word, not a heredoc: its text is not a body the guard reads,
				// so the file it would feed stays unreadable.
				i += 3
			case c == '<' && i+1 < len(rs) && rs[i+1] == '<':
				// A heredoc operator: its quoted delimiter names a body the shell does not expand.
				if delim, ok := githubPostHeredocDelimiter(rs, i); ok {
					delims = append(delims, delim)
				}
				i += 2
			default:
				i++
			}
		}
		line, from := githubPostLine{text: string(rs[start:i])}, i+1
		for _, delim := range delims {
			body, after := githubPostHeredocBody(rs, from, delim)
			line.bodies, from = append(line.bodies, body), after
		}
		i = from
		out = append(out, line)
	}
	return out, true
}

// githubPostHeredocDelimiter is the quoted delimiter of the heredoc introduced by the << at index i, and
// whether there is one. An unquoted delimiter is not read, because the shell expands that body.
func githubPostHeredocDelimiter(rs []rune, i int) (string, bool) {
	j := i + 2
	if j < len(rs) && rs[j] == '-' {
		j++
	}
	for j < len(rs) && (rs[j] == ' ' || rs[j] == '\t') {
		j++
	}
	if j+1 >= len(rs) || rs[j] != '\'' && rs[j] != '"' {
		return "", false
	}
	end, _ := githubPostQuotedEnd(rs, j)
	if end <= j+1 {
		return "", false
	}
	return string(rs[j+1 : end-1]), true
}

func githubPostHeredocBody(rs []rune, from int, delim string) (string, int) {
	body, j := strings.Builder{}, from
	for j <= len(rs) {
		end := j
		for end < len(rs) && rs[end] != '\n' {
			end++
		}
		if strings.TrimLeft(string(rs[j:end]), "\t") == delim {
			return body.String(), end + 1
		}
		body.WriteString(string(rs[j:end]) + "\n")
		if end >= len(rs) {
			break
		}
		j = end + 1
	}
	return body.String(), len(rs)
}

// githubPostSegments is one command line's simple commands, split by the shellwrite reader.
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

// githubPostWords splits one simple command into its words, keeping each word's written form too: a quoted
// region is one word, read as the shell reads it, and a backslash escapes what follows.
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

// githubPostExpands is whether the text as written holds an outer-shell expansion the shell acts on before
// gh sees it: a backtick, a command substitution, a brace, name, digit or special parameter, an arithmetic
// or process substitution, and a newline. A single-quoted region is literal to the shell. With unreadable
// true only the two whose result the guard cannot see at all count, a backtick and a command substitution.
func githubPostExpands(raw string, unreadable bool) bool {
	rs, mode := []rune(raw), rune(0)
	for i := 0; i < len(rs); i++ {
		switch c := rs[i]; {
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
			if next := rs[i+1]; next == '(' {
				return true
			} else if !unreadable && (next == '{' || next == '_' || strings.ContainsRune("0123456789@*#?$!-", next) ||
				next >= 'a' && next <= 'z' || next >= 'A' && next <= 'Z') {
				return true
			}
		case c == '<' && !unreadable && i+1 < len(rs) && rs[i+1] == '(':
			return true
		case c == '\n' && !unreadable:
			return true
		}
	}
	return false
}

// githubPostStripPrefixes is the command a simple command starts with: grouping words, wrappers and
// assignments. A post in (gh ...), { gh ...; } or ! gh ... is still read, and each wrapper's own options,
// option values and numbers go with it, so a post behind a runner prefix (timeout, xargs) is read too.
// env's --split-string value is a whole command line rather than a command, and is returned as one.
func githubPostStripPrefixes(words []githubPostWord) ([]githubPostWord, string) {
	out := []githubPostWord{}
	for _, w := range words {
		if t := strings.TrimLeft(w.text, "({!"); t != "" && t != "}" {
			out = append(out, githubPostWord{text: t, raw: w.raw})
		}
	}
	words, head := out, 0
	for head < len(words) {
		name := shellVerbName(words[head].text)
		if !githubPostWrapper(name) && !shellVerbAssignment(words[head].text) {
			return words[head:], ""
		}
		head++
		for head < len(words) {
			word := words[head].text
			if split, ok := githubPostSplitString(name, word, words, head); ok {
				return nil, split
			}
			if githubPostWrapperValue(name, word) && !strings.Contains(word, "=") && head+1 < len(words) {
				head += 2
				continue
			}
			if !githubPostWrapperWord(word) {
				break
			}
			head++
		}
		if head < len(words) && words[head].text == "--" {
			head++
		}
	}
	return words[head:], ""
}

// githubPostSplitString is env's --split-string value, a whole command line env splits and runs.
func githubPostSplitString(name, word string, words []githubPostWord, head int) (string, bool) {
	if name != "env" {
		return "", false
	}
	switch {
	case strings.HasPrefix(word, "--split-string="):
		return strings.TrimPrefix(word, "--split-string="), true
	case word == "--split-string":
		if head+1 < len(words) {
			return words[head+1].text, true
		}
		return "", false
	case len(word) > 1 && word[0] == '-' && word[1] != '-':
		for j := 1; j < len(word); j++ {
			switch word[j] {
			case 'S':
				if j+1 < len(word) {
					return strings.TrimPrefix(word[j+1:], "="), true
				}
				if head+1 < len(words) {
					return words[head+1].text, true
				}
				return "", false
			case 'u', 'C':
				return "", false
			}
		}
	}
	return "", false
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

// githubPostWrapper is a command that runs the rest of the line, so the post behind it is read.
func githubPostWrapper(name string) bool {
	for _, w := range [...]string{"env", "sudo", "doas", "command", "builtin", "nohup", "setsid", "time",
		"nice", "ionice", "stdbuf", "timeout", "chrt", "taskset", "xargs"} {
		if name == w {
			return true
		}
	}
	return false
}

// githubPostWrapperValue is a wrapper option that takes the next word as its value, as its own help lists
// them. The table is built inside the call rather than at package level, so nothing is initialized at start.
func githubPostWrapperValue(name, option string) bool {
	values := map[string]string{
		"env":     "-u --unset -C --chdir -S --split-string",
		"sudo":    "-u --user -g --group -p --prompt -C --close-from -h --host -U --other-user -R --chroot -r --role -t --type -D --chdir",
		"doas":    "-u --user -C --config",
		"timeout": "-s --signal -k --kill-after",
		"nice":    "-n --adjustment",
		"ionice":  "-c --class -n --classdata -p --pid",
		"stdbuf":  "-i --input -o --output -e --error",
		"chrt":    "-p --pid -T --sched-runtime -P --sched-period -D --sched-deadline",
		"taskset": "-p --pid",
		"xargs":   "-I --replace -n --max-args -L --max-lines -P --max-procs -s --max-chars -E --eof -d --delimiter -a --arg-file",
	}[name]
	for _, v := range strings.Fields(values) {
		if option == v || strings.HasPrefix(option, v+"=") {
			return true
		}
	}
	return false
}

// githubPostMentions is whether the text names gh with pr, issue or api, the fail-closed rule's words: the
// text is read as written, and a word character beside a name makes it a different word.
func githubPostMentions(text string) bool {
	present := func(word string) bool {
		for i := 0; i+len(word) <= len(text); i++ {
			if text[i:i+len(word)] != word {
				continue
			}
			if !githubPostWordByte(text, i-1) && !githubPostWordByte(text, i+len(word)) {
				return true
			}
		}
		return false
	}
	return present("gh") && (present("pr") || present("issue") || present("api"))
}

// githubPostWordByte is whether the byte of text at i continues a shell word; an index off either end is
// a boundary, so a word that starts or ends the text counts.
func githubPostWordByte(text string, i int) bool {
	if i < 0 || i >= len(text) {
		return false
	}
	switch c := text[i]; {
	case c == '_' || c == '-' || c == '.' || c == '/' || c >= 0x80:
		return true
	case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9':
		return true
	}
	return false
}
