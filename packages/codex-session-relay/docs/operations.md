# Operating the relay

Who owns the state, who owns the daemon, and what an operator can ask it. The delivery
contract itself is in [protocol-v1.md](protocol-v1.md) and the refusals that enforce it are
in [invariants.md](invariants.md).

Each section carries a status, because this document is written alongside the work that
makes it true: **implemented** means the behaviour exists and is tested in this package,
**planned** means the contract is agreed and the code lands in a named pull request.
Nothing here describes an installed runtime, a registered service or a live App Server;
source in this package changes none of those.

## Where the state lives

Every participant of one assignment must read and write the same SQLite store. The
directory is chosen by the first rule that applies:

| Precedence | Source | Notes |
|---|---|---|
| 1 | `--state <dir>` | explicit, wins over everything |
| 2 | `CODEX_SESSION_RELAY_STATE` | also read independently by the bridge adapter for its transport ledger |
| 3 | `XDG_STATE_HOME/codex-session-relay/<scope>` | the scope is a hash of the App Server socket path |
| 4 | `~/.local/state/codex-session-relay/<scope>` | the default |

The store is `<dir>/relay.sqlite3`. `codex-session-relay doctor` reports which rule won, the
value that won, the resolved database path and the measured read/write access, so a
participant never has to infer its own configuration.

Setting `--state` alone is not enough for an isolated run. The bridge adapter resolves its
transport ledger from `CODEX_SESSION_RELAY_STATE` independently, so a run that overrides only
the flag splits the relay store from the ledger that carries send idempotency. Set both.

Status: implemented. `resolve_state_dir` returns the winning rule and `doctor` reports it with
the resolved database and the measured access.

### When the rules cannot pick a store

The precedence table decides which directory a participant uses. It does not decide what
happens when the directory it lands on is ambiguous, and the answer there is that the relay
refuses rather than guesses. Creating a store on a guess is the one outcome that cannot be
undone by a later command: once a canonical database exists it wins every later resolution,
and the assignments in whatever it hid become unreachable without knowing they exist.

Three refusals cover it. All but one command decide them before that command runs, so a command
that opens no store — `service status`, for instance — is refused on the same evidence as one
that would create a database. The exception is `guard-evaluate`, which cannot be answered that
early and carries the same question to the point where the selection would be used; the
exemptions below own it.

| Reason | When | What it means |
|---|---|---|
| `ambiguous_state_directory` | default discovery would select a new database and two or more stores record this socket | either could be the right one, and choosing by sort order would serve one set of assignments today and the other after a rename |
| `unidentified_state_directory` | default discovery would select a new database and a sibling store records no socket at all | its directory hash cannot be inverted, so it cannot be ruled out as this socket's. "Records no socket" also covers a store whose metadata is unreadable or malformed |
| `state_directory_serves_another_socket` | the selected existing database records a different socket than the one requested | the service would claim and serve the new socket while the database went on attributing itself to the old one |

The first two are reached only after the canonical and legacy-spelling shortcuts have both
failed to find a store, which is why an installation that is simply running never sees them.
The third applies to any explicitly selected store, from `--state` or from
`CODEX_SESSION_RELAY_STATE`, because choosing a directory is not choosing what is already
inside it.

To recover, inspect before adopting. `doctor` names the candidates:

    codex-session-relay --socket <socket> doctor

then read each candidate, carrying the same socket so the comparison is like for like:

    codex-session-relay --state <candidate> --socket <socket> doctor
    codex-session-relay --state <candidate> --socket <socket> service status

`service status` groups by project, so the candidate holding the assignments you expect is
the one to keep. Neither command records provenance; only opening a store with a socket does
that, so inspection is safe to repeat.

Two things this recovery does not do. Selecting one of two claiming stores does not remove the
ambiguity — both still record the socket, so the next invocation that relies on default
discovery is refused again, and every participant of that assignment has to pass the same
explicit `--state` until one of the stores is retired. The Stop hook is the exception: a
directory chosen for one run does not settle it for `guard-evaluate`, whose refusal ends
differently (see the exemptions below). And the different-socket refusal's
own recovery list adopts nothing: using a store does not rewrite the socket it recorded, so
the two commands it prints only read the mismatched pair apart — this store under the socket
it actually records, and the socket you asked for under whichever store belongs to it. The
fix is to choose the matching pair; no command in that list makes the mismatch go away. When
`CODEX_SESSION_RELAY_STATE` is what pinned the selection, the socket-first line is printed
with `env -u CODEX_SESSION_RELAY_STATE`, because otherwise it would re-select the store that
produced the refusal and return it again. Only then: when `--state` caused the refusal the
variable may point at the store that does record the requested socket, and dropping it there
would send the operator to a default directory that usually holds no database at all.

`doctor` and `ack-proof` are exempt from these three guards, for opposite reasons: `doctor`
is how the candidates are found in the first place, and `ack-proof` derives a value from its
own two arguments and opens no store. Exempt from these guards is not the same as never
refusing — `doctor` still exits non-zero when a same-store comparison it was asked to make
comes back unproven or mismatched.

The nine marker commands are exempt too, for a third reason: the managed marker exists so that a
Stop hook can answer without asking the relay anything, and legacy state nobody is using must not
be able to switch that hook off. Two of them do reach a store and are exempt only conditionally.
`intent-declare` RECORDS the resolved path into the intent for the hook to read later, so it
stays guarded unless `--no-db-path` says to record none, and `intent-register` confirms the
relationship against a store, so it is exempt only when `--db-path` names which one.

