# Explaining a design

Use this when the request is to understand or review a system rather than to plan one: what a
component is for, whether an existing owner can be left where it is, how the parts fit together,
where a service ends and its database begins, or whether a proposed change actually helps. It also
applies when a planning document has to carry design reasoning somebody will read later without
the conversation that produced it.

An explanation authorises nothing; what a request to explain does and does not permit is at the
end of this page.

Treat a question asked three times as a sign that the previous answer was a list of component
names, rather than as evidence of a defect in the system. Five questions decide whether an
explanation worked, and an independent reader must answer all five from the explanation alone:
who runs it, where the state is, what actually refuses, how far it is built, and how an improvement
would be judged. The sections below are those five in the order a newcomer asks them, and they
depend on each other in that order: which state exists decides what can be enforced, what is
enforced decides which maturity claim means anything, and the maturity decides what a comparison
is able to measure.

## Lead with the result somebody can see

Open with what changes for the person using the system, in their vocabulary. Somebody who asks what
a hook is for is asking what stops happening to them once it exists.

When the path crosses more than one process, follow that with a sketch of at most five steps:
request, work, completion signal, delivery, verification. Name for each step who executes it and
what it leaves behind, because the arrows are the explanation and the boxes are not. A step whose
actor cannot be named is the interesting part of the design, so say that instead of skipping it.

Introduce an internal name only where a step cannot be described without it, and introduce it where
it is used rather than in an opening glossary. If a term appears once and decides nothing, cut it.
An inventory of components, ports, files and flags is what an explanation produces when nobody knew
which of them the reader needed.

## Say which process runs and which store keeps

A process and a store are not two views of one thing, and an explanation that leaves them merged is
where the question "where is the database" comes from. Give the state its own reading:

| For each piece of state | Say |
| --- | --- |
| What it is | The fact being kept, not the file format |
| Where it lives | The store, and whether it is durable, shared or per run |
| Who writes it | The single process that may write, and who may only read |
| Who keeps and restores it | Who is responsible after a crash, a reinstall or a wrong write, by name. Where nobody is, say that and say what is lost with it |

Keep three readings of a location apart, because they are routinely collapsed into one sentence and
only the second one is a fact about the run being discussed:

- the path a document names as the default,
- the path a particular run actually resolved and used,
- the path a proposal says operations should use.

Identify the first and the third by naming the document that owns them rather than copying the
value, so this explanation cannot go stale against its owner. Write only the second as a value, and
take it from a receipt or a reading of that run. Where nobody looked, `not observed here` is the
honest entry and an invented default is not.

Two settings that sound like one are worth stating explicitly: a store selector and a ledger or
cache selector often default to different roots, so moving one and not the other leaves two
processes reading different truths while both report success.

Some state has no restorer, and that is a finding rather than a gap in the writing. Name it: this
one is rebuilt by nobody, and losing it costs exactly this. What leaves an explanation unfinished
is not an unrecoverable store; it is a store whose recovery nobody asked about.

## Say what actually refuses

Four layers get called enforcement and only some of them refuse anything. Keep them apart, and say
for each what it cannot do:

| Layer | What it does | What it cannot do |
| --- | --- | --- |
| Skill and prompt guidance | Changes what an agent decides, when it is read | Refuse anything. Text in a skill is followed, not executed |
| The effective sandbox and permission profile | Bounds what the process may touch at all | Express intent. It is the boundary that actually holds, so read the effective profile back from the creation receipt and keep requested, effective and unknown apart |
| A host callback | Runs at an event the host defines and returns a decision the host may honour | Exist because it was declared. Registration, persisted trust, invocation and an observed firing are four separate facts, and only the last is behaviour |
| A service's delivery and recovery duty | Carries a result to whoever waits for it and recovers after a fault | Be proved by a firing. A callback that ran and a message that arrived are different events |

Name the enforcement point: the place where a wrong action is actually refused, and what happens
when that place is absent. If nothing refuses, say so. "No layer refuses this; the guard is an
early warning" is a useful answer, and it is the answer to give instead of pointing at a
registered hook and calling the rule real.

A hold or a block is also conditional on isolation somebody granted. Where the held process could
have written the facts the decision read, the decision proves nothing, and the honest design is to
observe rather than to tighten the held process until the guard looks strong.

## Say how far it is built

Five labels, one per claim, never blended into "it works":

| Label | Earned by |
| --- | --- |
| Proposed | A decision somebody wrote down |
| In source | Code or instructions in a named revision |
| Installed | An artifact present on a named host or destination |
| Active | Registered, trusted and reachable there |
| Observed | A run that produced the behaviour, with its command, revision, environment and result |

Carry the evidence with the label. A claim with no command, revision, environment and result is
`not observed`, whatever the code suggests. Editing source changes none of the later labels, and a
passing repository check says nothing about an installation.

Name the existing owners by name, say what the change touches for them and what it leaves alone,
and keep a capability nobody read out of the assertions: an API, a host behaviour or a tool option
that was not read or exercised is written as unverified, not as a feature.

