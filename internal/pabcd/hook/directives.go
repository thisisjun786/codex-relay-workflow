package hook

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	jstext "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// Text and assembly are ported from CXC v0.2.40 hook.ts:212-235,324-575
// (commit 3c1459ac), with the contract's CRW name and CLI substitutions.
// Constants stay unresolved; invocation lookup happens only at emission.

const phaseIDirective = "[crw: INTERVIEW]\n" +
	"Apply this pointer and its owners within exact user limits and permissions. No-delegation means no dispatch.\n" +
	"This also scopes the Mind instructions below. Load $crw:crw-interview for dimensions, questions, loop classification and readiness. Do not implement.\n" +
	"INTERVIEW-GROUND-01: when tracker writes are authorized, `crw pabcd scan record --session <id> --derive --map <questionId>=<dimension> ...`\n" +
	"records known[]/unknown[]; read `.crw/sessions/<id>.json` before the next question. Report unmet actions, not false readiness.\n" +
	"INTERVIEW-RENDER-01: show knowns, the weakest dimension and the answer's impact before the question.\n" +
	"INTERVIEW-INDEPENDENT-01: batch only INDEPENDENT questions; independence governs, not a count."

const phasePDirective = "[crw: PLAN]\n" +
	"Apply this pointer and its owners within exact user limits and permissions. No-delegation means no dispatch.\n" +
	"Load $crw:crw-pabcd for P and C2+ plan-output; $crw:crw-dev selects class and relevant surfaces. No implementation yet.\n" +
	"Formal P, including C2 compact and plan-only P: obtain the configured read-only architect proposal BEFORE the executable plan, send that concrete plan to the SAME architect for reflection BEFORE A, and record the consultation per $crw:crw-pabcd phase-plan/plan-output. The C0/C1 fast path needs none.\n" +
	"Plan-only ends with the plan. Forbidden checks: NOT RUN; naming an artifact grants no write permission."

const phaseADirective = "[crw: AUDIT]\n" +
	"Apply this pointer and its owners within exact user limits and permissions. No-delegation means no dispatch.\n" +
	"Load $crw:crw-dev-code-reviewer for review and $crw:crw-dev for relevant surfaces; authorized PABCD A uses $crw:crw-pabcd's audit owner. Do not build yet.\n" +
	"Authorized dispatch follows the owner's named-skill, same-reviewer and verdict contracts; main synthesizes. Report unmet independent review; inline review is not its proof. Do not bypass gates.\n" +
	"An amendment changing a module-responsibility, data-structure, interface or execution-flow decision needs reflection from the SAME architect before A completes ($crw:crw-pabcd phase-audit); text/test clarification alone does not. The reviewer stays independent."

const phaseBDirective = "[crw: BUILD]\n" +
	"Apply this pointer and its owners within exact user limits and permissions. No-delegation means no dispatch.\n" +
	"Use $crw:crw-dev for class/surfaces; authorized PABCD B uses $crw:crw-pabcd. Implement only authorized scope.\n" +
	"Forbidden checks: NOT RUN; no invented proof."

const phaseCDirective = "[crw: CHECK]\n" +
	"Apply this pointer and its owners within exact user limits and permissions. No-delegation means no dispatch.\n" +
	"Use $crw:crw-dev and $crw:crw-dev-testing; authorized PABCD C uses $crw:crw-pabcd's check owner, including C-RENDER-GROUNDING-01.\n" +
	"No-tests forbids tests, not separately authorized build/typecheck. No-goal/no-FSM restrict creation/mutations, not read-only inspection.\n" +
	"Independent review needs owner applicability and dispatch permission. Report unmet review; inline review is not its proof. Forbidden checks: NOT RUN. No pass or gate bypass without real evidence."

const phaseDDirective = "[crw: DONE]\n" +
	"Apply this pointer and its owners within exact user limits and permissions. No-delegation means no dispatch.\n" +
	"For authorized D closure load $crw:crw-pabcd; report evidence and unmet work, then IDLE. Remaining authorized work follows $crw:crw-loop from disk.\n" +
	"A header or budget/time stop is not completion; never fabricate attestations/receipts."

