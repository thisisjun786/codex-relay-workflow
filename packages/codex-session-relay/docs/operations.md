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
| `parent_busy` | the parent is mid-turn; it is never interrupted |
| `settings_rejected` | the host would not confirm the authorized execution settings |
| `withheld:<operation>` | refused before any transport call, naming the operation that refused |
| `turn_accepted` | the transport started a turn |
| `awaiting_ack` | delivered, acknowledgement outstanding |
| `awaiting_child_receipt` | a revision request was delivered; contract v1 defines no acknowledgement for that direction, so the child answers with its next completion receipt |
| `channel_closed` | the push channel itself is unavailable; stored, not woken |
| `superseded` | a newer generation or revision replaced this one |

Each carries the most recent failed operation, its concrete error, the exact settings
difference where there is one, and the next retry time.

Health is separate from liveness. A running process with a growing observation backlog is
reported as stalled: staged event age, when each current anchor was last successfully
polled, and the backlog per assignment are all exposed, and a live pid is never counted as
working. An anchor whose turn is terminal with nothing staged behind it is reported as
settled and excluded from freshness: the scheduler deliberately stops reading it, so its
last poll cannot advance, and ageing it out would report every quiet assignment as stalled.
New staged work on that turn makes it eligible again.

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

This is scheduler fairness, not transport concurrency. The adapter serialises on one worker,
so a stalled call still blocks the one behind it.

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