`guard-evaluate` is the third conditional one, and its condition is settled later than any of
these. It reads receipts from the first of three sources that answers: `--db-path`, then the
`dbPath` the coordinator recorded in the intent, then its own resolution. The first two are
selections somebody made, one explicit and one durable, and neither depends on discovery, so an
ambiguity in discovery is genuinely unrelated to them and the hook goes on classifying and
recording. The third is not a selection; it is a guess about somebody else's choice. That
question cannot be settled on the command line, because whether the coordinator recorded a path
is a fact in a marker the command has not read yet — the workspace it belongs to arrives inside
the Stop payload, on stdin. So the refusal is not skipped for this command, it is deferred: it is
asked again where that third source would be used, and only if execution gets there. A turn
released on the child's own declaration never opens a store and is never refused, and neither is
a readiness with no registered relationship, because there is nothing to look a receipt up by. A
Stop that would otherwise be judged against a store nobody selected is refused, with the
candidates and the recovery commands, and the payload says in `stopNotJudged` that the Stop was
neither classified nor recorded.

The two halves of that refusal are not worth the same. The different-socket half removes a wrong
answer: that store exists and opens, its rows belong to another installation, and a relationship
missing from it reads as `receipt_missing`, which holds a child that has finished.

It speaks only when the caller names a socket, because a store's provenance IS a socket and the
comparison needs one to compare against. So the Stop hook's settings carry `socketPath`, and both
adapter copies pass it as the global `--socket` option ahead of the subcommand. The field is
optional: requiring it would make every document written before it existed malformed, and a Stop
whose settings cannot be acted on releases in silence, which is a worse failure than the one this
closes. A host that configures no socket therefore keeps exactly what it had — nothing is compared,
and an inherited `CODEX_SESSION_RELAY_STATE` reaching another installation's store is read as
though it were this one's. Configure it on any host where more than one installation exists.

The ambiguous and unidentified halves do not remove a wrong answer either, today, when the
selection is discovery's own. Those selections are returned only when no canonical database is
there yet, so the fallback names a file nothing can open, and the guard already answers
`state_unreadable` and releases. For them the refusal trades a recorded release for a diagnosis:
the candidate stores and the commands that tell them apart, instead of "receipts unreadable".
Giving up the marker observation for that Stop is the cost, and it is the reason this boundary
is written down here rather than left to the code.

A state directory chosen for one run is different, and there the refusal does remove a wrong
answer. `--state`, or a `CODEX_SESSION_RELAY_STATE` the hook inherits from the host, returns
from `resolve_state_dir` before discovery runs, so the selection carries no candidates of its
own, and the store it names exists and opens: judged, a relationship missing from it reads as
`receipt_missing` and holds a child that has finished. So when the guard reaches its third
source through such an override and a socket is configured, it asks what discovery alone would
have said (`discover_state_dir`). If that is ambiguous or unidentified, it refuses with the same
reason, adding `overriddenBy` (the flag or the variable, as given) and `selectedDirectory`. If
discovery would have named exactly one store, the override is used, which is the answer a sole
discovered store gets. With no socket nothing is compared, the limit above. Every other command
keeps `--state` as the way to settle an ambiguity; only the Stop refuses it, because nobody
recorded that choice.

The guard's ambiguous and unidentified refusals end with lines that are true for a Stop instead
of the advice to pass `--state`: the Stop was released without being judged and is not judged
again later; a later Stop of the assignment is judged when the hook's settings name a store with
`--db-path` or the intent records it as `dbPath` (both are written once, so neither can be added
to an existing installation or assignment), or when discovery names exactly one store and
nothing pins another directory for the hook; and which candidate holds the assignment is not
something the refusal can establish. For an override they add that it is read before discovery
on every Stop that carries it. None of them writes anything. Their `doctor` line drops an
inherited `CODEX_SESSION_RELAY_STATE`, because under a pinned directory `doctor` does not look
for siblings.

Neither half holds anything. A hook adapter reads an exit of 2 carrying the relay's own error
record as the relay refusing a request it understood, prints no decision, and lets the turn end.

Status: implemented. `resolve_state_dir` carries the candidates on the selection and the
command line refuses on them, naming the candidates and the database it would otherwise have
created.

## Proving two participants share one store

A path string is not proof: symlinks, bind mounts and per-sandbox mounts all make equal
paths unequal and unequal paths equal. A stored identifier alone is not proof either,
because copying the database copies the identifier. A device and inode pair is conclusive
when it differs and insufficient when it agrees, because one inode can have more than one
pathname - a hardlink name or a file bind mount - and SQLite derives the write-ahead log from
the pathname a connection opens. Neither second pathname is visible to the participant doing
the comparing: it holds its own path and the peer's device and inode, and nothing that says
which pathname the peer opened. A nonce is the only live evidence, and it is not proof on its
own either: it says a write of the peer's reached the file being read, and a copy taken AFTER
the challenge was written carries it with the bytes. Nothing in the protocol establishes that
order. So proof takes both - a found nonce and an agreeing device and inode - and each alone
is unproven for its own reason.

