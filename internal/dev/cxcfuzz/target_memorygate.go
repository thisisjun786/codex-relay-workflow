//go:build dev

package cxcfuzz

import (
	"encoding/json"
	"math/rand"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// memorygateTarget is the memory write gate (CRW-703, absorbed from CRW-705): the ported
// hook.HandleMemoryWriteGate against the oracle's handleMemoryWriteGate at CXC v0.2.40
// (plugins/codexclaw/components/pabcd-state/dist/memory-write-gate.js:296-325). The input carries a
// PreToolUse payload and an fs scenario whose tree holds the protected memories root and links
// that lead into it. The answer is the hook envelope; the comparison is the decision and the deny
// reason: the oracle denies and the port allows is a miss (a write the gate must stop), the port
// denies and the oracle allows is extra.
func memorygateTarget() Target {
	return Target{
		Name:     "memorygate",
		Generate: memoryGateGenerate,
		Go:       memoryGateGo,
		Oracle:   Oracle{Command: "node", Shim: shimPath("memorygate"), Root: DefaultOracleRoot},
		Compare:  memoryGateCompare,
		Reading:  memoryGateReading,
	}
}

// memoryGateGo is the Go side: hook.HandleMemoryWriteGate over the payload, with the homes under
// the case's own root. The payload's ROOT placeholder takes that root in its string values and its
// object keys, before the payload is serialized, exactly as the shim substitutes its own
// (testdata/memorygate/shim.mjs substitute(), :19-27). Substituting into the serialized text
// instead corrupted the JSON whenever the root held a quote, a backslash or a line break.
func memoryGateGo(input any, env Env) (any, error) {
	payload, found := field(input, "payload")
	if !found {
		return "", nil
	}
	raw := canonical(substituteRootValue(payload, env.Root))
	return hook.HandleMemoryWriteGate(raw, memoryGateEnvOf(env)), nil
}

// memoryGateEnvOf is the environment the gate reads: the case's own homes, never a real one.
func memoryGateEnvOf(env Env) host.LookupEnv {
	vars := map[string]string{"HOME": env.Home, "CODEX_HOME": env.CodexHome, "CRW_HOME": env.CrwHome, "TMPDIR": env.TmpDir}
	return func(key string) (string, bool) { value, set := vars[key]; return value, set }
}

// memoryGateCompare judges the two envelopes by decision first, then by the deny reason.
func memoryGateCompare(goOut, oracleOut any) Verdict {
	goDecision, goReason := memoryGateAnswer(goOut)
	oracleDecision, oracleReason := memoryGateAnswer(oracleOut)
	switch {
	case goDecision == "?" || oracleDecision == "?":
		// A side that answered something the gate never answers - a worker failure, an error reply
		// - is not an allow: it is reported as a difference so it is never mistaken for agreement.
		return Verdict{Kind: Differ, Detail: "the oracle answers " + memoryGateClip(oracleDecision+" "+oracleReason) + "; the port " + memoryGateClip(goDecision+" "+goReason)}
	case oracleDecision == "deny" && goDecision != "deny":
		return Verdict{Kind: Miss, Detail: "the oracle denies a write the port allows: " + memoryGateClip(oracleReason)}
	case goDecision == "deny" && oracleDecision != "deny":
		return Verdict{Kind: Extra, Detail: "the port denies a write the oracle allows: " + memoryGateClip(goReason)}
	case goDecision != oracleDecision:
		return Verdict{Kind: Differ, Detail: "the oracle answers " + oracleDecision + ", the port " + goDecision}
	case memoryGateNormalise(oracleReason) != memoryGateNormalise(goReason):
		return Verdict{Kind: Differ, Detail: "the deny reasons differ"}
	default:
		return Verdict{Kind: Same}
	}
}

// memoryGateAnswer reads one side's decision and deny reason. An empty answer is an allow; a value
// that is not an envelope is its own decision, so a worker failure never reads as an allow.
func memoryGateAnswer(value any) (decision, reason string) {
	text, ok := value.(string)
	if !ok {
		return "?", canonical(value)
	}
	if text == "" {
		return "allow", ""
	}
	var envelope struct {
		Output struct {
			Decision string `json:"permissionDecision"`
			Reason   string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal([]byte(text), &envelope) != nil {
		return "?", text
	}
	return envelope.Output.Decision, envelope.Output.Reason
}

// memoryGateClip bounds a detail so a long deny reason does not fill a divergence file.
func memoryGateClip(reason string) string {
	if len(reason) > 200 {
		return reason[:200]
	}
	return reason
}

// memoryGateNormalise is the name substitution the corpus replayer applies to an oracle answer
// (contract/schema/cxc/name-substitution.json): the port renames CXC's brand text and its CLI
// verbs, so the oracle's deny reason keeps the old spelling. Only the fixed template phrases are
// rewritten, never a whole-word brand token: the reason also interpolates the destination, the
// session id and the cwd, and a destination may legitimately hold the token (a case tree can), so a
// global replacement would rewrite a path and could mask a real difference between the two answers.
// A decision, a path and every other byte stay as they are.
func memoryGateNormalise(text string) string {
	for _, phrase := range [][2]string{
		{"[codexclaw MEMORY-WRITE-GATE]", "[crw MEMORY-WRITE-GATE]"},
		{"Memory notes outlive codexclaw and reach", "Memory notes outlive crw and reach"},
		{"`cxc memory allow-write", "`crw pabcd memory allow-write"},
	} {
		text = strings.ReplaceAll(text, phrase[0], phrase[1])
	}
	return text
}

// memoryGateGenerateToolNames are the hook-facing names of the memory write tool, and the edit and
// shell tools that reach the same bytes. They mirror the oracle's MEMORY_WRITE_TOOL_NAMES,
// EDIT_TOOLS and SHELL_TOOLS, plus two tools that touch nothing.
func memoryGateGenerateToolNames() []string {
	return []string{
		"memoriesadd_ad_hoc_note", "memories.add_ad_hoc_note", "memories_add_ad_hoc_note", "add_ad_hoc_note",
		"Bash", "exec_command", "shell", "local_shell", "apply_patch", "Write", "Edit", "Read", "Grep",
	}
}

// memoryGateGenerate builds one PreToolUse payload over a scenario whose tree holds the memories
// root, a sibling memories-backup, and links into the root: aliases whose names carry a line break, a
// carriage return, a space or a quote, and link chains one to forty-five deep. Link targets mix the
// relative form with the case root's absolute form (the ROOT placeholder the scenario builder
// substitutes), so both forms the issue asks for reach the campaigns. Destinations mix the root
// itself, a path inside it, a sibling, a link, a home form and a relative form, and the command
// grammar is the shellwrite generator's own.
func memoryGateGenerate(rng *rand.Rand, size int) any {
	memories := rootPlaceholder + "/codex-home/memories"
	fs := []any{
		memoryGateDir("codex-home/memories"), memoryGateDir("codex-home/memories-backup"), memoryGateDir("work"),
	}
	dests := memoryGateDests()
	for i, alias := range memoryGateAliases() {
		fs = append(fs, memoryGateLink("work/"+alias, memoryGateTarget(i, "../codex-home/memories", memories)))
	}
	if rng.Intn(2) == 0 {
		// The home-prefixed destinations must resolve under the case's own HOME (${ROOT}/home), so the
		// link they follow stands there; the work/link entry gives the absolute and relative forms a
		// second link to reach.
		fs = append(fs, memoryGateLink("home/link", "../codex-home/memories"), memoryGateLink("work/link", memories))
		dests = append(dests, memoryGateLinkDests()...)
	}
	if rng.Intn(2) == 0 {
		depth := 1 + rng.Intn(45)
		for i := 0; i < depth; i++ {
			target := "c" + memoryGateInt(i+1)
			if i == depth-1 {
				target = memoryGateTarget(i, "../codex-home/memories", memories)
			}
			fs = append(fs, memoryGateLink("work/c"+memoryGateInt(i), target))
		}
		dests = append(dests, memoryGateChainDests()...)
	}
	return pyjson.Object{{Key: "fs", Value: fs}, {Key: "payload", Value: memoryGatePayload(rng, dests, size)}}
}

// memoryGateAliases are the link names the scenario builds: plain, and names carrying the characters a
// path reader trips on.
func memoryGateAliases() []string {
	return []string{"alias", "alias\n", "alias\r", "alias ", "alias'", "alias\""}
}

// memoryGateDests is the destination pool every generated payload draws from, so a test can walk the pool
// itself instead of copying entries by hand. The link-dependent destinations are NOT here: they are added
// by memoryGateLinkDests and memoryGateChainDests only in the branch that builds the links they follow,
// because a destination whose link the tree does not hold is not a memory write at all and would change
// what the campaign compares.
func memoryGateDests() []string {
	memories := rootPlaceholder + "/codex-home/memories"
	backup := rootPlaceholder + "/codex-home/memories-backup"
	dests := []string{
		memories + "/n.md", memories, memories + "/extensions/ad_hoc/notes/x.md",
		backup + "/n.md", "../codex-home/memories/n.md", memories + "/a b.md", memories + "/a'b.md",
		// A destination holding a brace pair, so the doubled-brace f literal the shell-write program
		// builder emits for a brace reaches a generated command (CRW-908).
		memories + "/{x}.md",
	}
	for _, alias := range memoryGateAliases() {
		dests = append(dests, alias+"/x.md", "./"+alias+"/x.md")
	}
	return dests
}

// memoryGateLinkDests are the destinations that reach the memories root through the home and work links,
// added to a payload only in the branch that builds those links.
func memoryGateLinkDests() []string {
	return []string{"~/link/x.md", "$HOME/link/x.md", "$CODEX_HOME/memories/n.md", rootPlaceholder + "/work/link/x.md", "work/link/x.md"}
}

// memoryGateChainDests are the destinations that reach the memories root through the link chain, added to
// a payload only in the branch that builds the chain.
func memoryGateChainDests() []string {
	return []string{"c0/x.md"}
}

// memoryGateTarget alternates a link target between the relative form and the case root's absolute
// form, so a generated tree carries both. The choice is by index rather than by the rng, so a seed
// draws both forms whatever the rest of the generator does.
func memoryGateTarget(index int, relative, absolute string) string {
	if index%2 == 1 {
		return absolute
	}
	return relative
}

func memoryGateInt(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func memoryGateDir(path string) pyjson.Object {
	return pyjson.Object{{Key: "path", Value: path}, {Key: "kind", Value: "dir"}}
}

func memoryGateLink(path, target string) pyjson.Object {
	return pyjson.Object{{Key: "path", Value: path}, {Key: "kind", Value: "symlink"}, {Key: "target", Value: target}}
}

// memoryGatePayload builds the PreToolUse envelope. The tool is a memory tool, an edit tool or a
// shell tool; the destination reaches the gate through the field that tool uses, and a malformed
// shape (an array where a command belongs, a string where the record belongs) is generated too.
func memoryGatePayload(rng *rand.Rand, dests []string, size int) pyjson.Object {
	payload := pyjson.Object{
		{Key: "hook_event_name", Value: "PreToolUse"},
		{Key: "session_id", Value: "s1"},
		{Key: "turn_id", Value: "t1"},
		{Key: "cwd", Value: rootPlaceholder + "/work"},
	}
	names := memoryGateGenerateToolNames()
	tool := names[rng.Intn(len(names))]
	payload = payload.Set("tool_name", tool)
	dest := dests[rng.Intn(len(dests))]
	switch tool {
	case "memoriesadd_ad_hoc_note", "memories.add_ad_hoc_note", "memories_add_ad_hoc_note", "add_ad_hoc_note":
		if rng.Intn(4) == 0 {
			payload = payload.Set("tool_input", pyjson.Object{})
			break
		}
		payload = payload.Set("tool_input", pyjson.Object{{Key: "filename", Value: dest}, {Key: "note", Value: "x"}})
	case "apply_patch":
		switch rng.Intn(3) {
		case 0:
			patch := "*** Begin Patch\n*** Add File: " + dest + "\n+hi\n*** End Patch\n"
			payload = payload.Set("tool_input", pyjson.Object{{Key: "command", Value: patch}})
		case 1:
			patch := "--- a/x\n+++ " + dest + "\n+hi\n"
			payload = payload.Set("tool_input", pyjson.Object{{Key: "command", Value: patch}})
		default:
			patch := "*** Begin Patch\n*** Update File: /w/a.md\n*** Move to: " + dest + "\n*** End Patch\n"
			payload = payload.Set("tool_input", pyjson.Object{{Key: "command", Value: patch}})
		}
	case "Write", "Edit":
		payload = payload.Set("tool_input", pyjson.Object{{Key: "file_path", Value: dest}, {Key: "content", Value: "x"}})
	case "Bash", "exec_command", "shell", "local_shell":
		if rng.Intn(8) == 0 {
			payload = payload.Set("tool_input", pyjson.Object{{Key: "command", Value: []any{dest}}})
			break
		}
		command := shellWriteCommandWith(rng, dests, size)
		payload = payload.Set("tool_input", pyjson.Object{{Key: "command", Value: command}})
	default:
		if rng.Intn(2) == 0 {
			payload = payload.Set("tool_input", dest)
		} else {
			payload = payload.Set("tool_input", pyjson.Object{{Key: "file_path", Value: dest}})
		}
	}
	return payload
}

// memoryGateReading is the c2g measure for this target: the input is unreadable when the payload is a PreToolUse call of a shell
// tool whose command one of the gate's own readings (hook.MemoryGateCommandReadable) cannot read, and the Go side refused it when
// the gate answered deny.
func memoryGateReading(input any, env Env, goOut any) (unreadable, refused bool) {
	payload, found := field(input, "payload")
	if !found {
		return false, false
	}
	payload = substituteRootValue(payload, env.Root)
	if event, _ := field(payload, "hook_event_name"); event != "PreToolUse" {
		return false, false
	}
	tool, _ := field(payload, "tool_name")
	switch tool {
	case "Bash", "shell", "exec_command", "local_shell":
	default:
		return false, false
	}
	toolInput, _ := field(payload, "tool_input")
	value, _ := field(toolInput, "command")
	command, isText := value.(string)
	if !isText {
		return false, false
	}
	cwd := ""
	if value, found := field(payload, "cwd"); found {
		cwd, _ = value.(string)
	}
	if hook.MemoryGateCommandReadable(command, cwd, memoryGateEnvOf(env)) {
		return false, false
	}
	decision, _ := memoryGateAnswer(goOut)
	return true, decision == "deny"
}
