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
// The words of a command are the ones the shared command reader (internal/pabcd/shellir) gives after the shell's own quote and
// backslash removal, so a quoted option, a glued redirect or a quoted program name is judged as the word the shell runs. A text
// the reader cannot read is refused; no text is judged by splitting it on whitespace.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/publishpolicy"
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

// GitHubPostInput is the component row's whole input policy: the payload within the bound, read before the guard judges it
// so the row can leave its invocation record first. over is a payload past the bound, which the guard refuses (see
// GitHubPostJudge); ok is false when the read fails, which the guard refuses without observing or judging the partial input.
func GitHubPostInput(in io.Reader) (raw string, over, ok bool) {
	b, err := io.ReadAll(io.LimitReader(in, GitHubPostMaxStdinBytes+1))
	if err != nil {
		return "", false, false
	}
	if len(b) > GitHubPostMaxStdinBytes {
		return "", true, true
	}
	return string(b), false, true
}

// GitHubPostJudge is the guard's answer for a payload GitHubPostInput read, or the deny envelope when it was over the bound.
func GitHubPostJudge(raw string, over bool) string {
	if over {
		return githubPostDeny(githubPostRuleUnread, githubPostWhereCommand)
	}
	return HandleGitHubPostGuard(raw)
}

// GitHubPostAnswer is the guard's answer for the payload: the two steps above in one. A read that fails is refused without judging partial input.
func GitHubPostAnswer(in io.Reader) string {
	raw, over, ok := GitHubPostInput(in)
	if !ok {
		return GitHubPostCancelledAnswer()
	}
	return GitHubPostJudge(raw, over)
}

// GitHubPostCancelledAnswer is the deny answer of a GitHub post guard cancelled before it judged the command. The command is
// not read, so the answer is the one for an unreadable command: the post is refused at the command.
func GitHubPostCancelledAnswer() string {
	return githubPostDeny(githubPostRuleUnread, githubPostWhereCommand)
}

// githubPostDeny is the deny envelope: the rule, the place, and one way forward.
func githubPostDeny(rule, place string) string {
	reason := "GitHub post blocked (" + rule + ") at " + memoryGateLabel(place) +
		": check the text in a regular file under TMPDIR, /tmp or /var/tmp, then use --body-file, -F body=@file or --input"
	return editAnswer("deny", reason, reason)
}

// Diagnostics classify the refusal without changing the rule or decision.
func githubPostDenyPayload(site githubPostSite, p map[string]any) string {
	if site.rule != githubPostRuleUnread {
		return githubPostDeny(site.rule, site.place)
	}
	recovery := "Run a readable script file."
	if p["agent_id"] != nil || p["agent_type"] != nil {
		recovery = "Report the blocked command and cause code to your parent."
	}
	reason := "Cannot read the command or its program (unreadable-github-post); GitHub posting has not been established. " + recovery
	if site.post {
		reason = "GitHub post cannot be verified (unreadable-github-post) at " + memoryGateLabel(site.place) + ". " + recovery
	}
	if site.cause == "outside-temp-roots" {
		reason = "GitHub body file is outside the allowed temp roots TMPDIR, /tmp or /var/tmp (unreadable-github-post). Move the checked text to a regular file under one of those roots."
		if p["agent_id"] != nil || p["agent_type"] != nil {
			reason = "GitHub body file is outside the allowed temp roots TMPDIR, /tmp or /var/tmp (unreadable-github-post). " + recovery
		}
	}
	return editAnswer("deny", reason, reason)
}

// githubPostSite is why and where a post is denied.
// line is the line, in the script text it was found in, of the statement that is refused (0: none); the script judge turns it
// into script-file:line.
type githubPostSite struct {
	rule, place string
	line        int
	post        bool   // established by the execution that refused, including carried files
	cause       string // bounded body-read cause; never reconstructed from the payload cwd
}

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

// githubPostForm is form A: the one allowed gh post shape. handled is false when the command is not a
// gh post shape at all, so the caller keeps judging; a gh read and every other known gh command are
// handled and allowed, whatever words follow.
func githubPostForm(words []string, cwd githubPostDir) (site githubPostSite, denied, handled bool) {
	if len(words) < 2 || githubPostProgram(words[0]) != "gh" || !githubPostGhCommand(githubPostProgram(words[1])) {
		return githubPostSite{}, false, false
	}
	sub := githubPostProgram(words[1])
	switch sub {
	case "alias":
		// An alias may expand to a post, so the guard never reads it.
		return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, line: 0}, true, true
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
			return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, line: 0}, true, true
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
			return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, line: 0}, true, true
		}
	}
	return githubPostSite{}, false, true // a known gh command that is not a post shape
}

