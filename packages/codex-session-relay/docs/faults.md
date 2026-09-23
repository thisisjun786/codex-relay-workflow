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
   ever. Enforced by `_issue_slot()`, which `record()`, `_enqueue()` (revival included),
   `next()`, `claim()`, `operation()`, `retry()` and `adopt()` consult; `queue()` refuses the
   create kind outright.
2. **An issued create is never repeated on a guess.** Handing out an operation marks it issued; a
   lapse or failure after that makes it uncertain; only `reconcile()` with an attested end of the
   request frees it. Enforced by `operation()` and `reconcile()`.
3. **Transitions belong to the claim.** Operation, completion and failure of a claimed or issued
   write need the current claim token; the first claimant is the writer and another needs a
   recorded takeover. Enforced by `_claimed()` for `operation()` and by the same current-token
   check inside `complete()` and `fail()`. An uncertain write has no live claim and so no
   token: it leaves uncertain only on a readback that finds its block - `complete()` without a
   token, or `reconcile()` - or on an attested end of its request (invariant 2).
4. **Only unissued writes are cancelled, and cancelling spends nothing earlier.** Pending, failed
   and claimed-not-issued rows only; a claimed one refunds its own attempt and budget. Enforced by
   `_cancel()` and its set-based form `_cancel_where()`, the only paths that cancel: `cancel()` is
   the public form, adoption, a pre-issue cancel and a create whose fault already owns an issue
   call `_cancel()`, and withdrawal and `set_project` supersession call `_cancel_where()`, which
   gives each claimed row back its own current claim's unit and attempt, and nothing earlier.
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
    relinks an owned issue. Which writes are target-bound is read from the requirement recorded
    when each was queued (`fault_publication_payloads.target_mode`), so a process that never
    loaded an extension kind still re-points that kind's writes. Enforced by `_rescope()` for
    `record()`, `move()` and `adopt()`, through `_repoint_where()`.
11. **The issue ends on the current project.** Each relink increments the link revision, a stale
    `set_project` is cancelled before issue, and every confirmation re-checks the target. With no
    project its product owns, an owned issue is unlinked - awaiting a target - and never
    reported linked; nor is it reported linked while a `set_project` to another project is issued
    or uncertain, because that write may still land (`_moving_elsewhere()`, which reads at most
    one row), and no second `set_project` is queued while any is issued or uncertain - to another
    project or to the target itself - whatever project the
    issue reads back in: the issue waits unlinked, and the write's readback or, when it ends
    with none, `relink()` takes it up again. A `set_project` is recognised by its payload's `op`, never by its trigger, and
    `update_record` is queued only through `request_update()`. Link state is decided when read, against the product's current owned target
    (`_link_state()` for `get()` and `snapshot()`, the same rule in `attention()`), so an issue a
    bounded relink batch has not reached yet is never reported linked. A target returning to the
    project the issue already sits in cancels every unsent `set_project` at once. Enforced by `_relink()` and `_unlink()` through `_link_to_target()`,
    and the built-in `set_project` pre-issue check.
12. **Budgets hold, never drop, and never starve another product.** A budget is decided per
    candidate inside the selection query of `next()` and of `reserve_notifications()`, and
    candidates are taken round-robin by product. Enforced by `consume()` and `_open_budget()`.
13. **No process skips a kind's checks.** An unregistered kind is never offered, claimed or
    issued. Enforced by `_kind()`.
14. **Every read is bounded and every rotation reaches the end.** An existence question asks the
    live supersession rule about a bounded number of deliveries per call, keeps its verdicts,
    and answers undetermined - clearing nothing - until a later call has judged them all.
    Re-pointing unsent writes and releasing lapsed leases take at most 100 rows per call and
    report what remains - a repeated `set_target()` with an unchanged target too, writing
    nothing; cancelling a fault's unissued writes is set-based; `limits()` and `policies()` are
    read a page at a time with a `next` cursor, and `queue_state()` says when its budgets are
    truncated. Enforced by
    `bounded()`, `faultsweep._rotation()`, `faultsweep._first_current()`, `_repoint_where()`,
    `expire_leases()` and `_cancel_where()`.
15. **One notification path.** Eligibility and budget are decided at reservation, a lapsed
    reservation is uncertain, caller-raised decisions use the same path, and candidates are taken
    least recently examined first, by an examination sequence that never ties (a time would, for
    calls at one instant), so one withheld or held never hides the ones behind it, whatever the
    caller's cadence.
    Enforced by `reserve_notifications()`.
