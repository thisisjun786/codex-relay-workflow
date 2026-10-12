// crw:lib/schemas: the output schemas of the flow roles. Codex sends outputSchema as a strict json_schema, so every
// schema here follows the strict structured-output rules: the root is an object, every object lists additionalProperties
// false and every property in required (an optional field is a required nullable union), and only supported keywords
// appear. Loading a script that imports this module checks every export against those rules
// (internal/flow/script, LintStrictSchema); a violation is a load error. The module exports schemas only.

const text = { type: 'string' }
const maybeText = { type: ['string', 'null'] }
const texts = { type: 'array', items: { type: 'string' } }
const integer = { type: 'integer' }
const flag = { type: 'boolean' }

// obj builds a strict object schema: every property is required.
function obj(properties) {
  return { type: 'object', properties, required: Object.keys(properties), additionalProperties: false }
}
function list(items) {
  return { type: 'array', items }
}

const criterion = obj({ id: text, met: flag, evidence: text })

const defect = obj({
  ref: text,
  kind: { type: 'string', enum: ['correctness', 'criteria_unmet', 'test_gap', 'evidence_missing', 'scope', 'maintainability', 'other'] },
  severity: { type: 'string', enum: ['P0', 'P1', 'P2', 'P3'] },
  introduced: flag,
  impact: text,
  inPromise: flag,
  file: text,
  line: integer,
  finding: text,
  evidence_question: { anyOf: [obj({ question: text, files_needed: texts }), { type: 'null' }] },
})

const verdictFields = {
  verdict: { type: 'string', enum: ['verified', 'needs_changes'] },
  criteria: list(criterion),
  defects: list(defect),
  summary: text,
}

// RECEIPT: the executor's report. The run, not the self-report, judges the worktree.
export const RECEIPT = obj({
  outcome: { type: 'string', enum: ['ready_for_review', 'needs_research', 'blocked_scope', 'failed', 'budget_exhausted'] },
  files: texts,
  verification: list(obj({ cmd: text, exit: integer, summary: text })),
  summary: text,
  gap: maybeText,
  research_question: maybeText,
  plan_mismatch: maybeText,
})

// VERDICT: one node's review, shaped for the premerge record.
export const VERDICT = obj(verdictFields)

// VERDICT_BATCH: one block per member of a batched review.
export const VERDICT_BATCH = obj({
  nodes: list(obj({ node: text, head: text, criteria_digest: text, ...verdictFields })),
})

// VERDICT_FINAL: the milestone-level review of reviewer-final, with the alignment judgement.
export const VERDICT_FINAL = obj({
  verdict: verdictFields.verdict,
  criteria: verdictFields.criteria,
  defects: verdictFields.defects,
  alignment: obj({
    verdict: { type: 'string', enum: ['ALIGNED', 'MISALIGNED', 'NOT_ASSESSED'] },
    drift: list(obj({ decision_id: text, observed: text, file: text, line: integer, affected_nodes: texts })),
  }),
  summary: text,
})

// DESIGN: the architect's decisions and per-issue plan.
export const DESIGN = obj({
  decisions: list(obj({ id: text, evidence: texts, change: text, alternatives: texts, assumptions: texts })),
  issues: list(obj({
    issue: text,
    node: text,
    size_fit: { type: 'string', enum: ['executor', 'child', 'split'] },
    size_reason: text,
    issue_size_decision: { type: 'string', enum: ['ok', 'over_line', 'over_line_accepted'] },
    edit_regions: texts,
    approach: text,
    tests_first: texts,
    done_condition: text,
    decision_refs: texts,
    order_after: texts,
    group: maybeText,
  })),
  open_questions: list(obj({ id: text, question: text, scope: text, files_needed: texts })),
  risks: texts,
})

// DESIGN_AUDIT: the independent audit of a DESIGN before the first executor starts. The auditor is a reviewer of another
// model family than the architect; needs_changes sends the DESIGN back to the architect (the same thread), and the executors
// start only on an approved audit. The findings name what must change, by the decision or issue they concern.
export const DESIGN_AUDIT = obj({
  verdict: { type: 'string', enum: ['approved', 'needs_changes'] },
  findings: list(obj({
    ref: text,
    severity: { type: 'string', enum: ['P0', 'P1', 'P2', 'P3'] },
    finding: text,
    evidence: texts,
  })),
  summary: text,
})

// REFLECTION: the architect's milestone-close check of its own decisions.
export const REFLECTION = obj({
  verdict: { type: 'string', enum: ['ALIGNED', 'MISALIGNED'] },
  misaligned: list(obj({ decision_id: text, observed: text, affected_nodes: texts, evidence: texts })),
})

// FINDINGS: the explorer's answer.
export const FINDINGS = obj({
  answer: text,
  evidence: texts,
  verified: texts,
  inferred: texts,
  unknown: texts,
})

const qaVerdict = { type: 'string', enum: ['PASS', 'REVISE', 'FAIL'] }
const qaFinding = obj({ ref: text, severity: { type: 'string', enum: ['P0', 'P1', 'P2', 'P3'] }, finding: text, evidence: texts })

// QA_CAPTURE: the answer of the QA capture call (SP-QA-1) of a final gate: what it captured from the surface, the scenarios it
// drove and the teardown of what it started. A capture that is not listed here does not exist for the gate; the call judges
// nothing in this answer (section 2 to 4 of the crw-qa skill).
export const QA_CAPTURE = obj({
  captures: list(obj({ path: text, kind: { type: 'string', enum: ['screenshot', 'terminal_capture', 'response', 'transcript'] }, shows: text })),
  matrix: texts,
  teardown: texts,
  summary: text,
})

// QA_PASS: the answer of one read-only oracle pass (SP-QA-2): Pass A judges design and functional integrity, Pass B visual fidelity
// and CJK precision, both over the same captures.
export const QA_PASS = obj({
  pass: { type: 'string', enum: ['A', 'B'] },
  verdict: qaVerdict,
  findings: list(qaFinding),
  summary: text,
})

// QA_RECEIPT: the QA verdict of a final gate. For a C2 milestone it is the self-review of the QA call over its own captures; for
// C3 and above it is the synthesis of the two oracle passes, asked on the thread of the QA call.
export const QA_RECEIPT = obj({
  verdict: qaVerdict,
  findings: list(qaFinding),
  summary: text,
})
