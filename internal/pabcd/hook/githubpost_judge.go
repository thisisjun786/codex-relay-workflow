package hook

// The GitHub post guard reads a command through the shared command reader (internal/pabcd/shellir). Every
// gh invocation the reader shows is judged by its argument words; a script file that a shell runs is read
// and judged the same way; an interpreter program that names a post is refused; a text the reader cannot
// read is refused. An argument word the reader cannot evaluate is passed to the rules as
// githubPostUnknownMark, so a rule that would read its value refuses it.

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"

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
	return githubPostDeny(site.rule, site.place)
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
	return githubPostJudgeTextDepth(command, cwd, 0)
}

func githubPostJudgeTextDepth(command, cwd string, depth int) (githubPostSite, bool) {
	res, err := shellir.Analyze(command, cwd)
	if err != nil {
		return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
	}
	return githubPostJudgeExecs(res.Execs, depth)
}

func githubPostJudgeExecs(execs []shellir.Exec, depth int) (githubPostSite, bool) {
	// The closed rule: a post is judged only as one simple command. A post that sits behind a wrapper,
	// a shell, a list, a pipe or a substitution is refused.
	simple := len(execs) == 1 && githubPostPlainContext(execs[0].Ctx)
	for _, e := range execs {
		if e.Kind == shellir.KindScriptFile {
			if githubPostScriptWritten(execs, e.Script.Value, e.Dir) {
				return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
			}
			if site, denied := githubPostJudgeScript(e, depth); denied {
				return site, true
			}
			continue
		}
		direct := githubPostDirectPath(e)
		if direct && githubPostProgram(e.Name) != "gh" {
			if githubPostScriptWritten(execs, e.Program.Value, e.Dir) {
				return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
			}
			if site, denied := githubPostJudgeDirect(e, depth); denied {
				return site, true
			}
			continue
		}
		if e.Inline != nil && githubPostInlineNamesPost(e.Inline.Source.Value) {
			return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
		}
		if githubPostRunnerNamesPost(e) {
			return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
		}
		if githubPostProgram(e.Name) != "gh" {
			continue
		}
		words := []string{"gh"}
		for _, a := range e.Args {
			if a.Known {
				words = append(words, a.Value)
			} else {
				words = append(words, githubPostUnknownMark)
			}
		}
		base := e.Dir.Path
		if !e.Dir.Known {
			base = githubPostNoDir
		}
		if site, denied := githubPostJudgeWords(words, base); denied {
			return site, true
		}
		if !simple && githubPostPostSub(words) {
			return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
		}
		// ./gh is a file in the directory, which may be a script and not the installed gh: the file is read as well.
		if direct {
			if site, denied := githubPostJudgeDirect(e, depth); denied {
				return site, true
			}
		}
	}
	return githubPostSite{}, false
}

// githubPostPlainContext is whether a context is the top level of the text: no wrapper, shell, list,
// pipe, substitution, loop, function or subshell around the command.
func githubPostPlainContext(c shellir.Context) bool {
	return !c.Conditional && !c.Background && !c.Coprocess && !c.Subshell && !c.Pipeline &&
		!c.Loop && !c.FuncBody && !c.CmdSubst && !c.ProcSubst && c.Carrier == "" && c.Stdin == shellir.StdinNone
}

// githubPostJudgeScript reads the file a shell or a sed or awk program runs and judges its text.
func githubPostJudgeScript(e shellir.Exec, depth int) (githubPostSite, bool) {
	unread := githubPostSite{githubPostRuleUnread, githubPostWhereCommand}
	if depth >= githubPostMaxScriptDepth || !e.Script.Known {
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
		return unread, true
	}
	switch e.Name {
	case "sed", "awk", "gawk", "mawk", "nawk":
		if githubPostInlineNamesPost(body) {
			return unread, true
		}
		return githubPostSite{}, false
	}
	return githubPostJudgeTextDepth(body, cwd, depth+1)
}

