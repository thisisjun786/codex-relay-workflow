# Root and descendant thread subscription lifetime

The bridge and relay release the App Server root subscriptions they own after
finishing their useful observations and confirming the delivered turn ended.
A finished root remains resumable and is never archived by this path. The next
relay delivery uses `thread/resume` again. Delivery acceptance, turn completion
and subscription release are separate facts; release changes no receipt field
and does not make an uncertain send retryable.

## Connection and terminal ownership

`thread/start` and `thread/resume` subscribe the calling connection. Reads and
`turn/start` do not. A watch admits on an established socket; every subsequent
operation call, including the bridge's final optional read and relay guard,
stays on that socket. Loss fails that scope rather than reconnecting it. A new
delivery establishes a new connection through its ordinary read/resume path.
Release sends `thread/unsubscribe` on the original socket and never reconnects.
A retired reader cannot complete or retire a replacement socket's watches.

## The busy hold: a subscription kept for the recipient's idle edge

A delivery that finds its recipient mid-turn waits a doubling backoff, and that timer used to be
the only thing that ended the wait: the head of a parent's receipt line held every younger
delivery behind it for up to five minutes at a time. While such a head waits, the relay holds a
subscription on the recipient, so the App Server reports that recipient's `thread/status/changed`
to this connection, and a report of `idle` or `notLoaded` releases the head (CRW-904, section 82
of the CRW-781 decision).

The hold is `Client.HoldThread`: one `thread/resume` carrying only `threadId` and `excludeTurns`,
with no overrides and no settings, on a thread that is usually loaded already. It starts no turn
and changes nothing about the recipient, so a busy recipient is never interrupted. It admits no
watch, because an admitted watch holds the root's mutation gate until `Finish` and would block the
delivery's own send; instead the root is marked busy-held, which `ready` and `prune` treat exactly
as they treat the never-run-root retention: not releasable while it stands. The two are separate
fields, so releasing one never drops the other, and a completed turn clears the retention without
clearing the hold. Connection loss ends the hold with the subscription it described.

The hold is the relay's own reference, taken even when a watch or the never-run-root retention has
already subscribed the thread (`ThreadHeld` answers for it alone, while `ThreadSubscribed` counts
any owner). Those other owners release their subscriptions on their own schedule — a `turn/completed`
or the release worker — and the busy backlog has to keep its idle edge through that, so it records a
hold of its own rather than borrowing theirs. The resume is skipped only for a subscription this
client made and recorded itself — this relay's own hold on this connection, or the bridge's retention
of a never-run root. A live watch is deliberately not evidence: `WatchTurn` only admits a watch and
sends no subscribing RPC, so a watch whose resume was refused or cancelled leaves the connection
receiving nothing while the watch is still live, and reading it as a subscription would let the hold
record itself on a socket the recipient's idle reports never reach. The cost of the other error is one
redundant resume on a thread that is already subscribed, which starts no turn and changes nothing.

The relay releases the hold (`Client.ReleaseThread`) when the recipient's backlog empties or the
delivery is delivered, and the ordinary release worker then sends `thread/unsubscribe` on the
original socket, with the existing retries and cleanup. A resume the host refuses leaves the
doubling backoff as that recipient's only trigger, and the refusal is not retried on every tick:
the relay remembers it until the delivery's own busy deadline, so the existing backoff curve stays
the pacing rather than the 20-second tick. The backoff is the safety net either way, for a report
the relay never saw, a host that reports no status, and a recipient that is busy again before the
attempt lands.

The reader records successful creation and turn acknowledgements before caller
cancellation can hide them. Matching terminals are retained independently of
the public notification buffer, including completion before the start reply.
All pending watches belong to the root until it is quiescent. A newer refused
or cancelled send cannot discard an older pending turn or release proof. A
known older turn's terminal does not complete a newer turn whose id is unknown.
An uncertain transmitted turn waits for its terminal or connection loss.

