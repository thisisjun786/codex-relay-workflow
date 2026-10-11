# Schedule contract

The names, sources and value words that every operation uses when it records or reads a schedule. `crw-plan` writes them, and `crw-check` and `crw-status` read them under these names.

## Schedule baseline contract

A recorded schedule creates facts that later operations read back: what a result was first
expected by, what it is expected by now, what actually happened, and what it waits on. Those
facts already live on Linear items and in the records around them, so this section fixes their
names, their sources and the checks over them rather than adding a second place to keep them.
Nothing here computes a date: there is no schedule database and no scheduler in this model.
`crw-plan` records these fields when a plan write is authorized, and any operation that later
compares progress with the plan, including [crw-check](../../crw-check/SKILL.md) and the
interim status reporting that consumes the same comparison, reads them under these names. A
record uses the field names as written here, and the value words below are fixed, because two
operations that paraphrase them stop agreeing about the same item.

| Field | What it holds, and where it is read from |
| --- | --- |
| Subject | The stable Linear ID and URL of one project, milestone or issue. A title, a branch or a checkout is not a subject. |
| Schedule baseline | The target date that the earliest record established for this subject, including the record a plan write creates at the moment it sets that subject's first target. It is `unconfirmed` only where a target already exists and no record establishing it can be found, and it is never filled in from the item's current date field or its creation time. |
| Baseline record | The record that set the schedule baseline, with its own timestamp: the identified project or initiative update, the planning record, or a dated decision comment. For a first target it is the dated record written alongside that target; for an older target whose establishing record cannot be found, its absence is what makes the schedule baseline `unconfirmed`. |
| Planned start | The date the subject is scheduled to begin, where the item carries one, which today is the project's own start date. It is a plan rather than an observation, so it never stands in for an actual start, and moving it follows the same change rule as a target. |
| Current target | The date the item carries now: the project's own target date, the milestone's target date, or the issue's due date. |
| Timezone | The IANA zone the dates were read and written in, stated rather than assumed. These targets are calendar dates, so a target means the end of that day in this zone and an evidence timestamp is compared in it. |
| Target nature | One of `confirmed`, `provisional`, `undetermined` or `awaiting authority`, recorded in the subject's own description because no Linear field carries it. |
| Actual start and finish | The real dates the work began and ended, each with the evidence that establishes it, and empty where no evidence does. |
| Change source | For the most recent change: its time, whose decision it was, the reason, the scope it covered including the stable IDs added and canceled with it, and the before and after dates, written into the record's own text. |
| Prerequisites | The subjects this one requires, each with the level it is recorded at, issue, milestone or project, and the relation or record line that carries it. |
| Delivery evidence | The same-class delivery records the item's target was last read from: their stable IDs and the times bounding each one, from assignment to the result that finished it, through the pull request and its review where that work had one and to the acceptance of its agreed artifact where it did not. It is `no sample` where no comparable record existed at that reading, and it holds those records rather than any figure derived from them. A change that reads the records again names them in its own entry and becomes the reading this field carries. A change that moves the date without reading them leaves the earlier reading here, and the change source is then what produced the date the item carries now. Records are never rewritten to claim a date they did not set, and `no sample` never stands for a date another source moved. |
| Wait cause | What the subject waits on now, one of `none`, `prerequisite`, `shared surface`, `review`, `decision` or `host`, named together with the subject, region, pull request, decision or stated constraint it waits on and the observable event that ends it. |
| Change reason | The word the most recent change's reason opens with, one of `pulled in`, `scope changed`, `blocked`, `critical path` or `authority`. It classifies the change the change source already records and replaces none of its text. |

`confirmed` is a delivery date an authority agreed to. `provisional` is a planning target the plan
proposed and may move. `undetermined` is an in-scope subject with no target yet. `awaiting authority`
is a subject whose date needs a decision nobody has made. An actual start date is none of these:
it is an observed fact about work that began, it never becomes a target, and a subject that has
started still carries a target nature of its own.

The wait cause words divide the same way. `none` means nothing holds the subject back now, and
whether it has begun is read from its execution state rather than from this word, so the
immediately startable batch is the subjects carrying `none` that are not already reported started.
`prerequisite` names a result this subject cannot finish without, including a whole project an
authority placed behind another. `shared surface` names a region another subject must change
first. `review` covers the subject's own delivery waiting on its review, on the checks required of
it, or on the merge that has not happened. `decision` covers an approval nobody has given, an
installation sign-off and the user's own stop. `host` covers a stated constraint or a slot the
plan cannot create. A wait cause describes the subject carrying it and never its predecessor, so a
subject waiting on a result that is itself in review carries `prerequisite` while that predecessor
carries `review`; where two words still fit, the cause that must resolve first wins and a tie
breaks in the order listed here.

