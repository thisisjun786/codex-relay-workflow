# Spawn policy

Which agent calls a flow run makes, which signal adds one, which work the run does itself, what is recorded for each decision, and the two kinds of refusal that no option overrides. A project orchestrator reads it to know why a run spawned or did not spawn; an initiative lead and the status readers use it to read a run's decisions. The commands are in [flow.md](flow.md), the script format is [docs/flow/script.md](../../../../../docs/flow/script.md), and the roles are named by policy key in [execution-roles.md](execution-roles.md), never by model.

The workflow decides the structural spawns. Signals with numeric thresholds decide the rest. Anything cheaper is done inline by code. Every decision (spawn, inline, reuse, skip, refused) and its outcome is an append-only relay record. A rule id is the reason a row gives for the decision it records. The ids are the constants of `internal/flow/ops/rules.go`, the one place the strings are written; a script names a rule by its key (`EXEC_1`, `TIER_1`) and the id is looked up there, so a rule renamed there is renamed here by the test that compares this page with that file.

## Structural spawns

The run makes these calls whatever the issue says; a script does not choose them.

| Rule | Spawn | When | Note |
|---|---|---|---|
| `SP-EXEC-1` | an executor | every issue node the DAG scheduler releases, one per attempt | A declared group (at most four issues) runs in its declared order through compile's order edges, one fresh executor per member; no thread is shared across nodes |
| `SP-REV-1` | a reviewer | every executor head with a passing verification, one per round | A separate thread, and its model family differs from every observed author pair |
| `SP-REV-2` | a reviewer over a candidate batch | the low-tier nodes of a milestone whose heads passed verification | The policy names it for a batch review. The project lane reviews each head with `SP-REV-1` and does not batch; a batch writes one verdict for each member, never one verdict for the batch |
| `SP-AUD-1` | a design-audit reviewer | once for each DESIGN candidate, before the first executor of the milestone starts | A reviewer of another model family than the architect; `needs_changes` returns the DESIGN to the architect, and a replan is audited again |
| `SP-ARCH-1` | the architect | once for each milestone | Covers every issue of the milestone |
| `SP-REVF-1` | reviewer-final | once at the milestone close, over the integrated range, after the final gate passes | After integration only: it rules on the range and its alignment. It writes no pre-merge record and never replaces `SP-REV-1` or `SP-AUD-1` |
| `SP-CHILD-1` | an issue child by managed start | the architect set `size_fit` to `child` | The child only coordinates: it runs the issue-lane workflow, whose executor calls author every commit |
| `SP-QA-1` | the QA capture call | the final gate of a milestone with a web, tui or desktop criterion and no fresh QA receipt for the pinned head | See [the QA calls](#the-qa-calls-of-the-final-gate) |
| `SP-QA-2` | two oracle passes | after `SP-QA-1` returned its captures, for a visual surface of work grade C3 or above | See [the QA calls](#the-qa-calls-of-the-final-gate) |

## Signals

A signal adds a call only when its threshold is crossed; the run reads the numbers from its own journal.

| Rule | Signal | Threshold | Action |
|---|---|---|---|
| `SP-EXPL-1` | the open questions of the architect's DESIGN, at the design audit and at the audit of a re-plan | one explorer for each question whatever its files needed, at most 3 explorers at once and at most 6 explorers for each milestone; the questions beyond 6 go to the orchestrator thread as design questions. A question whose scope starts with the notation of an out-of-authority kind (`authority:repo-rule`, `authority:other-record`, `authority:policy`, `authority:environment`, compared without regard to case) goes to the orchestrator thread as a design question and to no explorer; a missing or unknown notation goes to an explorer | spawn the explorer; the answers are appended to the packets they affect; the design audit packet shows each question's route and what it got (a reuse with the route its answer came from), and the design questions go up in a non-blocking `design_question` escalation: an authority question in the audit round that routed it, approved or not, the questions beyond 6 once the DESIGN is approved; a question the run asked already raises no ask of its own and is carried by each later ask while it has no answer, and the orchestrator's applied answer is reused from the next round on |
| `SP-EXPL-2` | an executor's receipt that needs research and carries a research question | one explorer for each distinct question digest of a node, at most 2 research continuations on a tier, then the node escalates as a scope question | spawn the explorer or reuse the answered digest, then continue the same executor thread with the answer; never a tier step |
| `SP-EXPL-3` | a reviewer's defect of kind `evidence_missing` | one explorer for each distinct evidence question before the next round, at most 2 explorers for each node | its findings go to the executor's fix packet and to the next reviewer's packet; no such defect is a recorded skip |
| `SP-TIER-1` | independent failure evidence on the current tier | any of: `needs_changes` in two consecutive rounds with a repeated finding; the required verification failing on two consecutive committed heads; a null result on two consecutive executor calls of the tier; 4 reviewed rounds without a verified verdict | the run appends a step decision with the evidence ids and a checkpoint; only then does a fresh executor of the next tier start, in a new worktree at the last head. One step for each node |
| `SP-TIER-2` | the tier step is exhausted | `SP-TIER-1` evidence again on the next tier | no spawn; the node escalates (`tiers_exhausted`) to the orchestrator thread: a split through `crw-add-issue` and a plan revision, a child, or a decision for Jun |
| `SP-CHILD-2` | `size_fit` is `child` | the architect sets it with a `size_reason` of the closed set of [crw-plan's size rules](../../crw-plan/SKILL.md#size-rules), or the orchestrator judges a blocked-scope node oversized | the node is released as an issue child instead of an executor |
| `SP-ARCH-2` | a re-plan | two distinct receipts that report a plan mismatch since the last DESIGN, a plan revision that changes more than a quarter of the milestone's node slices, or a misaligned verdict of reviewer-final or of the reflection; any one reason is enough | new releases pause, running lanes finish their call, the architect re-plans on its own thread and only unstarted nodes take packets from the delta. A skipped re-plan is recorded with its reason |
| `SP-ARCH-3` | the milestone close | the integrated range is final | the architect reflects on the range, on its own thread |
| `SP-NAT-1` | a native spawn from a flow-bound thread | the thread's binding allows it | one explorer for each call of the roles that may have one; the spawn hook applies the role's pair and records the decision |

A reason that is not a number does not make a signal: the wish to go faster, the executor's own report that it failed, a single null result, a held or refused call and an engine-initiated interrupt are never tier evidence.

## Inline work

Work that is cheaper to do than to ask is done by code or by the asker, and recorded as such.

| Rule | Work |
|---|---|
| `SP-INL-1` | A research question of an executor inside its declared edit regions, or with at most 3 files needed, is answered by the asker itself (recorded as inline); never an open question of a DESIGN, whose architect's turn has ended |
| `SP-INL-2` | A question whose digest was already answered in the run reuses that answer (recorded as reuse) |
| `SP-INL-3` | Compile, the commit and the worktree, the verification command, the assembly of the pre-merge record from the verdict, the Linear summary, accept and integrate are code steps of the run, never agents |
| `SP-INL-4` | The orchestrator thread is never spawned or woken for an ordinary issue |

## Records

Two append-only tables of the relay store hold the decisions: a spawn-decision row (the run, the path and position of the call, the rule id, the decision point, the role, the decision, the inputs that show the signal and its threshold, the node and the call id, with an outcome row appended when the call ends) and a step-decision row (the node, the tiers, the rule, the evidence ids and the checkpoint a tier step stands on). Only the run process writes them, in one transaction for a batch; an executor, a reviewer or an explorer never opens the store. They are read with `crw flow show` and `crw flow watch` for a run, with `crw flow measure` for a recorded trace, in the GUI Runs page and in the milestone report.

A native spawn from a flow-bound thread leaves a line in the spawn hook's ledger (the pair the caller sent, the pair the hook applied, the decision and its reason, the call id). The run imports those lines in its next batch, reads each receiver thread back, and records a pair that differs from the applied one as a configuration fault, and a spawn with no applied record as an unbound spawn. A standalone native spawn stays in the dispatch ledger only.

## Refusals

There are two kinds, and a recorded `refused` decision is one of them.

The first kind is a refused agent call of the run. It holds the effort, verification and attribution contract of the engine, and no option overrides it: `independence_refused` (a reviewer whose family equals an observed author family), `pair_refused` (a call outside its role pair, or whose thread read back another pair), `missing_step_decision` (an executor of the next tier without a recorded step decision), `policy_refused` (an alias outside the table, or an ambiguous one), `foreign_handle` (a continuation of another run's thread), `schema_not_strict` (a result schema the engine cannot check) and `author_unattributed` (a commit that no recorded executor call produced, found at acceptance).

The second kind is a refused native spawn of a flow-bound thread, decided by the spawn hook from the run's binding: `native_type_refused` (a type other than the explorer), `native_limit_refused` (more than the thread's allowance), `full_history_fork_refused` (a spawn that forks the whole history) and `flow_bound_unavailable` (the binding cannot be trusted, so every spawn of the thread is refused). The `CRW-SUBSPAWN-ALLOWED` grant mints nothing on a flow-bound thread.

`held`, `unknown`, `null_model` and `superseded` are host and engine states, not refusals: they follow the call-state table of [docs/flow/script.md](../../../../../docs/flow/script.md) and are never tier evidence.

## The QA calls of the final gate

The QA evidence of a milestone's final gate is made by calls the run owns, so no native spawn does it. `SP-QA-1` is one executor-role call over the pinned head, in a scratch worktree whose edits are discarded; its thread has a native allowance of 0, so the spawn hook refuses every native spawn of it, and a later turn on the same thread synthesises the results of `SP-QA-2` into the QA receipt. `SP-QA-2` is the two read-only explorer passes (Pass A for the design system and functional integrity, Pass B for visual fidelity and CJK) over the same capture packet, started after `SP-QA-1` returned and run at the same time; this is [crw-qa](../../crw-qa/SKILL.md)'s two-pass rule kept on the flow path. A pass that does not complete is INCONCLUSIVE, and an INCONCLUSIVE pass fails the gate.
