package hook

// The worktree deletion guard reads a command through the shared command reader (internal/pabcd/shellir).
// Every program the reader shows is judged: a recursive rm, an rmdir, or a git worktree remove whose target is
// the session's own worktree, its slot, or an ancestor of the directory the command runs in is denied. A text
// the reader cannot read is denied, and so is a removal whose target the reader cannot evaluate. A find that deletes
// (-delete, or -exec and its kin running rm, rmdir, unlink, shred or git worktree remove) is judged by its start points
// and a test before the action, and the same programs run by xargs by the names on its standard input (CRW-895).

import (
	"io"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// worktreeDelMaxScriptDepth bounds script files that run script files.
const worktreeDelMaxScriptDepth = 4

// worktreeDelMaxScriptBytes is the largest script file the guard reads.
const worktreeDelMaxScriptBytes = 1 << 20

// evaluateCommand is the verdict for one command in the session's worktree.
func evaluateCommand(command, cwd string, id WorktreeIdentity) GuardVerdict {
	if !id.Managed || text.Trim(command) == "" {
		return GuardVerdict{}
	}
	return worktreeDelJudgeText(command, shellirPayloadCwd(cwd), id, 0, nil)
}

// worktreeDelRead is the guard's reading of a text: the shared reader with no environment, so a variable is unknown whatever the
// session's environment holds. The differential fuzz counts the commands this reading refuses (WorktreeGuardCommandReadable).
func worktreeDelRead(command, cwd string) (shellir.Result, error) {
	return shellir.Analyze(command, cwd)
}

func worktreeDelUnreadable(id WorktreeIdentity) GuardVerdict {
	return GuardVerdict{Deny: true, Reason: denyReason("a command the guard cannot read", id)}
}

// worktreeDelJudgeText judges one text; outer is the writes of the texts that run it (a script file's body), which happen before
// its own commands.
func worktreeDelJudgeText(command, cwd string, id WorktreeIdentity, depth int, outer *githubPostWrites) GuardVerdict {
	res, err := worktreeDelRead(command, cwd)
	if err != nil {
		return worktreeDelUnreadable(id)
	}
	var written *githubPostTextWrites
	for i, e := range res.Execs {
		var v GuardVerdict
		if e.Kind == shellir.KindScriptFile {
			// A script the same text, or a script body run before it, rewrites is not the file the guard reads before the command runs.
			if written == nil {
				written = githubPostWritesOf(res.Execs, outer)
			}
			if written.stale(i, e.Script.Value, e.Dir) {
				return worktreeDelUnreadable(id)
			}
			v = worktreeDelJudgeScript(e, id, depth, written.at(i).as(githubPostBodyKey(e.Script.Value, e.Dir)))
		} else {
			v = worktreeDelJudgeExec(e, id)
		}
		if v.Deny {
			return v
		}
	}
	return GuardVerdict{}
}

// worktreeDelJudgeScript reads the script file a shell runs and judges its text in the script's directory.
func worktreeDelJudgeScript(e shellir.Exec, id WorktreeIdentity, depth int, writes *githubPostWrites) GuardVerdict {
	if depth >= worktreeDelMaxScriptDepth || !e.Script.Known || !e.Dir.Known {
		return worktreeDelUnreadable(id)
	}
	file, ok := githubPostRegularFile(githubPostScriptPath(e.Script.Value, e.Dir.Path))
	if !ok {
		return worktreeDelUnreadable(id)
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, worktreeDelMaxScriptBytes+1))
	if err != nil || len(b) > worktreeDelMaxScriptBytes {
		return worktreeDelUnreadable(id)
	}
	// The body is judged as any text is: the script files it runs are checked against what it and the texts around it write.
	return worktreeDelJudgeText(string(b), e.Dir.Path, id, depth+1, writes)
}

