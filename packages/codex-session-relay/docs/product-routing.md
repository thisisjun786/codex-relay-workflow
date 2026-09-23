# Product routing

The fault ledger ([faults.md](faults.md)) turns a breakage of this relay into one record and one
Linear issue. Product routing does the same for the products Jun builds or uses through Codex. It
covers a tool failure in a CRW-managed run of another repository, a failing verification, a
user's report, and an error a product explicitly forwards from real use. Each of these used to
reach Linear only when somebody noticed it, copied it and filed it. Filing by hand is how defects
ended up in the CRW team, in no project, or twice.

Routing decides WHERE an incident belongs. It does not store faults, decide thresholds, file
issues or count writes; the ledger does all of that, and routing reaches it through one adapter,
`ledger_port.py`. Nothing here performs a network call, dispatches work, or calls a model.

## The incident

Every surface enters in one shape, `product-incident/1`:

```json
{
  "schema": "product-incident/1",
  "product": "beta-meter",
  "repository": "example-org/beta-meter",
  "surface": "real_use",
  "phase": "in_use",
  "component": "billing",
  "symptom": "charge_twice",
  "severity": "broken",
  "origin": "observed",
  "occurrenceKey": "event:7f3a",
  "observedAt": "2026-09-23T04:10:00+00:00",
  "context": {"currentIssue": null, "run": null, "session": null, "regressionOf": null},
  "detail": {"impact": "customers charged twice", "expected": "one charge",
             "actual": "two charges", "reproduction": "pay with a retried card"},
  "evidence": [{"kind": "event", "ref": "forwarded:7f3a", "observed": {"count": 2}}]
}
```

The key set is closed. An incident carrying anything else is refused rather than stored, because
an intake that kept whatever it was handed would be exactly the indiscriminate collection this
feature must not do. `component` and `symptom` are keys, not prose: they decide identity, and
two products describing a failure in similar words stay two failures.

`surface` is one of `dev_run`, `verification`, `user_report` and `real_use`. Each product's
registry record says which of them are watched and how they are collected. An incident from a
surface the product does not watch is refused before anything is written, and `product-show`
lists every surface, so one nobody connected reads **unobserved** rather than quiet.

Severity is the observing source's own reading. Sources are on the harness side: CRW-managed runs
and events a product explicitly connected. Classification can later name the product, component
or goal; it never sets severity, because the ledger keeps the highest severity it has seen and
nothing could lower one a classifier raised.

## Where it belongs

Routing reads two local snapshots a credential holder keeps current: the **registry** (per
product: Linear workspace, team, family label, repositories, watched surfaces, triage project,
test target) and the **bindings** (projects and issues read back from Linear, with the
components and symptom keys they cover, their state, the fix that closed them and what they
follow up). From those readings alone it decides, in this order:

| Order | Condition | Disposition |
| --- | --- | --- |
| 1 | an expected state: cancelled, awaiting approval, recorded as unsupported | **observe**: recorded for an operator, never filed |
| 2 | exactly one open issue covers the component AND the symptom | **accumulate** on that issue |
| 3 | exactly one completed issue covers them | **reopen** that issue and comment the recurrence |
| 4 | the incident comes from the current issue's own managed run and its component is in that issue's scope | **attach** as failure evidence to the current issue |
| 5 | `regressionOf` names the fix of exactly one completed issue | **follow-up**: a new issue linked to it |
| 6 | otherwise | **new issue** in the one active project covering the component, else the triage project |

A component alone never makes two defects one: a shared component is two defects that happen to
live near each other. Several open issues claiming one symptom, several projects covering one
component with no goal to choose between them, or an owner issue that belongs to no project and a
product with no triage project are **held** with the reason, and surfaced as decisions. Routing
never picks the candidate that sorts first.

"The current issue's own managed run" is checked against this store. `context.run` must be a
relationship the relay registered, and its `issue_key` must equal `context.currentIssue`. A user
report or a real-use event never attaches to a current issue, however its component reads.

