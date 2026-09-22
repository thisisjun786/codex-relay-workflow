# Managed marker, completion state, and hook adjudication

Read when deciding whether a Codex hook may conclude that a managed assignment finished, or when
implementing the marker, disposition, and blocking behaviour that later issues install. This
reference DECIDES those rules; it installs nothing. No hook is registered by this contract, no CXC
state is read or written, and no Linear write happens from a hook. The one run that did register
hooks, recorded in the host-verification packet, did so inside a throwaway `CODEX_HOME` that was
removed afterwards, and left nothing installed.

Ordinary `crw-run` does not need any of this. Without a managed marker a session is an
ordinary session and every rule below is inert.

## Stop is a turn end, and the host proves it

The host's Stop input schema declares `last_assistant_message` required, so a hook is handed the
sentence "I finished" and must not act on it. A captured invocation delivered that field, so this
one is watched rather than deduced; most of what this document says about the host is still read
from the binary at rest, and the two are kept apart under the static observation heading below.
Natural-language completion wording is never evidence, and neither is a Stop event on its own, since
Stop is the host's response-turn boundary rather than a completion signal.

Completion that authorises parent verification requires three records from three different
producers, none of which is prose and none of which is a turn boundary:

| Producer | Record | What it establishes |
| -- | -- | -- |
| The child | A turn disposition of `ready_for_review` for the current `turn_id` | The child's own declared intent, not inferred from its text |
| The child, verified by the consumer | A relay receipt naming this session, this turn and this assignment's relationship, whose manifest digest is recomputed against the bytes on disk | That named artifacts exist with the claimed content, for this declaration rather than some other one |
| The host | `turn_status` of `completed` for that turn, observed through the relay's existing turn observation | That the turn ended normally rather than failing or being interrupted |

The identity on these records is load-bearing, not decoration. A receipt from an earlier turn, or from
another session, can still point at the current head while the turn being judged produced nothing;
accepting it would let one producer's record stand in for another's and quietly defeat the agreement
the three rows are for.

**One rule governs every identity comparison in this contract.** Two records name the same session or
the same turn only when both name it as a nonempty string. Missing, empty, blank and non-string
identities all name nothing, and two records that name nothing are never a match. Stated negatively,
because that is the form the rule gets violated in: `None == None` is not evidence.

| Comparison | What an unnamed identity means |
| -- | -- |
| A turn disposition against the current turn | The disposition is not this turn's, so this turn declared nothing and reads `undeclared_turn_end` |
| A readiness receipt against the current turn and the selected assignment | The receipt is unmatched, so readiness stands unreceipted |
| A claim against the session being judged | The claim names no verified owner, so it selects no assignment. A claim whose `dispatchRequestId` names nothing correlates with no assignment either, so after a bind it reads `claim_uncorrelated` and the turn is released |
| A bind record against the session being judged | Nothing can be shown to be the bound child, so the state is `bound_identity_unnamed` and the turn is released, never held |

The disposition comparison is the load-bearing one because it is upstream. A disposition admitted on
matching absences supplies the outcome every later rule reads, and an admitted releasing outcome ends
the hold outright, so tightening the receipt alone would leave the wider hole open.

**Where the identity comes from matters as much as whether it is named.** Every writer publishes
inside a directory it owns, so the path a fact sits at is an identity the filesystem enforced, while
the body is text the writer chose. The two are not interchangeable. A reader therefore takes a
claim's owner from its path and requires the body to agree, and reads a disposition at the path its
own Stop identity derives rather than searching for a body that matches. `factId` is the path the
reader walked to, assigned by the reader; a `factId` copied out of a body would prove nothing.

A body that contradicts its own location voids the fact **as an identity** without deleting it **as a
fact**. The two questions are separate and the separation is load-bearing. A competing claim asked
"whose is this?" answers nothing usable, so it selects no assignment and correlates no session. Asked
"is there unadjudicated evidence here?" it answers yes, and it keeps the assignment contested until a
resolution names it. Collapsing the two would hand a competitor the cleanest possible exit: write a
claim, contradict its own path, and disappear from the contested set by being void.

All three must agree. The relay already refuses a reviewable claim on a turn the host reports as
failed or interrupted, and already recomputes the digest rather than trusting it. This contract
adds the first row and leaves the other two where they are.

## The managed marker

A hook that only inspects registered relationships cannot see a missing registration. The marker
therefore exists before the relationship and is checkable without asking the relay anything.

The marker is an **assignment directory**, not a record anyone rewrites. It lives at
`<marker root>/sha256(realpath(workspace))/<assignment id>`, where the assignment id is the
`dispatchRequestIdHash` its own intent publishes. A hook holding only the delivered `cwd` hashes that
path, lists the one directory it names, and then reads an intent and probes a claim path per
assignment listed there. That cost is worth stating plainly rather than calling it a single lookup,
but it is still local: no relay query, no database and no network.
Inside each assignment, every fact is its own create-once file. Nothing is ever updated in place, so no writer can
regress or clobber another's fact, and a reader that arrives mid-race sees a subset of files, which
is always a valid earlier state rather than a corrupt one.

**A workspace path outlives the assignment that used it**, which is why the assignment owns a level of
its own instead of the workspace hash directly. A reused worktree would otherwise resolve to the
previous assignment, and because every fact there is create-once the new coordinator's `intent.json`,
`bound.json` and `relationship.json` all fail `EEXIST` while the new child reads the previous
session's bind and releases. The undeclared turn that is held on a fresh workspace then goes unheld on
a reused one: this contract's detection gap, reappearing through path reuse. Keying on the dispatch
request id hash means two coordinators sharing one dispatch collide exactly as they do today, and two
different dispatches never collide at all. The child does not search for its directory either: it
holds the dispatch request id preimage, so it computes the same hash the coordinator used.

Selecting among the assignments listed under one workspace hash:

1. An assignment holding `claims/<this session>/claim.json` is this session's assignment, where
   "this session" is read from the claim's path and the body is required to name it and agree. The
   path proves who could have written the record; only the body says the writer meant to claim the
   assignment, and an assertion nobody made must not be inferred from a directory that happens to
   exist. A child stays
   with the assignment it claimed, so declaring a later assignment for the same path can neither
   release a still-running earlier child nor make it read as somebody else's. Selecting on the body
   instead would hand that protection to the attacker it protects against: a later assignment's
   child may write inside its own claim directory, so it could name an earlier session there and
   capture that session's Stop. Should a session somehow hold claims in more than one assignment,
   the newest intent among them wins, ties broken on assignment id.
2. Otherwise the newest `intent.declaredAt` wins, ties broken on the assignment id so that every
   reader of the same listing selects the same one.