| Evidence | Verdict |
|---|---|
| a nonce readable by the other participant AND an agreeing device/inode AND an agreeing log location | proven |
| a nonce readable by the other participant, with no physical identity or no log location compared | unproven - a copy taken after the challenge carries the nonce, and one inode reached at a second pathname keeps a log of its own |
| equal store id and equal device/inode, nothing live | unproven - a copy carries the id, and one inode can be reached at more than one pathname |
| a log location the other participant does not share | unproven - the two were not shown to write into one write-ahead log |
| an inode with more than one name | unproven - the peer may have opened a different name |
| a store that states no identity, or a nonce that could not be read, or a nonce read from another file | unproven - absence is not agreement, and an answer that cannot be attributed is not evidence about this store |
| different store id, different device/inode, or the nonce absent | mismatch |

Insufficient evidence is decided before agreeing evidence, so an inode with more than one
name is unproven even when a nonce was found: the nonce says the peer's write reached this
file and cannot say which name the peer keeps writing through.

The name count catches one case and only one. `st_nlink` counts hardlink names, and a file
bind mount adds a pathname without changing it - measured on this host on 2026-09-22, where
such a mount reached `proven` while the two names each grew their own `-wal` and a frame
written live through one was unreadable through the other. What answers that generally is the
LOG LOCATION: SQLite writes the log beside the pathname a connection opened, so two
participants share one log when their opened databases share a directory entry. Each
participant measures its own and reports it with `store-identity`; the peer sends it back as
`--expect-log`.

Agreement there is a sufficient condition for one log rather than an equivalence, so a
DIFFERENCE is unproven rather than a mismatch: the sidecars can themselves be aliased, an
overlay merged path and its upperdir can differ as directories while the log entry is one
file, and SQLite documents the `-wal` suffix as what it usually appends. Read under the
default unix VFS with unaliased sidecars. `compare_store` grades all of it.

`doctor --expect-store <id>` exits non-zero on a mismatch, and `--expect-inode` and
`--expect-log` alongside `--expect-nonce` are what reach proven. Supplying any expectation at
all and getting no proof exits non-zero, including one whose value is empty or malformed. An
unproven result is never reported as healthy.

### Which file the evidence came from

All of that grading assumes the answer describes one file. A pathname cannot carry that
assumption: measuring the identity at the path before and after a read catches a replacement
that persists and misses one reverted inside the window, because both observations then report
the original inode while the rows came out of the interloper.

So the diagnostic reads hold the database open. The identity is `fstat`-ed from that
descriptor, every connection is opened through `/proc/self/fd/N`, and the descriptor is
required to still name this store - asked again immediately before each connection, because
`doctor` opens a read connection and a write probe through one descriptor. A read that cannot
establish this is refused rather than answered: it returns no rows, no identity and a detail,
and `compare_store` grades that as unproven, never as absence.

This is Linux-specific, the same assumption artifact authorization already makes (I-11). Where
`/proc/self/fd` is unavailable a read cannot be bound to the file it came from at all, so it
is refused rather than answered on the weaker measurement.

The answer is bracketed on both sides. SQLite resolves the descriptor to a real name and opens
that name on its own descriptor - which is what puts `-wal` and `-shm` beside the real file - so
the connection is also asked which file it opened, through `PRAGMA database_list`, and the
descriptor is checked again after the read. A store that is still moved when the descriptor is
asked is refused rather than answered, and `doctor` withdraws what a read had already published if
the store is still moved at its closing check. SQLite's answer refuses a move made before or inside
the connect and sees none after it. A move and a return that both land between two of the
descriptor's checks are not observed.

Nothing that could create a file runs first. The first statement that touches the file creates the
log beside whichever name SQLite opened, so a query or transaction on a moved name leaves a stray
`-wal` behind even when the answer is refused, and on the older builds measured (SQLite 3.34.1,
3.37.2 and 3.38.5) so does `PRAGMA database_list` itself. So every connection, including
`doctor`'s write probe, asks the descriptor again as soon as it is open - a readlink, which
creates nothing - and asks SQLite which file it opened before any query or transaction.

Two limits remain. One no check on this side can observe: a different file swapped ONTO the
expected pathname inside SQLite's own resolve-then-open. The other is a rename landing after the
descriptor's post-connect answer, which nothing checks before the next statement: SQLite keeps any
log beside the validated name, a read's closing check withdraws its answer only if the store is
still moved when it asks, and on the older builds measured, with the rename after SQLite's answer,
the write probe can report writable a file that has just moved. The checks are observations rather
than locks: the descriptor's refuse a store that is still moved when they ask, and a move and a
return that both land between two of them go unobserved.

And a store that is present but will not state its identity is not a store that is absent:
`doctor` reports it as present and unidentified, and the lifecycle commands treat it as
unverifiable rather than signalling a service whose store they could not compare.

Status: implemented. Unproven also exits non-zero, because a caller that asked whether this is
the same store must not read exit 0 as yes. Each participant runs the check in its own sandbox;
one invocation cannot establish another participant's access.

## The service

Two process roles. A **worker** is the existing bounded daemon: it runs a set number of ticks
or until a deadline and then exits, and cannot be constructed unbounded (I-64). A
**supervisor** owns the locks and launches successive workers, which is what carries an
assignment past any single process bound.

A bound only means something in the clock of the process enforcing it, so it crosses the
boundary between those two roles as an instant and not as a duration (I-197). The supervisor
passes `--deadline-monotonic`, the worker converts it against its own `CLOCK_MONOTONIC` as soon
as it has one, and the startup the worker has not paid yet comes out of its segment instead of
landing after it; `service start --deadline N` does the same for the supervisor it launches. A
process that reaches its own clock past that instant serves nothing and exits `5`, which its
supervisor reads as neither a clean segment nor a crash: it stops replacing workers, and reports
`degraded` when its own bound still had room, because a segment shorter than a worker costs to
start would otherwise churn processes that never serve.

