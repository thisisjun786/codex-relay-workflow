# Operational faults

When the machinery between a child, its parent and the supervisor breaks, somebody has to
notice. Until now that somebody was the user: they read a stalled project, asked what had
happened, and then asked for an issue to be filed. This removes both steps. The daemon reads
the breakages this store can already see, converges every repeated observation of one breakage
onto one record, and hands a caller that holds a Linear connector the exact write to make.

Nothing here performs a network call, for the same reason [the synchronisation
outbox](../src/codex_session_relay/sync.py) does not: the relay records WHAT must be written
and WHICH fact it belongs to, and the process holding the credential does the writing and
reports back. That separation is what lets a Linear failure be retried on its own without
re-deriving the diagnosis it describes.

## Corrected contract (CRW-205 follow-up)

Published ahead of its implementation on codex/crw-205-review-fixes so a consumer can bind to it
early. It corrects the merged contract where the post-merge independent review found it wrong or
short, and adds what CRW-206 needs from the ledger. Everything here is normative; where it
disagrees with an earlier section of this document, this section wins.

### Invariants

Every rule below is an instance of one of these. Each names the one function that enforces it;
a path that reaches the outcome without passing through that function is a defect.

1. **One issue per fault.** Only `open_record` creates a fault's issue; the slot is taken by an
   owned issue or a non-cancelled `open_record`, and there is one `open_record` row per fault,
   ever. Enforced by `_issue_slot()`, called from `next()`, `claim()`, `operation()`, `retry()`,
   `queue()`, `adopt()` and revival.
2. **An issued create is never repeated on a guess.** Handing out an operation marks it issued; a
   lapse or failure after that makes it uncertain; only `reconcile()` with an attested end of the
   request frees it. Enforced by `operation()` and `reconcile()`.
3. **Transitions belong to the claim.** Operation, completion and failure need the current claim
   token; the first claimant is the writer and another needs a recorded takeover. Enforced by
   `_claimed()`.
4. **Only unissued writes are cancelled, and cancelling spends nothing earlier.** Pending, failed
   and claimed-not-issued rows only; a claimed one refunds its own attempt and budget. Enforced by
   `cancel()`, the only path that cancels (adoption, withdrawal, pre-issue cancel and
   `set_project` supersession all call it).
5. **A clear withdraws what nothing landed for.** Owning an issue is not a write having landed.
   Enforced by `_landed()` inside `_transition()`.
6. **A cause that comes back is a new occurrence.** The first active observation after a clear
   opens an episode. Enforced by `record()`.
7. **One product per scope key.** Every assignment of a scope key refuses a key another product's
   fault carries. Enforced by `_assign_scope_key()` for `record()`, `move()`, `adopt()` and
   `set_target()`.
8. **A write goes only to its product's current target.** Targets are owned by the product that
   set them; a legacy row with no owner never issues; `operation()` re-checks the current owned
   target before any kind's own check. Enforced by `_owned_target()`.
9. **One record per failure per workspace.** Every fault id a caller hands in, and every alias
   registration, resolves through `canonical_id()` (aliases and the legacy workspace lookup).
   Enforced by `_canonical()`.
10. **One rescope step.** Every scope change re-points unsent writes, keeps uncertain ones, and
    relinks an owned issue. Enforced by `_rescope()` for `record()`, `move()` and `adopt()`.
11. **The issue ends on the current project.** Each relink increments the link revision, a stale
    `set_project` is cancelled before issue, and every confirmation re-checks the target.
    Enforced by `_relink()` and the built-in `set_project` pre-issue check.
12. **Budgets hold, never drop, and never starve another product.** Enforced by `consume()` and
    `next()`.
13. **No process skips a kind's checks.** An unregistered kind is never offered, claimed or
    issued. Enforced by `_kind()`.
14. **Every read is bounded and every rotation reaches the end.** Enforced by `bounded()` and
    `faultsweep._rotation()`.
15. **One notification path.** Eligibility and budget are decided at reservation, a lapsed
    reservation is uncertain, and caller-raised decisions use the same path. Enforced by
    `reserve_notifications()`.
16. **Waiting is never a fault, and no sweep contradicts itself.** Paused, archived, busy and
    waiting recipients are never collected; no source emits an active and a clear for one fault in
    one sweep. Enforced by `faultsweep.sweep()`.
17. **Nothing malformed reaches the store.** Enforced by `read_observation()`.

### Identity

- `faults.fault_id(product, fault_class, signature, *, workspace=None)` returns the 32-hex fault
  id without a store, so a caller can decide an owner or a target before the first record.
  `FaultLedger.canonical_id(product, fault_class, signature, *, workspace=None)` returns the id
  the store actually keeps it under, which differs only through an alias (below).
- Compatibility with stores written before workspace joined identity: when an observation with
  workspace W finds no fault under its id but a fault exists under the workspace-less id whose
  stored `scope.workspace` is W, that fault is the one - the W id is registered as its alias and
  the observation converges on it. `record()` and `canonical_id()` both apply this.
- The workspace is the observation's `scope.workspace`: a non-blank string naming the tenant the
  fault belongs to (for Linear, the workspace), never a checkout path. It is not checked against
  any registry. `faults.UNASSIGNED` (`"unassigned"`) is the sentinel for an incident whose
  product workspace is not known yet. Without a workspace the id is exactly the merged one.
- Two real workspaces never share a fault. A `scope.projectKey` change inside one workspace is
  the same fault, moved. A fault recorded under `unassigned` moves to a real workspace through
  `move()` and keeps its id; the id that workspace would have produced becomes an alias of it, so
  later observations from that workspace converge on the same record. Every public call that takes
  a fault id resolves aliases first.