A same-root gate orders admitted operations against unsubscribe. Release runs
on a connection-owned worker after the operation finishes its receipt and last
observation, and rechecks quiescence after taking that gate. Caller cancellation
leaves that worker alive. An unsubscribe failure keeps the proof and retries on
the same socket with delays from five seconds up to five minutes, logging the
first error. Socket loss abandons only that socket's work. Shutdown cancels and
drains release work before closing the transport.

The late-cancellation regressions observe successful client transmission before
cancelling the caller while the reply is still withheld. Fake-host reception
alone does not establish that boundary: cancellation during an unfinished write
can end the socket, and an unscoped test probe can reconnect. These tests retain
their terminal and premature-release assertions, then wait for release proof to
retire and check exactly one handshake and one unsubscribe attempt.

## Never-run roots and sub-threads

A newly acknowledged root without a first durable turn retains its subscription:
on the measured App Server, unloading a never-run root can leave no rollout for
resume. Retention is recorded before creation receipt checkpoints, including
both ordinary and worktree creation. A failed checkpoint or later refused send
cannot clear it; the first terminal does. No failed read is used to materialize
a rollout. Connection loss can still end a never-run subscription; this path
does not claim to make that host state recoverable across disconnection.

Before releasing a completed root subscription, the executable-configured callback
releases its finished descendants. A parent's unsubscribe alone does not prove
descendant release. The explicit finished-child cleanup procedure remains available.

## Automatic release of finished sub-threads

`cmd/crw` supplies `childcleanup.ConfigureSubscriptions` to the relay adapter and
bridge MCP entry point. The bridge library receives a callback and imports no relay
package. After pending watches finish their operation and observe their terminals,
the release worker invokes descendant-only cleanup on the original socket under
the existing root gate. A client-wide admission barrier also drains admitted
operations and prevents direct descendant sends during this callback. Waits hold
neither client nor subscription-manager mutex. Close cancels and drains the worker.

Cleanup first checks whether the root is idle with its latest turn completed, scans the
loaded subtree twice, and checks every member's completion. A running, unknown,
unreadable or never-run member holds archival of the whole subtree. An active root alone is
also an incomplete report, even with no loaded descendants. Pending completion
proof survives holds and later refused sends. Immediately before each descendant
archive, cleanup rechecks the root, reads the descendant's newest turn, then
re-reads its identity and status with `thread/read`. Only idle descendants whose
latest turn completed are archived, deepest first. The root is never archived.

An archived child stays a real child of its session, so only the managed dispatch
ledger tells whether its agent id is spent: a created report for an id that
another attempt holds is refused, and only an attempt the stopped close closed
(outcome stopped, executionState stopped, a reconciliation) gives its id back.

A dispatch ledger lock (`.session.lock` or `<id>.json.lock` in `.crw/dispatches/<session>`) holds an
`owner.json` with the pid, process start, host and creation time (ms) of the process that took it, and a
process that dies while holding one leaves it behind. `crw role helper dispatch-lock-clear --session <id>
(--dispatch <id> | --session-lock) --reason <text> [--cwd <dir>]` removes such a lock only when it is a real
directory with a readable `owner.json` of a process on this host that no longer exists or whose start time
differs (the pid was taken over), then appends the removal to `lock-clears.jsonl` in the session directory and
prints that line (exit 0). It removes nothing and exits 1 when the owner is alive (a pid it cannot signal still
counts), `owner.json` is missing or unreadable, the host differs, the start time cannot be compared, the
entry is a link or not a directory, or `lock-clears.jsonl` has other hard links; usage errors exit 2. Clears of
one session take turns, and a lock that turns out to have been replaced after the check is put back, or left at a
`.clearing-` name that the error reports, and never removed. Taking a lock and giving it back hold the session
directory shared (`flock`; a wait of two seconds, then the answer of a held lock) while a clear holds it exclusively
from before its judgment to the removal of its tombstone, so none of them happens in the middle of a clear, and a
release removes only the directory its holder created.

