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

// githubPostDeny is the deny envelope: the rule, the place, and one way forward.
// GitHubPostCancelledAnswer is the deny answer of a GitHub post guard cancelled before it judged the command.
func GitHubPostCancelledAnswer() string {
	return githubPostDeny("cancelled", "the hook")
}

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
		if strings.Contains(t, githubPostUnknownMark) {
			return githubPostSite{githubPostRuleExpand, githubPostWhereCommand}, true, true
		}
	}
	for _, t := range titles {
		if _, found := githubPostSecretLine(t); found {
			return githubPostSite{githubPostRuleSecret, githubPostWhereTitle}, true, true
		}
	}
	if fileSet {
		if strings.Contains(file, githubPostUnknownMark) {
			return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true, true
		}
		content, ok := githubPostReadFile(file, cwd)
		if !ok {
			return githubPostSite{githubPostRuleUnread, file}, true, true
		}
		if line, found := githubPostSecretLine(githubPostScanText(content)); found {
			return githubPostSite{githubPostRuleSecret, file + ":" + strconv.Itoa(line)}, true, true
		}
	}
	return githubPostSite{}, false, true
}

// githubPostScanText is the text the secret scan reads from a body file: the strings of a JSON document (keys and values,
// with their escapes decoded), so a secret inside a JSON string is seen as the text gh posts; any other file is its text.
func githubPostScanText(content string) string {
	if text, ok := githubPostJSONText(content); ok {
		return text
	}
	return content
}

// githubPostJSONText is the text of one JSON document: its keys and strings, with their escapes decoded, one per line. ok is
// false when the content is not one JSON document, so a body read as JSON must be one.
func githubPostJSONText(content string) (text string, ok bool) {
	var doc any
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		return "", false
	}
	var parts []string
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			parts = append(parts, t)
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for k, e := range t {
				parts = append(parts, k)
				walk(e)
			}
		}
	}
	walk(doc)
	return strings.Join(parts, "\n"), true
}

// githubPostAPI is form A2: gh api, with only the words the rule allows. At most one of -F body=@F,
// --field body=@F or --input F carries the text; none of them is a read.
func githubPostAPI(args []string, cwd string) (githubPostSite, bool, bool) {
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
			if fileSet {
				return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true, true
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
				return githubPostSite{githubPostRuleExpand, githubPostWhereCommand}, true, true
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
				if strings.Contains(v, githubPostUnknownMark) {
					return githubPostSite{githubPostRuleExpand, githubPostWhereCommand}, true, true
				}
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
		if strings.Contains(file, githubPostUnknownMark) {
			return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true, true
		}
		content, ok := githubPostReadFile(file, cwd)
		if !ok {
			return githubPostSite{githubPostRuleUnread, file}, true, true
		}
		// --input is the request body and is JSON: a body that is not one JSON document is unreadable, and the scan reads its strings.
		text := githubPostScanText(content)
		if jsonBody {
			var ok bool
			if text, ok = githubPostJSONText(content); !ok {
				return githubPostSite{githubPostRuleUnread, file}, true, true
			}
		}
		if line, found := githubPostSecretLine(text); found {
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

// githubPostReadFile reads the text a post names: a literal path that lies under a temporary root, is a
// regular file of at most 1 MiB, and whose bytes the guard can read.
func githubPostReadFile(name, cwd string) (string, bool) {
	raw := name
	if !filepath.IsAbs(raw) {
		// A relative name needs a known directory; the hook's own process directory is never a stand-in for it.
		if cwd == "" {
			return "", false
		}
		raw = strings.TrimSuffix(cwd, "/") + "/" + raw
	}
	// The cleaned path is only a first filter. A .. after a link steps up from the link's target, not from the link's parent
	// (failure class 5), so the path that is resolved and opened is the one as written.
	path := filepath.Clean(raw)
	// "-" is standard input, which the guard cannot read in the file it names.
	if name == "-" || !githubPostUnderRoots(path) {
		return "", false
	}
	// The trusted root is judged on the file the path resolves to, so a link inside it cannot reach another file.
	resolved, err := filepath.EvalSymlinks(raw)
	if err != nil || !githubPostUnderRoots(resolved) {
		return "", false
	}
	file, ok := githubPostRegularFile(resolved)
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
