# DAG summary outbox

A DAG plan's Linear summary is written by the project's parent with its own Linear connector. The relay holds no Linear credential, makes no Linear call and has no Linear client; what it holds is the
durable, ordered queue the parent drains: one table in the DAG zone and seven commands. A summary that is lost, refused or overtaken is recovered by writing a summary again. It never re-runs a child,
re-sends a correction or makes a second summary, because every command here writes the summary table and no other. This page is the normative description. It follows the DAG execution contract
(CRW-182) sections 1.3 and 7.6, the metrics of 8.6 and decision D-17: the relationship outbox (`sync_outbox`, [the relay README](README.md)) cannot hold a project summary, because
`sync_outbox.relationship_id` is NOT NULL, so a project-level outbox is a new table. `sync_outbox` keeps its relationship-level role and is not touched.

The summary states [the progress query](dag-progress.md) of the plan. The relay's log is the canonical plan and Linear is its projection (contract 1.3, decision D-13): this queue carries the projection
and decides nothing about the plan.

## The queue

A **stream** is a plan and a document (the Linear document the summary is written to: an id or a URL, opaque text of one line, at most 512 characters). Within a stream every entry has a sequence number
(`seq`) that only goes up, assigned by the relay in the transaction that appends the entry (1, then the highest plus one). The entry with the highest `seq` is the summary the plan owes the document; every older
entry that was not confirmed is superseded and is never written. Two plans, or two documents, are two streams, each from 1.

An entry is keyed by the decision it states, the document, the plan revision and the sequence number:

| Column | Meaning |
| --- | --- |
| `summary_id` | `sum-` and the first 32 hex digits of the sha256 of the canonical JSON of the plan, the document, the plan revision, the subject digest and the `seq`: the key as one value |
| `plan_id`, `project_key`, `document` | the stream (the project is the plan's, copied) |
| `plan_revision`, `seq` | the plan's head revision when the entry was made, and the entry's place in the stream |
| `subject_digest`, `state_digest` | the decision the entry states: the digest of the progress document it was made from (it contains the revision, the state digest and every node's stage), and the plan's state digest |
| `summary`, `summary_sha256` | the text of the summary and its digest |
| `state` | `pending`, `claimed`, `confirmed`, `failed`, `superseded` |
| `attempts`, `last_error` | failures recorded and the last one's text |
| `claim_token`, `claimed_by`, `claimed_at` | the claim: the token is present exactly while the entry is claimed |
| `readback`, `confirmed_at` | the block the parent read back, and when it was confirmed |
| `enqueued_by`, `coordinator_epoch`, `created_at`, `updated_at` | who made the entry, the coordinator epoch (0: nothing fences by epoch yet, see [DAG plans](dag-plans.md)) and the times |

An entry never changes its identity, and nothing is deleted. The moves of its state:

| From | To | By |
| --- | --- | --- |
| (none) | `pending` | `dag-summary-enqueue` |
| `pending` | `claimed` | `dag-summary-claim` |
| `claimed` | `claimed` | a second claim: a fresh token, the old one stops working |
| `claimed` | `confirmed` | `dag-summary-complete` with a readback that carries the entry's block |
| `claimed` | `pending` | `dag-summary-fail`: attempts plus one, the failure kept, the claim gone |
| `claimed` | `failed` | `dag-summary-fail` when it is the eighth failure |
| `failed` | `pending` | `dag-summary-retry`: attempts back to 0 |
| `pending`, `claimed`, `failed` | `superseded` | `dag-summary-enqueue`, for every entry of the stream that was not confirmed when a newer one is appended |

`confirmed` and `superseded` are final. There is no lease and no backoff: nothing here is resolved by time passing, and the parent decides when to claim again.

## An older summary never overwrites a newer one

Three layers keep it, and the first two are the relay's:

1. **The commands.** Only the newest open entry of a stream can be claimed; a claim, a confirmation, a failure or a retry of an entry that a newer one superseded is refused (`sync_not_claimable`). A claim holds
   a token and only a claim does, so a claimant that was overtaken, or a parent that was replaced (it is no longer the project's registered parent: `scope_role_mismatch`), can neither confirm nor fail the entry it no
   longer owns.
2. **The zone.** The same rules are enforced where the rows live, so a writer that skips the commands is refused too (`internal/relay/store/dag_zone.go`): an entry is appended pending, with the next sequence
   number of its stream and a plan revision that does not go back; there is one open entry per stream (a partial unique index), so older entries are superseded before a newer one is appended; only the legal moves
   above are allowed; an entry that has a newer sibling is never claimed or confirmed; a confirmed or superseded entry never changes; nothing is deleted.
3. **The document.** The write itself is one atomic conditional replacement of the plan's container text, and so is the creation of the container (a replacement of the whole document text that was read with that text and
   the empty container: two claimants that both found no container cannot make two). The parent's connector is Linear's `save_document` with `patch`: its description says the edits are applied to the
   current content in order and atomically, and an `old_string` must match the current content exactly once. So a write prepared before a newer summary landed no longer matches (the container changed) and is refused whole.
   The parent also writes only against the one read it reconciled, and never when `writable` is false or the document already holds a newer summary (`relation: newer`): a claimant that reads the document after a newer summary
   landed would otherwise match what it just read. This layer belongs to the connector and to the parent's procedure. The relay states the exact text to write and checks what was read back; it cannot make Linear conditional.

What the relay cannot close: a write that is not conditioned (a whole-document `content` save, an append) by a writer outside this procedure can put an older summary over a newer one. The next `dag-summary-reconcile` of the newest
confirmed entry reports it (`outcome: stale`, `relation: older`, `again: true`) and `dag-summary-enqueue --again` writes the summary anew. The relay's tests model the connector as the conditional replacement and show the
effect of a writer that is not (the overwrite is counted, detected and repaired). They do not exercise real Linear, and the atomicity of Linear's patch is the connector's documented behaviour and not something this change measured.

## The commands

All are relay commands (`codex-session-relay [--state DIR] <command>`); output is JSON on stdout. Exit 0 is an answer, 2 a refusal with its reason (JSON), or the parser's refusal of a missing option (usage on stderr, nothing on stdout), 3 the host's failure (a plan or a progress that disagrees with itself), 4 a usage
error of the command (a `@file` that cannot be read, a document over 8 MiB or not valid UTF-8: a document is read whole or not at all). The write commands name the task that acts (`--actor`): it must be the one registered parent of the plan's project.

| Command | Reads or writes | Answer | Refusals |
| --- | --- | --- | --- |
| `dag-summary-enqueue --plan P --actor A --document D [--again]` | reads the plan's progress and writes the entry in one transaction | `dag-summary-enqueue/1`: `replayed`, `superseded` (ids) and the `entry` | `unregistered_scope` (no such plan), `scope_role_mismatch`, `malformed_receipt` (a document that is empty, padded, over-long or not one line) |
| `dag-summary-status --plan P [--document D] [--history]` | reads only, store opened read-only | `dag-summary-status/1`: per document the newest entry, `owed`, `up_to_date`, `counts` per state and, with `--history`, every entry | `unregistered_scope`, `store_absent` |
| `dag-summary-claim --summary S --actor A` | claimed, a fresh token | `dag-summary-claim/1`: `claim_token`, the `entry` and the `operation` (document, container markers, `empty_container`, `block`, `container`, `protocol`) | `unregistered_scope`, `scope_role_mismatch`, `sync_not_claimable` (confirmed, superseded, failed, or not the newest) |
| `dag-summary-reconcile --summary S --observed TEXT or @file` | reads only, store opened read-only | `dag-summary-reconcile/1`: `outcome`, `detail`, `writable`, `again`, `repair`, `container`, and the block the document holds (`document_summary_id`, `document_seq`, `relation`, `previous_block`) | `unregistered_scope` |
| `dag-summary-complete --summary S --actor A --claim-token K --document D --readback TEXT or @file` | confirmed | `dag-summary-complete/1`: `replayed` and the `entry` | `unregistered_scope`, `scope_role_mismatch`, `sync_not_claimable` (the token is not the one held, or the entry was superseded), `sync_target_mismatch` (another document), `readback_mismatch` |
| `dag-summary-fail --summary S --actor A --claim-token K --error TEXT` | pending (or failed), the claim gone | `dag-summary-fail/1`: the `entry` | `unregistered_scope`, `scope_role_mismatch`, `sync_not_claimable`, `malformed_receipt` (an empty or over-long error) |
| `dag-summary-retry --summary S --actor A` | failed back to pending | `dag-summary-retry/1`: `replayed` and the `entry` | `unregistered_scope`, `scope_role_mismatch`, `sync_not_claimable` (the entry is final) |

No refusal reason is added: the reasons above are the relay's existing ones and mean here what they mean for the relationship outbox (`sync_not_claimable`: the job cannot be claimed or its claim is not held; `readback_mismatch`: the
readback does not carry this job's record; `sync_target_mismatch`: the right text in the wrong document). No reading reason is added either.

**Enqueue** records the summary the plan owes the document now. The same state of the plan is one entry: if the stream's newest entry already states it (same `subject_digest`), that entry is answered with
`replayed: true`, whatever state it is in. `--again` makes a new entry even then, but only when the newest entry is confirmed: an entry that is still owed is never doubled. `status` says whether the plan has moved on since the newest
entry (`up_to_date`), so the parent enqueues when a document is owed nothing and is out of date.

**Claim** gives the operation: the exact `block`, the exact `container` text that the document's current container is replaced with, the `empty_container` that `initialize` adds to a document that has none, and the protocol
as the relay states it. A second claim of the same entry is allowed at once (the answer of the first may be lost): it issues a new token and the old one is dead.

**Reconcile** describes a document against an entry and is how a lost response is resolved without writing twice. `repair` says how the entry is written into that document:

| Outcome | The document holds | `repair` | The parent |
| --- | --- | --- | --- |
| `already_written` | this entry's block, and nothing else in the plan's container | `none` | confirms with `complete`; writes nothing |
| `absent` | no block of this plan | `initialize` when there is no container, else `replace_container` | writes |
| `stale` | the plan's one block, but another entry's (`relation` older: replace it; newer: this entry must not be written), or this entry's with other text, or a container with more than the block | `replace_container`, or `none` for `relation: newer` | writes, or takes the newest entry |
| `duplicate` | more than one block of this plan, all inside the plan's one container | `replace_container` | replaces the container's text |
| `malformed` | a block or container that is not closed, a block outside the container, or two containers | `manual` | writes nothing, records the failure and asks a person: markers that appear twice cannot be replaced by an exact-once replacement |

`replace_container` is one conditional replacement of the container text that was read (the markers appear once, so it matches once). `initialize` is one conditional replacement of the whole document text that was read with that text
and the empty container. `writable` says whether the entry is open (pending or claimed): an entry that is superseded or confirmed is not written, and its `repair` is `none` whatever the document holds. `again` is true when the
entry is confirmed and the document no longer carries it.

**Complete** takes the whole document as read back. It confirms only if reconcile would say `already_written`; otherwise it refuses with `readback_mismatch`, keeps the problem in `last_error` and leaves the entry claimed. Completing a
confirmed entry answers its record (`replayed: true`) and checks nothing more: a confirmation is monotonic.

**Fail** and **retry** are the recovery of one entry. A failed write goes back to pending and the next claim is the same entry again (only the newest open entry can be claimed, so nothing else is retried). The eighth failure
parks the entry as `failed` and a claim of it is refused until `dag-summary-retry`, which touches that entry only.

## The summary text and its block

The text is a function of the progress document and holds no clock: equal progress is equal text. It names the plan, the project and the revision, the counts of accepted and integrated nodes against the
denominator, the denominator's last change, what the revisions took out of the plan, every stage with its nodes, the blocked nodes with their closed reasons, and every live node with its issue key, kind, stage, reason, the
plan's lifecycle for it and its pull request.

It is written as a block inside a container, plain text that a connector keeps byte for byte (HTML comments around a fenced body: the grammar the relationship outbox relies on, which JUN-92 measured for Linear):

    <!-- relay-dag-summary-container:PLAN -->
    <!-- relay-dag-summary:SUMMARY_ID -->
    blockFormat: dag-summary/1
    summaryId: SUMMARY_ID
    planId: PLAN
    projectKey: PROJECT
    document: DOCUMENT
    planRevision: 5
    seq: 3
    stateDigest: ...
    subjectDigest: ...
    summarySha256: ...

    ```text
    the summary text
    ```
    <!-- /relay-dag-summary:SUMMARY_ID -->
    <!-- /relay-dag-summary-container:PLAN -->

There is one container per plan, so two plans of a project may write one document; a container holds one block. The fence is longer than any run of backticks in the text. A readback is compared after the
line endings are made `\n` and the blanks at the end of each line are cut, so a connector that normalises them does not make a written block read as another one. The relay judges a document by parsing these markers and
the headers, never by looking for words.

## The parent's flow

The procedure is in the crw-run skill (`references/relay.md`, "The Linear summary of a DAG plan"). In order:

1. `dag-summary-status --plan P`: a document whose newest entry is owed, or is not up to date, needs a turn. If the plan has moved on, `dag-summary-enqueue --plan P --actor A --document D` first.
2. `dag-summary-claim --summary S --actor A`: take the newest open entry. An entry that already reads `claimed` (an earlier session of the parent, a restart) is claimed again the same way: only the second claim ends the earlier token.
3. Read the document from Linear and keep that one text. `dag-summary-reconcile` on it. `writable: false` or `relation: newer`: stop, the entry was overtaken, take the newest entry. `already_written`: go to 5.
4. Write as `repair` says: `replace_container`: one `save_document` with `patch` [`{op: replace, old_string: <the container text from that read>, new_string: <the container text>}`]; `initialize`: one `patch` that replaces the whole document
   text that was read with that text, a blank line and the empty container, then read again and reconcile again; `manual`: write nothing, record the failure and report it to a person. On any failure: `dag-summary-fail` with the claim token and the error, and go back to 2 for the same entry.
5. Read the document back and `dag-summary-complete` with the claim token, the document and the whole text. `readback_mismatch`: reconcile the readback and act on its `repair`. `sync_not_claimable` after a newer summary was enqueued: take the newest entry.

Everything in this loop touches the summary table only. A lost response (the write landed and its answer did not) is a failure to the parent, and the next turn finds the block in the document (`already_written`) and confirms it
without a second write; a concurrent edit makes the replacement refuse whole, and the retry reads the document as it is then; a replaced parent is refused at the relay, and the one write it had prepared is refused by the
connector once the container changed.

## What is not here

* The relay never reads Linear and never writes it. There is no Linear client, no credential and no daemon for this queue; the parent calls these commands when a wake, a block or a decision brings it back (see
  [the scheduler's parent turn](dag-scheduler.md#the-goal-free-parents-turn)).
* A store that still owes a repair the writable opener makes (a settlement backfilled from an observation, an index that was dropped) gets it at the first write-open of any write command, the five write commands here included.
  That is the opener's behaviour and not summary recovery: `status` and `reconcile` open the store read-only and change nothing.
* A summary is only as good as the document it is written into. A document with markers that appear twice, an unclosed block or container, or a block outside its container is `malformed` and no exact-once replacement can repair it:
  the relay never confirms such a document, and a person removes the surplus text. Two sessions of the parent that both initialise a container are made safe by the conditional replacement of the whole document, not by the relay.
* A failure has no backoff. The eighth parks the entry, so a parent that fails in a loop cannot burn more than eight attempts on one entry, and they can all be spent in one turn.
* A change of the progress document's content between builds gives a different digest: one extra entry after an upgrade, never a wrong one.
* Progress replay, stale handling, coordinator fencing and the parent's base refresh are later issues. These writers decide nothing about the plan and are not fenced by [the coordinator epoch](dag-scheduler.md#the-coordinator-epoch): a summary states the store's current progress, enqueueing it again is a replay, and the claim token fences a replaced session's confirmation or failure. A coordinator claim (`dag-coordinator-claim`) does not touch summary tokens: a restarted or replacement session claims again, at once, any entry that reads `claimed` (step 2 of the flow), and that second claim is what kills the old session's token. `coordinator_epoch` is 0 on every entry, so a later fence has the column.

## The store

One table, one index and five triggers appended to the DAG zone ([DAG plans](dag-plans.md#the-store)): `dag_summary_outbox`, `dag_summary_outbox_open` (one open entry per stream),
`dag_summary_outbox_order` (insert order), `dag_summary_outbox_immutable`, `dag_summary_outbox_transition`, `dag_summary_outbox_newest` and `dag_summary_outbox_no_delete`. Their text is in `testdata/dag_zone_shipped.json` and a shipped statement is never edited. The install swap gate reads the new
objects as `EXTENDS_ZONE` on a store whose zone predates them (the install command with `--backup-state-to`, as for any zone growth), and a build without them still opens a store that has them and never touches them
(`NARROWS_ZONE`).

## Calling it from Go

`internal/relay/dagsched`: `Scheduler.EnqueueSummary`, `SummaryStatus`, `ClaimSummary`, `ReconcileSummary`, `CompleteSummary`, `FailSummary`, `RetrySummary`; `SummaryText(Progress)` is the text.
Each write runs in one composing transaction of the store; `EnqueueSummary` reads `Scheduler.Progress` in the transaction it writes in.

## Where this page reads the contract

* Contract D-17 leaves the key open: this page keys an entry by the plan, the document, the plan revision, the decision it states (the progress digest) and a sequence number that only goes up in the stream. The contract's
  "decision identity" for a relationship job is its verdict; for a project summary it is the progress the summary states.
* Contract 7.6 says a Linear write is retried as the summary only and starts no child and no judgement. That is the table above: no other table is read or written by any of these commands but the plan's own rows
  (the progress reading) and the project's scope bindings (who the parent is).
* D-19: the contract's citations of the sections this issue consumes (`relay-sqlite.sql` lines 356-386 and 362 for `sync_outbox` and its `relationship_id`, and `operations.md` 1040-1045 for "the queue is the truth, a wake is only a hint") were
  resolved at the contract's commit and at this baseline: all identical.
