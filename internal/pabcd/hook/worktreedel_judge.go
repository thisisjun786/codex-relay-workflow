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
	var written *githubPostWrites
	for _, e := range res.Execs {
		var v GuardVerdict
		if e.Kind == shellir.KindScriptFile {
			// A script the same text rewrites is not the file the guard reads before the command runs.
			if written == nil {
				written = githubPostWritesOf(res.Execs, outer)
			}
			if written.rewrites(e.Script.Value, e.Dir) {
				return worktreeDelUnreadable(id)
			}
			v = worktreeDelJudgeScript(e, id, depth, written)
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

// worktreeDelJudgeFind is the verdict for a find that deletes what it finds (-delete): the start points are removed.
func worktreeDelJudgeFind(e shellir.Exec, id WorktreeIdentity) GuardVerdict {
	args, unknown := worktreeDelArgs(e.Args)
	deletes := false
	for _, a := range args {
		if a == "-delete" {
			deletes = true
		}
	}
	if !deletes {
		return GuardVerdict{}
	}
	if unknown {
		return worktreeDelUnreadable(id)
	}
	var starts []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") || a == "(" || a == "!" || a == ")" {
			break
		}
		starts = append(starts, a)
	}
	if len(starts) == 0 {
		starts = []string{"."}
	}
	for _, s := range starts {
		if worktreeDelTargetProtected(s, e, id) {
			return GuardVerdict{Deny: true, Reason: denyReason("find "+s+" -delete", id)}
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

// worktreeDelJudgeGit denies git worktree remove of a protected checkout. -C moves the directory git runs in.
func worktreeDelJudgeGit(e shellir.Exec, id WorktreeIdentity) GuardVerdict {
	args, unknown := worktreeDelArgs(e.Args)
	dir := e.Dir
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-C" && i+1 < len(args):
			if !e.Dir.Known {
				return worktreeDelUnreadable(id)
			}
			dir = shellir.Dir{Path: resolveFrom(dir.Path, args[i+1]), Known: true}
			i++
		case args[i] == "-c" && i+1 < len(args):
			i++
		default:
			rest = append(rest, args[i])
		}
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
