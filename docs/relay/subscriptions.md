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
never-run root. The relay performs no automatic archive or recovery operation beyond the one
narrow case the managed resend gate owns below, where it lowers an idle child it may then resend
to under the recorded profile.

A managed start can retry the same complete request after a recorded business send
failed before `turn/start`. It retains that failure under its original bridge operation
ID and derives at most two successor operation IDs from the managed request and attempt.
The managed request, fingerprint, slot, relationship, generation and routing dispatch ID
stay unchanged. An accepted successor passes the bridge ledger's fingerprint-aware lookup
for the exact packet before replaying admission, never starting a turn again; another
operation or message occupying its ID is refused.
Unknown outcomes, started or rejected deliveries, attempted `turn/start`, malformed
effect evidence and failures without affirmative pre-turn evidence remain held. Older
receipts without effect/delivery fields qualify only for the structured
`thread/resume` / `settings_not_preserved` refusal.
An explicit pre-turn effect trace must be empty or contain exactly one `thread/resume`;
unknown, misspelled, duplicate or contradictory effects do not qualify.

Before a resend, the host must list exactly the recorded standby turn with no continuation
cursor, then report the same child as `notLoaded`. A child the host holds loaded under other MCP
settings is lowered once instead of waiting, but only when its immediately preceding business
failure is the structured `settings_not_preserved` refusal and the same read reports it `idle`:
the gate rechecks that idle state, calls `thread/archive` then `thread/unarchive`, requires
`notLoaded` and the standby-only history again, and only then resends with the recorded profile.
That unload is the one automatic archive the relay performs, and it is bounded to one lowering per
business attempt inside the existing successor chain: a replay reconstructs the same attempt from
the retained failure, so the journal row an earlier lowering wrote is what stops the next one. An
`active` child, one whose history shows a foreign turn and one whose preceding failure is not that
refusal are never archived and hold as before.
An archive error answers incomplete `recipient_not_idle` with no unarchive and no send; an
unarchive that fails twice answers incomplete `lifecycle_unknown`, naming the archived thread for
an operator in the journal; a child still loaded afterwards answers `recipient_not_idle`. Every
unload writes one `managed_resend_unloaded` journal row naming the thread, the archive and
unarchive results and the load state observed afterwards; once the archive has succeeded the row
is written with a context that survives the caller's cancellation, because an archived child with
no row would leave an operator nothing to read. Empty, missing-rollout or unreadable
history stays held as `lifecycle_unknown`; another turn refuses as
`business_identity_unobserved`. The final business guard repeats the standby-only check after
the recorded-profile resume.
The recovery guard retains the complete archived scan and positive thread/goal reads,
and skips the unnecessary unarchived listing, so its history check fits the existing
ten-request deadline budget (at most seven calls).
Live-context observation errors in that recovery guard withhold as retryable
`not_attempted`; cancellation remains unknown.
If another client loads the thread between the precheck and resume, the ordinary settings
verifier still withholds `turn/start` and retains an honest failure; a later invocation
can use the next bounded successor. No fourth operation is created. Initial guard failures
recorded as `outcome_unknown` are not recovered by this path.

The inspected bridge and relay resume paths transmit the profile; subscription retirement
only unsubscribes, and their read paths do not issue an unprofiled resume. The copied
incident receipts show profiled creation followed by an idle, unprofiled MCP status at
business resume, with overrides transmitted and a settings refusal. They do not attribute
the intervening load to a client. An external client's identity and the live same-request
DAG recovery after runtime replacement remain separate operational verification.

A thread-bridge fallback to an unloaded child must state its actual role and released
`expected_settings.mcp_profile`. Without one the bridge refuses before resume when that role declares
profiles, or the role is omitted on a host that declares child profiles. It cannot reliably read the
relay's record or choose a default on behalf of the caller. An unnamed non-child is also refused in
that case; state its actual declared role. The existing pair guard still applies. Prefer relay delivery.
