---
name: crw-dev-code-reviewer
description: "Independent code review of a diff or pull request: review process, quality thresholds, antipatterns, verdicts and test auditor gates. The author side of requesting and receiving feedback is crw-dev. Triggers: code review, PR review, check my diff, before merge, antipattern, 코드 리뷰, 머지 전에 확인."
---

# Dev-Code-Reviewer — Code Review Guide

> **C0/C1 work (small local patches):** See `dev` §0.0 Work Classifier + §0.1 Patch Fast-Path before reading references.

> **`dev` is canonical:** `dev` §0.2 Rule Classes, §3 Verification Gate, and §5 Safety Rules apply to all work governed by this skill.
> **Read the `dev` skill first** for project-wide conventions before applying review rules.

Systematic code review patterns for finding real issues, not bikeshedding.
This skill activates by change-surface for review requests, pre-merge checks, or independent audit passes.

## CRW ownership boundary

`crw-dev-code-reviewer` reviews code changes and pull requests.
`crw-check` checks delivery against the canonical tracking requirements and issue criteria.
`crw-check` also finds contradictions and broken invariants across specifications and evidence.
Use the skill that owns the requested judgment; code review does not certify delivery against the tracking issue.
The author's side, requesting and receiving review, is `crw-dev` §4.1.

## Review Posture (REVIEW-POSTURE-01)

Review as a skeptical, independent outsider. Executor claims, passing tests, AI summaries, and
user-facing "done" prose are untrusted until you confirm them yourself — assume the work may have
failed and look for the regression or false-confidence test that proves it. Inspect artifacts
before believing them; a green run you did not read is not evidence.

---
## Modular References

| File | When to Read | What It Covers |
|------|-------------|----------------|
| `references/tech-debt.md` | Tech debt inventory or paydown | Debt quadrant, inventory template, review integration, paydown budget |
| `references/ai-assisted-review.md` | Using AI review tools in PR workflow | AI review workflow, severity classification, re-review policy, exclusions, metrics |

`dev-testing` owns test adequacy and QA execution.
`dev-debugging` owns RCA when review discovers a runtime failure.
`dev-architecture` owns coupling and boundary placement.

## External/current review evidence

For dependency CVEs, release-note claims, package maintainer/source checks,
provider behavior, or other current/public evidence used in a review, read the
active `search` skill and follow its query-rewrite, source-fetch, and
evidence-status rules. Browser fetch/open/text/get-dom/snapshot is downstream
verification after candidate URLs exist, not a raw-query search substitute.

---

## 1. Code Review Process

### Automated Pre-Scan

Run the smallest repo-native checks that observe the requested review scope.
Docs-only reviews use document/contract checks; a diagnostic review does not authorize
installs, product changes, or a repository-wide suite prohibited by the user.

1. Errors block review readiness.
2. Warnings are non-blocking but must be reported.
3. Tool findings appear before manual findings.
4. Unavailable tools are skipped with the gap stated; absence is non-blocking.

Routing: linters catch style, imports, and simple bugs; type checkers catch type
and null-safety errors; SAST catches common injection/auth patterns; dependency
audits catch known CVEs. Errors block, warnings inform. Manual review remains
responsible for architecture, correctness, business intent, and cross-file impact.

### Review Order (by impact, not preference)

1. **Architecture** — Does the approach make sense? Right layer? Right abstraction? Is this the right place for this code?
2. **Correctness** — Logic errors, edge cases, off-by-one, null/undefined handling, error paths
3. **Security** — Input validation, injection risks, auth checks, secrets exposure
4. **Performance** — N+1 queries, unbounded collections, missing indexes, unnecessary computation
5. **Maintainability** — Naming, structure, complexity, test coverage, documentation
6. **Style** — Last priority. Don't bikeshed formatting when there are real issues.

Delegation: coupling classification belongs to `dev-architecture` §3; boundary and
validation-location findings belong to `dev-architecture` §4.

### Review Mindset