// githubPostPost is form A1: a gh pr or gh issue post, with only the words the rule allows.
func githubPostPost(args []string, cwd githubPostDir) (githubPostSite, bool, bool) {
	file, fileSet, titles := "", false, []string{}
	for i := 0; i < len(args); i++ {
		w := args[i]
		switch {
		case !strings.HasPrefix(w, "-"):
			// a positional target: a number, a URL or a branch
		case w == "--body" || w == "-b" || strings.HasPrefix(w, "--body="):
			return githubPostSite{rule: githubPostRuleInline, place: githubPostWhereCommand, line: 0}, true, true
		case w == "--body-file" || w == "-F":
			v, ok := githubPostNext(args, &i)
			if !ok {
				return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, line: 0}, true, true
			}
			if v == "-" {
				return githubPostSite{rule: githubPostRuleInline, place: githubPostWhereCommand, line: 0}, true, true
			}
			if fileSet {
				return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, line: 0}, true, true
			}
			file, fileSet = v, true
		case strings.HasPrefix(w, "--body-file="):
			v := strings.TrimPrefix(w, "--body-file=")
			if v == "-" {
				return githubPostSite{rule: githubPostRuleInline, place: githubPostWhereCommand, line: 0}, true, true
			}
			if fileSet {
				return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, line: 0}, true, true
			}
			file, fileSet = v, true
		case githubPostValueName(w) != "":
			name, value, attached := githubPostOptionValue(args, &i, w)
			if !attached {
				return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, line: 0}, true, true
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
		if strings.Contains(t, githubPostUnknownMark) {
			return githubPostSite{rule: githubPostRuleExpand, place: githubPostWhereCommand, line: 0}, true, true
		}
	}
	for _, t := range titles {
		if _, found := githubPostSecretLine(t); found {
			return githubPostSite{rule: githubPostRuleSecret, place: githubPostWhereTitle, line: 0}, true, true
		}
	}
	if fileSet {
		if strings.Contains(file, githubPostUnknownMark) {
			return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, line: 0}, true, true
		}
		content, ok := cwd.read(file)
		if !ok {
			return cwd.unread(file), true, true
		}
		if line, found := githubPostRawSecretLine(content); found {
			return githubPostSite{rule: githubPostRuleSecret, place: file + ":" + strconv.Itoa(line), line: 0}, true, true
		}
	}
	return githubPostSite{}, false, true
}

// githubPostJSONText is the text of one JSON document: every key and string value, with its escapes decoded, one per line. A key
// that repeats is kept at each place it stands, so every value the body carries is scanned. ok is false when the content is not
// exactly one JSON document, so a body read as JSON must be one.
func githubPostJSONText(content string) (text string, ok bool) {
	if !json.Valid([]byte(content)) {
		return "", false
	}
	dec := json.NewDecoder(strings.NewReader(content))
	var parts []string
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case string:
			parts = append(parts, t)
		case json.Delim:
			for dec.More() {
				if err := walk(); err != nil { // an array element, or an object key
					return err
				}
				if t == '{' {
					if err := walk(); err != nil { // the value of that key
						return err
					}
				}
			}
			_, err := dec.Token() // the closing delimiter
			return err
		}
		return nil
	}
	if err := walk(); err != nil {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}

// githubPostRawSecretLine is the first line of a body file's raw content that holds a secret, and whether one does. gh posts the
// raw bytes, so they are scanned as they are: the secret patterns and the NAME=value lines of githubPostSecretLine. A JSON document
// also has its strings split out at the quotes, brackets, commas and \n escapes, so a NAME=value shape inside a string is read as
// it is in a line of its own. Nothing is decoded, and the line numbers are the raw ones.
func githubPostRawSecretLine(content string) (int, bool) {
	return publishpolicy.RawSecretLine(content)
}