// worktreeDelJudgeExec is the verdict for one program the reader shows.
func worktreeDelJudgeExec(e shellir.Exec, id WorktreeIdentity) GuardVerdict {
	if v := worktreeDelJudgeFeed(e, id); v.Deny {
		return v
	}
	if v := worktreeDelOwn(e, id); v.Deny {
		return v
	}
	if e.Inline != nil && !worktreeDelNamed(e) {
		return worktreeDelJudgeInline(e, id)
	}
	return GuardVerdict{}
}

// worktreeDelNamed says whether the program is one of those worktreeDelOwn judges by its own words.
func worktreeDelNamed(e shellir.Exec) bool {
	switch basename(e.Name) {
	case "rm", "rmdir", "git", "find":
		return true
	}
	return false
}

// worktreeDelOwn is the verdict for a program by its own words: a recursive rm, an rmdir, a git worktree remove, a find -delete.
func worktreeDelOwn(e shellir.Exec, id WorktreeIdentity) GuardVerdict {
	switch basename(e.Name) {
	case "rm":
		return worktreeDelJudgeRm(e, id)
	case "rmdir":
		return worktreeDelJudgeRmdir(e, id)
	case "git":
		return worktreeDelJudgeGit(e, id)
	case "find":
		return worktreeDelJudgeFind(e, id)
	}
	return GuardVerdict{}
}

// worktreeDelJudgeFind is the verdict for a find that deletes what it finds (-delete): the start points are removed unless a
// test keeps the action to the matches. A program that -exec, -execdir, -ok or -okdir runs is judged with the feed the reader
// gives it (worktreeDelJudgeFeed).
func worktreeDelJudgeFind(e shellir.Exec, id WorktreeIdentity) GuardVerdict {
	starts, actions, err := shellir.FindScan(e.Args)
	if err != nil {
		return worktreeDelUnreadable(id)
	}
	for _, a := range actions {
		if a.Name != "-delete" {
			continue
		}
		if v := worktreeDelJudgeFindStarts(starts, a.GuardedFor, e, id, "-delete"); v.Deny {
			return v
		}
	}
	return GuardVerdict{}
}

// worktreeDelJudgeFindStarts judges the start points of a find whose action deletes: a start that resolves to an ancestor of the
// managed worktree (the slot root and above) is refused; a start that is the worktree itself is refused unless a test stands
// before the action on every path that reaches it and leaves the start out (-name '*' leaves nothing out); a start read at run
// time is refused. No start point means the current directory.
func worktreeDelJudgeFindStarts(starts []shellir.Word, guarded func(string) bool, e shellir.Exec, id WorktreeIdentity, action string) GuardVerdict {
	if len(starts) == 0 {
		starts = []shellir.Word{{Known: true, Value: "."}}
	}
	for _, s := range starts {
		if !s.Known {
			return GuardVerdict{Deny: true, Reason: denyReason("find ("+s.Reason+") "+action, id)}
		}
		ancestor, self := worktreeDelTargetKind(s.Value, e.Dir, id)
		if ancestor || self && !guarded(s.Value) {
			return GuardVerdict{Deny: true, Reason: denyReason("find "+s.Value+" "+action, id)}
		}
	}
	return GuardVerdict{}
}

// worktreeDelTargetKind says whether a target, taken from dir, is an ancestor of the managed worktree (the slot root or a
// directory above it) or the worktree itself (the checkout, the directory the command runs in, or a directory between). A
// relative target from an unknown directory cannot be placed, so it counts as an ancestor.
func worktreeDelTargetKind(target string, dir shellir.Dir, id WorktreeIdentity) (ancestor, self bool) {
	if !dir.Known {
		return true, false
	}
	if !isProtectedTarget(target, dir.Path, id, true) {
		return false, false
	}
	resolved := strings.TrimSuffix(canonicalize(resolveFrom(dir.Path, target)), "/")
	if resolved == "" || resolved == id.SlotRoot && id.SlotRoot != "" {
		return true, false
	}
	for _, root := range []string{id.SlotRoot, id.CheckoutRoot} {
		if root != "" && strings.HasPrefix(root, resolved+"/") {
			return true, false
		}
	}
	return false, true
}

