---
name: crw-logic
description: "Find consequential contradictions and broken invariants across canonical Linear specifications, design decisions, calculations, implementation evidence, and evaluations, using focused Paperthin checks. Use for a requested logical-consistency or invariant audit; crw-check owns requirement-to-delivery coverage. Formerly linear-logic."
---

# CRW Logic

Find claims or behaviors that cannot both hold under the same stated conditions. Anchor the rules and accepted decisions in canonical Linear documents. Return reproducible contradictions or precise evidence gaps, not a list of dislikes. Standalone investigation is read-only; inside an authorized execution assignment, pass actionable findings into the coordinator's correction and verification path.

## Connect the sources and checks

Read [Integrations](../crw-plan/references/integrations.md). Resolve the target Linear project, canonical documents, linked issues, accepted decisions, and relevant repository evidence. Read full text and record IDs, dates/revisions, and source anchors. Do not infer the rule from issue status or a copied local summary.

If only a local file or pasted artifact is supplied, audit that bounded input and label its canonical Linear link unknown. Missing connector access can yield a partial audit; it cannot establish cross-document consistency. Do not create or migrate documents simply to run the check.

Read selected installed Paperthin skills and use them according to the question:

| Question | Route |
|---|---|
| Did we understand the request and its scope? | `readchk` when materially ambiguous |
| Do documents disagree about the same fact/rule? | `ssotize` in audit mode |
| Is a factual premise unsupported or false? | `factchk`; use `cxc-search` for current/public sources |
| Does an evaluation independently test its claim? | `mandela` |
| Would a fresh reader recover the same rule? | `shower` when a blind comprehension check is needed and delegation is available |
| Which single assumption could invalidate the plan? | `hate`, only when explicitly requested by the user |
| Do distinct failure modes lead to different judgments? | `prism`, only when explicitly requested by the user |
| Can the person justify a recent decision? | `feynman`, only when explicitly requested; do not turn an ordinary audit into an interrogation |

Preserve helper invocation and output rules from Integrations. If the user explicitly requests `hate`, preserve its one root and cheapest falsification; if they request `prism`, preserve its lens disagreement or shared conclusion. Do not relabel a same-context read as an independent check.

For code, use installed `cxc-dev` and the relevant verification owner. CXC owns authorized implementation, loops, and subagent routing; this audit does not start a loop or create a competing workflow. Return bounded findings to `crw-check` or `crw-run` when called by them.

Where this audit runs as a sub-task or bounded helper, its record changes are returned and not written: the record ID, the revision read, the reason, the smallest sufficient change and its evidence go to the caller, which decides and writes, under [record writes and returned proposals](../crw-plan/references/integrations.md#record-writes-and-returned-proposals).

## Establish what must hold

Read the target and relevant dependencies. Pin assumptions, units, time windows, population/scope, state/version, and decision authority. Distinguish approved requirements from proposals and implementation choices. Where two documents state the same fact differently, [where each document type is canonical](../crw-plan/references/integrations.md#where-each-document-type-is-canonical) decides which one is the rule and which is a copy that went stale; recency does not. Derive invariants from those contracts or independently justified facts, not from the function being tested.

State a suspected contradiction as: **given these conditions, rule A requires X, while claim or behavior B requires not-X**. If conditions differ, investigate that mismatch before declaring a contradiction. Missing evidence, uncertainty, a tradeoff, a documented exception, and a changed requirement are distinct outcomes.

Use [Counterexample patterns](references/counterexamples.md) when selecting a numerical, state-transition, evaluation, or cross-document probe. Apply only patterns justified by the actual contract.

When auditing status or delivery invariants, use
[Implementation Done](../crw-plan/references/integrations.md#implementation-done):
one current delivery PR and actual intended-target merge for new implementation,
reconciled combined PR coverage for legacy multi-PR scope, and verified result for
non-PR work. Existing accepted operational criteria remain binding. Test a reference or partial merge against a complete
delivery; treat automated Done without matching evidence as a claim to investigate,
not authority to rewrite status or criteria.

## Try to disprove the suspicion

Find the smallest case that distinguishes a violation from a valid exception. Inspect or run it in the authorized environment and record input, expected relation, actual result, and source/artifact. A static code proof may establish a violation but must not be reported as live execution. Keep diagnostic writes in a task-owned temporary location when permitted.

Pair a suspected failure with a valid contrasting case. Consider whether a correction would reject normal retries, erase uncertainty, clamp unrelated quantities, or change historical semantics. A passing pre-existing suite does not prove an untested invariant.

Use independent evidence appropriate to the claim. A second model agreeing with the first is not physical or business ground truth. If inputs, access, or observations are missing, report the evidence gap and cheapest resolving check. Do not manufacture a failure to satisfy the audit.

## Report and stop

Report consequential findings ranked by impact. Include the conflicting rules and Linear source anchors, shared conditions, minimal counterexample, valid contrast, consequence, and smallest correction or next check. Mark the evidence as **reproduced violation**, **static contradiction**, or **unverified hypothesis**; separate these from a resolved apparent contradiction caused by different scope or an accepted change.

When none is found, state the examined scope and remaining evidence limits. Missing verification cannot become a universal pass. Stop when the bounded questions are answered; more loops need a new question or invalidated evidence.

Under an existing management assignment, record results in the linked Linear coordination document, link the canonical rules, and read it back. Use a separate audit document only when prior authorization or an explicit recording request covers that document; do not ask again for authorization already granted. A bounded helper returns the record to its coordinator. A standalone read-only audit returns the proposed record. Recording findings does not authorize rewriting requirements or changing issue status.

Use the full assignment's authorization from [Integrations](../crw-plan/references/integrations.md#completion-follow-up-in-an-existing-execution-workflow). In an existing execution workflow, return evidence-backed corrections to the CXC/`crw-run` owner for implementation and recheck without another user permission round. Standalone audits return proposals. Route authorized planning corrections to `crw-plan`; a finding alone does not authorize new scope.
