// prompt_submit.go holds the leading section of handleUserPromptSubmit, CXC v0.2.40
// pabcd-state/src/hook.ts:656-754 (commit 3c1459ac), up to the `if (trigger) {` branch the successor
// unit continues. The registration it answers is the harness leg row
// user-prompt-submit-checking-pabcd-trigger; harness parses the payload as cli.ts does (parse.ts)
// and hands a handler what the parse produced, so the event-name check and the required string
// fields are already made when this runs, exactly as the oracle splits parse.ts from the handler.
//
// What the range decides, in the oracle's order: the memory-write request marker, written before
// every early return below (MEMORY-WRITE-GATE-01); the pabcd-off guard; the Stop-budget turn stamp;
// the already-injected-turn guard; the chat orchestrate command seam, whose handler is a separate
// unit's; the interview entry decision; the goal-mode Interview firewall; and the agbrowse and
// loop-arm branches. Texts are frozen byte for byte after contract/schema/cxc/name-substitution.json,
// and a backticked command is resolved at emission, never in a constant.
//
// Both state writes differ from the oracle in the way the memory gate, the idle-edit counter and
// handlePostCompact already differ: the oracle reads the session state, changes a field and writes
// the whole state back with no lock, so an update a participating writer lands between the read and
// the write is lost, and the write-back rebuilds the state from the reader's normalised value, so a
// stored record the reader cannot keep is lost with it. Each write here re-reads inside the session
// lock and refuses a rewrite the reader would not keep whole (docs/port-cxc/known-defects.md).
//
// The handler answers the context to hand the model, not the envelope: harness.ContextOutput wraps
// it (hook.ts:583-597 buildContextOutput), which is where the CRLF normalisation, the trim and the
// 32,000-unit cap live. This file has no package-level initializer and needs no Node at run time.
package hook