// QuestionShapeDirective is QUESTION_SHAPE_DIRECTIVE (hook.ts:428-437).
const QuestionShapeDirective = "[crw: INTERVIEW — user question]\n" +
	"Ask via request_user_input only (not an assistant choice fence). Each question must include:\n" +
	"- background: what is unresolved and why it matters,\n" +
	"- where the answer changes the plan,\n" +
	"- 2-3 concrete options (recommendation FIRST),\n" +
	"- one impact/tradeoff sentence per option.\n" +
	"Only the main session asks; subagents never generate or deliver questions.\n" +
	"While a question is pending, refuse or restate unrelated free-form answers."

// AgbrowseSearchDirective is AGBROWSE_SEARCH_DIRECTIVE (hook.ts:439-454).
const AgbrowseSearchDirective = "[crw: SEARCH — agbrowse requested]\n" +
	"Honor the user's agbrowse preference when the required capability is available and authorized.\n" +
	"Load crw-search and the shared dev/references/browser-routing.md policy when available.\n" +
	"agbrowse and Aside are optional tools; inspect the actual CLI/tool schema and session access.\n" +
	"For a known public URL, prefer HTTP proof such as `agbrowse fetch \"<url>\" --json --browser never`.\n" +
	"Discover candidate URLs with available hosted search first. Never use plain `agbrowse search \"<query>\"` as discovery.\n" +
	"For independent extraction or built-UI QA, use a suitable available browser capability.\n" +
	"Prefer suitable Aside for authenticated/judgment-heavy work unless the user specifies another tool.\n" +
	"If a preferred tool is absent, use a capability-preserving alternative and disclose the limitation; respect explicit tool restrictions.\n" +
	"Start a browser only for a diagnosed CDP connection failure and only when the session is task-owned.\n" +
	"HTTP, authentication, and missing-content failures do not justify blindly starting Chrome.\n" +
	"Inspect whether a side effect completed before retrying or switching tools; never assume cookies transfer.\n" +
	"Do not install tools or change accounts without authorization. No equivalent capability means report the gap, not PASS.\n" +
	"Verify inspect -> act -> re-inspect, read captured evidence, and confirm the actual source claim or interaction."

// TriggerAuthorityNote is TRIGGER_AUTHORITY_NOTE (hook.ts:467-474).
const TriggerAuthorityNote = "[crw: PHASE UNCHANGED — TRIGGER-AUTHORITY-01] A lexical phase hint is not execution authority. The phase on disk is unchanged; no-goal/no-FSM restrict creation/mutations, not read-only get_goal or orchestrate status. Do not start orchestration for ordinary work. Preserve adjacency, attestations and ledger checks. Only if a phase transition is authorized, use the crw-pabcd phase-control owner and `crw pabcd orchestrate <I|P|A|B|C|D> --session <id>` — work edges carry --attest."

// PAAttestExample is the shared compact P-to-A example (hook.ts:488-489).
const PAAttestExample = "{\"from\":\"P\",\"to\":\"A\",\"did\":\"...\",\"planUnit\":\"devlog/_plan/YYMMDD_slug\",\"workPhaseId\":\"wp1\"}"

// The Mind text is the composition fragment of minds.ts:66-81; no Mind orchestration is added.
const mindDispatchDirective = "[crw: INTERVIEW — Mind dispatch]\n" +
	"You (the main session) OWN this interview loop: select Minds, dispatch contradiction workers,\n" +
	"triage contradictions, ask the user if needed, edit the plan, update state, and re-question.\n" +
	"The hook only injects directives — it does not coordinate worker returns or plan edits.\n" +
	"Dispatch Minds ONLY from the top-level main session; if you are yourself a subagent, Mind\n" +
	"dispatch is unavailable (no nested orchestration) — fall back to inline reasoning, do not nest.\n" +
	"Each Mind is a read-only lens: it returns contradictions ONLY (never asks/edits/calls/writes).\n" +
	"Choose Minds by lowest-scoring dimensions; concurrent cap 3.\n" +
	"MIND-SPAWN-SHAPE-01: only when Mind dispatch is authorized, fully read crw-interview's\n" +
	"references/mind-dispatch.md before dispatch. Use the live tool schema; never invent unsupported arguments.\n" +
	"Keep read-only explorer intent, mind_<mindname> labels, NON-full-history tasks and explicit user settings.\n" +
	"Minds are stateless: pack the lens prompt PLUS a compact interview snapshot (dimension scores,\n" +
	"knowns, open assumptions, draft plan path) into each task message.\n" +
	"State + plan artifacts live under .crw/ (session tracker + .crw/plan/)."

