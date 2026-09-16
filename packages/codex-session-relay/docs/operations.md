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

Status: planned in PR-A. Today only the bounded foreground `daemon` command exists.

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

Status: planned in PR-A.

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

Status: the per-assignment refusal is partly implemented (I-13, I-74); the direction check,
project grouping and shared-service guarantees are planned in PR-A.

## Reading a stuck delivery

`status` distinguishes the stages that all previously read as one withheld state:

| Phase | Meaning |
|---|---|
| `awaiting_receipt` | the child has not produced a completion receipt yet |
| `parent_busy` | the parent is mid-turn; it is never interrupted |
| `settings_rejected` | the host would not confirm the authorized execution settings |
| `turn_accepted` | the transport started a turn |
| `awaiting_ack` | delivered, acknowledgement outstanding |
| `channel_closed` | the push channel itself is unavailable; stored, not woken |
| `superseded` | a newer generation or revision replaced this one |

Each carries the most recent failed operation, its concrete error, the exact settings
difference where there is one, and the next retry time.

Health is separate from liveness. A running process with a growing observation backlog is
reported as stalled: staged event age, when each current anchor was last successfully
polled, and the backlog per assignment are all exposed, and a live pid is never counted as
working.

Status: planned in PR-B.

## What a restart preserves

Assignments, generations and anchors, queued and deferred deliveries, attempt history and
frozen message bytes, acknowledgements, verdicts and the Linear outbox all live in the store
and survive any process boundary. On start the relay reconciles unresolved attempts before
doing anything else, and reconciliation establishes what happened without sending: an
attempt whose response was lost stays uncertain until evidence resolves it, and is never
resent on the strength of elapsed time.

Status: the durable state and reconciliation exist; supervised restart and lease recovery
are planned in PR-A.