3. An assignment with no published `intent.json` is not selectable. It is a directory someone is
   still building, and skipping it leaves the reader on a valid earlier state rather than on nothing,
   which is the property every other create-once fact already has.
4. No selectable assignment means the workspace is `unmanaged`.

Keyword matching never creates a marker. A session becomes managed because a coordinator declared an
intent for its workspace, and for no other reason.

### Who may write what

| File | Writer | Contents |
| -- | -- | -- |
| `intent.json` | Coordinator | `dispatchRequestIdHash`, issue, workspace realpath, criteria source, baseline revision, authorised settings, `declaredAt` |
| `attempts/<n>.json` | Coordinator | `outcome` of `accepted`, `unknown` or `failed`, optional `taskId`, `at` |
| `bound.json` | Coordinator | `sessionId`, `taskId`, `at` |
| `relationship.json` | Coordinator | `relationshipId`, `at` |
| `resolutions/<n>.json` | Coordinator | `chosenTaskId`, `chosenSessionId`, `reason`, `at`, and `adjudicated`, the list of `{factId, digest}` entries it covers |
| `conflicts/<n>.json` | Coordinator | `attemptedSessionId`, `attemptedTaskId`, `loserProcess`, `at`, written by a binder that lost the publication race |
| `claims/<session_id>/claim.json` | Child session | `dispatchRequestId` preimage, `sessionId`, `firstTurnId`, `at` |
| `dispositions/<session_id>/<turn_id>.json` | Child session | `sessionId`, `turnId`, `outcome`, `at` |
| `hook/<session_id>/<turn_id>/<seq>.json` | Child's hook | `observation`, `decisionState`, `held`, `sessionId`, `turnId`, `at` |

Every fact carries `factId`, its own create-once relative path. Readers use that id and never a list
position: prepending a fact would otherwise silently re-point an existing adjudication at evidence
nobody reviewed.

Every writer publishes inside a directory it owns, never as a bare file in a directory it does not.
That is why a claim lives at `claims/<session_id>/claim.json` rather than `claims/<session_id>.json`:
publication needs to create a sibling temp file next to its target, so the writable unit has to be the
directory, not the single name.

The hook record is per observation, not per turn. One turn can be observed more than once, because a
hold produces a continuation and another Stop, and because a turn can be seen before the bind and
again after it. A single create-once file per turn would let the first observation consume the only
name available and silently lose every later one, so the sequence number is part of the identity.

**Binding is coordinator-only. The child claims but never binds.** Only the party holding the
creation receipt can tell "the task I created" from "a session that read an id", so only it writes
`bound.json`. A child writes a claim, which correlates a session to an intent and confers nothing
else. The consequence worth stating plainly: a hold is only ever possible downstream of a coordinator
write, so a child cannot put itself into a holdable state through the decision logic. That is a
property of the decision, not of the filesystem. Whether a child can instead reach the holdable state
by writing where it should not be able to write depends entirely on the isolation described under
permissions, which is not enforceable everywhere. Where that isolation cannot be enforced, holding
must not be activated at all; see the activation prohibition there.

**Both writes are required before a turn can be held**, and the unconditional release on an unclaimed
marker means exactly this: the coordinator's bind alone does not make a session holdable, because the
coordinator can publish the bind and the relationship before the child publishes its claim. Until the
session's own claim is there, its turns read `marker_unclaimed` and are released and recorded. The bind
says who the coordinator believes the child is; the claim is the session saying so itself, and a turn
is only ever held against a session that has said it.

**The claim has to be this assignment's claim, on both sides of the bind.** Correlation is a chain
of three links and all three are required: the claim's `dispatchRequestId` preimage hashes to the
intent's `dispatchRequestIdHash`, and that hash is the assignment the intent was published under.
The third link is what makes the first two mean anything. Both facts inside the marker are writable
by the parties publishing there, so an intent naming a foreign dispatch and a claim agreeing with it
correlate with each other perfectly while correlating with nothing the coordinator dispatched. The
assignment is the directory name and the directory name IS the hash, so it is the one link no writer
inside the marker chooses.

Before the bind a broken chain reads `dispatch_uncorrelated`; after it, `claim_uncorrelated`.
Both are released and recorded, and the second is answered apart from `marker_unclaimed` because
the two clear differently: an unclaimed marker is the bind-before-claim race and ends the moment the
child publishes, while every fact in this chain is create-once, so a claim already standing at
`claims/<session>/claim.json` with the wrong dispatch request id can never be replaced by the
correct one. Recovery is therefore adjudication or superseding the relationship, not repair in
place. The record carries which link broke - `claim_dispatch_unnamed`, `claim_dispatch_mismatch`,
`intent_dispatch_unnamed` or `intent_assignment_mismatch` - because they are settled differently,
and it carries `pendingObservation` as well, since this answer replaces a classification the
coordinator still needs. A claim whose body contradicts its path is not this session's claim at all
and still reads `marker_unclaimed`; a preimage that is not a string is `marker_malformed`, because
shape is answered before correlation. A declared releasing outcome is read first, so this check only
ever turns a would-be hold into a release.

**Selection asks which assignment, not whether it holds together.** A workspace outlives the
assignment that used it, so several assignments can sit under one path and the reader consults this
session's claim before recency to keep a running child with the assignment it claimed. That claim
has to name the assignment it sits in. Read on the claimant alone, one uncorrelated file written
into a NEWER assignment selected that assignment, the decision path refused it, the turn released,
and an older assignment the session was correlated, bound and registered under never had its
omission looked for - so a check that only releases within one assignment removed a hold across two.

The selection test is the claim against the DIRECTORY and never against the intent: the path
authorises the owner, the body confirms the writer meant it, and hashing the preimage says which
assignment the claim belongs to. Selecting on the intent's content instead drops a candidate whose
intent cannot be read, and since an unreadable declaration sorts below every real one, the reader
lands on an older sibling and holds a turn against it while nobody is told the assignment this
session actually claimed could not be read. Among the assignments a session's own claim selects,
unreadable or malformed facts therefore outrank readable ones, and the turn reports
`state_unreadable` or `marker_malformed` on the assignment that carries them. That preference is
scoped to the claimed set: applied across the workspace, one stale corrupt directory nobody is
using would outrank the current assignment and switch detection off. When no claim selects
anything, recency decides exactly as before.

The pre-bind window is not blind. A correlated session whose bind has not landed reads as
`correlated_unbound`: released, never held, but its hook still records the turn's observation. When
the bind lands, the coordinator folds those records and sees an undeclared first turn it would
otherwise have missed. The window therefore costs at most one deferred continuation prompt, never a
detection.

The dispatch request id is stored in `intent.json` only as `sha256`. Storing it in the clear would
make correlation meaningless, because any session able to read the directory could then present it.
The child holds the preimage from its own dispatch and writes it into its claim.