16. **Waiting is never a fault, an overtaken obligation is not current, and no sweep contradicts
    itself.** Paused, archived, busy and waiting recipients are never collected - an attempt
    answered `deferred_busy` and a delivery held at `busy_cap` are a recipient mid-turn, and a
    fault one raised reads as absent and clears; a superseded
    delivery and an anchor the scheduler no longer reads (a paused assignment, a generation it
    moved past) are not collected and clear what they raised; no source emits an active and a
    clear for one fault in one sweep; and a sweep judges recovery only for faults its rows can
    speak for - the ones its own observations resolve to, its own workspace's, and those with no
    workspace - never another workspace's. Enforced by `faultsweep.sweep()`, whose delivery sources
    and `still_present()` ask `faultsweep._current()` (the send path's own
    `delivery.supersession_reason()`), whose anchor source reads the scheduler's own predicate,
    and whose `recovered()` asks `faultsweep._judged_here()`.
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
  `{scopeKey, team, projectRef, changed, backfilled, backfillPending, relinked, relinkPending}`.
  Values unchanged on a target this product already owns write nothing, and still report the
  `backfillPending` and `relinkPending` an earlier change left, so a caller retrying a call whose
  answer was lost does not read the remaining work as done. A change re-points only
  pending and failed writes whose target differs (an uncertain write stays where it may have
  landed), at most 100 per call with `backfillPending` counting the rest, and queues
  `set_project` for issues this scope's faults own whose linked project differs, at most 100 per
  call. `relink(*, limit)` continues both and returns
  `{relinked, relinkPending, backfilled, backfillPending}`. A write not yet re-pointed is never
  issued against the old target: `operation()` re-checks the current owned target first.
- `targets(product=None, *, limit, after=None)` lists them.
- An issue create is neither offered nor claimable while its scope's target has no
  `project_ref`: no issue is created without a project, and the fault waits as awaiting target.
- Relinking supersedes: queuing `set_project` cancels any earlier `set_project` of the same
  fault that has not been issued, and each relink increments the link's stored revision, which
  is part of the `set_project` write's identity (`update:set_project:<project>:r<revision>`),
  so a later target always gets its own write, even one returning to an earlier project.
  `set_project` carries a built-in pre-issue check: `operation()` cancels a `set_project` whose
  project is no longer the fault's current target - including when the scope no longer targets
  any project its product owns - so a stale write, whether still claimed or returned to pending by
  an attested absence, is never issued. Setting a target again queues a fresh one. When any `set_project` confirms, the link is compared with the
  target as it is NOW, and a fresh `set_project` is queued if they differ: the issue ends on the
  current target whatever order the writes completed in.
- A create is confirmed against the project it was ISSUED with, and linked against the project
  its scope targets NOW. The readback must name a project. When that project differs from the
  current target, the create is still confirmed (the issue exists and the fault owns it), the
  link is recorded unlinked, and `set_project` for the current target is queued on the same
  issue.
- A scope with no project its product owns - removed, never set, or owned by another product -
  leaves an owned issue unlinked, awaiting a target: `set_target(..., project_ref=None)` and
  `relink()` unlink such issues (and cancel their unsent `set_project`), a move or adoption into
  such a scope unlinks, and a readback confirmed while there is no current project records
  unlinked. `attention()` counts them. Setting a project again queues `set_project` on the same
  issue.
- A readback proves where the issue is only while no `set_project` to another project is issued
  or uncertain. While one is, the issue stays unlinked and no second write is queued: that
  write's own readback (`complete()`, or `reconcile()` then `complete()`) decides, and a repair is
  queued on the same issue from there. A stale relink cancelled before issue re-evaluates the
  link, and `relink()` settles one whose readback already matches - once no write it waits on
  is outstanding. While one is, `relink()` passes it over, so issues waiting on writes that may
  still land never take the batch from the issues behind them; the outstanding write's own
  readback re-evaluates the link.
- `linkState` from `get()` and `snapshot()`, and `attention()`'s `unlinked` count, are decided when
  read: an issue is linked only while it reads back in the project its product's current owned
  target names. A stored link a relink batch has not reached yet, or one whose scope no longer
  targets any project, is reported unlinked.

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

- `record(observation, *, adopt=None)` with `adopt={"externalRef": ..., "scope": {...}}` adopts
  the existing issue in the SAME transaction as the fault's first record, before suppression can
  open it. A caller that decides the owner before recording uses this.
- `adopt(fault_id, *, external_ref, scope)` adopts for a fault already recorded (aliases
  resolved); an unknown id is refused with `fault_unknown`. It returns `{faultId, externalRef,
  state, cancelled, publication}`.

Either way:

- Not open yet: the adoption is stored against that fault and materializes when suppression opens
  the record - the first publication is a comment on `externalRef` instead of a create.
- Already open: it materializes at once; a pending, failed or claimed-not-issued create is
  cancelled, the fault owns `externalRef`, and the opening comment is queued.
- `scope` carries the owner's `projectKey` and the identity's workspace; the fault moves to it.
- Refused with `fault_adopt_conflict` when the fault owns a different issue, holds a stored
  adoption naming a different issue, or has an issued or uncertain create (reconcile it first).
  Refused with `fault_scope_conflict` for another real workspace. Adopting the same issue again
  changes nothing.