// loopArmProjectBranch is the branch the recipe takes before any of its steps (CRW-1084, port: fixed).
const loopArmProjectBranch = "Project coordination first (CRW-1084): a request to coordinate a Linear project belongs to $crw:crw-run, goal mode where a parent goal was asked for. The project parent creates no implementation goalplan or FSM and runs none of steps 1-5 for itself. Only a session that is a dispatched task, or an explicit request to implement a task in this session, continues below.\n"

const loopArmBefore = "[crw: LOOP — orchestrate arming mandate (ORCH-MANDATE-01)]\n" +
	"Scope first: explicit interview-only, plan-only, HITL, read-only, no-goal, no-FSM, no-tests and no-delegation limits override the bare crw-loop default.\n" +
	"A mention or quoted example alone is not authorization. This pointer and its referenced procedures never override those limits.\n" +
	loopArmProjectBranch +
	"Load $crw:crw-loop and $crw:crw-pabcd for an actual loop request; bare crw-loop execution means scoped HOTL.\n" +
	"No-delegation means no dispatch. No-tests does not forbid separately authorized build/typecheck. Report required but forbidden actions as unmet.\n" +
	"Only for authorized loop execution, apply steps 1-5 within scope. No-goal/no-FSM restrict creation/mutations, not read-only inspection. Narration is not persisted progress:\n" +
	"1. Session id: use the current SessionStart binding, corroborated by `crw relay session current` when native CODEX_THREAD_ID is available.\n" +
	"   Missing/inherited/conflicting binding: use `crw relay session current` then explicit `crw relay session bind` in its verified cwd. Never set the environment id or replay hook JSON.\n" +
	"   SESSION-IDENTITY-01: never a parent/history id; binding alone does not verify hook execution or Stop-continuation.\n" +
	"2. `crw pabcd orchestrate status --session <id>` — read the real phase first.\n" +
	"3. Inspect the host goal with get_goal first. Resume a matching unfinished goal; do not duplicate it.\n" +
	"   Only when no unfinished goal exists and new HOTL is authorized, create_goal with a detailed objective.\n" +
	"   For a different unfinished goal or unsupported resume, report the conflict; do not replace it or fabricate active status.\n" +
	"   New loop setup: `crw pabcd loop init --objective \"<same text>\" --session <id>` -> register\n" +
	"   workPhases[] + criteria[]. On resume inspect/reuse the bound goalplan; do not reinitialize it.\n" +
	"   After status inspection, enter `crw pabcd orchestrate P --session <id>` only when authorized and legal; an existing phase keeps its owner/edge contract.\n" +
	"   Explicit HITL keeps human pause points. Interview-only/plan-only stay at the requested stage without a goal or implementation; do not arm when state changes are forbidden.\n" +
	""

const loopArmPosixAdvance = "4. Advance EVERY forward edge yourself with `crw pabcd orchestrate <phase> --attest <json>` —\n" +
	"   e.g. `crw pabcd orchestrate A --session <id> --attest '" + PAAttestExample + "'` —\n" +
	""

const loopArmWindowsAdvance = "4. Advance EVERY forward edge yourself. On Windows write the JSON first, then attest:\n" +
	"   `'" + PAAttestExample + "' | Set-Content -Encoding utf8 .crw/attest.json` then\n" +
	"   `crw pabcd orchestrate <phase> --session <id> --attest-file .crw/attest.json` —\n" +
	"   inline --attest cannot survive PowerShell argument parsing (quotes are stripped,\n" +
	"   and escaping them splits the value at its first space).\n" +
	""