- **Be specific.** "This could fail" → "This throws if `user` is null on line 42"
- **Suggest, don't demand.** Unless it's a security or correctness issue.
- **Explain why.** Not just "change X to Y" but "X causes N+1 queries because..."
- **Acknowledge good work.** If a complex problem is solved elegantly, say so briefly.

### Output Contract (REVIEW-OUTPUT-01)

Tool findings go first (Automated Pre-Scan item 3); then manual findings sorted
`Critical > High > Medium > Low > Style`; then a dedicated `blocking_issues` block; verdict last.
For dispatched outline-assessment (legacy state A) reviews the verdict is additionally
machine-scannable: end the reply with exactly two final lines, `LAUNCH: <the launch id the
packet gave you>` and then `VERDICT: PASS`, `VERDICT: GO-WITH-FIXES (blockers=N)`, or
`VERDICT: FAIL` (mapping: Approve -> PASS; Approve-with-suggestions -> GO-WITH-FIXES;
Request-changes / Block -> FAIL). `N` is the number of blocking findings you went ahead
with, a whole number from 1 to 9999; name them as `(blockers=N; findings=c1,r2)` when
that helps. The same grammar is what the task work observer records and what the relay
renders in a report. A bare `VERDICT: GO-WITH-FIXES` still counts, with no count; a count
on `PASS` or `FAIL`, or a count that is zero, negative or not a number, is no verdict at
all, and the round stays open. The dispatching agent's exit rule is AUDIT-LOOP-01
(`crw-work` references/phase-audit.md): FAIL always triggers another round, and each blocker of a
GO-WITH-FIXES is folded into the plan or rebutted, which the main agent records in the attest's `auditBlockers`.
Every finding carries a concrete `trigger`, `impact`, and `path:line` (FAMILY-CITE-01) — no
finding on a hunch. Do not file pre-existing debt unless the patch worsened it. When a change
introduces a value/type/message crossing a module boundary, trace the consumer side before
declaring it correct, rather than reviewing the emitting hunk alone.

Compact finding example:

```yaml
severity: High
title: Missing ownership check permits cross-account access
location: src/routes/accounts.ts:42
trigger: Authenticated caller supplies another account ID
impact: Caller can read another user's account data
evidence: Handler loads by ID without constraining owner_id
remediation: Scope the query to the authenticated owner
verification: verified
```

### Regression & false-confidence tests (REVIEW-REGRESS-01)

Run a dedicated pass: what previously-working behavior can now break, and do the tests cover that
surface? Flag deletion-only "fixes", tautological tests, tests that merely mirror the
implementation, and scope-drift abstractions added beyond the request.

### Pre-Review Checklist

- Build passes.
- Tests pass.
- The change explains what changed and why.
- The diff is small and structured enough to review.
- If the PR is one layer of a stack, follow `DEV-STACK-05` in `crw-dev`
  `references/stacked-prs.md` — review scope, standalone judgment, base-ref and
  force-push checks, and the merge boundary all live there.

---

## 2. Quality Thresholds

### Severity Definitions

| Severity | Definition |
|----------|------------|
| Critical | Exploitable security flaw, data loss, or production outage |
| High | Correctness or security defect affecting users |
| Medium | Bounded defect or material maintainability risk |
| Low | Minor risk with limited impact |
| Style | Convention-only issue with no behavioral risk |

Flag these during review:

| Issue | Threshold | Severity |
|-------|-----------|----------|
| Long function | >50 lines | Medium |
| Large file | >400 lines | Medium; apply `dev-architecture` §1 canonical split rule |
| God class | >20 methods | High |
| Too many parameters | >5 | Medium |
| Deep nesting | >4 levels | Medium |
| High cyclomatic complexity | >10 branches | High |
| Missing error handling | any unhandled async | High |
| Hardcoded secrets | API keys, passwords in source | **Critical** |
| SQL injection | string concatenation in queries | **Critical** |
| Debug statements | console.log, debugger left in | Low |
| TODO/FIXME | unresolved in production code | Low |
| TypeScript `any` | bypassing type safety | Medium |

### File Size Guidance

