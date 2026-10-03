// Package attest is the evidence gate of the PABCD phase transitions: the Go form of CXC v0.2.40 pabcd-state/src/attest.ts and
// plan-gate.ts (both whole files, commit 3c1459ac). A forward edge (P>A, A>B, B>C, C>D) advances only with evidence: a specific
// narrative; for A>B the pasted verdict of an independent reviewer and the agent's judgment of it; for C>D pasted command output
// with a passing exit code; and for P>A a plan that exists as numbered files. The package validates and writes nothing.
//
// Behaviour is ported as-is, oracle defects included (docs/port-cxc/known-defects.md). validateAttest is Validate, coerceAttest
// is Coerce, and the two sets are functions (IsGated, GatedTransitions, IsAuditVerdict). Reasons carry CRW names where the oracle
// names its command (name-substitution R9, R33, cli table): "crw pabcd plan init", "crw pabcd receipt test", "CRW-ROLE:".
//
// Callers run, in the oracle's order: Coerce, on P>A ValidatePlanArtifacts (orchestrate-cli.ts:600), on every gated edge
// ValidateWorkPhaseBinding with the bound goalplan's active work phase (nil when none), then Validate (fsm.ts:120). The FSM port
// must not live in package state, which this package imports for Phase.
//
// JavaScript semantics are reproduced where a value reaches a decision or a message (trim, toLowerCase with U+0130 and the final
// sigma, ASCII-only /i, number-to-string). Not carried: a lone surrogate (a Go string holds U+FFFD instead), and NaN or Infinity,
// which JSON cannot spell and only a direct Go caller can put in ExitCode (Coerce drops them, as the oracle does). Absent and
// empty strings are one value, as are an absent and a false override: nothing in the oracle reads the difference.
package attest

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// The values of AuditVerdict, the main agent's judgment of one audit round (AUDIT-LOOP-01); fail never advances.
const (
	VerdictPass     = "pass"
	VerdictNearPass = "near-pass"
	VerdictFail     = "fail"
)

// Attestation is the evidence attached to a phase transition; From and To are whatever the agent wrote (Validate checks them).
type Attestation struct {
	From state.Phase `json:"from"`
	To   state.Phase `json:"to"`
	Did  string      `json:"did"` // what the agent did in this phase
	// A>B: the pasted tail of the reviewer's verdict, the agent's own judgment of the round and, for near-pass, each residual
	// blocker with its disposition. AuditRounds is a ledger trail and never gates.
	AuditOutput   string   `json:"auditOutput,omitempty"`
	AuditVerdict  string   `json:"auditVerdict,omitempty"`
	AuditResidual string   `json:"auditResidual,omitempty"`
	AuditRounds   *float64 `json:"auditRounds,omitempty"`
	// C>D: the pasted tail of the command that was run and its exit status (nil is not a number).
	CheckOutput string   `json:"checkOutput,omitempty"`
	ExitCode    *float64 `json:"exitCode,omitempty"`
	// Override accepts an unready interview on I>P.
	Override bool `json:"override,omitempty"`
	// P>A: the plan unit directory and the plan documents the work phases execute from.
	PlanUnit  string   `json:"planUnit,omitempty"`
	PlanPaths []string `json:"planPaths,omitempty"`
	// The one work phase this cycle advances; the test receipt a bound session needs on C>D.
	WorkPhaseID     string `json:"workPhaseId,omitempty"`
	TestReceiptPath string `json:"testReceiptPath,omitempty"`
}

// Result is attest.ts AttestResult. Reasons lists every failing requirement of the edge in declaration order and Reason is the
// text shown: the single reason, or the reasons numbered "(i/n)" one per line. A work-phase binding refusal has a Reason only.
type Result struct {
	OK      bool
	Reason  string
	Reasons []string
}

// GatedTransitions lists the forward edges that need an attestation, in the oracle's order.
func GatedTransitions() []string { return []string{"P>A", "A>B", "B>C", "C>D"} }

// IsGated reports whether the edge from>to needs an attestation. Backward edges, the interview entry and the D>IDLE close do not.
func IsGated(from, to state.Phase) bool {
	return slices.Contains(GatedTransitions(), string(from)+">"+string(to))
}

