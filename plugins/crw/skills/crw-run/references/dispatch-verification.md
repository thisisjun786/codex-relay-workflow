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

## Recorded cases

The nineteen cases below were decided against the JUN-99 delivery at
`d91164d96b08c9d243786b9455989f87022ebc31` on the pre-rename tree, where these files
were `skills/linear-run/` and `skills/linear-plan/`. Each owning anchor was re-read
individually against this tree after the `crw-*` rename and the move under
`plugins/crw/skills/`, rather than derived by rewriting the path prefix. Seventeen
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
| S9 | With no explicit choice the child runs `anthropic/claude-opus-5` at `xhigh` with CXC Loop | [Default independent execution](../../crw-plan/references/integrations.md#default-independent-execution) | Default settings | unchanged |
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
S14 and S17 are the pair that is easiest to confuse. S14 removed the readiness turn; S17
added a capability check the coordinator performs before creating the task. A check that
happens on the coordinator's side, before anything exists to answer, is not a turn spent
asking the child whether it is ready.

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

**C3 — a parent coordination goal. Not a class; a different role.** A project parent holds
a native goal for the agreed scope and no CXC implementation goalplan or FSM. That is the
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