Canonical rule imported from `dev-architecture` §1: **>400 LOC -> split (DEFAULT)**.

| Range | Interpretation |
|-------|---------------|
| 200-400 lines | Healthy — easy to navigate and review |
| 400-500 lines | Should split unless the author states a concrete reason |
| >500 lines | Strong review signal; not a blocker by size alone. Accept a documented cohesion/risk rationale |

### Review Verdict

| Indicator | Verdict | Action |
|-----------|---------|--------|
| No high/critical issues | ✅ Approve | Merge |
| Only Medium/Low/Style issues | 🔧 Approve with suggestions | Fix Medium before merge unless the author explicitly marks it non-blocking with a stated reason and tracked follow-up |
| Any unresolved High issue | ⚠️ Request changes | Author must address before merge |
| Any Critical issue | 🚫 Block | Cannot merge until resolved |

Deterministic blocker semantics (REVIEW-BLOCK-01): any unresolved Critical or High blocks the
merge. Medium findings should be fixed before merge. When explicitly marked non-blocking by the
author with a stated reason, Medium may pass with a tracked follow-up. Style never affects the verdict.

---

## 3. Common Antipatterns

### Structural

| Pattern | Symptom | Fix |
|---------|---------|-----|
| God class | One class does everything | Split by single responsibility |
| Long method | Function does 5+ distinct things | Extract named helper functions |
| Deep nesting | 4+ levels of if/for/try | Guard clauses, early returns, extraction |
| Feature envy | Method uses another object's data more than its own | Move method to the data owner |
| Shotgun surgery | One change requires edits in 10+ files | Consolidate related logic |

### Dead Code

| Pattern | Detection | Fix |
|---------|-----------|-----|
| Unreachable code after return/throw | `no-unreachable`, compiler warnings | Delete the dead branch |
| Unused imports / variables | `no-unused-vars`, `@typescript-eslint/no-unused-vars` | Remove |
| Commented-out code blocks | Manual review | Delete — use version control history |
| Unused exports | `ts-prune`, `knip`, consumer search | Remove scoped internal dead exports; public contracts require compatibility review before removal |
| Stale feature-flagged code | Check flag status in flag service | Remove dead branch and the flag check |

Dead code is a maintenance tax — remove rather than comment out.

### Logic

| Pattern | Symptom | Fix |
|---------|---------|-----|
| Boolean blindness | `doThing(true, false, true)` | Named options object or enum |
| Stringly typed | `status === 'actve'` (typo = silent bug) | Define enum or union type |
| Magic numbers | `if (retries > 3)` | Named constant: `MAX_RETRIES = 3` |
| Primitive obsession | Passing 5 related strings around | Create a data object/type |
| Direct mutation | `user.name = 'x'`, `arr.push(y)` | Immutable: `{...obj, name: 'x'}`, `[...arr, y]` |
| Missing boundary validation | Business logic handles raw user input | Delegate placement to `dev-architecture` §4; schema/content depth to `dev-security` |

### Security

This section owns the mandatory review pre-scan; `dev-security` owns security
policy and deep analysis. Use this checklist for hardcoded secrets, injection,
validation, auth, authorization, and logging findings.

---

## 3.5 Security Review Quick-Check

For every review, scan these OWASP-aligned red flags.

### Must-Check Every PR

| Check | Red Flag | Severity |
|-------|----------|----------|
| Hardcoded secrets | API keys, passwords, tokens, or DB URLs in source | **Critical** |
| Injection | User-controlled strings composed into SQL, NoSQL, shell, or templates | **Critical** |
| Missing validation | Untrusted input reaches logic without schema/content checks | **High** |
| Missing auth/authz | Endpoint lacks authentication or permission enforcement | **High** |
| BOLA / ownership | Object access does not verify caller ownership | **High** |
| Sensitive logging | Tokens, passwords, credentials, or private payloads enter logs | **High** |

### Conditional Checks

