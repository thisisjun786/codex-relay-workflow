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
5. End the turn. Nothing is resident: the scheduler is a library the relay's commands call over the relay's one store; it adds no daemon, no listener and no database of its own
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
3. **Assemble the request.** Its id is derived from the node and the manifest digest only (`dag-` and 40 hex characters): no attempt counter, so a replay is the same request. The prompt carries the manifest inline,
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
the child verifies every input against the manifest before consuming it. A start that was released after its slot was reserved leaves the slot held and the node `blocked:release_abandoned`. Returning the slot frees capacity but does not revive the tombstoned request, and changing the node's slice does not replace its recorded intent: recovery of that intent is not implemented here (it belongs to invalidation and adoption).

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

The same output accepted again is a replay when it names the pull request recorded with the acceptance and the forge still shows the accepted head there (a head that moved is `merge_candidate_moved`, another pull request `disposition_conflict`; a pull request merged at the accepted head is still the same output). The same output ruled again under re-registered criteria is a **revalidation** of the same acceptance (`dag_acceptance_revalidations`, contract E-11): no second acceptance, generation or child. A new output replaces a node's active
acceptance only with `--supersedes <that acceptance>`; without it the call is `disposition_conflict`. The acceptance digest names the plan, the node, the manifest, the execution (relationship and generation), the event and revision, the criteria, the verdict and, for an implementation node, the head, the repository and the pull request number, so a changed row reads as `blocked:acceptance_tampered`. The evidence digest, the rule version, the acknowledgement tier and the times are recorded and left out of the identity, so a re-run of a check never changes what was accepted.
The rule version (skills digest, model, effort) is recorded with the acceptance and left out of its identity. A non-PR node's slot is returned by its acceptance; an implementation node's slot is held until it is integrated.

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
instruction line that names the manifest, a copy of its canonical bytes kept under the child's first artifact root (the file the child reads, since it cannot read the relay's store; see *The frozen copy*) and the sha256 of that file; the parent submits that line as the **restoration** finding of its `needs_changes` ruling. After the ruling, `dag-correct` (without `--prepare`) binds the generation the ruling opened to the manifest that finding names
(`dag_node_executions`, kind `correction`) once the manifest is stored for this node at its current slice and criteria and the restoration note carries, word for word, the instruction that manifest was prepared with (this generation, the path of the copy and its hash). No file is read when the generation is recorded: the child checks the copy against the hash in the line it was sent. The bound digest is derived from what the child was told: a digest in an ordinary finding does not bind, a restoration block that names two different manifests is refused, a ruling without the block leaves the previous manifest in force (`carried_over`), and a `--manifest-digest` that differs
is refused, also when the generation is already bound. The previous acceptance stays active and reads `blocked:stale_head` until the parent accepts the corrected result with `--supersedes`; invalidating what depends on it is a later issue.

### The frozen copy

A manifest that a child has to read from a file (an oversize release prompt, or a correction) is kept as `<first artifact root>/dag-input-manifests/<sha256 of its canonical bytes>.json`. The name is the hash of the
bytes, not the manifest digest: two bodies of one manifest differ in what the digest leaves out (the time of the build, the rule version), so they are two files. An existing regular file of that name is reused only after the authorized, byte-for-byte check below. A newly created file is created exclusively with mode 0600, in a directory that must be a directory of its own (a link is refused), and is read back through the relay's authorized
open (inside the root, no link on the way, a regular file, never blocking on a pipe) and compared byte for byte. The child owns its artifact root, so a link or a pipe planted there makes the freeze fail and never makes it wait. A link planted between the check of the directory and the creation of the file, or an artifact
root that is itself a link, can leave one file outside the root (it holds only a manifest the relay wrote); the read-back then refuses and nothing is bound. An artifact root with a control character is refused when a correction is prepared,
because the relay's message joins the lines of a finding and the child would read another path. A correction uses the body the store holds for the digest, so preparing twice from the same inputs gives the same file and the same line. The directory is created by the freeze and is not
cleaned up by it.

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

