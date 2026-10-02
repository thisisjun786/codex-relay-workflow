# DAG scheduler

The scheduler runs a stored DAG plan ([DAG plans](dag-plans.md)). It computes which nodes may start, says why every other node may
not, and (in the later sections of this page) releases a ready node to a Codex child, records what the parent accepted and what
landed, and judges merge eligibility. It is code in `internal/relay/dagsched`, reached through `crw relay dag-*` commands that read
and write the relay store the other relay commands use. It holds no goal, runs no loop, starts no daemon and opens no database of
its own: a goal-free parent calls it when a relay result, a block or a decision request wakes it, acts on the answer and ends its
turn. No predicate here depends on a model's judgement.

The page follows the DAG execution contract (CRW-182) sections 2, 3, 4, 7 and 8; where the code departs from the contract's wording it
says so in [Departures](#departures-from-the-contract).

## The goal-free parent's turn

The parent holds no goal and runs no loop. When a relay result, a block or a decision request wakes it, one turn goes like this, and it ends when only waiting remains:

1. `dag-ready --plan P` (add `--record --actor A` to keep the pass): the nodes that are ready, in release order, and for every other live node its one reason. The store is the truth and a wake is only a hint: the same reading is
   right whether the wake was the first, a duplicate or a late one.
2. For each ready node, `dag-release` (a node that is already owned is `skip:already_owned` in the reading and a replay in the release: no second child).
3. For a result that is reported and verified: `dag-accept`; for a correction: `dag-correct --prepare`, the `needs_changes` ruling carrying the line, `dag-correct`; for a decision edge: `dag-decision-record`.
4. For an accepted pull request: `dag-merge-request`, the merge lane's own commands, `assignment-mark merged`, `dag-integration-observe`.
5. For the plan's Linear summary: `dag-summary-status --plan P`; when a document is owed or out of date, `dag-summary-enqueue` and the claim, write, read back and confirm of [the summary outbox](dag-outbox.md). It writes the summary table only, so it never
   re-runs a child or re-sends a correction.
6. End the turn. Nothing is resident: the scheduler is a library the relay's commands call over the relay's one store; it adds no daemon, no listener and no database of its own
   (`TestNoNewStoreOrDaemon`, `TestForkJoinLeavesNoStoreOfItsOwn`).

The scheduler re-implements no I-17 loop or skill, and contract section 10 names no I-17 output that this issue needs.

## The ready set

`crw relay dag-ready --plan P` reads the plan at its head revision and the relay's execution records and answers, for every live node,
one disposition and (except for a ready node) one reason from a closed set. A reading writes nothing, reads no clock and opens
no second connection, so with unchanged store rows and artifact bytes two readings are byte-identical. Its `input_digest` hashes the plan identity, the node states and dispositions, the selected order, the capacity summary and the hashes of the artifact files it checked;
it is not a fingerprint of every source row read. A store with no DAG zone, or a plan it does not hold, answers `unregistered_scope` and is left as it was.

A node is a **candidate** when it is not owned and every incoming edge is satisfied (the predicates are [Edge satisfaction](#edge-satisfaction)).
Candidates are then checked against what lies outside the plan, ranked, and cut by free slots and by edit-region overlap.

### Dispositions and reasons

| Disposition | Reason | Meaning |
| --- | --- | --- |
| ready | | released next, in the order of `ready` |
| wait | `wait:edge:<edge_id>` | the first unsatisfied incoming edge (by edge id) has no recorded result yet |
| defer | `defer:no_capacity` | a candidate that the free slots do not reach: the binding ceiling is full |
| defer | `defer:capacity_unmeasured` | an enforced ceiling on a dimension nobody measured leaves no basis to say a slot is free |
| defer | `defer:edit_overlap` | its edit regions overlap a node that is running or accepted and not yet landed, or a region is undeclared |
| defer | `defer:merge_window` | a merge turn is moving (merging, or unknown) a branch the node's edges name |
| defer | `defer:ownership_unverified` | the plan's project does not have exactly one registered parent to reserve under |
| defer | `defer:authority_pending` | a decision edge has no recorded decision, from a required authority, with the digest the plan fixed |
| skip | `skip:already_owned` | a child, a release or a managed start already owns the issue |
| blocked | `blocked:manifest_incomplete` | the input manifest lacks a required field (B-01, B-16) |
| blocked | `blocked:manifest_tampered` | a manifest no longer digests to its name (B-02) |
| blocked | `blocked:input_missing` | an artifact a predecessor declared is not there, or the list or frozen copy cannot be read, or a code pin has nothing to pin (B-03, B-17) |
| blocked | `blocked:input_hash_mismatch` | an artifact's bytes or size differ from what the receipt declared (B-04) |
| blocked | `blocked:input_out_of_scope` | an artifact lies outside the predecessor's artifact roots (B-05) |
| blocked | `blocked:input_unaccepted` | the predecessor's acceptance is not tied to an execution of that node, or is not in the store (B-06) |
| blocked | `blocked:acceptance_tampered` | an acceptance row no longer digests to its id (B-07) |
| blocked | `blocked:acceptance_incomplete` | a required column of an acceptance is blank (B-08) |
| blocked | `blocked:stale_head` | the accepted revision is no longer the head of its generation, or the pull request head moved after acceptance (B-09) |
| blocked | `blocked:stale_criteria` | an acceptance stands on criteria that are no longer the plan's and the relationship's (B-10) |
| blocked | `blocked:decision_mismatch` | an approved decision of the subject exists, with another digest or authority (B-11) |
| blocked | `blocked:integration_unprovable` | a merge landed the head, but the target does not contain it: a squash or rebase landing (B-12) |
| blocked | `blocked:evidence_mismatch` | a merge check no longer digests to what was recorded (B-13) |
| blocked | `blocked:inconsistent_inputs` | the inputs rest on two acceptances of one node (B-14) |
| blocked | `blocked:input_unverified_at_consumption` | the child's own newest report is blocked_needs_input (B-15) |
| blocked | `blocked:stale_predecessor` | an input of an accepted predecessor is no longer the active acceptance of its node, or the accepted result of the predecessor is stale (see [Invalidation](#invalidation)): the detail names the edge |
| blocked | `blocked:creation_unknown` | a child's creation was armed and its outcome is not known; repeating the release reconciles it |
| blocked | `blocked:effect_unknown` | a merge turn for the accepted head ended with an unknown effect |
| blocked | `blocked:predecessor_cancelled` | the predecessor's relationship was cancelled before its result was usable |
| blocked | `blocked:release_abandoned` | the managed start of a decided release was released before it created a child |
| blocked | `blocked:evicted` | a required check failed again on the same head after its one retry, and the node left the merge lane |
| blocked | `blocked:ambiguous_head` | the relationship has more than one head revision, or an execution has no relationship |
| defer | `defer:plan_paused` | the plan is paused: nothing is released, accepted, corrected or sent to the merge lane until a resume revision |
| defer | `defer:node_paused` | the plan paused this node: it is not released, accepted, corrected or sent to the merge lane until a resume revision, and its slot stays held |
| skip | `skip:node_cancelled` | the plan cancelled this node: it is never released again, its result is not counted as done, and the cancel is not reverted |
| skip | `skip:node_archived` | the plan archived this node: it is never released again and nothing downstream is released from its result |
| blocked | `blocked:predecessor_archived` | the predecessor, or a node the predecessor's accepted result rests on, was archived by the plan before its result was usable |
| stale | `stale:slice_changed` | the accepted node's own slice (its spec, or its incoming edges) is no longer the one its consumed manifest recorded, and no single edge explains it |
| stale | `stale:criteria_changed` | only the node's criteria changed, and the same output has not been re-verified against them |
| stale | `stale:edge:<edge_id>` | the accepted node rests, over that incoming edge, on something that changed: an edge added or retired, a predecessor whose result is stale, an acceptance or decision it consumed that is no longer the one that satisfies the edge |
| done | `done:accepted` | the parent accepted the node's result |
| done | `done:integrated` | the accepted head is contained in every target it has to land on and the parent marked it merged |

`blocked:stale_epoch` is in the contract's vocabulary and is not emitted: coordinator fencing is a later issue.

The derived state of each node is shown beside its disposition: `waiting`, `ready`, `releasing`, `creation_unknown`, `running`, `reported`,
`verifying`, `correcting`, `accepted`, `stale`, `integrated`, `paused`, `cancelled`, `archived`, `closed` or `ambiguous` (a node of a paused plan that nobody owns reads `planned`). It is read from the rows each time and never stored.

### Edge satisfaction

An edge is satisfied by what the store holds now. Every predicate is scoped to the plan: node and edge ids are plan-local.

* `artifact_verified`: the predecessor has an active acceptance that digests to its id, has every required column, belongs to an
  execution of the node, stands on the plan's and the relationship's criteria (a later re-verification counts), is still the head of its
  generation while the relationship lives, rests on the active acceptances of its own inputs, and, for a code edge, carries the head, the
  pull request and the forge identity the edge pins. The latest merge check of the pull request, if any, must digest to its evidence and name
  the accepted head. A cancelled predecessor satisfies nothing.
* `integrated`: an observation says the accepted head is an ancestor of the target branch, no later observation says otherwise, the parent marked
  the same revision merged, and, when a merge turn carried the observation, that turn landed the same head on the same branch. A landing does not make a result current: as on an artifact edge, the acceptance
  must also be whole and digest to its id, belong to an execution of the node, stand on the plan's and the relationship's criteria now, and rest on active acceptances. The head's own currency is not asked, because what landed is in the target.
* `decision`: an active, approved decision of the subject with the plan's digest by an authority kind the edge names, or a settled supervisor
  directive with that digest. Authority text is opaque: it is compared, never read.
* A predecessor the plan **cancelled or archived** satisfies no artifact or decision edge and an integrated edge only when its landing is observed (the observation is refused for a cancelled node, so
  there it has to have been observed before the cancel; an archived node's landing can still be observed): the edge reads
  `blocked:predecessor_cancelled` or `blocked:predecessor_archived`, as it does for a predecessor whose relationship was cancelled. The same follows what an accepted predecessor consumed
  ([Pause, resume, cancel and archive](#pause-resume-cancel-and-archive)).

### Invalidation

A plan revision can change what an accepted node was built from. The reading judges that every time it is computed and never stores it: staleness is derived from the plan, the acceptances and the manifests, like every other node state, so it clears by itself when the cause is repaired. Two readings of one store state are equal byte for byte, as every reading is (no clock is read; the rebuild below takes the author, the time, the base, the volatile snapshots, the rule version and the size of an artifact from the manifest the node consumed, and hashes no file).

* **Seeds.** An accepted node that has not landed is a seed when what it consumed is no longer what the plan and the store ask for:
  * its slice digest ([DAG plans](dag-plans.md#digests): its spec, its title included, and its incoming edges) is not the one recorded by the manifest its acceptance consumed: a node edited, an edge added into it, an edge retired from it. The cause is read from the consumed inputs' edge ids against the current incoming edges (`edge_added`, `edge_retired`, else `slice_changed`);
  * the criteria its output stands on are not the plan's: the effective criteria of its acceptance, the latest re-validation or else the criteria it was accepted with, differ from the plan's (`criteria_changed`, contract 3.2, E-11). This is asked whether or not the slice moved, so a plan that went back to the criteria the acceptance was made with, after the output had been re-verified against others, makes the node a seed again: its slice is the consumed one and its output is not verified against the plan's criteria. Re-verifying the same output against the plan's criteria resolves it with no new generation (E-21), so the mark goes away. A change of the criteria together with another change of the spec reads as the slice change;
  * an acceptance it consumed is no longer the active acceptance of its node, a predecessor accepted again (`input_changed`, contract 3.1, E-25). This is what keeps a node stale after the node above it is repaired, until it is accepted again itself.

  A node that landed is never stale (E-20): its result is in the target branch, so a revision above it makes a new node, not a rerun.
* **Descendants.** Only the seeds and the nodes below them in the current graph are judged; a node outside that closure reads as before.
* **Judgement by value.** A seed is stale without a rebuild: for a changed slice its digest is part of its manifest, so the manifest digests cannot be equal, and a criteria or consumed-acceptance seed is stale by the facts named above. A descendant is rebuilt with `BuildManifest`, as it consumed, and its manifest digest is compared with the one its acceptance consumed. Equal means current, however much changed above it. A difference counts when an incoming edge, in id order, explains it: an `artifact_verified` or `integrated` predecessor whose accepted result is stale (asked directly, whatever other reason the edge shows: a change of slice and criteria together is not hidden by `blocked:stale_criteria`), or a satisfied edge whose consumed value differs: the acceptance, the head an integration landed, the decision (id, digest and revision, as the manifest records them). An integrated landing is unchanged when an observation of the current containment run names the landed commit the node recorded (the same acceptance, repository, base branch and head, positive and not reverted, with a landed carrier): the tip an observation read is not a value the node consumed. A difference that has no such edge (a moved head or a cancelled predecessor, other readings of the edges) is not a stale result, so the node stays accepted.
* **The edge gate.** An `artifact_verified` edge (a code pin included) from a stale predecessor is not satisfied: it reads `blocked:stale_predecessor` and its detail names the edge, the predecessor and why it is stale, so no node is released onto it (contract 8.2, E-25). It is the last check of the edge, after the criteria, head and consumed-input checks, so those reasons are the ones shown when several hold; the judgement itself does not depend on them. An `integrated` edge is gated the same way once a landing would satisfy it: a stale result that landed in some of its targets and not in all (one that landed in every target is never stale) hands nothing over, wherever its head landed, and an edge whose target has not landed keeps reading `wait:edge:<edge_id>`. A node that was accepted on such a landing is stale with it (`stale:edge:<edge_id>`, cause `predecessor_stale`), so its own successors and its pull request are held back too, until the predecessor has landed everywhere or the node is accepted again. `decision` edges are not gated: what they hand over is a recorded decision, not a result under review.
* **The merge lane.** A stale result never merges (contract 8.2): `dag-merge-judge` and `dag-merge-request` refuse a stale accepted pull request with `disposition_conflict`, naming the stale reason, and write no judgement row and no merge turn for a result that is already stale when the call is made. A revision that lands after a judgement has recorded an `eligible` row leaves that row in the history and creates no turn (see [Merge eligibility](#merge-eligibility)). No outcome and no refusal reason is added.
* **Containment does not follow dev.** The `integrated` predicate asks whether the accepted head is contained in the target, and ancestry is monotone, so a merge that moves dev changes no answer. The observation that satisfies the edge is the earliest positive one of the current containment run (the first after the last negative observation), so the landed commit that consumers' manifests name, and the time the edge became satisfied, stay as they were when the integration is observed again at a newer tip (contract E-27). A manifest recorded under the earlier choice of the newest observation is still read as unchanged for the same run.

A stale node reads state `stale`, disposition `stale`, reason `stale:slice_changed`, `stale:criteria_changed` or `stale:edge:<edge_id>` (when the reason rests on an incoming edge: an edge added or retired, a stale predecessor, a consumed value that changed), and a `stale` object beside the detail, printed for stale nodes only:

| Field | Meaning |
| --- | --- |
| `cause` | `slice_changed`, `criteria_changed`, `edge_added`, `edge_retired`, `predecessor_stale` (the predecessor over `edge_id` is stale) or `input_changed` (the value that satisfies `edge_id` is not the consumed one) |
| `seed_node_id` | the node the staleness stems from (the node itself when its own slice changed) |
| `edge_id`, `predecessor_node_id` | the incoming edge the reason rests on and the node on its other end (null when only the node's own slice or criteria changed) |
| `consumed_acceptance_id`, `current_acceptance_id` | the predecessor version consumed over the edge and the acceptance that satisfies the edge now |
| `consumed_decision`, `current_decision` | over a decision edge, `<decision id>@<revision>` consumed and now |
| `consumed_slice_digest`, `current_slice_digest` | the slice digest the consumed version rested on and the plan's now (the node's own, or the predecessor's for `predecessor_stale`) |
| `consumed_manifest_digest`, `rebuilt_manifest_digest` | the manifest the acceptance consumed and the one built now (null for a seed, which is not rebuilt) |
| `action`, `action_detail` | what is done about the stale result and the sentence that says why: `revalidate`, `correct`, `hold` or `redefine` ([Handling a stale node](#handling-a-stale-node)) |

The detail also carries the edge, the predecessor and the short acceptance ids, so the pass records and the reading's input digest hold them. A stale implementation node keeps holding its edit regions until its head lands. The relationship's own state comes first in the reading (a paused node reads paused, an evicted one `blocked:evicted`), while the gate on the edges built on the node follows the judgement.

What it does not do: the judgement marks and nothing else; what is owed to a marked node is [Handling a stale node](#handling-a-stale-node). It does not adopt a restarted run, pause or cancel, or answer a progress query. A node whose consumed manifest cannot be read is not judged (the edges out of it report `blocked:manifest_tampered`, or `blocked:acceptance_incomplete` when the manifest is not stored). A criteria re-registration without a plan revision is not a slice change here. The function that later issues ask is `staleOf`. Behind a landed node a mix of two acceptances of one predecessor is not a stale result, and the closure of B-14 (`blocked:inconsistent_inputs`) is what finds it.

### Handling a stale node

The reading says why a node is stale and, in the `action` of the stale object, what is owed to it. The route is derived with the rest of the reading from the stale reading and the relationship the node stands on (contract 3.2, 5 and 8.4); it is never stored and reading it writes nothing.

| `action` | When | What is done | What is never done |
| --- | --- | --- | --- |
| `revalidate` | only the node's criteria changed (cause `criteria_changed`) and everything it consumed is still current: no acceptance it consumed was replaced and the manifest rebuilt as it consumed it is complete and digests to the same name | the same output is ruled again under the plan's criteria and `dag-accept` records a revalidation of the same acceptance (E-11, E-21): the criteria registered for the relationship change, the parent rules the output `verified` under the new set, and accepts it again | a new generation or a new child |
| `correct` | everything else on a node whose relationship is active: its spec, an edge into it or an input it consumed changed, or a change of criteria that also moved an input | the output is reworked by the same child, as the next generation of the same relationship: `dag-correct --prepare`, then either the `needs_changes` ruling that carries its instruction or a generation opened by hand ([below](#two-ways-to-open-the-generation)), `dag-correct`, then `dag-accept --supersedes` for the reworked result | a child made again |
| `hold` | the node rests on a stale predecessor (whatever its own cause: cause `predecessor_stale`, or a replaced input, or a change of criteria on a node that does), or an input is not there (an incoming edge that is not satisfied now: a predecessor nobody has accepted yet, a decision that was rejected), or the plan holds the node (paused, or the whole plan paused), or its relationship is paused, or a correction generation of it is already open | nothing on this node: the detail names what to wait for | |
| `redefine` | the relationship has ended (archived, superseded or cancelled), so there is no child to send a correction to | the parent decides whether to redefine the node: a new relationship registered with `supersedes` (contract 5, ID-4), which makes a new child | |

Decision D-08 (how far a redefinition reuses the same child) is read from contract 5 and nothing wider: a **correction** is a new generation of the same relationship, the same child and the same node with a manifest rebuilt as the plan holds the node now; a **node redefinition** is a new relationship with `supersedes`, which is a new child. The scheduler routes between the two and refuses what the contract forbids; it never registers the new relationship.

Two refusals keep the routes apart. Both are `disposition_conflict` with the reason in the detail, so no refusal reason is added, and neither writes a row.

* `dag-accept` does not record a revalidation for a node that is stale for any reason but its criteria, or whose criteria and an input both changed: ruling the old output again resolves nothing, and the refusal names the route. A node that is not stale can still be revalidated, a node that landed included: a landing does not make a result current, and an outgoing `integrated` edge reads `blocked:stale_criteria` until the same output is ruled under the plan's criteria ([Edge satisfaction](#edge-satisfaction)).
* `dag-correct`, in both its steps, refuses a node whose accepted head landed in every target, whatever kind a later revision gives the node (a node that landed reads `done:integrated` whatever its kind). A node that landed is never run again (E-20): it is never stale, `dag-release` of it replays the release it recorded (or is refused when it recorded none) and makes no child, no correction is prepared for it, and a generation the relay opened for it is not recorded as its own. What changed above it is carried by a successor node of a new plan revision, which is released like any other node: its edges name what it consumes, and one built on a stale result reads `blocked:stale_predecessor` until that result is current.

What a node's siblings keep is whatever this page never touches. A revalidation writes one row of `dag_acceptance_revalidations` and a correction one row of `dag_node_executions`, both for the node asked about; the acceptances, executions and slot tenures of the nodes that are not stale, and of the nodes that are stale for another reason, stay as they were. This build has no separate ledger of the cost of a node's work: those records are what it keeps.

#### Two ways to open the generation

The correction goes to the same child as the next generation of the same relationship, and the generation is opened in one of two ways. `dag-correct --prepare` prints the instruction line, the manifest digest and the dispatch request id for either.

* **A ruling.** The relay's verdict writer takes another ruling on an event it already ruled `verified` only while a review is open, which the relay opens when the criteria registered for the relationship are no longer the set the head was ruled under (`delivery/ack.go`, re-review; the accepted event must still be the head of the current generation). The parent rules `needs_changes` with the instruction as the restoration finding, the writer opens the generation and sends the instruction, and `dag-correct` binds the manifest the finding names. A plan revision that changes the node's criteria digest, with the new set registered for the relationship, opens that review.
* **A generation opened by hand.** When the head is ruled `verified` under the criteria registered now, which is the case of a node made stale because an input it consumed was replaced and nothing else, the writer answers a replay and opens no generation. The relay has a route for that, written in the coordinator's skill (`crw-run`, "A fresh execution generation"): the coordinator opens the generation with `generation-open --reason needs_changes_revision` under the dispatch request id `dag-correct --prepare` printed, sends the instruction line to the child in the message that dispatches the re-review, and binds that turn with `generation-bind`. Then `dag-correct --manifest-digest D` binds the generation to the manifest. The relay's verdict writer is not changed.

What the second way binds is bounded, so it cannot make a rerun or bind a manifest other than the one the generation was opened for: the node's accepted result must be stale (a result that is current is corrected by a ruling; a node that landed was refused before); the manifest must be named with `--manifest-digest`, be the one stored for this node at its current slice and criteria, and still rest on the inputs the node's edges are satisfied by now (it is judged against the store again, the file bytes aside, as release does when it records its own intent: a predecessor accepted again after the prepare leaves another input than the one the child was told to consume, and nothing is bound); the generation must have been opened under the dispatch request id derived from that manifest (`CorrectionRequestID`: the plan, the node, the manifest and the generation), so it was opened for this manifest and no other; it must be bound to a dispatch turn, because a child reports in a bound generation only; and it must be the generation right after the one the accepted result stands on, so a correction that is already open is not skipped by opening another beside it (opening one moved the relationship past the generation that correction is accepted on: the refusal is reported and no further generation is opened, as when the plan moved the node). Each refusal is `disposition_conflict` with its reason in the detail and writes nothing. What is recorded of how the instruction reached the child is that request id (in `managed_request_id` of the execution row, which a ruling leaves empty) and the turn the generation is bound to; the answer says `opened_by` (`ruling` or `generation_open`), `dispatch_request_id` and `dispatch_turn_id`, and a repeated `dag-correct` answers the same as a replay. The generation is not recorded for a node whose route, without that generation, is anything but a correction: a change of the criteria alone is ruled again and opens no generation, and what rests on a stale predecessor or on an input that is not there waits.

What the second way does not prove: the relay does not read the child's thread, so the request id and the bound turn are the coordinator's statement of how the instruction was delivered, where a ruling's restoration note is compared with the instruction line. The child checks the manifest file against the digest and sha256 the line names and answers `blocked_needs_input` on a mismatch. The child's first receipt in the generation carries no `--supersedes-revision` (the generation has no earlier revision to supersede). A generation opened by hand is also not recoverable by dag-correct if the plan moved the node between the opening and the recording: the recording is refused (the manifest was prepared for another version of the node), preparing again names the next generation, and recording that one is refused at the gap (`generation 3 of R follows generation 2, and the last recorded execution of N is 1`). Open no further generation: report the refusal and stop, since the same holds for a ruling that opened its generation before the plan moved.

### Input checks

When all incoming edges of a candidate are satisfied, the artifacts it would consume are read again. These file checks apply to non-code `artifact_verified` inputs (a code-pinned input consumes the accepted head instead), and a frozen copy of the manifest is checked when the receipt records its reference: every file the predecessor's receipt declared must still
exist, hash to the declared digest and size and lie under the predecessor's artifact roots; a frozen copy of the manifest must read and agree; an artifact
edge that hands over no artifact is blocked, never an empty success; the list of artifacts a receipt declares must hash to the revision the parent accepted (so an artifact left out of the list, or added to it, is found even when every remaining file is intact); and following the consumed acceptances down through their manifests, no node may appear
under two acceptances. The first violated path decides the reason.

### Ranking and selection

Candidates are ordered by the hop count of the longest chain each starts (critical path), then by the nodes below it, then by the stored time it became ready,
then by node id. There is no wave barrier: a long independent node does not hold back a short dependent chain, because a node waits for its own predecessors. Capacity and edit-region conflicts may still defer a candidate. The first candidates
that fit the free slots and do not overlap are ready; each candidate cut is deferred with `defer:no_capacity`, `defer:capacity_unmeasured` or `defer:edit_overlap`, and the pass records which limit decided
the first cut: `none`, `no_capacity`, `edit_overlap` or `capacity_unmeasured`.

### Capacity

Free slots are the smallest headroom over the initiative above the project, the project and the store, counting held execution slots against each `runs` ceiling
the way `slot-reserve` does (a ceiling of 0.5 still allows one reservation). The DAG never runs more children than the standing cap of 6 unless a cap basis for that
limit revision is recorded in `dag_cap_basis` (decision D-05 is not made, so no value above 6 is assumed). An enforced ceiling on another dimension without a usage
observation leaves no slot free.

### Edit regions

`crw relay dag-region-declare --plan P --node N --actor A --regions @file` declares the places an implementation node will edit, before it is released: 1 to 64 regions of
a repository, path, kind (`tree`, `file`, `symbol` with its symbol) and change (`edit`, `rename`, `delete`). A repository is a forge `owner/name` or an existing local
checkout (the directory its absolute path reaches, links resolved); whitespace inside a path component is kept and the path is cleaned before it is stored, while surrounding whitespace and control characters are refused. A rename, a delete and the
hotspots (lockfiles, anything under `.github/`, a Makefile or Dockerfile, `.sql` files, a `schema` or `migrations` directory) are exclusive: they conflict with any other
region of the repository. A node with no declaration counts as overlapping every other node: it waits while any node holds regions, and every declared node waits while one
undeclared node holds. Regions are held from release until the head lands, so a declaration cannot change while the node holds them. A forge slug and a checkout path of one
repository are two keys; a plan names one spelling per repository.

### Pass records

`crw relay dag-ready --plan P --record --actor A` keeps the reading as a row of `dag_passes`: the ready node ids in order, the free slots, the ceiling, the held slots, the
deciding limit and the node dispositions. A pass is a fact and never a decision; a duplicate wake records another pass.

## Releasing a node

`crw relay dag-release --plan P --node N --actor A --request @file --marker-root DIR` (with the explicit `--state` and `--socket` a managed start needs) gives a ready node to a Codex child through the managed-start
engine. The actor is the project's registered parent: it owns the slot and the child. The request document, `dag-release-request/1`, carries what the plan and the store do not hold: for an
implementation node the base (repository and ref; the commit is read from the target, never given), the rule version the node is dispatched under (skills digest, model, effort, prompt template, relay build:
every field required), volatile snapshots (see below), the instructions, the criteria, the criteria source and scope reference, the artifact roots, the allowed recipients, and the parent's and the child's host and
settings.

The release is idempotent and never creates a second child. In order:

1. **Replay first.** An already-bound release returns its recorded execution without repeating the continuation checks below; they apply only to an unbound intent. An intent already recorded for the node is continued from the exact request bytes frozen with it (`dag_release_requests`) and never rebuilt: the managed engine fingerprints the whole
   request, while the manifest digest leaves out fields that are still in the prompt, so a clock or a base tip that moved would read as a second release. A replay of an intent whose slot was returned meanwhile reserves it again under the same ceilings, and is refused `capacity_exhausted` when none is free. A replay presented with another spelling of the marker
   root, the socket or the state selector than the intent was frozen under is refused (`disposition_conflict`) before the engine is asked; a stored manifest or request that no longer digests to what was recorded is
   `revision_mismatch`.
2. **Judge, without a transaction.** The reading says the node is ready (or deferred only for capacity: the ceiling is enforced in step 4, where the contest is recorded). For every pinned predecessor the relay reads
   the pull request itself from the forge, by the owner/name it recorded at acceptance, and refuses when its head is no longer the accepted one after recording a `stale_head` observation (E-10); an unreadable or
   incomplete read is the host's failure, nothing is written, and a retry is allowed; a closed or merged pull request is fine, only the head is compared. The request's criteria must digest to the plan's. The manifest is
   built and verified, every artifact and volatile snapshot hashed again.
3. **Assemble the request.** Its id is derived from the plan, the node and the manifest digest only (`dag-` and 40 hex characters; node ids are plan-local and request ids are global to the store): no attempt counter, so a replay is the same request. The prompt carries the manifest inline,
   or the path and hash of its frozen copy (`<first artifact root>/dag-input-manifests/<sha256 of the bytes>.json`, see *The frozen copy* below) when it exceeds 60000 UTF-8 bytes, and tells the child, in English, to
   verify every uri, hash and size before consuming an input and to report `blocked_needs_input` on any mismatch.
4. **Write the intent.** The acceptance whose pull request was read in step 2 must be the one the manifest consumes (a predecessor accepted again meanwhile is a head that was never checked: `disposition_conflict`, repeat the release). One transaction judges the store half again (a plan revision, an acceptance or a registration may have moved), checks the slice, the criteria and the incoming edges against the manifest,
   decides capacity, reserves a slot of subject kind `dag_node` and key `plan/node` (a character neither id may contain) (decision D-15) and writes the manifest, the frozen request and the release together, or nothing. When no slot is free the
   contest is recorded and `capacity_exhausted` answered; a refusal of the reservation itself (another parent, a ceiling at initiative or store scope) leaves its conflict row and no intent, and any other failure of the store rolls the whole transaction back.
5. **Start the child**, outside any transaction, with the frozen bytes. A refused or incomplete start (a creation whose outcome is unknown, settings that differ from what was asked) leaves the intent and the slot; the
   same call again reaches this step again and the engine reconciles with the creation it already made. A request another caller is advancing waits for that caller to bind its child.
6. **Bind** the child to the node (`dag_node_executions`, kind `initial`). The engine already refuses a creation whose model, effort, sandbox or approval policy differ from the request; the bind refuses a
   relationship that is not this node's issue and parent.

**What a replay does not repeat.** A replay continues a frozen intent; it is not a new release. It repeats the checks that decide whether the intent may continue (a tombstoned managed start, the slot's parent and
project, a returned slot reserved again under the same ceilings, the frozen request's and the stored manifest's digests, the selectors) and sends the same bytes. It does not re-run the reading, the freshness of the
pinned pull requests, the plan's slice, the base tip or the hashing of the input files: the intent's manifest is what it is, a change that invalidates it is the business of invalidation and adoption (a later issue), and
the child verifies every input against the manifest before consuming it. A start that was released after its slot was reserved leaves the slot held and the node `blocked:release_abandoned`, and a replay refuses it (`disposition_conflict`): the engine refuses a released request id for good. Returning the slot frees capacity but does not revive the request, and changing the node's slice does not replace its recorded intent. The way on is [Recovering an abandoned release](#recovering-an-abandoned-release).

The slot is held from step 4 until the parent releases it with the acceptance of a non-PR node or the integration of an implementation node, or an operator does.

### Recovering an abandoned release

A managed start that was released before it created a child (`managed-release`, possible only before the request is armed) is a tombstone: the engine refuses its request id for good, and a release's request id is derived from the plan, the node and the manifest digest only, so the same manifest can never start under it again. The node reads `blocked:release_abandoned` (its detail names the request id), its slot stays held, and a replay of `dag-release` refuses. Two steps end that.

1. `crw relay dag-release-close --plan P --node N --actor A --manifest D --request-id Q --reason R` (every option is required). The actor is a registered parent of the plan's project and the node is live. Q must be the node's open request, released and with nothing bound to the node, under manifest D. One transaction records the closure (a `closed` row of `dag_release_recoveries` with the slot and the frozen copy it dealt with) and returns the slot with the reason `dag_release_closed`: a slot an operator already returned is skipped, and one held by another parent or project is `disposition_conflict`. A running release, one whose start is still in flight, another manifest and an unknown request are `disposition_conflict`, an actor that is not the parent is `scope_role_mismatch` and a missing reason or request is `malformed_receipt`. The command starts nothing and creates no child. The same request again answers the recorded closure with `replayed: true` (and `successor_request_id` when the node was released again since), so a retry of a close whose answer was lost never closes a later abandoned intent of the same manifest: that one needs its own request id. After the commit, in a transaction of its own, the copy the closed intent's frozen request names is removed unless something live names it (see [The frozen copy](#the-frozen-copy)); `frozen_copy.removed` says whether the file is gone, and a repeat of the close tries again if it is not.
2. The ordinary `dag-release`. A closed intent no longer owns the node, so the reading says it is ready again, and the release judges it from scratch (steps 2 to 4 above). A manifest that changed since (a moved base tip, a predecessor accepted again) is an ordinary new release under its own digest and request id. The same manifest cannot take its old request id, so it takes `RecoveryRequestID`: `dag-` and 40 hex characters of the hash of the canonical list [`recover`, plan, node, digest, the closed request]. It is recorded as a `rereleased` row of `dag_release_recoveries` that holds the successor id and the exact request bytes and selectors, as `dag_release_requests` does for the first release (the shipped release rows are unique on the digest and cannot take a second request). A release after a close of an older manifest of the node is the successor of that manifest's last closed request.

The node's open intent is the `dag_releases` row or `rereleased` row whose request has no `closed` row; there is at most one, and the intent transaction checks that again before it writes, so two calls that release a closed node at once create one child (the loser replays the winner's intent). A replay of a successor sends the successor's own frozen bytes and checks them and their selectors as a first release's replay does. A live child still stops a release: the judgement refuses `duplicate_assignment` while a relationship or a managed start owns the issue. A foreign child that takes the issue after the intent was recorded makes the engine refuse the start without writing a managed row; that is an intent waiting for the issue (it keeps its slot, decision D-15) and not an abandoned one, and the same `dag-release` continues it once the issue is free. A store whose zone predates `dag_release_recoveries` reads as before (a read-only open creates nothing).

Not covered: the intent of a node that a plan revision retired (`dag-release-close` needs a live node; invalidation and adoption own it), and any judgement that a manifest is stale (it is made when the node is released again, from scratch).

### Input manifest

The manifest (`dag-input-manifest/1`, contract 4.2) is content-addressed and records, for every incoming edge, the value that satisfied it, read from the very rows the edge's predicate used (the observation that proved an integration, the decision that settled a decision edge): the acceptance and its revision, the pinned head, the artifacts with
uri, sha256, size and the artifact root they lie under, the landed commit, or the recorded decision; the node's slice and criteria digests; the base; the rule version; the volatile snapshots. A manifest is released
only when it is whole: a missing, mismatched or altered input, an unreadable store, or an empty rule-version field is never an empty success. The violated paths of contract 4.4 map onto the relay's existing refusal
reasons:

| Path | Reason in the reading | Refusal |
| --- | --- | --- |
| B-01, B-16 | `blocked:manifest_incomplete` | `malformed_receipt` |
| B-02, B-07 | `blocked:manifest_tampered`, `blocked:acceptance_tampered` | `revision_mismatch` |
| B-03, B-04, B-17 | `blocked:input_missing`, `blocked:input_hash_mismatch` | `manifest_unverified` |
| B-05 | `blocked:input_out_of_scope` | `scope_escape` |
| B-09 | `blocked:stale_head` | `merge_candidate_moved` |
| B-10 | `blocked:stale_criteria` | `criteria_set_changed` |
| B-13 | `blocked:evidence_mismatch` | `revision_mismatch` |
| B-06, B-08, B-11, B-12, B-14, B-15 and every other reason | as named | `disposition_conflict` |

A node that is owned already is `duplicate_assignment`, an unmeasured ceiling `capacity_unmeasured`, overlapping regions `region_overlap`. The closed reason travels in the refusal's detail.

A volatile snapshot is a file the node reads that can change (a Linear document, an issue) captured before dispatch: it must be an absolute path under one of the child's artifact roots (B-05), exist (B-03) and hash to
the digest the manifest names (B-04). It is part of the manifest digest, so a later edit is a different manifest.

## Accepting a result

`crw relay dag-accept --plan P --node N --actor A --rule-version @file [--repository OWNER/NAME --pull-request N] [--event E] [--supersedes ID]` records that the parent accepted a node's result (contract 4.3).
The child's end of task, its report, the transport acknowledgement and the parent's acceptance are four records, and only the last one opens an edge. The acceptance is written when P-AV-1 holds **now**, read from the rows in one transaction:

1. the result is the one head of its generation: a final, unsuppressed `ready_for_review` event (the `--event` the parent names must be that head; an older event is `superseded_revision`, an event of another generation `stale_generation`);
2. its acknowledgement was accepted, verified by the host, and rests on evidence above the `unverified` tier;
3. the parent's ruling on it is `verified`, still current, and made under managed verification;
4. the criteria registered for the relationship are the ones the ruling was made against, and the plan's criteria for the node are the same set;
5. the relationship is active, and the caller is its parent.

For an implementation node the head is never given: the relay reads the named pull request from the forge itself and accepts only an open, non-draft pull request whose snapshot is consistent. The accepted head, the pull request and the forge identity are recorded
(P-AV-2); a statement in a child's report never becomes the head. A plan revision that changes the node while the pull request is being read refuses the acceptance, because the verification no longer describes the plan.

The same output accepted again is a replay when it names the pull request recorded with the acceptance and the forge still shows the accepted head there (a head that moved is `merge_candidate_moved`, another pull request `disposition_conflict`; a pull request merged at the accepted head is still the same output). The same output ruled again under re-registered criteria is a **revalidation** of the same acceptance (`dag_acceptance_revalidations`, contract E-11): no second acceptance, generation or child. It is recorded only when the criteria are all that is stale ([Handling a stale node](#handling-a-stale-node)). A new output replaces a node's active
acceptance only with `--supersedes <that acceptance>`; without it the call is `disposition_conflict`. The acceptance digest names the plan, the node, the manifest, the execution (relationship and generation), the event and revision, the criteria, the verdict and, for an implementation node, the head, the repository and the pull request number, so a changed row reads as `blocked:acceptance_tampered`. The evidence digest, the rule version, the acknowledgement tier and the times are recorded and left out of the identity, so a re-run of a check never changes what was accepted.
The rule version (skills digest, model, effort) is recorded with the acceptance and left out of its identity. A non-PR node's slot is returned by its acceptance; an implementation node's slot is held until it is integrated. A parent that refreshes a branch whose base moved (crw-run's merge readiness) therefore does it before this acceptance: the head read here is the head that has to land, a pull request that moves afterwards reads `stale_head`, and the same output cannot be accepted again at the new head, so a base that moves after the acceptance goes back to the child for a new generation.

## Integration

`crw relay dag-integration-observe --plan P --node N --actor A [--target REPOSITORY@REF ...]` records whether the accepted head is contained in the branches it has to land on, as a fact the relay reads: `git merge-base --is-ancestor` against a local checkout, the
compare API (`behind_by` 0) for a forge repository. The parent's order is accept, merge, `assignment-mark merged`, observe. The required targets are those of the node's outgoing `integrated` and code-pinned edges, every target the acceptance already has an
observation for (a negative one included, so observing a subset never completes a node) and the ones observed now; a terminal node names its target with `--target`. A node is **integrated** when every required target contains the head and the `merged` mark exists on
the same revision of the same generation. The observation alone, or the mark alone, integrates nothing. A squashed or rebased landing leaves a head that is not an ancestor: when a merge turn recorded that it landed this head on the branch, the reading says `blocked:integration_unprovable` rather than guessing; without such a turn the node simply keeps waiting.
Observations are append-only: an identical reading (same head, same tip, same answer) is a replay, and any change is the next observation. A paused or cancelled relationship records none (contract 3.2); observations already written stay. The targets are read from the plan as it stands when the observation is written, and the node's newest slot tenure is returned, by name, when it integrates.

## Decisions

`crw relay dag-decision-record --plan P --actor A --subject S --digest D --disposition approved|rejected --authority-kind K --authority-ref R` writes the decision a `decision` edge waits for. Only the project's one registered parent records. Recording is not authority: the
edge compares the authority kind with the kinds it names and reads the digest, so a decision by another authority or about another digest records and opens nothing. The text of the authority is opaque (D-09). The same decision again is a replay; any other decision of the subject
supersedes the active one and keeps the history, so a rejection after an approval is a fact.

## Corrections

A correction goes back to the same child (contract 3.2): a `needs_changes` ruling opens the next generation of the same relationship and the existing verdict writer sends the correction message in the same transaction, so a manifest cannot be placed in that
generation afterwards. The protocol therefore puts the manifest into the one text the relay itself sends. `dag-correct --prepare --manifest-request @file` rebuilds and verifies the node's input manifest as the store holds it now, stores it, and prints an English
instruction line that names the manifest, a copy of its canonical bytes kept under the child's first artifact root (the file the child reads, since it cannot read the relay's store; see *The frozen copy*) and the sha256 of that file; the parent submits that line as the **restoration** finding of its `needs_changes` ruling. (When no ruling can open the generation, the coordinator opens it by hand under the dispatch request id the same step prints and carries the line in its own dispatching message: [Two ways to open the generation](#two-ways-to-open-the-generation).) After the ruling, `dag-correct` (without `--prepare`) binds the generation the ruling opened to the manifest that finding names
(`dag_node_executions`, kind `correction`) once the manifest is stored for this node at its current slice and criteria and the restoration note carries, word for word, the instruction that manifest was prepared with (this generation, the path of the copy and its hash). No file is read when the generation is recorded: the child checks the copy against the hash in the line it was sent. The bound digest is derived from what the child was told: a digest in an ordinary finding does not bind, a restoration block that names two different manifests is refused, a ruling without the block leaves the previous manifest in force (`carried_over`), and a `--manifest-digest` that differs
is refused, also when the generation is already bound. A node whose accepted head landed in every target is refused in both steps ([Handling a stale node](#handling-a-stale-node)). The previous acceptance stays active and reads `blocked:stale_head` until the parent accepts the corrected result with `--supersedes`; what consumed the old acceptance then reads stale ([Invalidation](#invalidation)).

### The frozen copy

A manifest that a child has to read from a file (an oversize release prompt, or a correction) is kept as `<first artifact root>/dag-input-manifests/<sha256 of its canonical bytes>.json`. The name is the hash of the
bytes, not the manifest digest: two bodies of one manifest differ in what the digest leaves out (the time of the build, the rule version), so they are two files. An existing regular file of that name is reused only after the authorized, byte-for-byte check below. A newly created file is created exclusively with mode 0600, in a directory that must be a directory of its own (a link is refused), and is read back through the relay's authorized
open (inside the root, no link on the way, a regular file, never blocking on a pipe) and compared byte for byte. The child owns its artifact root, so a link or a pipe planted there makes the freeze fail and never makes it wait. A link planted between the check of the directory and the creation of the file, or an artifact
root that is itself a link, can leave one file outside the root (it holds only a manifest the relay wrote); the read-back then refuses and nothing is bound. An artifact root with a control character is refused when a correction is prepared,
because the relay's message joins the lines of a finding and the child would read another path. A correction uses the body the store holds for the digest, so preparing twice from the same inputs gives the same file and the same line. The directory is created by the freeze. A release that ends without recording an intent of its own takes back the copy it froze, and closing an abandoned release takes back its own:

* **Moment.** The end of a `dag-release` call that recorded no intent (a refusal after the manifest was frozen, such as `capacity_exhausted` or a later refusal of the request; a write of the new copy that failed part way, such as a full disk; a store failure inside the intent transaction; a race lost to a call that recorded the same release), and the settling step of `dag-release-close`. A crash between the freeze and the intent leaves the file, and nothing sweeps the directory.
* **Target.** In a release, only a file this call created (the exclusive create succeeded); a file that already existed was only reused. At close, the file named by the header line of the closed intent's own frozen request (the line the relay wrote at the head of the prompt, for the manifest being closed: the operator's instructions are never read for a path), and only when its bytes are that manifest. Always only `<root>/dag-input-manifests/<sha256>.json`, a regular file whose bytes hash to its name, with one exception for the file this call created: when its own write failed part way, the file holds a strict prefix of the bytes it meant to write (possibly none) and is taken back as well, because a prefix cannot be a manifest copy anything relies on. A file that holds other bytes stays.
* **Reliance, never removed.** A copy a live intent's frozen request names (a bound release, an accepted node's, a release still waiting for its start) or a stored manifest holds exactly these bytes (an accepted node's, a prepared or recorded correction's). At close the stored manifest counts only when its digest is bound to an execution, is accepted, or an open relationship owns the issue, because the closed intent's own stored body is what its copy holds. The check runs in the same write transaction as the unlink, so no intent can commit between them, and the intent transaction freezes its copy again before it records the intent, so a copy that another call took back is there when the intent names it.
* **Mechanism.** `store.UnlinkPinned` opens every component of the root and of the directory with `O_NOFOLLOW` from `/`, each relative to the one before, opens the file relative to the directory's descriptor (`O_NONBLOCK`, so a FIFO is refused and cannot hold the transaction), reads at most 16 MiB, asks the retention check, compares the name with the file it read (device and inode) and unlinks on the directory's descriptor: a link in any component, or a component swapped meanwhile, cannot lead it elsewhere. It needs Linux, as every artifact read does; elsewhere the copy is kept. A mount point on the path and a file replaced in place between the last check and the unlink are not stopped.
* **Evidence.** A journal row per decision: `dag_manifest_copy_removed` or `dag_manifest_copy_kept` with the path, the sha256, the byte count, the reason (`relied_on`, `reliance_unknown` (a stored manifest could not be read), `link_in_path`, `not_a_copy`, `bytes_differ`, `too_large`, `changed`, `remove_failed`, `name_differs`, `unsupported_platform`) and the cause (the refusal reason that ended the release, or `closed`); a removal states the cause as its reason, or `incomplete_write` for the partial file a failed write left (its byte count is that of the file removed). The unlink comes first and the row after it in the same transaction, so a failure of the commit after the unlink loses the row of a file nothing referenced, never a referenced file. The cleanup never replaces the call's own error.

## Merge eligibility

`crw relay dag-merge-judge --plan P --node N --actor A [--repository OWNER/NAME --pull-request N]` decides whether the accepted pull request of an implementation node may go to the merge lane (criterion c8). The relay reads the pull
request (by the forge identity recorded with the acceptance) and the base branch itself; then one transaction applies these rules in order, the first decisive one winning, and appends the judgement to the node's history in
`dag_merge_checks` (a judgement that restates the latest one writes nothing, so the history is readable afterwards: the retry, the stale head, the eviction).

| Outcome | When |
| --- | --- |
| `evicted` | a required check failed again on the same head after its retry (decision D-12). Final for that head of that pull request, whichever acceptance recorded it, under whichever spelling of the repository (the forge does not tell owner/name apart by case) and whichever pull request carried the commit: nothing later, green checks and a fresh acceptance of the same head included, brings it back (the acceptance in force is given the eviction in its own history); a new head needs a new acceptance and starts with its own retry. The reading says `blocked:evicted` |
| `stale_criteria` | the plan's or the relationship's criteria are no longer the ones the acceptance stands on |
| `stale_head` | the pull request is at another head than the accepted one (E-10); the row stores both heads, so the reading blocks the edges built on it |
| `predecessor_not_landed` | an incoming `integrated` edge, or an incoming code-pinned edge (a stacked pull request), whose predecessor's accepted head is not yet in the edge's target |
| `stale_base` | the base branch tip is not contained in the head, so the required checks did not run on the tree that would land. The dev ruleset is strict (D-11), so a head that contains the tip is that tree; the pull request's own base field says nothing about what the checks ran against and is stored for information only. The local ancestry check needs the head fetched into the checkout; a head it does not have is a failure of the host and writes nothing |
| `checks_pending` | a required name has no run on the exact head, or one of its runs has not finished, or the checks are not usable evidence |
| `retry_same_sha` | every required check has finished and one failed, and no retry was spent on this head: run the failed checks again on the same head (no new generation, no reassignment). The same snapshot read again is the same judgement |
| `eligible` | every required check passed. After a recorded failure the reason says so |

A stale accepted result is not judged at all: a stale result never merges (contract 8.2, [Invalidation](#invalidation)). The call is refused `disposition_conflict` naming the stale reason, ahead of every rule above (an evicted head included), and writes no row of the history. The judgement is asked again inside the transaction that writes, so a plan revision that lands between the read of the pull request and the write is seen; `dag-merge-request` asks it once more in the transaction that creates the turn (after the criteria and the predecessors, so a criteria change that lands in that gap is `criteria_set_changed` and an integrated or code-pinned edge added in it, whose predecessor has not landed, is the unmet predecessor), and a revision that lands after the judgement has recorded an `eligible` row leaves that row in the history and creates no turn. A result that is current again (the plan repaired, the node accepted again) is judged as before.

A required check is read the way merge-evidence and the merge lane's own check read it: of every run only its newest attempt counts, every run of a required name must be a `success` (a skipped or neutral run is not one, so a head the judgement lets in is not refused by the lane for the same check), a required (name, integration) pair needs its own run (a name that two integrations must answer needs both), and the opaque run identities are never ordered. Failures count only when every required check has finished, so one red job seen beside a running one never uses up the retry. A second, different failure after the retry round opened evicts. A failure is told from another by the run's identity, its attempt and the time the forge last changed it (a commit status has no attempt and a check run can be reset in place), and a failure of a run that a judgement saw not failing (running or passing) since it failed is a new failure whatever its identity; another check running again says nothing about this one. Everything is computed from the newest reading of each run (a listing that still carries a failed first attempt beside the passing second one judges as the second), a failure counts once a judgement recorded it (a red job beside a running one is not counted), and a reading of a run that is older than one already judged (an earlier attempt, or an earlier completion of the same attempt, arriving late) is not judged: it is `merge_candidate_moved` and the caller reads again. So is a reading of the pull request that was in flight while another judgement of the node was recorded: it is older than the history whatever it contains, and nothing is written. Times are compared as instants (the forge's spelling does not matter). A forge that returns an older state of a run after a newer one was judged cannot always be told from a run that was reset: a reading that goes backwards without a time and with no judgement between the two (a run read as running after its completion) can count as a rerun, so a head can be evicted one failure early. Two failures that carry the same identity and the same time and that no judgement saw apart read as one failure: the node is not eligible either way, but it can take more than one retry. A pull request whose list of
required checks cannot be read is not judged (`merge_evidence_malformed`, nothing written): ignorance is not "none required". A draft, closed or merged pull request is `disposition_conflict`; evidence the relay could not read
completely is the host's failure; both write nothing. A row of the history that no longer digests to what was recorded (B-13) is `revision_mismatch`, and the edges built on it read `blocked:evidence_mismatch`.

`crw relay dag-merge-request --plan P --node N --actor A --host H` judges again now (never from the history) and, only when the node is eligible, asks the existing merge lane for a turn with the accepted head, the pull request and the
relationship, in one transaction that reads again the acceptance, the relationship, the criteria, the predecessors that have to have landed, whether the result is stale, and the latest judgement (its head, and the evidence it recorded digested again): a pause or a change that lands between them creates no turn and no grant. A node that is not eligible gets no turn and the refusal is the relay's own reason (`merge_candidate_moved` for a stale head, `merge_currency_stale` for a stale base, `criteria_set_changed` for stale criteria,
`disposition_conflict` naming the outcome for the rest); the judgement stays in the history, which is what counts the retry. The lane is not changed: a holder has one live claim per target, so the parent merges one pull request of
a project at a time (a second request while the first turn is open, even for another node accepted at the same commit, is `disposition_conflict` naming that turn), and turns of different projects are served first in, first out (D-16, `requested_at` then turn id). The parent's order is
accept, `dag-merge-request`, acknowledge the grant, `merge-turn-check`, merge on the forge, `merge-turn-land`, `assignment-mark merged`, `dag-integration-observe`. A pull request that went through `dag-merge-request` is
the only kind the DAG path lets into the lane, so every landed tree of it has an `eligible` judgement of the very head that landed.

## Pause, resume, cancel and archive

The plan holds or ends a node by a revision ([DAG plans](dag-plans.md#pause-resume-cancel-and-archive)): `pause_node`, `resume_node`, `cancel_node`, `archive_node`, `pause_plan` and `resume_plan`.
The scheduler reads that state at the revision it reads the plan at and obeys it in the reading and in the commands that advance a node. It reaches nothing in the relay: no relationship is paused
or cancelled by a plan change, a child keeps running and reporting, and a slot stays held until an explicit `slot-release` (contract 7.4 and 3.2). What the plan stops is the scheduler releasing a node,
accepting its result, correcting it or sending it to the merge lane. A lifecycle change releases no slot: a slot a node holds stays held, and keeps counting against the ceilings, while the node is paused or
ended and while the plan is paused, until it is returned as it always is (an explicit `slot-release`, the acceptance of a non-PR node, or the integration of an implementation node, which an archived node can still reach).

**The reading.** The plan's hold is read before the edges, so a held node is never a candidate whatever its edges say. The reason is the first that applies of: the node is cancelled or archived, the
plan is paused, the node is paused. The state word is the node's own lifecycle when it has one and `planned` for a node of a paused plan:

| The plan says | A node nobody owns reads | A node with a release or an execution reads |
| --- | --- | --- |
| the node is paused | state `paused`, `defer:node_paused` | the state its execution is in, with `defer:node_paused` in place of `skip:already_owned` |
| the plan is paused | state `planned`, `defer:plan_paused` | the same, with `defer:plan_paused` |
| the node is cancelled | state `cancelled`, `skip:node_cancelled` | the state its execution is in, with `skip:node_cancelled` in place of `skip:already_owned` and `done:accepted` |
| the node is archived | state `archived`, `skip:node_archived` | the same, with `skip:node_archived` |

Three readings are kept whatever the plan says, because the plan cannot take them back or an operator has to act on them: `done:integrated` (a landing is a fact; a cancel does not revert it),
every `blocked:*` reason (a creation of unknown outcome included: its detail says that a repeat of the release is refused until the plan lets the node run) and, for a pause only, `done:accepted` (a pause does not
invalidate an acceptance). A node the plan paused, cancelled or archived carries `lifecycle` in its reading and a paused plan carries `plan_state` at the top; both keys are absent otherwise, and the pass
record keeps the same optional key. Edit regions follow the relationship and not the plan: a child that still runs after the plan cancelled its node still holds its regions.
A stale node the plan cancels or archives reads as ended (state `cancelled` or `archived`, no stale object) and its detail keeps what made it stale; a stale node the plan pauses keeps its stale reading.

**What is blocked downstream.** A node the plan cancelled or archived is never released again, and nothing is released from its result: its artifact and decision edges stay unsatisfied
(`blocked:predecessor_cancelled`, `blocked:predecessor_archived`) until the plan is revised, and its integrated edge only when the landing is observed (see the edge rules above). The same gate follows what an accepted
node consumed: following the manifest of its acceptance, and then the acceptances its inputs rest on, an input whose node the plan cancelled or archived blocks the edges out of it, so a node accepted on the
result of A is not a base for what follows it once the plan cancels A. An integrated input is not followed (what it handed over is in the target). This is a gate and not an invalidation: no acceptance
row changes, and whether an acceptance is stale is the invalidation reading's, above.

**The commands.** A node the plan paused, cancelled or archived, and every node of a paused plan, is refused the work: `dag-release` (a new release, and a frozen unbound intent that would create its child:
nothing is created and no slot is reserved), `dag-accept` and `dag-correct` (prepare and record; the manifest of a correction is stored in the transaction that asks again, and the file and the instruction
follow only after it committed). A frozen release is also refused when the plan ended a node whose result its frozen manifest consumed since the intent was written, by the same gate as a new release. The landing of an accepted pull request (`dag-merge-judge`, `dag-merge-request`, `dag-integration-observe`)
is refused for a paused or cancelled node and a paused plan, and not for an archived node, as it is not for an archived relationship. A refusal is `disposition_conflict` with the closed reason and the node in the
detail, writes nothing, and is made again inside each command's transaction, because a pause moves neither the slice digest nor the criteria digest those transactions compare. A release already bound to its
child answers as it always did. `dag-decision-record` and `dag-region-declare` record facts and are not refused, and neither is `dag-release-close`: it ends an abandoned intent and creates no
child, so the operator can still return the slot of an abandoned release of a node the plan holds. The release that follows the close is refused like any other while the node is held, and the successor
request a close leads to is a frozen release, which the check below stops until the plan lets the node run.

A release asks again at the last point the DAG can stop a child: just before the managed start, for a new release and a continuation alike, and a node that left the plan (retired or replaced) is refused there as
`unregistered_scope`; the refusal leaves the intent and the slot where they are, and the resume lets the same release go on. That last check is a read, so a pause that commits after it, including while the managed
start is running, cannot undo a creation already begun outside the store: the child is then bound as it always is, and the node reads as running under the plan's hold (`defer:node_paused`); pausing or cancelling its
relationship is the relay's own operation.

**What a pause leaves alone.** An acceptance recorded before the pause is a value the pause does not touch: it stays active, the edges it satisfied stay satisfied, and the node reads `done:accepted` with
`lifecycle` beside it. A result that a child reports after the pause is recorded by the relay as every report is, but it is not accepted while the pause lasts, so the edges it would satisfy stay open and no successor
is released from it; after the resume the parent accepts it and the successor becomes ready. The child itself is not stopped: pausing or cancelling the relationship is the relay's own operation and a plan change does not do it.

## Conflict observations

`crw relay dag-conflict-observe --plan P --actor A --repository PATH --left-node N --right-node M --left-head SHA --right-head SHA` records how many files git cannot merge between the heads of two parallel branches of a plan
(`git merge-tree --write-tree`, git 2.38 or newer; 2.40 or newer to read the attributes committed in the commits), the number criterion c7 asks to be recorded. The checkout is whatever git says it is (a working tree, a linked working tree or a bare repository, at any path). The merge is computed in memory in a throwaway bare repository that reads the checkout's objects through an alternates file and has nothing else of it: the checkout's configuration (a merge driver is a command it would run), its working tree and an uncommitted `.gitattributes` can neither run code nor change the answer (the attributes are the ones committed in the left head, read by git 2.40 and newer), and the checkout gains no objects; git's NUL separated answer is read for the file names, and an answer without the shape of a merge result is a failure and never a count of zero. A submodule changed on both sides counts as a conflict, because git decides it by looking inside the submodule and the throwaway repository has none: the count can be higher than the merge's, never lower. The nodes are stored in sorted
order with their heads, so asking either way round is one row, and the same two heads over the same base are a replay. It is a measurement: nothing in the reading waits for it.

## Cap basis

`crw relay dag-cap-basis-record --limit L --revision R --w-minutes W --w-source S --s-minutes S --s-source S --actor A` records the evidence a runs ceiling above the standing cap of 6 rests on. The reading honours such a
ceiling only for a limit revision that has a row here, and clamps it to 6 otherwise. The value of a ceiling is decision D-05, which is not made: this writes evidence and raises nothing. Only the task that declared the limit or the
project's registered parent records, and a basis is not rewritten.

## Commands

| Command | Reads or writes | Answer |
| --- | --- | --- |
| `dag-ready --plan P [--record --actor A]` | reads; writes one `dag_passes` row with `--record` | the reading: `plan_id`, `plan_revision`, `state_digest`, `input_digest`, `pass`, `ready`, `nodes` |
| `dag-region-declare --plan P --node N --actor A --regions R` | writes `dag_node_regions` | the declaration in force and whether it replayed |
| `dag-release --plan P --node N --actor A --request R --marker-root D` | writes the intent (`dag_releases` and `dag_release_requests`, or a `rereleased` row of `dag_release_recoveries` after a close of the same manifest; `dag_input_manifests`), reserves a slot, starts a managed task, binds it (`dag_node_executions`) | the release: manifest digest, request id, slot, relationship, generation and child; exit 2 with the managed engine's answer nested under `managed` when the start was refused or incomplete |
| `dag-release-close --plan P --node N --actor A --manifest D --request-id Q --reason R` | writes the closure (`dag_release_recoveries`) and returns the node's slot, in one transaction; then removes the closed intent's frozen copy and journals it | the closure: `request_id`, `slot_id`, `slot_released`, `replayed`, `successor_request_id`, `frozen_copy` (path and whether the file is gone) |
| `dag-accept --plan P --node N --actor A --rule-version R [--repository R --pull-request N --event E --supersedes ID]` | writes `dag_acceptances` (and `dag_acceptance_forge`, or a revalidation); returns a non-PR node's slot | the acceptance: `acceptance_id`, `replayed`, `revalidated`, `superseded_acceptance_id`, `head_sha`, `evidence_digest`, `slot_released` |
| `dag-integration-observe --plan P --node N --actor A [--target REPO@REF]` | appends `dag_integration_observations`; returns an implementation node's slot when it is integrated | the observations, `integrated`, `mark_present`, `slot_released` |
| `dag-decision-record --plan P --actor A --subject S --digest D --disposition X --authority-kind K --authority-ref R` | writes `dag_decisions` | the decision id and revision, `replayed`, the decision it superseded |
| `dag-correct --plan P --node N --actor A [--prepare --manifest-request R] [--manifest-digest D]` | `--prepare` stores a manifest; otherwise writes `dag_node_executions` (kind `correction`) | the instruction line, the manifest digest and the dispatch request id, or the generation bound, `carried_over`, `opened_by`, `dispatch_request_id` and `dispatch_turn_id` |
| `dag-merge-judge --plan P --node N --actor A [--repository OWNER/NAME --pull-request N]` | appends `dag_merge_checks` (when the judgement differs from the latest) | the outcome, reason, round, check sequence, heads, base tip and failed required checks |
| `dag-merge-request --plan P --node N --actor A --host H [--repository OWNER/NAME --pull-request N]` | the judgement as above; asks the merge lane for a turn (`merge_turns`) when eligible | the judgement and the turn |
| `dag-conflict-observe --plan P --actor A --repository PATH --left-node N --right-node M --left-head SHA --right-head SHA` | writes `dag_conflict_observations` | the number of conflicting files and their names |
| `dag-cap-basis-record --limit L --revision R --w-minutes W --w-source S --s-minutes S --s-source S --actor A` | writes `dag_cap_basis` | the basis recorded |
| `dag-summary-enqueue`, `dag-summary-status`, `dag-summary-claim`, `dag-summary-reconcile`, `dag-summary-complete`, `dag-summary-fail`, `dag-summary-retry` | the project summary queue: `dag_summary_outbox` (`status` and `reconcile` read only) | the entry, the queue, the claim and the confirmation ([DAG summary outbox](dag-outbox.md)) |
| `dag-progress --plan P` | reads only, and opens the store read-only | the progress of the plan: stage distribution, cumulative counts, denominator per revision, a reason per blocked or stale node, links ([DAG progress](dag-progress.md)) |

Refusals use the relay's existing reasons: `unregistered_scope` (a plan or node that is not there), `malformed_receipt` (a region or a request that is not valid),
`disposition_conflict` (a node that edits no repository, or whose regions are held, or that is not ready), and the reasons of the table above. `dag-release-close` refuses with `malformed_receipt`, `unregistered_scope`, `scope_role_mismatch` and `disposition_conflict`. Commands can also refuse with `merge_target_unreadable`, `not_acknowledged`, `relationship_conflict`, `relationship_not_active`, `revision_ambiguous`, `scope_role_mismatch`, `slot_unknown` and `unregistered_relationship`. Exit codes are the relay's: 0, 2 refusal, 3 host, 4 usage.

## The store

The scheduler adds tables to the DAG zone ([DAG plans](dag-plans.md#the-store)) as appended statements. `dag_passes` and `dag_node_regions` are described above; `dag_release_requests` freezes the request of a release with its intent; `dag_release_recoveries` holds the closure of an abandoned release and the successor release of a closed one (see Recovering an abandoned release);
`dag_conflict_observations` holds the merge-tree conflict counts of parallel branches; `dag_merge_checks`, `dag_acceptance_revalidations` and `dag_acceptance_forge` hold what the relay observed of an accepted pull request, re-verification of an accepted
output under new criteria, and the forge identity of an accepted implementation node. `dag_summary_outbox` is the queue of the project's Linear summaries (see [DAG summary outbox](dag-outbox.md)).

## How the acceptance path is verified

The tests drive the real Go store, the real managed engine and bridge adapter over a fake App Server, a real git repository, the relay's own verdict writer and renderer, and the merge lane's own check and landing.
Two things are scripted and say so: the forge (a pull request's head, base and checks are what the test scripts, since no forge is reachable), and the child's report. The rows of a verified report (the final event, its
lineage, the acknowledgement and its evidence, the ruling and its context, the registered criteria) are written by a test helper in the shape the intake, acknowledgement and verdict writers give them; the verdict
writer itself is exercised for corrections (`needs_changes`, the generation it opens, the revision request the child receives and its rendering) and for a ruling accepted afterwards. The whole path from a child's
completion receipt through delivery and acknowledgement to a ruling is therefore not run end to end here; it is the relay's existing, separately tested path, and a real-model run of it is part of the installed
proof (M4). A forged `completed` (a report that was only staged, never acknowledged, never ruled, ruled for another head, or whose criteria moved) opens no edge: each link of P-AV-1 has a case that breaks only that link.

The pause, resume, cancel and archive of a node or a plan is covered the same way (`lifecycle_test.go`): revisions written as an operator writes them, the real store and scheduler, and the real merge lane over a real git
repository for the landing commands. The guards that sit inside a transaction are tested with the pause landing between the command's unlocked read and its transaction, from a test seam, and each case checks that the
seam fired. What is not run is a real child: a paused node's child is a seeded report, not a live Codex task.

## Departures from the contract

Where the code reads the contract differently, or adds to it, and why:

* Contract 7.2 names the `edit_regions` tables for regions; those are two-project agreements keyed by base revision with no unknown or hotspot rule, so per-node declarations have their own table and reuse only the place vocabulary.
* The managed request id of a release is derived from the plan, the node and the manifest digest (contract 2.6 names the node and the digest): node ids are plan-local and request ids are global to the store. Every predicate carries the plan id, which the contract's SQL predates. The acceptance digest includes the plan id too: acceptance ids are a store-wide key and node ids are plan-local.
* The reading's vocabulary adds `blocked:release_abandoned`, `blocked:evicted` (D-12) and `blocked:stale_predecessor`; `blocked:stale_epoch` is not emitted (coordinator fencing is a later issue). A `blocked_needs_input` receipt reads as `blocked:input_unverified_at_consumption` (B-15).
* No new refusal reason: an unmet ready predicate reads `disposition_conflict` with the closed reason in the detail, tamper `revision_mismatch`, missing or altered inputs `manifest_unverified` and `scope_escape`, a stale head `merge_candidate_moved`, stale criteria `criteria_set_changed`.
* A node's pause, resume, cancel and archive and the plan's pause and resume are revision changes (CRW-281, see [DAG plans](dag-plans.md#where-this-page-reads-the-contract)); the relay's relationship status
  is a separate fact the reading honours as before. There is no plan-level cancel or archive: the contract names none.
* Archive has no row in the contract's transition table. A node the plan archived is read as the archived relationship is where the code already had a rule (its accepted pull request may still land and be observed) and as a
  cancelled node where the contract has a rule for cancel (it is never released and what follows it is blocked, as `blocked:predecessor_archived`).
* Contract 7.4 keeps an acceptance across a pause and blocks the node's new acceptance. Here the edges a pre-pause acceptance satisfied stay satisfied (an edge is not held by its predecessor's pause), and a result that arrives
  after the pause is not accepted until the resume, so what it would release stays closed.
* The contract's cancel row blocks the edges that leave a cancelled node. The reading also follows what an accepted node consumed, so a node accepted on the result of a node the plan ended does not release its successors; an
  integrated input is the exception, and no acceptance is revoked.
* P-DEC-1 reads the plan's project scope; the decision subject stays opaque and a directive satisfies an edge only when its digest equals the edge's (D-09).
* B-13 is evaluated on `dag_merge_checks` rows, which keep the evidence body; `dag_acceptances.evidence_digest` excludes its body (contract 4.3) and cannot be recomputed.
* `defer:merge_window` is emitted for a target whose merge turn is in a state that can move the tip (merging, unknown), not for a waiting or holding turn; `defer:ownership_unverified` is a project without exactly one registered parent.
* E-11 against the shipped one-row-per-output index: the same output ruled again under re-registered criteria is a row of `dag_acceptance_revalidations`, not a second acceptance.
* After a close, the release of the same manifest takes a request id derived from the closed request (`RecoveryRequestID`): contract 2.6 names the node and the digest only, and the engine refuses a released request id for good.
* A release freezes its exact request bytes (`dag_release_requests`): the managed engine fingerprints the whole request, so a replay must send the same bytes.
* `stale_base` means the base tip is not contained in the head (strict ruleset, D-11), not that the pull request's own base field differs; contract E-27 only requires `merge_currency_stale` against the new tip.
* Corrections: the manifest of a correction is prepared before the ruling and travels as a line of the ruling's restoration finding, because the verdict writer opens the generation and sends the message in one transaction.
* An unknown edit region overlaps everything; a node that edits no repository (`non_pr`) never conflicts.
* The reading adds the disposition and state `stale` with the reasons `stale:slice_changed`, `stale:criteria_changed` and `stale:edge:<edge_id>` (contract 3.1 `stale:<seed>`), and an artifact edge, or an integrated edge a landing satisfies, from a stale result reads `blocked:stale_predecessor`; the merge judgement refuses a stale result with the existing `disposition_conflict`. No refusal reason, outcome or zone statement is added. Contract 4.2 lists the landed commit among the consumed values: the integrated predicate takes the earliest positive observation of the containment run, so a manifest names a stable landing, and a landed commit that an observation of the same run names is read as unchanged ([Invalidation](#invalidation)).
* The stale reading carries a route, `action` and `action_detail` (`revalidate`, `correct`, `hold`, `redefine`), which the contract leaves to the parent. Decision D-08 is read from contract 5: a correction is a new generation of the same relationship and a node redefinition a new relationship with `supersedes`; the scheduler never registers the second. No refusal reason, reading reason, outcome or zone statement is added: a revalidation of a node that is not stale for its criteria alone, and a correction of a node that landed, are refused with `disposition_conflict`.
* Contract 3.2 and E-19 send a correction to the same child through the existing path. That path opens a generation with a `needs_changes` ruling, and the relay's verdict writer answers a replay to a second ruling on a head it ruled `verified` unless the criteria registered for the relationship changed since. For a stale node whose criteria did not change the generation is opened by hand with the relay's `generation-open` and `generation-bind` (documented for the coordinator in `crw-run`), and `dag-correct` binds it to the prepared manifest under the conditions of [Two ways to open the generation](#two-ways-to-open-the-generation). The verdict writer is not changed, and no refusal reason, zone statement or table is added: the dispatch request id is kept in the existing nullable `managed_request_id` column of `dag_node_executions`.

## Where the code reads the contract (D-19)

The contract cites file and line at an older commit. Every citation of the sections this issue consumes (76 of them) was resolved at that commit and at this baseline: 53 are identical at the cited lines, 23 moved with identical content
(the largest offsets: `registry.go` +30, `delivery/service.go` +84, `relay.md` +27), none changed and none is gone. The claims the design leans on still hold, restated for this baseline: before this change managed-start, registry and delivery did not consult execution slots (the scheduler now couples slot reservation with release intent); there was no
ancestor check (this change adds one); `work_reports` has no product writer; `managed.Start` rejects a different body under one request id; the managed request id limit is unchanged at 128. New since the contract: the plan
store of CRW-183, and the delivery role gate (`registry.CheckBoundRole`), which no longer withholds a relay-managed parent's completion delivery.

The stale handling (CRW-284) resolved the citations of the sections it consumes (3.2, 5, 5.1, 8.4 and E-11, E-19 to E-21, E-24, E-25, E-28: 36 of them, the schema ones against `contract/schema/relay-sqlite.sql`) at the contract's commit and at its baseline: 20 are identical at the cited lines, 16 moved with identical content (`registry/registry.go` by up to 30 lines, `delivery/service.go` by 141, `crw-run/references/relay.md` by 27), none changed and none is gone. The claim it leans on holds as cited: the verdict writer takes another ruling on a head it ruled verified only on a re-review (`delivery/ack.go` 432 to 440), and a correction is the next generation of the same relationship (`delivery/ack.go` 499).
