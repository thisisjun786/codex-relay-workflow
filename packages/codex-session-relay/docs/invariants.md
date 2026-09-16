# Invariants

JSON Schema is the structural layer only. Everything here is contract prose or observed host
behaviour that the schema cannot express, with the code that actually refuses. Each row carries a
status. Every row below is implemented and carries a test; the suite is the proof, not this table.

## Identity and registration

| # | Invariant | Enforced in | Status |
|---|---|---|---|
| I-01 | Relationship id is deterministic, so re-registration is idempotent | `identity.relationship_id`, `registry.register` | implemented |
| I-02 | Identity is actual task ids; no title or "latest session" is ever a routing key | `registry` accepts only `Endpoint(taskId, hostId, cwd)`; no name reaches the routing path | implemented |
| I-03 | The current generation must exist in the retained generation list | `registry._assert_generation_invariant` | implemented |
| I-04 | Replaying one dispatch request id opens no second generation | `registry.open_generation` looks up by dispatch request id first | implemented |
| I-05 | A pending anchor never auto-binds to whichever turn appears next | `registry.bind_anchor` requires an explicit turn id and a dispatch-receipt source | implemented |
| I-06 | A receipt under an unbound generation is refused, and the pending anchor stays reportable | `receipts._check_generation` | implemented |
| I-07 | A superseded relationship is preserved, never rewritten | `registry.supersede` writes links on both sides; the original record blob is immutable | implemented |

## Scope and artifact authorization

| # | Invariant | Enforced in | Status |
|---|---|---|---|
| I-10 | Containment is by path component, so `/a/b` does not contain `/a/bc` | `scope.is_within` | implemented |
| I-11 | The bytes that are hashed are provably inside an authorized root | `scope.open_authorized`: a pinned walk from `/` opening every component with `O_NOFOLLOW`, then `readlink("/proc/self/fd/N")` must equal the declared path, then hashing from that descriptor | implemented |
| I-12 | The declared normalized absolute path enters the digest unchanged | `manifest.canonical_payload` uses the declared string verbatim; authorization never rewrites it | implemented |
| I-13 | A recipient outside the authorized list is refused before any transport call | `delivery.enqueue` and `delivery.attempt`, the second immediately before the adapter call | implemented |
| I-80 | A path whose ancestor is relocated mid-resolution is refused, not read | the `/proc/self/fd` check above; `ELOOP`/`ENOTDIR` map to a symlink refusal, `ENOENT`/`ESTALE` to a path-changed refusal | implemented |

## Receipts and classification

| # | Invariant | Enforced in | Status |
|---|---|---|---|
| I-20 | A completed turn with no child receipt is an ordinary turn end, never success | `receipts.classify_observation` | implemented |
| I-21 | A daemon observation may assert only failure or interruption | `receipts.daemon_observation` | implemented |
| I-22 | Blocked-needs-input is a child assertion only | same; also a recorded capability limit, since approvals are unsupported end to end | implemented |
| I-23 | A reviewable claim is verified against actual bytes, not trusted | `manifest.verify_against_disk` re-hashes every file; truncation or absence refuses | implemented |
| I-24 | The digest is recomputed by the consumer | `receipts` recomputes from the manifest and compares | implemented |
| I-25 | Event ids are recomputed and compared | `identity.event_id` plus an intake comparison | implemented |
| I-26 | The turn reference must equal the observation that accompanied the receipt | intake comparison | implemented |
| I-27 | One revision collapses on re-observation; a different revision is a separate retained target | events keyed by event id; a new digest yields a new id and both rows persist | implemented |
| I-28 | Any outcome other than reviewable carries a null manifest and the no-deliverable sentinel, for any producer | `receipts._check_outcome_consistency`, branching on outcome before producer | implemented |
| I-29 | Artifact bytes are only ever read | no write mode anywhere in the package; asserted by test | implemented |
| I-75 | A frozen manifest copy keeps the original declared paths, so relocation does not change the digest | `manifest.freeze` and `manifest.verify_frozen` | implemented |

## Delivery

| # | Invariant | Enforced in | Status |
|---|---|---|---|
| I-30 | An active recipient is never interrupted; the send is withheld before any resume | pre-check in `delivery.attempt`, plus the transport's own busy guard | implemented |
| I-31 | Model, effort, sandbox and approval policy are never changed | the only mutating call carries a request id, a thread id and a message; no overrides exist | implemented |
| I-32 | The error code is classified before the method prefix | `transport.classify_operation_receipt` | implemented |
| I-33 | An unfinished receipt is never proven non-delivery | unfinished maps to unknown and uncertain | implemented |
| I-34 | Retry-safe is true only for a completed, attributable pre-send rejection | `transport`, re-checked by `delivery._assert_attempt_invariants` | implemented |
| I-35 | Dispatched is not acknowledgement | the aggregate state stays dispatched until an acknowledgement row exists | implemented |
| I-36 | A returned turn id is checked, not assumed to be a fresh turn | `delivery._check_turn_identity` compares against turn ids known before the send | implemented |
| I-37 | A retry never replays a cached failure under one request id | attempt numbers always increment; a settled attempt row is immutable | implemented |
| I-38 | An inbox-only fallback is never reported as a wake | reported as stored-not-woken and held, not retried | implemented |
| I-39 | A request-id collision across different events is detected, not cross-wired | the request id is a primary key | implemented |
| I-70 | Claiming a delivery and allocating its attempt are one transaction | a single `BEGIN IMMEDIATE` does the conditional update and the attempt insert | implemented |
| I-78 | A lease prevents a second concurrent claimer and authorizes nothing on expiry | expiry hands the attempt to reconciliation, which still requires evidence | implemented |
| I-11b | The rendered message is deterministic, so one request id keeps one transport fingerprint | `delivery.render_message` derives only from the stored event | implemented |

