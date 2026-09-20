# Protocol v1

Derived from the frozen cross-session communication contract, bundle revision
`c37d332e2daba95c9ef47adf00a82bc9c6a539ab62538f0ad857561e470d2989`. This document is portable: it
carries no task identifiers, host paths or private history. The five JSON schemas ship under
`src/codex_session_relay/schema/`.

**Status: planned.** The protocol below is frozen and real. Its implementation is not yet written.

The three-level execution linkage - an initiative supervisor over a project parent over an issue
child, and peer links between parents - is NOT part of this contract and has no schema here. It
is a relay-owned record set, described in [linkage.md](linkage.md).

## 1. Identity

Identity is the pair of actual task ids. A title, or "the most recent session", is never a routing
key anywhere in this system.

    relationshipId = "rel-" + sha256(parentTaskId|childTaskId|issueKey)[:16]

Deterministic, so re-registering after an uncertain response is idempotent rather than duplicating.
`hostId` is carried for routing and audit but stays outside the hash, because thread ids are
already globally unique.

A relationship carries `authorizedScope`: `artifactRoots`, the absolute directory prefixes every
deliverable path must lie under, and `allowedRecipients`, the task ids it may deliver to. A
delivery naming a recipient outside that list, or a manifest path outside those roots, is refused
before any transport call.

`executionGeneration` is an ordinal per relationship, incremented when the parent issues a new
assignment or a revision request. Every generation is retained with its own anchor. An anchor is
`bound` only to the exact dispatch turn id from the dispatch receipt. `anchor_pending` never
auto-binds: an unrelated turn that happens to appear next does not become the registered execution.
Pending is a reportable state, not a problem to guess away.

## 2. Completion receipts

A host turn reaching "completed" is the trigger to look. It is never by itself a reviewable result.

Five outcomes are distinguished, and only the first asserts a product:

| Outcome | Who may assert it |
|---|---|
| `ready_for_review` | the child only |
| `failed` | the child, or a daemon observing a failed turn |
| `interrupted` | the child, or a daemon observing an interrupted turn |
| `blocked_needs_input` | the child only; no terminal turn status identifies it |
| ordinary turn end | neither; a completed turn with no child receipt is not a completion event |

`eventId` derivation is split, because one identity cannot serve both purposes:

    ready_for_review:  sha256(relationshipId|generation|revisionHash|outcome)[:32]
    otherwise:         sha256(relationshipId|generation|outcome|turnId|attempt)[:32]

Product-level for the first, so re-emitting one reviewable revision collapses instead of
duplicating verification. Execution-level for the rest, because there is no revision to identify
and two interrupted turns in one generation are two facts. Canonical rendering: fields joined by a
pipe, integers unpadded, a null attempt as the literal `null`.

An execution-only receipt carries `manifest: null` and the `NO_DELIVERABLE` sentinel of 64 zeros,
for any producer. The JSON schema only enforces that for daemon observations; the runtime enforces
it for the child too, because the contract prose is the stronger rule.

### Canonical manifest serialization

1. Each entry path is an ABSOLUTE, normalized POSIX path. No relative path, no `~`, no symlink
   resolution, no trailing slash. The path is used exactly as written.
2. Entries are sorted by that absolute path string, byte-wise ascending.
3. Each entry serializes as `<absolutePath>:<lowercase hex sha256 of the file bytes>`.
4. Entries are joined with a single `\n`. There is no trailing newline.
5. `revisionHash` is the lowercase hex sha256 of that joined UTF-8 string.

Because the absolute path is inside the hash, moving deliverables changes `revisionHash` even when
every byte is identical. A receipt that must survive relocation carries `manifestRef` pointing at a
frozen copy, and a consumer verifies against that copy rather than the moved files. The frozen copy
stores the ORIGINAL declared paths, so the digest never changes.

The consumer recomputes `revisionHash` and re-hashes the actual bytes. It never trusts the claim.

## 3. Delivery

    requestId = "del-" + eventId[:12] + "-a" + attemptNo

Stable within an attempt so an uncertain send is reconciled rather than duplicated, and
deliberately separate from `eventId` so a retry never reads as a second completion.

States: `queued`, `deferred_busy`, `withheld_pre_send`, `dispatched`, `held_uncertain`,
`inbox_only`, `acknowledged`, `superseded`. `dispatched` means the transport returned a turn id.
It is not acknowledgement and not verification. **No state is reachable by a timeout.**

### Transport classification

One operation receipt becomes one set of delivery facts. The error code is read before the method
prefix, because a busy refusal and an approval-policy refusal would otherwise be misread.

| Receipt | State | sendAttempted | retrySafe |
|---|---|---|---|
| accepted with a non-empty turn id | dispatched | yes | no |
| accepted without a usable turn id | held_uncertain | unknown | no |
| unfinished | held_uncertain | unknown | no |
| transport outcome unknown | held_uncertain | unknown | no |
| busy refusal before resume | deferred_busy | no | **yes** |
| read or resume refusal before resume | withheld_pre_send | no | **yes** |
| approval policy refusal after resume | inbox_only | no | no |
| reconnect failure | held_uncertain | unknown | no |
| turn-start failure | held_uncertain | yes | no |
| anything else | held_uncertain | unknown | no |

`retrySafe` is true only for a completed, attributable pre-send rejection. An unfinished receipt is
never proven non-delivery: the transport persists a receipt before its calls run, so a missing
field can simply mean the operation has not got there yet.