Each callback attempt has a thirty-second budget. Known running holds keep polling
with the existing five-second to five-minute backoff, including mixed subtrees
with an incomplete idle member. With no known running member, interrupted, failed,
missing, unreadable or unlistable completion leaves unproved members unarchived,
logs their identities once, and releases the root subscription on the first attempt.
Unsubscribe retries reuse that decision; a new acknowledged turn rearms cleanup.
This advisory release shares the independent-client race described below.
Transport, phase and archive
errors stop the attempt before an ancestor archive; eight consecutive errors log
that descendants remain unreleased and fall back to root subscription release.
Success resets the error streak; a new acknowledged turn rearms exhausted cleanup
without discarding older completion proof. An unreadable RPC observation forbids archive.
Loss abandons only the original socket;
cleanup never reconnects. None of this runs the cleanup command or changes receipts.

An isolated codex-cli 0.154.0 probe found that `thread/archive` **accepts an active
sub-thread**, unloads it, interrupts its turn and stops its helper. Host refusal
is therefore no safety fence. The guarantee covers this client's guarded sends
and the checked host observations. The residual race is another client starting
a turn on that descendant between its final re-read and archive. Archival is
reversible with `thread/unarchive`, which restores visibility, not an interrupted
turn. A host API with an atomic idle precondition would be needed to remove that race.
The managed resend gate's own archive has the same window: another client may archive the child
between the gate's idle read and its `thread/archive`, and that case is not distinguished from
this call's reply being lost, so a listing that shows the child archived after an unanswered
archive continues as if this call had applied it.

The isolated reproduction used a fresh home and socket, a synthetic Responses
provider, explicit `historyMode: legacy`, one MCP helper per thread, and the configured Go client's normal watch
lifecycle. Three completed roots and one completed sub-thread measured as follows:

| Observation after finishing watches | Loaded threads | MCP helpers |
| --- | ---: | ---: |
| Before automatic release | 4 | 4 |
| 1.036 seconds | 3 | 3 |
| 60.413 seconds | 0 | 0 |

No cleanup command ran. All probe-owned processes were stopped afterward. These
are measurements of this host version, not a universal unload deadline. Synthetic
regressions additionally cover running descendants, unknown completion, F4's sole
active-root hold, the admission barrier, socket loss and bounded errors.

## Measurement limits

An isolated codex-cli 0.154.0 turn remained active and kept its MCP helper for
80 seconds after its sole subscription was released. The host retains running
turns; the ordinary root release worker also waits for their terminal. Unloading
finished roots and stopping their helpers after the last subscription ends is a
host behavior, with an approximately sixty-second delay on this version, rather
than an API timing guarantee. Another client's subscription can keep a root
loaded after this connection releases it.

## Recovery from other loaded MCP settings

An idle child already loaded under other MCP settings ignores overrides on subsequent resumes.
The relay keeps `settings_not_preserved` on `mcpServers` and names the recovery in its detail. Only
after a completed durable turn and confirmation that its rollout is resumable, release all root
subscriptions and observe `thread/read` reporting `notLoaded`. Other clients may retain subscriptions,
and unloading is the host's decision. If it remains loaded, an operator can use `thread/archive` then
`thread/unarchive`; the next relay delivery resumes under the recorded MCP profile. Never unload a
never-run root. The relay performs one automatic archive of a child: the managed start's orphan
archive below, which replaces the child instead of lowering it.

A managed start waits for its standby turn inside the call. It holds the creation's turn watch
open from the creation until the wait is over, reads the watch's `turn/completed` first and the
host's own record of the turn (`thread/turns/list`) every second when the watch has not reported
it, and answers `incomplete` / `standby_incomplete` only when the 60 second bound
(`Start.StandbyWait`) has passed; a caller's timeout must be longer than the bound. The watch is
let go once the wait is over and on every way out of the call.

