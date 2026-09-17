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
| I-54 | Duplicate delivery cannot cause a second verification | `ack.claim_verification` is an idempotent insert keyed on event id, reopened only where `ack._re_review_open` holds | implemented |
| I-55 | A revision routes to the same child under a new generation | `ack.record_verdict` opens the generation on the same relationship | implemented |
| I-73 | A verdict requires an acknowledged event | `ack.record_verdict` | implemented |
| I-74 | The child must be an authorized recipient before a revision is queued | checked before anything is written | implemented |
| I-76 | The reverse direction is stored as a relay-internal record, outside the parent-shaped schemas | delivery rows carry a kind; conformance validation partitions by kind | implemented |
| I-77 | Acknowledgement and disposition evaluation are unreachable for a revision request | kind guards raise | implemented |
| I-143 | A verified acknowledgement is settled, and a competing disposition cannot replace it | `ack.acknowledge` re-reads the acknowledgement as the first statement inside its write transaction and returns the stored record whatever the caller asked for | implemented |
| I-144 | A criteria edit is re-reviewable exactly where the view calls it re_review_needed: a verified ruling, or a claimed review, on the revision this generation still stands on | `ack._re_review_open`, the single condition both `claim_verification` and `record_verdict` read | implemented |
| I-145 | A re-review replaces what the assignment stands on, never the record of deciding it | `ack.record_verdict` journals `verdict_superseded` with the replaced ruling and both criteria digests | implemented |

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

## Work reports and the CXC report contract