import (
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/fsm"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/projectcfg"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// promptSubmitMaxInjectedTurns is MAX_INJECTED_TURNS (hook.ts:578): the cap on the session state's
// injectedTurns list, so a long session's state file cannot grow without bound (audit blocker #2).
const promptSubmitMaxInjectedTurns = 50

// PromptSubmitPayload is what the harness hands PromptSubmitHandle: the UserPromptSubmit fields
// handleUserPromptSubmit reads. TurnID is the payload's turn_id, or empty when it is absent or not a
// string (the oracle's `?? ""`). PabcdEnabled is the caller's options.pabcdEnabled, which cli.ts
// always supplies from readPabcdEnabled of the payload's cwd; false is the oracle's `=== false`.
type PromptSubmitPayload struct {
	Cwd, SessionID, Prompt, TurnID string
	PabcdEnabled                   bool
}

// PromptSubmitHandle is the leading section of handleUserPromptSubmit (hook.ts:656-754). It returns
// the context to inject, or "" for every path that injects nothing.
func PromptSubmitHandle(p PromptSubmitPayload, platform string, env host.LookupEnv) string {
	return promptSubmitHandle(p, platform, env, state.WithSessionLock)
}

// promptSubmitHandle takes the session lock as an argument so that a test can land a participating
// writer's update before the handler's own read, the way the oracle's unlocked read would miss it.
func promptSubmitHandle(p PromptSubmitPayload, platform string, env host.LookupEnv, lock func(cwd, sessionID string, fn func() error) error) string {
	if env == nil {
		env = os.LookupEnv
	}
	turn := p.TurnID

	// MEMORY-WRITE-GATE-01 (260909 wp1-A): record the remember request BEFORE the turn guard and
	// before every early return below. PreToolUse carries no prompt (codex-rs
	// hooks/src/schema.rs:278-296), so this write is the only place the gate's evidence can come
	// from - and the paths that return early here (an already-injected turn, a suppressed interview,
	// a silent un-armed session) are ordinary prompts that may still ask to remember something. Same
	// placement rule as loopArmSeen below. Directive-free: the marker is bookkeeping, so nothing is
	// injected into the model.
	//
	// It also runs BEFORE the state snapshot every later branch spreads. Writing it afterwards would
	// land the marker on disk and then have the next state write overwrite it with the pre-marker
	// snapshot. A marker that cannot be persisted degrades to a deny the user can lift with
	// `crw recall memory allow-write`; it must never break prompt handling, so a failed or
	// refused write is dropped as the oracle's catch drops one.
	if DetectMemoryWriteRequest(p.Prompt) {
		_ = lock(p.Cwd, p.SessionID, func() error {
			fresh, unreadable := state.ReadStateStrict(p.Cwd, p.SessionID)
			if unreadable || !promptSubmitRewritable(p.Cwd, p.SessionID, fresh) {
				return nil
			}
			fresh.MemoryWriteRequested = true
			fresh.MemoryWriteTurn = promptSubmitTurn(turn)
			return state.WriteState(p.Cwd, fresh)
		})
	}
	if !p.PabcdEnabled {
		return ""
	}
	current := state.ReadState(p.Cwd, p.SessionID)
	if turn != "" && promptSubmitStateExists(p.Cwd, p.SessionID) && (current.StopBlockTurnID == nil || *current.StopBlockTurnID != turn) {
		_ = lock(p.Cwd, p.SessionID, func() error {
			fresh, unreadable := state.ReadStateStrict(p.Cwd, p.SessionID)
			if unreadable || (fresh.StopBlockTurnID != nil && *fresh.StopBlockTurnID == turn) {
				return nil
			}
			if !promptSubmitRewritable(p.Cwd, p.SessionID, fresh) {
				return nil
			}
			fresh.StopBlockTotal = 0
			fresh.StopBlockTurnID = &turn
			fresh.StopBlockCapNotified = false
			current = fresh
			return state.WriteState(p.Cwd, fresh)
		})
	}
	if turn != "" && slices.Contains(current.InjectedTurns, turn) {
		return ""
	}

	// L3b: parser-first AUTHORITATIVE path. An explicit, line-anchored `orchestrate <verb>` command
	// actually moves the FSM (the missing wire). This is the HUMAN (chat) source -> free-pass: forward
	// edges advance without --attest. The loose detectTrigger heuristic below runs ONLY when this
	// returns null.
	if command := fsm.ParseOrchestrateCommand(p.Prompt); command != nil {
		if out, handled := promptSubmitOrchestrateCommand(p, current, turn, command); handled {
			return out
		}
		// not handled => fall through to the loose path (e.g. suppressed interview).
	}

	rawTrigger, hasTrigger := DetectTrigger(p.Prompt)

	// 260829 config-autopilot wp4: a plan request may OPEN with the interview instead of requiring
	// the word "interview". Advisory only - the phase below is unchanged, so this cannot wedge a
	// session behind the I->P gate. Only the P trigger is eligible (A/B/C stay non-Interview hints
	// under TRIGGER-AUTHORITY-01), and the goal-active lookup stays behind that check so an ordinary
	// prompt opens no sqlite.
	policy := projectcfg.DefaultPolicy
	goalSuppresses := false
	if hasTrigger && rawTrigger == state.PhaseP {
		policy = projectcfg.ReadPolicy(p.Cwd)
		goalSuppresses = host.SuppressesInterview(sessionHookGoalStatus(p.SessionID, env))
	}
	entry := projectcfg.DecideEntry(projectcfg.EntryInput{
		Trigger:             string(rawTrigger),
		Policy:              policy,
		OrchestrationActive: current.OrchestrationActive,
		GoalSuppresses:      goalSuppresses,
	})
	trigger := entry.Phase

	// L11: in active goal mode, the Interview (I) phase is suppressed - do not inject the I directive
	// and do not create/update interview state (HOTL boundary). Other phase triggers (P/A/B/C) still
	// work; goal mode runs PABCD, just never reopens I.
	if trigger == "I" && host.SuppressesInterview(sessionHookGoalStatus(p.SessionID, env)) {
		return ""
	}

	agbrowseRequested := DetectAgbrowseSearchRequest(p.Prompt)
	loopArmRequested := DetectLoopArmRequest(p.Prompt)

	// TRIGGER-AUTHORITY-01 (040): the loop-arm branch is evaluated BEFORE the trigger branch.
	// "pabcd 여러 번 돌려서 구현해" reads as both a B trigger and a loop request, and the trigger
	// branch used to return first - so the prompt that most clearly asks for a loop got a BUILD
	// directive and never saw the arming mandate. Only the un-armed loop-arm case is promoted; the
	// agbrowse-only case keeps its original position so a plain search request is unaffected.
	if !current.OrchestrationActive && loopArmRequested {
		// 260714 wp3 (audit decision a): persist loopArmSeen OUTSIDE the turn guard - a turnless
		// payload must not lose the flag; injectedTurns stays turn-guarded.
		_ = lock(p.Cwd, p.SessionID, func() error {
			fresh, unreadable := state.ReadStateStrict(p.Cwd, p.SessionID)
			if unreadable || !promptSubmitRewritable(p.Cwd, p.SessionID, fresh) {
				return nil
			}
			fresh.LoopArmSeen = true
			if turn != "" {
				fresh.InjectedTurns = promptSubmitAppendTurn(fresh.InjectedTurns, turn)
			}
			return state.WriteState(p.Cwd, fresh)
		})
		parts := []string{ResolveCRWInDirective(LoopArmDirective(platform), env)}
		if agbrowseRequested {
			parts = append(parts, AgbrowseSearchDirective)
		}
		return strings.Join(parts, "\n\n")
	}

	// The oracle continues at hook.ts:755 with the trigger branch, the agbrowse-only branch and the
	// passive pipeline; that remainder belongs to the successor unit of the same port.
	return ""
}

// promptSubmitTurn is the oracle's `turn === "" ? null : turn` for the marker's memoryWriteTurn.
func promptSubmitTurn(turn string) *string {
	if turn == "" {
		return nil
	}
	return &turn
}

// promptSubmitStateExists is the oracle's existsSync(statePath(...)): the path, of any type, is there.
func promptSubmitStateExists(cwd, sessionID string) bool {
	_, err := os.Stat(state.StatePath(cwd, sessionID))
	return err == nil
}

// promptSubmitRewritable says whether writing next back over the session file would keep every record the file stores: each
// stored unverified subagent must come back as it was stored, a legacy D-close marker would lose its distinction, and an
// interview tracker longer than the reader keeps would lose its oldest entries (sessionHookStateRewritable). A file that is
// not there yet stores nothing, so a fresh state loses nothing; a file that cannot be read is refused.
func promptSubmitRewritable(cwd, sessionID string, next state.State) bool {
	raw, err := os.ReadFile(state.StatePath(cwd, sessionID))
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	return err == nil && sessionHookStateRewritable(raw, next)
}

// promptSubmitAppendTurn is appendTurn (hook.ts:577-581): the turn is appended and the list is cut to
// its last promptSubmitMaxInjectedTurns entries. The result is a new slice, as the oracle's
// `[...turns, turn]` is, so a caller that still holds the previous list is unaffected.
func promptSubmitAppendTurn(turns []string, turn string) []string {
	next := make([]string, 0, len(turns)+1)
	next = append(next, turns...)
	next = append(next, turn)
	if len(next) > promptSubmitMaxInjectedTurns {
		next = next[len(next)-promptSubmitMaxInjectedTurns:]
	}
	return next
}

// promptSubmitOrchestrateCommand is the seam for handleOrchestrateCommand (hook.ts:860+), the chat
// command handler of the L3b free-pass path. That handler belongs to the issue that ports the chat
// orchestrate command (CRW-385), so this unit only parses the command and leaves the seam: it reports
// whether the command was handled and, when it was, the context to inject. Unhandled means control
// falls through to the loose path, exactly as the oracle's `null` return does, which is why the
// command fixtures of the corpus stay pending until that unit lands.
func promptSubmitOrchestrateCommand(p PromptSubmitPayload, current state.State, turn string, command *fsm.OrchestrateCommand) (string, bool) {
	_, _, _ = p, current, turn
	return "", false
}