- An alias only ever points at a recorded fault, and an adoption only ever belongs to one, so no
  alias registration - by `move()` or by the legacy lookup in `record()` - has an adoption to carry.

### Move

`move(fault_id, *, scope)` returns `{faultId, scopeKey, moved, repointed, repointPending, alias}`:
at most 100 writes are re-pointed per call and `relink()` continues the rest; a repeated move to
the scope the fault is already in writes nothing and still reports `repointPending`. The same
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
  complete read it from text; a kind that creates must use it, since the single-create rule
  finds a create by its block and its publication records the object it made - `register_kind()`
  refuses `creates=True` with `"fields"`) or `"fields"` (confirmed from fields read back from the owned
  issue).
- `confirm(expected, observed)` returns the problems with a readback; `validate(payload)`
  those with a payload. `expected` is the publication row exactly as `publication()` returns it
  (id, fault id, kind, trigger, payload, target), so a create's confirmation can compare what was
  read back with what was queued; `validate` receives that row's payload.
- `pre_issue(context)` receives `context = {"publication": the row as publication() returns it
  (id, fault_id, kind, trigger, payload, target), "fault": the fault as get() returns it, "db": the
  connection of the transaction operation() is running in, "now": the ledger clock's time}`. A
  kind's check may read any table of this store through `context["db"]` and must not write. That
  is enforced, not a convention: the check runs inside a savepoint that is always rolled back, and
  if the connection's change count moved during it the operation is refused, nothing is issued and
  the row stays claimed. (An SQLite authorizer alone would not do: it is consulted only when a
  statement is prepared, and a cached statement is not prepared again.)
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
  (`queue(kind="open_record")` is refused with `fault_state_conflict`), and so is the built-in
  `update_record`, whose one entry point is `request_update()`: it keys each update on the issue
  and puts every `set_project` through the link's revision. A class whose only
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
  revision instead, above). `set_project` on request runs the same converging relink a target
  change does: it accepts only the project the scope targets (another is refused with
  `fault_state_conflict`), and queues nothing - answering `queued: false` with the link state -
  when the issue already reads back there or a write to another project may still land: `set_project` (project id), `reopen` (null), `add_relation` (`{type, issue}`), `add_label` (label name). A cause
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
  `set_limit(product, kind, *, max_count, window)` and `limits(product, *, limit=20, after=None)`,
  one page in kind order of the kinds this process registered and every kind a limit was stored
  for, loaded here or not, as `{limits, next}`; pass `next` as `after` for the rest.
- `budget(product, kind)` returns `{limit, window, used, remaining, source}`.
  `consume(product, kind, *, ref)` returns `{consumed, remaining, reason}`; one ref is consumed
  once; a spent budget answers `consumed: false, reason: budget_spent`. Any caller may use it.
- `claim()` consumes one unit, charged against the claim's own attempt row rather than its attempt
  number (which `retry()` starts again), so a retried write is charged again. A spent budget
  refuses with `fault_budget_spent` and the write stays pending. Nothing a budget holds is dropped.
- A claim that ends without issuing - cancelled, held or cancelled by a pre-issue check,
  re-pointed, or lapsed - gets its own unit and attempt back; nothing earlier is refunded. Every
  transition of a claim updates that claim's attempt row and no other, so earlier attempts keep
  their history.
- `next(limit)` is fair across products: each candidate's product and kind budget is decided
  inside the selection query, and the rest are taken round-robin by product, so a capped product
  never hides another's work however many products are capped. `queue_state(limit)` returns `ready`, `held` with reasons, and `budgets`.

### Collection