| Command | Effect |
|---|---|
| `service status` | intent, liveness, ownership, store, conflicts, projects, observation health |
| `service enable` / `disable` | records the owner's intent; disable also stops a running service |
| `service start` | refuses if already running, if another store owns the operating scope, or if intent is disabled |
| `service stop` | refuses unless this installation owns the process; confirms both supervisor and worker exited |
| `service restart` | stop then start, preserving the recorded intent |
| `service run` | the foreground supervisor; what `start` launches |

An update tool should read `service status`, act on `stop` or `restart`, and rely on one
guarantee: **a service that was disabled is never enabled as a side effect**. Intent lives in
`service.json` and absence means never configured, which is not enabled.

Status: implemented. `service run` is the supervisor: it holds the locks once and replaces
bounded workers, so an assignment continues on the same store and generation past any single
process lifetime. A clean segment waits the restart interval; repeated failure backs off
exponentially to a cap and is reported as `degraded` rather than retried silently.

### Ownership

Single ownership is enforced at two levels, because either alone has a hole.

- `<state>/daemon.lock` prevents two daemons per state directory.
- A per-user scope registry, keyed by the App Server socket and located independently of the
  state directory, prevents two daemons **with different state directories** from serving the
  same App Server. A file lock in one state directory cannot see a rival that chose another.

Both locks are held by the supervisor and inherited by its worker, so ownership persists
until every process of the service has exited. A dead supervisor with a live worker does not
release the scope.

Termination is bound to a process handle rather than a pid, and identity includes the boot
id, the installation and the store, so a reused pid is never signalled by mistake. Where a
stable handle is unavailable, ownership reports `unverifiable` and stop refuses.

Status: implemented. The supervisor acquires both locks, marks them inheritable and passes
them to each worker, which adopts them rather than taking a second lock. A worker is
authenticated by a token recorded in `daemon.json` plus a device/inode check on each
descriptor, arms `PR_SET_PDEATHSIG` in its own bootstrap and immediately re-checks its
parent, and refuses to serve if the supervisor has already gone.

A lifecycle command that cannot establish who owns the directory refuses rather than acting,
and the reason says which kind of uncertainty it hit. The distinction matters because the
operator's next step differs.

| Reason | Evidence behind it | What to do |
|---|---|---|
| `not_ours` | a definite mismatch — the record names another installation, another store, or a different boot | nothing here is yours to stop. Find the installation that owns it |
| `ownership_unverifiable` | evidence is insufficient rather than contrary — no start time for the pid, no boot id on a host that has one, or the lock held by something that has published no record | re-read `service status`; a supervisor mid-start resolves on its own. A record that stays unverifiable names a process that cannot be identified, and signalling it on a pid match alone is how an unrelated process gets killed |
| `replaced_by_new_launch` | the daemon lock could not be taken after both recorded processes were confirmed gone, or a new record appeared during the stop | the service is **not** stopped, and the launch this command started from may already have been terminated. Re-inspect ownership before doing anything else; do not read the failure as "nothing happened" |
| `supervised_store_mismatch` | a worker's own store does not match the `storeId` its supervisor recorded | the database moved or was replaced under a running service. Reconcile which store was intended before restarting — a restart does not repair this, and the check permits a record that carries no store id at all and cannot detect a copy that kept one |

Status: implemented, with one exception named in the table above. `replaced_by_new_launch` is
decided after a stop has already run, which is why it reports that the service is not stopped
while the launch the command started from may already have been terminated; it is a refusal
to claim success, not a refusal to act. The others are returned before the command takes its
action, so a refused stop leaves the supervisor untouched and a refused disable leaves the
shared intent as the owner wrote it. The windows this does not close - a lock probe that cannot
open the file, an intent write during a holder handoff, a start confirmed from a record and a
separate probe - are recorded under "Recorded limits" in [invariants.md](invariants.md).

## One service, several projects

A single supervisor and a single store serve every assignment on that host, user and App
Server, across projects and repositories. Each delivery is checked against its own
assignment before any transport call: the recipient must be that assignment's own parent for
a completion, or its own child for a revision. Membership in the authorized recipient list
alone is not sufficient, because two assignments may legitimately list the same recipient.
`service status` groups by project so one service carrying several projects is visible.

Cancelling, pausing or archiving one assignment removes it from the loop. It does not stop
the shared service: only the supervisor's own bound, an explicit stop, or disabled intent
does that.

Status: implemented. The direction check runs at both `enqueue` and `attempt`, before any
transport call.

## Reading a stuck delivery

`status` distinguishes the stages that all previously read as one withheld state:

| Phase | Meaning |
|---|---|
| `awaiting_receipt` | the child has not produced a completion receipt yet |
| `awaiting_send` | the receipt is collected and accepted; this relay has not reached the recipient yet |
| `in_flight` | a send was claimed and its outcome is not yet settled |
| `parent_busy` | the parent is mid-turn; it is never interrupted |
| `settings_rejected` | the host would not confirm the authorized execution settings |
| `withheld:<operation>` | refused before any transport call, naming the operation that refused |
| `turn_accepted` | the transport started a turn |
| `awaiting_ack` | delivered, acknowledgement outstanding |
| `awaiting_ack:turn_check_undecided` | delivered, acknowledgement outstanding, and the relay could not tell whether the host still has the delivery's turn; waiting will not tell it either (see "When the host loses an accepted turn") |
| `redelivering:host_lost_turn` | the host lost the turn the last attempt started; the same event is queued for its next attempt |
| `held:host_lost_turn` | the host lost the redelivery's turn as well; held, and nothing sends it again |
| `awaiting_child_receipt` | a revision request was delivered; contract v1 defines no acknowledgement for that direction, so the child answers with its next completion receipt |
| `awaiting_grant_acknowledgement` | a merge-turn grant was delivered and its turn has not recorded the acknowledgement; it is answered with `merge-turn-acknowledge`, never with an `ack` |
| `grant_acknowledged` | the grant was acknowledged on its merge turn, in any delivery state (a parent can answer before the notice is sent) and including after that turn later landed or was returned |
| `channel_closed` | the push channel itself is unavailable; stored, not woken |
| `superseded` | a newer generation or revision replaced this one |
| `superseded:<reason>` | an outstanding send a newer generation or revision replaced; its state is left alone so a lost response stays reconcilable. For a merge-turn grant, a notice its own turn no longer needs, in any delivery state: `merge_turn_regranted`, `merge_turn_closed` (closed unanswered), `merge_turn_absent` or `merge_turn_grant_unreadable` |

Each carries the most recent failed operation, its concrete error, the exact settings
difference where there is one, and the next retry time.

`status` also reports `pendingIntents`: events whose delivery was wanted and refused before a
delivery row could exist, which a paused or unauthorized assignment produces. They have no
phase in the table above because they have no delivery; they carry the refusal, the attempt
count and the next retry instead. Without them the most stuck state in the system was the one
status could not show.

Health is separate from liveness. A running process with a growing observation backlog is
reported as stalled: staged event age, when each current anchor was last successfully
polled, and the backlog per assignment are all exposed, and a live pid is never counted as
working. An anchor whose turn is terminal with nothing staged behind it is reported as
settled and excluded from freshness: the scheduler deliberately stops reading it, so its
last poll cannot advance, and ageing it out would report every quiet assignment as stalled.
New staged work on that turn makes it eligible again.

Settlement is recorded per assignment. Two assignments can legitimately watch the same child
turn, and the `observations` table is keyed by the turn alone, so it can only ever name whichever
assignment settled it first. `assignment_settlements` carries the per-assignment fact, which is
what the observation scheduler and this health block ask. Without it every other assignment
on a shared turn looked permanently unsettled, was re-polled on every round and spent
observation budget forever.

So is the work itself. Staged claims are selected and settled per assignment, because a child
thread can serve several and a claim on one of its turns belongs to exactly one of them.
Selecting by thread alone put a paused assignment's claim into an active assignment's ring,
and settling by turn alone let whichever assignment polled first suppress the owner's claim
while producing no receipt of its own - so the owner's parent waited on an outcome that had
already been discarded. An inactive assignment's staged claim is left untouched until it is
resumed, and is excluded from backlog for the same reason: the scheduler will not process it.

Each parent's delivery window rotates. The parent order decides who goes first; a persistent
per-parent cursor decides where that parent's own window starts, and it advances only by what
was actually attempted. Without it the window was always a parent's oldest rows, so a delivery
that fails before changing its own state stays eligible, stays oldest and blocks every later
delivery for that parent indefinitely.

A delivery that has reached its busy or pre-send attempt cap is annotated when its generation
advances. Once a cap sets a hold, `attempt` returns before the pre-send supersession check, so
that is the only occasion on which such a row can ever be told its generation has moved on.

Status: implemented. `status` reports the phase, the most recent failed operation with its
error code and, for a settings rejection, the exact fields the host disagreed on, plus the
next retry time. The field-level difference is read from the raw receipt, because the
transport classification keeps only a code.

Observation health is reported beside it: staged event ages, when each current anchor was
last successfully polled, and the backlog per assignment. A failed read updates the attempt
time and never the success time, so an anchor whose first read failed reads as never polled
rather than fresh. Process liveness is reported separately and is never counted as health.

## When the host loses an accepted turn

An accepted `turn/start` is a transport fact, not a turn the host keeps. CRW-124's H0 rehearsal
stopped the App Server right after it accepted a delivery's turn: the rollout kept only
`task_started`, the restarted host listed no such turn, and the delivery read
`dispatched_awaiting_ack` for good. That is the same reading a lost acknowledgement gives, and
nothing read the parent's turns to tell the two apart.

The daemon now asks. For every delivered completion still waiting for its acknowledgement, a
bounded pass (`max_turn_checks_per_tick`, 4 lookups a tick) looks for the attempt's turn in the
parent's own turn list, newest first, among the turns begun since the send. That includes a
completion whose send was uncertain and which reconciliation confirmed from its token in the
parent's items: its attempt keeps the honest `held_uncertain` snapshot, and the turn looked for is
the one the token was found in. The turn counts as lost only when:

- the host answered: the listing ended, or reached a turn that began more than 61 seconds before
  the send, without it. The lookup pages back through up to 1000 turns. An empty or unreadable
  listing, or one that never reached the send, is no answer;
- the send is more than 61 seconds old, which covers the host's whole-second start times and
  a clock skew;
