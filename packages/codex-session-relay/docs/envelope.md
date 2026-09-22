# The shared envelope, and what is owed upward

Two relations carry messages here. A parent and its child exchange a completion and a
correction, and that pair has a queue, a receipt, an acknowledgement and a verdict. A
supervisor and a parent exchange instructions and reports, and that pair has none of those.

`envelope.py` owns what they share. `supervision.py` owns what the level above is still owed.
Neither adds a table. Every value is derived from rows another module already writes, which is
what lets the same reading survive a restart with nothing remembered; the one thing either of
them writes is a journal entry saying a report was produced, which is how a second reading of one
fact finds the first report instead of making another.

## relay-envelope/1

A **kind** says what the recipient owes. A **request** owes an answer from the recipient, a
**decision** owes one from the user and from nobody else, a **notification** owes none, and a
**status_response** answers a question somebody asked. The kind is what makes a message worth a
turn at all; whether one is actually produced is `supervision.select`, which also reads whether
the fact is already in the record, whether a report was already produced for it, and whether the
recipient can be reached.

A **purpose** is why the message was sent, and each direction has its own set. The supervisor
sends a project assignment, a midpoint check, a resume, a scope correction, a relayed decision
or a user stop. The parent sends a completion, a block, a decision request or a status
response. A purpose belongs to one direction and carries one kind, so a caller cannot label a
completion as an instruction: `kind_of` derives it and refuses a pairing that does not exist.

A relayed decision is a request rather than a decision. Jun has already decided; what the
parent owes is the application, not another opinion.

### Two identifiers

`messageId` is derived from the logical facts - direction, relation, purpose and subject - and
stays put across retries, restarts and second readings. `requestId` is `identity.request_id`,
which renders `del-<event>-a<attempt>` and is supposed to move, because it identifies one
transport attempt.

The Linear scope is deliberately not an input to either. It is a display field a reader may or
may not be able to resolve, and feeding it in would make the identity depend on whether the
caller happened to hold a scope row.

### Three absences

`inherited` means the value lives one level up and is not restated. `unknown` means nothing
readable said. `not_applicable` means this kind has no such thing. A field is never filled
with a plausible value to avoid one of the three, and `shown()` prints the reason rather than a
blank, because a blank reads as a value nobody bothered to fill in.

`observedAt` is not required, which is deliberate: `report.py` composes a message before its
row exists, to find out whether a restoration block would survive the composition, and at that
moment nothing has recorded a time.

### The five stages, per direction

| Direction | transport_accepted | received | agreed | applied | verified |
|---|---|---|---|---|---|
| child to parent, completion | `attempts` | `acks` | `acks` | `verdicts` | `verdicts` |
| parent to child, revision request | `attempts` | none | none | `events` | `verdicts` |
| supervisor to parent, directive | none | `scope_directives` | `scope_directives` | none | none |
| parent to supervisor | none | none | none | none | none |

The halves differ because the directions differ. A child's completion is acknowledged;
`ack.acknowledge` exists for exactly that message and refuses a revision delivery. What shows a
correction was applied is the completion receipt of the generation it opened.

Where a column says none, the stage answers `not_applicable` and not `unmeasured`. The two are
different news: unmeasured says go and look, not_applicable says there is nothing to look at.
`check_reach` enforces the table, so a caller cannot hand the contract a supervisor
acknowledgement sourced from `acks` and have it written down.

A stage answers `yes`, `no`, `conditional`, `unmeasured` or `not_applicable`, and a yes, no or
conditional needs the record that says so. `conditional` is never inferred: `ack.acknowledge`
clears the rejection reason on acceptance and requires one on rejection, so that column is
evidence of a rejection and not of a qualified yes.

### The directive pointer

`scope_directives` has a nullable, format-free `reference` column and no message kind of its
own - `link_kind` says execution or reference, which is hierarchy authority and a different
question. So the envelope rides there as a pointer and the row's own columns stay the identity.

`record_directive` checks it by RE-DERIVING the message id from the row's link and digest
rather than by comparing copies: the pointer carries no duplicate of anything, so there is
nothing to drift, and a pointer that derives something else belongs to another instruction and
is refused with the contest retained. An operator's note in that column, and any directive
written before this contract, are stored and read exactly as they are, with an unknown kind.

## supervisor-obligation/1

Three things are news for the level above: a completion, a new real block, and a decision only
the user can make. The kind comes from the child's own report status - done and noop are
completions, blocked and budget-exhausted are blocks, unsafe and needs-human are decisions -
and from the outcome only when no report exists. A failed or interrupted turn raises nothing:
how a turn ended is the parent's business and re-dispatching it is ordinary work.

An obligation id is derived from the kind, the relation and the subject, so one fact yields one
obligation however many times it is read, across a restart or a service replacement.

### What this does not claim

That a supervisor received anything. No row in this store could say so: `deliveries` holds one
recipient per event and serves the registered parent-child pair, `ack.acknowledge` records a
parent acknowledging a child completion, and the relay carries no supervisor channel at all.
Convergence means one fact yields one obligation, not that a second wake was prevented
somewhere else.

### What discharges one

The Linear record the supervisor reads for itself, and only at `confirmed` - the state reached
after a readback verified what was written. It has to be a `verdict` row, because a confirmed
progress note is a real record about the same event and is different news, and it has to be in
the document currently configured for that relationship, because a confirmation written to a
target since repointed is a record in a place nobody is reading.

`pending`, `claimed`, `written` and `failed` all leave the obligation standing. `failed` is
worth naming: `SyncOutbox.fail` reaches it after eight attempts and drops the row out of
automatic selection, so it looks terminal while meaning the opposite of done.

A report that was PRODUCED is recorded in the journal under `supervisor_report`, keyed on the
obligation id, and suppresses the next wake for the same fact. That is a separate answer from
whether the obligation is discharged, and both are returned: a report whose Linear write failed
is suppressed as a duplicate AND still standing.

### A report nobody wrote

A turn that ended without reporting emits no event, so no query over this store can find it.
The reading comes from `omitted.observe`, which needs a marker root and explicit selectors, and
is passed in. Only `reportingState == unreported` raises an obligation. `unmeasured` is carried
as a gap instead, because a failed evidence read is the absence of an answer rather than
evidence that a report is owed, and `reported`, `in_progress` and `unmanaged` raise nothing.

### Enumeration

`standing_for` reads every relationship in a project, including superseded ones. A real
handover registers a successor and supersedes the row it replaces, so filtering on status would
drop exactly the obligations a replacement owner most needs to see. Each obligation carries the
status of the row it came from.

An obligation is raised about the issue it came from and is never promoted into a statement
about the project. `AssignmentView.project_state` owns that question and its strongest word is
`complete_candidate`.

### An explicit request is its own path

`status_answer` is reached by a question somebody asked. Automatic suppression does not silence
it, because skipping it would answer a different question from the one that was put.

## Scope of these claims

Source-implemented and covered by this package's own suite:
`tests/test_supervisor_envelope.py` and `tests/test_supervisor_reporting.py`. That is evidence
about this source, not about an installed runtime, an activated service, or any message
reaching any supervisor on any host. No supervisor round trip is claimed here, because the
channel for one does not exist.