| Check | Trigger | Red Flag |
|-------|---------|----------|
| SSRF | User controls an outbound URL | No allowlist or host/protocol validation |
| Path traversal | User controls a file path | No canonicalization or containment check |
| Mass assignment | Request object populates a model | No explicit field allowlist |
| Dependency audit | Dependencies are added or updated | No repo-native vulnerability audit |
| Lockfile | Lockfile changes | Unexpected or unexplained resolution changes |

Load `dev-security` for deep analysis. Security routing is required when the diff
touches auth, credentials, untrusted input, sensitive data, dependencies, agent
tools, or trust boundaries.

---

## 3.6 Performance Review Quick-Check

Scan every PR for these common performance pitfalls:

### Database & API

| Check | Red Flag | Fix |
|-------|----------|-----|
| N+1 queries | Loop containing DB call or API fetch | Batch with `WHERE IN (...)` or DataLoader |
| Missing pagination | `.findAll()` or `SELECT *` without LIMIT | Add cursor-based or offset pagination |
| Missing index | New WHERE/JOIN column without index | `CREATE INDEX` on filtered/joined columns |
| Unbounded query | No LIMIT on user-facing list endpoints | Always set max page size |

### Frontend-Specific

| Check | Red Flag | Fix |
|-------|----------|-----|
| Measured expensive re-renders | Profiler identifies repeat work | Check Compiler activation and state ownership first; use manual memoization only where still useful |
| Bundle size impact | New large dependency (>50KB gzipped) | Check `bundlephobia.com`, consider alternatives or lazy loading |
| Missing `key` prop | List rendering without stable keys | Use unique ID, never array index for dynamic lists |
| Unoptimized images | Large images without `next/image`, `loading="lazy"`, or srcset | Use framework image optimization |

### General

| Check | Red Flag | Fix |
|-------|----------|-----|
| Missing timeout | External HTTP call without timeout | Set timeout on all network requests |
| Sync blocking | CPU-intensive work on main thread/event loop | Offload to worker/queue |
| Memory leak | Event listeners/subscriptions without cleanup | Add cleanup in `useEffect` return / `finally` block |

---

## 4. Reviewing AI-Generated Code

Run this in addition to the normal review when the diff is substantially AI-generated:

| Check | Typical failure | Required action |
|-------|-----------------|-----------------|
| Invented APIs | Plausible but nonexistent methods/options | Verify against installed-version docs |
| Hallucinated dependencies | Nonexistent or impersonated packages | Verify existence, maintainer, and provenance |
| Missing authz edges | Happy path lacks ownership checks | Trace endpoints against the BOLA check |
| Shallow tests | Tests mirror implementation | Require behavior-level assertions |
| Scope drift | Unrequested abstractions/refactors | Flag and restore one logical change per PR |

---

## 5. Subagent Review Mode

Parallelize review only when domain breadth exceeds one reviewer's context (e.g., frontend + backend + infra in a single diff, or when the diff spans too many unrelated domains for a single pass). Each subagent receives its file subset, the review process from sections 1-4, and outputs structured findings. The reviewer that dispatched them deduplicates, normalizes severity, and presents a unified review.

### AI Tool Integration Awareness

Read `references/ai-assisted-review.md` for tool coordination, AI-generated-code
checks, re-review policy, and agentic security triggers. Focus manual review on
architecture, intent, and cross-system impact. **STRICT
(REVIEW-AI-EVIDENCE-01):** de-duplicate, reproduce, and severity-normalize AI
findings; they are evidence to inspect, not authority.

### AI Slop Cleanup Checklist (REVIEW-SLOP-01)

Activate for explicit slop cleanup or >=3 slop findings. Lock behavior with green
tests before deletion.

| # | Category | Flag |
|---|----------|------|
| 1 | Obvious comments | Restatement, dead code, vague TODOs |
| 2 | Over-defense | Impossible guards, broad/empty catches |
| 3 | Excess complexity | Deep nesting, nested ternaries, god functions |
| 4 | Needless abstraction | Pass-through or speculative indirection |
| 5 | Boundary violations | Wrong-layer imports or misplaced logic |
| 6 | Oversized modules | >250 pure LOC smell; >400 split rule is canonical |
| 7 | Performance equivalents | Avoidable quadratic work or allocation |
| 8 | Scope leaks | Mutable globals or scattered environment reads |
| 9 | Missing behavior tests | Changed behavior without regression coverage |

