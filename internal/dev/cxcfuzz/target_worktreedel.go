//go:build dev

package cxcfuzz

import (
	"errors"
	"math/rand"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// errNotAnObject is the target's own failure for an input that is not the JSON object the grammar
// describes, so a failure on either side compares as the same kind of answer. It is built at the
// call, not held in a package-level variable: the issue's runtime criterion forbids an initializer
// that does work when the program starts.
func errNotAnObject() error {
	return errors.New("the input is not a JSON object")
}

// worktreeDelTarget fuzzes the managed-worktree deletion guard: the Go side is
// hook.HandleWorktreeGuardPreTool (internal/pabcd/hook/worktreedel.go) and the oracle side is
// pabcd-state/dist/worktree-guard.js handleWorktreeGuardPreTool. The two answers are compared as the
// allow/deny decision plus the deny reason.
func worktreeDelTarget() Target {
	return Target{
		Name:     "worktreedel",
		Generate: worktreeDelGenerate,
		Go:       worktreeDelGo,
		Oracle:   Oracle{Command: "node", Shim: shimPath("worktreedel"), Root: DefaultOracleRoot},
		Compare:  worktreeDelCompare,
		Reading:  worktreeDelReading,
	}
}

// The managed worktree every scenario builds: <root>/home/.codex/worktrees/zk3q. The slot id is one
// the port's own tests use, so no temporary path holds it by accident.
const (
	worktreeDelSlot      = "zk3q"
	worktreeDelRepo      = "repo"
	worktreeDelSibling   = "sibling"
	worktreeDelOtherRoot = "home/other-roots"
	worktreeDelOtherSlot = "q7mv"
)

// worktreeDelEntry is one "fs" entry of the harness's scenario array.
func worktreeDelEntry(path, kind, content, target string, mode int) pyjson.Object {
	entry := pyjson.Object{{Key: "path", Value: path}, {Key: "kind", Value: kind}}
	if content != "" {
		entry = entry.Set("content", content)
	}
	if target != "" {
		entry = entry.Set("target", target)
	}
	if mode != 0 {
		entry = entry.Set("mode", mode)
	}
	return entry
}

// worktreeDelLayout is the managed layout under the case root: the slot's repo with a .git gitfile
// (so detection finds a checkout), a subdirectory, a sibling slot, a directory whose name holds a
// space and one whose name holds a LF, an unrelated directory, and a link that stays inside the root.
func worktreeDelLayout() []any {
	base := "home/.codex/worktrees"
	slot := base + "/" + worktreeDelSlot
	return []any{
		worktreeDelEntry(slot+"/"+worktreeDelRepo, "dir", "", "", 0o755),
		worktreeDelEntry(slot+"/"+worktreeDelRepo+"/.git", "file", "gitdir: /fake/main/.git/worktrees/"+worktreeDelSlot+"\n", "", 0o644),
		worktreeDelEntry(slot+"/"+worktreeDelRepo+"/deep", "dir", "", "", 0o755),
		worktreeDelEntry(slot+"/"+worktreeDelRepo+"/deep/child", "dir", "", "", 0o755),
		worktreeDelEntry(slot+"/"+worktreeDelSibling, "dir", "", "", 0o755),
		worktreeDelEntry(base+"/other slot", "dir", "", "", 0o755),
		worktreeDelEntry(base+"/line\nbreak", "dir", "", "", 0o755),
		worktreeDelEntry("home/elsewhere/build", "dir", "", "", 0o755),
		worktreeDelEntry("home/elsewhere/build/keep", "file", "x", "", 0o644),
		// The link into the slot is written with the ROOT-prefixed absolute form, so it names the
		// managed checkout itself. A relative target would be resolved against the link's own
		// directory (home/), and "home/.codex/..." would then land on <root>/home/home/.codex/...
		// - a path that does not exist, so the link would never reach the checkout and the
		// link-path judgement could not be fuzzed at all.
		worktreeDelEntry("home/link-into-slot", "symlink", "", rootPlaceholder+"/"+slot+"/"+worktreeDelRepo, 0),
		// A second managed root, so a case that declares it can be denied there rather than under the
		// default CODEX_HOME.
		worktreeDelEntry(worktreeDelOtherRoot+"/"+worktreeDelOtherSlot+"/"+worktreeDelRepo, "dir", "", "", 0o755),
		worktreeDelEntry(worktreeDelOtherRoot+"/"+worktreeDelOtherSlot+"/"+worktreeDelRepo+"/.git", "file", "gitdir: /fake/main/.git/worktrees/"+worktreeDelOtherSlot+"\n", "", 0o644),
		worktreeDelEntry("home/elsewhere/codex/worktrees/"+worktreeDelOtherSlot+"/"+worktreeDelRepo, "dir", "", "", 0o755),
		worktreeDelEntry("home/elsewhere/codex/worktrees/"+worktreeDelOtherSlot+"/"+worktreeDelRepo+"/.git", "file", "gitdir: /fake/main/.git/worktrees/"+worktreeDelOtherSlot+"\n", "", 0o644),
	}
}

// worktreeDelCwds are the directories a payload's cwd is taken from: the checkout, two subdirectories
// of it, the slot root, the worktrees root, an unrelated directory, and no cwd at all.
func worktreeDelCwds() []string {
	slot := "home/.codex/worktrees/" + worktreeDelSlot
	return []string{
		slot + "/" + worktreeDelRepo,
		slot + "/" + worktreeDelRepo + "/deep",
		slot + "/" + worktreeDelRepo + "/deep/child",
		slot,
		"home/.codex/worktrees",
		"home/link-into-slot",
		worktreeDelOtherRoot + "/" + worktreeDelOtherSlot + "/" + worktreeDelRepo,
		"home/elsewhere/build",
		"",
	}
}

// worktreeDelCommands are the commands the generator draws from: the removal verbs, their targets,
// the shell wrappers, the substitution forms and the harmless neighbours the issue names. Every
// target is spelled the way a session inside the checkout would write it, so each side resolves it
// against its own root.
func worktreeDelCommands() []string {
	return []string{
		"rm -rf .",
		"rm -rf ..",
		"rm -rf ../" + worktreeDelRepo,
		"rm -rf ../../" + worktreeDelSlot,
		"rm -r .",
		"rm -rf $HOME/.codex/worktrees/" + worktreeDelSlot,
		"rm -rf ~/.codex/worktrees/" + worktreeDelSlot,
		"rm -rf ../*",
		"rm -rf ./",
		"rm -rf \"$PWD/../" + worktreeDelRepo + "\"",
		"rm -rf '../" + worktreeDelRepo + "'",
		"rm -rf /tmp/unrelated",
		"rm -f ./file",
		"rmdir ..",
		"rmdir ../" + worktreeDelRepo,
		"rmdir /tmp/unrelated",
		"find . -delete",
		"git worktree remove .",
		"git worktree remove ../" + worktreeDelRepo,
		"git -C . worktree remove ..",
		"mv . /tmp/elsewhere",
		"mv ../" + worktreeDelRepo + " /tmp/elsewhere",
		"cd .. && rm -rf .",
		"cd /tmp && rm -rf .",
		"sh -c 'rm -rf .'",
		"bash -c \"rm -rf ../" + worktreeDelRepo + "\"",
		"sh -c 'r\\\nm -rf .'",
		"eval 'rm -rf .'",
		"eval \"eval 'rm -rf .'\"",
		"su -c 'rm -rf .'",
		"su -cPROG",
		"echo $(rm -rf ../" + worktreeDelRepo + ")",
		"echo \u0060rm -rf .\u0060",
		"printf '%s' \"$(rm -rf .)\"",
		"sudo rm -rf .",
		"command rm -rf ..",
		"env FOO=1 rm -rf .",
		"ls -la",
		"git status",
		"rm -rf .#keep",
	}
}

// worktreeDelDeepEval wraps one removal in n nested evals: the reader follows a program string down
// to its depth budget, so this is where a program nested past that budget is judged.
func worktreeDelDeepEval(n int) string {
	command := "rm -rf ."
	for i := 0; i < n; i++ {
		command = "eval " + worktreeDelQuoteForShell(command)
	}
	return command
}

// worktreeDelQuoteForShell wraps a command in single quotes, escaping any it already holds.
func worktreeDelQuoteForShell(command string) string {
	return "'" + strings.ReplaceAll(command, "'", "'\\''") + "'"
}

// worktreeDelGenerate builds one case. It is deterministic for a given rng, and it never emits a
// float: the harness's canonical form would spell one differently from the oracle's.
func worktreeDelGenerate(rng *rand.Rand, size int) any {
	cwds := worktreeDelCwds()
	commands := worktreeDelCommands()
	command := commands[rng.Intn(len(commands))]
	if rng.Intn(8) == 0 {
		command = worktreeDelDeepEval(1 + rng.Intn(10))
	}
	if rng.Intn(16) == 0 {
		command = ""
	}
	tool := "Bash"
	if rng.Intn(8) == 0 {
		tool = []string{"Read", "Write", "bash"}[rng.Intn(3)]
	}
	event := "PreToolUse"
	if rng.Intn(16) == 0 {
		event = []string{"SessionStart", "PostToolUse", ""}[rng.Intn(3)]
	}
	// A case may point CODEX_HOME at another directory, or unset it, so the managed root is not
	// always the one the layout builds.
	var env pyjson.Object
	switch rng.Intn(6) {
	case 0:
		env = env.Set("CODEX_HOME", "home/elsewhere/codex")
	case 1:
		env = env.Set("CODEX_HOME", nil)
	}
	var roots []string
	if rng.Intn(6) == 0 {
		roots = append(roots, worktreeDelOtherRoot)
	}
	value := pyjson.Object{
		{Key: "fs", Value: worktreeDelEntries(roots)},
		{Key: "cwd", Value: cwds[rng.Intn(len(cwds))]},
		{Key: "command", Value: command},
		{Key: "tool", Value: tool},
		{Key: "event", Value: event},
	}
	if len(env) > 0 {
		value = value.Set("env", env)
	}
	if len(roots) > 0 {
		value = value.Set("worktree_roots", worktreeDelStrings(roots))
	}
	return value
}

// worktreeDelEntries is the layout plus the extra worktree roots a case declares.
func worktreeDelEntries(roots []string) []any {
	entries := worktreeDelLayout()
	for _, root := range roots {
		entries = append(entries, worktreeDelEntry(root, "dir", "", "", 0o755))
	}
	return entries
}

func worktreeDelStrings(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

// worktreeDelGo is the target's Go side: it builds the PreToolUse payload from the case and calls
// the guard with a host.LookupEnv over the case's environment, then reduces the hook's answer to the
// decision and reason the comparison names. An empty answer is an allow with no reason.
func worktreeDelGo(input any, env Env) (any, error) {
	object, ok := input.(pyjson.Object)
	if !ok {
		return nil, errNotAnObject()
	}
	payload := pyjson.Object{
		{Key: "hook_event_name", Value: worktreeDelText(object, "event")},
		{Key: "cwd", Value: worktreeDelCwd(object, env)},
		{Key: "tool_name", Value: worktreeDelText(object, "tool")},
		{Key: "tool_input", Value: pyjson.Object{{Key: "command", Value: worktreeDelText(object, "command")}}},
	}
	raw := pyjson.Dumps(payload, pyjson.Options{SortKeys: false})
	out := hook.HandleWorktreeGuardPreTool(raw, worktreeDelEnv(object, env))
	answer, err := worktreeDelAnswer(out)
	if err != nil {
		return nil, err
	}
	// The guard canonicalizes the cwd before it names the slot, so the reason carries a resolved
	// path while the harness rewrites the root it made. Both sides write the case root the way the
	// harness spelled it, so the two answers compare.
	return worktreeDelAsGivenRoot(answer, env.Root), nil
}

// worktreeDelCwd is the payload's cwd: the case's root-relative cwd joined against this side's own
// root, or the empty string the case asked for. An empty cwd stays empty, because the oracle answers
// an empty cwd before it reads anything else, and joining would hand it the root instead.
func worktreeDelCwd(object pyjson.Object, env Env) string {
	cwd := worktreeDelText(object, "cwd")
	if cwd == "" {
		return ""
	}
	return filepath.Join(env.Root, cwd)
}

// worktreeDelText reads one text field of the case.
func worktreeDelText(object pyjson.Object, key string) string {
	value, found := object.Lookup(key)
	if !found {
		return ""
	}
	text, _ := value.(string)
	return text
}

// worktreeDelEnv builds the environment the guard reads. HOME is the case's own home and CODEX_HOME
// is deliberately absent, so both sides resolve the worktrees root the way the issue's layout
// describes: the oracle falls back to homedir()/.codex and the port to HOME + "/.codex", both
// <root>/home/.codex. The case's own "env" object then overrides a name, or unsets it with a JSON
// null — an unset variable and an empty one are distinct to resolveCodexHome, so the table is not a
// plain map.
func worktreeDelEnv(object pyjson.Object, env Env) host.LookupEnv {
	values := map[string]string{
		"HOME":     env.Home,
		"CRW_HOME": env.CrwHome,
		"TMPDIR":   env.TmpDir,
	}
	unset := map[string]bool{}
	if value, found := object.Lookup("env"); found {
		if table, ok := value.(pyjson.Object); ok {
			for _, item := range table {
				if item.Value == nil {
					unset[item.Key] = true
					continue
				}
				if text, ok := item.Value.(string); ok {
					values[item.Key] = filepath.Join(env.Root, text)
				}
			}
		}
	}
	if value, found := object.Lookup("worktree_roots"); found {
		if list, ok := value.([]any); ok {
			var roots []string
			for _, item := range list {
				if root, ok := item.(string); ok {
					roots = append(roots, filepath.Join(env.Root, root))
				}
			}
			values["CRW_WORKTREE_ROOTS"] = strings.Join(roots, string(filepath.ListSeparator))
		}
	}
	return func(key string) (string, bool) {
		if unset[key] {
			return "", false
		}
		value, ok := values[key]
		return value, ok
	}
}

// worktreeDelAnswer reduces the hook's answer to {decision, reason}. The hook answers either an empty
// string (allow) or the PreToolUse envelope whose decision is deny and whose reason is written twice.
func worktreeDelAnswer(out string) (any, error) {
	if strings.TrimSpace(out) == "" {
		return worktreeDelAllow(), nil
	}
	parsed, err := pyjson.Loads(out, pyjson.LoadOptions{Python: true, Constants: true, Surrogates: true, Deep: true})
	if err != nil {
		return nil, err
	}
	object, ok := parsed.(pyjson.Object)
	if !ok {
		return nil, errNotAnObject()
	}
	specific, found := object.Lookup("hookSpecificOutput")
	if !found {
		return worktreeDelAllow(), nil
	}
	inner, ok := specific.(pyjson.Object)
	if !ok {
		return nil, errNotAnObject()
	}
	decision, _ := inner.Lookup("permissionDecision")
	text, _ := decision.(string)
	if text != "deny" {
		return worktreeDelAllow(), nil
	}
	reason, _ := inner.Lookup("permissionDecisionReason")
	text, _ = reason.(string)
	return pyjson.Object{{Key: "decision", Value: "deny"}, {Key: "reason", Value: text}}, nil
}

func worktreeDelAllow() pyjson.Object {
	return pyjson.Object{{Key: "decision", Value: "allow"}, {Key: "reason", Value: ""}}
}

// worktreeDelAsGivenRoot rewrites a resolved spelling of the case root back to the spelling the
// harness gave, in every string of the answer.
func worktreeDelAsGivenRoot(value any, root string) any {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved == root {
		return value
	}
	return worktreeDelRewrite(value, resolved, root)
}

func worktreeDelRewrite(value any, from, to string) any {
	switch v := value.(type) {
	case string:
		return strings.ReplaceAll(v, from, to)
	case pyjson.Object:
		out := make(pyjson.Object, 0, len(v))
		for _, item := range v {
			out = append(out, pyjson.Field{Key: strings.ReplaceAll(item.Key, from, to), Value: worktreeDelRewrite(item.Value, from, to)})
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, worktreeDelRewrite(item, from, to))
		}
		return out
	default:
		return value
	}
}

// worktreeDelCompare is the issue's comparison: the oracle's deny against the Go side's allow is a
// miss (the guard let through what it must refuse), a Go-only deny is an extra, and two denies whose
// reasons differ are a differ.
func worktreeDelCompare(goOut, oracleOut any) Verdict {
	goDecision, goReason := worktreeDelDecision(goOut)
	oracleDecision, oracleReason := worktreeDelDecision(oracleOut)
	switch {
	case oracleDecision == "deny" && goDecision != "deny":
		return Verdict{Kind: Miss, Detail: "the oracle denies and the Go side allows"}
	case goDecision == "deny" && oracleDecision != "deny":
		return Verdict{Kind: Extra, Detail: "the Go side denies and the oracle allows"}
	case goDecision == "deny" && oracleDecision == "deny" && goReason != oracleReason:
		return Verdict{Kind: Differ, Detail: "both deny, with different reasons"}
	default:
		return Verdict{Kind: Same}
	}
}

func worktreeDelDecision(value any) (string, string) {
	object, ok := value.(pyjson.Object)
	if !ok {
		return "", ""
	}
	decision, _ := object.Lookup("decision")
	reason, _ := object.Lookup("reason")
	text, _ := decision.(string)
	detail, _ := reason.(string)
	return text, detail
}

// worktreeDelReading is the c2g measure for this target: the input is unreadable when the guard judges it (a PreToolUse call of Bash
// with a cwd and a command in a managed checkout) and the guard's own reading (hook.WorktreeGuardCommandReadable) cannot read the
// command, and the Go side refused it when the guard answered deny.
func worktreeDelReading(input any, env Env, goOut any) (unreadable, refused bool) {
	object, ok := input.(pyjson.Object)
	if !ok {
		return false, false
	}
	if worktreeDelText(object, "event") != "PreToolUse" {
		return false, false
	}
	if tool := worktreeDelText(object, "tool"); tool != "" && tool != "Bash" {
		return false, false
	}
	cwd, command := worktreeDelCwd(object, env), worktreeDelText(object, "command")
	if cwd == "" || command == "" {
		return false, false
	}
	// The case environment decides only whether the cwd is a managed checkout; the command is read the way the guard reads it, with
	// no environment (a variable is unknown), so $HOME/tool is unreadable here as it is to the guard.
	if !hook.WorktreeCwdManaged(cwd, worktreeDelEnv(object, env)) || hook.WorktreeGuardCommandReadable(command, cwd) {
		return false, false
	}
	decision, _ := worktreeDelDecision(goOut)
	return true, decision == "deny"
}