When the standby turn has ended and the child is not yet recorded, the start judges the child.
A child is an orphan when the host reports it `idle` (loaded) with a model, reasoning effort or
working directory other than the request's, and its history is exactly the recorded standby turn:
a resume cannot change the settings of a thread the host holds loaded, so no business turn could
start on it under the recorded profile, and nothing else lives on it. The start then archives the
orphan once and creates the child again under the same managed request, as it creates again after
a creation that left no usable thread: the new creation has a bridge operation id derived from the
request and the attempt, and at most three creations are made (`maxCreationAttempts`). A child
whose history shows any other turn (`business_identity_unobserved`), or whose history cannot be
read completely (`lifecycle_unknown`), is somebody's work or unknown: it is never archived or
replaced and the start is held. When the creations are spent the start stops `creation_unknown`
with `creationReconciliation` `attempts_exhausted` and the last child stays where it is.

The archive is a host effect and is bounded by its journal rows. The readiness policy and the ledger
are asked again before it; a `managed_standby_orphan` row with `archive` `attempting` is written
before the host call, on a context the caller's cancellation does not end, and a second row
closes it (`ok`, `refused` when the host answered with its own JSON-RPC error, or `skipped` when
the last `thread/read` before the archive no longer showed an idle child). An archive whose answer
is lost may have applied, and another client may have unarchived and loaded the child again since:
neither the archived listing nor a second archive can settle that, so the start answers `incomplete`
`lifecycle_unknown` (`creationReconciliation` `standby_orphan_held`) and the `attempting` row, which
no later row closes, keeps every repeat of the request, in this process or after a restart, from
archiving the thread again. A repeat that finds `ok` or `refused` for the attempt continues with
the next creation.

A business send that certainly failed before `turn/start` is retained under its original bridge
operation ID and the start answers `incomplete` / `business_failed`; a new operation ID is not
derived for it, and the relay neither lowers nor archives the registered child. The managed request,
fingerprint, slot, relationship, generation and routing dispatch ID never change.

A request that already holds `managed_resend_unloaded` rows from the earlier unload-and-resend
recovery keeps their meaning, read only: nothing writes such a row, archives or unarchives for it.
Its successor operation IDs (at most two, derived from the managed request and attempt) are still
read; an accepted successor passes the bridge ledger's fingerprint-aware lookup for the exact
packet before replaying admission, never starting a turn again, and another operation or message
occupying its ID is refused. A successor that has no attempt yet is sent only when the host lists
exactly the recorded standby turn with no continuation cursor and reports the same child as
`notLoaded`, as the closing row recorded; a begin row without a closing row, and a closing row
whose child the host still holds loaded, hold as `recipient_not_idle` with no archive and no send.
Unknown outcomes, started or rejected deliveries, attempted `turn/start`, malformed effect evidence
and failures without affirmative pre-turn evidence are held, as before; an explicit pre-turn effect
trace must be empty or contain exactly one `thread/resume`. The final business guard repeats the
standby-only history check for a successor, with the complete archived scan and positive
thread/goal reads (at most seven calls, inside the ten-request deadline budget). If another client
loads the thread between the precheck and resume, the ordinary settings verifier still withholds
`turn/start` and retains an honest failure. Initial guard failures recorded as `outcome_unknown` are
not recovered by this path.

The inspected bridge and relay resume paths transmit the profile; subscription retirement
only unsubscribes, and their read paths do not issue an unprofiled resume. The idle edge's
own hold is the one exception: while a head delivery waits out its recipient's busy backoff
the relay opens a subscription with an override-free `thread/resume` carrying no model,
effort or settings override and starting no turn (CRW-904; the subscription is a reference
the backlog takes and releases, and the recipient stays busy, so nothing here is the
business resume these paths guard). The copied
incident receipts show profiled creation followed by an idle, unprofiled MCP status at
business resume, with overrides transmitted and a settings refusal. They do not attribute
the intervening load to a client. An external client's identity and the live same-request
DAG recovery after runtime replacement remain separate operational verification.

A thread-bridge fallback to an unloaded child must state its actual role and released
`expected_settings.mcp_profile`. Without one the bridge refuses before resume when that role declares
profiles, or the role is omitted on a host that declares child profiles. It cannot reliably read the
relay's record or choose a default on behalf of the caller. An unnamed non-child is also refused in
that case; state its actual declared role. The existing pair guard still applies. Prefer relay delivery.