A required check is read the way merge-evidence and the merge lane's own check read it: of every run only its newest attempt counts, every run of a required name must be a `success` (a skipped or neutral run is not one, so a head the judgement lets in is not refused by the lane for the same check), a required (name, integration) pair needs its own run (a name that two integrations must answer needs both), and the opaque run identities are never ordered. Failures count only when every required check has finished, so one red job seen beside a running one never uses up the retry. A second, different failure after the retry round opened evicts. A failure is told from another by the run's identity, its attempt and the time the forge last changed it (a commit status has no attempt and a check run can be reset in place), and a failure of a run that a judgement saw not failing (running or passing) since it failed is a new failure whatever its identity; another check running again says nothing about this one. Everything is computed from the newest reading of each run (a listing that still carries a failed first attempt beside the passing second one judges as the second), a failure counts once a judgement recorded it (a red job beside a running one is not counted), and a reading of a run that is older than one already judged (an earlier attempt, or an earlier completion of the same attempt, arriving late) is not judged: it is `merge_candidate_moved` and the caller reads again. So is a reading of the pull request that was in flight while another judgement of the node was recorded: it is older than the history whatever it contains, and nothing is written. Times are compared as instants (the forge's spelling does not matter). A forge that returns an older state of a run after a newer one was judged cannot always be told from a run that was reset: a reading that goes backwards without a time and with no judgement between the two (a run read as running after its completion) can count as a rerun, so a head can be evicted one failure early. Two failures that carry the same identity and the same time and that no judgement saw apart read as one failure: the node is not eligible either way, but it can take more than one retry. A pull request whose list of
required checks cannot be read is not judged (`merge_evidence_malformed`, nothing written): ignorance is not "none required". A draft, closed or merged pull request is `disposition_conflict`; evidence the relay could not read
completely is the host's failure; both write nothing. A row of the history that no longer digests to what was recorded (B-13) is `revision_mismatch`, and the edges built on it read `blocked:evidence_mismatch`.

`crw relay dag-merge-request --plan P --node N --actor A --host H` judges again now (never from the history) and, only when the node is eligible, asks the existing merge lane for a turn with the accepted head, the pull request and the
relationship, in one transaction that reads again the acceptance, the relationship, the criteria, the predecessors that have to have landed and the latest judgement (its head, and the evidence it recorded digested again): a pause or a change that lands between them creates no turn and no grant. A node that is not eligible gets no turn and the refusal is the relay's own reason (`merge_candidate_moved` for a stale head, `merge_currency_stale` for a stale base, `criteria_set_changed` for stale criteria,
`disposition_conflict` naming the outcome for the rest); the judgement stays in the history, which is what counts the retry. The lane is not changed: a holder has one live claim per target, so the parent merges one pull request of
a project at a time (a second request while the first turn is open, even for another node accepted at the same commit, is `disposition_conflict` naming that turn), and turns of different projects are served first in, first out (D-16, `requested_at` then turn id). The parent's order is
accept, `dag-merge-request`, acknowledge the grant, `merge-turn-check`, merge on the forge, `merge-turn-land`, `assignment-mark merged`, `dag-integration-observe`. A pull request that went through `dag-merge-request` is
the only kind the DAG path lets into the lane, so every landed tree of it has an `eligible` judgement of the very head that landed.

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
| `dag-release --plan P --node N --actor A --request R --marker-root D` | writes the intent (`dag_releases`, `dag_release_requests`, `dag_input_manifests`), reserves a slot, starts a managed task, binds it (`dag_node_executions`) | the release: manifest digest, request id, slot, relationship, generation and child; exit 2 with the managed engine's answer nested under `managed` when the start was refused or incomplete |
| `dag-accept --plan P --node N --actor A --rule-version R [--repository R --pull-request N --event E --supersedes ID]` | writes `dag_acceptances` (and `dag_acceptance_forge`, or a revalidation); returns a non-PR node's slot | the acceptance: `acceptance_id`, `replayed`, `revalidated`, `superseded_acceptance_id`, `head_sha`, `evidence_digest`, `slot_released` |
| `dag-integration-observe --plan P --node N --actor A [--target REPO@REF]` | appends `dag_integration_observations`; returns an implementation node's slot when it is integrated | the observations, `integrated`, `mark_present`, `slot_released` |
| `dag-decision-record --plan P --actor A --subject S --digest D --disposition X --authority-kind K --authority-ref R` | writes `dag_decisions` | the decision id and revision, `replayed`, the decision it superseded |
| `dag-correct --plan P --node N --actor A [--prepare --manifest-request R] [--manifest-digest D]` | `--prepare` stores a manifest; otherwise writes `dag_node_executions` (kind `correction`) | the instruction line and manifest digest, or the generation bound and `carried_over` |
| `dag-merge-judge --plan P --node N --actor A [--repository OWNER/NAME --pull-request N]` | appends `dag_merge_checks` (when the judgement differs from the latest) | the outcome, reason, round, check sequence, heads, base tip and failed required checks |
| `dag-merge-request --plan P --node N --actor A --host H [--repository OWNER/NAME --pull-request N]` | the judgement as above; asks the merge lane for a turn (`merge_turns`) when eligible | the judgement and the turn |
| `dag-conflict-observe --plan P --actor A --repository PATH --left-node N --right-node M --left-head SHA --right-head SHA` | writes `dag_conflict_observations` | the number of conflicting files and their names |
| `dag-cap-basis-record --limit L --revision R --w-minutes W --w-source S --s-minutes S --s-source S --actor A` | writes `dag_cap_basis` | the basis recorded |

Refusals use the relay's existing reasons: `unregistered_scope` (a plan or node that is not there), `malformed_receipt` (a region or a request that is not valid),
`disposition_conflict` (a node that edits no repository, or whose regions are held, or that is not ready), and the reasons of the table above. Commands can also refuse with `merge_target_unreadable`, `not_acknowledged`, `relationship_conflict`, `relationship_not_active`, `revision_ambiguous`, `scope_role_mismatch`, `slot_unknown` and `unregistered_relationship`. Exit codes are the relay's: 0, 2 refusal, 3 host, 4 usage.