## Judge an improvement before claiming it

A proposal is not judged by how reasonable it sounds. Carry five things with it:

1. **The miss it removes.** The failure or omission, counted or described concretely enough that
   its absence would be noticeable.
2. **The comparison.** The same conditions with one switch, so the difference is attributable.
   The criterion must not be the switch itself: if turning the feature on is what produces the
   records the check reads, the check measures the flag.
3. **The costs.** What the change can do wrong: refuse something that was fine, deliver twice,
   add delay, or add a new way to fail. Count them in the same run rather than assuming them away.
4. **The decision rule.** The threshold that would make this a keep or a revert, fixed before the
   measurement rather than after seeing it. Where a proposal has no such rule yet, say so in those
   words and treat it as the piece that is missing: the work can proceed, and it cannot be accepted
   on a measurement nobody agreed how to read.
5. **Expected and measured, in separate paragraphs.** What the change is expected to do belongs
   before any run; what it did belongs after one, with the run named. A sentence that contains
   both is how an expectation becomes a result nobody produced.

Where no run exists yet, that is a complete and acceptable answer: the expectation stands, the
measurement is absent, and nothing claims otherwise.

## Explaining is not doing

A request to explain or review is done when the explanation is delivered. Do not install, execute,
register or restart anything to make the answer better, and do not open work the request did not
ask for.

Where the request or the scope it already carries includes reflecting the outcome in Linear, do it
in the same pass: update the canonical document and every plan item the explanation actually
changed, read each one back after the write, and report what the read-back returned. Do not ask
again for an approval already given, and do not leave a verbal agreement as the only record. The
authority, the read-back duty and the ownership rules for those writes stay in
[Integrations](integrations.md).

## Worked example: a completion hook and a session relay

For the person running work across tasks, the result is this: when a task stops without saying
whether it finished, somebody finds out in that moment instead of when the work is missed later.

Request, work, completion signal, delivery, verification. A parent assigns an issue to a child
task. The child does the work in its own checkout. At the end of a response turn the host invokes a
Stop hook, which reads whether the child recorded anything for that turn. The relay carries what
the child recorded to whoever waits for it. The parent verifies the result against the issue's
criteria. The hook never decides that the child is finished; it decides whether the child said
anything at all, which is a fact about a record rather than about an intention.

State, with a writer and a restorer for each piece. Three stores, and they fail differently.

The marker root holds what each party declared. Its writers are split by party: the coordinator
writes the assignment's own facts, a child writes only under its own session, the child's hook
process writes only its own hook directory, and the relay daemon writes none of it and reads none
of it. It sits outside both the source checkout and the relay's database. Retention belongs to
whoever owns that root, and [the hook contract](../../crw-run/references/hook-contract.md) says
plainly that no owner is fixed inside it, so assignments accumulate with no natural bound. Nothing
restores it either: a lost or wrongly written marker is not rebuilt, and what is lost with it is
the record of what each party declared for those assignments. That is the honest entry, an unowned
cost named as unowned rather than a restorer invented to fill the row.

The relay's store holds the assignment itself, and it is the piece that survives everything else.
Every process in the assignment writes it through the relay, all of them passing the same store
selector. A process reaching its deadline, restarting or failing outright leaves the assignment
where it was, because recovery continues from the existing store and never from an empty one. The
store itself is backed up and restored by whoever installed and operates it, under
[the operations contract](../../crw-run/references/operations.md), which also makes merging two
stores that already exist in one scope a migration with its own decision and its own backup. The
relay does not restore itself, and no other component rebuilds it.

The adapter's ledger is written by the adapter, and it is where duplicate suppression and delivery
recovery live. It is selected by a different setting than the store: move one with a command-line
option and not the other and a run reports one location while recovery reads another. Lose the
ledger and recovery reads a ledger that never saw the earlier attempts, so it can miss a delivery
or repeat one. Nothing rebuilds it, and nobody is responsible for restoring it, which is why the
two settings are worth a sentence in any explanation of this system.

The coordination summary is written by the parent into the Linear document, and Linear keeps and
restores it as its own service, which makes it the only piece here whose recovery is somebody
else's job entirely. Raw run records are private receipts written by whoever ran the thing, kept
outside this repository, and restored from no backup this explanation knows of. The defaults and
the one-store-per-operating-scope rule belong to
[the relay reference](../../crw-run/references/relay.md), named here rather than copied. What a
given run actually resolved is a receipt value, and this explanation has none.

What refuses. Skill text refuses nothing. The sandbox profile the child was created with is the
boundary that holds. The hook can hold a turn only where it is registered, trusted, invoked and
running in hold mode, and the default installation is observe mode, in which every block is
downgraded to a release and nothing is held. Holding is permitted only where the held child could
not have forged the facts the decision read. The rules are in
[the hook contract](../../crw-run/references/hook-contract.md); what matters here is that a hook
appearing in a configuration file has established none of it.

