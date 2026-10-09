package hook

// The worktree deletion guard reads a command through the shared command reader (internal/pabcd/shellir).
// Every program the reader shows is judged: a recursive rm, an rmdir, or a git worktree remove whose target is
// the session's original worktree, its slot, or their ancestors is denied. A text
// the reader cannot read is denied, and so is a removal whose target the reader cannot evaluate. A find that deletes
// (-delete, or -exec and its kin running rm, rmdir, unlink, shred or git worktree remove) is judged by its start points
// and a test before the action, and the same programs run by xargs by the names on its standard input (CRW-895).

import (
	"io"
	"path/filepath"
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
	if id.cwd == "" && shellirPayloadCwd(cwd) != "" {
		id.cwd = canonicalize(cwd)
	}
	return worktreeDelJudgeText(command, shellirPayloadCwd(cwd), id, 0, nil, false)
}

// worktreeDelRead is the guard's reading of a text: the shared reader with no environment, so a variable is unknown whatever the
// session's environment holds. The differential fuzz counts the commands this reading refuses (WorktreeGuardCommandReadable).
func worktreeDelRead(command, cwd string, cdpath bool) (shellir.Result, error) {
	return shellir.AnalyzeDeletionScript(command, cwd, cdpath)
}

func worktreeDelUnreadable(id WorktreeIdentity) GuardVerdict {
	return GuardVerdict{Deny: true, Reason: "[crw: WORKTREE-GUARD-03] cannot analyze this command (analysis-unavailable). Run a smaller command with literal paths, or inspect the script and use supported shell commands so the guard can check its targets. See $crw:crw-worktree-guardian."}
}