const loopArmAfter = "   a phase without its persisted transition + artifact did not happen (ORCH-ARTIFACT-01).\n" +
	"   EVERY attest carries \"from\" and \"to\" naming the edge: they are coerced before any\n" +
	"   gate runs, so omitting them is refused on every edge (ATTEST-SHAPE-01).\n" +
	"   When a goalplan is bound, include the active workPhaseId in every gated attest\n" +
	"   (one work-phase = one full PABCD cycle).\n" +
	"   Bound chat D-close requires workPhaseId as the fixed close target unless every work-phase is already done.\n" +
	"5. After authorized D closes to IDLE with authorized work remaining under an active goal, re-enter\n" +
	"   with `crw pabcd orchestrate P --session <id>` (LOOP-UNIT-CHAIN-01).\n" +
	"HOTL does not grant push, merge, release, deploy or external-message permission. Stop for missing authority.\n" +
	"Preserve guards and real evidence; do not bypass a gate or fabricate an attestation/receipt to satisfy this advice."

// loopScopeParentLine is added to the scope pointer for a session the verified registry read reports as a project parent.
const loopScopeParentLine = "This session is registered as a project parent: it coordinates through $crw:crw-run and starts no loop for itself.\n"

// loopScopeBefore and loopScopeAfter are the scope pointer (CRW-1084, port: fixed) a loop request that names a project gets in
// place of the implementation recipe. It names the owners and the one exception and carries none of the recipe's steps, so a
// project parent that reads it has nothing to run for itself.
const loopScopeBefore = "[crw: LOOP — scope choice (ORCH-MANDATE-01)]\n" +
	"This request names a project or a coordination. Settle the scope before any loop step; this pointer starts no goal, goalplan or FSM.\n"

const loopScopeAfter = "- Coordinating a Linear project (independent children, parallel delivery, verification): $crw:crw-run, with its goal mode only where a parent goal was asked for. The project parent never follows the crw-loop or crw-pabcd procedure: it creates no goal, goalplan or FSM and enters no PABCD phase for itself, and its children run crw-loop.\n" +
	"- Implementing one task in THIS session, when the user says so explicitly (or this session is a dispatched task that owns its goalplan): $crw:crw-loop and $crw:crw-pabcd. Say which task, and the arming steps come with that request.\n" +
	"- Unclear: ask once, or take the smaller scope. A project link or the word project is not a role and does not make this session a parent.\n" +
	"Explicit interview-only, plan-only, HITL, read-only, no-goal, no-FSM, no-tests and no-delegation limits still win. A mention or quoted example alone is not authorization."

const phaseFooterTail = "At the end of your reply, print exactly one status line in the format `IPABCD: <phase> (<LABEL>)`, using the latest verified persisted phase and its matching label for the current SessionStart-bound session and cwd. A later authorized, successful phase transition supersedes this snapshot for reporting. Otherwise retain the latest verified state; a request, lexical hint, narration, or failed transition is not a persisted phase change. This reporting instruction requires no additional tool calls and authorizes no transitions or gate bypasses. D closes to IDLE; a later authorized successful re-entry supersedes that resting state too."

// ActiveWorkPhase is the caller's binding target; ActiveWorkPhaseOpts loads the bound goalplan.
type ActiveWorkPhase struct{ ID, Title string }

// DirectiveOptions supplies the optional B-phase slice (hook.ts:399-411).
type DirectiveOptions struct{ ActiveWorkPhase *ActiveWorkPhase }

// PhaseDirective returns the unresolved phase pointer, naming only B's active slice when supplied.
func PhaseDirective(phase state.Phase, opts *DirectiveOptions) string {
	base := phaseDirectiveBase(phase)
	if phase == state.PhaseB && opts != nil && opts.ActiveWorkPhase != nil && base != "" {
		wp := opts.ActiveWorkPhase
		return base + "\nACTIVE WORK-PHASE: " + wp.ID + " — " + wp.Title + ". This cycle\n" +
			"implements THIS slice only; other work-phases are OUT OF SCOPE until D closes\n" +
			"(LOOP-UNIT-CHAIN-01). Attest gated edges with this workPhaseId."
	}
	return base
}

