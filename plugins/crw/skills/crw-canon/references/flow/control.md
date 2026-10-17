
## Who owns control in a flow-managed run

A flow-managed node is an issue node that the child's run released to an executor. There the run is the one process that knows what happens next; no thread it starts holds that knowledge. A head the child made outside a run is landed by a `land-head` run, which releases the node to itself and holds the gates below (verification, an independent review, acceptance and integration) as a project lane does; the thread does not land it another way.

| Control | Owner | What it means |
|---|---|---|
| Phases and their order | the run: its script and its journal | Build, unit check, commit, verify, land and close follow the workflow the run was started with. An executor builds and returns its receipt; it never decides the next phase. A resume goes on from the journal |
| Audit | the run | The run calls the critic in a separate review execution for the critic's design check (`crw.designAudit`), and runs the unit check of each node's work itself (`crw.unitCheck`, a separately invoked review tool: Codex review by default, Bugbot on a Codex quota; no reviewer agent or agent thread id), and `crw flow audit` checks each acceptance against the unit-check row it rests on, or, for an acceptance of an old run on a review, reads that review back from the reviewer thread on the host. Model family is provenance only; an actually observed same-thread review is warning-only, recorded, and the run continues |
| Evidence | the run | A verification record, a verdict, a pre-merge record or a QA receipt is made from calls the run owns and commands the run itself runs. What an executor writes about its own work, a skill-loading line or a quoted instruction is no evidence |
| The step exchange of a session run | the run publishes each step; the child spawns its role subagent and reports it | `crw flow next` hands the child the steps and `crw flow dispatched` reports the subagent each was spawned as. The run binds the subagent after it reads it on the host, reads its answer from the host and records it; the child never copies a subagent's answer, edits no node's directory and spawns a step's subagent only for a step it was handed |
| Escalations and decisions | the child; Jun for what only Jun takes | The run raises an escalation and waits; `crw flow answer` applies the answer. The thread does not release, accept or integrate a node the run owns |
| Acceptance and integration | the run, through the relay's DAG scheduler | The run holds the plan's coordinator epoch, so a transition written by another hand is refused by the epoch fence (`stale_coordinator_epoch`) and raised as an escalation, and the run does not take it over |
| The PABCD state machine | nobody in a flow-managed node | The executor and an oversized child start no PABCD state machine. The standalone PABCD rules are unchanged: a task that is not a flow-managed node keeps its own phases |

### The high-risk procedure of a C4 issue

An issue that [crw-dev](../../../crw-dev/SKILL.md) classifies as C4 runs in the flow in this order. The numbered steps are the run's own lane; none is a thread's choice.

1. One background explorer for each node of the milestone, at the design head, then the architect DESIGN of the milestone over their findings.
2. The critic's design check of the DESIGN (`crw.designAudit`), pre-build: the critic rules on it in a separate review execution before any executor starts. An observed same-thread review is recorded in optional `warnings: [review_same_thread]`, with `critic_thread` and `architect_thread` in the journal; it does not block. `needs_changes` returns to the architect on its own thread, and a replan is audited again. Only an approved DESIGN reaches an executor.
3. The unit check of each node: the run checks the executor's uncommitted work with a separately invoked review tool (Codex review by default, Bugbot on a Codex quota) before the run commits it (`crw.unitCheck`), with at most two fix round trips on the same executor thread; the committed head then needs its passing verification. The tool review has no agent thread id.
4. After the issues are integrated, the final gate with full verification and the risk record, then the crosscheck over the integrated range: two passes (coverage and a bug hunt) of a review engine in a separate tool invocation, run by the run itself with no reviewer agent or agent thread id; the milestone closes only on a verified crosscheck. If a same-thread review is actually observed on an agent path, it is warning-only and recorded; the run continues. Model family is provenance only.
5. The acceptance audit of the closed milestone (`crw.audit`): each acceptance is checked against the unit-check row it rests on, and a mismatch goes up as an `audit_mismatch` escalation.
6. The report of the milestone (`crw.reportUp`), with the audit, the close's gate, its crosscheck and the architect's reflection.

The crosscheck does not replace the critic's design check: it rules on a range that is already built, and step 2 exists to stop a flawed design before any executor writes it. Neither does it replace step 3.

### Milestone close and delivery

The close crosscheck judges code and its evidence before publication. Mark criteria about delivery in the handoff's milestone entry
with `delivery_criteria`, referring to their original ids. For example,
`"delivery_criteria": [{"id":"c1","checks":[{"kind":"remote_push"},{"kind":"issues_closed"}]}]` assigns all of `c1` to the later delivery
check. Split a criterion that mixes functional behavior and delivery before marking it. The remaining criteria keep their ids; the
two crosscheck passes still review the integrated range even if every criterion is deferred.

Compile and start warn and record valid delivery annotations. Suspected delivery wording without an annotation produces a warning
and stays in the build crosscheck; prose matching never exempts a requirement. Invalid annotations warn and stay in the build
crosscheck too. These warnings do not refuse the handoff.

A first crosscheck finding gets one close correction. All-evidence findings go to the explorer; findings that include a code or test
defect go to a close-fix executor. Its `needs_research` follows the existing research route, and the answer returns on the same executor
handle with its edits preserved. The run commits the completed correction and repeats the gate and crosscheck. The research limit,
host holds and unresolved-finding escalations still apply.

When delivery criteria exist, the closed run carries `delivery.state: pending` with its head and criterion ids. After the authorized
publication and tracking updates, the child runs `crw flow audit --run RUN --delivery --json`. This observes all the declared checks
of the run's closed milestone: `remote_push` requires the origin branch from the pinned handoff to contain the verified head;
`issues_closed` requires every node's issue to be closed as completed; `milestone_closed` requires the explicit tracker
`ref` (for example `{"kind":"milestone_closed","ref":"acme/app#6"}`) to be closed. It verifies the completed close and its gate and
crosscheck before reading delivery state. An unavailable API or unproven remote ancestry is `unobserved`, never passed.

The command records a `delivery_checked` run event with the proof head and observations and exits 0 only when all checks pass
(`--no-record` prints without recording). Pending or unobserved checks exit 1; an unusable run or invocation exits 2. The command
performs readback, while publication remains with its existing owner and authority. Report the deferred criteria as fulfilled only
after the readback passes; the code close alone leaves them pending.
