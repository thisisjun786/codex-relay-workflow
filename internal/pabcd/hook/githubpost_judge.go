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
	return githubPostJudgeTextDepth(command, cwd, 0, nil)
}

// githubPostJudgeTextDepth judges a text that a script file holds; outer is the writes of the texts that run it, which also
// happen before the script's own commands.
func githubPostJudgeTextDepth(command, cwd string, depth int, outer *githubPostWrites) (githubPostSite, bool) {
	res, err := shellir.Analyze(command, cwd)
	if err != nil {
		return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
	}
	return githubPostJudgeExecs(res.Execs, depth, outer)
}

func githubPostJudgeExecs(execs []shellir.Exec, depth int, outer *githubPostWrites) (githubPostSite, bool) {
	// The closed rule: a post is judged only as one simple command. A post that sits behind a wrapper,
	// a shell, a list, a pipe or a substitution is refused.
	simple := len(execs) == 1 && githubPostPlainContext(execs[0].Ctx)
	var written *githubPostWrites
	writes := func() *githubPostWrites {
		if written == nil {
			written = githubPostWritesOf(execs, outer)
		}
		return written
	}
	for _, e := range execs {
		if e.Kind == shellir.KindScriptFile {
			if writes().rewrites(e.Script.Value, e.Dir) {
				return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
			}
			if site, denied := githubPostJudgeScript(e, depth, writes().as(githubPostBodyKey(e.Script.Value, e.Dir))); denied {
				return site, true
			}
			continue
		}
		direct := githubPostDirectPath(e)
		if direct && githubPostProgram(e.Name) != "gh" {
			if writes().rewrites(e.Program.Value, e.Dir) {
				return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
			}
			if site, denied := githubPostJudgeDirect(e, depth, writes().as(githubPostBodyKey(e.Program.Value, e.Dir))); denied {
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
			if site, denied := githubPostJudgeDirect(e, depth, writes().as(githubPostBodyKey(e.Program.Value, e.Dir))); denied {
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
func githubPostJudgeScript(e shellir.Exec, depth int, writes *githubPostWrites) (githubPostSite, bool) {
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
	return githubPostJudgeTextDepth(body, cwd, depth+1, writes)
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
// a regular file, or in a directory the reader does not know is unreadable. A binary (no #! line and a NUL in the first 4 KiB) is not
// a script and is not judged, whatever its size; a script over 1 MiB is unreadable. A script with a shell shebang, or none (the shell
// runs it), is judged as shell text; a script of another interpreter that names a post is refused (its lines cannot satisfy the line
// rule).
func githubPostJudgeDirect(e shellir.Exec, depth int, writes *githubPostWrites) (githubPostSite, bool) {
	unread := githubPostSite{githubPostRuleUnread, githubPostWhereCommand}
	if depth >= githubPostMaxScriptDepth || !e.Dir.Known {
		return unread, true
	}
	body, kind := githubPostReadDirect(e.Program.Value, e.Dir.Path)
	switch kind {
	case githubPostFileUnreadable:
		return unread, true
	case githubPostFileBinary:
		return githubPostSite{}, false
	case githubPostFileShell:
		return githubPostJudgeTextDepth(body, e.Dir.Path, depth+1, writes)
	}
	if githubPostScriptMentionsPost(body) {
		return unread, true
	}
	return githubPostSite{}, false
}

// githubPostFileKind is what a file run by path holds.
type githubPostFileKind int

const (
	githubPostFileUnreadable githubPostFileKind = iota // missing, not a regular file, or a script over 1 MiB
	githubPostFileBinary                               // no #! line and a NUL in the first 4 KiB
	githubPostFileShell                                // a #! line naming a shell, or none: the shell runs the text
	githubPostFileOther                                // a #! line naming another interpreter
)

// githubPostDirectHead is how much of a file run by path decides whether it is a binary.
const githubPostDirectHead = 4096

// githubPostReadDirect opens a file run by a relative path once and reads its head: a file whose head has no #! line and a NUL is a
// binary and is read no further, so an executable of any size is not refused; any other file is a script, read whole up to 1 MiB.
func githubPostReadDirect(name, dir string) (string, githubPostFileKind) {
	file, ok := githubPostRegularFile(githubPostScriptPath(name, dir))
	if !ok {
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
	if githubPostShellName(interp) {
		return body, githubPostFileShell
	}
	return body, githubPostFileOther
}

// githubPostShellName is whether a program name is a POSIX-family shell that reads a script file as shell text.
func githubPostShellName(name string) bool {
	switch name {
	case "sh", "bash", "dash", "zsh", "ksh", "mksh", "ash":
		return true
	}
	return false
}

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
func githubPostWritesOf(execs []shellir.Exec, outer *githubPostWrites) *githubPostWrites {
	w := newGithubPostWrites(outer)
	w.collect(execs, 0, map[string]*githubPostWrites{})
	return w
}

func newGithubPostWrites(outer *githubPostWrites) *githubPostWrites {
	return &githubPostWrites{files: map[string]bool{}, existing: map[int64][]os.FileInfo{}, unknownBodies: map[string]bool{}, outer: outer}
}

// githubPostBodyKey names a script a record runs: the file by the identity the kernel gives it, and the directory it runs in.
func githubPostBodyKey(script string, dir shellir.Dir) string {
	if !dir.Known {
		return ""
	}
	return githubPostIdentity(githubPostScriptPath(script, dir.Path)) + "\x00" + dir.Path
}

// collect adds the writes of the records of one text; depth counts the script bodies around it, memo holds the writes of each body
// already read (by script identity and directory), so a text that runs one script many times reads it once.
func (w *githubPostWrites) collect(execs []shellir.Exec, depth int, memo map[string]*githubPostWrites) {
	for _, o := range execs {
		if o.Kind == shellir.KindScriptFile {
			switch o.Name {
			case "sed", "awk", "gawk", "mawk", "nawk":
				// a sed or awk program file is not shell text
			default:
				w.merge(githubPostBodyKey(o.Script.Value, o.Dir), githubPostBodyWrites(o.Script, o.Dir, depth, memo, false))
			}
			continue
		}
		if githubPostDirectPath(o) {
			w.merge(githubPostBodyKey(o.Program.Value, o.Dir), githubPostBodyWrites(o.Program, o.Dir, depth, memo, true))
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
func githubPostBodyWrites(script shellir.Word, dir shellir.Dir, depth int, memo map[string]*githubPostWrites, direct bool) *githubPostWrites {
	unknown := &githubPostWrites{unknown: true}
	if !script.Known || !dir.Known || depth >= githubPostMaxScriptDepth {
		return unknown
	}
	key := githubPostBodyKey(script.Value, dir)
	if got, ok := memo[key]; ok {
		return got
	}
	memo[key] = unknown // a script that runs itself is not read again
	var body string
	if direct {
		var kind githubPostFileKind
		body, kind = githubPostReadDirect(script.Value, dir.Path)
		switch kind {
		case githubPostFileBinary, githubPostFileOther:
			memo[key] = nil
			return nil
		case githubPostFileUnreadable:
			return unknown
		}
	} else {
		var ok bool
		if body, ok = githubPostReadScript(script.Value, dir.Path); !ok {
			return unknown
		}
	}
	res, err := shellir.Analyze(body, dir.Path)
	if err != nil {
		return unknown
	}
	got := newGithubPostWrites(nil)
	got.collect(res.Execs, depth+1, memo)
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

// rewrites is whether the text itself writes the script file it runs (cp evil.sh post.sh && bash post.sh): the file the guard reads
// before the command is then not the file that runs. A write to a destination the reader cannot name is taken as a write to the
// script.
//
// The writes of the script bodies the text runs count as well (bash writer.sh; bash post.sh): a known write wherever it is, an unknown
// one for every script but the one whose body makes it. In a body judged inside another, the writes around it are walked with the
// script of each level excluded the same way.
func (w *githubPostWrites) rewrites(script string, dir shellir.Dir) bool {
	if !dir.Known {
		return false // the script is unreadable already
	}
	p := githubPostScriptPath(script, dir.Path)
	self := githubPostBodyKey(script, dir)
	for x := w; x != nil; x = x.outer {
		if x != w {
			self = x.self
		}
		if x.unknown || x.reaches(p) || x.underTree(p) {
			return true
		}
		for key := range x.unknownBodies {
			if key != self {
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