A retry always opens `attemptNo + 1`, so it carries a new `requestId`. Replaying the same request
id would return the transport's cached failure forever.

### Dual channel

The durable inbox is the guarantee. A parent whose approval policy is not "never" cannot be pushed
to at all, so a push-only design would be undeliverable to exactly the interactive parents this
exists to serve. The recipient's approval policy is recorded verbatim, so an audit can tell an
inbox-only fallback from a push never attempted.

## 4. Reconciliation

An uncertain attempt stays uncertain until affirmative evidence arrives. The order is fixed:

1. Re-read the operation receipt for the same `requestId`.
2. Only then read the recipient's exact actual turn items.

Exactly three things are affirmative evidence: the receipt carries a turn id, a turn matching this
attempt exists in the recipient's items, or a confirmed pre-send rejection. Elapsed time is not on
that list, and a timeout never opens a new attempt.

Item paging uses the forward cursor in one direction with a bounded total scan. A scan that stops
before the cursor is exhausted is inconclusive — absence in a truncated page is not proof of
absence. The reverse cursor is only for a deliberate direction change. Cursor bytes are opaque and
passed through verbatim.

When only the item scan finds the delivery, the stored attempt keeps its honest transport snapshot
and gains a reconciliation record. An accepted transport status is never fabricated.

## 5. Acknowledgement

Only the parent, from inside a real parent turn, closes `dispatched` to `acknowledged`.

    ackProof = sha256(eventId|ackTurnId)

The parent supplies it, computed over its OWN turn id, which does not appear in the message it
received. That is the difference between a recipient that acknowledged and a recipient that echoed.
The relay verifies the proof rather than computing it for the caller, verifies the turn exists in
the recipient's real turn list, and verifies it started after the delivery. An acknowledgement whose
turn cannot be verified is stored and reported, but does not close the attempt.

Disposition is enforced in the same transaction: an event that went stale, unknown, duplicate,
revision-mismatched, or whose relationship is no longer active cannot be accepted.

## 6. Verdicts and the reverse direction

A verdict is `verified`, `needs_changes`, `unverified` or `aborted`, with per-criterion detail, and
requires an acknowledged event. `needs_changes` opens a new generation and routes the revision
request back to the SAME child. The child does not stay active waiting for review; the request
reaches it when it can receive one.

**Recorded boundary.** Contract v1 defines its delivery and acknowledgement record types for the
child-to-parent direction only: the delivery record's recipient is specified as the registered
parent, and the acknowledgement is parent-authored. The parent-to-child revision request therefore
reuses the same delivery mechanism but is stored as a relay-internal record, explicitly outside
those schemas. No child-authored acknowledgement is invented. Receipt is evidenced the way the
contract already provides for: the dispatch receipt's turn id binds the new generation's anchor,
and the child's real answer is its next completion receipt under that generation. A future contract
revision may define this type; this implementation does not silently become that decision.

## 7. Recorded deviations

| Deviation | Reason |
|---|---|
| Terminal turns are detected by bounded polling rather than a receive-only notification connection | the contract itself lists as unresolved whether a client receives turn completion for threads it neither started nor resumed, and the transport cannot subscribe. Polling establishes the behaviour with the supported read path; a notification listener ships as an optional early-wake hint that is never evidence |
| There is no operator release: an attempt with no affirmative evidence stays held | the contract lists three affirmative evidences and does not contemplate an operator override, so none is implemented. An attempt holding none of them stays held and reports what it is missing, and there is no flag, audited or otherwise, that releases it. An earlier revision of this table described an implementation behind an `allow_operator_release` switch; that is not in the delivered core. `RefusalReason.OPERATOR_RELEASE_DISABLED` is retained in the taxonomy and is unreachable, because no code path raises it |

## 8. Work reports

A delivered message has to be something the recipient can act on. The identifiers above are
how it answers, not how it decides, so the message leads with the result, the repository and
pull request, the base and head commit, what was verified, what is still unresolved, and
what to do next.

None of that fits in a completion receipt. The five schemas are frozen with
`additionalProperties: false`, so a work report is a relay-owned record, like the criteria
set and the revision request, stored against the exact event, relationship, generation and
revision it describes. The repository is stored beside the pull request number and the two
are never rendered apart, so the same number on two projects stays two pull requests. A
report about one head cannot answer for another: a later push is a new report, not an
inherited one.

An event with no work report renders exactly what it rendered before. A receipt from before
this contract has no pull request to centre a report on, and inventing one would be the same
guess this implementation refuses everywhere else. `report.version_of` names which of the
two an event is on, and the report-backed body says so in its own `contract:` line.

When a message has to be shortened, the shortening is announced. Required action, scope and
unresolved items keep their headings and gain an explicit count of what is missing, every
reduction adds one `omitted:` line naming what was dropped and the command that shows the
whole record, and a budget too small to hold the required parts refuses rather than shipping
a message that silently lost them. A message that quietly drops its unresolved items reads
exactly like a message that had none.

The CXC report vocabulary this maps onto is documented separately in
[the CXC contract map](cxc-contract-map.md). Its one load-bearing rule: a `DONE` report, an
opened pull request, a review `PASS` and a green required check are all evidence for a
verdict and none of them is one. Only the parent, from inside its own turn, against
registered criteria, writes `verified`.