**REVIEW-GUARD-REMOVAL-01 (DEFAULT).** Deleting input validation or error handling at a
trust boundary requires a regression test that actually EXERCISES the deleted path. For an
input-validation guard that means malformed/hostile input; for an error handler it means
injecting the fault that reaches it — network timeout, connection reset, filesystem I/O
failure, subprocess failure. Attaching an unrelated input test to satisfy the form does not
meet this bar. Without it the deletion is a **High** blocker: row 2 (Over-defense) above is
not by itself grounds for calling a boundary guard unnecessary. Trust boundaries are where
external input first lands: hook stdin, CLI arguments, file parsing, network responses,
subagent output.

**REVIEW-REMOVED-BACKEND-01 (DEFAULT).** A change touching `search/SKILL.md` gets checked
for removed search backends creeping back in as if they were available: `progrok`,
`web-AI`, `Grok Expert`, `GPT Pro`, `Exa`, `Tavily`, `Perplexity`, `Brave`. These names may
appear only in non-goal or historical framing.

There is no automated check, and there cannot be a useful one: crw owns no registry
of available backends to compare the prose against, because `web_search` is host-provided,
and a scan that reads one prose file for phrase existence breaks on rewording and proves
nothing (`TEST-PROMPT-SEAM-01` below). This is a reviewer's read, not a contract. If a
backend registry lands in code, revisit whether a real two-source check is possible.

Judge by deletion kind. A **replacing/relocating** deletion (the check moved elsewhere) must
stay GREEN after the old guard is removed, and go RED only when the surviving boundary check
is removed too. For a **non-replacing** deletion, a regression that goes RED proves the guard
is load-bearing — do not approve the deletion.