// worktreeDelDeleter names the removal a program does when find or xargs gives it the operands (rm, rmdir, unlink, shred, git
// worktree remove), with the directory its operands are resolved from; "" is a program that removes nothing by itself.
func worktreeDelDeleter(e shellir.Exec) (string, shellir.Dir) {
	switch name := basename(e.Name); name {
	case "rm", "rmdir", "unlink", "shred":
		return name, e.Dir
	case "git":
		rest, dir, _, _ := worktreeDelGitArgs(e)
		if len(rest) >= 2 && rest[0] == "worktree" && rest[1] == "remove" {
			return "git worktree remove", dir
		}
	}
	return "", e.Dir
}

// worktreeDelHasUnknownArg says whether a program has an operand the reader cannot evaluate: in a shell text that a wrapper runs,
// such an operand ("$@", "$1") is where the wrapper's operands arrive.
func worktreeDelHasUnknownArg(e shellir.Exec) bool {
	for _, a := range e.Args {
		if !a.Known {
			return true
		}
	}
	return false
}

// worktreeDelJudgeFeed judges a removal whose operands arrive from find or xargs: find's start points decide it (an ancestor of
// the worktree always, the worktree itself when no test guards the action), and the names xargs reads from standard input decide
// it (a name that resolves to the worktree, its slot or an ancestor, spelled other than .; a source the reader cannot read is
// refused). The program's own operands are judged by its own verdict. A feed that belongs to a wrapper outside the shell text the
// program stands in reaches it only through the shell's positional parameters, so it applies when the program uses one (or, for
// xargs -I, when the shell text holds the replace string).
func worktreeDelJudgeFeed(e shellir.Exec, id WorktreeIdentity) GuardVerdict {
	for f := e.Ctx.Feed; f != nil; f = f.Outer {
		if f.Carried && !f.Replaced && !worktreeDelHasUnknownArg(e) && !(f.Replace != "" && worktreeDelReplaceUsed(e, f.Replace)) {
			continue
		}
		var v GuardVerdict
		switch f.Wrapper {
		case "find":
			deleter, _ := worktreeDelDeleter(e)
			if deleter == "" {
				continue
			}
			v = worktreeDelJudgeFindStarts(f.Starts, f.GuardedFor, e, id, "-exec "+deleter)
		case "xargs":
			v = worktreeDelJudgeXargs(f, e, id)
		}
		if v.Deny {
			return v
		}
	}
	return GuardVerdict{}
}

// worktreeDelNameProtected says whether an operand that xargs builds, taken from dir, names the worktree, its slot or an ancestor.
// A name spelled . is the directory the command runs in and stays allowed, as a find start of the same spelling does.
func worktreeDelNameProtected(name string, dir shellir.Dir, id WorktreeIdentity) bool {
	if name == "." {
		return false
	}
	return !dir.Known || isProtectedTarget(name, dir.Path, id, true)
}

