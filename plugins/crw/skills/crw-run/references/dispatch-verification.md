# Dispatch verification

A dispatch can fail in four different places, and the four are routinely read as one.
This reference holds the judgment cases that keep them apart: which fact establishes
which verdict, what the recorded regression cases already decided, and what a missing
CXC Loop looks like when it is real rather than assumed.
[Prepare and dispatch](../SKILL.md#prepare-and-dispatch) owns the dispatch itself and
[Observe and verify](../SKILL.md#observe-and-verify) owns the evidence table these cases
apply. Loop mechanics belong to the child's own `cxc-loop`/`cxc-pabcd`; nothing here
restates them, and nothing here adds a preparation turn before the work starts.

## Four verdicts, judged separately

| Verdict | Established by | Not established by |
|---|---|---|
| Instruction existence | The text of THIS dispatch, read back from the child's own accepted turn and the assignment message matching it | A packet draft, a template, the coordinator's intent, the bootstrap and context messages a session opens with, or an earlier launch's prompt on a task being reused |
| Settings request | The real arguments of the call that carried this dispatch | A sentence in the prompt naming a model or a workflow |
| Settings observed on the task | The receipt's returned model, effort, working directory and permission profile, at the scope the receipt itself claims | A catalog entry, the request quoted back, or a conclusion about what the turn then ran on |
| Loop execution | The child's own session binding, active goal, and current goalplan/FSM state | Opus as the served model, an active native goal, or the skill named in the prompt |

Which dispatch is being judged decides which evidence answers. For a creation it is the
assignment message and the creation arguments and receipt. For a reused task it is the
correction or resume message and that mutation's own arguments and receipt: the original
launch still shows whatever it carried, so reading it would pass a correction that dropped
the workflow.

Find the assignment message by the dispatch's own marker or receipt rather than by
position. A session does not open with its assignment: the bootstrap and context messages
a host injects arrive first and several of them carry the user role, so "the first user
message" names one of those and would read every normally dispatched child as carrying no
instruction at all.

Third-column evidence is read at the scope its own transport claims, which for a settings
receipt is an observation at creation or at resume rather than a guarantee about the turn
that follows. What a turn actually ran under is shown by that turn's own record on the
task, and where a claim needs that, the receipt does not supply it.

Each verdict can hold while the next one fails, which is why a single pass/fail for
"the child got its assignment" hides the defect rather than finding it. A packet that
names the Loop, a creation call that requested it, a receipt that returned the right
model, and a child that never initialized a goalplan are one consistent observation of
a failed dispatch, and only the fourth column is wrong.

## What a missing Loop actually is

Separate these seven outcomes before calling anything a defect. They need different
repairs, and two of them are not defects: L5 is the working case and L6 is an unknown
rather than a finding. The authorized exceptions are not classes at all and are held by
the contrast cases below.

| Class | What happened | Distinguishing evidence |
|---|---|---|
| L0 | The invocation was never in the assignment | The assignment message for this dispatch, selected by its marker or receipt and read from that task, carries no installed-skill invocation and no agreed alternative |
| L1 | The invocation was sent, the skill was never loaded or never invoked | Assignment carries it; no session binding, no orchestration entry, no goalplan |
| L2 | It was invoked, but own-session binding or goalplan/FSM initialization failed | Binding or orchestration attempted and refused, or bound to the wrong session; the refusal itself is the evidence |
| L3 | A native goal is active and no CXC Loop exists | Host goal present; no bound goalplan and no persisted phase transitions |
| L4 | Activation succeeded, then was lost on resume or compaction | Earlier goalplan/FSM evidence exists; the later turn runs without it, or against a second goal for the same assignment |
| L5 | Running correctly | The child's own bound goalplan identifier, current phase, and persisted transitions for this assignment |
| L6 | Insufficient evidence | Nothing readable yet distinguishes the classes above |

L3 is the one most often misread, in both directions. A native goal is what the host
schedules on; a goalplan and its FSM are what the Loop persists. A parent coordinating
a project holds the first without the second by design, and that is its normal state,
not an L1 child. An implementation child that holds only the first has an unarmed loop
even though every status surface looks active.

Recovery differs by class and none of these is repaired by sending the assignment again
as if it were new. L0 and L1 are supplied on the same task, adding only the instruction
that was missing. L2 is a capability or binding failure reported with the exact refusal.
L4 continues the goal and goalplan that already exist, because opening a second goal for
one assignment reads from outside as a duplicate rather than a resume; where activation
never succeeded there is nothing to continue and starting it then is the activation that
was owed. See [Launch packet](task-packet.md#launch-packet) for what travels with a
correction and [Restoration block](task-packet.md#restoration-block) for a resume.

## Evidence rules that prevent a false verdict

A single reading of activation state is L6, not L1. A child still inside its first turn
reads exactly like a child that never armed anything, and the two only separate later.
This was measured rather than assumed. In one batch of four implementation children, a
reading taken about two minutes after dispatch found three of them with no bound goalplan;
all three had in fact armed by roughly two and a half minutes in, and all four went on to
run with persisted phases. Had that reading been the verdict it would have recorded three
false L1s. Pair an early reading with a later one, or read the child's own report, before
assigning a class. The per-child timings behind this are operational evidence and stay in
the private task record.

Evidence that cannot change afterwards does not need that patience. A readback of the
dispatched prompt showing neither an invocation nor a named alternative establishes L0 on
its own, since no later state puts an invocation into a prompt already sent, and a
recorded refusal establishes L2 the same way. The caution above is about negative state
readings, not about definitive prompt evidence or a refusal that was actually observed.

Arming telemetry is not the execution verdict in either direction. In the same sample one
child recorded the arming pointer as seen while holding no goalplan yet, and an earlier
child recorded it as unseen while holding a bound goalplan and slug. What the host noticed
about the prompt and what the child actually persisted are two facts.

Model identity is not Loop evidence. A child may be on the defaulted model and effort and
be running no loop at all; a child on an explicitly different model may be running one.

A phrase check is not any of these verdicts. Grep over a prompt finds instruction
existence at best, and only in the copy that was actually dispatched; it finds nothing
about application or execution. Judge L5 from the goal and goalplan identifiers, current
phase and completion evidence the child returns, which
[Launch packet](task-packet.md#launch-packet) already requires it to report.

## The record form of that separation

The four verdicts and the seven classes above are read by a person. The same separation has a
record form in `relay-packet/1`, which carries activation as three fields rather than one:
whether the invocation was in the assignment, whether the child's own binding and goalplan
answer, and whether a host goal is active. Each names the record that answered it, and an
answer with no record behind it is `unverified` rather than absent - which is the same
caution this file states above, that a single negative reading is L6 and not L1.

The mode decides which of the three can answer at all. A coordination parent holds a native
goal and persists no implementation FSM by design, and an authorized non-Loop assignment arms
neither, so both answer `not_applicable` for the activation field and classify as L5. That
is what stops the L3 misreading in the second direction: reading a parent's normal state as
an unarmed child.

The classes are derived in the order this file gives them. What cannot change afterwards is
read first - a prompt with no invocation is L0 whatever happens later, and a recorded refusal
is L2 the same way - and the negative readings are consulted only after that, with L6 as the
answer when nothing distinguishes them yet. The details live with the relay, in its own
`docs/relay/packets.md`, and are read there when changing the relay rather than when running an
assignment.

## Recorded cases

The nineteen cases below were decided against the JUN-99 delivery at
`d91164d96b08c9d243786b9455989f87022ebc31`, on an earlier tree where these files sat
under different skill names and paths. Each owning anchor was re-read individually against
this tree after the skill rename and the move under `plugins/crw/skills/`, rather than
derived by rewriting the path prefix. Seventeen
verdicts stand on sentences that did not change; two, S2 and S14, sit on sentences whose
wording was revised while the verdict held. None needed a new run to re-establish it, so
they are reused at that grain: reuse a case whose owner still reads the same, and re-read
a case whose owner has moved or been reworded before relying on it.

| Case | What it decides | Current owner | Criterion | Status at this revision |
|---|---|---|---|---|
| S1 | A concrete new-task plan accepted with "진행해" executes within that scope, with no creation keyword demanded | [Determine the requested operation](../SKILL.md#determine-the-requested-operation) invocation table | Approval context | unchanged |
| S2 | An approved whole-scope run continues into its next ready batch, including after compaction | Same table, resume row | Approval context | revised wording, verdict holds |
| S3 | Clear delegation where independent tasks are the established workflow reuses first and creates within scope | Same table, delegation row | Approval context | unchanged |
| S4 | A standalone short invocation with no creation intent prepares the packet and asks only for the missing decision | Same table, short-invocation row | Preserved limits | unchanged |
| S5 | A skill name, an unsubmitted UI default, or a quoted example authorizes nothing | The paragraph following that table | Preserved limits | unchanged |
| S6 | A busy task or an uncertain send is reconciled rather than duplicated | [Independent implementation tasks](../SKILL.md#independent-implementation-tasks) and [Bridge launch and recovery](bridge.md) | Recovery | unchanged |
| S7 | Permission for an unrelated earlier task does not travel to this scope | The paragraph following that table | Preserved limits | unchanged |
| S8 | Explicit current-task, read-only, status-only and no-create limits win | Same paragraph, with precedence in [Default independent execution](../../crw-plan/references/integrations.md#default-independent-execution) | Preserved limits | unchanged |
| S9 | With no explicit choice the child runs `anthropic/claude-sonnet-5-5` at `xhigh` with CXC Loop | [Default independent execution](../../crw-plan/references/integrations.md#default-independent-execution) | Default settings | criterion revised 2026-09-29 (child pair moved from `anthropic/claude-opus-5-5`); the owning sentence reads the pair from the declared policy and did not change, so the verdict holds |
| S10 | A named-model correction steers this dispatch and does not change global or role configuration | [Keep a project run moving](../SKILL.md#keep-a-project-run-moving) and the packet's permissions line | Default settings | unchanged |
| S11 | A standing Loop choice yields to a later read-only limit | Precedence list, with the packet's non-Loop branch | Preserved limits | unchanged |
| S12 | A later instruction supersedes only the same constraint, so an unrelated limit survives | Same section, precedence sentence | Preserved limits | unchanged |
| S13 | A live child at the wrong effort is corrected on that same task, with stable IDs and preserved progress | Same section's correction bullets and [Prepare and dispatch](../SKILL.md#prepare-and-dispatch) | Recovery | unchanged |
| S14 | The full work prompt is dispatched once; there is no readiness handshake before the body | [Prepare and dispatch](../SKILL.md#prepare-and-dispatch) and [First full assignment required fields](task-packet.md#first-full-assignment-required-fields) | No preparation turn | revised wording, verdict holds |
| S15 | A status question during an active run wakes nothing and cancels nothing | [Determine the requested operation](../SKILL.md#determine-the-requested-operation) | Preserved limits | unchanged |
| S16 | An explicit non-Loop or no-goal alternative omits the invocation and names the agreed workflow | [Default independent execution](../../crw-plan/references/integrations.md#default-independent-execution) and the packet's non-Loop branch | Default settings | unchanged |
| S16b | Where Loop is effective, the packet carries the literal installed-skill invocation, not only a workflow label | [Launch packet](task-packet.md#launch-packet) Loop branch | Default settings | unchanged |
| S17 | A setting the creation path cannot apply is settled before the task exists, never silently downgraded | [Default independent execution](../../crw-plan/references/integrations.md#default-independent-execution) settings bullet | Recovery | unchanged |
| S18 | A mismatch found after creation is reconciled on that same task | Same bullet list | Recovery | unchanged |
| S19 | A child writes its messages, commits, pull request text and receipts in English, while task titles, Linear records and reports to the user stay Korean | [Default independent execution](../../crw-plan/references/integrations.md#default-independent-execution) language paragraph and the packets' `Language:` line | Default settings | added 2026-10-02; partly measured in P-CRW-115, inspected artifacts held, see below |
| S20 | A relay-managed child that finishes on a goal-continuation or post-restart turn attaches a continuation claim naming the generation's anchor, read from the relay, and does not read the `unassigned_turn` refusal as a delivery defect; a turn of another task stays refused whatever it claims | [Launch packet](task-packet.md#launch-packet) relay bullet on which turn the receipt is emitted from, and [Completing on a later turn of the same child](relay.md#completing-on-a-later-turn-of-the-same-child) | Recovery | added 2026-10-02; did not occur in the receipts read in P-CRW-115, see below |
| S21 | An implementation issue the size check answers `split_recommended` for is not assigned and is reported with its draft; an exception the user explicitly approved passes with its record; `ok` changes nothing | [Check the size before dispatch](../SKILL.md#check-the-size-before-dispatch) and [Check the size of an issue](../../crw-plan/references/issue-boundaries.md#check-the-size-of-an-issue) | Preserved limits | added with CRW-273; the command is tested and was replayed over the recorded issues, no run has had a parent apply the step yet |
| S22 | A candidate whose only block is BEHIND is updated by the parent itself with a guarded update-branch call, then proved again on the new head (the base-refresh check, every required job, the child's threads; the `Devin Review` status is not a gate) before an expected-head merge; in a DAG-managed project the update comes before `dag-accept` | [Refresh the base yourself when only the base moved](merge-readiness.md#refresh-the-base-yourself-when-only-the-base-moved) | Recovery | added 2026-10-03; basis below |
| S22b | What still goes back to the child: a conflict the parent does not settle by a declared mechanical rule (the update did not happen, so the correction names the head the branch is at and has the child sync first when an earlier refresh already moved it); on the new head, a refusal from the check, a job that failed again after its one rerun, a new thread the parent has not dispositioned for the new head, or a blocking finding (the correction names the new head and has the child fast-forward its worktree first); and a base that moves after `dag-accept`. A job still pending is waited on, not returned | Same section, and [Restoration block](task-packet.md#restoration-block) | Recovery | added 2026-10-03; same basis, see below |
| S22c | The parent refreshes only the candidate about to merge: a landing never starts a refresh of the remaining pull requests, and a candidate that is not next waits with the head its child reported | [Refresh the base yourself when only the base moved](merge-readiness.md#refresh-the-base-yourself-when-only-the-base-moved) | Recovery | added 2026-10-03; basis below |
| S22d | A refreshed head is accepted only as a tree identity: one merge commit with the parents (previous head, base tip) in that order whose tree is what git merges from them; a chain is proved one step at a time; the check's `evidence:` line (previous head, dev tip, new head, tree OID, rule) is copied into the merged mark and the merge record | Same section | Recovery | added 2026-10-03; basis below |
| S22e | A required job that fails on a refreshed head is rerun once on the same SHA before the candidate goes back (in a DAG-managed project the scheduler's `retry_same_sha`, answered after `dag-accept`; in a project without a plan, by hand); each merge leaves a lane entry with its service time, CI runs, conflict episodes and `dev_after` | Same section | Recovery | added 2026-10-03; basis below |
| S22f | A conflict whose every file lies in a place declared `mechanical` is settled by the parent by that place's rule and proved with `crw skill base-refresh mechanical` before anything is pushed: the head must differ from git's clean three-way result only inside those places, a union must keep every line of both sides, and a regeneration command run twice on a checkout of the head must leave the head's files as they are. `renumber` (no check yet), a place no declaration covers and any refusal go back to the child, and the merged mark names the rule applied and carries the check's output | [Resolve a mechanical conflict yourself](merge-readiness.md#resolve-a-mechanical-conflict-yourself) | Recovery | added 2026-10-03; the command is tested on real temporary repositories and was run once end to end with the real record command on a throwaway clone, no parent has applied the step in a run yet |
| S23 | The packet's `Title:` names the Codex task, and the child titles its own pull request in English under the target repository's rules | [Child task titles](task-packet.md#child-task-titles) and the `Title:` line of the [Launch packet](task-packet.md#launch-packet) | Default settings | added 2026-10-03; measured on published pull request titles, not on a child that read the rule, see below |
| S23b | A temporary directory the packet names is short enough for a Unix socket under it to bind, and the packet states the socket path limit | The `Capacity and large artifacts:` line of the [Launch packet](task-packet.md#launch-packet) and [First full assignment required fields](task-packet.md#first-full-assignment-required-fields) | Default settings | added 2026-10-03; limit measured on this Linux host and read for macOS, see below |
| S23c | A make target, CI check or repository script a packet names under `Verification:` exists in the target repository, and the packet's writer confirmed it when writing the packet | The `Verification:` line of the [Launch packet](task-packet.md#launch-packet) and [First full assignment required fields](task-packet.md#first-full-assignment-required-fields) | Default settings | added 2026-10-03; failure mode measured at dev 53c7e4e8, see below |
| S24 | A High finding of the child's own internal review that the child rejects, whether it rebutted it or put it outside the issue, is not closed by the child: the handoff lists it as a decision request, or the child asks first and ends its turn `blocked_needs_input`, and the parent's verdict answers each request (rebuttal confirmed, minor residue accepted with its five records, or a fix) before it is `verified`; a rejected High in neither place sends the handoff back | [What a handoff discloses](task-packet.md#what-a-handoff-discloses) (`internalReview`, `decisionRequests`) and [What the handoff discloses, checked at the verdict](merge-readiness.md#what-the-handoff-discloses-checked-at-the-verdict) | Handoff | added 2026-10-03; basis below |
| S24b | The child's independent review ends on the head the pull request is opened from and the hosted review follows on the open pull request: the handoff names the reviewed head and lists the commits after it, and the parent checks that every first-parent commit between the two is a listed merge of the base or a listed later commit, and where there is no reviewed head the merges after the assignment baseline are compared with the listed refreshes. The rule is about the order of the two reviews and does not make one run | The [Launch packet](task-packet.md#launch-packet) `Execution:` bullets, [What a handoff discloses](task-packet.md#what-a-handoff-discloses) (`internalReview`) and the commit accounting in [the verdict checks](merge-readiness.md#what-the-handoff-discloses-checked-at-the-verdict) | Handoff | added 2026-10-03; same basis, see below |
| S24c | A merge of the base with no conflict and no hand-resolved hunk is a `clean` refresh by tree identity (the tree of `git merge-tree --write-tree <previous> <merged>` is the merge's own tree, which is what `crw skill base-refresh check` measures): it is not new scope and reruns only the gates, and the parent re-checks the claim with the helper | [What a handoff discloses](task-packet.md#what-a-handoff-discloses) (`baseRefresh`) and [Refresh the base yourself when only the base moved](merge-readiness.md#refresh-the-base-yourself-when-only-the-base-moved) | Handoff | added 2026-10-03; same basis, see below |
| S24d | A `mechanical` refresh (hunks resolved by a rule the assignment names, each reproduced) reruns the gates and the deterministic checks over those hunks, and a `manual` one adds an independent check of the hand-resolved hunks only, run by the child's reviewer. A claimed `clean` the helper refuses is read by its reason and never accepted on the child's word: an ordinary edit between the reviewed head and a conflicting merge is caught by accounting for every first-parent commit, and an edit riding inside a merge by `git show --remerge-diff` | The same two owners, and the refusal reasons in [the verdict checks](merge-readiness.md#what-the-handoff-discloses-checked-at-the-verdict) | Handoff | added 2026-10-03; same basis, see below |
| S24e | The handoff compares the changed paths with the declared edit regions (the parent re-checks with `git diff --name-only origin/dev...<head>`, and by hunk where a region covers part of a file) and carries a sibling impact, and a correction's restoration block carries the siblings' landings and the conflicts the parent expects as relay facts, so a task restored after compaction does not assume a base that is not there | [What a handoff discloses](task-packet.md#what-a-handoff-discloses) (`changedPaths`, `siblingImpact`), the verdict checks and the [Restoration block](task-packet.md#restoration-block) | Handoff | added 2026-10-03; same basis, see below |
| S25 | Where a packet's `Delivery:` covers publication, the child pushes its task branch and opens the pull request without stopping for a push approval, because the packet carries the approval CXC `DEV-GIT-PUSH-01` requires, and it never merges, force-pushes, tags or pushes to `dev` or `main`; a packet that excludes publication carries none and the child pushes nothing | [Default dev integration](../../crw-plan/references/integrations.md#default-dev-integration), the publication bullet of the [Launch packet](task-packet.md#launch-packet) and [First full assignment required fields](task-packet.md#first-full-assignment-required-fields) | Approval context | added 2026-10-03; the rule text was read and the identifier search returned no match, a child at the push step was not observed, see below |
| S25b | A child that needs something only a person can give does not call `request_user_input`, which CXC denies while a goal is active: it writes the question out, records `blocked_needs_input` on its turn and, where a relay holds the assignment, emits that outcome; where none does it returns the CXC status the case takes with the question | The same three places and [OPS-6.2](operations.md#ops-62-record-shape) | Recovery | added 2026-10-03; the hook denial was read in the CXC source, no child's use of the route was observed, see below |
| S25c | `LOOP-DOCS-FIRST-01` applies to a CRW child as CXC states it, to the child's own issue: a single-cycle issue skips the docs-only first cycle, and a child that plans two or more work-phases opens with one, with CXC's roadmap debt for scope found later | [Default independent execution](../../crw-plan/references/integrations.md#default-independent-execution) and the Loop bullet of the [Launch packet](task-packet.md#launch-packet) | Default settings | added 2026-10-03; read against recorded goalplans, the correction-generation and publication-phase cases it left open are decided by S25d, see below |
| S25d | A correction generation, a base-refresh generation and a separated publication step are not the first work-phase of new work, so they do not open `LOOP-DOCS-FIRST-01`'s docs-only cycle, and the CXC rule text is unchanged | [Default independent execution](../../crw-plan/references/integrations.md#default-independent-execution), the Loop bullet of the [Launch packet](task-packet.md#launch-packet) and the workflow bullet of the [Restoration block](task-packet.md#restoration-block) | Default settings | added 2026-10-03; decides the cases S25c left open, from S25c's goalplan record, and no child reading the line was observed, see below |
| S26 | A task that has to stop its own tests, a child or the parent, stops them by the pid it recorded when it started them or by the process group it created for them, and never by pattern or name (`pkill -f`, `killall`); a long command records its pid from the start or runs under `timeout`, and a process the task did not start is reported with its pid and working directory, not stopped | The `Processes you start:` line of the [Launch packet](task-packet.md#launch-packet) and its line in [First full assignment required fields](task-packet.md#first-full-assignment-required-fields); for the parent, [Observe and verify](../SKILL.md#observe-and-verify) and [Check CI for this candidate](merge-readiness.md#check-ci-for-this-candidate) | Preserved limits | added 2026-10-03; the mechanics were measured on this host's own processes, no child or parent reading the wording has been observed, see below |
| S27 | Devin Review and the Codex review are references and not merge gates: a candidate merges when every CI job succeeds on one and the same head, the coordinator verified it by its usual procedure (it read the diff and the code and reran the tests the criteria rest on) and the local gates pass; the merge waits for no Devin or Codex status or review, neither inheriting an earlier review by patch-id nor a substitute review comment is required, and a later head with no review of its own is normal | [The three gates](merge-readiness.md#the-three-gates) | Merge gate | added 2026-10-03; basis below |
| S27b | The child waits for the one run of each reviewer to end before it emits: a Devin status `Analyzing your changes` is waited for without a time limit and `Full review skipped: trial expired and no credits remaining` is not; Codex is waited for while its eyes reaction is on the pull request or any row of its summary comment is not `Completed`, and each of its code review and security review must be `Completed` or skipped by the bot's notice (a thumbs-up counts for both when no row contradicts it), so a Security Review row `Completed` alone is not finished; when neither reviewer shows any signal 30 minutes after the pull request became ready or was opened, the handoff says `review unavailable (no signal)` and the child goes on, and when only one is silent the same 30 minutes apply to it and the handoff says so; a thread that arrives after the receipt is a late finding and returns the candidate unless the parent has dispositioned it for that head (`--late-dispositions`) | [The one run of each reviewer, awaited before the receipt](merge-readiness.md#the-one-run-of-each-reviewer-awaited-before-the-receipt) and [Recheck, integrate, and record](merge-readiness.md#recheck-integrate-and-record) and [a late thread the parent dispositions itself](merge-readiness.md#a-late-thread-the-parent-dispositions-itself) | Merge gate | added 2026-10-03; basis below |
| S27c | The Devin status is read by its description and never by its state: `Completed analysis in <time>` is a completion even when the state is `failure`, the credits-exhausted description with state `success` is a skip, and a pending, missing or unfamiliar status is never recorded as a pass; an unfamiliar text is recorded as read and waited on, and the child does not guess its meaning | Same section | Merge gate | added 2026-10-03; basis below |
| S27d | A finding is handled by its grade whoever wrote it: Devin red, Codex P0 and P1 and any security finding (Devin `kind` security, the Codex security review) are fixed or answered with code evidence before the receipt, a defect that is real and blocks is never recorded `not_applicable`, and Devin yellow and Codex P2 and P3 get a reply and are resolved or listed for the backlog in the handoff | [What each finding needs before the receipt](merge-readiness.md#what-each-finding-needs-before-the-receipt) and [Judge a finding by its impact](merge-readiness.md#judge-a-finding-by-its-impact) | Merge gate | added 2026-10-03; basis below |
| S27e | A pull request that changes activation wiring, manifest declarations, the installers, `SECURITY.md` or `POLICY.md` merges under the same three gates, with no extra reviewer and no substitute review | [The three gates](merge-readiness.md#the-three-gates) | Merge gate | added 2026-10-03; basis below |
| S27f | A packet criterion or gate line "Devin has no red or security finding" is read as "if a Devin review exists, its red and security findings are resolved; no new Devin review is awaited", and the handoff and the merge record give each reviewer's reading, the disposition of each red, P0, P1 and security finding, and the three gates | [What the record says](merge-readiness.md#what-the-record-says) and the child bullets of the [Launch packet](task-packet.md#launch-packet) | Merge gate | added 2026-10-03; basis below |

S14 and S17 are the pair that is easiest to confuse. S14 removed the readiness turn; S17
added a capability check the coordinator performs before creating the task. A check that
happens on the coordinator's side, before anything exists to answer, is not a turn spent
asking the child whether it is ready.

S21 is unobserved in a run. The command it relies on has Go tests over its answers, and the 33 recorded issues of five projects were replayed
through it (the 31 that finished small answer `ok`, CRW-183 and CRW-184 answer `split_recommended`). Whether a parent runs it before
`managed-start`, holds a flagged issue as `defer:size_check` and records an approved exception with the user's own statement is a reading of
this text, not a measurement; the first project run after it lands is where to look.

S19 and S20 were read against P-CRW-115, a relay-managed project run: its six issue
children, whose pull requests are #271 to #276 (merges 14b01079, 6b5ba199, 9fb54f4b,
ca4316b4, 2ecad795 and 3a71c5d5), read on 2026-10-02 at dev 3a71c5d5. S19 was written before
the run (PR #269, commit 15796f52); S20 was introduced during it, by PR #272 (commit
bf4bc149). The receipts and relay records read for this are private and are not linked.

S19 is partly measured. All six packets carried the `Language:` line, and what was inspected
of what the children published is English: the titles and bodies of the six pull requests,
their 34 commit messages and the 9 non-bot replies and comments on them contain no Korean,
read through and searched for Hangul, and the seven receipt artifacts the children emitted
read as English too. This is one run in which the line was always present, so it shows that
the instruction held for those artifacts when it was given, not what a child without it
would write. Two things were not measured. A child's own messages and final return, which
the row also names, are not published and were not read. The CRW-255 child reported that its
internal independent reviewers wrote their reports in Korean; those reports are in no pull
request, commit or receipt and the report could not be checked, but the audit reviewer of
the CRW-257 child, given an English prompt, also answered in Korean, so the line does not
seem to reach a subagent's report. The line names the child's own messages and final return,
its commit messages, its pull request text and its receipt text, not a subagent's report, so
this does not change what was measured. Whether a reviewer's report should follow the same
line is a change to the rule, not a finding about this run.

S20 did not occur in the seven receipts read. All seven completion receipts of the six
children (CRW-255's twice, because its first receipt drew a revision request) were emitted
from a turn the relay had already admitted: the business turn managed-start delivered, in
generation 1, and, for CRW-255's generation 2, the generation's anchor, which is the turn
its revision request arrived in. Each receipt's turn was compared with that admitted turn as
the relay reports it, so none needed a continuation claim (a receipt from an admitted turn
is accepted without one) and the case the row decides, a child finishing on a
goal-continuation or post-restart turn the relay did not admit, was not seen. The row rests
on the tests and documentation of PR #272, not on a live run. The run does show one thing
about the procedure: the generation-2 anchor is not the `standbyTurnId` of the routing
record, which the managed-start routing text still tells a child to name for a continuation
claim (an open entry in the refactor backlog's real-use run 3 list).

S22 to S22e rest on a reading of the code and on tests, not on a live parent run. The code
read is dev 53c7e4e8, read again at aae27cce after dev was merged in: `dagsched` (`Accept` records the head the forge shows and refuses the same output
at another head, `Judge` rules `stale_head` before `stale_base`), `mergeturn` (`merge-turn-ready --head`
resets readiness, `merge-turn-check` compares the head with any recorded work report) and `evidence`
(the problems `merge-evidence --restate` raises for a moved head and a moved base). The helper behind
the check was rewritten as a tree identity (one merge commit; the walk over chains is gone). It has
its own tests on real repositories, and twelve of twelve mutations of it, one rule each, turn named
tests red. Replayed over this repository's history at dev ced7de60, it passes 27 of the 28 merges
whose subject is "Merge branch 'dev' into <branch>" (`--previous` the first parent, `--base` the
second) and refuses the one that records conflicts resolved by hand. S22c to S22e rest on the same
reading of `dagsched` (`judgeChecks` counts only failures a recorded judgement saw, which is why the
rerun of a DAG-managed project follows `dag-accept`) and on one merged pull request whose runs by
head SHA, landing time and landing-commit run were read from the forge, to show that each field of
the lane entry has a source; no parent has made that entry or that rerun yet. The `Devin Review`
status was read on one
merge-only head, where it completed in 2 seconds and left no review object; its behaviour on other
heads was not measured there, and the later readings are in the basis for S27 to S27f. Not measured at all: the forge's update-branch call on a live pull request,
an installed runtime, and the rule under load.

S23, S23b and S23c were read on 2026-10-03 against the pull requests #278 to #294 as GitHub
showed them and this checkout at dev 53c7e4e8. They measure published artifacts and this host.
None of them measures a child that read the new wording, which waits for the next run, and
a title alone does not show why a child chose it.

S23: the seventeen titles were read with `gh pr view N --json title` and tested for Hangul. Six
are the Korean task title in the task-title shape: #282, #285, #287, #292, #293 and #294. The
other eleven are English, in several shapes (`CRW-263: ...`, `fix(relay): [CRW-271] ...`,
`CRW-270 · per-parent due cap and parent-id rotation`). CONTRIBUTING.md and POLICY.md of this
repository state no title format, so the rule defers to the rules of the target repository and
has the packet name a shape where there is none. The packets of those children are not in the
repository: the refactor backlog records that the packets of #282 and #285 carried a `Language:`
line naming the pull request's title, and nobody checked it for the other four.

S23b: on this Linux host a Unix socket bound at a path of 107 bytes and failed with `AF_UNIX path
too long` at 108, probed with a Python `bind` in a temporary directory. The macOS bound of 104 is
read from internal/bridge/appserver/fakehost/fakehost.go (lines 166 to 169) and from the body of
PR #285, not measured. The rule gives no length for `TMPDIR` itself: the test adds its own
directory and file names, and the body of PR #280 reports a socket path of 111 bytes, set by a
test's own name, under a `TMPDIR` already shorter than the one that failed elsewhere.

S23c: at dev 53c7e4e8 `make -n contract` prints `make: Nothing to be done for 'contract'.` and
exits 0, because the Makefile has no `contract` rule (`.PHONY` on line 23 lists build, test,
test-binary, test-part, lint, dist and crw-dev) and a directory `contract/` exists, while
`make -n nosuchtarget` prints `No rule to make target` and exits 2. A gate that ran `make
contract` would have passed with nothing checked; the bodies of PRs #281 and #282 both report
the missing target. A focused test is confirmed with an anchored `go test -list '^Name$' ./pkg`,
run with the verification's own package and tags. In `./internal/bridge/appserver/fakehost` of this
checkout the pattern `TestStart_bindsAndServes` without anchors lists
`TestStart_bindsAndServes_whenTMPDIRIsLong` although no test has that exact name, the anchored
`^TestStart_bindsAndServes$` lists nothing for it, and `go test -run '^TestNoSuchTest$'` on the same
package exits 0 with `[no tests to run]`. An empty listing means no matching top-level test in that
build, since `-list` does not print subtests; a missing package, and a package whose files are all
behind the `dev` tag listed without `-tags dev`, fail with exit 1.

S24 to S24e were read on 2026-10-03 against this checkout at dev ced7de60, and the code they cite
(the base-refresh helper, the merge-evidence reading and the emit command) was unchanged when dev
moved to 6071bc8a. They rest on a reading of the text and of that code, and on one experiment; none has been seen working in a
run, and no child or parent has read the new wording yet, which waits for the next real-use run. The
observations that motivated the rules are the issue's own analysis of 27 issues: two of the three
defects a parent sent back had been rated High by the child's own review and rejected without a
trace in the receipt, 12 of 27 internal code reviews ran at or after the pull request was created
and so overlapped the hosted review on one head, and the re-audits that followed a base refresh took
12 rounds and 54 minutes and found no product defect. Those numbers come from the issue text. The
receipts behind them are private and are not linked, and they were not re-derived here.

What was read: `merge-evidence --restate` reads the record's own `handoff` member and only the keys
it grades, and `emit` carries the handoff file as an artifact without parsing it, so a `disclosures`
member is carried and checked by nothing but the parent's verdict. What was run, with git 2.53.0 and
a build of `crw` from this checkout in a temporary repository: a merge of a base that moved with no
overlap had equal expected and actual trees and `crw skill base-refresh check` exited 0 and printed
that merge's first parent, second parent and tree; a merge that resolved a same-line conflict by
hand was refused `merge_conflicts` (exit 1) and `git show --remerge-diff` printed exactly the one
resolved hunk; a conflict-free merge with an added file was refused `tree_differs` naming that file
and `git show --remerge-diff` printed it as a new file; and `git show --remerge-diff` printed
nothing for the clean merge. The merges-only window command
(`git rev-list --first-parent --merges <baseline>..<head>`) was run on this branch only, where it
listed the one merge made here. The helper stops at the first refusal it meets from the head, a
conflicting merge for example, which is why the verdict accounts for every first-parent commit after
the reviewed head and does not rely on it alone. Not measured: mechanical and manual refreshes
beyond those synthetic merges (no rule-resolved hunk was reproduced by a command), the forge, a
relay holding a handoff that carries the member, and any child or parent following the text.

S25, S25b and S25c were read on 2026-10-03 against this checkout at dev ced7de60 and the CXC plugin
0.2.40+codex.20260929183231. They decide what the packet and the integration text say. None was
observed on a child that read the new wording: whether a child stops at the push step is to be
observed in the next real-use run, and a test that matches the wording would show only that the text
is present.

S25: `DEV-GIT-PUSH-01` is skill prose in CXC's `cxc-dev` (the Safety rules bullet "Push requires
explicit user approval", class ESCALATE). A search of the plugin's non-Markdown files, hooks and
compiled components included, for the rule identifier returned no match. That shows no hook cites it
by name, not that nothing equivalent exists, so the one mechanism found is the rule text itself: a
child stops if it reads the rule and follows it. The coordinator's reading of the October child
rollouts on 2026-10-03, which was not repeated here, found no child that stopped pushing because of
it, so the conflict is latent: a change of model or effort could make a child that follows the rule
to the letter report BLOCKED at the push step. The sentence is wording, so it is an early warning
and no enforcement. A child can still read the CXC rule first and stop, and "never merge" rests on
this text and on the coordinator's review of the pull request, with the merge gate that POLICY.md
describes. Whether that gate is active on the server was not read back here.

S25b: the CXC hook `hooks/pre-tool-use-guarding-interview-in-goal.json` denies the user-input
request while a goal is active (the denial is implemented in the plugin's
components/pabcd-state/dist/goal-gate.js, read as source and not observed on a host), which is real
enforcement for the first half of the line. That a `blocked_needs_input` turn then reaches the
parent is not shown: [OPS-8.1](operations.md#ops-81-parent-continuation-and-waiting) says whether
anything enqueues a delivery for that disposition is a property of the installed runtime and
unmeasured, which is why the line has the child emit the outcome over a file where a relay holds the
assignment. No child's use of the route was observed.

S25c: read-only over the working-tree goalplan files of this host's CRW child worktrees (private,
not linked). Since 2026-10-01 they hold 33 plans with work-phases: 24 with one, 4 with two (three
are a generation-2 correction, one is an implementation followed by a publication phase) and 5 with
three or more, four of which record a docs-only roadmap phase first; an independent recount
reproduced these figures and found three explicit generation-2 correction phases. A single-cycle
issue opens no docs-only cycle in this record. Two cases were left open because CXC counts
work-phases and not what a phase does: a correction or base-refresh generation, and a publication
phase split off from the implementation. CXC says multi-cycle scope discovered later pays the
roadmap debt in the next P, and none of the three recorded generation-2 corrections and the one
recorded implementation-then-publication plan opened a docs-only phase. That is recorded practice,
not permission. S25c did not decide these cases; S25d does. The CXC text stands.

S25d: read on 2026-10-03 against this checkout at dev aae21a98 and the CXC plugin
0.2.40+codex.20260929183231, and decided on that date within the scope Jun delegated, from the
record S25c holds and not from a new reading of goalplans. For a multi-cycle loop,
`LOOP-DOCS-FIRST-01` (cxc-loop, "Docs-first multi-cycle entry") makes the first work-phase of new
work a docs-only roadmap cycle, and a single-cycle task skips it. A correction generation (the same
deliverable of the same issue), a base-refresh generation (a merge of the base and the resolution of
its conflicts) and a separated publication step (the push and the pull request) all come after the
first work-phase of the issue's work, so the reasoning for the decision is that a docs-only cycle
there would only add round trips; S25c measured whether such cycles opened and not what they cost.
It counted three generation-2 corrections and one implementation followed by a publication phase,
and none opened a docs-only phase; it reports no separately identified base-refresh observation.
That is practice, which is why the line is written and not relied on. It stands in the Loop bullet
of the Launch packet, in the workflow bullet of the Restoration block and in the Default independent
execution paragraph, which the packet points to. It does not waive `LOOP-DOCS-FIRST-01` when a loop
is entered, or the roadmap debt for new scope found later: it says only that these three later steps
are not the first work-phase of new work. The CXC rule text is not changed and still counts
work-phases, so the line is how CRW reads that rule for its own children. Not observed: a child
reading the line at a correction, a base refresh or a publication step; that waits for the next
real-use run, and a test that matches the wording would show only that the text is present.

S26 was read on 2026-10-03 against this checkout at dev 321f5bd0. The observation behind it is the
issue's account of real use that day: a child stopped its own tests with `pkill -f 'make test'`
while the children of several projects ran `make test` in their own worktrees on this host, and
another child's local test part ended on a signal at about the same time (its hosted CI passed, so
no merge was affected). The account is the issue's, taken from a parent's report; that report and
the other child's test output are private and were not re-read, and nothing here shows that the
pattern kill caused the signal. Before the change `rg -i 'pkill|killall' plugins/crw/skills` found
nothing. The close-of-run paragraph of SKILL.md already said that a busy port, a matching command
line or a familiar directory does not identify an owner and that a process the run cannot claim is
reported rather than stopped. It did not say that a task stops its own processes by a pid recorded
at the start or by its own process group, and it did not forbid choosing a target by name or
pattern; the parent's verification procedure did not say how its own runs are stopped.

What was run, on processes started for the purpose and stopped only by their own pids or process
groups (no target was selected by name or pattern; the script and its output are kept in the
private evidence root): a command tree started under `setsid` with its pid recorded had a process
group id equal to that pid; a TERM to that pid alone left its two child processes running, and
`kill -TERM -- -<pid>` ended the group; `timeout 2` over a command with two children put itself,
the command and its two children in one process group and left nothing in that group at expiry
(exit 124); and two processes with identical command lines were told apart only by pid. That is
why the line names the recorded pid and the group, and says that the pid of a shell or of `make`
alone is not enough for a tree. Not measured: a child or a parent following the new wording, the
reuse of a recorded pid, `timeout --foreground`, and macOS, which was not tried (the line names
`setsid` only as an example, and `timeout` because the issue does). The line is an instruction.
Nothing in this repository enforces it and the host's hooks and sandbox profile were not checked
for a refusal of `pkill`, so a child that never reads it can still use it, and a test that matches
the wording would show only that the text is present.

S27 to S27f decide what the merge gate and the first-run wait say. They rest on the coordinator's
instructions of 2026-10-03 (Jun's decisions, relayed at about 16:50 to 17:05 KST) and on what the forge
shows, not on a parent or child that read the new wording. Read through `gh`, read-only, on 2026-10-03: the
`Devin Review` status descriptions on the current heads of 15 pull requests chosen by recency. The
credits-exhausted text `Full review skipped: trial expired and no credits remaining`, state `success`, is on
359, 366, 367 and 368, the earliest created 07:40:47Z with no pending status before it; `Completed analysis
in` 55s to 1m 16s follows a pending `Analyzing your changes` on 360, 364 and 365; 1s to 4s is on final heads
that only merged the base, on 357, 358, 361, 362 and 363. Pull requests 293, 294 and 348 were read earlier
and one of them, 294, shows a failure whose description is `Completed analysis in 4s`. The Codex shapes were
read on 365, 367 and 368: one issue comment by `chatgpt-codex-connector[bot]` with a table of review rows,
`Completed` rows, a thumbs-up by the bot on 365 and 367, and on 368 a Security Review row finished at
08:02:07Z, review comments badged `P2` at 08:02:36Z and the Code Review row finished at 08:02:39Z. The late-thread
consequence is read from `RestateProblems` in internal/relay/evidence/collector.go, which grades a review
thread on the head that is not in the record's `threadsSeen` as `late_finding`.

Observed later, on pull request 375 itself (the one that carries this text): the Devin status was `Full review
skipped: trial expired and no credits remaining` within three seconds of the pull request opening; the bot posted
`You have reached your Codex usage limits for security reviews. Please try again later.` seven seconds after the
open, then a summary comment whose only row was the Code Review, `🔄 Running since` 08:43:42Z, with the bot's eyes
reaction at 08:43:48Z; the row read `Completed` at 08:54:51Z, the eyes reaction was gone by the 08:56:59Z reading, a COMMENTED review with
seven comments badged `P1` and `P2` had arrived at 08:54:47Z, and there was no thumbs-up. All 68 reviews by
the two bots on pull requests 318 to 377 (the 60 most recent when read at about 08:58Z) are `COMMENTED`, none `CHANGES_REQUESTED`.

Not measured: the move from in progress to finished for Devin (it skipped), a Codex review that finished with
no findings in this state of the system (the thumbs-up is read from 365, 367 and the Codex footer), and any
parent or child applying the text. One review per pull request and the 30-minute rule are the coordinator's statements, not read from
either reviewer's configuration. The registered criteria c1, c5 and c7 of this issue say that no Devin review
is waited for; the later instruction has the child wait for the first run before its receipt, the rows follow
the later instruction, and the handoff asks the coordinator to record that amendment.

## Negative cases

These reproduce the paths where the Loop goes missing. Each case sits at one lifecycle
point, resolves to exactly one class, and is failed by the outcome named rather than by
the absence of a keyword.

**N1 — the assignment never carried it. Create. L0.** A child is created with the issue
scope and the settings, and the prompt names no installed-skill invocation and no agreed
alternative workflow. Evidence: this dispatch's own assignment message, selected by its
marker or receipt rather than by position. Instruction existence fails while the settings
request and the settings observed on the task both pass, which is why a receipt check
alone reports this dispatch as clean. Failing case: recording the dispatch as sound
because the model and effort came back correct.

**N2 — carried and never acted on. Create. L1.** The invocation is in the dispatched
prompt and the child never loads or invokes the skill: no session binding, no
orchestration entry, no goalplan, and no refusal to quote either. Evidence: the
assignment text and the absence of any attempt in the child's own state, read after its
first turn has ended. This is distinct from N3, where something was attempted and failed,
and the repair differs — here the instruction is supplied again on the same task, with
nothing to report as a capability gap. Failing case: merging it with N3 and reporting a
refusal that never happened.

**N3 — invoked and not initialized. Create. L2.** The child loads the skill and its
session binding or goalplan initialization is refused. Evidence: the refusal itself, or a
binding that resolves to another session. Failing case: reporting the assignment as
running because the task accepted the turn, or re-dispatching the same assignment as new
work instead of reporting the exact refusal on that task.

**N4 — a native goal standing in for a loop. Create. L3.** The child opens a host goal,
works, and never binds a goalplan. Evidence: an active goal with no bound goalplan and no
persisted transitions. Failing case: reading the active goal, or the served model, as Loop
evidence. The missing activation is supplied on the same task.

**N5 — the workflow dropped on a later send. Resume or compaction. L4.** A correction or a
resume restates the branch, the baseline and the scope, and omits the workflow because the
transport has no field for it and nobody noticed. The child continues without the Loop it
started with, or a compacted child never restores it. Evidence: earlier goalplan and phase
evidence, then later turns with none. Failing case: treating the transport's checkable
settings as the whole restatement.

N5 owns L4. Its recovery has a characteristic way of going wrong, recorded here as a
subcase rather than a class of its own: an assignment whose activation actually succeeded
is "recovered" by opening a second goal and a second goalplan for the same issue, leaving
two where neither is identifiable as current. The evidence is those two goalplans, and the
failing case is a resume that does not first read whether the goal and goalplan already
exist.

**N6 — a verdict reached too early. Any point. L6.** A class is assigned from one reading
that cannot distinguish the classes, most often a child still inside its first turn.
Evidence: the reading's own timestamp against the dispatch time, and the absence of a
paired later reading. Failing case: recording L0 or L1 and correcting a child that was
arming normally. The honest verdict here is L6 and the repair is to read again.

## Contrast cases

**C1 — a normal Loop child. L5.** Effective workflow is CXC Loop, the first full
assignment carries the invocation with the applicable skills, and the child's first
execution leaves a bound goalplan and persisted transitions it can name in its report.
This needs no coordinator turn beyond reading what the child returns. The four children of
the measured batch are this case, each arming within about two and a half minutes of its
own first turn.

**C2 — an authorized non-Loop or read-only child. Not a class; an exception.** An explicit
read-only, no-goal or non-Loop audit assignment omits the invocation and names the agreed
workflow instead. The absence of a goalplan here is the instruction being followed.
Judging it as L0 or L1 is the mirror-image error to N4, and a non-PR audit child's
execution mode is settled by its own assignment rather than by this default.

**C3 — a parent coordinating without a goal. Not a class; a different role.** A project parent
holds no native goal by default and no CXC implementation goalplan or FSM either, and is resumed by
the delivery path rather than by a goal. Where an explicit Loop was requested it holds a native
goal for the agreed scope and still no goalplan or FSM. Either way that is the
parent's defined shape, not an unarmed child. The role that requires both is the
issue-owning implementation child.

## Limits

These are instructions, and no hook, gate or script enforces them. A coordinator that
never loads this skill never applies them, and the evidence above is readable only where
the transport exposes the child's own state and report. Where a class is not
distinguishable, the honest verdict is L6.

Where the cause is not the assignment text — an installed runtime that refuses a
supported call, a transport that drops a field, a hook that does not fire, or a dispatcher
that conveys the workflow by naming the skills rather than by the invocation this contract
specifies — this reference records the observation and the owning issue takes the repair.
An outcome that came out right through a path the contract did not specify is still worth
routing, because the next dispatch on that path has no guarantee. Correcting an
instruction here does not change an installed skill, a running child, or a registered
hook, and none of these cases is a reason to reset a child that is still working.