- `delivery_refused`: each settings or permission refusal before sending, read from its
  append-only `delivery_withheld` journal record. Only reasons in `faultsweep.SETTINGS_REFUSALS`
  count - the settings, sandbox, approval and role-binding refusals the settings check raises -
  so a recipient withheld as paused, archived or unloaded never counts. Only the delivery's
  CURRENT streak counts. A streak ENDS, and the fault (degraded) clears, on exactly the same
  events: the delivery is sent (a settled attempt, journaled `delivery_attempted`, after the
  refusal) or settles, a person pauses or archives the assignment (`delivery_withheld_inactive`),
  or its newest withholding carries another reason. A busy deferral in between ends neither,
  because it says nothing about the settings that were refused; three refusals for one reason are
  repetition whatever waited between them. A streak that ended is never counted again. Signature
  `{relationship, errorCode}`, occurrence key `refused:<journal seq>`. `managed_start_failed`: the host
  answered a managed start and the relay attached no child (broken; signature `{issueKey,
  receiptStatus}`, so a rejection and a partial start are different faults). The answer is read
  where it actually lives. `managed.ManagedStart` records a receipt only for an ACCEPTED creation,
  so for an armed request with no receipt the answer is the newest creation-stage row it
  journaled (`managed_start_observed`, subject = the request id; the row `managed-show` reads):
  a reason `creation_failed`, `creation_unknown`, `creation_identity_unobserved` or
  `creation_settings_unverified` is an answer after a create was attempted, and its suffix is the
  status. The last two come after the host ACCEPTED the creation - its receipt named an unusable
  identity, or settings that did not match the request - so the incident says a child may exist
  that the relay could not attach, never that none was published. The incident states only what
  the answer establishes: a child the journaled answer names (the `retainedChildTaskId` a partial
  creation left) is named and said not to be attached; `unknown` - the managed start's own
  classification of a receipt it could not read as accepted or failed - says whether a child was
  created is not established; only a journaled answer whose receipt named no thread says so. A
  recorded receipt that is not accepted still decides where one exists, and is stated as the
  registry's stored status whatever it names, never as the host's answer or a journaled one: the
  registry keeps a child id only for a receipt it accepts and stores an accepted one missing its
  thread or standby id as `partial` with none, so from that row neither what the host answered
  nor whether it created a child is established. The record a fault publishes says it clears on
  exactly what `still_present` reads: an accepted receipt, attaching, or a newer creation-stage
  answer. The newest creation-stage row is found through the partial index
  `journal_managed_creation`, which holds only those rows, so it is one probe however many rows
  a request's retries journaled. Any other
  reason at that stage - a worker that cannot take the pair - means the host was not asked on
  that attempt, and is not a fault; nor is a request with no creation-stage row at all, which is
  a start still waiting for the host. Elapsed time decides nothing. The fault clears when the
  request records an accepted receipt or attaches, or its newest creation-stage row says
  something else (a later refusal at creation, or another answer, which is a different fault).
  Later rows of other stages (preflight, intent) never decide: they say nothing about what the
  host answered. Stated limits: a caller that stops after arming without journaling any answer
  (a crash, or an exception that leaves `run()` without a result) is not seen until the same
  request is retried, which journals the answer the bridge kept; a request that stalls on
  `intent_conflict` never reached creation and is not collected.
- No source emits an active and a clearing observation for one fault in one sweep: a reading
  batch is reduced to the last reading per relationship and turn BEFORE it is paged, except that
  an `unmeasured` reading never replaces an established one of the same turn - a later read that
  established nothing does not un-establish an answer. A paused, archived, busy or waiting
  recipient is never a fault, and `still_present()` asks exactly what each source collects: a
  held-delivery fault is present only while a delivery to its recipient is held for a reason
  other than `busy_cap`, so a waiting delivery never keeps it open.
- A delivery whose obligation was superseded is not current, for every delivery kind: one in the
  `superseded` state, one annotated in `delivery_supersession` (a delivery held at its attempt cap
  cannot be rewritten, so it is annotated), one whose relationship was replaced, and one the send
  path's own rule (`delivery.supersession_reason()`: a later generation, an answered revision
  request, a newer final revision, a regranted merge turn) says is overtaken. `delivery_stalled`
  and `delivery_refused` neither collect such a delivery nor let it keep a fault present, so a fault
  it raised clears.
- `observation_stalled` reads only the anchors the scheduler reads - the current generation of an
  active relationship nobody replaced, the predicate `observation_health` uses - so a paused
  assignment or a generation it moved past is never a stalled one.
- Constructed with `fault_selection` (the bounded run passes the store selection), the daemon
  reads CRW-180 readings for attached managed turns through `omitted.observe`,
  `MANAGED_READINGS_PER_SWEEP` (8) per tick in a rotation over the settlements: each settled
  turn once, of a relationship still on its managed start's generation, never the standby
  (bootstrap) turn. The call is the one `reporting-show` makes - the selection, marker root,
  workspace, hashed assignment, child session and turn - and writes nothing. An observer error is
  a `managed_reading_failed` gap.
- Every source is read in rotations bounded by its upper key at rotation start, and every
  rotation reaches the end. Asking whether a derived fault's source still produces it judges at
  most `PRESENT_CHECKS` deliveries against the send path's live rule per call; each overtaken
  verdict is kept (`fault_overtaken_deliveries`, permanent like the supersession it records) and
  excluded in SQL afterwards. Until every candidate is judged the answer is undetermined: the fault
  is not cleared and a `presence_undetermined` gap names it.
