package cli

import (
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// workDiagnosticTemplate renders command hints in a fixed template at the presentation
// boundary. It is only called with templates/constants owned by the calling producer —
// never a completed reason, an error, an identifier, or a joined warning list: inserted
// data (session ids, paths, plan units) keeps every byte (CRW-1627).
func workDiagnosticTemplate(template string) string {
	return strings.NewReplacer(
		"crw pabcd orchestrate", "crw work state",
		"crw pabcd loop", "crw work continue",
		"crw pabcd plan", "crw work outline",
		"crw pabcd review-round", "crw work assessment",
		"crw pabcd", "crw work",
		"IPABCD", "task work",
		"PABCD", "task work",
	).Replace(template)
}

// workDiagnosticSuffix re-renders a fixed trailing template owned by the calling producer.
// Only that literal suffix is rendered; the preceding opaque bytes stay untouched. A reason
// that does not end with the literal passes through unchanged.
func workDiagnosticSuffix(reason, legacySuffix string) string {
	if prefix, ok := strings.CutSuffix(reason, legacySuffix); ok {
		return prefix + workDiagnosticTemplate(legacySuffix)
	}
	return reason
}

// workUnbindDiagnostic matches the two refusal templates the unbind gate formats with the
// session's own values, and re-renders the template instead of the completed message.
func workUnbindDiagnostic(reason string, cur state.State) string {
	const active = "unbind: a PABCD cycle is in flight at phase %s, so the session's goalplan stays bound. Close the cycle (advance to D) or run `crw pabcd orchestrate reset --session %s`, then unbind again (reset closes the cycle and keeps the slug)."
	const pending = "unbind: a check or a D-close is still pending on this session, so the session's goalplan stays bound. Run `crw pabcd orchestrate reset --session %s`, then unbind again."
	if reason == fmt.Sprintf(active, cur.Phase, cur.SessionID) {
		return fmt.Sprintf(workDiagnosticTemplate(active), cur.Phase, cur.SessionID)
	}
	if reason == fmt.Sprintf(pending, cur.SessionID) {
		return fmt.Sprintf(workDiagnosticTemplate(pending), cur.SessionID)
	}
	return reason
}

// workPlanDiagnostic re-renders the fixed guidance the plan-gate appends after a planUnit the
// caller gave. The unit itself stays opaque.
func workPlanDiagnostic(reason string) string {
	for _, suffix := range []string{
		"Scaffold one with `crw pabcd plan init <slug>` if missing, write the docs, then re-attest. " +
			"Put the JSON in a file and pass --attest-file <path> (required on Windows, where " +
			"inline JSON cannot survive shell argument parsing): " +
			`{"from":"P","to":"A","did":"...","planUnit":"devlog/_plan/..."}`,
		" does not exist (or is not a directory). Create it with `crw pabcd plan init <slug>` and write the plan docs before P -> A.",
		" resolves outside the working directory (symlinks followed). A plan unit must live inside the workspace: create it there with `crw pabcd plan init <slug>` and write the plan docs before P -> A.",
	} {
		if strings.HasSuffix(reason, suffix) {
			return workDiagnosticSuffix(reason, suffix)
		}
	}
	return reason
}

// workPhaseDiagnostic re-renders the fixed workPhaseId guidance after the opaque check reason.
func workPhaseDiagnostic(reason string) string {
	return workDiagnosticSuffix(reason, `); pass "workPhaseId" in the attest. One work-phase = one full PABCD cycle (LOOP-UNIT-CHAIN-01).`)
}

// workCheckDiagnostic re-renders the check-cycle guidance: the whole old-cycle template, or
// the fixed receipt-production suffix after an opaque reason.
func workCheckDiagnostic(reason string) string {
	const oldCycle = "this check cycle predates CHECK-BINDING-01, so no receipt can bind to it. Step back with `crw pabcd orchestrate B` and re-enter `crw pabcd orchestrate C` to mint a binding."
	if reason == oldCycle {
		return workDiagnosticTemplate(oldCycle)
	}
	return workDiagnosticSuffix(reason, " (CHECK-BINDING-01). Produce one with: crw pabcd receipt test --session <id> -- <command>, then pass its path as testReceiptPath.")
}

// workTransitionDiagnostic re-renders the fixed receipt-hint suffix after an opaque reason.
func workTransitionDiagnostic(reason string) string {
	return workDiagnosticSuffix(reason, "C -> D on a goalplan-bound session ALSO requires \"testReceiptPath\" (CHECK-BINDING-01), produced by `crw pabcd receipt test -- <command>`. Supplying it now avoids another round trip.")
}

// workInterviewDiagnostics re-renders each warning's fixed trailing guidance, one warning at
// a time — the map never runs over a joined list.
func workInterviewDiagnostics(warnings []string) []string {
	const suffix = " reached \"high\" without an answered question in the interview ledger — " +
		"ask and record one per dimension (`crw pabcd scan record --derive --map <questionId>=<dimension>`), " +
		"or assert the level deliberately"
	out := make([]string, len(warnings))
	for i, warning := range warnings {
		out[i] = workDiagnosticSuffix(warning, suffix)
	}
	return out
}

// workSourceDiagnostic re-renders the no-resolvable-source template when the gate's reason is
// exactly it; any other (dynamic) source error passes through unchanged.
func workSourceDiagnostic(reason string) string {
	template := strings.Join([]string{
		"this workspace has no resolvable git source identity, and a goalplan-bound",
		"cycle cannot be closed without one: C -> D requires a testReceiptPath",
		"(CHECK-BINDING-01) and `crw pabcd receipt test` refuses to write a receipt it cannot",
		"bind to a commit.",
		"",
		"Pick one:",
		"  - bind a git source tree:  crw relay session source <absolute-path> --json",
		"  - or run this cycle unbound (no `crw pabcd loop init --session`), which closes D on",
		"    checkOutput + exitCode alone.",
	}, "\n")
	if reason == template {
		return workDiagnosticTemplate(template)
	}
	return reason
}