// worktreeDelJudgeText judges one text; outer is the writes of the texts that run it (a script file's body), which happen before
// its own commands.
func worktreeDelJudgeText(command, cwd string, id WorktreeIdentity, depth int, outer *githubPostWrites, cdpath bool) GuardVerdict {
	res, err := worktreeDelRead(command, cwd, cdpath)
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
	return worktreeDelJudgeText(string(b), e.Dir.Path, id, depth+1, writes, e.Cdpath)
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
	case "mv":
		return worktreeDelJudgeMv(e, id)
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
		if a.FollowLinks {
			return worktreeDelUnreadable(id)
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

// worktreeDelTargetKind resolves a target from dir, then compares it to the fixed
// session roots: their ancestor (including the slot) or the checkout itself. A
// relative target from an unknown directory cannot be placed, so it counts as an ancestor; an absolute target names the same path
// from every directory and is judged as itself, from the checkout when the directory is not known.
func worktreeDelTargetKind(target string, dir shellir.Dir, id WorktreeIdentity) (ancestor, self bool) {
	if !dir.Known {
		if !strings.HasPrefix(target, "/") || id.CheckoutRoot == "" {
			return true, false
		}
		dir = shellir.Dir{Known: true, Path: id.CheckoutRoot}
	}
	if !isProtectedTarget(target, dir.Path, id) {
		return false, false
	}
	resolved := strings.TrimSuffix(canonicalize(resolveFrom(dir.Path, target)), "/")
	if resolved == "" || resolved == id.SlotRoot && id.SlotRoot != "" {
		return true, false
	}
	for _, root := range protectedWorktreeRoots(id) {
		if root != "" && strings.HasPrefix(canonicalize(root), resolved+"/") {
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
// program stands in reaches it through the shell's positional parameters, and through the text itself where the wrapper replaces a
// string in it (find's {}, xargs -I's string), so it applies when the program uses a parameter or holds that string, and to every
// program of a text that holds the string anywhere (cd {} sets the directory the rest of the text runs in). find's start points
// are judged from the directory find runs in and from the one the program runs in.
func worktreeDelJudgeFeed(e shellir.Exec, id WorktreeIdentity) GuardVerdict {
	for f := e.Ctx.Feed; f != nil; f = f.Outer {
		if f.Carried && !f.Replaced && !f.TextUses && !worktreeDelHasUnknownArg(e) &&
			!(f.Replace != "" && worktreeDelReplaceUsed(e, f.Replace)) && !(f.Wrapper == "find" && worktreeDelReplaceUsed(e, "{}")) {
			continue
		}
		var v GuardVerdict
		switch f.Wrapper {
		case "find":
			deleter, _ := worktreeDelDeleter(e)
			if deleter == "" {
				continue
			}
			if f.FollowLinks {
				return worktreeDelUnreadable(id)
			}
			if v = worktreeDelJudgeFindWords(f, e, deleter, id); v.Deny {
				return v
			}
			at := e
			at.Dir = f.Dir
			if v = worktreeDelJudgeFindStarts(f.Starts, f.GuardedFor, at, id, "-exec "+deleter); v.Deny {
				return v
			}
			// The paths find found resolve from the directory the program runs in only where one of its words holds them ({}); a
			// program that moved to a directory the reader does not know (cd {}) and names nothing of what find found does not
			// read the start points again from there.
			if e.Dir.Known || worktreeDelReplaceUsed(e, "{}") {
				v = worktreeDelJudgeFindStarts(f.Starts, f.GuardedFor, e, id, "-exec "+deleter)
			}
		case "xargs":
			v = worktreeDelJudgeXargs(f, e, id)
		}
		if v.Deny {
			return v
		}
	}
	return GuardVerdict{}
}

// worktreeDelJudgeFindWords refuses a word of a removal that find runs where {} is joined to other text so that the path it builds
// is not bounded by the start points. find puts each path it finds in place of {}: the start point as it was written, or a path
// below it. So the word is its text before {}, then the start point (or a path below), then its text after {}. A ".." component
// after {}, a start point with a ".." component under a prefix, and a prefix and start point that together name the worktree, its
// slot or an ancestor (../{} from the checkout, a prefix spelled to end in the name of a start) are refused; a prefix such as
// build/ or /tmp/stash/ keeps the word below that directory.
func worktreeDelJudgeFindWords(f *shellir.Feed, e shellir.Exec, deleter string, id WorktreeIdentity) GuardVerdict {
	starts := f.Starts
	if len(starts) == 0 {
		starts = []shellir.Word{{Known: true, Value: "."}}
	}
	deny := func(word string) GuardVerdict {
		return GuardVerdict{Deny: true, Reason: denyReason("find -exec "+deleter+" "+word+" (a path find builds from {} that its start points do not bound)", id)}
	}
	for _, a := range e.Args {
		k := strings.Index(a.Value, "{}")
		if !a.Known || k < 0 {
			continue
		}
		for _, part := range strings.Split(a.Value[k+2:], "/") {
			if part == ".." {
				return deny(a.Value)
			}
		}
		prefix := a.Value[:k]
		if prefix == "" || prefix == "./" {
			continue
		}
		for _, s := range starts {
			if !s.Known {
				return deny(a.Value)
			}
			for _, part := range strings.Split(s.Value, "/") {
				if part == ".." {
					return deny(a.Value)
				}
			}
			word := strings.ReplaceAll(a.Value, "{}", s.Value)
			if ancestor, self := worktreeDelTargetKind(word, e.Dir, id); ancestor || self {
				return deny(a.Value)
			}
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
	return !dir.Known || isProtectedTarget(name, dir.Path, id)
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
		if len(rest) > 0 {
			rest = append(rest, args[i]) // the subcommand and what follows it
			continue
		}
		switch {
		case args[i] == "-C" && i+1 < len(args):
			if !dir.Known {
				dirLost = true
				dir = shellir.Dir{}
			} else {
				dir = shellir.Dir{Path: resolveFrom(dir.Path, args[i+1]), Known: true}
			}
			i++
		case (args[i] == "-c" || gitGlobalTakesValue(args[i])) && i+1 < len(args):
			i++
		case strings.HasPrefix(args[i], "-"):
			// A global flag before the subcommand (-P, --no-pager, --bare) moves no directory. The shared reader accepts
			// only the global options it models, so every flag here is one of them.
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
	// Structured find and xargs feeds are judged by worktreeDelJudgeFeed. Other run-time wrappers cannot place the target.
	if unknown || e.Ctx.Feed == nil && e.Ctx.RuntimeCarrier != "" {
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

// worktreeDelJudgeMv is the verdict for mv. A move takes its sources away from the session as rm -r does, so a source that is
// the managed checkout or one of its ancestors is refused. A move whose operands arrive at run time (behind xargs, find or
// parallel, or in a nested shell that find runs) is refused. A source the reader cannot read is refused too, since from any
// directory it may name the checkout through a variable. A destination the reader cannot read is not a source and stays allowed.
func worktreeDelJudgeMv(e shellir.Exec, id WorktreeIdentity) GuardVerdict {
	if e.Ctx.RuntimeCarrier != "" {
		return worktreeDelUnreadable(id)
	}
	args, _ := worktreeDelArgs(e.Args)
	// getopt permutes the arguments, so an option after the first operand is an option; with POSIXLY_CORRECT it is an operand.
	// Both readings are judged, as sed's two readings are.
	for _, stopAtOperand := range []bool{false, true} {
		sources, ok := mvSources(args, stopAtOperand)
		if !ok {
			return worktreeDelUnreadable(id)
		}
		for _, s := range sources {
			if s == "" {
				return worktreeDelUnreadable(id)
			}
			if worktreeDelTargetProtected(s, e, id) {
				return GuardVerdict{Deny: true, Reason: denyReason("mv "+s, id)}
			}
		}
	}
	return GuardVerdict{}
}

// mvSources returns the operands mv moves: with -t or --target-directory every operand is a source, otherwise all but the
// last. The destination of -t is not a source. stopAtOperand is the POSIXLY_CORRECT reading, where options end at the first
// operand. ok is false for an option the reader does not model.
func mvSources(args []string, stopAtOperand bool) (sources []string, ok bool) {
	var operands []string
	targetDir, flagsDone := false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case !flagsDone && a == "--":
			flagsDone = true
		case !flagsDone && strings.HasPrefix(a, "--"):
			name, _, hasValue := strings.Cut(a, "=")
			switch name {
			case "--target-directory":
				targetDir = true
				if !hasValue {
					i++
				}
			case "--suffix":
				if !hasValue {
					i++
				}
			case "--force", "--interactive", "--no-clobber", "--verbose", "--update", "--no-target-directory", "--strip-trailing-slashes", "--debug", "--backup":
			default:
				return nil, false
			}
		case !flagsDone && len(a) > 1 && a[0] == '-':
			for k := 1; k < len(a); k++ {
				c := a[k]
				if c == 't' || c == 'S' {
					targetDir = targetDir || c == 't'
					if k == len(a)-1 {
						i++
					}
					break
				}
				if !strings.ContainsRune("finvbuTZ", rune(c)) {
					return nil, false
				}
			}
		default:
			operands = append(operands, a)
			flagsDone = flagsDone || stopAtOperand
		}
	}
	if targetDir {
		return operands, true
	}
	if len(operands) < 2 {
		return nil, true
	}
	return operands[:len(operands)-1], true
}

// worktreeDelTargetProtected says whether a removal target, taken from the program's directory, is protected. A
// relative target from an unknown directory cannot be placed, so it is protected.
func worktreeDelTargetProtected(target string, e shellir.Exec, id WorktreeIdentity) bool {
	if !e.Dir.Known {
		// An absolute target is independent of a failed cd. Relative targets
		// still cannot be placed against the fixed protection set.
		if !filepath.IsAbs(target) {
			return true
		}
	}
	return isProtectedTarget(target, e.Dir.Path, id)
}

// gitGlobalTakesValue names the git global options that take their value in the next word.
func gitGlobalTakesValue(s string) bool {
	switch s {
	case "--git-dir", "--work-tree", "--namespace", "--super-prefix":
		return true
	}
	return false
}
