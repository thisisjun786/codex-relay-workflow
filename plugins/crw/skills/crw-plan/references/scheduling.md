# Schedule and dependency roadmap

How `crw-plan` builds, checks and records a schedule. The field names, their sources, the
precedence between records, the evidence rules, the wait cause and change reason words and
the four graph checks belong to the shared
[Schedule baseline contract](integrations.md#schedule-baseline-contract); this file applies
them and does not restate them. Read it when a request covers a project or initiative plan,
or a scoped update to a schedule that already exists.

## What the default result carries

An authorized project or initiative plan write carries its schedule instead of waiting to be
asked for one: the project's start and target dates, milestones named after the result they
finish with their target dates, each issue's milestone link and the target date it actually
needs, the prerequisites that really hold between them, and the areas that can proceed in
parallel. Name the subjects that have no date and why, rather than leaving the schedule silent
about them.

The same write carries the four facts a reader has to act on today. The critical path is the
chain of prerequisites whose targets leave no room between one result's confirmation and the
next one's target, counted over the prerequisites still open, because a result already confirmed
constrains nothing. Where two chains tie, both are on it and the project's target is the latest
target on either, so two readers derive the same answer. The immediately startable batch is every
subject whose wait cause is `none` and whose execution state does not already report it started;
an empty actual start never decides that on its own, because the field stays empty wherever no
evidence exists, including for work that is already running. Every other subject carries its real
wait cause together with the observable event that would end it, and that event is the next
re-evaluation condition rather than a date chosen to serve as one.

A calendar target marks when a result is expected. It is not a gate: starting or finishing
earlier needs no permission and no change to the target. Three things survive that, and none of
them is a date before which work may not start. A `confirmed` deadline is a date this plan does
not move. The user's own stop halts the work it names. Execution nobody authorized does not begin.

Scope holds while doing it. A request that updates one issue reads the initiative and project as
context and proposes any parent schedule change without writing it. A scoped project update
covers that project's own subjects. Neither becomes a reason to move dates the request never
mentioned.

## Reading a target from delivery records

When the request states no dates, read the target from what delivery has actually taken: the most
recent records of the same class of work at the same stage of the same product, each running from
assignment through the pull request and its review to the merge or acceptance that finished it,
under the review and host conditions this subject will meet. Recent means those records newest
first, matched by class, stage and conditions, and never a lookback window, because a window is
the fixed period this section exists to refuse. Read back to the last change in the conditions
those records describe and stop there: a record from before that change describes work under
conditions this subject will not meet, so the boundary is that change rather than a count of
records or a number of days, and two runs reading the same history read the same set. Where
several records match equally well and disagree, the target follows the slower of them and the
roadmap names the spread, because a target read from the single fastest run is a best case rather
than a target.

Reading a record is evidence, not an estimate. Nothing here estimates duration: no cycle length,
sprint, working day, working week, default period, per-issue day count or model-produced remaining
time becomes a target, and a horizon that suited one product is an example rather than the next
product's default. A delivery date the user states is `confirmed` and a proposal does not replace
it. A subject whose date needs a decision nobody has made is `awaiting authority`. A subject is
`undetermined` exactly when its earliest day cannot be read at all, because a prerequisite carries
neither a target nor an actual finish; a missing delivery record is never that situation. Both are
reported plainly instead of filled in.

The day that evidence yields is the earliest one on which this subject's prerequisites can have
been confirmed and a slot exists for it, extended only by the part of those records that really
takes longer, such as review latency or an installation wait. Concurrency is read rather than
invented: use the limit the execution side states ([crw-run](../../crw-run/SKILL.md)), and where
nothing states one, place independent subjects together and record no `host` wait at all. Those
records set this subject's own target and nothing else: they produce no multiplier, no rate, and
no rule of days for another class, stage or product. Record which records were read as the
subject's delivery evidence, so a later reader sees that the target was read rather than chosen.

Where no comparable record exists, propose that same earliest day as the day the result is
expected to finish, mark the target `provisional`, record the delivery evidence as `no sample`,
and name the gap in the record that sets it, instead of padding the date until evidence appears.
It is deliberately the aggressive end rather than an estimate, and no precision is claimed for it.
Where it proves short, the subject reads late against it: a first record arriving is not one of
the reasons that move a target, so an optimistic day shows as lateness instead of disappearing
into a quiet extension.

Order comes from the prerequisites, and each target is then placed inside the dates already
approved around it: the project window it belongs to, the milestone it completes, and the results
it has to follow. Where the plan proposed the target instead of a person choosing it, the change
source names this plan request and the authority that approved the write, so a later reader is
never told that someone picked a date they never saw.

Where the plan write is already authorized, record the proposed dates in that same request rather
than asking for the same approval again. A proposal never becomes past performance: a `provisional`
target does not turn into an actual start or finish.

## Preserving what already happened

Read each subject's real start and finish from the evidence the contract admits for each of them,
and record the zone they were read in. Restore a first real start from the subject's own start
record, an explicit user statement, or a dated decision saying when it began. Never restore it from
a timestamp that appeared when someone corrected a status or ran a bulk edit, or from a pull request
landing, which dates the finish instead. Where nothing evidences the start, leave it empty rather
than moving the finish into its place. Completed and canceled work stays where it is: it is not
reopened, pulled into the new schedule, or given a fresh target so that a chart looks fuller than
the work is.

## Prerequisites at the level that is true

Derive edges from what each result actually consumes, then record them at the level the contract
requires: a whole project only when the whole project must finish, and otherwise the producing
issues or the milestone that groups them, so parallel work is not blocked by a wait it does not
have. Run the contract's four graph checks before writing and report what each one found. A defect is
settled before the graph is written: correct the edge or the date that is actually wrong and record
why, and where the available evidence cannot say which of them is wrong, leave that edge an
explicit unresolved planning dependency instead of dropping it or writing a graph already known to
be broken. Where the connector cannot write a relation, record the prerequisite as a line naming
both subjects.

An edge comes from two things and nothing else: a result this subject cannot finish without, and a
region two subjects would both have to change, which the closing check records as exactly one
ordering ([issue boundaries](issue-boundaries.md#close-the-plan)). Where neither of them needs the
other's result, either order is valid, so record the order chosen and the reason for it; an
unrecorded choice reads later as a dependency that never existed. Separate regions of one file are
not an edge. Delivery does not delete an edge either: once the contract a successor waited on has
landed, the relation stays as the record of what that successor needed and the active wait ends, so
the implementations and verifications behind it proceed together.

A level is also true when an authority decided it. Where the user puts a whole project behind
another result, that is a project-level prerequisite whose record line names the decision, and
every issue in the deferred project waits behind it: its preparation, its design, its API, its
screens and its verification alike. The parallelism inside that project is still recorded, and it
begins after the deferral is met rather than around it.

## Placing independent work in the same window

Subjects with no edge between them and no shared region are placed together in the earliest window
the result that frees them allows, up to the concurrency the execution side states. A stated limit
bounds what runs at once rather than what the window holds, and the two are read separately: a
subject with no free slot carries `host` until one opens, while its target stays inside the window
wherever the records show that slot freeing within it. Because these
targets are calendar dates, that usually reads as one date carrying several subjects, which is the
point: a plan that spreads independent work one subject to a day has recorded a wait that nothing
causes. Four shapes do exactly that, and none of them is written: placing subjects serially day by
day, holding a whole project until its batch is ready, giving each issue a fixed number of days,
and adding a buffer again at each stage when the records already carried that wait once.

A preparation step belongs inside the child that needs it. Where a subject is already running and
part of its work does not depend on what it waits on, that part runs first in the same child. It
becomes no issue, no relation and no second assignment, because a wish to parallelize never splits
an issue ([issue boundaries](issue-boundaries.md#decide-the-boundary)).

Waiting for a date to arrive when the work it depended on is already done is the padding this file
removes, so a confirmed result sends its successors back through the rule above in the same turn,
under [Re-evaluating at a checkpoint](#re-evaluating-at-a-checkpoint).

## Recording the schedule baseline and its changes

Write the schedule baseline, the current approved dates and every change into records Linear
already holds: the project or initiative update that reports the change, the planning or
coordination record the project already owns, and a dated comment on the changed subject. Each
entry carries the change's time, the deciding authority, the reason, the scope it covered with
the stable IDs added and canceled, and the before and after dates. The reason opens with the
change reason word the contract fixes, so a reader tells a pull-in from an extension without
parsing the prose behind it. A later replan adds an entry and never overwrites the earlier one, so
two plans stay comparable across added and canceled scope. When this write establishes a subject's
first target, the dated record it writes is that subject's baseline record and the target it sets
is the schedule baseline. Leave the schedule baseline `unconfirmed` only where a target already
exists and no record establishing it can be found, and never infer one from the current field. A
retry repairing this same operation is not that case: where the target landed and its record did
not, write the missing baseline record from this operation's own recorded time and target instead
of reclassifying a date this request just set.

Two baselines stay readable at once. The schedule baseline is the original one, and the current
approved baseline is the after value of the latest entry an authority approved, while the item's
own field is whatever it carries now. Where that field and the approved after value disagree, the
comparison against the field is unverified and both are reported until they are reconciled; the
drift does not erase what was approved.

Moving a target date does not settle a delay. The entry that moves it records what it moved from,
so a result that is late against the schedule baseline still reads as late after the move.

## Re-evaluating at a checkpoint

A checkpoint compares each subject with both datums, the schedule baseline and the current target,
and names which one it measured. What it does next follows the evidence rather than the calendar.

Where a result was confirmed earlier than its target, every successor whose earliest day moves and
whose target this plan may move is pulled in during that same turn, inside the schedule-change
scope the request already carries, with an entry whose reason opens `pulled in` and names the
confirmed result, its before and after dates, and the schedule baseline it still measures against.
A successor carrying a `confirmed` target keeps that date, because it is one this plan does not
move, and its earlier readiness is reported instead so the authority that set the date can decide.
Nothing waits for its own date once the work it depended on has landed.

Where a result is unfinished, nothing moves. A checkpoint having run, work still being in
progress, and a target approaching or passing are not reasons, and a target that rolls forward on
any of them hides the delay it was recording. The subject reads late against both datums and
carries its real wait cause instead. A later target is written only when the accepted scope
actually changed, a real blocker actually appeared, or a predecessor's own recorded move actually
shifted the critical path; the entry opens with `scope changed`, `blocked` or `critical path`,
names the scope, the blocker or the predecessor's entry, and leaves both baselines readable. A date
the deciding authority itself sets is the fourth case and opens with `authority`, whichever
direction it moves: it records that authority's own decision, such as an agreed deadline they
moved, and is never a reason this plan supplies for itself.
`critical path` carries only a predecessor's own recorded move: a predecessor that is merely late
leaves its successors' targets where they are, and they read late in turn, because the alternative
launders every extension through the first delay.

Two requests are refused rather than written. A second attempt to extend the same subject on a
reason already recorded or already refused, carrying no evidence that was not there the first time,
is drift, and it returns as an explicit replan need naming what has not changed since. A refusal
writes no entry, so the attempt it refused is what the next one is compared against, and a repeated
word that carries genuinely new evidence and a newly justified date is an ordinary change rather
than drift. Regenerating every target from the current date is refused whatever the request
calls it: a replan moves the subjects whose evidence actually changed and leaves every other
target and baseline where it stands.

Moving a target does not rewrite what was read. Where the move read the records again, its entry
names the records it read and they become the reading the subject carries; where it moved the date
without reading them, the earlier reading stays and that entry is what produced the date the
subject carries now. Either way a later reader can reproduce the current date, and no record is
credited with a date it did not set.

A checkpoint carrying no schedule-change authority writes none of this. It returns one replan
request to its parent naming the subjects, the confirmed or missing results, and the moves it
would make, under [Authority](#authority) below.

## A faster target changes no criterion

A target read from fast records buys nothing from the rest of the delivery. Review, required
checks, installation evidence and an unattended round trip are satisfied exactly as they were
before the target shortened ([Implementation Done](integrations.md#implementation-done)), and a
subject waiting on its review, a decision or a host constraint carries that wait cause and stays
unfinished. A wait is never recorded as a finish. Report a host constraint, a slow review and an
unmet prerequisite as what they are, because a schedule that hides them is the padded one facing
the other way.

What the records support is their own class at its own stage. They do not support a claim that
work is some number of times faster in general, a rate carried to another product, or a project
target computed by multiplying an issue count: a project's target is the latest target on its
critical path.

## Applying, reading back and reporting

Read each subject's current values immediately before writing, and preserve what the request did
not come to change: the user's own edits, the accepted design text, assignees and execution
state. Match existing milestones and records by stable ID and meaning before creating anything,
so running the same request again writes no second milestone and no duplicate entry. After
writing, read the dates, milestone links and relations back and compare them with what was sent.

Report a partial write as partial: name the subjects that landed with their IDs, repair the
remaining fields on those same IDs instead of creating the subject a second time, and look the
current state up before any retry. Where a view scale, an ordering or a project connection line
cannot be applied, report `recorded in the data` and `not applied in the view` separately rather than
implying the whole roadmap was drawn.

## Authority

A schedule change happens only inside the plan write authority the request already carries. A
check, consultation, plan-only, draft-only or read-only request returns these fields as a
proposal and writes none of them. An issue-only request proposes a parent schedule change instead
of writing it. Within an already approved full-plan scope, apply the schedule without asking for
the same approval again. A pull-in and an extension are both schedule changes, so both run under
that same authority and neither adds an approval step, and a checkpoint without it returns the
replan request above rather than writing one. A record owned by an active supervisor or parent is
returned to that owner under [supervisor, parent and child scope](integrations.md#supervisor-parent-and-child-scope)
rather than written over. Recording a date assigns nobody, starts no execution, installs nothing
and restarts nothing; those remain separate authorizations under
[Default independent execution](integrations.md#default-independent-execution).

## Judgment cases

Apply the rules above to each Given without reading its outcome, then compare. Outcomes are
counts, words and records, so an independent run either matches or differs visibly, and each row
names the clause it exercises, so an edit to a rule shows which case it breaks. Agreement between
an independent application and the recorded outcome is the check; matching words in this file is
not. A divergence is either an ambiguous rule or a wrong row, and both are fixed here.

D0 is the day the plan is written in the recorded zone, and a target means the end of that day.
Records show hours means the same-class records run from assignment to landing inside one day;
records show days means they do not. Each row carries the records its day is read from, because no
rule here states a period.

| # | Given | Plan outcome | Clause |
| --- | --- | --- | --- |
| 1 | Three issues, no edge between them, no shared region, the execution side states room for three, records show hours. | All three carry target D0 and wait cause `none`, all three in the startable batch; 0 serial day placements, 0 buffers added. | Placing independent work. |
| 2 | The same three where the execution side states room for two and the records show each of them finishing inside that window. | All three carry D0, because the slot the first pair frees opens again inside the window; the first two carry `none` and are the startable batch, and the third carries `host` naming the stated limit until that slot opens; 0 days added, 0 limits invented. | Concurrency is read, not created. |
| 3 | Prerequisite A is confirmed merged on the morning of D0; successor B's current target is D2 and `provisional`; the checkpoint carries schedule-change authority; records show hours. | B re-evaluated in that same turn to D0, with 1 entry opening `pulled in` naming A, before D2 and after D0, and the schedule baseline still D2 and readable; 0 entries overwritten, 0 waits for D2. | Checkpoint: pull-in. |
| 4 | The same as 3 with no schedule-change authority. | 0 writes; 1 replan request to the parent naming B and A's confirmed result; B still reads D2 in the data. | Authority. |
| 5 | Issue C waits on contract G, and C also needs a fixture that G does not affect. | The fixture runs first inside the child already holding C; 0 new issues, 0 new relations, 0 second assignments; C's wait cause stays `prerequisite` naming G. | Preparation inside the child. |
| 6 | Issues D and E change separate regions of one file and need nothing from each other. | 0 ordering relations, both carry D0 and `none`, both in the startable batch. | Separate regions are not an edge. |
| 7 | The same two where the regions overlap, neither needs the other's result, and the records show both finishing inside one window even when they run in turn. | Exactly 1 ordering relation, in whichever direction the plan records, with the reason for that choice recorded; both carry D0 and the later subject carries `shared surface` naming the region; 0 extra days, 0 unrecorded orderings. | A region two subjects change. |
| 8 | Contract G lands under an authorized plan write; the three implementations and the verification that waited on it are otherwise independent; each of the four carries a current target of D2 and an earliest day of D0 once G is confirmed. | The 4 relations stay as the record of what they needed; all 4 wait causes leave `prerequisite`; 4 moves to D0, each with 1 entry opening `pulled in` and naming G; 0 relations deleted, 0 subjects held for a second round, 0 baselines overwritten. | Delivery ends the wait, not the edge. |
| 9 | Successor F needs the API shape that decision issue G has not settled, and G is `awaiting authority`. | F is `undetermined` with wait cause `prerequisite` naming G and the decision as the event that ends it; 0 invented dates, 0 `provisional` targets assuming the decision. | Undetermined where the prerequisite carries no date. |
| 10 | The user states that the GUI project follows the port; the port targets D10; the GUI's design, API, screens and verification issues have no edges among themselves. | 1 project-level prerequisite on the GUI whose record line names the user's decision; every GUI issue carries `prerequisite`; 0 GUI issues in the startable batch and 0 placed before D10; their own parallelism recorded behind it. | An authority decides the level. |
| 11 | Issue H's pull request merged; its criteria include an installation nobody has approved. | H unfinished with its actual finish empty and wait cause `decision`; 0 target moves, 0 criteria dropped, 0 finishes recorded from the merge. | A wait is never a finish. |
| 12 | Same-class records show implementation landing within hours and review completing days after the pull request opens. | The target is read from the whole record and falls on the day review completes; while its delivery is waiting on that review the subject carries `review`, which also covers its required checks and the merge, and the word leaves once the merge lands; 0 review or check criteria reduced, 0 claims about another class. | The record runs to acceptance. |
| 13 | A class with no comparable record; prerequisites confirmed; a slot exists. | Target D0, `provisional`, delivery evidence `no sample`, the gap named in the record that sets it; 0 default periods, 0 `undetermined`, 0 precision claimed. | No sample. |
| 14 | Two comparable records match equally well; one landed in three hours, the other in nine. | The target follows the nine-hour record and the roadmap names the spread; 0 targets read from the faster run alone. | Which record the target follows. |
| 15 | The user adds a criterion to issue I on D1 under an authorized project plan write, and the records covering the added work put I's earliest day past its current target. | 1 move with 1 entry opening `scope changed`, carrying the IDs added and the before and after dates; the schedule baseline unchanged and still readable; 0 overwrites. Had the added work still fitted the current target, 0 moves and 0 entries; had the request carried no schedule-change authority, 0 writes and 1 proposal. | Recording a change. |
| 16 | A checkpoint on D3 finds I unfinished against its D1 target, with no scope change, no blocker and no predecessor move, and refuses the extension it was asked for; on D4 the same extension is requested again on the same reason and with no evidence that was not there on D3. | 0 target moves and 0 entries on either day; I reads late against both datums with its real wait cause; the D4 attempt is refused as drift and returns 1 explicit replan need naming what has not changed since D3. | No auto-roll; a repeated attempt is drift. |
| 17 | A request to rewrite every target from the current date. | 0 writes and a refusal; what is offered instead is a scoped replan naming a cause per subject, leaving every other target and baseline where it stands. | Regeneration refused. |
| 18 | Predecessor J's target moves later with `blocked`; K follows J with no room between them; L does not depend on J. | K moves with 1 entry opening `critical path` naming J's entry, its baseline preserved; L: 0 moves. | The critical path carries a legal move. |
| 19 | Predecessor J is merely late at its target; K depends on J. | 0 moves on either; both read late and K carries `prerequisite`; 0 `critical path` entries. | Lateness travels as lateness, not as dates. |
| 20 | A project of eight issues under an authorized project plan write, where seven are independent and the eighth follows one of them, and the records show the follower fitting inside the same window as the issue it follows. | The project target equals the follower's target, which is the latest on its critical path; 0 targets derived from the issue count, 0 counts multiplied by a sample, 0 speed claims recorded. | No multiplier. |
| 21 | An authority moves issue I's target later under an authorized plan write, and nobody reads the delivery records again. | 1 move with 1 entry opening `authority` and naming that decision; the delivery evidence stays the reading it already was, neither rewritten nor set to `no sample`, and that entry is what produced the date I now carries; the schedule baseline unchanged. | A move that reads no records. |
| 22 | Issue J's target was set with delivery evidence `no sample`; added scope makes the plan read two comparable records, which put J's earliest day past its current target. | 1 move with 1 entry opening `scope changed` that names the two records it read; the delivery evidence becomes that reading and is no longer `no sample`; the schedule baseline unchanged, and a later reader can reproduce the new date from the entry. | A move that reads the records again. |
| 23 | Prerequisite A is confirmed on the morning of D0 under an authorized checkpoint; successors B and C both carry D5, B's `confirmed` and C's `provisional`; with A confirmed the records and a free slot put each successor's earliest day at D0. | C moves to D0 with 1 entry opening `pulled in` naming A; B: 0 moves and 0 entries, its D5 kept and its earlier readiness reported to the authority that set it; 0 schedule baselines rewritten on either. | A confirmed successor is not pulled in. |
| 24 | An authority brings issue K's agreed deadline forward from D9 to D4 with no predecessor result behind it. | 1 move with 1 entry opening `authority` naming that decision; 0 `pulled in` entries and 0 confirmed results named; the schedule baseline unchanged. | `authority` moves in either direction. |