A defect is filed under the product it belongs to, not the product that saw it. A tool failure in
a CRW-managed run of another repository goes to that repository's product and team. CRW is one
registered product among others, and nothing defaults to it.

### No issue without its project

Issues are filed only into a scope whose ledger target names a project. A product with no suitable
project and no triage project files nothing: its incidents are held, and the next binding of a
project decides them again from their stored input. When a created issue's project reads back
different from its target, the ledger keeps the issue and repairs the link on the same issue id.
A family label, a team or a relation never stands in for the project.

Labels follow the operating model: an issue carries the repository label of the repository it is
about, and the product-family label belongs on the project. The ledger's create carries no label,
so an issue routing creates owes its repository label as an update once the create confirms. An
issue routing adopts belongs to someone else, and routing adds no label to it.

## Unclear ownership

An incident whose product cannot be resolved is kept as ONE pending-classification record. That
happens when it names an unregistered product, or a repository registered to no product or to
several. The record lives under the ledger product `unclassified` at notice severity, so it can
never be filed, and it keeps the incident's own severity beside it. A severe incident waiting for
an owner is a decision for Jun, never a Linear issue in an arbitrary team.

`route-classify` names the product, and optionally the component, symptom or goal, from an
operator or from a bounded model judgement recorded with who made it. The stored incidents are
replayed under that product, the pending record is withdrawn, and later incidents with the same
pending identity are forwarded to the classified product. Replay counts at most the sixteen newest
stored incidents, so the new record's occurrence count is a count of replayed observations.

Identity carries the Linear workspace. A resolved product uses its registry workspace, and an
incident declaring another is refused. A pending incident uses the workspace it declares, or
`unassigned`, so two workspaces never share a pending record.

## Shared causes

When an incident names a cause in another product, typically a CRW fault that broke a product's
run, the cause is verified first. The fault must exist, belong to the named product, and match
the signature the incident gives for it. An unverified cause is held and merges nothing.

A verified cause produces two records, linked once both own issues. The cause fault gains an
occurrence at its own current severity, with evidence naming the affected product. The affected
product's own defect is routed as above at its own severity. A severe impact in one product
therefore reaches that product's team, and does not escalate a CRW record that its own observers
judged minor. A cause that was resolved and comes back is reopened by its new occurrence, which
is intended.

## Projects

A project is created only under an explicitly configured `project_creation` policy whose basis
names this request. Creation needs no suitable project for the members' components, plus at least
`minIndependentFixes` (two or more) distinct held defects sharing one declared user goal with
completion criteria. Issue, file or error counts alone never create one, and a single defect goes
into its product's existing suitable project.