- Every alias registration - by `move()` or by the legacy lookup in `record()` - first resolves
  the new id through `canonical_id()` (aliases AND the legacy workspace lookup), in the same
  transaction, and refuses with `fault_scope_conflict` when that resolves to another recorded
  fault. Two records never come to stand for one failure in one workspace.
- Every path that changes a fault's scope - `record()`, `move()`, `adopt()` - does the same three
  things in one transaction: re-points unsent writes to the new target, leaves uncertain ones
  where they may have landed, and queues `set_project` when the fault owns an issue whose linked
  project differs from the new target.
- An observation moves its fault's scope only inside the fault's current workspace. An observation
  still carrying `unassigned` for a fault that has moved to a real workspace is recorded and
  leaves the scope where it is.
- Validated before anything is written, else `FaultRefused(fault_observation_malformed)`:
  product a plain identifier (letters, digits, `.`, `_`, `-`; no `:`, `@` or `|`, so the
  product always ends where a target key's first separator begins); faultClass a non-blank string
  without `|`; signature a non-empty object;
  occurrenceKey a non-blank string; detail a string or null; observedAt a string or null; scope
  an object whose values are JSON scalars, where `scope.workspace` and `scope.projectKey`, when
  present, are non-blank strings (a number or an empty string is refused, so no two inputs name
  one target); cleared a boolean.
- `faults.target_key(product, *, workspace=None, project=None)`: without a workspace it is the
  merged key, `product` or `product:project`. With one it is
  `ws|<product>|<workspace>|<project>`, each part percent-encoded, which no merged key can
  equal and no two inputs share. A store written before products were restricted may hold a
  fault whose product contains `:` and whose key therefore reads like a newer product and
  project. EVERY path that assigns a scope key - `record()` for a first record and for a scope
  move, `move()`, `adopt()` and `set_target()` - goes through one check that refuses, in the same
  transaction, a key already carried by a recorded fault of another product
  (`fault_scope_conflict`; for `record()` the observation is refused and nothing is written). Such
  a legacy fault stays readable but takes no new observation. A collision that already exists in
  an older store is caught where it could do harm: a write that needs a target is never offered,
  claimed or issued while its fault's scope key is also carried by a recorded fault of another
  product. It waits as `scope_key_contested`, shown by `queue_state()` and `attention()`, until
  `move()` separates the two. An observation files under
  `target_key(product, workspace=scope.workspace, project=scope.projectKey)`.

### Occurrences and episodes

- The same occurrence key inside one episode converges: a repeated sweep records nothing.
- The first ACTIVE observation after a recorded clear opens a new episode, whatever its key, and
  is a new occurrence. A cause that comes back is seen before or after resolve, and resolve
  refuses when it has come back after the reverification.
- `observation_unmeasured` is per turn (`{relationship, turn}`) and clears when that turn is
  later measured, so one relationship's turns cannot alternate one record open and closed.
- `occurrence_count` counts active occurrences only; clears are reported separately.

### Targets and project linkage

- A target belongs to the product that set it: the target record names its product, and a
  write is issued only against a target owned by its fault's product. A target row written
  before this contract names no product and is never used to issue anything; calling
  `set_target()` for it records the owner (so that call is not a no-op even when team and
  project are unchanged).
- `set_target(*, product, workspace=None, project=None, team, project_ref)` returns
  `{scopeKey, team, projectRef, changed, backfilled, relinked, relinkPending}`. Values unchanged
  on a target this product already owns write nothing. A change re-points only pending and failed writes whose target differs (an
  uncertain write stays where it may have landed) and queues `set_project` for issues this
  scope's faults own whose linked project differs, at most 100 per call; `relink(*, limit)`
  continues the rest.
- `targets(product=None, *, limit, after=None)` lists them.
- An issue create is neither offered nor claimable while its scope's target has no
  `project_ref`: no issue is created without a project, and the fault waits as awaiting target.
- Relinking supersedes: queuing `set_project` cancels any earlier `set_project` of the same
  fault that has not been issued, and each relink increments the link's stored revision, which
  is part of the `set_project` write's identity (`update:set_project:<project>:r<revision>`),
  so a later target always gets its own write, even one returning to an earlier project.
  `set_project` carries a built-in pre-issue check: `operation()` cancels a `set_project` whose
  project is no longer the fault's current target, so a stale write that an attested absence
  returned to pending is never issued. When any `set_project` confirms, the link is compared with the
  target as it is NOW, and a fresh `set_project` is queued if they differ: the issue ends on the
  current target whatever order the writes completed in.
- A create is confirmed against the project it was ISSUED with, and linked against the project
  its scope targets NOW. The readback must name a project. When that project differs from the
  current target, the create is still confirmed (the issue exists and the fault owns it), the
  link is recorded unlinked, and `set_project` for the current target is queued on the same
  issue.

### Owning an issue is not the same as a write having landed

- A fault OWNS an issue when a create for it confirmed or an adoption materialized
  (`external_ref`).
- A write has LANDED, or may have, when one of the fault's publications is issued, uncertain or
  confirmed.
- A clearing observation withdraws a fault when nothing has landed, even if it owns an issue, and
  cancels its pending, failed and claimed-not-issued publications. A later open revives the
  cancelled write under the same publication id; there is never a second row.
- ONE ISSUE SLOT per fault. Only the built-in `open_record` creates the fault's own issue; no
  registered kind can. The slot is taken when the fault owns an issue (a confirmed create or a
  materialized adoption) or has a non-cancelled `open_record`. There is at most one
  `open_record` row per fault, ever. A cancelled one is revived under its id only while the
  fault owns no issue; once it owns one, the open trigger queues the opening comment on that
  issue instead. Every path that could issue an `open_record` - `next()`, `claim()`,
  `operation()`, `retry()` and revival - checks the slot: `open_record` carries a built-in
  pre-issue check that cancels it when the fault already owns an issue, and `retry()` refuses one
  for such a fault. A fault that owns an issue never has a create claimed or issued. Any other
  create kind has at most one non-cancelled row per fault and kind.

### Adoption

Two entry points, and neither ever stores anything for a fault that has not been recorded:

- ~record(observation, *, adopt=None)~ with ~adopt={"externalRef": ..., "scope": {...}}~ adopts
  the existing issue in the SAME transaction as the fault's first record, before suppression can
  open it. A caller that decides the owner before recording uses this.
- ~adopt(fault_id, *, external_ref, scope)~ adopts for a fault already recorded (aliases
  resolved); an unknown id is refused with ~fault_unknown~. It returns ~{faultId, externalRef,
  state, cancelled, publication}~.

Either way:

- Not open yet: the adoption is stored against that fault and materializes when suppression opens
  the record - the first publication is a comment on ~externalRef~ instead of a create.
- Already open: it materializes at once; a pending, failed or claimed-not-issued create is
  cancelled, the fault owns ~externalRef~, and the opening comment is queued.
- ~scope~ carries the owner's ~projectKey~ and the identity's workspace; the fault moves to it.
- Refused with ~fault_adopt_conflict~ when the fault owns a different issue, holds a stored
  adoption naming a different issue, or has an issued or uncertain create (reconcile it first).
  Refused with ~fault_scope_conflict~ for another real workspace. Adopting the same issue again
  changes nothing.
- An alias only ever points at a recorded fault, and an adoption only ever belongs to one, so no
  alias registration - by ~move()~ or by the legacy lookup in ~record()~ - has an adoption to carry.

### Move

`move(fault_id, *, scope)` returns `{faultId, scopeKey, moved, repointed, alias}`. The same
workspace, or out of `unassigned` into a real one (refused with `fault_scope_conflict` when a
fault already exists under the id that workspace produces). Pending and failed writes follow the
new target; uncertain ones stay; an owned issue whose linked project differs from the new target
gets `set_project`; an unchanged scope writes nothing.

### Publication kinds

`register_kind(name, *, creates, requires_issue, target, evidence, confirm, validate=None,
pre_issue=None)` declares a kind. What a registered create makes is recorded on its own
publication (`external_ref`) and never becomes the fault's issue:

- `target`: `"team+project"`, `"team"` or `None` - what must be configured before it is
  offered.
- `evidence`: `"block"` (the write carries this publication's marker block; reconcile and
  complete read it from text) or `"fields"` (confirmed from fields read back from the owned
  issue).
- `confirm(expected, observed)` returns the problems with a readback; `validate(payload)`
  those with a payload.
- `pre_issue(context)` is called by `operation()` immediately before the row is issued. It
  answers `None` to proceed, `{"hold": reason, "seconds": n}` to return the row to pending,
  not offered again until n seconds pass (default 30) and with the reason recorded, or
  `{"cancel": reason}`. Either refunds only the current claimed-not-issued attempt. No path
  issues a write without it.
- Before any kind's own check, `operation()` compares the target a write that needs one was
  queued with (team, and project where required) against its fault's CURRENT target owned by its
  product. If they differ, the write is re-pointed to the current target and returned to pending,
  refunding only the current claimed-not-issued attempt; it is never issued against a target the
  scope has left. This is built in for every kind and is not a kind's optional check.
- Registration lives in the process: a kind is registered by importing the module that declares
  it, and the command line takes `--kind-module <module>` (repeatable) to do so. A publication
  whose kind is not registered in the acting process is never offered, claimed or issued; it is
  refused with `fault_kind_unregistered`, so no process can skip a kind's own checks.
- `creates=True` inherits the single-create rule: an issued write whose lease lapses or that
  fails becomes uncertain, and only a reconciliation reporting what was observed moves it.
- `queue(fault_id, *, kind, trigger, payload=None)` queues any registered kind on any recorded
  fault, including one suppression never opened: it is an explicit caller act, and the rule that
  a fix or resolve on a never-opened fault queues nothing governs only the ledger's own
  remediation writes. The one exception is the issue create, which only suppression opens
  (`queue(kind="open_record")` is refused with `fault_state_conflict`). A class whose only
  writes are another kind is recorded at a severity that never files and queued explicitly. CRW-206's
  project create is `creates=True, target="team", evidence="block"`; the
  issue-create project rule applies to issue creates only.

Built-in kinds:

- `open_record` (creates the fault's issue, team+project, block). The operation carries
  `trackerRef`, `projectRef` and the block. `complete(..., readback, external_ref,
  project_ref)` needs the project the saved issue reads back as.
- `append_comment` (requires the owned issue, block); confirmed only against that issue.
- `update_record` (requires the owned issue, fields). `request_update(fault_id, *, op,
  value)` queues one idempotent update per operation, value and cycle (`set_project` per link
  revision instead, above): `set_project` (project id), `reopen` (null), `add_relation` (`{type, issue}`), `add_label` (label name). A cause
  that comes back after resolution queues `reopen` beside its comment.
- Comments and updates need only the owned issue; targets decide creates.

Reads: `publication(publication_id)` returns one write with its kind, state, trigger, target,
payload, `external_ref` and newest attempts; `publications(fault_id, *, kind=None, state=None,
limit, after=None)` lists a fault's writes. A registered create leaves what it created in its
publication's `external_ref` and nowhere else.

Confirmation and reconciliation, for every kind:

- `complete(publication, *, claim_token=None, readback=None, external_ref=None,
  project_ref=None, observed=None)`: a block kind confirms from `readback`; a fields kind from
  `observed`, whose `issue` must be the owned issue.
- `reconcile(publication, *, observed_text=None, searched=False, observed=None)`: `present`
  when the block, or the fields, are found; `absent` only when the search is attested
  (`searched=True` for text; an `observed` naming the owned issue for fields). Under a live
  issued lease an absence answers `absent_in_flight` and changes nothing. From uncertain (or a
  lapsed lease) an attested absence returns the row to pending at the current target only when
  somebody ATTESTS that the issuing request has ended: `fail(..., ended=True)` by the claim
  holder when the connector answered with a definitive refusal, or `reconcile(...,
  prior_ended=True, reason=...)`. Both are recorded with who said so. The ledger never infers
  an end: a timeout, a lost response or a plain `fail()` after issue proves nothing, because the
  connector's create takes no idempotency key and a request still travelling can land after the
  search. Without the attestation the answer is `absent_unproven` and the row stays uncertain.
- `cancel(publication, *, reason)` cancels a pending, failed or claimed-not-issued write. For a
  claimed one it refunds that claim's attempt and budget; the budget and attempts of earlier,
  issued attempts are never refunded. An issued or uncertain write is refused with
  `fault_not_claimable`.

### Writers and attempts

`claim(publication, *, owner, takeover=False)`: the first claimant is the publication's writer.
Another owner needs `takeover=True`, which is recorded, or is refused with
`fault_writer_conflict`. Every claim appends an attempt carrying owner, takeover, claim and issue
times, outcome and error; `attempts(publication, *, limit)` returns them.

### Budgets

- One sliding window per product and kind. Defaults per hour: `open_record` 5,
  `append_comment` 20, `update_record` 20, `notification` 10, any other kind 20.
  `set_limit(product, kind, *, max_count, window)` and `limits(product)`.
- `budget(product, kind)` returns `{limit, window, used, remaining, source}`.
  `consume(product, kind, *, ref)` returns `{consumed, remaining, reason}`; one ref is consumed
  once; a spent budget answers `consumed: false, reason: budget_spent`. Any caller may use it.
- `claim()` consumes one unit; a spent budget refuses with `fault_budget_spent` and the write
  stays pending. Nothing a budget holds is dropped.
- `next(limit)` is fair across products: spent product and kind pairs are excluded inside the
  query and the rest are taken round-robin by product, so a capped product never hides another's
  work. `queue_state(limit)` returns `ready`, `held` with reasons, and `budgets`.

### Collection

- `delivery_refused`: each settings or permission refusal before sending, read from its
  append-only `delivery_withheld` journal record. Only reasons in `faultsweep.SETTINGS_REFUSALS`
  count - the settings, sandbox, approval and role-binding refusals the settings check raises -
  so a recipient withheld as paused, archived or unloaded never counts. Only the delivery's
  CURRENT streak counts. A streak ENDS, and the fault (degraded) clears, on exactly the same
  events: the delivery is sent (a settled attempt after the refusal) or settles, or its newest
  withholding carries another reason. A busy deferral in between ends neither, because it says
  nothing about the settings that were refused; three refusals for one reason are repetition
  whatever waited between them. A streak that ended is never counted again. `managed_start_failed`: the host
  answered a managed start without publishing a child (broken; clears when a later receipt for
  that request is accepted - the registry replaces a non-publishing receipt - which is the only
  transition the registry offers an armed request).
- No source emits an active and a clearing observation for one fault in one sweep: a reading
  batch is reduced to the last reading per relationship and turn BEFORE it is paged. A paused, archived,
  busy or waiting recipient is never a fault.
- Constructed with `fault_selection`, the daemon reads CRW-180 readings for attached managed
  turns through `omitted.observe`, a bounded number per tick; an observer error is a gap.
- Every source is read in rotations bounded by its upper key at rotation start, and every
  rotation reaches the end.
- `reading_faults(..., limit, after)` and `sweep(..., readings_after)` return
  `readingsNext`; more than 1000 readings are refused. `record_all` turns a refused
  observation into a gap and records the rest.

### Lifecycle stages

`record_stage(fault_id, *, stage, ref, detail="")` with stage `accepted`, `assigned`,
`merged` or `installed`. Accepted and assigned need an owned issue; merged and installed need a
fix in the cycle. Recording one runs, merges and installs nothing. `progress(fault_id)` returns
the newest of each. When `installed` follows the newest fix, the resolving reverification must
follow it too.

### Attention and notifications

- `attention()` counts unsent writes - ready, awaiting target, held, claimed (live or lapsed
  lease), failed, uncertain, awaiting record - and returns a warning; `status` shows it and a
  daemon tick carries it as a note.
- Notifications are `blocking` (a broken fault opened), `decision` (a write became uncertain
  or failed for good) and `resolved`. `notifications(*, limit)` lists pending ones with their
  eligibility: a paused, cancelled or archived relationship withholds one; a parent recipient
  that is uncontactable or unmeasured withholds one; a spent `notification` budget holds one.
- `raise_notification(fault_id, *, reason, ref)` lets a caller raise its own decision (for
  example an incident awaiting classification, or an owner or project hold). It is idempotent per
  fault and reason and enters the same eligibility, budget and reservation path as the ledger's
  own; there is one notification path.
- `reserve_notifications(*, owner, limit)` atomically takes eligible ones, consumes their budget
  and leases them, each with a stable `deliveryKey` the deliverer must pass to its transport as
  the idempotency key. `ack_notification(id, *, token, ref)` records delivery and is accepted for
  the current token whatever has happened to eligibility since; `fail_notification(id, *, token,
  error)` returns it to pending with the error, when the deliverer knows nothing was sent. A lease
  that lapses makes the notification uncertain, never pending: `reconcile_notification(id, *,
  delivered, ref)` settles it from what the deliverer can read back.
  `notifications(*, state=None, limit, after=None)` lists any state - pending with eligibility,
  reserved and uncertain with their id and `deliveryKey` - so a process that lost a reservation
  can find and settle it. Eligibility is decided at reservation; a withheld one cannot be
  reserved, and nothing is dropped.

### Policy

`policies(product)` and `set_policy(product, fault_class, severity, *, threshold=None,
window=None, reason)`. A degraded threshold and window can be adjusted; a broken fault files at
once and a notice never files, and changing either is refused with `fault_policy_fixed`. A change
is prospective: it decides the next occurrence recorded, and it is journaled with the value it
replaced. A suppression reason names the policy that decided it.

### Listing

`snapshot(*, product=None, fault_class=None, scope_key=None, state=None, limit, after=None)`.
`get()` and every snapshot row carry `linkState` (`linked`, `unlinked` or `none` when no
issue is owned) and `linkedProject`, and `attention()` counts unlinked issues, so an issue
without its project is visible wherever status is read.

### New refusal reasons and tables

Refusals: `fault_adopt_conflict`, `fault_scope_conflict`, `fault_writer_conflict`,
`fault_budget_spent`, `fault_policy_fixed`, `fault_kind_unregistered`.

Tables, all new because this store has no migration path: `fault_target_projects`,
`fault_publication_payloads`, `fault_links`, `fault_adoptions`, `fault_aliases`,
`fault_publication_attempts`, `fault_budget_uses`, `fault_limits`, `fault_notifications`,
`fault_policies`.

## What a fault is

A fault is the machinery failing to do its job. It is not a child failing at its task: a child
that reports `blocked` has worked correctly, and
[supervision](../src/codex_session_relay/supervision.py) owns deciding what the level above is
owed about it. A fault is the layer below that — the turn that settled without reporting at
all, the deliveries that will not leave the queue for one recipient, the Linear writes that
exhausted their attempts against one document, the anchor nobody has successfully polled. Each
is invisible in the product's own vocabulary, which is why they reached the user as silence.

The two paths stay apart on purpose. A supervisor obligation asks *has the level above been
told*; a fault asks *is this system working*. An unreported turn raises both, and they are not
the same record: one is discharged when the coordination document carries the outcome, the
other when the reason the turn could not report has been fixed and reverified.

## The observation

Everything enters through one shape, `fault-observation/1`, so a second product can feed the
same ledger without this package learning anything about it:

```json
{
  "schema": "fault-observation/1",
  "product": "crw",
  "faultClass": "report_omitted",
  "component": "reporting",
  "severity": "broken",
  "signature": {"relationship": "rel-0123456789abcdef", "turn": "turn-7"},
  "scope": {"projectKey": "CRW", "issueKey": "CRW-205"},
  "occurrenceKey": "observation:rel-0123456789abcdef:turn-7",
  "observedAt": "2026-09-22T04:10:00.000000+00:00",
  "detail": "an admitted turn settled without a report",
  "evidence": [{"kind": "row", "ref": "events", "observed": {"rows": 0}}],
  "cleared": false
}
```

`product`, `faultClass` and `signature` decide identity. `scope` decides where the record is filed
and is deliberately outside identity: a relationship can be re-read into a different project
without becoming a different fault. `severity` and the class decide suppression.
`occurrenceKey` decides whether this is a new occurrence or the same one read again.

`observedAt` is the observer's own clock and is kept as displayed evidence ONLY. Nothing this
module decides is decided by comparing it; [the lifecycle](#the-lifecycle) says why.

A second product registers its classes with `register_class` and feeds `FaultLedger.record` the
same shape. Nothing in the ledger, the suppression rules or the publication path knows what
`crw` means.

## Identity: what makes two observations the same fault

`fault_id = sha256(product | faultClass | canonical(signature))[:32]`, with the signature
rendered as JSON with sorted keys so two callers building the same dictionary in a different
order produce the same id.

What is deliberately NOT in it: the time, the occurrence, the attempt, the scope, and — the one
that is easy to get wrong — the individual event. Each changes while the fault stays the same,
and an identity carrying any of them files a second issue every time the system fails again.

So the signature is the FAILURE DOMAIN, not the incident. One recipient that cannot be reached
strands every delivery queued for it; keying on the delivery would file one issue per stranded
event for a single broken recipient, which is exactly the spray this ledger exists to prevent.
The domain is the recipient. The individual deliveries are its occurrences.

| Class | Signature (identity) | Occurrence key | Cleared by | Severity |
|---|---|---|---|---|
| `delivery_stalled` | recipient and the attempt's classified state | the attempt's `request_id` | the sweep no longer deriving it | degraded, `broken` at the attempt cap |
| `record_sync_failed` | target and target ref | `(sync_id, attempts)` | the sweep no longer deriving it | broken |
| `observation_stalled` | relationship and generation | `(relationship, generation, turn, last_attempt_at)` | the sweep no longer deriving it | `broken` when never polled, degraded otherwise |
| `report_omitted` | relationship and turn | `observation:<relationship>:<turn>` | a reading that says `reported` | broken |
| `observation_unmeasured` | relationship | `unmeasured:<relationship>:<turn>` | a later reading that establishes something | notice |

A retrying delivery is read from its SETTLED attempt rows, not from the delivery. An
attempt row is written in_flight with a provisional `held_uncertain` state before the
transport call returns, so every reader of attempt state waits for `internal_state` to
be `settled`; otherwise three healthy sends caught mid-flight would reach the degraded
threshold. A reconciled uncertain outcome is settled, and stays eligible. The hold reason
is only set at a cap, so a query that required one saw nothing until a delivery had
already given up, and the degraded tier - three observations inside a window - could
never be reached. An attempt's request id advances once per actual failure rather than
once per sweep, which is the occurrence identity this needs. Identity takes the
attempt's own classified state and never the delivery's hold reason: that reason is
set when a delivery gives up and it is MUTABLE, so deriving identity from it meant one
continuous failure owned two faults the moment it hit its cap. Severity still reads
it, because severity is not identity and the same fault escalates instead of forking.

The hold reason alone was not enough for a delivery. `attempt_cap` covers every pre-send failure
there is, so a settings rejection and a transport error would have merged into one record that
named neither; the last attempt's classified state is in the signature to keep them apart.

An anchor whose attempts have never succeeded is `broken` rather than degraded, and that
is not severity inflation. Its occurrence key cannot change while nothing succeeds and no new
attempt is recorded, so it produces exactly one occurrence, and at degraded, which needs
three, permanent scheduler starvation would be the one failure that could never reach the
threshold.

An ATTEMPT has to exist first. A generation bound a moment ago has no poll row yet and is not
stalled, because it has not been due, and raising on that absence filed a broken fault for
every healthy new assignment. The cost is stated rather than hidden: a scheduler that never
attempts at all leaves nothing to see here.

Every class declares what clears it, and that column is not documentation: `CLASS_POLICY`
carries it and `register_class` refuses a class without one. `refusal_recurring` is the absence
worth naming — the refusals table is append-only and carries no later success, so nothing in
this store could ever clear one, and it is therefore not registered.

## Occurrences and evidence

Each observation records an occurrence, keyed by
`sha256(fault_id | episode | direction | occurrenceKey)[:32]` and inserted once, unique on
(fault, episode, key) in the table as well as in the id. The key matters more than it looks: the sweep runs on every tick and reads the
same stuck row each time, so without an occurrence identity one stuck delivery would count
thousands of occurrences within the hour and escalate itself past every threshold. The adapter
names the occurrence after the underlying fact — the attempt's request id, the publication's
attempt number, the poll's attempt time — so re-reading an unchanged state records nothing and
a genuinely new failure records exactly one.

An occurrence is identified WITHIN its episode, which is part of its stored key and not only of its id. An episode ends when the fault is
cleared or resolved, and the next one begins there. That is what separates the sweep
reading the same stuck row again, which must converge, from the thing that was fixed
coming back, which must reopen: both arrive under the key that names the underlying
fact, and only the episode tells them apart. An episode opens when a fault actually CLOSES, so a clearing
reading repeated on a published fault says nothing new rather than opening one each
time. Whether an occurrence has been seen is asked of the timeline, which is never
pruned, so removing evidence an operator has finished reading cannot make a familiar
occurrence look new.

Evidence is a SNAPSHOT, not a pointer. The rows a fault is read from are mutable: a poll row's
error is overwritten on its next attempt, and a failed synchronisation row's state and error
change when it eventually succeeds. An occurrence carries the values as they were observed,
with a digest over them, alongside the identity of the row they came from. A reader who follows
the pointer later and finds something else can see that, instead of concluding the fault was
never real. Evidence is bounded at `MAX_EVIDENCE` items and `MAX_EVIDENCE_BYTES`, and a
truncated list says so in the record.

## Suppression: what never reaches Linear

Recording is cheap and publishing is not. Every observation is recorded; only some are
published, and the rule is a table rather than a judgment:

| Severity | Threshold | Meaning |
|---|---|---|
| `broken` | 1 | the function is not being performed; one observation is enough |
| `degraded` | 3 within the window | it is working badly, and once may be weather |
| `notice` | never | recorded for the operator, never filed |

`CLASS_POLICY` is the only place a class overrides that. The window bounds which occurrences
count, and it is counted from `fault_timeline` rather than from the evidence rows, because
`fault-prune` removes evidence an operator has finished reading and that must not change what
the next observation decides.

Nothing in that table advances by itself. **A fault is never cleared because it stopped being
observed**, which is the rule the rest of this package already follows: silence is the absence
of evidence, not evidence of recovery. Clearing is an explicit observation carrying
`cleared: true`, and the sweep produces it by RE-DERIVING the fault: if it reads the delivery
rows and no longer produces a signature it produced before, it positively established the
absence and says so. A class the sweep does not derive — anything built from a reading handed
in from outside — is never cleared that way, because a reading nobody supplied establishes
nothing. That is the difference between the thing being gone and nobody having looked, and it
is why an `unmeasured` reading never clears an omission.

A fault that never earned a Linear record is withdrawn when it clears — including one a locally
recorded fix moved to `fix_pending`, which is still a fault no record carries. A fault that owns a
record is not closed by clearing, because the closed loop below is what closes it.

Suppression is also the only thing that opens a record. Recording a fix or a resolution against
a fault the threshold never published queues nothing: a remediation must not be the back door
through which a notice reaches Linear.

Occurrences stay append-only. `fault-prune` is an explicit operator act that records in the
journal how many rows it removed, because a store that silently discards its own evidence on a
schedule is worse than a large one.

## The lifecycle

```
observed ──threshold──→ open ──fix──→ fix_pending ──reverified──→ resolved
    │                    ↑                  │                        │
  cleared                └───occurrence─────┴────────occurrence──────┘
    ↓
withdrawn
```

**Every comparison that decides a transition is made on this store's own insertion sequence.**
`fault_timeline` exists for that: occurrences live in one table and remediations in another, and
SQLite rowids are per-table, so "was this reverification recorded after that fix" has no answer
without a shared sequence. The reason it is not a timestamp is concrete: one occurrence misdated
to 2099 would otherwise refuse every honest reverification until then, and a recurrence carrying
a stale timestamp would sort before the check that missed it and let the fault be resolved
anyway. Order comes from the sequence; elapsed windows come from the injected clock, which is
the clock the delivery backoff already schedules from.

`record_fix` attaches the change that is supposed to have fixed it — a pull request, a commit, a
configuration edit — and moves the fault to `fix_pending`. That is all it does.

A reverification is structured: the method (`suite`, `command` or `observation`), the exact
command or reading, and an outcome from a closed vocabulary — `passed`, `absent` or `failed`. The
vocabulary is closed because an open string let `outcome="still failing"` satisfy the resolution
gate, which is the one sentence a fault ledger must never accept as proof.

Its identity includes the fix it follows. Running the same command again after a second fix is a
second verification of a different change; without that, the rerun took the first run's identity,
was dropped as a duplicate, and the fault could never be resolved again.

`resolve` takes the newest fix in the sequence, requires a reverification recorded after it,
requires that reverification to have found the fault gone, and requires that no occurrence has
been recorded since. A second fix therefore invalidates the first fix's verification by
construction — it is newer — so `fix → reverify → fix → resolve` is refused until the second fix
has been verified in its own right.

A new occurrence after `resolved` reopens the SAME record, increments its cycle and adds a
comment to the SAME issue. It never opens a second one. A fault fixed twice reads as one record
with two remediation cycles, which is the history worth having: it shows the first fix did not
hold.

## Publication: one issue, then comments

A published fault owns one Linear issue for its whole life. The first publication opens it;
everything afterwards is a comment on it. Four separate mechanisms keep that true, because each
one fails differently:

1. **The ledger.** Repeated observations converge on one `fault_id`, so there is only ever one
   record to publish.
2. **The trigger.** A publication's id is derived from the fault and from WHY it is being
   published — `open`, `escalate`, `recur`, `fix`, `resolve`, `reopen:<cycle>` — so the same reason
   never queues twice, however many times the ledger is swept.
3. **One create, ever.** At most one `open_record` row exists per fault. Choosing the kind from
   whether the fault owns an issue was not enough on its own: between queuing the create and
   confirming it the ledger holds no reference, so a fix or a resolve arriving in that window
   queued a SECOND create under its own trigger. A trigger arriving in that window is queued as
   a comment instead, and held ineligible until the issue it comments on exists.
4. **The uncertainty rule**, which is a refusal rather than a guarantee.

### What a create can and cannot promise

The connector's issue create takes no idempotency key, so a create can succeed and lose its
response. No local identifier fixes that, and this module does not claim exactly-once creation.
What it claims is narrower and actually enforceable: **a second create is never issued
automatically.**

Handing out a create operation marks the row `issued`. From there:

- reporting failure moves it to `uncertain`, never back to `pending`;
- a `pending` row has no write outstanding, so it cannot be completed at all: a readback
  captured before a reconciliation established the block ABSENT must not confirm it afterwards;
- a backoff applies to a direct claim as well as to the queue, or it is not a backoff;
- a publication carries the CYCLE it was queued in, so a fault that reopens while a comment is
  waiting does not make that comment describe a cycle it was never about;
- a lease expiring on it moves it to `uncertain`, while a lease expiring on a merely `claimed`
  row — one that never reached the connector — is safely released;
- nothing but `reconcile` leaves `uncertain`. The caller reports what it observed: the marker
  found confirms the row against the issue that already exists; the marker absent moves it back
  to `pending` for one further create, and only when the caller attests that it looked. An
  unattested negative read changes nothing, because a negative read is not proof of absence.

This is the same reasoning the coordination document uses for its conditional replacement, at
the one place where that technique is unavailable: you cannot conditionally replace a document
that does not exist yet.

`open_record` also needs somewhere to file, which `fault_targets` supplies per scope. A scope
with no configured target is not an error and does not lose the fault: it stays recorded, its
publication waits, and `fault-target` backfills what was waiting. Filing into a guessed project
would be worse than waiting.

## What runs by itself, and what does not

The daemon's tick sweeps the store and records what it finds, so a fault is detected and queued
without anybody asking. The pass is bounded like every other pass — each source reads at most
`SWEEP_LIMIT` rows — and it ROTATES: `fault_cursors` remembers where each source stopped and the
next sweep resumes there, wrapping to the start when a page comes back short. A fixed prefix
re-read on every tick would have starved everything behind it forever, which is the same shape
the delivery window keeps a per-parent cursor to avoid. The scan over open faults rotates too, for the same reason.

Recovery is asked of each fault DIRECTLY - an existence query for its own signature -
rather than by differencing against a page. A page is a bounded prefix, so once a source
holds more rows than one page no page is ever the whole source, and a rule that required
one would have stopped clearing anything exactly when a store got busy. An existence
query is exact however large the source is, and a class this cannot ask about is still
never cleared by absence.

The pass counts only what was NEWLY recorded, so a steady-state failure read again on every tick
does not hold the loop at its fastest cadence forever. A daemon given no ledger ticks exactly as
it did before.

It cannot own the other half. The relay holds no Linear credential by design, so the write is
performed by the process that does — the coordination parent, or an operator running the
commands below. Until that consumer runs, a published fault is a queued publication, and this
module says exactly that rather than implying an issue exists.

## What this does not claim

It does not claim anybody read the issue. It records that a write was queued, that a caller
claimed it, and that a readback found the expected block in the expected place.

It does not claim to see every fault. It sees what this store's rows and the readings handed to
it can show. A breakage that leaves no row and produces no observation is invisible here, and a
fault ledger implying otherwise would be worse than none. Where a source overwrites its own
history — a poll row keeps only its latest attempt — the occurrence count is a count of what was
OBSERVED, not of what happened, and the record says so.

It does not diagnose. A fault record names what was observed and where the evidence is. Why it
happened is the work the fix cycle exists for.

## Tables

| Table | What it holds |
|---|---|
| `fault_ledger` | one row per distinct fault: identity, scope, state, cycle, counts, the issue it owns |
| `fault_occurrences` | append-only, one row per distinct occurrence, with its evidence snapshot |
| `fault_timeline` | the single sequence every lifecycle decision is ordered by, never pruned |
| `fault_remediations` | append-only fixes and structured reverifications, per cycle |
| `fault_publications` | the outbox: what must be written to Linear, and how far it got |
| `fault_targets` | where a scope's fault issues are filed |
| `fault_cursors` | where each source stopped and how many full pages it has taken, so the sweep rotates and still wraps |

## Python API

This is the surface CRW-206 and any other product builds on. Everything below takes a
`Store` and an injected clock and performs no network call.

```
faults.register_class(name, *, component, clears, threshold=None, window=None)
faults.observation(*, product, fault_class, severity, signature, occurrence_key,
                   scope=None, observed_at=None, detail="", evidence=(), cleared=False)
faults.fault_id(product, fault_class, signature)

ledger = faults.FaultLedger(store, clock)
ledger.record(observation)                 -> faultId, recorded, state, publication
ledger.set_target(scope_key, tracker_ref)
ledger.get(fault_id)
ledger.snapshot(scope_key=None, state=None, limit=20, after=None) -> faults, next
ledger.occurrences(fault_id, limit=3)
ledger.remediations(fault_id, limit=20)
ledger.record_fix(fault_id, ref=..., detail="")
ledger.record_reverification(fault_id, method=..., ref=..., outcome=..., detail="")
ledger.resolve(fault_id)
ledger.prune(fault_id, keep=...)

ledger.next(limit=4)                       # publications a credential holder may act on
ledger.claim(publication_id, owner=...)    -> claimToken
ledger.operation(publication_id, claim_token=...)
ledger.reconcile(publication_id, observed_text, searched=False)
ledger.complete(publication_id, readback=..., claim_token=None, external_ref=None)
ledger.fail(publication_id, claim_token=..., error=...)
ledger.expire_leases()
ledger.retry(publication_id)

faultsweep.sweep(store, product="crw", scope=None, readings=(), limit=32)
faultsweep.record_all(ledger, batch, store=store)
```

Every `limit` is a positive integer and is refused otherwise, because SQLite reads
`LIMIT -1` as no limit. A listing is continued by passing the `next` it returned as
`after`; the cursor is a rowid, so faults recorded between pages land after it. A second
product registers its classes once at import and feeds `record`; the ledger, the
suppression rules and the publication path need nothing else from it.

## Commands

```
fault-target      --scope <key> --tracker-ref <ref>
fault-observe     --observation <json|@path>
fault-sweep       [--product <name>] [--project <key>] [--readings <json|@path>]
fault-show        [--fault <id>] [--scope <key>] [--fault-state <state>] [--limit <n>]
                  [--after <next>]
fault-fix         --fault <id> --ref <ref> [--detail <text>]
fault-reverify    --fault <id> --method suite|command|observation --ref <text>
                  --outcome passed|absent|failed [--detail <text>]
fault-resolve     --fault <id>
fault-next        [--limit <n>]
fault-claim       --publication <id> --owner <name>
fault-operation   --publication <id> --claim-token <token>
fault-reconcile   --publication <id> --observed <text|@path> [--searched]
fault-complete    --publication <id> [--claim-token <token>] --readback <text|@path>
                  [--external-ref <ref>]
fault-fail        --publication <id> --claim-token <token> --error <text>
fault-retry       --publication <id>
fault-prune       --fault <id> --keep <n>
```

`--fault-state` rather than `--state`: that name belongs to the global option naming the store
directory, and a subcommand option of the same name overwrites it in the namespace, so every
such command would read an unconfigured default store and answer that the fault did not exist.

Status: implemented in this package, with the suite as the proof. Detection and queuing run in
the daemon tick; the Linear write is performed by a credential holder outside this package.
Nothing here is evidence about an installed runtime, a live service, or anything that has
actually been written to Linear.