// githubPostInlineNamesPost is whether an interpreter's program text names a gh post. The program may run
// that post through a shell or a system call, so a text that names one is refused.
func githubPostInlineNamesPost(src string) bool {
	lower := strings.ToLower(src)
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
func githubPostJudgeWords(words []string, cwd string) (githubPostSite, bool) {
	expansion := githubPostSite{githubPostRuleExpand, githubPostWhereCommand}
	unread := githubPostSite{githubPostRuleUnread, githubPostWhereCommand}
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

// githubPostDirectPath is whether an exec runs a file by a relative path with a slash (./post.sh, scripts/post.sh, ../gh): the
// file is in the directory the command runs in, not an installed program the guard knows by its name. An absolute path names an
// installed program (/usr/bin/gh, /usr/bin/env) and is judged by its base name.
func githubPostDirectPath(e shellir.Exec) bool {
	return e.Kind == shellir.KindCommand && e.Inline == nil && e.Program.Known &&
		strings.Contains(e.Program.Value, "/") && !filepath.IsAbs(e.Program.Value)
}

// githubPostJudgeDirect reads the file a command runs by a relative path, as a script the shell runs: a file that is missing, not
// a regular file, over 1 MiB, or in a directory the reader does not know is unreadable. A binary (no #! line and a NUL in the
// first 4 KiB) is not a script and is not judged. A script with a shell shebang, or none (the shell runs it), is judged as shell
// text; a script of another interpreter that names a post is refused (its lines cannot satisfy the line rule).
func githubPostJudgeDirect(e shellir.Exec, depth int) (githubPostSite, bool) {
	unread := githubPostSite{githubPostRuleUnread, githubPostWhereCommand}
	if depth >= githubPostMaxScriptDepth || !e.Dir.Known {
		return unread, true
	}
	b, ok := githubPostReadScriptBytes(e.Program.Value, e.Dir.Path)
	if !ok {
		return unread, true
	}
	body := string(b)
	if !strings.HasPrefix(body, "#!") {
		head := b
		if len(head) > 4096 {
			head = head[:4096]
		}
		if bytes.IndexByte(head, 0) >= 0 {
			return githubPostSite{}, false
		}
		return githubPostJudgeTextDepth(body, e.Dir.Path, depth+1)
	}
	line, _, _ := strings.Cut(body, "\n")
	words := strings.Fields(strings.TrimPrefix(line, "#!"))
	interp := ""
	if len(words) > 0 {
		interp = filepath.Base(words[0])
		if interp == "env" {
			interp = ""
			for _, w := range words[1:] {
				if !strings.HasPrefix(w, "-") {
					interp = filepath.Base(w)
					break
				}
			}
		}
	}
	switch interp {
	case "sh", "bash", "dash", "zsh", "ksh", "mksh", "ash":
		return githubPostJudgeTextDepth(body, e.Dir.Path, depth+1)
	}
	if githubPostInlineNamesPost(body) {
		return unread, true
	}
	return githubPostSite{}, false
}

// githubPostReadScriptBytes is githubPostReadScript's read: the bytes of a regular file of at most 1 MiB.
func githubPostReadScriptBytes(name, cwd string) ([]byte, bool) {
	file, ok := githubPostRegularFile(githubPostScriptPath(name, cwd))
	if !ok {
		return nil, false
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, githubPostMaxFileBytes+1))
	if err != nil || len(b) > githubPostMaxFileBytes {
		return nil, false
	}
	return b, true
}

// githubPostScriptWritten is whether the text itself writes the script file it runs (cp evil.sh post.sh && bash post.sh): the
// file the guard reads before the command is then not the file that runs. A write to a destination the reader cannot name is
// taken as a write to the script.
func githubPostScriptWritten(execs []shellir.Exec, script string, dir shellir.Dir) bool {
	if !dir.Known {
		return false // the script is unreadable already
	}
	target := script
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir.Path, target)
	}
	target = filepath.Clean(target)
	for _, o := range execs {
		if o.Kind == shellir.KindScriptFile {
			continue
		}
		for _, d := range shellIRExecDests(o) {
			if d == shellIRUnknownDest {
				return true
			}
			if d == "/dev/null" {
				continue
			}
			if !filepath.IsAbs(d) {
				if !o.Dir.Known {
					return true
				}
				d = filepath.Join(o.Dir.Path, d)
			}
			if filepath.Clean(d) == target {
				return true
			}
		}
	}
	return false
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