- this attempt's delivery token is in none of the parent's items since the send. The scan reads
  newest first and counts only when it reaches an item of a turn the listing showed began before
  the send, or the end. Items of turns missing from the list are read like any other, because a
  turn the host dropped from its list can keep its items. A found token means the message
  arrived, whatever happened to the turn row. A scan that stops at its bound (200 items) first
  has shown nothing.

A lost acknowledgement keeps its turn: listed, interrupted, with the message in it. It reads
present and keeps reading `awaiting_ack`, as before.

A listed turn is not always a kept one. CRW-124's isolated re-check at R3 (H0R3-F1) stopped the
App Server right after it accepted a delivery's turn and had the parent loaded again before the
relay's check. The restarted host listed the lost turn back as interrupted, with none of its
items, and the relay, which read any listed turn as present, never looked at it again. So a listed
turn that has finished (completed, interrupted or failed) counts as present only when its own
items, read oldest first through `thread/items/list` with its `turnId` (up to 2000 of them), hold
this attempt's message. The message opens a turn it started, so one page is usually enough; a send
folded into a turn already running sits further in. A finished turn listed without it is judged
like an unlisted one, under the
second and third conditions: the same allowance, the same scan since the send, the same loss and
redelivery, with a `detail` that names the listed status. When the message turns up under another
turn, the delivery is not sent again, but the reading carries no status, so the daemon does not
keep it as settled and a later reload that empties that turn as well is still caught. A turn
still in progress is present as listed and is read again once it finishes.

The token has to be in the message itself, not in something the parent printed or wrote about
it: relay commands such as `status` and `show` print request ids into the parent's own command
output, and a parent can write one into a file. Where the host gives item types,
reconciliation's scan and the in-turn read count only a `userMessage` item, and the loss scan
passes over every type App Server 0.154.0 names for agent output (`agentMessage`,
`collabAgentToolCall`, `commandExecution`, `contextCompaction`, `dynamicToolCall`,
`enteredReviewMode`, `exitedReviewMode`, `fileChange`, `functionCallOutput`, `imageGeneration`,
`imageView`, `mcpToolCall`, `plan`, `reasoning`, `sleep`, `subAgentActivity`, `webSearch`). A
token found only in an item of another type, a hook prompt or a type the relay does not know,
may be the message or an echo of it. That reading is neither a loss nor a delivery: nothing is
sent again, and the attempt is named `turn_check_undecided:token_in_other_item`. A host that
gives no types is read by text, as before, and there a command's output can still be taken for
the message.

A lost turn is recorded on the attempt as `host_lost_turn`, and the delivery is queued again, so the
ordinary claim sends the next attempt of the same event under a new request id. If the host loses
that turn too, the delivery is held as `held:host_lost_turn` and is not sent again. Recovering it
is the parent's job: read the report with `show --event`, then open a fresh generation if the work
still needs verifying. The lost attempts stay in `attemptDetail` with the state `host_lost_turn`.
The fault sweep collects them as `delivery_stalled`, and the held row's hold as well, while the
delivery is not dispatched. Only completions are recovered this way. A revision's dispatched turn
is its generation's anchor, which is never rebound, so for revisions and merge-turn grants the
check is reported and changes nothing. A lost revision anchor already shows up in observation
health as an absent turn.

The pass writes nothing while the turn is listed. A turn listed as finished is not read again while
its delivery still waits for the acknowledgement. The daemon remembers those request ids in memory,
and each tick asks the store about the page of them it checked longest ago, forgetting any whose
delivery has since been acknowledged, held, lost or sent again. The memory therefore stays about the
size of the deliveries still waiting. The pass resumes where the last one stopped and returns to
the first waiting delivery only after reaching the last, so a long run of finished turns delays the
deliveries behind it by a page a tick and never hides them. An in-progress turn and an unreadable
host are read again on a later rotation, within the budget.

Some readings cannot decide however long the relay waits: a listing that never reached the send,
a listing with no turns at all once the send is more than 61 seconds old, a token scan that could
not cover the turns since it, or an attempt without a send time. An empty listing is what a
parent shows when the lost delivery was its only turn, and also what a host shows for a thread it
answers for without its turns, so it is named and never taken for absence. These are recorded on
the attempt as `turn_check_undecided:<reason>` (`listing_bounded`, `listing_empty`,
`token_scan_bounded`, `token_in_other_item`, `no_send_time`). One more reading is named the
same way although it
decides the veto: the delivery's token is in the parent's items but its turn is gone from the
list (`token_without_turn`). The message is there, so it is not sent again, but the turn that would
have acted on it is gone, which the parent has to be able to see. `status` reads the phase as
`awaiting_ack:turn_check_undecided`, and `assignment-show` carries the reason as
`projection.completion.delivery.turnCheck`. Nothing is sent again on an undecided reading. The
daemon reads such a row again after ten minutes. Only a later reading that decides clears the
name. A reconcile whose own read fails, or that settles the same dispatch from its receipt again,
keeps it. The parent can still acknowledge the delivery, or read the report with `show --event`
and recover.

Reconciliation reads first and writes later. If the daemon records a loss in between, the
reconcile's own write refuses to overwrite it and reports the recorded loss, with its reading and
the `redelivery` the loss recorded. Without that refusal, the attempt would go back to dispatched,
and the one count that stops a third send would be lost.

A send already confirmed from its token stays confirmed. When a later `reconcile` finds no
accepted receipt, it does not scan the items again: not finding the token a second time is not
evidence against the first find, and writing the attempt back with evidence `none` would also
make a later loss record `none`. The reconcile asks only whether the parent still has the turn the
token was found in, and a recorded loss reports the evidence the attempt was delivered on.