- Every automatically collected incident carries a `facts` evidence item: what was expected, what
  happened, the impact, what the reading cannot see, the subject (event, relationship, generation,
  turn) where its source holds them - the delivery, retry and refusal sources read generation and
  turn from the delivery's event - and the installation - package version, the location of
  the installed copy, and the revision it was installed from where the runtime installer's own
  record attributes one to this copy (`installation.revision`: `repositoryCommit`,
  `repositoryTree`, `subdirectoryTree`, `workingTreeClean`, with the entry's `environment` and
  `integrity`, and `revisionRecord` naming the file read). The record read is
  `$XDG_STATE_HOME/codex-relay-workflow/host-record.json`, or the same under
  `$HOME/.local/state` - the path `scripts/crw_runtime/hostrecord.record_path` writes - read as
  JSON, importing nothing from the installer. Only the install entry whose location is this
  package's own directory is read, and only its `source`, which the installer writes onto the
  entry in the same save that adds it; the component-level commit is never used, because a
  failed install's rollback removes its entry but leaves the component facts it wrote. Anything
  else - no record, an unreadable one, another record version, no entry for this location, or an
  entry written before entries carried their revision (every install made before this change,
  until it is reinstalled), or an incomplete one (any of the three tree identities or
  `workingTreeClean` missing or malformed) - leaves `revision` null with `revisionReason` saying
  which, and the observation then carries the unknown-revision limit. A copy installed from a
  working tree with uncommitted changes states its commit with the limit that the commit does
  not fully identify the installed bytes. The record is re-read whenever the file's device,
  inode, size or modification or change time differs from the last reading. First and latest occurrence are the ledger's own `first_seen_at` and `last_seen_at`.
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
  lease), failed, uncertain, awaiting record, and pending ones past its classification bound as
  `unclassified` - and returns a warning. An issued write in flight is counted (`issued`) without
  a warning; one whose lease has lapsed, or that no holder leases, is counted as `issuedLapsed`
  and warned about whether or not `expire_leases()` has run, because only a reconcile settles
  it. A notification reservation whose lease has lapsed is likewise counted
  (`notifications.reservedLapsed`) and warned about as uncertain before anything lapses it; `status` shows it under
  `faults`, and a daemon tick carries it as a note on the tick where it appears or changes.
- Notifications are `blocking` (a broken fault opened), `decision` (a write became uncertain
  or failed for good) and `resolved`. `notifications(*, limit)` lists pending ones with their
  eligibility: a paused, cancelled or archived relationship withholds one; a parent recipient
  that is uncontactable or unmeasured withholds one; a spent `notification` budget holds one.
- `raise_notification(fault_id, *, reason, ref)` lets a caller raise its own decision (for
  example an incident awaiting classification, or an owner or project hold). It is idempotent per
  fault and reason and enters the same eligibility, budget and reservation path as the ledger's
  own; there is one notification path.
- `reserve_notifications(*, owner, limit)` atomically takes eligible ones - round-robin by
  product, a spent product excluded inside the query, least recently examined first, every
  candidate examined stamped with the next examination sequence number
  (`fault_notifications.examined_seq`) - consumes their budget and leases them, each with a stable `deliveryKey` the deliverer must pass to its transport as
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

