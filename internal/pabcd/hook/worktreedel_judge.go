package hook

// The worktree deletion guard reads a command through the shared command reader (internal/pabcd/shellir).
// Every program the reader shows is judged: a recursive rm, an rmdir, or a git worktree remove whose target is
// the session's own worktree, its slot, or an ancestor of the directory the command runs in is denied. A text
// the reader cannot read is denied, and so is a removal whose target the reader cannot evaluate.

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
	if e.Inline != nil {
		return worktreeDelJudgeInline(e, id)
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
		if v := worktreeDelJudgeFindStarts(starts, a.Guarded, e, id, "-delete"); v.Deny {
			return v
		}
	}
	return GuardVerdict{}
}

// worktreeDelJudgeFindStarts judges the start points of a find whose action deletes: a start that resolves to an ancestor of the
// managed worktree (the slot root and above) is refused; a start that is the worktree itself is refused when no test stands
// before the action. No start point means the current directory.
func worktreeDelJudgeFindStarts(starts []shellir.Word, guarded bool, e shellir.Exec, id WorktreeIdentity, action string) GuardVerdict {
	if len(starts) == 0 {
		starts = []shellir.Word{{Known: true, Value: "."}}
	}
	for _, s := range starts {
		ancestor, self := worktreeDelTargetKind(s.Value, e.Dir, id)
		if ancestor || self && !guarded {
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

// worktreeDelJudgeFeed judges a removal whose operands arrive from find or xargs: find's start points decide it (an ancestor of
// the worktree always, the worktree itself when no test guards the action), and the names xargs reads from standard input decide
// it (a name that resolves to the worktree, its slot or an ancestor, spelled other than .; a source the reader cannot read is
// refused). The program's own operands are judged by its own verdict.
func worktreeDelJudgeFeed(e shellir.Exec, id WorktreeIdentity) GuardVerdict {
	if e.Ctx.Feed == nil {
		return GuardVerdict{}
	}
	deleter, dir := worktreeDelDeleter(e)
	if deleter == "" {
		return GuardVerdict{}
	}
	for f := e.Ctx.Feed; f != nil; f = f.Outer {
		switch f.Wrapper {
		case "find":
			if v := worktreeDelJudgeFindStarts(f.Starts, f.Guarded, e, id, "-exec "+deleter); v.Deny {
				return v
			}
		case "xargs":
			if f.Unread != "" {
				return GuardVerdict{Deny: true, Reason: denyReason("xargs "+deleter+" ("+f.Unread+")", id)}
			}
			for _, n := range f.Names {
				if !n.Known {
					return GuardVerdict{Deny: true, Reason: denyReason("xargs "+deleter+" (the names it reads are not known)", id)}
				}
				for _, name := range shellir.FeedNames(n.Value) {
					if name == "." {
						continue
					}
					if !dir.Known || isProtectedTarget(name, dir.Path, id, true) {
						return GuardVerdict{Deny: true, Reason: denyReason("xargs "+deleter+" "+name, id)}
					}
				}
			}
		}
	}
	return GuardVerdict{}
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