// IsAuditVerdict reports whether v is one of the three verdicts.
func IsAuditVerdict(v string) bool {
	return v == VerdictPass || v == VerdictNearPass || v == VerdictFail
}

func refuse(reasons ...string) Result {
	reason := reasons[0]
	if len(reasons) > 1 {
		numbered := make([]string, len(reasons))
		for i, r := range reasons {
			numbered[i] = fmt.Sprintf("(%d/%d) %s", i+1, len(reasons), r)
		}
		reason = strings.Join(numbered, "\n")
	}
	return Result{Reason: reason, Reasons: reasons}
}

// Coerce is attest.ts coerceAttest: it turns a decoded JSON value (map[string]any, numbers as float64 or json.Number) into an
// Attestation, or nil unless both from and to are strings. A missing did still coerces (to "") so Validate can say so. A field of
// the wrong type is dropped; text is trimmed.
func Coerce(v any) *Attestation {
	rec, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	from, fromOK := rec["from"].(string)
	to, toOK := rec["to"].(string)
	if !fromOK || !toOK {
		return nil
	}
	str := func(key string) string { s, _ := rec[key].(string); return text.Trim(s) }
	att := &Attestation{
		From: state.Phase(from), To: state.Phase(to), Did: str("did"),
		AuditOutput: str("auditOutput"), AuditVerdict: lowerJS(str("auditVerdict")), AuditResidual: str("auditResidual"),
		AuditRounds: finite(rec["auditRounds"]), CheckOutput: str("checkOutput"), ExitCode: finite(rec["exitCode"]),
		PlanUnit: str("planUnit"), WorkPhaseID: str("workPhaseId"), TestReceiptPath: str("testReceiptPath"),
	}
	att.Override, _ = rec["override"].(bool)
	paths, _ := rec["planPaths"].([]any)
	for _, p := range paths {
		if s, ok := p.(string); ok && text.Trim(s) != "" {
			att.PlanPaths = append(att.PlanPaths, text.Trim(s))
		}
	}
	return att
}

// finite is the value as a JavaScript number that Number.isFinite accepts, or nil. A json.Number that overflows (1e999 is
// Infinity to JSON.parse) is not finite.
func finite(v any) *float64 {
	f, isNumber := v.(float64)
	if n, ok := v.(json.Number); ok {
		f, _ = strconv.ParseFloat(string(n), 64)
		isNumber = true
	}
	if !isNumber || math.IsInf(f, 0) || math.IsNaN(f) {
		return nil
	}
	return &f
}

// ValidateWorkPhaseBinding is attest.ts validateWorkPhaseBinding (LOOP-UNIT-CHAIN-01): on a gated edge of a goalplan-bound session
// the attestation must name the one active work phase. active is nil when no goalplan is bound, which never refuses.
func ValidateWorkPhaseBinding(att *Attestation, active *string) Result {
	switch {
	case active == nil:
		return Result{OK: true}
	case att == nil || att.WorkPhaseID == "":
		return Result{Reason: "A goalplan is bound (active work-phase " + *active + `); pass "workPhaseId" in the attest. One work-phase = one full PABCD cycle (LOOP-UNIT-CHAIN-01).`}
	case att.WorkPhaseID != *active:
		return Result{Reason: "attest.workPhaseId=" + att.WorkPhaseID + " but the active work-phase is " + *active + ". Close this cycle through D before touching another unit (LOOP-UNIT-CHAIN-01)."}
	}
	return Result{OK: true}
}

