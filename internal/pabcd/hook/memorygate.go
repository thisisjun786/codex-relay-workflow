package hook

import (
	"os"
	"path"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// MEMORY-WRITE-GATE-01 (pabcd-state/src/memory-write-gate.ts, CXC v0.2.40, 3c1459ac): a note, or a file under the Codex
// memories directory, is written only when this session holds an explicit request for it, a CLI grant first and then the
// turn-scoped marker the prompt hook records. The detector of that marker is DetectMemoryWriteRequest (detect.go) and the
// destinations of a shell command are ShellWriteDestinations (shellwrite.go); the edit tool names are editTool (lint.go).

// MemoryWriteAttempt says why a PreToolUse call counts as a memory write.
type MemoryWriteAttempt struct {
	Surface string // "tool", "edit" or "shell"; empty when the call is no memory write
	Target  string // the destination that triggered the judgement, as the deny reason names it
}

// HandleMemoryWriteGate is handleMemoryWriteGate, the PreToolUse leg: the deny envelope for a memory write nobody asked for,
// else empty. Anything that is not one PreToolUse object, and any tool that writes nothing under the memories root, passes.
// The leg is Recover in the harness, so a panic answers nothing, as the oracle's try/catch does.
func HandleMemoryWriteGate(raw string, env host.LookupEnv) string {
	return memoryGateHandle(raw, env, state.WriteState)
}

// memoryGateHandle takes the state write as an argument so that a test can fail it.
func memoryGateHandle(raw string, env host.LookupEnv, write func(string, state.State) error) string {
	p := editObject(raw)
	if p["hook_event_name"] != "PreToolUse" {
		return ""
	}
	tool, _ := p["tool_name"].(string)
	cwd, _ := p["cwd"].(string)
	sid, _ := p["session_id"].(string)
	turn, _ := p["turn_id"].(string)
	if tool == "" {
		return ""
	}
	attempt := memoryGateClassify(tool, p["tool_input"], cwd, env)
	if attempt.Surface == "" {
		return ""
	}
	reason := memoryGateReason(attempt, sid, cwd)
	// With no cwd or no session id there is no state to consult, and a write nobody can prove was asked for is what the gate
	// stops; this deny is not the fail-open exception, which covers a crash.
	if cwd != "" && sid != "" {
		allowed, note := memoryGateConsume(cwd, sid, turn, write)
		if allowed {
			return ""
		}
		reason += note
	}
	return editAnswer("deny", reason, reason)
}

func memoryGateReason(a MemoryWriteAttempt, sid, cwd string) string {
	what := "a file under the Codex memories directory (" + a.Target + ")"
	if a.Surface == "tool" {
		what = "a memory note (" + a.Target + ")"
	}
	where := cwd
	if where == "" {
		where = "the session working directory"
	}
	if sid == "" {
		sid = "<id>"
	}
	return strings.Join([]string{
		"[crw MEMORY-WRITE-GATE] Blocked a write of " + what + ": this session has no explicit user request to remember anything.",
		"Memory notes outlive crw and reach every later session, so they are written only when the user asks.",
		"Two ways forward: ask the user to confirm they want this remembered (a prompt such as \"기억해둬\" or \"remember this\" authorizes the next write),",
		"or record an explicit grant with `crw pabcd memory allow-write --session " + sid + "` from " + where + ".",
		"The grant is stored per cwd; issuing it from a different working directory will print success and never be seen by this hook.",
		"If the user did ask, say so and retry — the request must appear in their own message, not in yours.",
	}, " ")
}

// memoryGateSpend names the authorization the state holds for this turn: the CLI grant first, then the marker. A marker that
// belongs to another turn was spent there; a call that names no turn, or a marker that names none, spends it outright.
func memoryGateSpend(s state.State, turn string) string {
	switch {
	case s.MemoryWriteGrant:
		return "grant"
	case !s.MemoryWriteRequested:
		return ""
	case turn != "" && s.MemoryWriteTurn != nil && *s.MemoryWriteTurn != turn:
		return ""
	}
	return "marker"
}

// memoryGateConsume spends one authorization and says whether the write may go ahead, and why not when the state kept it
// from being spent. The oracle read and wrote the state with no lock, so two calls could spend one grant; here the check is
// repeated inside the session lock, and a call is allowed only once the write that spends its authorization has succeeded.
// A state with nothing to spend is answered from an unlocked read, which touches nothing (taking the lock would create the
// sessions directory in the workspace). The CLI grant clears only itself: the oracle also cleared the turn of the marker
// beside it, which let any later turn spend a marker meant for one.
func memoryGateConsume(cwd, sid, turn string, write func(string, state.State) error) (allowed bool, note string) {
	if memoryGateSpend(state.ReadState(cwd, sid), turn) == "" {
		return false, ""
	}
	file := state.StatePath(cwd, sid)
	err := state.WithSessionLock(cwd, sid, func() error {
		s, unreadable := state.ReadStateStrict(cwd, sid)
		kind := memoryGateSpend(s, turn)
		if unreadable || kind == "" {
			return nil
		}
		if !memoryGateRewritable(file, s) {
			note = " The authorization could not be spent because the session state holds records this hook cannot rewrite without losing them; repair " + file + " first."
			return nil
		}
		if kind == "grant" {
			s.MemoryWriteGrant = false
		} else {
			s.MemoryWriteRequested, s.MemoryWriteTurn = false, nil
		}
		// The authorization is spent in the state every reader sees once the write published it, so a failure after the
		// rename (state.Published) is a spent authorization and this call, which it authorized, goes ahead.
		if err := write(cwd, s); err != nil && !state.Published(err) {
			return err
		}
		allowed = true
		return nil
	})
	if err != nil {
		return false, " The authorization could not be spent because the session state is locked or cannot be written (" + file + "); retry, or remove a stale " + file + ".lock by hand."
	}
	return allowed, note
}

// memoryGateRewritable says whether writing back the state the reader rebuilt would keep every record: each stored unverified
// subagent must come back as it was stored, an interview tracker longer than the reader keeps would lose its oldest entries, and
// no record class the shared judgement covers would change (state.RewriteKeepsStored). A legacy D-close marker is refused
// separately, as this writer always has (state.DcloseRecoveryLegacy). The memory allow-write command and the scan and evidence
// commands refuse on the same judgement.
func memoryGateRewritable(file string, s state.State) bool {
	if state.DcloseRecoveryLegacy(s) {
		return false
	}
	raw, err := os.ReadFile(file)
	return err == nil && state.RewriteKeepsStored(raw, s)
}

// memoryGateClassify is classifyMemoryWrite with the protected root worked out from env.
func memoryGateClassify(tool string, input any, cwd string, env host.LookupEnv) MemoryWriteAttempt {
	g := newMemoryGateEnv(env)
	root, ok := g.root()
	if !ok {
		return MemoryWriteAttempt{}
	}
	record, _ := input.(map[string]any) // a bare string (unparseable arguments), an array or null is no record
	switch {
	case memoryGateToolName(tool):
		name, _ := record["filename"].(string)
		if name == "" {
			name = "(ad hoc note)"
		}
		return MemoryWriteAttempt{Surface: "tool", Target: name}
	case record == nil:
	case editTool(tool):
		command, _ := record["command"].(string)
		candidates := []string{}
		if direct, _ := record["file_path"].(string); direct != "" {
			candidates = append(candidates, direct)
		} else {
			candidates = memoryGatePatchTargets(command)
		}
		dir := shellirPayloadCwd(cwd)
		for _, candidate := range candidates {
			if dir == "" && !path.IsAbs(candidate) {
				return MemoryWriteAttempt{Surface: "edit", Target: "(a destination the gate cannot read)"}
			}
			if target, ok := g.hit(candidate, dir, root); ok {
				return MemoryWriteAttempt{Surface: "edit", Target: target}
			}
		}
	case memoryGateShellTool(tool):
		// A path that is only read, quoted, or in a heredoc body is no write; redirections, tee, sed -i, cp and mv targets and
		// perl and ruby -i operands are the write surface (ShellWriteDestinations).
		command, _ := record["command"].(string)
		dir := shellirPayloadCwd(cwd)
		if dests, readable := shellIRWriteDestsResolved(command, dir, env); readable {
			for _, token := range dests {
				if token == shellIRUnknownDest {
					return MemoryWriteAttempt{Surface: "shell", Target: "(a destination the gate cannot read)"}
				}
				if target, ok := g.hit(token, dir, root); ok {
					return MemoryWriteAttempt{Surface: "shell", Target: target}
				}
			}
		}
		// A Python program the reader cannot finish - an f-string replacement field it cannot walk - may hold a write
		// it never sees, so it is a write attempt of its own and the gate fails closed (CRW-741).
		if what, ok := shellIRFStringUnreadable(command); ok {
			return MemoryWriteAttempt{Surface: "shell", Target: "(a program the gate cannot read: " + what + ")"}
		}
		// A shell program position the outer shell builds at run time - a -c program, an eval operand, a source
		// operand, a shell reading a pipe, a here-string or a here-document - may hold a write the destination reader
		// never sees, so it is a write attempt of its own and the gate fails closed (CRW-726, beside CRW-741's check).
		if !memoryGateShellReadable(command, dir, env) {
			return MemoryWriteAttempt{Surface: "shell", Target: "(a program the gate cannot read: the command reader refused it)"}
		}
	}
	return MemoryWriteAttempt{}
}

// memoryGateShellReadable is whether every reading the gate makes of a shell command succeeds: the destination reading with the
// session's environment, and the readings with none in the payload's directory and in no directory (the f-string check). A command
// one of them refuses is a write attempt of its own. The differential fuzz counts the commands this refuses
// (MemoryGateCommandReadable).
func memoryGateShellReadable(command, dir string, env host.LookupEnv) bool {
	if _, err := shellir.AnalyzeEnv(command, dir, env); err != nil {
		return false
	}
	if _, err := shellir.Analyze(command, dir); err != nil {
		return false
	}
	_, err := shellir.AnalyzeNoDir(command)
	return err == nil
}

// memoryGateToolName: flat_tool_name joins the namespace and the name with no separator, so the hook sees
// memoriesadd_ad_hoc_note; the punctuated forms are defensive against a separator appearing later.
func memoryGateToolName(tool string) bool {
	switch tool {
	case "memoriesadd_ad_hoc_note", "memories.add_ad_hoc_note", "memories_add_ad_hoc_note", "add_ad_hoc_note":
		return true
	}
	return false
}

func memoryGateShellTool(tool string) bool {
	switch tool {
	case "Bash", "shell", "exec_command", "local_shell":
		return true
	}
	return false
}

// memoryGatePatchTargets is patchTargets: the destinations an apply_patch envelope names, on its Add, Update, Delete and Move
// File directives and its unified +++ headers. The oracle's Move File: is not apply_patch grammar, whose move destination is
// `*** Move to: <path>`, so that is read too. No regular expression: nothing here may be built at package level.
func memoryGatePatchTargets(patch string) []string {
	out := []string{}
	for _, line := range text.SplitLines(patch) {
		if rest, ok := strings.CutPrefix(text.Trim(line), "*** "); ok {
			if target, ok := memoryGateDirective(rest); ok {
				out = append(out, text.Trim(target))
				continue
			}
		}
		if rest, ok := strings.CutPrefix(line, "+++ "); ok {
			if len(rest) > 2 && strings.HasPrefix(rest, "b/") {
				rest = rest[2:]
			}
			if memoryGateDot(rest) {
				out = append(out, text.Trim(rest))
			}
		}
	}
	return out
}

func memoryGateDirective(s string) (string, bool) {
	for _, verb := range [...]string{"Add File: ", "Update File: ", "Delete File: ", "Move File: ", "Move to: "} {
		if target, ok := strings.CutPrefix(s, verb); ok && memoryGateDot(target) {
			return target, true
		}
	}
	return "", false
}

// memoryGateDot is what /.+/ must match to the end of the line: something. The oracle's JavaScript dot refuses U+2028 and
// U+2029, so a patch path holding one was never read, although apply_patch accepts such a name.
func memoryGateDot(s string) bool { return s != "" }

// memoryGateEnv is what the gate reads of the environment: the home directory (Node's os.homedir(), which the oracle's
// expandHomePrefix asks for on every path) and CODEX_HOME, which names the memories root.
type memoryGateEnv struct {
	home, codexHome string
	homeOK          bool
}

func newMemoryGateEnv(env host.LookupEnv) memoryGateEnv {
	home, err := host.Home(env)
	g := memoryGateEnv{home: home, homeOK: err == nil}
	if v, ok := env("CODEX_HOME"); ok {
		g.codexHome = text.Trim(v)
	}
	return g
}

// root is memoriesRoot: $CODEX_HOME/memories, else ~/.codex/memories, made absolute and not cleaned. The oracle cleaned it
// with path.join, which turns "<link>/../memories" into the sibling of the link's own directory, while an open follows the
// link and goes up from where it leads; memoryGateIsPath therefore reads this one string two ways, its clean as the text root
// and the place an open of it reaches as the physical root (known defect, fixed). It is not ok when the oracle's homedir()
// would throw, which ends its judgement in a pass.
func (g memoryGateEnv) root() (string, bool) {
	base := g.codexHome
	if base == "" {
		if !g.homeOK {
			return "", false
		}
		base = ".codex" // an empty home joined to nothing: the root stays relative to the working directory
		if g.home != "" {
			base = g.home + "/.codex"
		}
	}
	root := base + "/memories"
	if !path.IsAbs(root) {
		if wd, err := os.Getwd(); err == nil {
			root = wd + "/" + root
		}
	}
	return root, true
}

type memoryGatePrefix struct{ prefix, base string }

// expand is expandHomePrefix: a home prefix of shell text or of a patch header, in any case, followed by a slash or a
// backslash. Shell text also names the root through ${HOME}, $CODEX_HOME and ${CODEX_HOME}, which the shell expands to it
// and the oracle read as relative paths. The case folding is ASCII only, as the prefixes are: JavaScript does not fold the
// dotted capital I of U+0130 to an ASCII letter. norm is the expansion joined and cleaned as the oracle does; kept is the same
// expansion with the ".." components of the text left alone, because an open follows them from the place a link led to.
func (g memoryGateEnv) expand(raw string) (norm, kept string) {
	if raw == "~" {
		return g.home, g.home
	}
	if strings.HasPrefix(raw, "~/") || strings.HasPrefix(raw, "~\\") {
		return memoryGateJoin(g.home, raw[2:])
	}
	prefixes := []memoryGatePrefix{{"%userprofile%", g.home}, {"$env:userprofile", g.home}, {"$home", g.home}, {"${home}", g.home}}
	if g.codexHome != "" {
		prefixes = append(prefixes, memoryGatePrefix{"$codex_home", g.codexHome}, memoryGatePrefix{"${codex_home}", g.codexHome})
	}
	lower := []byte(raw)
	for i, b := range lower {
		if 'A' <= b && b <= 'Z' {
			lower[i] = b + 'a' - 'A'
		}
	}
	for _, p := range prefixes {
		if string(lower) == p.prefix {
			return p.base, p.base
		}
		if len(raw) > len(p.prefix) && string(lower[:len(p.prefix)]) == p.prefix && (raw[len(p.prefix)] == '/' || raw[len(p.prefix)] == '\\') {
			return memoryGateJoin(p.base, raw[len(p.prefix)+1:])
		}
	}
	return raw, raw
}

func memoryGateJoin(base, rest string) (norm, kept string) {
	if base == "" { // the shell expands an empty home to nothing, so the name is absolute; Node's join made it relative
		return path.Clean("/" + rest), "/" + rest
	}
	return path.Join(base, rest), base + "/" + rest
}

type memoryGatePath struct{ clean, joined string }

// abs is absolutize: the quotes off the ends, the home prefix expanded, backslashes read as slashes, and the path resolved
// against cwd. clean is that path, which the lexical test reads and the deny reason names; joined is the same path with its
// ".." components kept, which the physical test follows. These readings are the oracle's, made for Windows text: they trim
// the ends, strip quotes, expand a home prefix and turn a backslash into a separator. On POSIX the destination the shell
// parser or the tool hands over is an exact name, whose ends may be a space or a quote, whose first word may look like a
// home prefix and whose backslash is a character (a directory called `\..` is one component), so each reading may also be
// left out, and the oracle's, with all three, stays first. A relative name is opened from the physical cwd, whose own
// links are followed on their own budget.
func (g memoryGateEnv) abs(raw, cwd string) []memoryGatePath {
	if !g.homeOK {
		return nil
	}
	base := cwd
	if cwd != "" {
		if !path.IsAbs(cwd) {
			wd, _ := os.Getwd()
			base = wd + "/" + cwd
		}
		base = memoryGateReal(base, true)
	}
	v := text.Trim(raw)
	head, tail := v != "" && (v[0] == '"' || v[0] == '\''), len(v) > 1 && (v[len(v)-1] == '"' || v[len(v)-1] == '\'')
	if tail {
		v = v[:len(v)-1]
	}
	if head {
		v = v[1:]
	}
	var out []memoryGatePath
	for _, as := range [...]string{v, raw} { // every combination of the three readings is a candidate; duplicates are dropped
		if as == "" {
			continue
		}
		for _, expand := range [...]bool{true, false} {
			norm, kept := as, as
			if expand {
				norm, kept = g.expand(as)
			}
			for _, slashes := range [...]bool{true, false} {
				n, k := norm, kept
				if slashes {
					n, k = strings.ReplaceAll(n, "\\", "/"), strings.ReplaceAll(k, "\\", "/")
				} else if !strings.Contains(k, "\\") {
					continue
				}
				var p memoryGatePath
				switch {
				case path.IsAbs(n):
					p = memoryGatePath{path.Clean(n), k}
				case cwd != "":
					p = memoryGatePath{resolveFrom(cwd, n), base + "/" + k}
				default:
					continue
				}
				if !slices.Contains(out, p) {
					out = append(out, p)
				}
			}
		}
	}
	return out
}

// hit says whether a destination named in a command or a patch is the memories root or inside it, and names it as the deny does.
func (g memoryGateEnv) hit(raw, cwd, root string) (string, bool) {
	for _, p := range g.abs(raw, cwd) {
		if memoryGateIsPath(p.clean, p.joined, root) {
			return p.clean, true
		}
	}
	return "", false
}

// memoryGateIsPath is isMemoryPath: the path is the memories root or inside it, on a separator boundary so that a sibling
// such as memories-backup is not. The oracle compared the text only; a symlink in the workspace that leads into the root
// writes the same bytes, so the places the paths reach count too: the place the last component leads to, and the entry
// itself, because a rename over a link inside the root replaces the link and not what it points at. The root is read both
// ways too (see root): a place inside its cleaned text or inside the place an open of it reaches is a memory path.
func memoryGateIsPath(clean, joined, root string) bool {
	if clean == "" {
		return false
	}
	roots := [2]string{path.Clean(root), memoryGateReal(root, true)}
	within := func(p string) bool { return memoryGateWithin(p, roots[0]) || memoryGateWithin(p, roots[1]) }
	return within(clean) || within(memoryGateReal(joined, true)) || within(memoryGateReal(joined, false))
}

func memoryGateWithin(p, root string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/") // a root that is "/" holds every absolute path
}

// memoryGateReal is the place an open of the absolute path p reaches: each component that is a symlink is replaced by its
// target (a dangling link too), and a ".." goes up from the place reached so far, not from the text before it. A component
// that does not exist stays as written; a chain of more than 40 links is left alone. With follow false the last component
// is kept as the entry it names.
func memoryGateReal(p string, follow bool) string {
	resolved, rest, hops := "/", strings.Split(p, "/"), 0
	for len(rest) > 0 {
		c := rest[0]
		rest = rest[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			resolved = path.Dir(resolved)
			continue
		}
		next := path.Join(resolved, c)
		if len(rest) == 0 && !follow {
			resolved = next
			continue
		}
		target, err := os.Readlink(next)
		if err != nil {
			resolved = next
			continue
		}
		if hops++; hops > 40 {
			return p
		}
		if path.IsAbs(target) {
			resolved = "/"
		}
		rest = append(strings.Split(target, "/"), rest...)
	}
	return resolved
}