### Writing a fact

Every file above is created the same way: write `<target>.tmp.<pid>.<rand>` in the same directory,
fsync it, `link()` it onto the target, unlink the temp. `link()` is atomic and fails `EEXIST` when the
target exists, which makes first-publication-wins an operating-system fact rather than a convention a writer
might forget.

The failure modes are what recommend it. A writer that dies mid-write leaves an orphan temp and no
target, so readers simply see the fact as absent. Two writers racing produce exactly one `EEXIST`, so
the loser learns synchronously that it lost and re-reads instead of silently discarding the winner's
update. No invariant is allowed to span two files, which is why `bound.json` carries both ids inside
itself.

Two alternatives were rejected on their failure modes rather than on taste. A version counter with
whole-record rewrite is not a compare-and-set at all: two writers reading version 3 both write
version 4, and the second silently discards the first, so a lost update looks exactly like success.
An append-only log folded by readers is safer, since nothing is lost, but POSIX does not guarantee
that concurrent appends are untorn, a writer dying mid-append poisons the fold, and write-once
degrades from an enforced fact into reader logic every reader must implement identically.

### The assignment state is derived, never stored

Storing a state field means every writer must agree on transition rules and every delayed writer is a
regression risk, which is precisely how the previous ordering model failed. The state is computed
from which files exist, in this precedence:

| # | Condition | State |
| -- | -- | -- |
| 1 | `bound` and `relationship` | `relationship_registered` |
| 2 | `bound` | `identity_bound`. A hook on the bound session reads this as `managed_unregistered` unless that turn declared one of the releasing outcomes, which is read first and releases. Readiness is not among them here: with no relationship published there is nothing to tie a receipt to, so a receipted readiness is unmatched and the turn is held like any other unregistered Stop |
| 3 | No `bound`, and two or more distinct accepted `taskId`s or two or more claims, and no valid resolution | `ambiguous_identity` |
| 4 | No `bound`, and the clock is past `declaredAt + 30 minutes` | `intent_expired` |
| 5 | No `bound`, an `unknown` attempt, no accepted attempt, and no valid `resolution` | `creation_unknown` |
| 6 | No `bound`, and any accepted attempt | `creation_accepted` |
| 7 | Otherwise | `intent_declared` |

Expiry is anchored on `declaredAt` and on nothing else. Any acceptance-derived anchor can be moved
later by a fact that arrives later, which would let an already-expired intent revive, and an anchor
that never moves is the only way to keep expiry monotonic without storing a state nobody may rewrite.
It also means a `creation_unknown` intent expires normally, which an acceptance anchor could never
reach, since acceptance is exactly what never arrived. Expiry is evaluated before `creation_unknown`
and `creation_accepted` and after ambiguity, because an unresolved identity needs a decision from the
coordinator whether or not the window has closed.

### What a resolution adjudicates

A resolution is scoped to the evidence it names. It carries `adjudicated`, a list of `{factId,
digest}`, and a fact is covered only when both match: the id selects the fact, the digest confirms
the content equals what was recorded. A resolution naming nothing adjudicates nothing. Absent fields in legacy records confer no
identity or coverage. A present null or wrongly typed chosen identity or adjudication list is
malformed and is released with an observation before decision derivation.

The digest is SHA-256, hex encoded, over the fact's JSON with `factId` removed, serialised with
sorted keys, no whitespace between tokens, non-ASCII escaped, then encoded UTF-8. In Python that is
`json.dumps(payload, sort_keys=True, separators=(",", ":"))`. The exact spelling matters because an
independent writer must reproduce it byte for byte: hashing the same fact with literal Unicode
instead of escapes, or leaving `factId` in, yields a digest this reader rejects. Reproducible
vector: the fact `{"factId": "conflicts/0", "at": "2026-01-01T00:06:00+00:00"}` digests the payload
`{"at":"2026-01-01T00:06:00+00:00"}` to
`30250e28118e703a042e74d53844e078bbd318ae45a4479eac217c385a5c284a`.

Coverage never reads timestamps. Publication order is not timestamp order, so a fact published after
a resolution can carry an earlier or equal `at` and would otherwise be covered with nobody having
reviewed it. That would hand a child a way to clear its own contest, which it must never have.

A fact carrying no `factId` can never be covered. That denial is deliberate, and its recovery is
escalation rather than softening: create-once records cannot be repaired in place, so the assignment
stays contested and the coordinator settles it outside the marker by superseding the relationship.
Every softening of this rule is a way for unreviewed evidence to disappear.

Resolutions accumulate. A later adjudication is another file, never a rewrite of an earlier one, so
each keeps the scope it was published with. Coverage does not depend on their order.

Before a bind, an adjudication clears ambiguity only when the applicable resolutions agree on one
identity and together cover every ambiguous fact. Identity means the whole task and session pair;
agreeing on the task while differing on the session is disagreement, not a decision, and ambiguity
survives it. Nothing selects the first or last record implicitly.

Both halves of that identity must name something the record really contains, and the task half is
stricter than it looks: it must come from an `accepted` attempt. A `failed` or `unknown` attempt
may carry a `taskId`, but does not establish an accepted creation. Naming that task alone cannot
resolve ambiguity, even when the resolution covers every fact. The session half comes from a claim.

**What this does not guarantee.** A digest rejects different content. It cannot distinguish content
that was already published from identical content published afterwards, because the record carries
no publication order. A coordinator that prepares a competing fact, publishes a resolution naming
it, and only then publishes the fact produces an adjudication covering evidence nobody read, and no
reader can detect that from the files. The guarantee is therefore exactly this and no more: a
resolution covers the facts it names whose content matches the digest it recorded. Closing the
remaining gap is a writer obligation, listed with the other unenforceable premises below.

Because the derivation is monotonic in file presence, a late write cannot move the state backwards.
An acceptance recorded after a bind lands as a record and changes nothing. Ordering is never assumed,
so an interleaving the old model had no rule for now simply has no special case.

After a bind, a competing fact does not rebind. An extra claim, an accepted attempt naming a
different `taskId`, or a conflict record sets the annotation `identity_contested` for as long as no
applicable resolution covers it. The coordinator reads that and withholds a verdict; the hook's
decision does not change, because the bind winner stands. Only a resolution naming the bound identity
is applicable after a bind: one choosing a competitor must not authorise a verdict merely because it
covers that competitor's evidence. Identity is immutable under every writer: `bound.json` is
create-once, so a second bind attempt fails `EEXIST` and `bound.sessionId` cannot change no matter
who writes next.

`managed_unregistered` is managed. An ordinary session with no marker is not. The two are never
conflated, and the second is never blocked: having no relationship record is a reason to ignore a
session, never a reason to hold it.