How far it is built. The contract, the comparison harness and the document recording which of its
six measures were performed are in this checkout; two of the six are recorded as not performed. The
revision is deliberately not written into this page, because a commit named inside the commit that
contains it is self-referential; an answer written anywhere else names the revision it read, and a
reader is right to ask for it. Nobody here re-ran it, no run result is committed, and no host is
claimed to have this installed, active or live.

CXC is an existing owner and is left alone: the contract modifies no CXC state, and the
comparison deliberately keeps a foreign CXC Stop entry in the same hook file, reads it back after
the install to prove it was not displaced, and never executes it.

How an improvement is judged. The off and on comparison in `docs/hook-comparison.md` builds both
arms from one command where a single flag is the only difference, and its own rule is the one
worth copying: the pass is never the difference between the arms, because that difference is
settled by the flag before any turn ends. What it can answer is bounded and it says so. It reaches
both missed-detection measures. Handoff success and duplicate execution, meaning no verification
or correction running twice for one event across a hold, a restart or a recovery, are recorded as
not performed, and the installed runtime is outside what it reads at all, because no daemon runs
in it and nothing is installed.

The criteria were fixed before implementation so the comparison could not be tuned once numbers
arrived, and they are per-measure criteria rather than a combined keep-or-revert threshold. No such
threshold has been fixed, and no adoption or hold decision is written anywhere, because the
measures that would support one are among those not performed. Saying that is the point: a missing
decision rule is reported as missing, not replaced by the writer's own. The expected effect is that
fewer finished turns go unreported. There is no measured effect to report, so the question stays
open rather than being answered yes.

## Worked example: a queued thumbnail job

An illustration, not a system in this repository, to show the same five answers with entirely
different machinery.

The visible result: a person uploads a photo, the page responds at once, and a thumbnail appears a
few seconds later.

Request, work, completion signal, delivery, verification. The web process writes one upload row and
answers immediately. The queue holds a job and moves nothing by itself. A separate worker process
claims the job, writes the thumbnail, updates the row, and sends the notification; the web process
never sends it. The person seeing the thumbnail is the verification.

State. Four pieces with different owners and lifetimes: the upload row, written by the web process
and restored from the database's own backups by whoever operates it; the job, written and
redelivered by the queue and lost with it if that queue is not durable; the thumbnail, written by
the worker into object storage and restored by re-running the job rather than from a backup, which
a scheduled sweep over rows still marked pending starts, owned by the same team that runs the
worker; and
the notification record, written by the worker and rebuilt by nobody, because a notification that
was never sent cannot be recovered after the fact. The row and the stored object survive a restart
and the worker's in-memory handle does not. A job lost with a non-durable queue has no restorer
either; that same scheduled sweep is the only thing that brings it back. No process rebuilds
a thumbnail whose row no longer says pending, which is the one recovery path deliberately left
closed.

What refuses. One thing refuses: the unique constraint on the thumbnail's upload key, which makes
a second insert fail rather than produce a second thumbnail. At-least-once redelivery is recovery,
not enforcement; it creates the duplicate rather than refusing it, so the enforcement point is the
consumer's insert-if-absent against that key, and a worker that notifies before that insert
notifies twice however carefully it was written. Refusing nothing at all: the sentence in the
README asking workers to be idempotent, the naming convention, and the retry helper nobody is
obliged to call.

How far it is built. The web process, the row, the queue, the object store and the unique
constraint all run in production; the worker exists in source and runs only in staging; the
notification is a drawing. Every piece named above gets one of those labels, because a piece left
unlabelled is the one a reader will assume is running. In a real answer each label would cite the
deploy record or the run that earned it; this one cites nothing, because it describes no real
system, and an illustration is the only place that excuse works.

How an improvement is judged. Adding a retry answers a counted miss: uploads still pending after
ten minutes. The comparison replays one recorded day's workload against the same code with the
retry as the only switch. The costs counted are duplicate notifications, added delay before a row
reaches ready, and jobs retried after they had already succeeded. The rule is fixed before the run
and written down: keep the retry only if that pending count falls and duplicate notifications stay
at zero, revert it if either fails, and treat an unchanged pending count as a wrong diagnosis
rather than a reason to retry harder. The expectation and whatever the run returns go in different
paragraphs.

## Self-check

Before delivering, answer these as the reader, using only what the explanation says:

| Question | Answered by | A missing answer means |
| --- | --- | --- |
| Who runs it? | The visible result and the five-step sketch | The explanation named parts without naming actors |
| Where is the state, and who restores it? | The state reading | A store has no owner, or a default path was written as an observation |
| What actually refuses? | The four layers | Guidance or a registration is standing in for enforcement |
| How far is it built? | The five labels | A claim is carrying no evidence and should read not observed |
| How would an improvement be judged? | The five-part proposal | Either the comparison measures its own switch, or an expectation is being reported as a result |

A question the reader cannot answer is a gap in the explanation, not in the reader. Fix the
explanation rather than adding a section that names more components.