## Reconciliation and recovery

| # | Invariant | Enforced in | Status |
|---|---|---|---|
| I-40 | Fixed order: the operation receipt first, then the recipient's real turns | `reconcile.reconcile_attempt` records which step ran | implemented |
| I-41 | Exactly three things are affirmative evidence, and elapsed time is not one | the evidence enum has no time input | implemented |
| I-42 | A truncated scan is inconclusive, never proof of absence | the scan reports whether it exhausted the cursor | implemented |
| I-43 | Same-direction paging uses the forward cursor, with a total scan bound | `bridge_adapter.find_token`; the reverse cursor is only for a direction change | implemented |
| I-44 | Every state transition is committed before its side effect | the claim transaction commits before the adapter is called | implemented |
| I-45 | An interrupted transition leaves no partial record | `store.transaction` rolls back on any exception | implemented |
| I-46 | Paused, cancelled and archived relationships are never auto-resumed | eligibility joins on active status; resume requires restated generation and scope | implemented |
| I-71 | Reconciliation never fabricates an accepted transport status | the transport snapshot is re-derived only from a fresher receipt for the same request id | implemented |
| I-72 | A missing transport ledger row is an observation, not affirmative evidence | the adapter maps the transport's unknown-request error to a missing observation | implemented |
| I-79 | No default-reachable configuration permits an attempt without one of the three affirmative evidences | operator release defaults to disabled | implemented |

## Acknowledgement and the reverse direction

| # | Invariant | Enforced in | Status |
|---|---|---|---|
| I-50 | Only the parent, from inside a real turn, closes dispatched to acknowledged | `ack.acknowledge` verifies the turn in the recipient's real turn list | implemented |
| I-51 | The proof is supplied by the parent and verified, never computed for the caller | `ack.acknowledge` refuses on mismatch | implemented |
| I-51b | The delivered message contains no parent turn id | asserted by test | implemented |
| I-52 | An acknowledging turn must have started after the delivery | compared against the attempt's observation time | implemented |
| I-53 | An unverifiable turn does not close the attempt | stored as unverified; the delivery state is unchanged | implemented |
| I-54 | Duplicate delivery cannot cause a second verification | `ack.claim_verification` is an idempotent insert keyed on event id | implemented |
| I-55 | A revision routes to the same child under a new generation | `ack.record_verdict` opens the generation on the same relationship | implemented |
| I-73 | A verdict requires an acknowledged event | `ack.record_verdict` | implemented |
| I-74 | The child must be an authorized recipient before a revision is queued | checked before anything is written | implemented |
| I-76 | The reverse direction is stored as a relay-internal record, outside the parent-shaped schemas | delivery rows carry a kind; conformance validation partitions by kind | implemented |
| I-77 | Acknowledgement and disposition evaluation are unreachable for a revision request | kind guards raise | implemented |

## Bounds

| # | Invariant | Enforced in | Status |
|---|---|---|---|
| I-60 | Repeated failure cannot become an infinite wake | an attempt cap, then a hold that is observable and never auto-retried | implemented |
| I-61 | A busy recipient is not a failure, but is still bounded | a separate cap and backoff curve | implemented |
| I-62 | No notification flood | a per-recipient minimum interval and hourly cap | implemented |
| I-63 | Unchanged states stay quiet | a tick that changes nothing writes no journal rows | implemented |
| I-64 | An unbounded daemon loop is not constructible | `run` requires a tick count, a deadline or a stop signal | implemented |
| I-65 | A supervisor is bounded by the owner's intent, not by a timer | it re-reads `service.json` and the stop request between segments; `RelayDaemon.run` is unchanged, so every worker it launches is still bounded by I-64 | implemented |
| I-66 | The current generation cannot be starved by history | candidates are filtered before the per-tick budget, the current anchor is reserved, and the remainder rotates through a persisted cursor | implemented |
| I-67 | One parent's backlog cannot consume another parent's opportunity | selection asks which parents are eligible before asking how many rows each has, then deals a bounded share one at a time | implemented |
| I-68 | A delivery whose generation has moved on cannot be claimed | the claim statement refuses it, so the decision cannot be overtaken between checking and acting | implemented |
| I-69 | An outstanding send is never rewritten as terminal | suppression annotates it instead, because reconciliation refuses to promote a terminal superseded aggregate and a lost response would become unresolvable | implemented |


## Recorded limits, so a row above is not read as more than it is

| Limit | Consequence |
|---|---|
| Ordering is not lineage | a later turn is admitted only by an explicit continuation record; host ordering corroborates and can contradict, never admits |
| Byte stability is enforced only under a read lease | the lease is opt-in because holding one blocks writers for the kernel lease-break timeout; otherwise each detector has a named evasion |
| Inode ownership is not proven | a hardlink or bind mount can expose the same bytes under another authorized path, which the contract permits because it authorizes paths |
| The JSON date-time format is unvalidated | the available validator has no working format checker, so timestamp format is unverified rather than implied |
| Terminal turns are polled, not subscribed | the transport cannot subscribe, so automatic invocation is a bounded poll that then dispatches |
| Scheduler fairness is not transport concurrency | the adapter serialises on one worker, so a stalled call still blocks the one behind it; what is guaranteed is that a struggling parent stops being handed the rest of the budget |
| A live process is not a working one | health is computed from staged age, anchor poll freshness and backlog; liveness is reported separately and never counted |
| Archive state can be unknown | an inconclusive listing withholds rather than guessing, and a later observation releases it |
