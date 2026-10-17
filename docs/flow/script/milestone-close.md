
## The milestone close

`crw.closeMilestone(ms)` (`crw:lib/close`) closes a milestone over evidence the run makes itself, in this order; the order is what the
close guarantees: the final gate, the crosscheck, then the close. The Go side is `internal/flow/ops/{finalgate,qa,crosscheck,lane_close,audit}.go`,
`internal/flow/reviewadapter` and `internal/flow/audit`.

A new `project-lane` run of the same plan can retry a failed milestone close without rebuilding integrated nodes. Before admitting its build lanes, `crw.reuseIntegrated(milestone, node)` checks the relay's current acceptance and integration observations, the unchanged node slice and approved design, and the earlier run's acceptance and integration records against the current target. A proven node is recorded as landed with its source run, acceptance and batch commit; a pending node still runs its ordinary lane. Missing proof for an integrated node refuses reuse. A reused landing also carries its source run's original review base: a source that re-planned after landing keeps the first base the milestone judged under, which the rerun's gate, crosscheck and close keep as their range base - separately from the latest design revision's own base tip - and the design decisions an issue references must be the same decisions, not only the same ids, or the reuse is refused. The new run makes its own final-gate and crosscheck evidence and audits the original acceptances; the earlier failed gate remains in its original journal. Its crosscheck inherits executor receipts from the source runs rather than treating the reused nodes as having no executor. Repeating the close does not turn a failed check into a pass.

**The final gate.** `crw.finalGate(milestone)` pins the integrated head H (the tip of the integration target now) and judges the
evidence that stands on it: the test evidence, a verification of H for each command of the plan's required set (`crw.verify` with no node;
a plan that names none requires `crw-dev ci local`), and, when a criterion of the milestone has the surface `web`, `tui` or `desktop`,
the QA receipt (`crw.qaReceipt`). Each piece is `fresh` (it stands on H), `missing` (there is none) or `stale` (it stands on another head,
or on an identity git could not resolve: that verifies nothing); the identities are compared with `source.Compare` of
`internal/pabcd/source`, the rule of the native final gate (`source.CommitIdentity` is the identity of a pinned commit), and the
judgement of every piece is in the `crw.finalGate` row (the call row of the gate). The gate is `pass` when every piece is fresh and
passes, `failed` when a fresh piece does not pass (a verification that ran and failed, a receipt that is `REVISE`, `FAIL` or
`INCONCLUSIVE`), and `pending` while a piece is missing or stale. A verification whose record says no test package ran tests
(`testPackagesRun` 0 in verification-record/1, CRW-1328; a record without the member is unknown, never zero) is recorded on its
`crw.verify` row with the warning `no_tests_run`, and the gate lists it in `warnings` and on its evidence; its state is the evidence's as
before. The gate makes no evidence. The close makes it, at H: `crw.verify(null node)` for each
command named in `need_tests`, and the QA calls below when `need_qa`; then the gate is judged again. A verification that cannot run holds
the milestone (`verify_unavailable`). A head that moves while evidence is made is judged again from the new head, at most three rounds.
A gate that failed ends the close with an escalation (`final_gate_test_failed`, `qa_failed`, `qa_inconclusive`) and the crosscheck does not
run; neither does it when the gate cannot be settled (`final_gate_unsettled`). The native final-gate hook (`[CRW-FINAL-GATE]`, goalplan
`finalGate`) and the standalone `crw-qa` skill are unchanged and read none of this.