### Traces

Deterministic interleavings, with T0 as the contrast. These are the cases that behave differently;
permutations that behave identically are not listed. Each has a fixture under
`../scripts/fixtures/decisions`, and the ones whose reading changes over time additionally have a
sequence fixture asserting the progression rather than one snapshot.

What those fixtures establish, and what they do not: they assert the derived reading a hook would
produce from a given set of facts, and for sequences the transition between readings. They do not
execute the write protocol. No fixture creates a file, races a `link()`, or kills a writer, so the
atomicity argument above is reasoned from the system call's documented behaviour and is not tested
here. Treat the write protocol as specified-but-unexercised until an implementation tests it.
`../scripts/fixtures/decisions`.

| ID | Interleaving | Derived state, and what the hook does |
| -- | -- | -- |
| T0 | Normal: intent, accepted, bound, claim, relationship, then turns | `intent_declared`, `creation_accepted`, `identity_bound`, `relationship_registered`. A Stop landing at `identity_bound` reads `managed_unregistered` |
| T1 | Pre-response child: intent, then the child claims and ends a turn, then acceptance and bind arrive | At that Stop the state is `intent_declared` and the hook reads `correlated_unbound`: release, and record the observation. After the bind the coordinator folds that record and sees the undeclared first turn |
| T2 | Duplicate acceptance: two accepted attempts naming different task ids, no bind | `ambiguous_identity`, withheld until the coordinator writes `resolution` and then `bound` |
| T3 | Delayed acceptance after bind: an `unknown` attempt, reconciliation, bind, then a late `accepted` | `creation_unknown` then `identity_bound`. A late attempt naming the same task changes nothing; a different task sets `identity_contested` and the bind stands |
| T4 | Concurrent bind by two coordinator processes | One `EEXIST`. Same value is a no-op. A different value means the loser writes a `conflicts/<n>.json` record naming what it tried to bind, because writing nothing would leave the contest invisible to every later reader, and the assignment stays `identity_contested` until a resolution naming the bound identity covers that exact record |
| T5 | Late registration: bound, several turns, then `relationship` | Each Stop before it reads `managed_unregistered`, held within bounds and then `unresolved_handoff`. Reversed, with `relationship` before `bound`, a correlated session still reads `correlated_unbound` and releases |
| T6 | Uncorrelated occupant, before and after bind | `dispatch_uncorrelated` before, `marker_claimed_by_other_session` after. Released both times, never held |
| T7 | Second claim after bind | The new session reads `marker_claimed_by_other_session`; the assignment is `identity_contested`; the winner stands |
| T8 | Expiry, then a late claim | `intent_expired` by the reader's clock. The claim leaves it unbound and `correlated_unbound`; only a coordinator bind changes that |
| T9 | Immutable identity: after bind, a delayed write of any other fact and a second bind attempt | Other facts land as their own files; the second bind fails `EEXIST`; `bound.sessionId` never changes |
| T10 | A writer dies mid-write | An orphan temp, no target, derived state unchanged. A retry either succeeds or gets `EEXIST` from whoever completed it |
| T11 | A workspace path reused by a later assignment | The new child resolves to its own assignment and its undeclared turn is held exactly as on a fresh workspace. Named by the workspace alone it read the previous session's bind and released instead |
| T12 | A published record that names no identity | A disposition naming no turn declared nothing, so the turn reads `undeclared_turn_end`; a bind naming no session reads `bound_identity_unnamed` and releases |
| T13 | A later assignment declared for the path while the earlier child is still running | The earlier child stays with the assignment it claimed. A newer intent neither releases it nor makes it read as somebody else's, which is why selection consults the claim before recency |
| T14 | A later assignment's child names an earlier session in its own claim body | The claimant comes from the path the write was authorised against, and a body contradicting its path voids the fact, so the earlier child keeps its assignment and its undeclared turn is still held. Selecting on the body released it instead |
| T15 | One session holding a current-head receipt earned under another assignment's relationship | The receipt names this session and this turn and is still not this assignment's, so readiness stands unreceipted |
| T16 | A competing claim that leaves its session blank | It competes anyway, because the owner is read from the path. Read from the body it vanished and `identity_contested` went false, which is the one thing that lets a verdict settle over unadjudicated evidence |
| T17 | A competing claim that writes the bound session's name into its own body | It competes anyway. This is the variant that defeats reading the path alone: a body contradicting its location voids the fact as an identity, and if voiding also deleted it the competitor would have found the cleanest exit of all |
| T18 | A published record that is not the shape a fact must be, such as a claim that is a bare string | `marker_malformed`: released like an unreadable store and recorded. Read straight through it ended the hook in a traceback, which records nothing at all, so one wrongly typed value switched detection off for the workspace |
| T19 | A session that left a path-only claim in an older assignment, whose own assignment has not published its claim yet | It stays with its own. Reading the path as an assertion captured it in the older one and released its undeclared turn as somebody else's marker; a claim naming nobody owns nothing, so selection falls through to the newest published intent |
| T20 | A store that could not be fully read, carrying a fact that is not a fact | Still `state_unreadable`, and still recorded. Deciding whether to summarise the marker by the state's name rather than by whether the marker is readable left this combination deriving from records that were not records, so the hook ended with nothing recorded at all |
| T21 | A hold count that is not a count, at the one boundary that reads them | `marker_malformed`. The counts are read only when a hold is weighed, so this is reachable nowhere else, and a reader counts its own hook records: a non-integer says the store it counted is wrong. Reported rather than read as zero, which would quietly hand back a full hold budget |
| T22 | The bind and the relationship published before the child's claim | `marker_unclaimed`: released and recorded. The coordinator holds the creation receipt, so it can bind first, and that race must not prompt a session which never asserted this assignment. A hold needs the child's own claim, not only the coordinator's bind |
| T23 | A resolution whose adjudication list, or an entry in it, is not a record | `marker_malformed`. The list is read to decide coverage, so validating the resolution and not its nested list left the same silent failure one level down |
| T24 | Present null counter record or count, or negative persisted counts | `marker_malformed`: release and record corruption. Only absent optional counters default to zero; corrupt counts cannot renew a hold budget |
| T25 | Array/object identities in either resolution form or an accepted attempt | `marker_malformed` before identity set construction; no traceback can suppress the observation |
| T26 | Present null chosen identity or adjudication list in either resolution form | `marker_malformed`, released and recorded; absent legacy fields still confer no identity or coverage |
| T27 | A releasing disposition with corrupt persisted counters | Preserve the declared disposition; counters are validated only when an omission hold is weighed |
| T28 | Bound and registered, with a claim naming a dispatch request id that hashes elsewhere; and the variant where a forged intent agrees with that claim | `claim_uncorrelated` both times: released and recorded, with the broken link in `claimEvidence` and the replaced classification in `pendingObservation`. The post-bind path asked only whether the claimant was this session, so an uncorrelated claim satisfied the hold precondition and the turn was held, indistinguishable from a correlated one. The variant is why the chain runs through the assignment: an intent and a claim agreeing about a foreign dispatch correlate with each other and with nothing dispatched. Kept apart from `marker_unclaimed` because a create-once claim carrying the wrong dispatch never clears itself, and apart from a declared outcome, which is still read first |