// worktreeDelJudgeXargs judges the program xargs runs. With -I and names the reader proves, the reader has already read the
// template once for each name, so e is the command that runs and every operand of the removal is judged. Otherwise the names are
// appended to the operands, one command for each name.
func worktreeDelJudgeXargs(f *shellir.Feed, e shellir.Exec, id WorktreeIdentity) GuardVerdict {
	deleter, dir := worktreeDelDeleter(e)
	base := basename(e.Name)
	if f.Replace != "" && !f.Replaced && worktreeDelReplaceUsed(e, f.Replace) {
		// The replace string stands in a word of a program that can remove, and the names that replace it are not known.
		switch {
		case base == "git" || base == "find":
			return GuardVerdict{Deny: true, Reason: denyReason("xargs "+base+" (the names -I puts in its words are not known)", id)}
		case deleter != "" && !worktreeDelReplaceWholeWord(e, f.Replace):
			return GuardVerdict{Deny: true, Reason: denyReason("xargs "+deleter+" (the names -I puts in its words are not known)", id)}
		}
	}
	if deleter == "" {
		return GuardVerdict{}
	}
	if f.Unread != "" {
		return GuardVerdict{Deny: true, Reason: denyReason("xargs "+deleter+" ("+f.Unread+")", id)}
	}
	if f.Replaced {
		return worktreeDelJudgeOperands(worktreeDelOperands(deleter, e), dir, id, "xargs "+deleter+" ")
	}
	if !f.Named || f.Replace != "" {
		return GuardVerdict{}
	}
	endOfOptions := false
	for _, a := range e.Args {
		if a.Known && a.Value == "--" {
			endOfOptions = true
		}
	}
	for _, item := range f.Items {
		e2 := e
		e2.Args = append(append([]shellir.Word{}, e.Args...), shellir.Word{Known: true, Value: item})
		if !endOfOptions && strings.HasPrefix(item, "-") && deleter != "git worktree remove" {
			return GuardVerdict{Deny: true, Reason: denyReason("xargs "+deleter+" "+item+" (a name that begins with - is an option)", id)}
		}
		if v := worktreeDelJudgeOperands([]string{item}, dir, id, "xargs "+deleter+" "); v.Deny {
			return v
		}
		if v := worktreeDelOwn(e2, id); v.Deny {
			return v
		}
	}
	return GuardVerdict{}
}

// worktreeDelOperands are the words a removal program takes as the things to remove: all its words for rm, rmdir, unlink and
// shred, the words after worktree remove for git.
func worktreeDelOperands(deleter string, e shellir.Exec) []string {
	if deleter == "git worktree remove" {
		rest, _, _, _ := worktreeDelGitArgs(e)
		if len(rest) > 2 {
			return rest[2:]
		}
		return nil
	}
	args, _ := worktreeDelArgs(e.Args)
	return args
}

// worktreeDelJudgeOperands refuses a removal operand that names the worktree, its slot or an ancestor. An option is judged by the
// program's own verdict.
func worktreeDelJudgeOperands(operands []string, dir shellir.Dir, id WorktreeIdentity, what string) GuardVerdict {
	for _, a := range operands {
		if a == "" || strings.HasPrefix(a, "-") && a != "-" {
			continue
		}
		if worktreeDelNameProtected(a, dir, id) {
			return GuardVerdict{Deny: true, Reason: denyReason(what+a, id)}
		}
	}
	return GuardVerdict{}
}

// worktreeDelReplaceUsed says whether the replace string stands in a word of the program.
func worktreeDelReplaceUsed(e shellir.Exec, repl string) bool {
	for _, a := range e.Args {
		if a.Known && strings.Contains(a.Value, repl) {
			return true
		}
	}
	return false
}

// worktreeDelReplaceWholeWord says whether the replace string stands only as whole words of the program: a name that replaces such
// a word is an operand of its own, and a program whose names the text does not give (ls) is not read further.
func worktreeDelReplaceWholeWord(e shellir.Exec, repl string) bool {
	for _, a := range e.Args {
		if a.Known && strings.Contains(a.Value, repl) && a.Value != repl {
			return false
		}
	}
	return true
}

// worktreeDelJudgeInline judges an interpreter's inline program: one the reader cannot show is unreadable, and one that
// removes a file, starts another program or reaches a name at run time is refused in a managed worktree.
func worktreeDelJudgeInline(e shellir.Exec, id WorktreeIdentity) GuardVerdict {
	if !e.Inline.Source.Known {
		return worktreeDelUnreadable(id)
	}
	if why := worktreeDelProgramRefusal(e.Inline.Language, e.Inline.Source.Value); why != "" {
		return GuardVerdict{Deny: true, Reason: denyReason(e.Inline.Language+" "+why, id)}
	}
	return GuardVerdict{}
}

// worktreeDelArgs splits the known operands of a program; an unknown operand is returned as unknown.
func worktreeDelArgs(args []shellir.Word) ([]string, bool) {
	out := make([]string, 0, len(args))
	unknown := false
	for _, a := range args {
		if !a.Known {
			unknown = true
			out = append(out, "")
			continue
		}
		out = append(out, a.Value)
	}
	return out, unknown
}