More generally, an attempt row has several writers: the sender itself, the daemon's reconcile, a
manual `reconcile`, and a confirmation through an acknowledging turn (below). Each settles only
the attempt it read. A write is a compare-and-set over the attempt's settlement state, state and
evidence as that reader saw them, so a reader that lost a race writes nothing and reports the
attempt as it now stands (`changed`), and the daemon reconciles it again on the next tick. The
sender settles its attempt only while it is still in flight; when a reconcile got there first,
the sender's result reports `_settledElsewhere` and the delivery state that reconcile left, and
the transport ledger keeps the receipt for the next reading. A reading that found no evidence
never writes `none` over evidence already settled (`kept`).

An acknowledgement can arrive before the relay has confirmed the delivery it answers. The R3
re-check (H0R3-F2) cut the `turn/start` receipt while the host ran the delivery turn: the attempt
settled `held_uncertain`, and the parent, inside that very turn, claimed the report and
acknowledged it. The acknowledgement was refused because the delivery was not delivered yet,
reconciliation confirmed the delivery seconds later from its token, and nothing asked the parent
again, so the event waited for an acknowledgement for good while `assignment-show` read
`parent_verifies`.

`ack` now settles such a delivery first. With a host, once the proof checks, it reconciles the
current attempt, and if that leaves the send uncertain it reads the acknowledging turn's own first
2000 items, oldest first, for the attempt's message: the parent acknowledged from inside the
delivery turn, so the message is in that turn however long it ran, at its start or, for a send
folded into a turn already running, wherever the turn had got to. A message found there is the
same evidence as a token found in the thread (I-41); the acknowledgement itself never is. Once
the delivery is confirmed, the acknowledgement is judged as usual, and the turn the message was
found in counts as the delivery's turn, so an acknowledgement from a folded turn that began
before the send is not refused for its start time. While the delivery is not confirmed, or while
the send is still in flight, the acknowledgement is kept exactly as authored, unverified, with
`ack_evidence.last_reason` `delivery_unconfirmed`, and the delivery is left alone. The daemon
confirms each kept acknowledgement's delivery through its turn, paced by the pending pass's
backoff, and completes the acknowledgement in the same tick once the delivery is confirmed. A
verdict completes it first. An ordinary acknowledgement makes no extra host read.

`reconcile --request-id` runs the same read for a receipt that carries a turn id and reports it
as `recipientTurn`: `{turnId, finding, status, detail}`, where the finding is `present`,
`host_lost_turn` or `unknown`. `recipientTurnsChecked` in the record is true only when the host
answered. For a completion it applies the same recovery, reporting `redelivery` as `queued`,
`held` or `not_moved` (when an acknowledgement is already recorded, or the attempt is no longer
the current one). Reconciling an attempt that was already found lost reports it again and
changes nothing. `recover` reads the same way for an open attempt whose receipt now carries a
turn id.

`assignment-show` names who moves an unanswered completion next, taken from its own delivery row
rather than from the assignment state alone. This applies in `received`, `corrected` and
`verifying`. A claim is not an acknowledgement: a parent can claim a report inside a delivery turn
the relay has not confirmed, so a claimed completion reads `parent_verifies` only once its
acknowledgement is verified.

| Delivery | `nextExpectedAction` |
|---|---|
| acknowledged and accepted, or acknowledged after a claim | `parent_verifies` |
| held after a host loss, for any reason | `parent_recovers_host_lost_turn` |
| `held_uncertain`, or a claim still `sending`, with or without a kept acknowledgement | `daemon_reconciles_delivery` |
| dispatched or `inbox_only`, acknowledgement recorded and not yet confirmed, or kept while the delivery was unconfirmed | `daemon_verifies_acknowledgement` |
| dispatched or `inbox_only`, acknowledgement refused by verification | `parent_reacknowledges` |
| dispatched or `inbox_only`, no acknowledgement | `parent_acknowledges` |
| queued, deferred or withheld after a host loss | `daemon_redelivers_host_lost_turn` |
| queued, deferred or withheld | `daemon_delivers` |

`projection.completion.delivery.hostLostAttempts` counts the event's lost attempts and stays
above zero after a redelivery reaches the parent. A verified rejection keeps the state's own
answer, and so does a held completion that was never lost.

Status: implemented, and tested against the fake host (`tests/test_host_lost_turn.py`). Whether
the installed App Server lists turns the way this relies on is re-checked on the isolated
host with CRW-124's K5 and H7 legs, and the reload and lost-receipt cases with its K5c and K5u
legs.

## What one tick guarantees

The loop is bounded, so the interesting question is not what it does but what it cannot
starve or lose.

**No anchor is left behind.** A revision can reach `dispatched` by several routes, and binding
used to happen on only one of them, which left the generation unbound and made every later
receipt for it refused. Binding is now a recovery over state that runs first in each tick, so
whichever route dispatched it, the next tick repairs it and a receipt arriving in that same
tick is accepted.

**A refused queue is remembered, not lost.** Finalizing a claim, recording the observation
that finalized it and queuing what it produced are one commit. A refusal that may not last -
a paused relationship, a recipient not yet authorized - records a delivery intent, and
recovery retries that intent with an exponential backoff so one permanently unqueueable event
cannot hold a slot. Anything else rolls the whole thing back, and the next tick re-observes.

