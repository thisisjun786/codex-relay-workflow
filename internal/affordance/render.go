package affordance

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"regexp"
	"strconv"
	"strings"
)

// Texts port map-affordance.ts:103-231 after the canonical name substitution.
const map40Text = "[crw] This workspace has SIZE source files. A ranked structure map is available on demand: run `crw map <dir>` (tree-sitter symbols + PageRank) to see which files own which symbols BEFORE deep rg dives into unfamiliar territory. It is a stateless one-shot tool — use it when you need the shape of code you do not yet know. Keep rg for byte/text search, and use ast-grep (skill: $crw-ast-grep) for syntax-shape search and deterministic codemods."
const skillText = "[crw] External skill catalogs are searchable on demand. Priority: jaw (cli-jaw-skills, 1st-class, default) > clawhub (2nd) > hermes (3rd, sparse). When a task needs a capability you do not have loaded, browse `dev/references/skill-catalog.md` for the full jaw catalog first, or run `crw skill search <query>` then `crw skill show <id>` to load it (adapter preamble applies; crw-dev discipline wins on conflict)."
const kwriteText = "[crw] When writing Korean prose for the user (docs, answers, announcements), keep it human: no translationese (~에 대해/~를 통해/~함으로써), no AI idioms (시사하는 바가 크다/결론적으로/기대된다 endings), no 첫째/둘째 enumeration, one consistent register throughout. For explicit 윤문/polish requests or long-form Korean output, load the $crw-kwrite skill for the full revision protocol."
const bindingText = "[crw] This session's id is `SESSION`. Every mutating `crw pabcd orchestrate` command (I/P/A/B/C/D/reset) MUST pass `--session SESSION` — the implicit latest-session fallback is disabled for writes, which prevents ACCIDENTAL implicit-fallback collisions between concurrent/forked sessions. IDENTITY RULE: use the MOST RECENT SessionStart binding line, never a parent/history id. With native CODEX_THREAD_ID, verify via `crw relay session current` before mutation. Missing/inherited/conflicting binding: use `crw relay session current`, then `crw relay session bind` in its verified cwd. Never set the environment id. Binding does not verify hooks or arm Stop-continuation."
const loopText = "[crw] Loop contract: for actual loop work load $crw:crw-loop + $crw:crw-pabcd. Bare crw-loop means scoped HOTL; a mention alone grants no authority. Exact user limits and separately allowed actions scope this pointer and its owners. No-delegation means no dispatch. Read-only inspection remains allowed under no-goal/no-FSM; for actual loop work inspect `crw pabcd orchestrate status --session <your id>` first. No-tests does not forbid an explicitly allowed build. One work-phase = one full PABCD cycle. No extra external permissions; do not bypass guards or invent evidence."
const stackText = "[crw] For PR work or dependent branches, read $crw:crw-dev references/stacked-prs.md (DEV-STACK-06/07/08). Use ordinary PRs/manual chains by default. Do not suggest or create GitHub native stacks unless the user clearly and strongly requests them for this task. Inspect existing membership and CI separately; a parent base or Can Stack banner is not opt-in. Parallel branch/PR lanes: one Codex task each, not subagents (same checkout); the lane request authorizes them. Per-PR CI is expected. This is guidance, not authorization to register, restack, cancel CI, or merge."
const backgroundText = "[crw] Long-running or collision-risky commands (dev servers, builds, test suites, 5min+ probes) SHOULD use managed background execution: `exec_command` with short `yield_time_ms` → get `session_id` → end turn or continue other work → poll later with `write_stdin` (empty chars = poll, no typing). Do NOT block the turn inline for commands that might outlive compaction or conflict with parallel work. CLI: `/ps` lists active background terminals; `/stop` terminates all in the current session. A session_id is NOT an OS PID — it is a Codex-managed execution handle."
const questionsText = "[crw] User questions: main agents may leave useful questions during work, including active goals. Outside Interview, prefer exposed and host-allowed `request_user_input_async`; do not expect replies or wait. Continue authorized work with reasonable assumptions, incorporate later replies, and ask distinct useful questions without reminders. Interview uses `request_user_input` only; see $crw:crw-interview. Absent tools stay absent; silence grants no approval. Subagents send question candidates to main. Details: $crw:crw-dev references/async-questions.md."

func invocation(env host.LookupEnv) string {
	inv, err := host.Invocation(env)
	if err != nil {
		return "crw"
	}
	return inv
}

// ResolveCRWCommands rewrites command code spans only, honoring the invocation seam
// at render time. Skill names, chat commands and noun phrases remain untouched.
func ResolveCRWCommands(s string, env host.LookupEnv) string {
	// JavaScript \s also contains these Unicode spaces; RE2's \s is ASCII only.
	re := regexp.MustCompile("\x60crw ([^\\s\\x{000b}\\x{00a0}\\x{1680}\\x{2000}-\\x{200a}\\x{2028}\\x{2029}\\x{202f}\\x{205f}\\x{3000}\\x{feff}\x60]+)")
	return re.ReplaceAllStringFunc(s, func(match string) string {
		verb := strings.TrimPrefix(match, "\x60crw ")
		switch verb {
		case "orchestrate":
			verb = "pabcd orchestrate"
		case "session":
			verb = "relay session"
		}
		return "\x60" + invocation(env) + " " + verb
	})
}

func RenderMapAffordance(count int, env host.LookupEnv) string {
	size := strconv.Itoa(count)
	if count >= CountCap {
		size = strconv.Itoa(CountCap) + "+"
	}
	return ResolveCRWCommands(strings.ReplaceAll(map40Text, "SIZE", size), env)
}
func RenderSkillSearchAffordance(env host.LookupEnv) string {
	return ResolveCRWCommands(skillText, env)
}
func RenderSessionBinding(id string, env host.LookupEnv) string {
	if !validSessionID(id) {
		return ""
	}
	return ResolveCRWCommands(strings.ReplaceAll(bindingText, "SESSION", id), env)
}

// Native UUIDs and supported recorder/session keys are ASCII tokens. Reject
// invalid identities intact, rather than sanitize or truncate into another key.
func validSessionID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i, c := range id {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		if i > 0 && (c == '-' || c == '_') {
			continue
		}
		return false
	}
	return true
}
func RenderLoopAffordance(env host.LookupEnv) string { return ResolveCRWCommands(loopText, env) }
func RenderKwriteAffordance() string                 { return kwriteText }
func RenderStackedPrAffordance() string              { return stackText }
func RenderBackgroundTerminalAffordance() string     { return backgroundText }
func RenderQuestionAffordance() string               { return questionsText }