| # | Invariant | Enforced in | Status |
|---|---|---|---|
| I-90 | A CXC report status never chooses a relay outcome; it is checked against the one the receipt asserted | `cxc.check_status`, called from `report.record` | implemented |
| I-91 | An unrecognised report status is refused by name, with the accepted set and the contract version | `cxc.check_status` | implemented |
| I-92 | DONE, an open pull request, a review PASS and a green check are never a relay verdict | `cxc.NOT_VERIFICATION`, `cxc.refuse_promotion`; no path writes a verdict outside `ack.record_verdict` | implemented |
| I-93 | A verdict line is rendered only for a message carrying a review, in the fixed PASS / GO-WITH-FIXES (blockers=N) / FAIL form | `cxc.verdict_line`, `cxc.assert_reviewed` | implemented |
| I-94 | A report is bound to one event, generation and revision, and cannot answer for a later head | `report.assert_current` | implemented |
| I-95 | A pull request number is never rendered or compared without its repository | `report.pr_ref`, `report.pr_key` | implemented |
| I-96 | A report naming a pull request names the head commit it is about | `report.record` | implemented |
| I-97 | A shortened message names what it dropped and where to read it; a budget too small to hold the required parts refuses | `report._compose` | implemented |
| I-98 | An event with no work report renders the pre-contract message unchanged | `delivery._render_completion`, `delivery._render_revision` | implemented |
| I-99 | A wait result never authorises a re-run, and a bare timeout is neither failure nor success | `cxc.classify_wait` | implemented |
| I-100 | A report reads its relationship, generation, revision and outcome from the stored event; no caller supplies them | `report.record` | implemented |
| I-101 | A correction names the criteria the recorded verdict names; a review only adds notes and anchors, and anything it raises alone is labelled | `report._finding_lines` | implemented |
| I-102 | A shape or length that could only fail at render time is refused at record time, because rendering runs inside the delivery claim | `report._check_evidence`, `_check_unresolved`, `_bounded` | implemented |
| I-103 | A submission already frozen into a delivered attempt cannot be replaced in place; one never sent stays correctable | `report._assert_resubmission` against `attempt_report_submissions` | implemented |
| I-104 | An omission notice is placed before the final verdict, so an elided correction still ends on its judgment | `report._compose` | implemented |
| I-105 | A revision request cannot carry a PASS verdict | `report.record` | implemented |
| I-106 | A report-backed message keeps the receipt manifestRef the pre-contract message carried | `report._manifest_lines` | implemented |
| I-107 | The command an omission notice names returns the whole report, so every elided field stays recoverable | `cli.cmd_show` | implemented |
| I-108 | A restore section naming a skill owner nobody has is refused at record time | `report._check_restore` | implemented |
| I-109 | The frozen-manifest pointer is its own section, so shortening the file listing never drops it | `report._manifest_ref_lines` | implemented |
| I-110 | Every malformed report shape is a named refusal, never a host exception from the validator itself | `report._check_restore`, `_check_evidence`, `_check_unresolved` | implemented |
| I-111 | Every report field that lands on a line the composer cannot shorten is length-bounded at record time | `report._bounded`, `_bounded_optional` | implemented |
| I-112 | A collection field that is not an ordered sequence is refused rather than iterated, so a mapping never becomes a list of its own keys and a string never becomes a list of characters | `report._sequence` | implemented |
| I-113 | Recording a later submission preserves the earlier one, so a recipient holding an older elided message can still recover what it promised | `report.record` keyed on (event, submission); `report.read_all`; `cli.cmd_show` | implemented |
| I-114 | Every delivered message states its report submission and survives elision doing so, so the frozen bytes identify which stored submission produced them | `report.render_completion`, `render_revision`; the identity is its own section with a floor covering it | implemented |
| I-115 | A report with no pull request still renders its base, head and criteria digest rather than dropping them unannounced | `report._commit_lines` | implemented |
| I-116 | An attempt that is proven never to have sent is not counted as a delivered submission; anything unproven is | `report._may_have_reached` | implemented |
| I-117 | A manifest reference too long to render is truncated visibly rather than making the event unsendable | `report._manifest_ref_lines` | implemented |
| I-118 | A submission number is a positive integer or a named refusal, never a coerced one, because it is half the identity and is printed in frozen bytes | `report._submission` | implemented |
| I-119 | An exit code is an integer or absent, so evidence a reader cannot interpret is refused rather than delivered | `report._exit_code` | implemented |
| I-120 | An inbox-only attempt counts as having reached the recipient, because its frozen message is the durable inbox item | `report._may_have_reached` | implemented |
| I-121 | No report value, top-level or nested, may contain a line break, so nothing can splice an extra line into the message protocol | `report._single_line`, applied to fields, evidence, unresolved, findings and restore | implemented |
| I-125 | A blocker count too large to render is refused, because it lands on a line the message cannot shorten | `cxc.verdict_line` | implemented |
| I-126 | A required report field must be text, not a value coerced through `str()` into a Python repr | `report._required` | implemented |
| I-127 | A line break is anything `str.splitlines` treats as one, so a separator other than CR or LF cannot splice a line either | `report._single_line` | implemented |
| I-128 | A correction renders the unresolved items the report marked open, rather than storing them unseen | `report.render_revision` | implemented |
| I-129 | A pull request number outside what the store can hold is refused, not left to raise `OverflowError` on insert | `report.record` | implemented |
| I-130 | A finding disposition is one of the frozen criteria values, checked rather than passed through | `report._disposition` | implemented |
| I-131 | The verdict parser and the verdict renderer accept the same language, including the blocker ceiling | `cxc.parse_verdict_line` | implemented |
| I-132 | Every producer-supplied integer is inside what the store can hold, and its refusal never tries to print an unprintable value | `report._submission`, `report.record` | implemented |
| I-133 | A blank evidence entry is refused, because an empty verification line is not verification | `report._check_evidence` | implemented |
| I-134 | A finding that exists only to enrich an authoritative one needs no disposition of its own | `report._disposition` | implemented |
| I-135 | A line value that cannot be encoded as UTF-8 is refused where it is recorded, not where it is measured or sent | `report._single_line` | implemented |
| I-136 | A correction carries the CXC status and its reason, like a completion does | `report.render_revision` | implemented |
| I-137 | One criterion carries one finding; a duplicate id is refused rather than silently replacing the first | `report._check_review` | implemented |
| I-138 | Shortening carries a running byte total rather than recounting, so it stays linear inside the claim transaction | `report._compose` | implemented |
| I-139 | Fixed protocol prose is never shortened away, because the advertised command returns records and not template text | `report._preserve_lines` | implemented |
| I-140 | Every producer-supplied text field is required to be text, never coerced through `str()` into a representation of itself | `report._required`, `_text_or_none`, and the evidence, unresolved and finding checks | implemented |
| I-141 | An exit code is a number a process could have exited with, so it can always be serialised | `report._exit_code` | implemented |
| I-142 | An unrenderable receipt manifestRef is reported as present rather than blocking the delivery, because the receipt is contract-validated and the recipient is not at fault | `report._manifest_ref_lines` | implemented |
| I-122 | Pull-request fields are refused when no pull request is named, rather than stored and never rendered | `report.record` | implemented |
| I-123 | Every restore field is a supported, bounded, single-line string; an unsupported or unrenderable one is refused | `report._check_restore` | implemented |
| I-124 | A submission must clear both floors, the delivered one and the highest stored one, so no write is accepted that nobody would ever see | `report._assert_resubmission` | implemented |


## Recorded limits, so a row above is not read as more than it is