### Marker root and permissions, as an integration obligation

These are requirements this contract places on the operations contract, stated here because deferring
them to a document that has no marker-root interface would leave them unowned.

| Party | Read | Write |
| -- | -- | -- |
| Coordinator | The whole marker root | `intent.json`, `attempts/`, `bound.json`, `relationship.json`, `resolutions/`, `conflicts/` for its own assignments |
| Child session | Its own assignment directory | `claims/<own session>/`, `dispositions/<own session>/` |
| Child's hook process | Its own assignment directory | `hook/<own session>/` |
| Relay daemon | Nothing | Nothing |

Assignment retention is owned there too. Assignments accumulate under a workspace hash and nothing
here retires them, so a long-lived worktree makes every Stop pay to read one intent and probe one
claim path per assignment ever declared for it. The growth is slow and local, but it has no natural
bound and no owner in this contract, which is exactly the shape of a cost that goes unnoticed until
it is large. Retention belongs to whoever owns the marker root, and a reader must keep selecting on
published facts rather than on whatever pruning happens to have run.

The root sits outside the source checkout and outside the relay database, on a writable parent both
the coordinator and the child can reach. The daemon gets no access at all: nothing in the marker
feeds delivery, and the hook is already forbidden to read CXC state.

On a single-uid host, which is the configuration measured here, file permissions cannot separate
coordinator from child, and the directory name is derivable from any workspace path, so neither read
nor write isolation is achievable that way. Two consequences follow, and neither may be softened.

First, `link()` gives atomic publication, not immutable storage. It guarantees that the first writer
to publish a name wins and that the loser learns it lost. It does not stop an actor with write access
to the directory from later truncating or unlinking what it published. Immutability of a fact is
therefore a property of who can write, not of how it was written.

Second, the enforceable lever is the sandbox rather than the filesystem. A workspace-write task
cannot write outside its granted roots, so the coordinator grants the child exactly
`<root>/<assignment>/claims/<own session>/` and `<root>/<assignment>/dispositions/<own session>/`,
and the hook process `<root>/<assignment>/hook/<own session>/`. Each grant is a directory rather than
a filename, because publication writes a temp sibling before it links the target, so a filename-only
grant would forbid the very write it is meant to allow. Granting whole `claims` and `dispositions`
directories instead would let a child write another session's records, which is the contradiction
these narrower grants remove.

**Activation prohibition.** Where those per-session grants cannot actually be applied, this hook runs
in observe-only mode: it classifies and records, and it never holds. Holding depends on a child being
unable to forge the facts the decision reads, and that premise is supplied by the sandbox grant, not
by the decision logic. An implementation that cannot establish the grant and holds anyway is outside
this contract.

Correlation is replayable evidence, not authentication. The intent stores only the hash, but the
child's own claim necessarily stores the preimage, and on a single-uid host another session can read
that claim and copy the value into a claim of its own. Doing so confers nothing: binding is the
coordinator's, so a copied preimage produces at most a second claim, which is exactly the
`identity_contested` condition the coordinator must resolve. Read correlation as "this session
presented the id", never as "this session is the one that was dispatched".

Four limits belong on the record rather than being assumed away. A resolution must be published only
after its writer has read the facts it adjudicates and computed their digests from what it read;
nothing in the record enforces that order, so it is an obligation on the coordinator rather than a
property of the files. Whether a coordinator can set a
child's writable roots at creation is shown to exist as a mechanism but not shown to be settable by a
coordinator. Whether the hook process runs inside the task's sandbox is unobserved; if it does not,
its `hook/` write is unconstrained by that lever. And no filesystem race, crash, or recovery
behaviour described here has been executed, only reasoned about.

## Turn disposition

Every turn of a claimed managed session ends with a disposition recorded for that exact
`session_id` and `turn_id`. The vocabulary reuses the relay's existing outcome names and adds one:

| Disposition | Meaning | Completion obligation |
| -- | -- | -- |
| `in_progress` | Work continues in this assignment | None |
| `ready_for_review` | This generation's work is finished | A receipt at the current head revision naming this session, this turn and this assignment's relationship is required. Where the work is a pull request, the report that accompanies it also carries the merge-readiness handoff, because a finished generation whose review is still open is not finished |
| `blocked_needs_input` | Waiting on a person | None, and the turn is never held |
| `interrupted` | The user stopped it | None, and the turn is never held |
| `failed` | The attempt failed, with a reason | None. A failure is reported, not retried by a hook |

`in_progress` is the addition. Without it, "still working" and "finished but silent" are the same
observation, which is the gap this contract exists to close.

## The detection guarantee, and its boundary

The relay classifies a completed turn carrying no child receipt as an ordinary turn end. That
classification is correct and conservative, and it is also where two different situations collapse
into one reading: a child mid-progress and a child that finished and recorded nothing both leave a
completed turn, no receipt, and an ordinary Stop.

A positive disposition splits that single reading in two:

| Observation at Stop | State | Detected |
| -- | -- | -- |
| Disposition present for this session and `turn_id`, and one of `in_progress`, `blocked_needs_input`, `interrupted` or `failed` | `declared_` plus that outcome | Nothing to report |
| Disposition `ready_for_review`, no receipt at the current head naming this session, this turn and this assignment's relationship | `receipt_missing` | Yes |
| No disposition naming this session and this `turn_id`, including one that names no identity at all | `undeclared_turn_end` | Yes |

The hook never decides whether the child is finished. It decides whether the child said anything at
all, which is a fact about a record rather than about an intention, and then asks. The child answers
on the next continuation, and the ambiguity is resolved by the child rather than guessed by a detector.

**Bounded guarantee.** Detection covers a missing declaration and a declaration of readiness whose
receipt is absent. It does NOT cover a child that records `in_progress` while its work is actually
complete: a false declaration is indistinguishable from a true one, because nothing outside the child
knows the difference. That residual is accepted and stated rather than engineered away, and no hook
may report it as covered.