Absence of a delivery row is deliberately NOT treated as evidence that delivery was wanted: a
receipt emitted with `--no-enqueue` and an event stranded by an old generation look exactly
the same from outside, and neither should be sent.

**The current generation is always reachable.** Observation reads are capped per tick. Within
that cap the tick serves a rotating subset of relationships rather than promising every one
of them a read, because that promise stops being possible once the relationship count passes
the budget. Each served relationship gets its current anchor first and then a rotating slice
of the rest, from a cursor persisted in the database so a restart resumes the rotation.

| | anchor revisit | full backlog coverage |
|---|---|---|
| share of two or more | every service round | `ceil(R / served) * ceil(N / (share - 1))` ticks |
| share of one | every two service rounds | `ceil(R / served) * 2N` ticks |

A candidate with nothing left to learn is dropped before the budget rather than after it,
which is what the old prefix got wrong: past eight generations the slice was permanently the
first eight, every one already observed, and the generation actually running was never
selected again.

An observation is also no longer treated as the end of a turn. A receipt written just after
the completion was seen still has to be resolved, so a turn is skipped only when it has been
observed and has no unresolved staged claim.

Status: implemented.

**Every parent gets a turn.** Selection asks which parents have anything to send before it
asks how much each of them has, then takes a bounded share from each, dealt one at a time.
A single oldest-first window let one parent's backlog take every slot. Reconciliation is
selected the same way. A parent whose send errors or defers is skipped for the rest of that
tick only; it reserves no capacity and creates no hold.

This is scheduler fairness. Transport concurrency is a separate guarantee, and it is now a
real one: the adapter dispatches each submission as its own task and allows one send in
flight per recipient, so a call stalled on one recipient no longer blocks a call to another.
What is still held back is a second send to the SAME recipient, and it is reported
`thread_busy` without being sent rather than queued behind the first. An abandoned send goes
on holding its own recipient until the transport's deadline, which is one RPC timeout for
each phase of each request. A send is three requests — `thread/read`, `thread/resume`,
`turn/start` — and the client re-establishes the connection in front of any of them whose
reader has finished, so each can also cost `unix_connect` and `initialize`.

**One transfer is three phases, and each has its own bound.** `establish` covers the wait on
`AppServer._connect_lock` and the handshake behind it, `transmit` covers the request frame
draining into the socket, and `ack` covers the response coming back. They used to share one
budget, which is how a slow establishment for one recipient spent the budget a different
recipient needed for its own write and response — `_connect_lock` serialises establishment, so
part of every connect is other recipients rebuilding. Queueing behind them now costs a caller
its `establish` bound and no more, and the expiry names the phase. A caller refused there
wrote no frame; on `thread/read` or `thread/resume` that is recorded as `withheld_pre_send`
and stays retry-safe, instead of parking a delivery that demonstrably never left.

Two things this does NOT give, worth naming beside the guarantee.

The connection is shared even though the scheduling is not. There is one `AppServer` and one
socket. A `transmit` expiry retires that connection rather than reusing it, because a cancelled
partial write corrupts the outbound stream every recipient shares, and retiring it fails the
unresolved requests it was carrying — across recipients. Settled requests are kept, and the
replacement the next `connect()` builds is untouched. Per-recipient scheduling isolation and a
shared transport failure domain are two different guarantees.

And the deadline still buys finiteness, not sufficiency. Every RPC wait inside a send is now
bounded by its own phase, but the deadline also covers work no caller-side timer can preempt:
the bridge ledger's transactions, and a `before_start` guard that does synchronous filesystem
and SQLite work before its first `await` and so blocks the event loop the timer runs on.
`guard_rpc_requests` bounds how many host calls such a guard may make, never how long it may
hold the loop. So the deadline can still fire on a send whose phases were all within their
bounds, and what it leaves behind is final: `_guarded_send` writes an `outcome_unknown`
receipt on cancellation and re-raises, and nothing replaces that row later. Recovering the
event from there is reconciliation's job under I-71 in
[invariants.md](invariants.md), and what stops a second copy being sent is that `_settle`
never reschedules `held_uncertain` — an unknown outcome waits to be reconciled instead of
being retried. It is the delivery state machine that protects the event, not same-id replay:
`derive_request_id` gives every attempt its own id, so a retry is a new id the retained
receipt says nothing about. Read this bound as a backstop against the local work no timer can
interrupt, not as a promise about how long a working send may take — the waits it used to
stand in for are bounded where they happen now.

**A stale event is stopped before the send.** A generation that has moved on invalidates
every outcome of the previous one, whether or not the new generation has produced a revision
yet, and the claim statement itself refuses one. An outstanding send is annotated rather than
rewritten, so reconciliation can still settle it, and an already delivered copy keeps its
history without being read as verification of the current head.

## What a restart preserves

Assignments, generations and anchors, queued and deferred deliveries, attempt history and
frozen message bytes, acknowledgements, verdicts and the Linear outbox all live in the store
and survive any process boundary. On start the relay reconciles unresolved attempts before
doing anything else, and reconciliation establishes what happened without sending: an
attempt whose response was lost stays uncertain until evidence resolves it, and is never
resent on the strength of elapsed time.

Status: implemented. The supervisor runs recovery before its first worker, and an expired
lease returns its delivery to `held_uncertain` for the reconciler to judge, never to the
send queue, because a queued row would be eligible to send again on no evidence.