The change reason words are fixed in the same way, and each names the cause rather than the
direction, because the same cause can move a date either way. `scope changed` is a move the
accepted scope's own change produced, including scope canceled from it, which can bring a date
forward. `blocked` names a real blocker on the subject itself, and `critical path` a predecessor's
own recorded move that shifted it. `pulled in` is the earlier move the plan makes after a confirmed
result. `authority` is a date the deciding authority itself set, in either direction, because an
authority can bring a deadline forward with no predecessor result behind it. Where more than one
fits, the first that applies wins, in the order `scope changed`, `blocked`, `critical path`,
`pulled in`, `authority`, so scope an authority approved reads as `scope changed` and a blocker
reads as `blocked` on the subject it blocks and as `critical path` on the successors its recorded
move shifted. Three facts are not change reasons at all: that a checkpoint ran, that the work is
still unfinished, and that the target is approaching or has passed. A target that moves on one of
those records a delay it is hiding.

Records disagree, so their precedence is fixed. The item's own date fields are the planned start and
the current target.
The most recent record carrying a date change is the change source. The earliest record that
established a target is the schedule baseline. The subject's description carries its target nature
and timezone note and nothing that competes with those fields. Each record states its before and
after dates in its own text, so an operation that cannot see a rendered diff still reads them.
Where the current target disagrees with the latest change record's after value, the comparison is
unverified until the two are reconciled; neither side is silently preferred.

A start and a finish take different evidence. An actual finish is evidenced by the landing of the
delivering pull request under [Implementation Done](linear-model.md#implementation-done), by an explicit user
statement, or by a dated decision record. An actual start is evidenced only by a record of the work
beginning: the subject's own start record, an explicit user statement, or a dated decision saying
when it began. A landing dates the finish and says nothing about the beginning, so it never fills an
actual start, and an actual start nothing evidences stays empty rather than borrowing the finish. A
status timestamp that a status correction or a bulk edit produced evidences neither, because it
records when someone fixed the register rather than when the work happened; where two admissible
dates disagree the earlier evidenced one stands. A subject already Done or Canceled keeps the dates it
has and is not given a new target.

A prerequisite is recorded at the level that is actually true. A project-level prerequisite means
the whole project must finish first. When a result needs only particular outputs, the prerequisite
belongs on the issues that produce them or on the milestone that groups them, so the remaining
work in both projects keeps running in parallel. Cross-project prerequisites travel by relation
under [scope of each level](hierarchy.md#scope-of-each-level); one project does
not absorb another's issues to express a date, and no finish-to-start relation is invented to make
a chart read better.

Four defects are checked over the resulting graph, and each check reports what it found including
when it found nothing: a cycle among prerequisites; a prerequisite whose target falls after the
target of the result that needs it; an issue carrying a milestone's target while sitting outside
that milestone; and a required prerequisite absent from the graph entirely.

A comparison built on these fields names which datum it measured against, the schedule baseline or
the current target, and reports both where a reader needs both; measuring against one and
presenting it as the other is what makes a moved date look like progress. Naming the verdicts that
comparison reaches belongs to the operation performing it rather than to this contract, so the same
fields never acquire two meanings.

Two report states are named, because an operation reading the result must classify the same fact
the same way. `recorded in the data` means the dates, links and relations were written and read
back. `not applied in the view` means a scale, an ordering or a connection line the connector does
not expose was left alone. Neither stands for the other.

Reading this contract grants nothing. Recording a schedule stays inside the plan write authority
the request already carries; a check-only or draft-only request returns these fields as a proposal
and writes none of them; and a registered date is not an assignment, an execution start, an
installation or a restart. The Plan-side procedure that builds, checks and records these fields is
[Schedule and dependency roadmap](scheduling.md).

A consumer judges the result rather than its wording: every in-scope subject has a row or an
explicit `undetermined`; what was read back equals what was written; running the same request again
adds no milestone and no second record; a Done or Canceled subject's dates are unchanged; and a
target date that moved still shows the schedule baseline it moved from together with the change
source that moved it. Every in-scope subject also carries a wait cause; a target that moved later
carries one of the extension words with the scope, blocker or predecessor entry it names; a target
that moved at all carries the word for what moved it, whichever direction it went, naming the scope
change, the blocker, the predecessor's entry, the confirmed result behind a `pulled in`, or the
authority's own decision; and a schedule whose
targets were all rewritten from one current date fails whatever the request that produced it was
called.