`policies(product, *, limit=20, after=None)` - one page of at most `limit` classes, each with every
severity, as `{policies, next}` - and `set_policy(product, fault_class, severity, *, threshold=None,
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
`fault_policies`, `fault_overtaken_deliveries`.

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

`product`, `faultClass`, `signature` and `scope.workspace` decide identity. The rest of `scope`
decides where the record is filed and is deliberately outside identity: a relationship can be
re-read into a different project without becoming a different fault. `severity` and the class
decide suppression.
`occurrenceKey` decides whether this is a new occurrence or the same one read again.

`observedAt` is the observer's own clock and is kept as displayed evidence ONLY. Nothing this
module decides is decided by comparing it; [the lifecycle](#the-lifecycle) says why.

A second product registers its classes with `register_class` and feeds `FaultLedger.record` the
same shape. Nothing in the ledger, the suppression rules or the publication path knows what
`crw` means.

## Identity: what makes two observations the same fault

`fault_id = sha256(product | faultClass | canonical(signature) [| workspace=<w>])[:32]`, with the
signature rendered as JSON with sorted keys so two callers building the same dictionary in a
different order produce the same id. The workspace part is present only when the observation
names one, so every id computed before workspaces joined identity is unchanged.

What is deliberately NOT in it: the time, the occurrence, the attempt, the project, and — the one
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
| `observation_unmeasured` | relationship and turn | `unmeasured:<relationship>:<turn>` | a later reading of the same turn that establishes something | notice |
| `delivery_refused` | relationship and refusal reason | `refused:<journal seq>` | the streak ending: a send, the delivery settling, a pause, or another reason | degraded |
| `managed_start_failed` | issue key and the answer (the registry's non-accepted receipt status, else the newest creation answer journaled) | `managed:<request>:<answer>` | the request recording an accepted receipt or attaching, or its newest creation-stage answer saying something else | broken |

A notice recorded per relationship before notices were per turn is still answered: any
establishing reading of that relationship clears it, under one constant key, so the clear is
recorded at most once. The two collected classes are described under [Collection](#collection).

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

`CLASS_POLICY` is the only place a class overrides that, and `set_policy` the only way a product
adjusts a degraded threshold or window - prospectively, journaled, and never for broken or notice
(see [Policy](#policy)). The window bounds which occurrences
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

A fault for which no write has landed is withdrawn when it clears, and its unsent writes are
cancelled — including one a locally recorded fix moved to `fix_pending`, and one that owns an
adopted issue no write has reached yet (invariant 5). A fault with a landed write is not closed by
clearing, because the closed loop below is what closes it.

Suppression is also the only thing that opens a record. Recording a fix or a resolution against
a fault the threshold never published queues nothing: a remediation must not be the back door
through which a notice reaches Linear. `queue()` is an explicit caller act for another kind and
refuses the issue create.

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
  row — one that never reached the connector — is safely released. `expire_leases()` takes at
  most 100 lapsed leases per call, oldest first, and returns `{released, uncertain, more}`; a
  lapsed row waiting for a later call is still refused by `claim()`;
- nothing but `reconcile` leaves `uncertain`. The caller reports what it observed: the marker
  found confirms the row against the issue that already exists; the marker absent moves it back
  to `pending` for one further create only when the caller attests that it looked AND somebody
  attests that the issuing request has ended (`fail(..., ended=True)` by the holder, or
  `reconcile(..., prior_ended=True, reason=...)`). Anything less leaves it uncertain, because a
  negative read is not proof of absence while a request may still land.

This is the same reasoning the coordination document uses for its conditional replacement, at
the one place where that technique is unavailable: you cannot conditionally replace a document
that does not exist yet.

`open_record` also needs somewhere to file: a team and a project, owned by the fault's product
and set per scope with `set_target` ([Targets and project linkage](#targets-and-project-linkage)).
A scope with no configured target, or none with a project, is not an error and does not lose the
fault: it stays recorded, its publication waits as awaiting target, and setting the target
re-points what was waiting. Filing into a guessed project would be worse than waiting.

## What runs by itself, and what does not

The daemon's tick sweeps the store and records what it finds, so a fault is detected and queued
without anybody asking. The pass is bounded like every other pass — each source reads at most
`SWEEP_LIMIT` rows — and it ROTATES: `fault_cursors` remembers where each source stopped and the
upper key it captured when its rotation started, the next sweep resumes there, and a short page
ends the rotation. Every row present when a rotation starts is read within that rotation, however
many full pages it takes; a row behind the cursor or past the captured bound is read by the next
one (invariant 14, `faultsweep._rotation()`). A fixed prefix re-read on every tick would have
starved everything behind it forever, and a cursor that wrapped after a fixed number of full
pages starved everything past them. The scan over open faults rotates too, for the same reason.
Given the store selection, the tick also reads the relay's own managed turns through the CRW-180
projection, a few per tick ([Collection](#collection)).

Recovery is asked of each fault DIRECTLY - an existence query for its own signature -
rather than by differencing against a page. A page is a bounded prefix, so once a source
holds more rows than one page no page is ever the whole source, and a rule that required
one would have stopped clearing anything exactly when a store got busy. An existence
query is exact however large the source is, and a class this cannot ask about is still
never cleared by absence.

The store's source rows carry no workspace: they are this relay's own, and a sweep records what
it derives under the workspace of its scope (none, for the daemon and `fault-sweep`). A fault of a
derived class recorded under another workspace - by a caller, for another tenant's relay - is
therefore neither cleared nor held open by this store's rows. Recovery judges the faults the
sweep's own observations resolve to through `canonical_id()` (so a fault moved to another
workspace is still judged under its id), faults in the sweep's workspace, and faults recorded
with none (`faultsweep._judged_here()`).

The pass counts only what was NEWLY recorded, so a steady-state failure read again on every tick
does not hold the loop at its fastest cadence forever. One refused observation is one
`observation_refused` gap and the rest of the batch is still recorded. Unsent writes are carried
as a note when the warning appears or changes. A daemon given no ledger ticks exactly as it did
before.

It cannot own the other half. The relay holds no Linear credential by design, so the write is
performed by the process that does — the coordination parent, or an operator running the
commands below. Until that consumer runs, a published fault is a queued publication, and this
module says exactly that rather than implying an issue exists.

### Who tells the level above (criterion 7)

Today, nobody delivers a fault notification. The ledger raises them - `blocking` when a broken
fault opens, `decision` when a write becomes uncertain or fails for good, `resolved` - and keeps
them `pending` with their eligibility, but the only caller of `reserve_notifications` on an
installed system is the `fault-notification-reserve` command. The daemon tick sweeps and
records; it never reserves or sends a notification, and nothing in the plugin wiring or the
skills does either. A pending notification therefore waits until a person or a coordinating
task runs `fault-notification-reserve` and acknowledges it. What does reach the level above
without anybody running a command is the unsent-write warning: `attention()` is on `status`
under `faults`, and the daemon carries it as a tick note when it appears or changes.

The smallest wiring that closes this, left as its own follow-up rather than built here: the
daemon tick reserves eligible notifications (`reserve_notifications`) and stages each through
the supervisor upward channel the relay already runs on its tick (CRW-215), passing the
notification's `deliveryKey` as the idempotency key, then acknowledges it from that channel's
own answer (`ack_notification`, or `reconcile_notification` after a lapsed lease). That keeps one
notification path - eligibility, budget and pause/archive/no-contact are already decided at
reservation - and one upward transport.

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
| `fault_targets` | the team a scope's fault issues are filed with |
| `fault_target_projects` | the product that owns a scope's target, and the project its issue creates are filed in |
| `fault_cursors` | each source's rotation position: where it stopped and the upper key captured when the rotation started (`pages` is no longer read) |
| `fault_publication_payloads` | a write's payload, the project it was queued with, and why it was last held |
| `fault_publication_attempts` | every claim of a write: owner, takeover, claim and issue times, outcome |
| `fault_links` | the project an owned issue is linked to as read back, and the link revision |
| `fault_adoptions` | an existing issue a fault adopts, until suppression opens the record |
| `fault_aliases` | another id naming the same fault: a move out of `unassigned`, or a legacy id |
| `fault_budget_uses` | budget units consumed, per product, kind and ref |
| `fault_limits` | per-product budget overrides, per kind |
| `fault_notifications` | blocking, decision and resolved notifications and their delivery state |
| `fault_policies` | per-product suppression overrides, with the reason |
| `fault_overtaken_deliveries` | deliveries the send path's rule was found to overtake, kept so an existence question stays bounded |

## Python API

This is the surface CRW-206 and any other product builds on. Everything below takes a
`Store` and an injected clock and performs no network call. The contract sections above say what
each call promises; this is the list.

```
faults.register_class(name, *, component, clears, threshold=None, window=None)
faults.register_kind(name, *, creates, requires_issue, target, evidence, confirm,
                     validate=None, pre_issue=None)