func worktreeDelJudgeRm(e shellir.Exec, id WorktreeIdentity) GuardVerdict {
	args, unknown := worktreeDelArgs(e.Args)
	recursive, flagsDone := false, false
	var targets []string
	for _, a := range args {
		switch {
		case !flagsDone && a == "--":
			flagsDone = true
		case !flagsDone && strings.HasPrefix(a, "--"):
			recursive = recursive || a == "--recursive"
		case !flagsDone && strings.HasPrefix(a, "-") && len(a) > 1:
			recursive = recursive || strings.ContainsAny(a, "rR")
		default:
			targets = append(targets, a)
		}
	}
	if !recursive {
		return GuardVerdict{}
	}
	if unknown || shellIRRunTimeCarrier(e.Ctx.Carrier) {
		return worktreeDelUnreadable(id)
	}
	for _, t := range targets {
		if worktreeDelTargetProtected(t, e, id) {
			return GuardVerdict{Deny: true, Reason: denyReason("rm -r "+t, id)}
		}
	}
	return GuardVerdict{}
}

func worktreeDelJudgeRmdir(e shellir.Exec, id WorktreeIdentity) GuardVerdict {
	args, unknown := worktreeDelArgs(e.Args)
	for _, a := range args {
		if a == "" && unknown {
			return worktreeDelUnreadable(id)
		}
		if strings.HasPrefix(a, "-") && a != "" {
			continue
		}
		if worktreeDelTargetProtected(a, e, id) {
			return GuardVerdict{Deny: true, Reason: denyReason("rmdir "+a, id)}
		}
	}
	return GuardVerdict{}
}

// worktreeDelGitArgs splits the operands of a git command into the words after -C and -c: rest, the directory git runs in
// (-C moves it), whether an operand is unknown, and whether -C moved an unknown directory.
func worktreeDelGitArgs(e shellir.Exec) (rest []string, dir shellir.Dir, unknown, dirLost bool) {
	args, unknown := worktreeDelArgs(e.Args)
	dir = e.Dir
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-C" && i+1 < len(args):
			if !dir.Known {
				dirLost = true
				dir = shellir.Dir{}
			} else {
				dir = shellir.Dir{Path: resolveFrom(dir.Path, args[i+1]), Known: true}
			}
			i++
		case args[i] == "-c" && i+1 < len(args):
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	return rest, dir, unknown, dirLost
}

// worktreeDelJudgeGit denies git worktree remove of a protected checkout. -C moves the directory git runs in.
func worktreeDelJudgeGit(e shellir.Exec, id WorktreeIdentity) GuardVerdict {
	rest, dir, unknown, dirLost := worktreeDelGitArgs(e)
	if dirLost {
		return worktreeDelUnreadable(id)
	}
	if len(rest) < 2 || rest[0] != "worktree" || rest[1] != "remove" {
		return GuardVerdict{}
	}
	if unknown {
		return worktreeDelUnreadable(id)
	}
	target := ""
	for _, t := range rest[2:] {
		if !strings.HasPrefix(t, "-") {
			target = t
			break
		}
	}
	if target != "" && worktreeDelTargetProtected(target, shellir.Exec{Dir: dir}, id) {
		return GuardVerdict{Deny: true, Reason: denyReason("git worktree remove "+target, id)}
	}
	return GuardVerdict{}
}

// worktreeDelTargetProtected says whether a removal target, taken from the program's directory, is protected. A
// relative target from an unknown directory cannot be placed, so it is protected.
func worktreeDelTargetProtected(target string, e shellir.Exec, id WorktreeIdentity) bool {
	if !e.Dir.Known {
		// The directory is unknown, so the target may name the managed checkout whatever its spelling: protected.
		return true
	}
	return isProtectedTarget(target, e.Dir.Path, id, true)
}