// githubPostAPI is form A2: gh api, with only the words the rule allows. At most one of -F body=@F,
// --field body=@F or --input F carries the text; none of them is a read.
func githubPostAPI(args []string, cwd githubPostDir) (githubPostSite, bool, bool) {
	file, fileSet, endpoint, jsonBody := "", false, false, false
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
			// Standard input is not a file the guard can read: it is an inline body, as --body-file - is (A1).
			if v == "-" {
				return githubPostSite{rule: githubPostRuleInline, place: githubPostWhereCommand, line: 0}, true, true
			}
			if fileSet {
				return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, line: 0}, true, true
			}
			file, fileSet = v, true
			jsonBody = true
		case w == "-F" || w == "--field":
			v, ok := githubPostNext(args, &i)
			if !ok {
				return githubPostSite{}, false, false
			}
			name, value, found := strings.Cut(v, "=")
			if !found || name != "body" {
				return githubPostSite{}, false, false
			}
			if strings.Contains(value, githubPostUnknownMark) {
				return githubPostSite{rule: githubPostRuleExpand, place: githubPostWhereCommand, line: 0}, true, true
			}
			if !strings.HasPrefix(value, "@") || value == "@-" {
				return githubPostSite{rule: githubPostRuleInline, place: githubPostWhereCommand, line: 0}, true, true
			}
			if fileSet {
				return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, line: 0}, true, true
			}
			file, fileSet = strings.TrimPrefix(value, "@"), true
		case w == "-f" || w == "--raw-field":
			v, ok := githubPostNext(args, &i)
			if !ok {
				return githubPostSite{}, false, false
			}
			if name, _, found := strings.Cut(v, "="); found && name == "body" {
				if strings.Contains(v, githubPostUnknownMark) {
					return githubPostSite{rule: githubPostRuleExpand, place: githubPostWhereCommand, line: 0}, true, true
				}
				return githubPostSite{rule: githubPostRuleInline, place: githubPostWhereCommand, line: 0}, true, true
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
		if strings.Contains(file, githubPostUnknownMark) {
			return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, line: 0}, true, true
		}
		content, ok := cwd.read(file)
		if !ok {
			return cwd.unread(file), true, true
		}
		// --input is the request body and is JSON: a body that is not one JSON document is unreadable, and the scan reads its
		// strings. A field file is posted as its raw text, so that text is scanned.
		line, found := 0, false
		if jsonBody {
			text, ok := githubPostJSONText(content)
			if !ok {
				return cwd.unread(file), true, true
			}
			line, found = githubPostSecretLine(text)
		} else {
			line, found = githubPostRawSecretLine(content)
		}
		if found {
			return githubPostSite{rule: githubPostRuleSecret, place: file + ":" + strconv.Itoa(line), line: 0}, true, true
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

// githubPostUnknownGh is a simple gh command whose first word after gh is not a known gh command: an
// alias or a program the guard cannot place may expand to a post.
func githubPostUnknownGh(words []string) (githubPostSite, bool) {
	if len(words) < 2 || githubPostProgram(words[0]) != "gh" || githubPostGhCommand(githubPostProgram(words[1])) {
		return githubPostSite{}, false
	}
	return githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, line: 0}, true
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

// githubPostDir is the directory a post's relative file names resolve from, and where the files the post reads are recorded
// (reads may be nil): the judge of a script asks whether a write of the script can reach them.
type githubPostDir struct {
	path  string
	reads *[]string
}

// read is githubPostReadFile, recording the file that was read.
func (d githubPostDir) read(name string) (string, bool) {
	text, resolved, ok := githubPostReadFileAt(name, d.path)
	if ok && d.reads != nil {
		*d.reads = append(*d.reads, resolved)
	}
	return text, ok
}

// githubPostReadFile reads the text a post names: a literal path that lies under a temporary root, is a
// regular file of at most 1 MiB, and whose bytes the guard can read.
func githubPostReadFile(name, cwd string) (string, bool) {
	text, _, ok := githubPostReadFileAt(name, cwd)
	return text, ok
}

// githubPostReadFileAt is githubPostReadFile that also returns the path the file was opened by (links resolved).
// githubPostBodyLocation owns both the containment decision and its diagnostic
// cause. It resolves against the actual execution directory before reading.
func githubPostBodyLocation(name, cwd string) (string, string) {
	if name == "-" {
		return "", "unreadable-body-file"
	}
	raw := name
	if !filepath.IsAbs(raw) {
		if cwd == "" || cwd == githubPostNoDir {
			return "", "unreadable-body-file"
		}
		raw = strings.TrimSuffix(cwd, "/") + "/" + raw
	}
	if !githubPostUnderRoots(filepath.Clean(raw)) {
		return "", "outside-temp-roots"
	}
	resolved, err := filepath.EvalSymlinks(raw)
	if err != nil {
		return "", "unreadable-body-file"
	}
	if !githubPostUnderRoots(resolved) {
		return "", "outside-temp-roots"
	}
	return resolved, ""
}
func (d githubPostDir) unread(name string) githubPostSite {
	_, cause := githubPostBodyLocation(name, d.path)
	return githubPostSite{rule: githubPostRuleUnread, place: name, post: true, cause: cause}
}
func githubPostReadFileAt(name, cwd string) (string, string, bool) {
	resolved, cause := githubPostBodyLocation(name, cwd)
	if cause != "" {
		return "", "", false
	}
	file, ok := githubPostRegularFile(resolved)
	if !ok {
		return "", "", false
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, githubPostMaxFileBytes+1))
	if err != nil || len(b) > githubPostMaxFileBytes {
		return "", "", false
	}
	return string(b), resolved, true
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

// githubPostBeforeOpen is a test seam: it runs between the name lookup and the open of githubPostRegularFile, so a test
// can swap the file for a named pipe at that moment. It is nil outside tests.
var githubPostBeforeOpen func(path string)

// githubPostRegularFile opens a path only when it names a regular file. The path is opened first, without blocking and
// without following a final link, and the opened descriptor is the one checked and read: a named pipe or a device swapped
// in between a check and the open never blocks the guard, because the check is on the descriptor (failure class 3).
func githubPostRegularFile(path string) (*os.File, bool) {
	if githubPostBeforeOpen != nil {
		githubPostBeforeOpen(path)
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, false
	}
	file := os.NewFile(uintptr(fd), path)
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() {
		file.Close()
		return nil, false
	}
	return file, true
}