**Host dependency.** Attribution needs the Stop input to carry the session and turn identity. On a host
that delivers no turn identity nothing can be attributed to the turn, so every turn reads
`undeclared_turn_end` and is held until the bounds release it: one hold per turn, two per generation,
three per rolling hour, then `unresolved_handoff`. The cost is bounded rather than a loop, but it is
real. On the host in the packet below both identities are delivered, and the session identity is the
same value a dispatch receipt returns, so attribution works there. That is one host and one version;
the dependency stays stated because the next host has to be watched, not assumed.

**Malformed is its own answer.** An unreadable store reads as `state_unreadable` because "I could not
look" must never be reported as "there is nothing there". A record that is readable but is not the
shape a fact must be belongs to the same family and gets `marker_malformed`, ranked directly below
unreadable and above an absent marker, released and recorded rather than derived from. The bound this
places on the guarantee is worth stating rather than discovering: a writer who can corrupt the marker
can suppress the hold, exactly as one who can make the store unreadable already could, but not the
record. Reading straight through was worse than either, because a traceback leaves no observation, no
state and no row, and the turn behind it is indistinguishable from an ordinary end.

That guarantee holds only if nothing downstream reads the marker once it is known to be unusable, so
the rule is about the marker rather than about which state was reached: an unreadable store carrying a
malformed fact answers `state_unreadable` and must still be spared the summary. The counts a hold is
weighed against must be readable when an omission hold is considered; releasing dispositions do not
read or validate that budget. They are not host input; a reader counts its own
hook records, so a present count must be a non-negative integer (not null or boolean). An absent
optional count defaults to zero; a malformed count says the store is wrong and is reported
rather than read as zero, which would quietly hand back a full hold budget.

A hook never manufactures the missing evidence. It does not write a receipt, does not record a
verdict, and does not mark anything verified.

## The host hook surface, as statically observed

**Read this paragraph before any line below it.** Everything in this section comes from the
installed binary at rest: the JSON Schemas it embeds per event, and its own diagnostic strings.
Nothing in this section was obtained by running a hook, so where a sentence below states a
behaviour, read it as "the binary declares this" rather than "this was observed happening".
Schema-and-string evidence establishes what the host is built to accept and what it is built to
complain about, which is worth keeping separate from what it does. The packet at the end of this
section is the separate record of a scoped host run, and it is what settles criterion 3. Where the
two agree, say which one a claim rests on; where a claim needs the run, the static reading below
does not supply it.

Extracted from codex-cli 0.154.0 by reading the JSON Schemas the installed binary embeds for each
event, titled `<event>.command.input` and `<event>.command.output`. `hook_probe.py observe` in
`../scripts` extracts them, and `--sanitize` writes the shareable capability record kept under
`../scripts/fixtures/host`, beside the observation record the packet run produced. The two are
separate readings of the same host and are named separately for that reason. Re-run both on any
other host or version before relying on either; nothing below is assumed to hold elsewhere.

Twelve events carry schemas: `pre-tool-use`, `permission-request`, `post-tool-use`, `pre-compact`,
`post-compact`, `session-start`, `session-end`, `user-prompt-submit`, `stop`, `subagent-start`,
`subagent-stop`, and `interrupt`.

`stop.command.input` requires `cwd`, `hook_event_name`, `last_assistant_message`, `model`,
`permission_mode`, `session_id`, `stop_hook_active`, `transcript_path` and `turn_id`. The delivered
`cwd` is what locates the marker, and the delivered `turn_id` is what scopes the disposition.

`stop.command.output` accepts `continue`, `decision`, `reason`, `stopReason`, `suppressOutput` and
`systemMessage`. `decision` has exactly one legal value, `block`, and the host requires `reason`
when it is used. There is no `hookSpecificOutput` and no `additionalContext` on Stop, so the only
channel from a Stop hook back to the model is a block carrying its reason. The host also reports
`ignoring additionalContextLimit` for events that cannot emit context, which confirms the split
rather than implying it.

Exit codes are distinct from the JSON channel. Exit 2 is the blocking code, and the host reads the
explanation from stderr: for Stop and SubagentStop it is specifically a continuation prompt, and the
host complains when code 2 arrives with an empty stderr. Malformed stdout is rejected as invalid
output for that event rather than silently accepted. A hook that dies without an exit code is
reported as terminated, not as success.

The binary's own diagnostics read as a continuation request rather than a veto: it carries the string
`Stop hook requested continuation without a prompt; ignoring the block`, which is written for a case
where `decision: "block"` asked for another pass and no reason accompanied it. That is a deduction
from a message the binary can emit, not an observation of it being emitted. H2 and H3 settled the
behaviour it describes: a block carrying a reason continues the turn, and a block carrying none is
reported as a failed run and continues nothing.
`session-end` has an input schema and no output schema, so session end cannot influence anything and
is unusable as an enforcement point. Which events a host actually registers varies, and two signals
disagree by construction: `hook_probe.py observe` reports the distinct event names that declaration
files ask for and, separately, the event names the host recorded in its own hook trust state. On the
host measured here those were seven and nine. Neither is proof that a handler ran for a given
session, which only an observed invocation gives, so a count from either source is a floor.

Discovery-time handling is also visible: the host clamps an over-long `timeout`, runs an `async`
handler synchronously when the event requires it, ignores `additionalContextLimit` on events that
cannot emit context, and spills oversized hook output to a file it then names in a message. None of
these change a decision, but they mean a hook cannot assume its declared timeout or output size
survived unchanged.

**Absent from the static evidence.** Two independent passes over the installed artifacts found
nothing bearing on any of the following, and that is still true: the static surface does not say.
Each was settled instead by the run recorded in the packet below, so what follows is a map from a
silence in the binary to the observation that answered it, and each answer carries the run's scope
rather than the schema's generality:

- How the host combines several matching handlers for one event. Deny-wins, first-result and
  concatenation are each unevidenced in the static surface, as is the operative meaning of the
  traced `display_order` and `scope` fields. H4 watched it: concurrent execution, every result
  honoured, reasons concatenated in declaration-index order, and no observable effect from
  `display_order` set on a command entry.
- Whether a hook that times out, crashes, or writes invalid JSON fails open or fails closed. Only
  the host's failure *logging* was found, not its effect on the action. H5 watched the action: all
  three fail open.
- What the host sets `stop_hook_active` to, and whether the host itself suppresses hooks or caps
  continuations when it is true. The schema declares the field required on Stop and SubagentStop.
  H6 watched the flag: false on a turn's first Stop, true afterwards, and no suppression of the
  handler while true. Whether the host caps continuations at all is not settled and cannot be.
- Whether exit 1 is distinguished from a crash. H5 covered it too: not on this surface.

