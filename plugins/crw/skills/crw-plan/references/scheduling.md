# Schedule and dependency roadmap

How `crw-plan` builds, checks and records a schedule. The field names, their sources, the
precedence between records, the evidence rules and the four graph checks belong to the shared
[Schedule baseline contract](integrations.md#schedule-baseline-contract); this file applies them
and does not restate them. Read it when a request covers a project or initiative plan, or a
scoped update to a schedule that already exists.

## What the default result carries

An authorized project or initiative plan write carries its schedule instead of waiting to be
asked for one: the project's start and target dates, milestones named after the result they
finish with their target dates, each issue's milestone link and the target date it actually
needs, the prerequisites that really hold between them, and the areas that can proceed in
parallel. Name the subjects that have no date and why, rather than leaving the schedule silent
about them.

Scope holds while doing it. A request that updates one issue reads the initiative and project as
context and proposes any parent schedule change without writing it. A scoped project update
covers that project's own subjects. Neither becomes a reason to move dates the request never
mentioned.

## Proposing a target without estimating

When the request states no dates, propose a short operating target that fits the scope in hand
and the schedule already recorded around it, mark it `provisional`, and name whose decision set
it. Nothing here estimates duration: no cycle length, sprint, default period or model-produced
remaining time becomes a target, and a horizon that suited one product is an example rather than
the next product's default. A delivery date the user states is `confirmed` and a proposal does not
replace it. A subject whose date needs a decision nobody has made is `awaiting authority`, and one
with no target yet is `undetermined`; both are reported plainly instead of filled in.

Order comes from the prerequisites, and each target is then placed inside the dates already
approved around it: the project window it belongs to, the milestone it completes, and the results
it has to follow. Where nothing constrains a date that way, leave the subject `undetermined`
rather than choosing a length for it. Where the plan proposed the target instead of a person
choosing it, the change source names this plan request and the authority that approved the write,
so a later reader is never told that someone picked a date they never saw.

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

## Recording the schedule baseline and its changes

Write the schedule baseline, the current approved dates and every change into records Linear
already holds: the project or initiative update that reports the change, the planning or
coordination record the project already owns, and a dated comment on the changed subject. Each
entry carries the change's time, the deciding authority, the reason, the scope it covered with
the stable IDs added and canceled, and the before and after dates. A later replan adds an entry
and never overwrites the earlier one, so two plans stay comparable across added and canceled
scope. When this write establishes a subject's first target, the dated record it writes is that
subject's baseline record and the target it sets is the schedule baseline. Leave the schedule
baseline `unconfirmed` only where a target already exists and no record establishing it can be found,
and never infer one from the current field. A retry repairing this same operation is not that case:
where the target landed and its record did not, write the missing baseline record from this
operation's own recorded time and target instead of reclassifying a date this request just set.

Moving a target date does not settle a delay. The entry that moves it records what it moved from,
so a result that is late against the schedule baseline still reads as late after the move.

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
the same approval again. A record owned by an active supervisor or parent is returned to that
owner under [supervisor, parent and child scope](integrations.md#supervisor-parent-and-child-scope)
rather than written over. Recording a date assigns nobody, starts no execution, installs nothing
and restarts nothing; those remain separate authorizations under
[Default independent execution](integrations.md#default-independent-execution).