The proposal is a `project_needed` record of the product, recorded at notice under the scope
`__projects__` (a target with the product's team and no project), so it can never file an issue
itself. The create is queued on it explicitly as a ledger publication of routing's own
`project_create` kind. A create kind has one write per record: a goal that qualifies again
after its create was cancelled revives that write under the same id with the new members, and the
pre-issue check runs again before it is issued. The kind gets the ledger's single-create,
uncertain, reconcile and readback rules unchanged, and two evaluations of the same goal converge
on one record and one write.

Immediately before the write is issued, the kind's pre-issue check recomputes the whole predicate
from this store inside the ledger's own transaction: the policy still enabled, enough members
still held for want of a project under that goal, and no active project bound meanwhile that
covers a member's component. Any failure cancels the unissued write and nothing is created.

When the create confirms, `route-reconcile` binds the created project, and so does every digest
before it reports anything. Binding decides the held members again, so they move into it, all in
one transaction. The proposal records which project it became. Once no create of it is
outstanding, because its project is bound or every create it queued was cancelled, the proposal
is settled and leaves the filed stage. Nothing that looks for outstanding work reads it again,
and a later evaluation that queues a new create files it again.

### Holder protocol

Routing writes nothing to Linear. The credential holder performs every write with the ledger's own
holder commands (`fault-next`, `fault-claim`, `fault-operation`, `fault-complete`, `fault-fail`,
`fault-reconcile`) and passes `--kind-module codex_session_relay.projects` to each. Importing that
module registers routing's fault classes and the `project_create` kind in the holder's process.
Without it the ledger refuses a `project_create` write as unregistered, so no process can skip
the pre-issue check.

Completing a `project_create` write takes the created project's id as the external reference,
plus the fields read back from the project, which must include its `team`. A readback that
does not show the team, or shows another one, is refused. Otherwise a project made somewhere
else would be bound to the product on faith, and every member defect would follow it there.

What a route owes after its fault owns an issue is discharged by `route-reconcile`. That covers
the repository label of a created issue, the reopen of an adopted completed issue, the relation
from a follow-up to the issue whose fix regressed, and the relation between a shared cause and
the affected product's record. Each is queued once as the ledger's idempotent update, and only
when both ends own issues. A create confirmed through the raw holder command is therefore still
linked without another intake. `route-reconcile` reads at most `--limit` filed routes and
answers `next`; passing it back as `--after` continues from there. The digest reconciles each
route it reads.

## Completion checks

`completion-check` compares what a subject claims with what was observed, inside the bounds of
what can be observed:

| Check | Applies when | Mismatch |
| --- | --- | --- |
| acceptance | the issue is Done | the agreed acceptance evidence is absent or failed |
| install, realUse | the PR merged | the result is required and absent |
| handoff | the session ended | the artifact or handoff is required and absent |
| recurrence | always | a fault the subject owns recurred after its newest fix |

A requirement is required only when the reading says so. Deployment is never assumed mandatory,
and an unknown requirement or an unobservable result is **unverified**, recorded under its own
notice identity rather than guessed. A follow-up split counts as an exception only when read back
from Linear: the follow-up is a different, open issue of the product that lists this subject and
this check among what it took over. An approved scope reduction counts only with the approval and
its reference. Anything else is **exception_unverified**.

The subject must be an issue bound to the product as read back, because a mismatch is filed as a
re-verification demand on the subject issue itself. It is a `completion_mismatch` record,
one per subject and check, adopted by the subject issue and recorded at degraded. The class
declares a threshold of one, since a reading that looked for the evidence and did not find it is
the whole proof. Under that default the first reading files, and its first write is a comment on
the subject, never a new issue. The ledger's policy stays authoritative: a product that raises
its own degraded threshold for `completion_mismatch` waits for that many readings, as for any
class. Until then the mismatch is recorded, the subject's adoption is stored, and
`route-show --attention` and the digest list it as `completion_mismatch_open`. The answer
reports each recorded mismatch's ledger state (`observed` recorded, `open` filed). Completion
checks queue no update of any kind. A mismatch found again after its record was
resolved starts a new round, a new record for the same subject and check adopted the same way.
Were it the old record coming back, the ledger's rule for a resolved fault that recurs would
queue a reopen of the subject. A completion check never changes the subject's state, so the new
round's first write is again a comment. After twenty closed rounds the check refuses, and the
decision goes to a person. A replay of a reading one of those rounds recorded is still recognised
first.

A reading that already failed on a closed round is recognised when it is handed in again. A retry
of an old reading is old evidence, so it opens no round. Each round keeps every failing reading it
recorded for that comparison, one small row each, so how many readings came after it does not
matter.

A mismatch closes only on evidence. A later reading can carry a fix reference and a verification
reference; then the fix is recorded, a reverification that passed is recorded, and the record is
resolved. An approved exception closes it the same way, with the exception as the fix. A
requirement quietly dropped after a mismatch keeps it open. An unverified check is cleared by a
later reading that establishes it either way. A reading where every check now agrees, but an open
mismatch still lacks its fix and verification references, answers `closure_pending` rather than
`consistent`: nothing is wrong any more, and nothing is closed. A legitimate Done produces no
write at all.

Recurrence is read from the ledger. A defect the subject owns is recurring when it is open again
after a fix in its current cycle, or open in a cycle after a resolution. The ledger has already
commented on it, so the check reports the recurrence and never files it again. A check reads at
most a thousand of the product's defect records; past that, recurrence is unverified.

## Reporting

`route-show --attention` lists what needs somebody. Each route waits on at most one decision,
named the same way everywhere:

| Decision | When |
| --- | --- |
| `awaiting_classification` | a pending-classification record |
| `held_<hold>` | a held route, with its hold |
| `link_incomplete` | the owned issue's project link reads back unlinked |
| `completion_mismatch_open` | an open completion mismatch |
| `project_proposed` | a project proposal whose project is not bound yet |

`route-digest` answers a midpoint check. It first binds the projects that confirmed creates
made. Binding one moves its member defects wherever they sit in the listing, and reporting a
member as held in the same answer would announce a decision already made. It checks at most
`--limit` outstanding proposals, least recently checked first, and moves each to the back of the
rotation. A proposal waiting on a slow create therefore cannot keep a later confirmed one from
being bound: successive digests reach every one. A defect held for want of a project whose own
goal has an outstanding proposal this digest did not reach is reported by the digest that
reaches it; `proposalsUnreached` counts those proposals. Then, for each route it reads, it
discharges what that route owes, reads the route again, and compares it with the snapshot it
last reported. It answers only what changed: new severe records, new decisions,
resolutions of records that owned an issue, and routine accumulation summarized per product.
Each new decision is also raised as a ledger notification under its
decision name. The ledger keeps one notification per record and reason, so a decision is
announced once however often digests run. A severe pending incident or a severe hold is raised
at intake under the same name. Asked again with nothing changed, the digest answers quiet and
reports nothing. The only thing it writes then is the rotation position of each outstanding
project proposal it checked. It runs only when somebody asks, and it calls no model.

## What this does not claim

It sees what its inputs show. A product surface nobody connected is unobserved, and an incident
nobody forwarded does not exist here. It does not claim detection inside products it cannot
observe, and product-specific instrumentation belongs to that product's repository.

Filing is not approval. Every filed record says execution is not approved. Creation, execution
approval, assignment, the fix and the reverification are separate steps, and nothing here starts
any of them.

A queued write is not an issue anybody has written, and a confirmed one is not an issue anybody
read.

## Tables

| Table | What it holds |
| --- | --- |
| `product_registry` | one validated registry record per product |
| `product_bindings` | projects and issues as read back from Linear |
| `routing_policy` | the explicit project creation policy and its basis |
| `incident_routes` | per routed fault: disposition, stage, target, hold, origin, classification, the last reported snapshot |
| `route_incidents` | the newest incidents per route, the input redecide and classification replay read |

## Commands

```
product-register  --record <json|@path>
product-bind      --record <json|@path>
product-show      [--product <key>]
route-policy      --record <json|@path>
route-intake      --incident <json|@path>
route-classify    --fault <id> --classification <json|@path>
route-reconcile   [--product <key>] [--limit <n>] [--after <next>]
route-show        [--product <key>] [--attention] [--limit <n>] [--after <next>]
route-digest      [--limit <n>] [--after <next>]
route-projects    --product <key>
completion-check  --reading <json|@path>
```

`product-bind` also decides again every held route of the product, and every filed one whose
fault owns no issue yet, from its latest stored incident. It does this in the same transaction as
the binding, so a binding whose consequences were refused is not kept. Its answer lists what
changed.

Status: the registry, the decision and the completion verdicts do not depend on the ledger. The
paths that record, adopt, target, move, update or queue go through `ledger_port.py`, which binds
only when CRW-205's corrected ledger contract is present, every function and keyword it passes
included. Until then every route command except the registry ones refuses with
`route_ledger_pending` before writing anything. An intake from a surface the product does not
watch is refused first, since that check needs no ledger. Nothing here is evidence about an
installed runtime, a live service, or anything written to Linear.