**The QA evidence.** The QA capture call (SP-QA-1) is one `executor` call with `qa: true` over the head H: a scratch worktree at H, the
matrix and the captures of sections 1 to 4 of the `crw-qa` skill, the edits discarded, no native helper (`QA_CAPTURE`: the captures with
path, kind and what each shows, the scenarios driven, the teardown). A work grade of C2 (the milestone's `Grade`): the same call's
self-review, one more turn on its thread, is the receipt (`QA_RECEIPT`). From C3 (a grade nobody gave is read as C3), the run starts the two
oracle passes itself (SP-QA-2): two read-only `explorer` calls over the same capture packet, run at the same time - Pass A the design and
functional integrity, Pass B the visual fidelity and the CJK precision (`QA_PASS`) - and the synthesis of the two results is the next turn
of the QA call's thread (`QA_RECEIPT`: `PASS`, `REVISE` or `FAIL`). Why the run starts the passes: section 5 of `crw-qa` asks for two
read-only reviews in parallel, a native thread of the flow may spawn one explorer per call (SP-NAT-1), and a call the run owns satisfies
both; the QA overlay tells the call not to start them and the binding line of the call allows no native spawn, so the hook refuses it.
`crw.qaReceipt` judges what the calls answered and writes the receipt: it is `INCONCLUSIVE` when the capture did not complete or lists no
capture, when the tree the capture call ended in is not the clean head the gate pinned (`capture_source_modified`; `capture_source_unknown`
when the engine recorded none), when a relative artifact path cannot be resolved (`capture_root_unknown`, `capture_path_outside`), when a pass did not complete (the synthesis is then not asked for), is not an explorer call or answers for the other pass, or
when the synthesis is not a completed `QA_RECEIPT` of a call that continues the QA call (the journal's `resume_of`: after a resume the continuation runs on a fork of the thread); a `PASS` over a P0 or P1 finding is not a valid answer,
and the receipt is as bad as the worst of the verdicts it rests on. The spawn decisions of the three calls (SP-QA-1, SP-QA-2 twice) are
rows of `flow_spawn_decisions`; the roles and the pairs the threads read back are in the receipt.

The capture call may edit its scratch worktree, so the head it was started at is not what it exercised. When a QA call completes, the
engine asks the backend for the source identity of the directory the call ended in (`engine.SourceReader`; the agent backend runs
`source.Capture` with the run's own writes under `.crw/` left out) and records it on the completed row (`qa_source`, or `qa_source_error`).
`crw.qaReceipt` compares it with the clean identity of the pinned head and keeps it in the receipt (`source`); a PASS stands only over the
head the gate pinned, and the gate compares the identity of a fresh receipt again. The artifacts of the capture are files of its own
worktree, named relative to it (`.crw/evidence/<session>/qa/...`) while the passes and the self-review read checkouts of their own, so
`crw.qaCapture` resolves each path against the directory the capture ran in (the call-private worktree persists with the run) and the
packets of the passes and of the self-review name that path. The QA mark is part of the input digest of a call: the same call with another
mark is another call on a resume.

**The crosscheck.** After a passed gate, `crw.crosscheck(milestone, gate, opts)` (CRW-1473) reviews the integrated range of the gate
(`base..head`) in one round of two passes: pass A judges coverage (every criterion of the milestone and every decision of the approved
DESIGN, met or not, with its evidence), pass B hunts bugs. The script builds the text of each pass (`crosscheckPacket`: the range, the
criteria, the decisions, which live in the journal's ledger row and not in the checkout, the lens of the pass and the close rule word for
word), and the op runs both passes itself, in Go, through the review adapter (`reviewadapter.Adapter` over the engine's command line,
answering `reviewadapter.AnswerSchema`): no agent call, no reviewer thread and no spawn decision.
The op appends the EXECUTOR RECEIPTS section to each prompt: for every node of the milestone it reads the newest completed `crw.unitCheck`
row of role `unit-check` in the run's journal and its executor receipt; a node the run reused (`crw.reuseIntegrated`, which has no unit
check of this run) reads instead the newest unit check of the journal the reuse record names (`source_run`) that matches the node and its
accepted head (an `accepted` copy is read and its sha256 checked; `not_assigned`, and `no_executor` for a node not reused with no unit
check in the run, are listed and block nothing), writes the receipt bytes to
`<run dir>/crosscheck/<milestone>-r<round>-receipts.md`, outside both checkouts - a reused node's block naming its source run, unit-check
call and acceptance call - and names that file, its sha256 and each node's state and digest, so the engine checks each criterion's
red-first and mutation-red lines against the receipts. A receipt that cannot be handed over holds the round before any pass
(`crosscheck_receipt_missing`, also when a reused node's source journal cannot be read or its unit check or accepted copy is missing;
`crosscheck_receipt_unverified`; `crosscheck_receipt_not_copied`, also when a reused node's accepted receipt names no copy - the original
path is never re-read; `crosscheck_receipt_mismatch`, the row naming the node). The prompt carries refs, never diff bytes: one over `CrosscheckPromptLimit`
(120 KiB, since the engine takes it as one command-line word) is not sent (`crosscheck_prompt_too_large`). The first round is round 1;
the close's correction is followed by round 2 with the prior verdict. A prior verdict or round that does not describe that continuation
is refused `crosscheck_prior_invalid`.

**The run directory and the checkouts.** Every file the crosscheck writes lies under the run's directory (`Deps.LandRoot`,
`<state>/flow/<run>` in the run process), which must be an absolute path: an empty or relative one fails the round before anything is
written or checked out (`crosscheck_no_run_dir`). Each pass runs in a checkout of its own of the gate head,
`<run dir>/crosscheck-wt/<milestone>-r<round>-e<evidence round>-<pass><attempt>-<head12>`, made with `git worktree add --detach` in the
run's repository, and both checkouts of a round are made and checked before either pass starts. A checkout is used only while its `HEAD`
is the head and its working files are the head's tree (`ops.Snapshot` equal to `ops.TreeOf` of the head), a fresh one as much as one a
resume finds again: one at another commit fails the round `crosscheck_worktree_moved`, one with a modified, deleted or untracked file
`crosscheck_worktree_dirty`; no pass runs, and the directory is kept as it is for the child to read. A checkout a pass ran in is removed
after the pass (`git worktree remove --force`, then `git worktree prune`; a path outside `<run dir>/crosscheck-wt/` is never removed). An
ignored file is outside both trees and is not judged: the engines read the tracked tree.

**The engine.** The lane ships two engine profiles, tried in this order: astra (`codex exec -m gpt-6-astra`, family gpt, answering under a
strict form of the answer schema the op writes to `<run dir>/crosscheck/answer.strict.json`) and grok (`grok -m grok-4.7`, family grok),
each pass with a limit of 40 minutes. The profile order is the parent's decision; a round runs the first configured profile regardless of author family. If author model information is
undecided, it is journaled under `author_warnings` and does not block. When no profile is configured, the round is refused `crosscheck_no_engine`.
The old `crosscheck_author_undecided` and `crosscheck_no_independent_engine` constants remain readable as legacy reasons. The crosscheck is made in a separate tool invocation; tool reviews have no agent thread id. If a same-thread review is actually observed on an agent review path, it is
warning-only and recorded, and the run continues. Model family remains provenance. The authors are read from the run's own pairs for
attribution, never to select or exclude an engine. Round 1 follows configured profile order:

| Authors of the milestone | Round 1 |
| --- | --- |
| any author family, including mixed or unknown | astra (gpt), when available |
| first profile unavailable | grok (grok), when available |

**The verdict.** The two passes run together, each one check of the review adapter; an `invalid` answer is asked once more. A pass that
read no repository is void (`not_read`: under the floors of its engine profile on the commands it ran that succeeded, the tokens it read and the time it
took, CRW-1504): when both passes are void the round gives no verdict at once (`no_verdict`, `crosscheck_no_verdict`); when one is, that
pass alone runs once more on a fresh checkout (attempt 2; the first attempt stays in V's passes as `voided`), and a second void attempt
gives no verdict too. A pass that failed (`worktree_changed`, `tool_failed`, `invalid` after its retry, or a state the op has no rule for,
`crosscheck_pass_<state>`) fails the round, and one that gave no judgment (`quota`, `no_answer`) holds it. The findings of the two passes
are merged into one list (`reviewadapter.MergeFindings`: one finding for each id, the first copy's file, line and text, the higher
severity, a blocking impact over a non-blocking or empty one), and one rule, `ops.CrosscheckRule`, makes the verdict V; the packets of
both passes say it word for word:

> The impact of each finding is one of core_function, safety_or_data, permission_boundary, criterion_unmet, regression_introduced, minor_separable: a finding of P0 to P2 blocks whatever its impact, and of the impacts only minor_separable does not block. The milestone closes only on a verified crosscheck: verified means no finding blocks; a finding that does not block is carried as a follow-up.

A finding that blocks (`premerge.Blocks`) makes V `needs_changes`; the others are carried, by id (`carried`) and as one line each
(`follow_ups`: `id [severity/impact] file:line text`), and no issue is registered for them. The engine's word follows the same rule: an
answer of verified over a finding that blocks is invalid, not overridden. V is `verified` only when both passes answered verified too: a
pass whose own answer is needs_changes makes V `needs_changes` even when no finding blocks (each attempt's word is its pass's `verdict`). V (row `crw.crosscheck`, [journal.md](../journal.md#the-proofs))
names the word, the findings, the engine family and model, the round, the milestone and its plan, the gate's head and base, the run, the
author families and each author with its source, name, model and family, the receipts and every attempt of each pass.

**The close.** Only a V of `verified` lets the milestone close: `crw.milestoneClose(milestone, {gate, crosscheck: V})` records the tip it
judged, and refuses, recording no close, evidence whose crosscheck is not a verified V of the gate's milestone and head (and, for a journal
of a close made before the crosscheck, evidence with no crosscheck whose `final` member, the old close's `crw.reviewerFinal` review,
is not verified, or evidence with neither):
`close_unverified`. When the target is no longer the head the gate judged, the close is judged again from the new head, at most three
times.

**The close's correction.** A first `needs_changes` verdict gets one correction before the close stops. When every finding is
`evidence_missing`, the run asks the explorer and rechecks the same head with its answers. A mixed set, including a `test_gap` and
`evidence_missing`, goes to one close-fix executor. If that executor returns `needs_research`, the run uses the existing research route:
the explorer answers the question and `answerPacket` resumes the same executor handle, retaining its worktree edits. The existing
research limit still applies; an unavailable explorer holds the close, and an exhausted research limit escalates `blocked_scope`.
Once the executor is ready, the run commits the correction, repeats the final gate and crosscheck at the repaired head, and publishes
the local repair only when both pass. A research request alone never discards the correction or becomes `close_fix_research`.

A crosscheck that still does not verify the range keeps the milestone open, and the close stops:

- `needs_changes`: the run ends escalated `crosscheck_pending`, ask `crosscheck_pending`, with V in the escalation's evidence and in the
  run's value (`crosscheck`, V as the op recorded it) and the fix packet (`fix`, `crosscheckFixPacket`: the blocking findings by id, the
  range as `git diff <base>..<head> -- <files>`, the milestone's criteria and the route of the correction). Every node of the milestone has
  landed, so the route is a correction node of a new plan revision: a correction issue of the milestone naming the finding ids
  (`crw-add-issue`, a plan revision), fixed by a new executor subagent on a branch from the integration tip, landed with
  `crw flow start land-head`, and the verdict and the landed head reported up. The child hands V and the fix packet up to the parent
  (repository session) with `crw flow report-up`; the run makes no further close-fix executor call for it. The escalation's id is of the run, the call and
  V's canonical digest: a resume that judges the range again (the target moved, or a proof error ran the gate again at the same head)
  and gives another V is a new escalation with the new V, the current question of the milestone, told again, while the earlier
  escalation keeps its V; the same V again is the same escalation.
- `refused` (`crosscheck_no_engine`, `crosscheck_prior_invalid`): `crosscheck_refused`, ask `configuration`, a question for the parent.
  Historical `crosscheck_author_undecided` and `crosscheck_no_independent_engine` remain readable; author model uncertainty is recorded in `author_warnings` and does not block.
- `failed` (`crosscheck_no_run_dir`, `crosscheck_worktree_dirty`, `crosscheck_worktree_moved`, `crosscheck_prompt_too_large`, a pass that
  failed): `crosscheck_failed`, ask `crosscheck_failed`; `no_verdict`: `crosscheck_no_verdict`, ask `crosscheck_no_verdict`. Both go up to
  the parent.
- `held`: the milestone is held (`crw.hold` with the round's `hold`), and a resume runs the round again once the cause is gone.

A closed milestone's result carries V as `crosscheck`, and its report names the gate, the crosscheck and the reflection's alignment
([reporting.md](../reporting.md)). What the alignment of the range does is in
[The architect's continuity and the re-plan](design-replan.md#the-architects-continuity-and-the-re-plan).

**The audit.** `auditAcceptances(ms)` of `crw:lib/close` runs `crw.audit` and archives nothing; `reportClose` runs it
before `crw.reportUp`, which reports the audit with the close. An acceptance on a unit check (basis `unit_check`) rests on no reviewer
thread: its audit vouches for the `crw.unitCheck` row it names (completed, role `unit-check`, result state `ok`, a verified verdict, of
the node, on the head, under the criteria digest of the node's release) and reads nothing from the host. For an acceptance of an old
run on a review, the audit reads the reviewer call's persistent thread from the host (`thread/turns/list` with `itemsView` full, and
`thread/items/list` for a turn the list returned in summary) and checks that the journal recorded the thread, a turn id and the result
digest; that the turn is `completed` on the host; that the digest of the last `agentMessage` is the result digest of the journal; that the
packet the thread was sent names the head and the criteria digest of the acceptance (the `HEAD:` and `CRITERIA DIGEST:` lines of the
reviewer packet); and that the pair of the executors that wrote the head was read back (the calls the recorded commits of the node name; an executor attempt that
never created a thread or never completed wrote nothing, and a retry that replaced it is judged by its own pair). A reviewer call that continues
another on its thread records that thread on its prepared row and creates none, so the thread is the continued call's; its packet is the first
turn of the call that started the thread. The result of an acceptance is `ok`, `mismatch`
(the record and the host differ, or the record is incomplete) or `unobserved` (the host does not list the turn, does not return its items,
does not answer, or the executor's pair was never read back); unobserved is not a pass. The results are a row `crw.audit` in the journal.
A mismatch is escalated (`audit_mismatch`). The `crw.archive` op stays registered for the records of old runs and no shipped script
calls it: it archives the reviewer thread of an acceptance only when the newest audit row about it says `ok`, reading the rows and not
the script; a thread already archived is not archived again, and a host that refuses keeps the thread. `crw flow audit` is the same
audit on demand.

**`crw flow audit`.** `crw flow audit (--run RUN | --acceptance ID) [--milestone M] [--state DIR] [--socket PATH] [--json] [--no-record]`
opens the relay store the journal of the run is in (an acceptance given alone is looked up in the store), connects to the App Server
control socket, and prints the result of each acceptance. It exits 0 when every acceptance is `ok`, 1 when one is a `mismatch` or is
`unobserved`, and 2 for a command line, a store or a run it cannot use. It appends its result to the journal as a `crw.audit` row with a
call id of its own (`audit_...`), unless `--no-record` is given or the run has not ended: the journal has no `finished` or `run/paused` row
after its last `started` row (a run that is still writing owns its journal, and so does a resumed process after an earlier process's end).
The row is appended under the run lock, taken without waiting after the host reads, on the journal opened again under it, where the
end is read again: while a process holds the run (one that wrote its end row and still reconciles what goes up) nothing is recorded.

**The scenario harness** (`internal/flow/scenario`) runs this script on the engine over the fake App Server host, a temporary relay
store (the journal and the decision rows), a temporary git repository with an integration branch, a scripted model, a fake relay and
a fake verifier. Its scenarios are the tests of the project lane named in CRW-1201; `go test ./internal/flow/scenario/` is the
command.
