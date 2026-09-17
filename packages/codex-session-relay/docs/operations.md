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

Three refusals cover it. All three are decided before the command runs, so a command that
opens no store — `service status`, for instance — is refused on the same evidence as one
that would create a database.

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

Two things this recovery does not do. Selecting one of two claiming stores does not remove
the ambiguity — both still record the socket, so the next invocation that relies on default
discovery is refused again, and every participant of that assignment has to pass the same
explicit `--state` until one of the stores is retired. And the different-socket refusal's
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

Status: implemented. `resolve_state_dir` carries the candidates on the selection and the
command line refuses on them, naming the candidates and the database it would otherwise have
created.

## Proving two participants share one store

A path string is not proof: symlinks, bind mounts and per-sandbox mounts all make equal
paths unequal and unequal paths equal. A stored identifier alone is not proof either,
because copying the database copies the identifier.

| Evidence | Verdict |
|---|---|
| a nonce written by one participant is readable by the other | proven |
| equal store id and equal device/inode | proven |
| equal store id, different inode, no nonce | unproven - a copy is possible |
| different store id | mismatch |

`doctor --expect-store <id>` exits non-zero on a mismatch. An unproven result is never
reported as healthy.

Status: implemented. Unproven also exits non-zero, because a caller that asked whether this is
the same store must not read exit 0 as yes. Each participant runs the check in its own sandbox;
one invocation cannot establish another participant's access.

## The service

Two process roles. A **worker** is the existing bounded daemon: it runs a set number of ticks
or until a deadline and then exits, and cannot be constructed unbounded (I-64). A
**supervisor** owns the locks and launches successive workers, which is what carries an
assignment past any single process bound.

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
| `awaiting_child_receipt` | a revision request was delivered; contract v1 defines no acknowledgement for that direction, so the child answers with its next completion receipt |
| `channel_closed` | the push channel itself is unavailable; stored, not woken |
| `superseded` | a newer generation or revision replaced this one |
| `superseded:<reason>` | an outstanding send a newer generation or revision replaced; its state is left alone so a lost response stays reconcilable |

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
each stage the bridge bounds separately. A send is three requests — `thread/read`,
`thread/resume`, `turn/start` — and the client re-establishes the connection in front of any
of them whose reader has finished, so each can also cost `unix_connect` and `initialize`.
Two limits worth naming beside that guarantee, because neither is visible from the sentence
above it.

The isolation begins at the connection. `AppServer._connect_lock` serialises establishment,
so a stalled `connect()` is still shared by every recipient, reads included, and only what
happens after a connection exists is isolated per recipient.

And the deadline buys finiteness, not sufficiency. It is not an upper bound on a send that is
still making progress and cannot be turned into one by choosing a larger multiple: the same
timer also covers the time this send spends waiting on that shared connect lock while a
DIFFERENT recipient rebuilds, and a queue of rebuilds ahead of it has no constant bound. So
the deadline can fire on a healthy send, and what it leaves behind is final: `_guarded_send`
writes an `outcome_unknown` receipt on cancellation and re-raises, and nothing replaces that
row later. Recovering the event from there is reconciliation's job under I-71 in
[invariants.md](invariants.md), and what stops a second copy being sent is that `_settle`
never reschedules `held_uncertain` — an unknown outcome waits to be reconciled instead of
being retried. It is the delivery state machine that protects the event, not same-id replay:
`derive_request_id` gives every attempt its own id, so a retry is a new id the retained
receipt says nothing about. Read this bound as a backstop against a write that never drains,
not as a promise about how long a working send may take.

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