The design narrows the exposure rather than claiming immunity. Observation and holding are separate
steps: the omission is classified and recorded from the delivered payload and the marker, disposition
and receipt stores alone, so that classification does not depend on ordering, on another hook, or on
whether a hold is granted. Only the hold is exposed to the unknowns, and it degrades toward
releasing. The packet below removed two of those unknowns and left one standing. A hold is delivered,
and it does not need to land before or after another handler's, because every holding handler's
reason is carried in the same continuation rather than one winning. What still cannot be claimed is
that a continuation already in flight was requested by this hook rather than another, which is why
`stop_hook_active` is read only as "a continuation is running", true whoever asked for it. That
reading was chosen as the weakest assumption that still makes the flag useful, and H6 found the flag
behaving that way rather than as a host-side suppression switch.

## Host-verification packet, observed

Criterion 3 is settled. Every row below was watched on a running host rather than read out of the
binary, under a throwaway `CODEX_HOME` created for the run and removed after it. The user's own
Codex home was read and never written; its `hooks.json` hashed identically before and after. The
redacted record is `../scripts/fixtures/host/host-observation-codex-0.154.0.json`. `hook_probe.py
replay` reads the row ids out of the table below, requires a record to carry every one of them,
and holds the record to the capability record it names, so dropping a row here is a failed check
rather than a quieter packet.

Read every row as a statement about codex-cli 0.154.0 on one Linux host. Another host or version
has to be watched again, exactly as the static table does.

| # | Question | Evidence taken | What it showed | Status |
| -- | -- | -- | -- | -- |
| H1 | Which fields the host actually delivers to a Stop hook, and whether they match `stop.command.input` | One captured stdin payload from a real Stop invocation, field names and JSON types only | Exactly the nine field names the schema marks required, and no others. `stop_hook_active` arrives as a JSON boolean; the other eight are strings | Resolved |
| H2 | Whether stdout JSON with `decision: "block"` and a `reason` produces a continuation, and what the model receives | One blocking invocation, the turn it produced, and the transcript entry carrying the prompt | It continues the turn. The model receives the reason verbatim as a user-role message wrapping it in `<hook_prompt hook_run_id="stop:<index>:<declaration source>">`, and it answered the instruction the reason carried | Resolved |
| H3 | Whether exit 2 with stderr behaves as the same continuation channel, and how it differs from the JSON route | Paired invocations, one per route, plus both degenerate variants | The same channel, down to the wrapper and the `hook_run_id`: stderr becomes the fragment body and the next Stop carries `stop_hook_active` true. The routes part only when the prompt is missing, where a block with no reason and exit 2 with empty stderr are both reported as a failed run and neither continues | Resolved |
| H4 | How two matching handlers on one event compose: ordering, whether a hold from one suppresses the other, and what `display_order` does | Four registrations: two handlers in one matcher group, the same two in separate groups, one holding against one releasing, and the pair with `display_order` reversed | Both handlers start concurrently and both results are honoured. Reasons are concatenated into one continuation in declaration-index order, so neither deny-wins nor first-result describes it. A releasing handler does not suppress a holding one. Grouping is not observable, and `display_order` on a command entry changed neither execution nor delivery order | Resolved |
| H5 | Whether a hook that times out, crashes, or writes invalid JSON fails open or fails closed | Four invocations: a handler sleeping past its registered timeout, one killed with `SIGKILL`, one writing invalid JSON, and one exiting 1 | All fail open. Each is reported as a failed hook run, none requests a continuation, and the turn ends normally. Exit 1 is labelled the same as the crash, so it is not distinguished on this surface | Resolved |
| H6 | What the host sets `stop_hook_active` to, whether it suppresses handlers when true, and whether the host caps continuations independently | The delivered flag across a first Stop and its continuation; then a bounded run holding on every Stop until 10 consecutive continuations or 5 minutes, whichever came first | `false` on a turn's first Stop and `true` on every Stop after a continuation. The host does not suppress the handler while it is true: the hook ran on all 11 Stops. The run reached 10 consecutive continuations in 27.7 seconds and the count bound stopped it, not the clock and not the host. That is evidence about 10 continuations and nothing more; no run of any length shows the host has no cap | Resolved |
| H7 | Whether the `session_id` the host delivers to a hook is the same value as the task id a creation receipt returns | One real dispatch through this repository's bridge against a probe-only App Server, its creation receipt, and the Stop payload captured from that same first turn | The same value. The delivered `session_id` equalled the receipt's thread id, and the delivered `turn_id` equalled the receipt's turn id, which the question did not ask for | Resolved |

H7 was the load-bearing one. The coordinator writes a bound session id and the hook compares the
delivered `session_id` against it, so a different kind of identifier there would have failed the
marker's whole eligibility test closed. It is the same identifier, and the turn identity matches
too, so a disposition keyed on the delivered `turn_id` and a bind keyed on the delivered
`session_id` are both checkable against what a dispatch returns.

**What the packet still does not license.** A hold was observed being delivered, so the blocking
policy's bounds are no longer the only thing standing between a held turn and a loop; but every
bound in it remains this hook's own accounting, because the host was observed not to cap ten
continuations rather than observed to have no cap. Two facts sharpen the per-turn bound rather than
loosening it: a continuation does not start a new turn, so all eleven Stops carried one `turn_id`,
and the first continuation already carries `stop_hook_active` true. Under this contract's own rules
the hold therefore ends after one, twice over. Reporting a row here still requires citing the run,
not the static table above it: a report that cites the static table for any of H1 to H7 is
misreporting, and so is one that cites this table without the fixture behind it.

## Blocking policy

The decision is a pure function of the marker state, the delivered `turn_id` and `stop_hook_active`,
the disposition record, and the receipt record. It reads no CXC state, assumes no hook ordering, and
shares no mutable state with any other hook.

A hold is only ever issued at Stop, only for a claimed managed session, and only for
`undeclared_turn_end`, `receipt_missing`, or `managed_unregistered`.

| Bound | Value |
| -- | -- |
| Holds per turn | 1, counted by this hook against the delivered `turn_id`. A continuation was observed to keep the turn it continues, so every Stop of a held chain carries one `turn_id` and this count is what ends the chain. A delivered `stop_hook_active` of true releases as well, as corroboration rather than as the mechanism |
| Holds per relationship generation | 2 |
| Holds per session per rolling 60 minutes | 3 |
| Hook wall clock | 5 s self-imposed, against a 10 s registered host timeout. This bounds the hook process, not the continuation it asks for |
| Added latency budget | 2 s median, 5 s at the 95th percentile, per Stop |

Every bound here is self-imposed and must be enforced by the hook. A bounded run held ten consecutive
continuations without the host intervening, which shows the host does not cap at ten rather than that
it has no cap, so nothing here may lean on a host limit. The per-turn count is the hook's own record
keyed on the delivered `turn_id`, and that key was observed to be stable across a continuation chain.
The delivered flag is a second, independent reason to release, now observed to be false on a turn's
first Stop and true on every Stop after a continuation. Losing the count fails toward one extra hold,
which the generation and window bounds then catch.