faults.observation(*, product, fault_class, severity, signature, occurrence_key,
                   scope=None, observed_at=None, detail="", evidence=(), cleared=False)
faults.fault_id(product, fault_class, signature, *, workspace=None)
faults.target_key(product, *, workspace=None, project=None)

ledger = faults.FaultLedger(store, clock)
ledger.record(observation, *, adopt=None)  -> faultId, recorded, state, publication
ledger.canonical_id(product, fault_class, signature, *, workspace=None)
ledger.adopt(fault_id, *, external_ref, scope)
ledger.move(fault_id, *, scope)          -> faultId, scopeKey, moved, repointed, repointPending, alias
ledger.set_target(*, product, workspace=None, project=None, team, project_ref=None)
ledger.targets(product=None, *, limit=20, after=None)
ledger.relink(*, limit=100)              -> relinked, relinkPending, backfilled, backfillPending
ledger.get(fault_id)                       # carries linkState and linkedProject
ledger.snapshot(*, product=None, fault_class=None, scope_key=None, state=None,
                limit=20, after=None)      -> faults, next
ledger.occurrences(fault_id, *, limit=3)
ledger.remediations(fault_id, *, limit=20)
ledger.record_fix(fault_id, *, ref, detail="")
ledger.record_reverification(fault_id, *, method, ref, outcome, detail="")
ledger.record_stage(fault_id, *, stage, ref, detail="")
ledger.progress(fault_id)
ledger.resolve(fault_id)
ledger.prune(fault_id, *, keep)
ledger.set_policy(product, fault_class, severity, *, threshold=None, window=None, reason)
ledger.policies(product, *, limit=20, after=None)   -> policies, next

ledger.queue(fault_id, *, kind, trigger, payload=None)
ledger.request_update(fault_id, *, op, value)
ledger.publication(publication_id)
ledger.publications(fault_id, *, kind=None, state=None, limit=20, after=None)
ledger.attempts(publication_id, *, limit=20)
ledger.next(*, limit=4)                    # fair across products
ledger.queue_state(*, limit=20)            -> ready, held (with reasons), budgets, budgetsTruncated
ledger.claim(publication_id, *, owner, takeover=False)   -> claimToken
ledger.operation(publication_id, *, claim_token)
ledger.reconcile(publication_id, observed_text=None, *, searched=False, observed=None,
                 prior_ended=False, reason=None)
ledger.complete(publication_id, *, readback=None, claim_token=None, external_ref=None,
                project_ref=None, observed=None)
ledger.fail(publication_id, *, claim_token, error, ended=False)
ledger.cancel(publication_id, *, reason)
ledger.expire_leases()                   -> released, uncertain, more (at most 100 per call)
ledger.retry(publication_id)

ledger.budget(product, kind)
ledger.consume(product, kind, *, ref)
ledger.set_limit(product, kind, *, max_count, window)
ledger.limits(product, *, limit=20, after=None)     -> limits, next

