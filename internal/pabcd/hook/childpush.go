package hook

// The relay child push guard (CRW-1284). In a relay child session a git push may move only the child's own branch: integration is
// the parent's and the integrator's. Every refspec must name the own branch as its destination (HEAD:own); any other push (HEAD:dev,
// src:dev, a pattern, --all, --mirror, a refspec without a destination, which remote.<name>.push may map onto dev) is refused,
// through any remote. The command is read with the shared reader as
// the other guards read it (CRW-1283): a program wrapper is read; a script file a shell runs, or one run by its path, is read; a
// "$@" forwarder, a word or text the reader cannot read, and a program file or inline program that spells push are refused. A
// session the relay does not know as a child (a parent, an integrator) is not judged.

import (
	"context"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/gitprobe"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// ChildPushBinding is what the relay says about a session: whether it is a relay child, the branch it may push ("" when it has
// none) and why the relay's record of it could not be read.
type ChildPushBinding struct {
	Child      bool
	Own        string
	Unreadable string
}

// HandleChildPushGuard is the guard's PreToolUse leg: the deny envelope for a push it stops, else empty. bind is asked only for a
// command that pushes or that the reader cannot read; its answer for any other session than a child lets the command through.
// While the relay's record cannot be read the session may be a child, so a push is refused in any session, and so is a command the
// reader cannot read that spells push; one that spells no push passes, as it does in a session that is no child.
func HandleChildPushGuard(raw string, bind func(session, cwd string) ChildPushBinding) string {
	p := editObject(raw)
	tool, _ := p["tool_name"].(string)
	input, _ := p["tool_input"].(map[string]any)
	if p["hook_event_name"] != "PreToolUse" || !memoryGateShellTool(tool) || input == nil {
		return ""
	}
	cwd, _ := p["cwd"].(string)
	cwd = shellirPayloadCwd(cwd)
	command, _ := githubPostCommand(input)
	if words, ok := githubPostArgv(input); ok {
		command = "'" + strings.Join(func() (q []string) {
			for _, w := range words {
				q = append(q, strings.ReplaceAll(w, "'", `'\''`))
			}
			return q
		}(), "' '") + "'"
	}
	if text.Trim(command) == "" {
		return ""
	}
	var s childPushScan
	s.text(command, cwd, 0, nil, false)
	if s.why == "" && len(s.pushes) == 0 {
		return ""
	}
	session, _ := p["session_id"].(string)
	b := bind(session, cwd)
	switch {
	case b.Unreadable != "" && len(s.pushes) == 0 && !childPushSpells(command):
		return ""
	case b.Unreadable != "":
		s.why = "the relay's record of this session cannot be read (" + unreadableLabel(b.Unreadable) + ")"
	case !b.Child:
		return ""
	}
	for _, push := range s.pushes {
		if s.why == "" {
			s.why = childPushJudge(push, b.Own)
		}
	}
	if s.why == "" {
		return ""
	}
	own := b.Own
	if own == "" {
		own = "<its own branch>"
	}
	reason := "[crw CHILD-PUSH-GUARD] 통합은 부모·통합기 몫 (integration is the parent's and the integrator's): a relay child pushes only its own branch, " +
		"named as the destination of a direct git push (git push origin HEAD:" + own + "). Refused: " + s.why + "."
	return editAnswer("deny", reason, reason)
}

// ChildPushInputUnreadAnswer is the deny of a leg whose input could not be read whole: the command it carries is not known.
func ChildPushInputUnreadAnswer() string {
	reason := "[crw CHILD-PUSH-GUARD] 통합은 부모·통합기 몫 (integration is the parent's and the integrator's): the hook input cannot be read whole, so whether the command pushes another branch is not known."
	return editAnswer("deny", reason, reason)
}

// ChildPushOwnBranch is the branch a child in cwd may push: the branch its checkout has, unless that is an integration branch (dev
// and main, which the canon's delivery grant never covers, or the branch origin's HEAD names) or the parent's checkout has it, in
// any letter case, or its name begins with refs/, heads/, tags/ or remotes/; then the child has none. git resolves a push
// destination without refs/ on the remote by the rules refs/%s, refs/tags/%s, refs/heads/%s, refs/remotes/%s, so a branch named
// heads/dev reaches refs/heads/dev; a name without those prefixes reaches a branch of the remote only under refs/heads/<name>. The
// checkout is read at each command, so a branch renamed to dev or heads/dev is not the own one.
func ChildPushOwnBranch(ctx context.Context, cwd, parent string) string {
	branch := func(dir, ref string) string {
		out, err := gitprobe.Command(ctx, dir, nil, "symbolic-ref", "-q", ref).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	own, _ := strings.CutPrefix(branch(cwd, "HEAD"), "refs/heads/")
	lower := strings.ToLower(own)
	for _, p := range []string{"refs/", "heads/", "tags/", "remotes/"} {
		if strings.HasPrefix(lower, p) {
			return ""
		}
	}
	if own == "" || lower == "dev" || lower == "main" || strings.EqualFold("refs/remotes/origin/"+own, branch(cwd, "refs/remotes/origin/HEAD")) ||
		parent != "" && strings.EqualFold(branch(parent, "HEAD"), "refs/heads/"+own) {
		return ""
	}
	return own
}

// childPushScan is what a text shows of pushing: the git push executions it runs, the bodies of its scripts included, or why it
// cannot be read.
type childPushScan struct {
	pushes []childPush
	why    string
}

// childPush is one git push: its words after push, and whether find or xargs gives it words.
type childPush struct {
	args []shellir.Word
	feed bool
}

func (s *childPushScan) text(command, cwd string, depth int, outer *githubPostWrites, cdpath bool) {
	res, err := shellir.AnalyzeScriptEnv(command, cwd, cdpath, githubPostEnv)
	if err != nil {
		s.why = "the command cannot be read: " + unreadableCause(err)
		return
	}
	var written *githubPostTextWrites
	for i, e := range res.Execs {
		dir := e.Dir.Path
		if !e.Dir.Known {
			dir = githubPostNoDir
		}
		var name, body string
		kind := githubPostFileShell
		switch {
		case e.Kind == shellir.KindScriptFile:
			name = e.Script.Value
			var ok bool
			if body, ok = githubPostReadScript(name, dir); !e.Script.Known || !ok {
				s.why = githubPostScriptReason("script file cannot be read", name)
				return
			}
			if e.Name == "sed" || strings.HasSuffix(e.Name, "awk") {
				kind = githubPostFileOther
			}
		case basename(e.Name) == "git":
			s.git(e)
			continue
		case e.Inline != nil:
			s.mentions(e.Inline.Source, "an inline program")
			continue
		case githubPostRunner(e.Name) || childPushInterpreter(e.Name):
			for _, a := range e.Args {
				if s.mentions(a, "the arguments of "+e.Name); a.Known && s.why == "" && childPushInterpreter(e.Name) {
					if text, ok := githubPostReadScript(a.Value, dir); ok {
						s.mentions(shellir.Word{Known: true, Value: text}, "the program file "+a.Value)
					}
				}
			}
			continue
		case githubPostDirectPath(e):
			name = e.Program.Value
			if body, kind = githubPostReadDirect(name, dir); kind == githubPostFileBinary || kind == githubPostFileAbsent {
				continue
			}
		default:
			continue
		}
		if written == nil {
			written = githubPostWritesOf(res.Execs, outer)
		}
		switch {
		case kind == githubPostFileUnreadable || written.stale(i, name, e.Dir):
			s.why = githubPostScriptReason("script file cannot be read before it runs", name)
		case kind != githubPostFileShell:
			s.mentions(shellir.Word{Known: true, Value: body}, "the script file "+name)
		case depth >= githubPostMaxScriptDepth:
			s.why = "scripts run scripts too deeply"
		default:
			s.text(body, dir, depth+1, written.at(i).as(githubPostBodyKey(name, e.Dir)), e.Cdpath)
		}
		if s.why != "" {
			return
		}
	}
}

// mentions refuses a word the reader cannot evaluate, or one that spells push, where a program the guard does not read gets it.
func (s *childPushScan) mentions(w shellir.Word, what string) {
	if s.why == "" && (!w.Known || childPushSpells(w.Value)) {
		s.why = what + " may push, and the guard does not read it"
	}
}

// childPushSpells is whether a text spells push once quotes and backslashes are taken out, in any letter case.
func childPushSpells(v string) bool {
	return strings.Contains(strings.ToLower(strings.NewReplacer("\"", "", "'", "", "\\", "").Replace(v)), "push")
}

// childPushInterpreter is whether a program runs a program file it is given: the guard reads that file for a push.
func childPushInterpreter(name string) bool {
	for _, p := range []string{"python", "node", "perl", "ruby", "php", "bun", "deno", "lua", "tclsh", "Rscript", "osascript"} {
		if strings.HasPrefix(basename(name), p) {
			return true
		}
	}
	return false
}

// git records a git execution whose subcommand is push.
func (s *childPushScan) git(e shellir.Exec) {
	for i := 0; i < len(e.Args); i++ {
		v := e.Args[i].Value
		switch {
		case !e.Args[i].Known:
			s.why = "a git argument cannot be read"
			return
		case v == "-C" || v == "--git-dir" || v == "--work-tree" || v == "--namespace" || v == "-c" || v == "--super-prefix":
			i++
		case strings.HasPrefix(v, "-"):
		case v == "push":
			s.pushes = append(s.pushes, childPush{args: e.Args[i+1:], feed: e.Ctx.Feed != nil})
			return
		default:
			return
		}
	}
}

// childPushFlags are the push options that take no value and move nothing but the refspecs: everything else (--all, --mirror,
// --tags, --repo, --receive-pack, a submodule push) is refused.
var childPushFlags = map[string]bool{"--set-upstream": true, "--force": true, "--force-with-lease": true, "--no-force-with-lease": true,
	"--force-if-includes": true, "--no-force-if-includes": true, "--quiet": true, "--verbose": true, "--progress": true, "--no-progress": true,
	"--dry-run": true, "--porcelain": true, "--atomic": true, "--no-atomic": true, "--no-verify": true, "--verify": true, "--thin": true,
	"--no-thin": true, "--ipv4": true, "--ipv6": true, "--delete": true, "--recurse-submodules=no": true, "--recurse-submodules=check": true}

// childPushJudge is why one push is refused for a child whose own branch is own, or "" when it pushes own only.
func childPushJudge(p childPush, own string) string {
	if own == "" {
		return "this session has no branch of its own (its checkout has none, an integration branch or a name under refs/, heads/, tags/ or remotes/, or the parent's checkout has the same)"
	}
	if p.feed {
		return "find or xargs gives the push its words"
	}
	var operands []string
	options := true
	for i := 0; i < len(p.args); i++ {
		if !p.args[i].Known {
			return "a word of the push cannot be read"
		}
		v := p.args[i].Value
		switch {
		case !options || v == "-" || !strings.HasPrefix(v, "-"):
			operands = append(operands, v)
		case v == "--":
			options = false
		case v == "--push-option" || v == "-o":
			i++
		case strings.HasPrefix(v, "--"):
			if !childPushFlags[v] && !strings.HasPrefix(v, "--force-with-lease=") && !strings.HasPrefix(v, "--push-option=") {
				return "git push " + v + " is not an option the guard reads"
			}
		default:
			// a bundle of short flags; -o takes the rest of the bundle, or the next word, as its value
			for j := 1; j < len(v) && v[j] != 'o'; j++ {
				if !strings.ContainsRune("ufqvnd46", rune(v[j])) {
					return "git push " + v + " is not an option the guard reads"
				}
			}
			if strings.HasSuffix(v, "o") && strings.Count(v, "o") == 1 {
				i++
			}
		}
	}
	if len(operands) < 2 {
		return "a push without a refspec goes where git configuration says"
	}
	for _, ref := range operands[1:] {
		src, dst, colon := strings.Cut(strings.TrimPrefix(ref, "+"), ":")
		switch {
		case strings.Contains(ref, "*") || strings.HasPrefix(src, "^"):
			return "the refspec " + ref + " is a pattern"
		case !colon:
			// without a destination, remote.<name>.push (or push.default) decides where src goes
			return "the refspec " + ref + " names no destination, which git configuration then chooses"
		case strings.TrimPrefix(dst, "refs/heads/") != own:
			return "git push " + operands[0] + " " + ref + " does not push " + own
		}
	}
	return ""
}
