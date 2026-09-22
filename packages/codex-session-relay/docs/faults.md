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
| `delivery_stalled` | recipient, hold reason, last attempt state | the attempt's `request_id` | the sweep no longer deriving it | degraded, `broken` at the attempt cap |
| `record_sync_failed` | target and target ref | `(sync_id, attempts)` | the sweep no longer deriving it | broken |
| `observation_stalled` | relationship and generation | `(relationship, generation, turn, last_attempt_at)` | the sweep no longer deriving it | `broken` when never polled, degraded otherwise |
| `report_omitted` | relationship and turn | `observation:<relationship>:<turn>` | a reading that says `reported` | broken |
| `observation_unmeasured` | relationship | `unmeasured:<relationship>:<turn>` | a later reading that establishes something | notice |

The hold reason alone was not enough for a delivery. `attempt_cap` covers every pre-send failure
there is, so a settings rejection and a transport error would have merged into one record that
named neither; the last attempt's classified state is in the signature to keep them apart.

A never-polled anchor is `broken` rather than degraded, and that is not severity inflation. Its
occurrence key cannot change while nothing succeeds and no new attempt is recorded, so it
produces exactly one occurrence — and at degraded, which needs three, permanent scheduler
starvation would be the one failure that could never reach the threshold.

Every class declares what clears it, and that column is not documentation: `CLASS_POLICY`
carries it and `register_class` refuses a class without one. `refusal_recurring` is the absence
worth naming — the refusals table is append-only and carries no later success, so nothing in
this store could ever clear one, and it is therefore not registered.

## Occurrences and evidence

Each observation records an occurrence, keyed by `sha256(fault_id | occurrenceKey)[:32]` and
inserted once. The key matters more than it looks: the sweep runs on every tick and reads the
same stuck row each time, so without an occurrence identity one stuck delivery would count
thousands of occurrences within the hour and escalate itself past every threshold. The adapter
names the occurrence after the underlying fact — the attempt's request id, the publication's
attempt number, the poll's attempt time — so re-reading an unchanged state records nothing and
a genuinely new failure records exactly one.

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

A fault cleared before it reached its threshold is withdrawn and never touches Linear. A fault
cleared after publication is not closed by the clearing either, because the closed loop below
is what closes it.

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
without anybody asking. The pass is bounded like every other pass — each source is read at most
`SWEEP_LIMIT` rows — and it counts only what was NEWLY recorded, so a steady-state failure read
again on every tick does not hold the loop at its fastest cadence forever. A daemon given no
ledger ticks exactly as it did before.

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

## Commands

```
fault-target      --scope <key> --tracker-ref <ref>
fault-observe     --observation <json|@path>
fault-sweep       [--product <name>] [--project <key>] [--readings <json|@path>]
fault-show        [--fault <id>] [--scope <key>] [--fault-state <state>] [--limit <n>]
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