ledger.attention()
ledger.raise_notification(fault_id, *, reason, ref=None)
ledger.notifications(*, state=None, limit=20, after=None)
ledger.reserve_notifications(*, owner, limit=20)
ledger.ack_notification(notification_id, *, token, ref)
ledger.fail_notification(notification_id, *, token, error)
ledger.reconcile_notification(notification_id, *, delivered, ref)

faultsweep.sweep(store, *, product="crw", scope=None, readings=(), limit=32, policy=None,
                 readings_after=0, selection=None, now=None)
    -> observations, clears, gaps, completeSources, cursors, readingsNext, readingsTotal
faultsweep.reading_faults(readings, *, product, scope, store=None, limit=32, after=0)
faultsweep.managed_readings(store, selection, *, limit=8, cursor=None, now=None)
faultsweep.record_all(ledger, batch, *, store=None)
```

Every `limit` is a positive integer and is refused otherwise, because SQLite reads
`LIMIT -1` as no limit. A listing is continued by passing the `next` it returned as
`after`; the cursor is a rowid, so faults recorded between pages land after it. A second
product registers its classes and kinds once at import and feeds `record`; the ledger, the
suppression rules and the publication path need nothing else from it.

## Commands

```
fault-target      --product <p> [--workspace <w>] [--project <key>] --team <team>
                  [--project-ref <project>]
fault-observe     --observation <json|@path> [--adopt <json|@path>]
fault-sweep       [--product <name>] [--project <key>] [--readings <json|@path>]
                  [--readings-after <n>]
fault-show        [--fault <id> | --publication <id>] [--product <p>] [--fault-class <c>]
                  [--scope <key>] [--fault-state <state>] [--limit <n>] [--after <next>]
fault-fix         --fault <id> --ref <ref> [--detail <text>]
fault-reverify    --fault <id> --method suite|command|observation --ref <text>
                  --outcome passed|absent|failed [--detail <text>]
fault-stage       --fault <id> --stage accepted|assigned|merged|installed --ref <ref>
                  [--detail <text>]
fault-resolve     --fault <id>
fault-adopt       --fault <id> --external-ref <issue> --scope <json|@path>
fault-move        --fault <id> --scope <json|@path>
fault-queue       --fault <id> --kind <kind> --trigger <text> [--payload <json|@path>]
fault-update      --fault <id> --op set_project|reopen|add_relation|add_label
                  [--value <json|@path>]
fault-next        [--limit <n>]
fault-claim       --publication <id> --owner <name> [--takeover]
fault-operation   --publication <id> --claim-token <token>
fault-reconcile   --publication <id> [--observed <text|@path>]
                  [--observed-fields <json|@path>] [--searched]
                  [--prior-ended --reason <text>]
fault-complete    --publication <id> [--claim-token <token>] [--readback <text|@path>]
                  [--external-ref <ref>] [--project-ref <project>]
                  [--observed-fields <json|@path>]
fault-fail        --publication <id> --claim-token <token> --error <text> [--ended]
fault-cancel      --publication <id> --reason <text>
fault-retry       --publication <id>
fault-relink      [--limit <n>]
fault-policy      --product <p> [--fault-class <c> --severity <s> --reason <text>
                  [--threshold <n>] [--window <seconds>]] [--limit <n>] [--after <next>]
fault-limit       --product <p> [--kind <kind> --max-count <n> --window <seconds>]
                  [--limit <n>] [--after <next>]
fault-attention
fault-notifications          [--notification-state <state>] [--limit <n>] [--after <next>]
fault-notification-raise     --fault <id> --reason <text> [--ref <ref>]
fault-notification-reserve   --owner <name> [--limit <n>]
fault-notification-ack       --notification <id> --token <token> --ref <ref>
fault-notification-fail      --notification <id> --token <token> --error <text>
fault-notification-reconcile --notification <id> --delivered yes|no --ref <ref>
fault-prune       --fault <id> --keep <n>
```

Every one of these reads and writes the store and never calls the host, so each is listed in
`OFFLINE_COMMANDS`. `fault-next` returns the writes ready now, the held ones with the reason each
waits, and the budgets. `fault-show --fault` adds the fault's publications and stage progress;
`fault-show --publication` shows one write with what it created and its newest attempts; the two
are alternatives, and a listing filter (`--product`, `--fault-class`, `--scope`, `--fault-state`,
`--after`) beside either is refused rather than ignored. `status`
carries `attention()` under `faults`. The global option `--kind-module <module>` (repeatable)
imports a module that registers a publication kind before the command runs; a process that does
not import it never offers, claims or issues that kind's writes.

`--fault-state`, `--notification-state` and never `--state`: that name belongs to the global option
naming the store directory, and a subcommand option of the same name overwrites it in the
namespace, so every such command would read an unconfigured default store and answer that the
fault did not exist.

Status: implemented in this package, with the suite as the proof. Detection and queuing run in
the daemon tick; the Linear write is performed by a credential holder outside this package.
Nothing here is evidence about an installed runtime, a live service, or anything that has
actually been written to Linear.