Test adequacy for prose-wording changes follows `TEST-PROMPT-SEAM-01` in [Test auditor gates](#test-auditor-gates); `crw-dev-testing` owns test adequacy (see its scope).

---

## Test auditor gates

What a reviewer checks in a test change. The author-side test strategy is in `crw-dev-testing`; the rule ids are unchanged.

### Test Oracle Integrity (TEST-PROMPT-SEAM-01 / TEST-ORACLE-INDEPENDENCE-01 / TEST-PRECEDENCE-FIXTURE-01, DEFAULT)

Three narrow contracts that stop false-green. All three are agent-followed prose; no gate enforces
them and the reviewer is the only check.

**TEST-PROMPT-SEAM-01 (DEFAULT).** Do not assert on prose. A test may read a document only
when it EXTRACTS a value and COMPARES it against a value from another source. Asserting that
a phrase exists in a file is a violation no matter how many files you read or what the test's
header says it is for.

- Forbidden: `assert.match(readFileSync(".../SKILL.md"), /prefer rg first/i)` — one source,
  phrase existence, breaks on harmless rewording, proves no behavior.
- Allowed: parse the frontmatter `description`, pull the model token out of it, and
  `assert.ok(NATIVE_OPENAI_MODELS.includes(token))` — two sources, values compared, breaks
  only when they genuinely disagree.
- Also allowed (outside this rule's scope): asserting on non-prose values — version pins,
  license names, runtime output, CLI stdout, hook payloads, file existence.

Where prose has no counterpart in code, a test that reads it only checks that the words are
still there. Such prose (a doctrine statement, a skill's wording) is a human-review item with no automated
check; inventing a second document to compare it against adds a false green.

**TEST-ORACLE-INDEPENDENCE-01 (DEFAULT).** Never derive the expected value from the code
under test.

- Forbidden: `assert.equal(fn(x), fn(x))`; building the expectation with a helper the DUT
  also uses; refreshing a snapshot from current output and calling that verification.
- Allowed: hardcode the expectation in the fixture, or compute it by an independent route
  (a second implementation, a hand-worked example, an external spec).

**TEST-PRECEDENCE-FIXTURE-01 (DEFAULT).** When testing override / default / fallback, the
three values must all DIFFER — otherwise the test cannot tell which path ran.

- Forbidden: override `"x"`, default `"x"`, fallback `"x"` — every branch passes.
- Allowed: override `"from-flag"`, default `"from-config"`, fallback `"builtin"`, and each
  case asserts the specific one.

A regression test should FAIL when the defect is reintroduced. Confirm it once by mutation —
break it, watch it go red, restore it, watch it go green.

### Patch Integrity Gate (TEST-PATCH-INTEGRITY-01, DEFAULT)

An agent that obtains green by weakening tests has not fixed the bug. Before
claiming implementation complete:

1. **Baseline**: record which tests fail and their failure signatures BEFORE any
   production code change.
2. **Implement**: write the fix/feature.
3. **Re-run originals**: execute the ORIGINAL test suite (not the modified version)
   against the patched source. All baseline failures must now pass.
4. **Classify test changes**: every test/config change is `required` (new test for
   new behavior), `suspicious` (deleted assertion, lowered threshold, added skip,
   reduced coverage exclusion), or `unrelated`.
5. **Justify suspicious changes**: each suspicious change needs a stated reason.
   "The test was wrong" is valid only with evidence of the original test's incorrectness.

Red flags that trigger escalation:
- Deleted assertions without replacement
- Snapshot updates without visual/behavioral verification
- Coverage exclusions added in the same PR as the fix
- `@skip` or `.skip()` added to failing tests — unless it is a `TEST-FLAKE-QUARANTINE-01` quarantine carrying all four required fields (`../crw-dev-testing/references/ci-pipeline.md` §5.3)
- Threshold reductions (e.g., coverage 80% → 60%)
- Type assertion suppressions (`as any`, `@ts-ignore`) in test files

### TDD Evidence Contract (TEST-TDD-EVIDENCE-01, DEFAULT)

When TDD is claimed, durable evidence must show:
- RED: failing test name + failure message (before production code)
- GREEN: same test passing (after production code)
- REFACTOR: full affected suite passing (after cleanup)

A TDD claim without RED evidence is not TDD.

## Changed-File Coverage Ledger (REVIEW-COVERAGE-01, DEFAULT)

Account for every changed file as `reviewed`, `skipped (reason)`, or
`out-of-scope (reason)` before verdict. Generated, lock, vendored, binary, and
outside-domain files may be skipped only with an explicit reason. Any
unaccounted file makes the verdict incomplete.

## Finding Falsification (REVIEW-FALSIFY-01, DEFAULT)

Before reporting a finding:

1. State it as a testable claim.
2. Search tests, guards, caller context, and docs for contradictory evidence.
3. Downgrade or retract the claim when contradictory evidence disproves it.
4. Retain the claim when it survives the falsification attempt.

Every finding includes `verification: verified|unverified`. Use `unverified`
when the falsification attempt could not be completed or evidence is incomplete.

## Interdiff Re-Review (REVIEW-INTERDIFF-01, DEFAULT)

Anchor the re-review to both the previous reviewed commit/range and the new head;
record those anchors in the review. Review only the interdiff, preserve unresolved
findings, verify each claimed fix, and process new findings normally. Revisit
unchanged code when a cross-file dependency changed. If either anchor is missing,
history was rewritten ambiguously, or the interdiff cannot be trusted, fall back
to a full review of the current base-to-head diff.

**REVIEW-WORKTREE-01 (DEFAULT).** Never check out another review ref in the worktree you
were handed. If the review target is already checked out in the worktree the dispatcher
assigned, review it there; otherwise create or attach a dedicated named worktree and run the
checkout, tests and QA in that one. Record `pwd -P` and the target `HEAD` alongside the
anchors above.

The condition is observable state, not ownership: a reviewer, often an executor-level subagent, cannot know
which branch the task that dispatched it owns, but it can always see what is checked out where it
stands.
