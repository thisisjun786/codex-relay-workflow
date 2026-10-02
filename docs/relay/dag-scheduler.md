# DAG scheduler

The scheduler runs a stored DAG plan ([DAG plans](dag-plans.md)). It computes which nodes may start, says why every other node may
not, and (in the later sections of this page) releases a ready node to a Codex child, records what the parent accepted and what
landed, and judges merge eligibility. It is code in `internal/relay/dagsched`, reached through `crw relay dag-*` commands that read
and write the relay store the other relay commands use. It holds no goal, runs no loop, starts no daemon and opens no database of
its own: a goal-free parent calls it when a relay result, a block or a decision request wakes it, acts on the answer and ends its
turn. No predicate here depends on a model's judgement.

The page follows the DAG execution contract (CRW-182) sections 2, 3, 4, 7 and 8; where the code departs from the contract's wording it
says so in [Departures](#departures-from-the-contract).

## The ready set

`crw relay dag-ready --plan P` reads the plan at its head revision and the relay's execution records and answers, for every live node,
one disposition and (except for a ready node) one reason from a closed set. A reading writes nothing, reads no clock and opens
no second connection, so two readings of one store state are byte-identical; its `input_digest` covers everything it read, the bytes of
the artifact files included. A store with no DAG zone, or a plan it does not hold, answers `unregistered_scope` and is left as it was.

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
| blocked | `blocked:stale_predecessor` | an input of an accepted predecessor is no longer the active acceptance of its node |
| blocked | `blocked:creation_unknown` | a child's creation was armed and its outcome is not known; repeating the release reconciles it |
| blocked | `blocked:effect_unknown` | a merge turn for the accepted head ended with an unknown effect |
| blocked | `blocked:predecessor_cancelled` | the predecessor's relationship was cancelled before its result was usable |
| blocked | `blocked:release_abandoned` | the managed start of a decided release was released before it created a child |
| blocked | `blocked:evicted` | a required check failed again on the same head after its one retry, and the node left the merge lane |
| blocked | `blocked:ambiguous_head` | the relationship has more than one head revision, or an execution has no relationship |
| done | `done:accepted` | the parent accepted the node's result |
| done | `done:integrated` | the accepted head is contained in every target it has to land on and the parent marked it merged |

`blocked:stale_epoch` is in the contract's vocabulary and is not emitted: coordinator fencing is a later issue.

The derived state of each node is shown beside its disposition: `waiting`, `ready`, `releasing`, `creation_unknown`, `running`, `reported`,
`verifying`, `correcting`, `accepted`, `integrated`, `paused`, `cancelled`, `closed` or `ambiguous`. It is read from the rows each time and never stored.

### Edge satisfaction

An edge is satisfied by what the store holds now. Every predicate is scoped to the plan: node and edge ids are plan-local.

* `artifact_verified`: the predecessor has an active acceptance that digests to its id, has every required column, belongs to an
  execution of the node, stands on the plan's and the relationship's criteria (a later re-verification counts), is still the head of its
  generation while the relationship lives, rests on the active acceptances of its own inputs, and, for a code edge, carries the head, the
  pull request and the forge identity the edge pins. The latest merge check of the pull request, if any, must digest to its evidence and name
  the accepted head. A cancelled predecessor satisfies nothing.
* `integrated`: an observation says the accepted head is an ancestor of the target branch, no later observation says otherwise, the parent marked
  the same revision merged, and, when a merge turn carried the observation, that turn landed the same head on the same branch.
* `decision`: an active, approved decision of the subject with the plan's digest by an authority kind the edge names, or a settled supervisor
  directive with that digest. Authority text is opaque: it is compared, never read.

### Input checks

When all incoming edges of a candidate are satisfied, the artifacts it would consume are read again: every file the predecessor's receipt declared must still
exist, hash to the declared digest and size and lie under the predecessor's artifact roots; a frozen copy of the manifest must read and agree; an artifact
edge that hands over no artifact is blocked, never an empty success; the list of artifacts a receipt declares must hash to the revision the parent accepted (so an artifact left out of the list, or added to it, is found even when every remaining file is intact); and following the consumed acceptances down through their manifests, no node may appear
under two acceptances. The first violated path decides the reason.

### Ranking and selection

Candidates are ordered by the hop count of the longest chain each starts (critical path), then by the nodes below it, then by the stored time it became ready,
then by node id. A long independent node therefore never holds back a short dependent chain: a node waits only for its own predecessors. The first candidates
that fit the free slots are ready; each candidate cut is deferred with `defer:no_capacity` (or `defer:capacity_unmeasured`), and the pass records which limit decided
the first cut: `none`, `no_capacity`, `edit_overlap` or `capacity_unmeasured`.

### Capacity

Free slots are the smallest headroom over the initiative above the project, the project and the store, counting held execution slots against each `runs` ceiling
the way `slot-reserve` does (a ceiling of 0.5 still allows one reservation). The DAG never runs more children than the standing cap of 6 unless a cap basis for that
limit revision is recorded in `dag_cap_basis` (decision D-05 is not made, so no value above 6 is assumed). An enforced ceiling on another dimension without a usage
observation leaves no slot free.

### Edit regions

`crw relay dag-region-declare --plan P --node N --actor A --regions @file` declares the places an implementation node will edit, before it is released: 1 to 64 regions of
a repository, path, kind (`tree`, `file`, `symbol` with its symbol) and change (`edit`, `rename`, `delete`). A repository is a forge `owner/name` or an existing local
checkout (the directory its absolute path reaches, links resolved); a path is kept as written and refused when it has whitespace at either end. A rename, a delete and the
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

1. **Replay first.** An intent already recorded for the node is continued from the exact request bytes frozen with it (`dag_release_requests`) and never rebuilt: the managed engine fingerprints the whole
   request, while the manifest digest leaves out fields that are still in the prompt, so a clock or a base tip that moved would read as a second release. A replay of an intent whose slot was returned meanwhile reserves it again under the same ceilings, and is refused `capacity_exhausted` when none is free. A replay presented with another spelling of the marker
   root, the socket or the state selector than the intent was frozen under is refused (`disposition_conflict`) before the engine is asked; a stored manifest or request that no longer digests to what was recorded is
   `revision_mismatch`.
2. **Judge, without a transaction.** The reading says the node is ready (or deferred only for capacity: the ceiling is enforced in step 4, where the contest is recorded). For every pinned predecessor the relay reads
   the pull request itself from the forge, by the owner/name it recorded at acceptance, and refuses when its head is no longer the accepted one after recording a `stale_head` observation (E-10); an unreadable or
   incomplete read is the host's failure, nothing is written, and a retry is allowed; a closed or merged pull request is fine, only the head is compared. The request's criteria must digest to the plan's. The manifest is
   built and verified, every artifact and volatile snapshot hashed again.
3. **Assemble the request.** Its id is derived from the node and the manifest digest only (`dag-` and 40 hex characters): no attempt counter, so a replay is the same request. The prompt carries the manifest inline,
   or the path and digest of its frozen copy (`<first artifact root>/dag-input-manifests/<digest>.json`, create-exclusive, mode 0600, re-read) when it exceeds 60000 characters, and tells the child, in English, to
   verify every uri, hash and size before consuming an input and to report `blocked_needs_input` on any mismatch.
4. **Write the intent.** The acceptance whose pull request was read in step 2 must be the one the manifest consumes (a predecessor accepted again meanwhile is a head that was never checked: `disposition_conflict`, repeat the release). One transaction judges the store half again (a plan revision, an acceptance or a registration may have moved), checks the slice, the criteria and the incoming edges against the manifest,
   decides capacity, reserves a slot of subject kind `dag_node` and key `plan:node` (decision D-15) and writes the manifest, the frozen request and the release together, or nothing. When no slot is free the
   contest is recorded and `capacity_exhausted` answered; a refusal of the reservation itself (another parent, a ceiling at initiative or store scope) leaves its conflict row and no intent, and any other failure of the store rolls the whole transaction back.
5. **Start the child**, outside any transaction, with the frozen bytes. A refused or incomplete start (a creation whose outcome is unknown, settings that differ from what was asked) leaves the intent and the slot; the
   same call again reaches this step again and the engine reconciles with the creation it already made. A request another caller is advancing waits for that caller to bind its child.
6. **Bind** the child to the node (`dag_node_executions`, kind `initial`). The engine already refuses a creation whose model, effort, sandbox or approval policy differ from the request; the bind refuses a
   relationship that is not this node's issue and parent.

The slot is held from step 4 until the parent releases it with the acceptance of a non-PR node or the integration of an implementation node, or an operator does.

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

## Commands

| Command | Reads or writes | Answer |
| --- | --- | --- |
| `dag-ready --plan P [--record --actor A]` | reads; writes one `dag_passes` row with `--record` | the reading: `plan_id`, `plan_revision`, `state_digest`, `input_digest`, `pass`, `ready`, `nodes` |
| `dag-region-declare --plan P --node N --actor A --regions R` | writes `dag_node_regions` | the declaration in force and whether it replayed |
| `dag-release --plan P --node N --actor A --request R --marker-root D` | writes the intent (`dag_releases`, `dag_release_requests`, `dag_input_manifests`), reserves a slot, starts a managed task, binds it (`dag_node_executions`) | the release: manifest digest, request id, slot, relationship, generation and child; exit 2 with the managed engine's answer nested under `managed` when the start was refused or incomplete |

Refusals use the relay's existing reasons: `unregistered_scope` (a plan or node that is not there), `malformed_receipt` (a region or a request that is not valid),
`disposition_conflict` (a node that edits no repository, or whose regions are held, or that is not ready), and the reasons of the table above. Exit codes are the relay's: 0, 2 refusal, 3 host, 4 usage.

## The store

The scheduler adds tables to the DAG zone ([DAG plans](dag-plans.md#the-store)) as appended statements. `dag_passes` and `dag_node_regions` are described above; `dag_release_requests` freezes the request of a release with its intent;
`dag_merge_checks`, `dag_acceptance_revalidations` and `dag_acceptance_forge` hold what the relay observed of an accepted pull request, re-verification of an accepted
output under new criteria, and the forge identity of an accepted implementation node.

## Departures from the contract

* Contract 7.2 names the `edit_regions` tables for regions; those are two-project agreements keyed by base revision, so per-node declarations have their own table.
* Every predicate carries the plan id, which the contract's SQL predates.
* The acceptance digest includes the plan id: acceptance ids are a store-wide key and node ids are plan-local.