Two things this hook cannot bound. It cannot limit how long the continuation it requested runs,
because the host owns that; the release owner is the next Stop evaluation, which sees that the turn
already spent its hold. It also cannot hold a turn it failed on: a hook that times out, crashes or
writes invalid JSON was observed to fail open, so a detector that dies detects nothing and the turn
ends as an ordinary one. That is the direction to fail in, and it is why the wall-clock budget sits
well under the registered timeout, but it means an unreliable hook degrades into no hook rather than
into a stuck session.

Other hooks hold independently, and the host was observed to honour every one of them: two handlers
holding the same Stop both had their reasons carried into a single continuation, and a handler that
released did not suppress one that held. The installed CXC Stop hook applies its own continuation
caps, so a turn can be held by more than one owner and the totals compound. This contract governs
only its own holds; it never inspects, relaxes, or counts another hook's.

Release is unconditional on any of: `stop_hook_active` true; a disposition recorded for this session
and turn whose outcome is one of `in_progress`, `blocked_needs_input`, `interrupted` or `failed`; a
`ready_for_review` disposition whose receipt at the current head names this session, this turn and
this assignment's relationship; no marker; an unclaimed
marker; a marker bound to another session; an unreadable marker, disposition or receipt store; and any
bound above being reached.

The list of outcomes above is exhaustive, not illustrative. An outcome outside that vocabulary is not
a declaration at all: it reads as `undeclared_turn_end` and is held like any other missing
declaration. Reading it as "anything that is not `ready_for_review` releases" would let a typo buy a
release, which is the one way this design could fail open.

When the generation or rolling-window bound is reached the hook stops holding and records
`unresolved_handoff`, which is an observable incomplete state. Nothing is reported as finished, and
nothing is retried silently. The per-turn guard and an in-flight continuation are different: they
record `hold_in_flight`, which says not right now rather than not converging.

Exhaustion is evaluated before the per-turn and in-flight guards, and deliberately so. Reaching the
generation or window bound is a terminal statement about the assignment, while "this turn already
held" and "a continuation is running" only say not right now. If the transient guard answered first,
an assignment that had stopped converging would keep reporting itself as merely busy.

User interruption always wins. `blocked_needs_input` and `interrupted` are released immediately,
because holding a turn that is waiting for a person is the one failure this policy cannot trade away.

## Decision criteria, fixed before implementation

The hook-on versus hook-off comparison is judged against these, fixed here so the comparison cannot
be tuned after the numbers arrive. Both omission cases are measured separately, because collapsing
them is precisely the error this contract corrects.

| Measure | Criterion |
| -- | -- |
| Missed-detection, state and receipt both absent | Every injected `undeclared_turn_end` is reported. Measured on its own, never merged with the case below |
| Missed-detection, receipt absent only | Every injected `receipt_missing` is reported, with the declared state present |
| Handoff success | A reported omission is followed by a real receipt and one parent verification, with no new user message |
| Wrong block | Zero holds on unmarked sessions, on `blocked_needs_input`, and on `interrupted` |
| Duplicate execution | No verification or correction runs twice for one event id across a hold, a daemon restart, or a recovery |
| Added latency | Within the budget above, reported as a distribution rather than a mean |

A count of tests, the presence of a registration, or a green hook run is not one of these.

The allow and hold table above is executable. Every row, plus the binding race, the release-precedence
combinations and the bound-reached cases, is a fixture under `../scripts/fixtures/decisions`, and
`hook_probe.py replay` checks each against its recorded expectation.

Replay also measures return-site coverage: it reads its own decision functions' return statements
from the module AST, traces which ones the fixtures actually execute, and exits non-zero naming any
that no fixture reached. The denominator is derived rather than declared, so a return added later
enters it whether or not anyone remembers. This is return-site coverage, not branch coverage, which
would need branch instrumentation. `--allow-unreached` reports the gap and waives only that gap; a
fixture mismatch is never waived.

Replay proves three things and no more: that this decision function agrees with expectations written
alongside it, that no return site in it goes unexercised, and that the recorded host observations
still cover every row the packet asks about and agree with the capability record each one names. All
three artifacts share an author, so this is a consistency check rather than independent evidence, and
the third checks a recording rather than a host: it re-runs no hook and would keep passing on a
machine where hooks are switched off. Whether a host invoked a hook, honored its output, delivered a
hold, persisted anything, or met a latency budget is settled in the host-verification packet and in
the measurement section, not here, and nothing here exercises a real creation race.

## Boundaries

The marker root, database location, daemon ownership, and workspace permissions belong to the
operations contract. Registering the hook, adding the relay's intent and adjudication commands, and
wiring `crw-run` to write markers are later issues. Watching the host settled how a hook behaves
once it runs; it installed none, so it moved none of those issues. The measured comparison is a
later issue too, and it reuses the criteria above rather than restating them.

A coordinator may prepare Git metadata, such as a branch in an assigned worktree, without widening
the child's permissions. Where the child's run is restricted enough that it cannot commit, the
coordinator owning commits and worktree retention is the **current fallback for that restricted
run**, not the general division of labour: the child owns source edits in its checkout, and a run
that can commit keeps its commits. Ownership of pull requests, review intake and the fixes that
follow belongs to the operations contract, which is where a capable child's delivery path is
decided; it is deliberately not restated here.

No child is ever given tighter settings merely so that this hook becomes able to hold it. Holding
depends on isolation the coordinator can actually grant, and where that isolation is absent the
answer is observe-only, never a narrower child. A permission change made to enable enforcement is
outside this contract.

Where a host supports event-driven return, a parent is woken by the delivery path rather than by
repeated polling, and a native subagent completion is not the same thing as an independent task's
relay handoff. No host is assumed to support automatic wake.

This contract does not modify CXC, does not change any other skill's reference, and does not
authorise a hook to write Linear.
| T29 | Two assignments under one workspace: the older correlated, bound, registered and owing a hold, the newer carrying a claim by the same session that names a foreign dispatch | The older one is selected and held. Selection consults this session's claim before recency, and consulting the claimant alone let one uncorrelated file select the newer assignment: the decision path refused it, released, and the older assignment's omission was never looked for, so one file switched holding off for a session correlated and bound elsewhere. Selection tests the claim against the directory, so the refusal stays scoped to the assignment that produced it. Two controls: the same shape with a correlating newer claim, where recency still moves the child on; and a newer claim that DOES name its assignment while that assignment's intent cannot be read, which is selected and reported rather than skipped, since selecting on the intent's content would hold the older turn with the unreadable store unmentioned |