## The store

The scheduler adds tables to the DAG zone ([DAG plans](dag-plans.md#the-store)) as appended statements. `dag_passes` and `dag_node_regions` are described above; `dag_release_requests` freezes the request of a release with its intent;
`dag_conflict_observations` holds the merge-tree conflict counts of parallel branches; `dag_merge_checks`, `dag_acceptance_revalidations` and `dag_acceptance_forge` hold what the relay observed of an accepted pull request, re-verification of an accepted
output under new criteria, and the forge identity of an accepted implementation node.

## How the acceptance path is verified

The tests drive the real Go store, the real managed engine and bridge adapter over a fake App Server, a real git repository, the relay's own verdict writer and renderer, and the merge lane's own check and landing.
Two things are scripted and say so: the forge (a pull request's head, base and checks are what the test scripts, since no forge is reachable), and the child's report. The rows of a verified report (the final event, its
lineage, the acknowledgement and its evidence, the ruling and its context, the registered criteria) are written by a test helper in the shape the intake, acknowledgement and verdict writers give them; the verdict
writer itself is exercised for corrections (`needs_changes`, the generation it opens, the revision request the child receives and its rendering) and for a ruling accepted afterwards. The whole path from a child's
completion receipt through delivery and acknowledgement to a ruling is therefore not run end to end here; it is the relay's existing, separately tested path, and a real-model run of it is part of the installed
proof (M4). A forged `completed` (a report that was only staged, never acknowledged, never ruled, ruled for another head, or whose criteria moved) opens no edge: each link of P-AV-1 has a case that breaks only that link.

## Departures from the contract

Where the code reads the contract differently, or adds to it, and why:

* Contract 7.2 names the `edit_regions` tables for regions; those are two-project agreements keyed by base revision with no unknown or hotspot rule, so per-node declarations have their own table and reuse only the place vocabulary.
* Every predicate carries the plan id, which the contract's SQL predates. The acceptance digest includes the plan id too: acceptance ids are a store-wide key and node ids are plan-local.
* The reading's vocabulary adds `blocked:release_abandoned`, `blocked:evicted` (D-12) and `blocked:stale_predecessor`; `blocked:stale_epoch` is not emitted (coordinator fencing is a later issue). A `blocked_needs_input` receipt reads as `blocked:input_unverified_at_consumption` (B-15).
* No new refusal reason: an unmet ready predicate reads `disposition_conflict` with the closed reason in the detail, tamper `revision_mismatch`, missing or altered inputs `manifest_unverified` and `scope_escape`, a stale head `merge_candidate_moved`, stale criteria `criteria_set_changed`.
* Plan-level pause does not exist in the revision operations, so a plan is always active until coordinator fencing.
* P-DEC-1 reads the plan's project scope; the decision subject stays opaque and a directive satisfies an edge only when its digest equals the edge's (D-09).
* B-13 is evaluated on `dag_merge_checks` rows, which keep the evidence body; `dag_acceptances.evidence_digest` excludes its body (contract 4.3) and cannot be recomputed.
* `defer:merge_window` is emitted for a target whose merge turn is in a state that can move the tip (merging, unknown), not for a waiting or holding turn; `defer:ownership_unverified` is a project without exactly one registered parent.
* E-11 against the shipped one-row-per-output index: the same output ruled again under re-registered criteria is a row of `dag_acceptance_revalidations`, not a second acceptance.
* A release freezes its exact request bytes (`dag_release_requests`): the managed engine fingerprints the whole request, so a replay must send the same bytes.
* `stale_base` means the base tip is not contained in the head (strict ruleset, D-11), not that the pull request's own base field differs; contract E-27 only requires `merge_currency_stale` against the new tip.
* Corrections: the manifest of a correction is prepared before the ruling and travels as a line of the ruling's restoration finding, because the verdict writer opens the generation and sends the message in one transaction.
* An unknown edit region overlaps everything; a node that edits no repository (`non_pr`) never conflicts.

## Where the code reads the contract (D-19)

The contract cites file and line at an older commit. Every citation of the sections this issue consumes (76 of them) was resolved at that commit and at this baseline: 53 are identical at the cited lines, 23 moved with identical content
(the largest offsets: `registry.go` +30, `delivery/service.go` +84, `relay.md` +27), none changed and none is gone. The claims the design leans on still hold, restated for this baseline: before this change managed-start, registry and delivery did not consult execution slots (the scheduler now couples slot reservation with release intent); there was no
ancestor check (this change adds one); `work_reports` has no product writer; `managed.Start` rejects a different body under one request id; the managed request id limit is unchanged at 128. New since the contract: the plan
store of CRW-183, and the delivery role gate (`registry.CheckBoundRole`), which no longer withholds a relay-managed parent's completion delivery.