// Validate is attest.ts validateAttest, the form-only gate: only a gated edge needs an attestation, and a missing or placeholder
// narrative, a missing reviewer verdict or a failing check is refused, naming every failing requirement of the edge at once.
func Validate(from, to state.Phase, att *Attestation) Result {
	if !IsGated(from, to) {
		return Result{OK: true}
	}
	// With none, or one for another edge, every other field check would be about the wrong edge.
	if att == nil {
		return refuse(fmt.Sprintf(`%s -> %s requires an attestation with a non-empty "did". Pass --attest-file <path> (required on Windows) or --attest '{"from":"%s","to":"%s","did":"..."}'.`, from, to, from, to))
	}
	if att.From != from || att.To != to {
		return refuse(fmt.Sprintf("Attestation from/to (%s->%s) does not match the requested transition %s->%s.", att.From, att.To, from, to))
	}
	var reasons []string
	add := func(format string, args ...any) { reasons = append(reasons, fmt.Sprintf(format, args...)) }
	if att.Did == "" || isPlaceholderDid(att.Did) {
		add(`%s -> %s needs a specific "did" narrative (not empty or a placeholder).`, from, to)
	}
	switch string(from) + ">" + string(to) {
	case "A>B":
		if att.AuditOutput == "" {
			add(`A -> B additionally requires "auditOutput": paste the tail of the independent reviewer verdict you actually received. Dispatch a reviewer subagent with agent_type "reviewer" if the live schema exposes that native role; otherwise use agent_type "explorer" with CRW-ROLE: reviewer before TASK:; if the host has no agent_type field, omit agent_type and prepend CRW-ROLE: reviewer before TASK: (DISPATCH-AGENT-TYPE-01) at the A gate; a self-written sentence is not an audit.`)
		}
		if !IsAuditVerdict(att.AuditVerdict) {
			add(`A -> B additionally requires "auditVerdict": "pass" | "near-pass" | "fail" - YOUR OWN judgment of this audit round (AUDIT-LOOP-01). "fail" never advances; "near-pass" means every blocking finding was folded into the plan or explicitly rebutted (also supply "auditResidual").`)
		}
		if att.AuditVerdict == VerdictNearPass && att.AuditResidual == "" {
			add(`A -> B with "near-pass" additionally requires "auditResidual": name each residual blocker and its disposition (folded into plan / rebutted with rationale), e.g. "GO-WITH-FIXES; 2 blockers folded back: (1) ..., (2) ...".`)
		}
		// The contradiction checks run only once every required field is present, so a message never both demands a field and
		// reasons about its value.
		if len(reasons) == 0 {
			if att.AuditVerdict == VerdictFail {
				add(`A -> B is blocked: you judged this audit round "fail". Synthesize the blockers (REVIEW-SYNTHESIS-01), amend the plan, and re-audit with the SAME reviewer (v2 surface: followup_task to its task_name; v1 surface: send_input to its agent_id). Re-attest with "pass" or "near-pass" once only folded/rebutted residuals remain; after 3 failed rounds return to P with a changed plan (LOOP-REPAIR-01).`)
			} else if HasFailVerdictTail(att.AuditOutput) {
				add(`The pasted auditOutput tail ends with a FAIL verdict line, contradicting auditVerdict="%s". Run another audit round (same reviewer) and paste the round that actually reached PASS / GO-WITH-FIXES - or attest "fail" and keep looping (AUDIT-LOOP-01).`, att.AuditVerdict)
			}
		}
	case "C>D":
		if att.CheckOutput == "" {
			add(`C -> D additionally requires "checkOutput": paste the tail of the test/tsc command you actually ran.`)
		}
		if att.ExitCode == nil {
			add(`C -> D additionally requires "exitCode": the exit status of the command whose output you pasted. Report the real number - a check with no outcome is not a check.`)
		}
		// The receipt reminder below follows missing fields only (the narrative counts as one): a complete attest whose check
		// failed gets none.
		missing := len(reasons)
		if att.ExitCode != nil && *att.ExitCode != 0 {
			add(`C -> D requires a passing check, but the attestation reports exitCode %s. Fix the failure (orchestrate B) before advancing.`, jsNumber(*att.ExitCode))
		}
		if missing > 0 && att.TestReceiptPath == "" {
			add("C -> D on a goalplan-bound session ALSO requires \"testReceiptPath\" (CHECK-BINDING-01), produced by `crw pabcd receipt test -- <command>`. Supplying it now avoids another round trip.")
		}
	}
	if len(reasons) == 0 {
		return Result{OK: true}
	}
	return refuse(reasons...)
}