| Limit | Consequence |
|---|---|
| Ordering is not lineage | a later turn is admitted only by an explicit continuation record; host ordering corroborates and can contradict, never admits |
| Byte stability is enforced only under a read lease | the lease is opt-in because holding one blocks writers for the kernel lease-break timeout; otherwise each detector has a named evasion |
| Inode ownership is not proven | a hardlink or bind mount can expose the same bytes under another authorized path, which the contract permits because it authorizes paths |
| The JSON date-time format is unvalidated | the available validator has no working format checker, so timestamp format is unverified rather than implied |
| Terminal turns are polled, not subscribed | the transport cannot subscribe, so automatic invocation is a bounded poll that then dispatches |
| Transport isolation is per recipient, not per call | the adapter dispatches each submission as its own task and allows one send in flight per recipient, so a stall no longer reaches a different recipient. What is NOT bounded by the caller's budget is how long an abandoned send goes on holding its own recipient: it runs to the transport's own deadline of four times the RPC timeout, because `rpc.py` awaits the websocket write outside its response timeout and cancelling a send mid-flight is what produces an unknown outcome instead of a real one |
| A live process is not a working one | health is computed from staged age, anchor poll freshness and backlog; liveness is reported separately and never counted |
| Archive state can be unknown | an inconclusive listing withholds rather than guessing, and a later observation releases it |
| A head commit is not observable from here | the relay cannot watch a forge, so `assert_current` enforces generation on the delivery path and takes `head_sha` only from a caller that already knows the current head. A push that changes the declared manifest is structurally a new event, because the revision hash and therefore the event id change with it; a push that changes nothing declared is not, and `_check_resubmission` is what stops an old report standing for it silently |
| `work_reports` ships with its composite key | the schema is applied with `CREATE TABLE IF NOT EXISTS`, which never reshapes an existing table, so a store created from an intermediate revision of this change that used an event-only key cannot hold a second submission. No released version has this table, so there is nothing to migrate; a store built from such a revision is recreated rather than upgraded. The write itself no longer names a conflict target, so it does not depend on which revision created the table |
| A store is matched to its socket by provenance, not arithmetic | a hash cannot be inverted, so a store created under a spelling we cannot guess is findable only because it recorded which socket it serves. Stores record that from now on and selection asks them before creating a canonical database. A store created before that existed says nothing and is reported under `siblingStores` rather than adopted on a guess, because adopting the wrong store is worse than reporting an ambiguity |
| Ownership decisions are taken under the lock, not beside it | probing the daemon lock and then acting on the result are two operations, and a supervisor can start between them. `stop` and `disable` decide under the lock, and `stop` writes its final record only while holding it: holding it is the proof that nothing is running and nothing can start, and failing to take it is the answer that someone is there. What this does NOT give is mutual exclusion with a supervisor that is already running - that is ownership, decided from the record - and it does not cover the handoff in the three rows below |
| `lock_is_held` answers False when it cannot open the lock file | it is a probe rather than an acquisition, and an `open()` failure - a mode or ACL change on `daemon.lock` - is reported as not held. So `service status` can advertise a free lock that `service start` then cannot take. `daemon_lock_if_free` distinguishes contention from operational failure; this probe does not |
| `disable` writes the shared intent without the lock when the holder looks like ours | when acquisition fails and the record classifies as this installation's, the intent write happens outside the lock. A replacement can acquire the lock in that window, and the write then lands on a supervisor this command never examined, which obeys it at its next worker boundary. `enable` has the same shape. The refusal paths are unaffected; this is the accept path |
| `start` confirms from a record and a separate lock probe | it reads a record matching its own launch id, then probes the lock, then reports success - and a replacement can hold the lock while that record still describes the launch which published it. It also reports before re-checking child exit, so a supervisor that published readiness and then died can be reported as started |
| These are forward fixes | per-assignment settlement does not restore claims a previous global settlement already suppressed, and capped-state annotation does not reach deliveries whose generation advanced before it existed. Historical repair is separate work with its own evidence |
| A re-review is not re-synchronised when it lands on the same disposition | `sync` derives `sync_id` from target, ref, kind, relationship, event, generation, revision and verdict, and enqueues with `INSERT OR IGNORE`. The criteria digest is not part of that identity, so a re-review that rules `verified` a second time produces the same id and no second job: the coordination document keeps the summary written against the earlier wording. A re-review that changes the disposition does enqueue. The local record is complete either way - `verdict_context.set_digest` carries the set actually ruled on and the `verdict_superseded` journal entry carries both digests - so this is a gap in what is pushed outward, not in what is known |