func phaseDirectiveBase(phase state.Phase) string {
	switch phase {
	case state.PhaseI:
		return phaseIDirective
	case state.PhaseP:
		return phasePDirective
	case state.PhaseA:
		return phaseADirective
	case state.PhaseB:
		return phaseBDirective
	case state.PhaseC:
		return phaseCDirective
	case state.PhaseD:
		return phaseDDirective
	default:
		return ""
	}
}

func stageLabel(phase state.Phase) string {
	switch phase {
	case state.PhaseI:
		return "INTERVIEW"
	case state.PhaseP:
		return "PLAN"
	case state.PhaseA:
		return "AUDIT"
	case state.PhaseB:
		return "BUILD"
	case state.PhaseC:
		return "CHECK"
	case state.PhaseD:
		return "DONE"
	default:
		return string(phase)
	}
}

// BuildStageHeader is the short stage marker (hook.ts:546-549).
func BuildStageHeader(phase state.Phase) string {
	return fmt.Sprintf("[crw — %s: %s]", phase, stageLabel(phase))
}

// PhaseFooter describes the prompt-time snapshot and later persisted transition precedence.
func PhaseFooter(phase state.Phase) string {
	return fmt.Sprintf("Prompt-time persisted snapshot: `IPABCD: %s (%s)`. %s", phase, stageLabel(phase), phaseFooterTail)
}

// WithFooter appends one blank line and the footer, preserving an empty input (hook.ts:573-575).
func WithFooter(directive string, phase state.Phase) string {
	if directive == "" {
		return directive
	}
	return directive + "\n\n" + PhaseFooter(phase)
}

// LoopArmDirective takes a Node platform spelling. Empty selects this host's platform.
// Only win32 uses the PowerShell attest-file instructions (hook.ts:491-532).
func LoopArmDirective(platform string) string {
	if platform == "" {
		platform = runtime.GOOS
		if platform == "windows" {
			platform = "win32"
		}
	}
	advance := loopArmPosixAdvance
	if platform == "win32" {
		advance = loopArmWindowsAdvance
	}
	return loopArmBefore + advance + loopArmAfter
}

// LoopScopeDirective is the short scope pointer of a loop request that names a project (CRW-1084). parent adds the sentence for
// a session the verified registry read reports as a project parent.
func LoopScopeDirective(parent bool) string {
	line := ""
	if parent {
		line = loopScopeParentLine
	}
	return loopScopeBefore + line + loopScopeAfter
}

// InterviewDirective composes the I pointer and Mind text, resolving commands at emission.
func InterviewDirective(env host.LookupEnv) string {
	return ResolveCRWInDirective(phaseIDirective+"\n\n"+mindDispatchDirective, env)
}

// ResolveCRWInDirective resolves only backtick-anchored command prefixes (hook.ts:226-233).
// The input must be directive text whose commands are all backticked, not arbitrary prose.
// The host owner supplies CRW_BIN or the runtime pointer; an error keeps the whole original text.
func ResolveCRWInDirective(directive string, env host.LookupEnv) string {
	if env == nil {
		env = os.LookupEnv
	}
	return resolveDirective(directive, func() (string, error) { return host.Invocation(env) })
}

func resolveDirective(directive string, invocation func() (string, error)) string {
	var out strings.Builder
	rest := directive
	for {
		anchor := strings.Index(rest, "`crw ")
		if anchor < 0 {
			out.WriteString(rest)
			return out.String()
		}
		start := anchor + len("`crw ")
		end := start
		for end < len(rest) {
			r, size := utf8.DecodeRuneInString(rest[end:])
			if r == '`' || jstext.Trim(string(r)) == "" {
				break
			}
			end += size
		}
		if end == start {
			out.WriteString(rest[:start])
			rest = rest[start:]
			continue
		}
		prefix, err := invocation()
		if err != nil {
			return directive
		}
		out.WriteString(rest[:anchor])
		out.WriteString("`" + prefix + " " + rest[start:end])
		rest = rest[end:]
	}
}
